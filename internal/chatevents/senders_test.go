package chatevents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// These payloads are transcriptions of what was measured on 2026-11-25, not inventions:
//   - the Chat messages.list sender, under Ed's own OAuth credential, carries name AND email AND
//     displayName: {"name":"users/113070394191408311024","email":"initialed85@gmail.com",…}
//   - the room listener sender carries neither: {"name":"users/108049318480764095606","type":"HUMAN"}
// The test exists because the whole point of this file is the gap between those two shapes.

type lookupHarness struct {
	t      *testing.T
	chat   *httptest.Server
	token  *httptest.Server
	chatOK func(w http.ResponseWriter, r *http.Request)
	hits   int
}

func newLookupHarness(t *testing.T) (*lookupHarness, *SenderLookup) {
	t.Helper()

	h := &lookupHarness{t: t}
	h.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fake-access-token","expires_in":3600,"token_type":"Bearer"}`)
	}))

	h.chat = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits++
		if h.chatOK == nil {
			h.t.Fatalf("chat read with no handler configured: %s", r.URL.Path)
		}
		h.chatOK(w, r)
	}))

	lookup := NewSenderLookup(Credential{ClientID: "cid", ClientSecret: "secret", RefreshToken: "refresh"}, nil).
		WithTokenURL(h.token.URL).
		WithChatEndpoint(h.chat.URL)
	return h, lookup
}

func (h *lookupHarness) close() {
	h.chat.Close()
	h.token.Close()
}

// TestSenderLookup_ResolvesTheIDTheListenerShipsUsingTheHistoryChatGives is the fix for the deployed
// failure: the room event ships users/<id> with no email, and messages.list in the same space ships the
// same id with an email.
func TestSenderLookup_ResolvesTheIDTheListenerShipsUsingTheHistoryChatGives(t *testing.T) {
	h, lookup := newLookupHarness(t)
	defer h.close()

	h.chatOK = func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("path = %q, want the space's messages collection", r.URL.Path)
		}
		// The query shape is pinned because this package has already been broken by a wrong base URL,
		// and a wrong fields= would be the same class of silent failure.
		if got := r.URL.Query().Get("fields"); got != "messages/name,messages/sender" {
			t.Errorf("fields = %q", got)
		}
		if got := r.URL.Query().Get("pageSize"); got == "" {
			t.Errorf("pageSize missing")
		}
		// Ascending is the API default and it silently returns the wrong decade, so the pin here is a
		// regression pin against a bug that was live in production, not a style preference.
		if got := r.URL.Query().Get("orderBy"); got != "createTime desc" {
			t.Errorf("orderBy = %q, want %q (ascending reads return the oldest messages of the space)", got, "createTime desc")
		}
		if got := r.Header.Get("authorization"); got != "Bearer fake-access-token" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[
			{"sender":{"name":"users/113070394191408311024","email":"initialed85@gmail.com","displayName":"initialed85 _","type":"HUMAN"}},
			{"sender":{"name":"users/108049318480764095606","type":"HUMAN"}}
		]}`)
	}

	id, found, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "users/113070394191408311024")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !found {
		t.Fatalf("not found, want Ed's email from the space history")
	}
	if id.Email != "initialed85@gmail.com" {
		t.Errorf("email = %q", id.Email)
	}
	if id.DisplayName != "initialed85 _" {
		t.Errorf("displayName = %q", id.DisplayName)
	}
	if lookup.Calls() != 1 {
		t.Errorf("calls = %d, want 1", lookup.Calls())
	}

	// The cache has to be load bearing: a room is a busy place and re-reading history per message would
	// make every untagged message cost a Chat round trip.
	if _, found, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "113070394191408311024"); err != nil || !found {
		t.Fatalf("bare legacy id: found=%v err=%v", found, err)
	}
	if lookup.Calls() != 1 {
		t.Errorf("calls = %d after a cached lookup, want 1", lookup.Calls())
	}
}

// TestSenderLookup_AnIDChatGaveNoEmailForIsNotInvented covers the trap: the history read itself can
// contain a sender with no email, which is the same hole the listener has. Returning someone else's
// email for that id would put the wrong person on the allowlist.
func TestSenderLookup_AnIDChatGaveNoEmailForIsNotInvented(t *testing.T) {
	h, lookup := newLookupHarness(t)
	defer h.close()

	h.chatOK = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[
			{"sender":{"name":"users/113070394191408311024","email":"initialed85@gmail.com","displayName":"initialed85 _","type":"HUMAN"}},
			{"sender":{"name":"users/108049318480764095606","type":"HUMAN"}}
		]}`)
	}

	_, found, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "users/108049318480764095606")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if found {
		t.Fatalf("found an identity for a sender Chat published without an email")
	}

	// A miss must not become a Chat read per message from the same unknown person.
	if _, _, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "users/108049318480764095606"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if lookup.Calls() != 1 {
		t.Errorf("calls = %d, want 1: an unresolvable id should be on a cooldown, not retried per message", lookup.Calls())
	}
}

