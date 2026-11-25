package chatevents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeCredentialFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	clientFile := filepath.Join(dir, "oauth-client.json")
	tokenFile := filepath.Join(dir, "refresh-token")
	if err := os.WriteFile(clientFile, []byte(`{"type":"installed","client_id":"cid","client_secret":"secret"}`), 0o600); err != nil {
		t.Fatalf("write client: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("refresh-value\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return clientFile, tokenFile
}

func TestLoadCredential(t *testing.T) {
	clientFile, tokenFile := writeCredentialFiles(t)
	cred, err := LoadCredential(clientFile, "", tokenFile)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cred.ClientID != "cid" || cred.ClientSecret != "secret" || cred.RefreshToken != "refresh-value" {
		t.Fatalf("credential = %+v", cred)
	}

	// A bare token file and a JSON-wrapped one are both accepted: which one a Secret mount produces
	// depends on how it was created, and that is not worth betting on.
	wrapped := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(wrapped, []byte(`{"refresh_token":"wrapped-value"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cred, err = LoadCredential(clientFile, "", wrapped)
	if err != nil || cred.RefreshToken != "wrapped-value" {
		t.Fatalf("wrapped token: cred=%+v err=%v", cred, err)
	}
}

// TestLoadCredentialReadsTheSecretMountShape is the layout home-ops actually shipped: a Secret built
// from --from-literal projects three plain files, and none of them is JSON.
func TestLoadCredentialReadsTheSecretMountShape(t *testing.T) {
	dir := t.TempDir()
	idFile := filepath.Join(dir, "client_id")
	secretFile := filepath.Join(dir, "client_secret")
	tokenFile := filepath.Join(dir, "refresh_token")
	write := func(path, value string) {
		if err := os.WriteFile(path, []byte(value+"\n"), 0o400); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	write(idFile, "id-from-secret")
	write(secretFile, "secret-from-secret")
	write(tokenFile, "token-from-secret")

	cred, err := LoadCredential(idFile, secretFile, tokenFile)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cred != (Credential{ClientID: "id-from-secret", ClientSecret: "secret-from-secret", RefreshToken: "token-from-secret"}) {
		t.Fatalf("credential = %+v", cred)
	}

	// A JSON client file that names no secret falls back to the separate secret file, because that is
	// the half-written state an operator gets into when they export a client and forget the secret.
	jsonFile := filepath.Join(dir, "client.json")
	if err := os.WriteFile(jsonFile, []byte(`{"client_id":"json-id"}`), 0o400); err != nil {
		t.Fatalf("write: %v", err)
	}
	cred, err = LoadCredential(jsonFile, secretFile, tokenFile)
	if err != nil || cred.ClientID != "json-id" || cred.ClientSecret != "secret-from-secret" {
		t.Fatalf("mixed shape: cred=%+v err=%v", cred, err)
	}
}

func TestLoadCredentialRefusesAnIncompleteCredential(t *testing.T) {
	dir := t.TempDir()
	clientFile := filepath.Join(dir, "client.json")
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(clientFile, []byte(`{"client_id":"cid"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("t"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadCredential(clientFile, "", tokenFile); err == nil {
		t.Fatal("expected an error when the client secret is missing")
	}
	if _, err := LoadCredential(filepath.Join(dir, "missing.json"), "", tokenFile); err == nil {
		t.Fatal("expected an error when the client file does not exist")
	}
}

// fakeWorkspaceAPI answers the three calls one renewal actually makes, in the order Google makes them
// awkward: the only way to learn a subscription's name is the 409 that create returns for a duplicate
// target, because listing subscriptions is refused by the API.
type fakeWorkspaceAPI struct {
	mu       sync.Mutex
	requests []string
	existing string
	created  int
	expire   string
	// createAsOperation reproduces what the live API actually does: the create response names a
	// long-running operation and carries the subscription inside its response.
	createAsOperation bool
}

func (f *fakeWorkspaceAPI) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/subscriptions"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			if f.existing != "" {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"message": "Subscription associated with the resource already exists.",
						"status":  "ALREADY_EXISTS",
						"details": []any{map[string]any{
							"reason":   "SUBSCRIPTION_ALREADY_EXISTS",
							"metadata": map[string]string{"current_subscription": f.existing},
						}},
					},
				})
				return
			}
			f.created++
			f.existing = fmt.Sprintf("subscriptions/chat-created-%d", f.created)
			if f.createAsOperation {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"name": fmt.Sprintf("operations/op-%d", f.created),
					"response": map[string]any{
						"name":       f.existing,
						"expireTime": f.expire,
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": f.existing, "expireTime": f.expire})
		case r.Method == http.MethodDelete:
			if strings.Contains(r.URL.Path, "/operations/") {
				t.Errorf("delete targeted a long-running operation: %s", r.URL.Path)
			}
			if f.existing == "" || !strings.HasSuffix(r.URL.Path, f.existing) {
				t.Errorf("delete of %q while existing is %q", r.URL.Path, f.existing)
			}
			f.existing = ""
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/operations/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true,
				"response": map[string]any{
					"name":       f.existing,
					"expireTime": f.expire,
				},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"expireTime": f.expire})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
}