// TestSenderLookup_AScopeRefusalIsReportedAsOne keeps a 403 (which is what members.list answers, and a
// likely answer if a scope is ever missing) from looking like "unknown person".
func TestSenderLookup_AScopeRefusalIsReportedAsOne(t *testing.T) {
	h, lookup := newLookupHarness(t)
	defer h.close()

	h.chatOK = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{ "error": { "code": 403, "message": "Request had insufficient authentication scopes.", "status": "PERMISSION_DENIED" } }`)
	}

	_, found, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "users/113070394191408311024")
	if found {
		t.Fatalf("found an identity from a refused read")
	}
	if err == nil {
		t.Fatalf("no error: a permission problem must not be indistinguishable from an unknown sender")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "insufficient authentication scopes") {
		t.Errorf("err = %v, want the status and Google's own words", err)
	}
	if strings.Contains(err.Error(), "<html") || strings.Contains(err.Error(), "<!DOCTYPE") {
		t.Errorf("err = %v, want a snippet rather than a pasted HTML page", err)
	}
}

// TestSenderLookup_HTMLOrBodilyAnswersDoNotBecomeIdentities is the HTML-404 failure mode this package
// has already been bitten by once: Google's front door answers a bad path with a page, and the symptom
// was a JSON decode error.
func TestSenderLookup_HTMLOrBodilyAnswersDoNotBecomeIdentities(t *testing.T) {
	h, lookup := newLookupHarness(t)
	defer h.close()

	h.chatOK = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = io.WriteString(w, "<!DOCTYPE html><html lang=en><title>Error 404 (Not Found)</title></html>")
	}

	_, _, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "users/113070394191408311024")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want an http 404 report rather than a decode error", err)
	}
}

// TestSenderLookup_MissingSpaceOrIDFailsBeforeTheNetwork keeps an empty id - which is what the deployed
// bridge logged for every rejected room message before the diagnostic existed - from producing a call.
func TestSenderLookup_MissingSpaceOrIDFailsBeforeTheNetwork(t *testing.T) {
	h, lookup := newLookupHarness(t)
	defer h.close()

	for _, tc := range []struct{ space, userID string }{{"", "users/1"}, {"spaces/AAAA1pNBS1A", ""}, {"", ""}} {
		_, found, err := lookup.Resolve(context.Background(), tc.space, tc.userID)
		if err == nil || found {
			t.Errorf("space=%q user=%q: found=%v err=%v, want an error before any request", tc.space, tc.userID, found, err)
		}
	}
	if h.hits != 0 {
		t.Errorf("chat hits = %d, want 0", h.hits)
	}
	if lookup.Calls() != 0 {
		t.Errorf("calls = %d, want 0", lookup.Calls())
	}
}

// TestSenderLookupDefaultURLsAreTheRealAPIsShape pins the constants, not just the behaviour: the last
// time a base URL in this package was wrong, every test still passed because they all pointed a fake
// server at themselves.
func TestSenderLookupDefaultURLsAreTheRealAPIsShape(t *testing.T) {
	if chatAPI != "https://chat.googleapis.com/v1" {
		t.Errorf("chatAPI = %q", chatAPI)
	}
	if !strings.HasPrefix(chatAPI, "https://") || strings.HasSuffix(chatAPI, "/spaces") || strings.HasSuffix(chatAPI, "/messages") {
		t.Errorf("chatAPI = %q, want the API base with no collection appended", chatAPI)
	}
	if _, err := url.Parse(chatAPI); err != nil {
		t.Errorf("chatAPI unparseable: %v", err)
	}
	if missCooldown <= 0 || lookupTTL <= missCooldown {
		t.Errorf("missCooldown=%v lookupTTL=%v", missCooldown, lookupTTL)
	}
}

// TestSenderLookup_CachesExpire stops a binding becoming a permanent fact: an account deleted or an
// email changed should not keep granting access from a cache written weeks ago.
func TestSenderLookup_CachesExpire(t *testing.T) {
	h, lookup := newLookupHarness(t)
	defer h.close()

	h.chatOK = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"messages":[{"sender":{"name":"users/113070394191408311024","email":"initialed85@gmail.com","type":"HUMAN"}}]}`)
	}

	if _, found, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "users/113070394191408311024"); err != nil || !found {
		t.Fatalf("first resolve: found=%v err=%v", found, err)
	}
	lookup.mu.Lock()
	for key := range lookup.cached {
		lookup.cached[key] = time.Now().Add(-lookupTTL - time.Minute)
	}
	lookup.mu.Unlock()

	_, found, err := lookup.Resolve(context.Background(), "spaces/AAAA1pNBS1A", "users/113070394191408311024")
	if err != nil {
		t.Fatalf("resolve after expiry: %v", err)
	}
	if !found {
		t.Fatalf("not found after expiry, want a fresh read of the space history")
	}
	if lookup.Calls() != 2 {
		t.Errorf("calls = %d, want 2: an expired cache should re-read, not answer from stale data", lookup.Calls())
	}
}

func TestSenderLookup_DecodesOnlyIdentityFields(t *testing.T) {
	var page struct {
		Messages []struct {
			Sender struct {
				Name        string `json:"name"`
				Email       string `json:"email"`
				DisplayName string `json:"displayName"`
				Type        string `json:"type"`
			} `json:"sender"`
		} `json:"messages"`
	}
	// A real-looking page with text present: this package must never read message text, only identity.
	body := `{"messages":[{"name":"spaces/AAAA1pNBS1A/messages/abc.abc","text":"private conversation","sender":{"name":"users/113070394191408311024","email":"initialed85@gmail.com","displayName":"initialed85 _","type":"HUMAN"}}]}`
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Sender.Email != "initialed85@gmail.com" {
		t.Fatalf("decoded page = %+v", page.Messages)
	}
	if strings.Contains(fmt.Sprint(page), "private conversation") {
		t.Errorf("the decoder pulled message text into the identity struct")
	}
}