func newTestRenewer(t *testing.T, server *httptest.Server) *Renewer {
	t.Helper()
	cred := Credential{ClientID: "cid", ClientSecret: "secret", RefreshToken: "refresh-value"}
	r := NewRenewer(cred, "home-ops-513116", "pi-chat-user-events", 2*time.Hour,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	r.APIURL = server.URL + "/v1"
	r.TokenURL = server.URL + "/token"
	return r
}

// The production constant is the thing that broke in the cluster, and no fake could catch it: every
// test supplied its own base ("<server>/v1"), while the shipped default already contained the
// collection, so create POSTed to /v1/subscriptions/subscriptions and Google answered the front-door
// 404 as HTML. This asserts the default itself, in the shape the calls actually build.
func TestRenewerDefaultURLIsTheRealAPIsShape(t *testing.T) {
	r := NewRenewer(Credential{ClientID: "c", ClientSecret: "s", RefreshToken: "t"}, "home-ops-513116", "pi-chat-user-events", 3*time.Hour, nil)
	if got := r.api() + "/subscriptions"; got != "https://workspaceevents.googleapis.com/v1/subscriptions" {
		t.Fatalf("create URL = %q, want the collection directly under /v1", got)
	}
	name := "projects/home-ops-513116/subscriptions/pi-chat-user-events"
	if got := r.api() + "/" + name; got != "https://workspaceevents.googleapis.com/v1/"+name {
		t.Fatalf("delete URL = %q", got)
	}
	if strings.Contains(strings.TrimSuffix(r.api(), "/v1"), "subscriptions") {
		t.Fatalf("api base %q already contains the collection, which double-paths every call", r.api())
	}
	if r.tokenURL() != "https://oauth2.googleapis.com/token" {
		t.Fatalf("token URL = %q", r.tokenURL())
	}
}

func TestRenewFindsTheLiveSubscriptionThroughThe409AndReplacesIt(t *testing.T) {
	api := &fakeWorkspaceAPI{expire: "2026-11-25T09:00:00Z"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-1"})
			return
		}
		api.handler(t).ServeHTTP(w, r)
	}))
	defer server.Close()

	renewer := newTestRenewer(t, server)
	// The live subscription was created outside this process (by the probe tool), so the renewer starts
	// with no name and must discover it the only way the API allows.
	api.existing = "subscriptions/chat-spaces-czotOjExMzA3MDM5NDE5MTQwODMxMTAyNDoxMDM5MDI2MjA2OTQ5OTk1MzgzNTU"

	expire, err := renewer.Renew(context.Background())
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if expire != api.expire {
		t.Fatalf("expire = %q, want %q", expire, api.expire)
	}

	var sawConflictDeleteCreate bool
	api.mu.Lock()
	requests := append([]string(nil), api.requests...)
	api.mu.Unlock()
	for i, req := range requests {
		if strings.HasPrefix(req, "POST") && i+2 < len(requests) &&
			strings.HasPrefix(requests[i+1], "DELETE") && strings.HasPrefix(requests[i+2], "POST") {
			sawConflictDeleteCreate = true
		}
	}
	if !sawConflictDeleteCreate {
		t.Fatalf("requests = %v, want create (409) then delete then create", requests)
	}
	if api.created != 1 {
		t.Fatalf("subscriptions created = %d, want 1", api.created)
	}
}

// The live API creates a subscription as a long-running operation. The renewer must remember the
// subscription, never the operation, or every later renewal deletes a path that cannot exist and the
// subscription quietly dies at its expiry.
func TestRenewResolvesALongRunningCreateToTheSubscription(t *testing.T) {
	api := &fakeWorkspaceAPI{expire: "2026-11-25T15:00:00Z", createAsOperation: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-op"})
			return
		}
		api.handler(t).ServeHTTP(w, r)
	}))
	defer server.Close()

	renewer := newTestRenewer(t, server)
	expire, err := renewer.Renew(context.Background())
	if err != nil {
		t.Fatalf("first renew: %v", err)
	}
	if expire != api.expire {
		t.Fatalf("expire = %q, want %q", expire, api.expire)
	}
	if strings.HasPrefix(renewer.name, "operations/") {
		t.Fatalf("renewer remembered the operation %q instead of the subscription", renewer.name)
	}
	if renewer.name == "" {
		t.Fatal("renewer did not resolve a subscription name from the operation")
	}

	// The second renewal is where the bug was fatal: it deletes what it remembered.
	if _, err := renewer.Renew(context.Background()); err != nil {
		t.Fatalf("second renew: %v", err)
	}
	api.mu.Lock()
	requests := append([]string(nil), api.requests...)
	api.mu.Unlock()
	var deletes, creates int
	for _, req := range requests {
		switch {
		case strings.HasPrefix(req, "DELETE"):
			deletes++
			if strings.Contains(req, "/operations/") {
				t.Fatalf("deleted an operation: %s", req)
			}
		case strings.HasPrefix(req, "POST"):
			creates++
		}
	}
	if deletes != 1 || creates != 2 {
		t.Fatalf("requests = %v, want create, delete, create", requests)
	}
}

func TestRenewRunsAgainOnTheRememberedName(t *testing.T) {
	api := &fakeWorkspaceAPI{expire: "2026-11-25T12:00:00Z"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-2"})
			return
		}
		api.handler(t).ServeHTTP(w, r)
	}))
	defer server.Close()

	renewer := newTestRenewer(t, server)
	if _, err := renewer.Renew(context.Background()); err != nil {
		t.Fatalf("first renew: %v", err)
	}
	if _, err := renewer.Renew(context.Background()); err != nil {
		t.Fatalf("second renew: %v", err)
	}
	api.mu.Lock()
	requests := append([]string(nil), api.requests...)
	created := api.created
	api.mu.Unlock()
	if created != 2 {
		t.Fatalf("creates = %d, want 2", created)
	}
	deletes := 0
	for _, req := range requests {
		if strings.HasPrefix(req, "DELETE") {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("deletes = %d, want 1: the second renewal knows its own name and does not need the 409 probe", deletes)
	}
}

func TestRenewReportsARefusedTokenInsteadOfGoingSilent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "Refresh token has been revoked"})
	}))
	defer server.Close()

	renewer := newTestRenewer(t, server)
	if _, err := renewer.Renew(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("err = %v, want the token refusal surfaced: a revoked refresh token must not look like a quiet success", err)
	}
}

func TestRenewSurvivesAnOldSubscriptionAlreadyGone(t *testing.T) {
	// The remembered subscription may have expired away while the pod was down. Delete fails, create
	// must still succeed: room ingestion restarting is the point, not preserving the error.
	var existing string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-3"})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "not found", "status": "NOT_FOUND"}})
		case r.Method == http.MethodPost:
			existing = "subscriptions/chat-fresh"
			_ = json.NewEncoder(w).Encode(map[string]any{"name": existing, "expireTime": "2026-11-25T15:00:00Z"})
		}
	}))
	defer server.Close()

	renewer := newTestRenewer(t, server)
	renewer.name = "subscriptions/chat-expired"
	expire, err := renewer.Renew(context.Background())
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if expire != "2026-11-25T15:00:00Z" {
		t.Fatalf("expire = %q", expire)
	}
}

func TestRenewerIntervalIsClampedInsideTheSubscriptionLifetime(t *testing.T) {
	quick := NewRenewer(Credential{}, "p", "t", time.Minute, nil)
	if quick.Interval != DefaultInterval {
		t.Fatalf("interval = %s, want the default: a one-minute renewal loop is a renewal storm", quick.Interval)
	}
	slow := NewRenewer(Credential{}, "p", "t", 10*time.Hour, nil)
	if slow.Interval > 4*time.Hour {
		t.Fatalf("interval = %s, want at or under the measured ~4h subscription lifetime", slow.Interval)
	}
}

// TestDefaultMaskNamesTheIdentityFields guards the one field the room path cannot work without. A
// subscription whose payload has a sender type but no email makes every untagged room message
// unactionable, and the only symptom is a log line nobody reads.
func TestDefaultMaskNamesTheIdentityFields(t *testing.T) {
	for _, want := range []string{"message.sender", "message.sender.email", "message.sender.displayName", "message.sender.name", "message.text", "message.thread"} {
		if !strings.Contains(DefaultMask, want) {
			t.Errorf("DefaultMask missing %q: %s", want, DefaultMask)
		}
	}
}
