// Package chatevents owns the Workspace Events subscription that makes room listening possible.
//
// A Workspace Events subscription is not a long-lived resource: it expires a measured ~4 h after
// createTime, and its payload shape (the fieldMask) is fixed at creation - PATCH payloadOptions
// returns 400. So keeping room ingestion alive means periodically deleting and recreating the same
// subscription, and the bridge does that itself rather than relying on a cron outside it: a bridge
// that cannot renew is a bridge that goes deaf four hours after a deploy with nothing in the pod to
// say so.
//
// Measured behaviour this code depends on (internal/bridge/room-ingestion.md):
//   - the only working way to find an existing subscription is the 409 that create returns for a
//     duplicate target, whose error metadata carries `current_subscription`. GET /v1/subscriptions
//     is refused with FILTER_PARSING_FAILURE, listing does not work.
//   - validateOnly accepts a fieldMask naming fields that do not exist, so a mask is only proven by
//     what actually arrives; nothing here pretends to validate one.
//   - the topic outlives the subscription, so the chat-api-push publisher grant survives a renewal.
package chatevents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultShape is the subscription this deployment uses: every space the authorising human belongs to,
// message-created events only, pushed to one topic.
const (
	DefaultTarget = "//chat.googleapis.com/spaces/-"
	DefaultEvent  = "google.workspace.chat.message.v1.created"
	// DefaultMask names the sender object *and* its leaves. Measured 2026-11-25T07:00Z on the deployed
	// listener, which logs the sender object verbatim: it arrives as exactly
	//   {"name":"users/<id>","type":"HUMAN"}
	// - no email, no displayName, no legacyId - and widening this mask to name those leaves changed
	// nothing. The mask is advisory; Google ships what it ships. The leaves are kept because stating the
	// fields this code needs is more honest than implying the subscription provides them, and because it
	// costs nothing. Identity therefore cannot come from the payload: it comes from messages that did
	// carry an email (mentions and DMs on the app credential, whose captured payloads do contain
	// "name":"users/<id>" alongside email and displayName), and nowhere else - chat.googleapis.com
	// users.get answers 404 for this credential and spaces.members.list answers 403 insufficient scopes.
	DefaultMask     = "message.name,message.text,message.sender,message.sender.name,message.sender.email,message.sender.displayName,message.sender.type,message.annotations,message.thread,message.space"
	DefaultInterval = 3 * time.Hour
	tokenEndpoint   = "https://oauth2.googleapis.com/token"
	// subscriptionsAPI is the API *base*, not the collection: create appends "/subscriptions" and
	// delete/get append the subscription name. Getting this wrong is not loud - it POSTs to
	// /v1/subscriptions/subscriptions, Google's front door answers with an HTML 404 page, and the only
	// symptom is "decode create response: invalid character '<'". The unit tests always used a fake
	// server whose base was "<url>/v1", so they could not catch a constant that already contained the
	// collection. TestRenewerDefaultURLIsTheRealAPIsShape now pins the constant itself.
	subscriptionsAPI = "https://workspaceevents.googleapis.com/v1"
)

// Credential is a human OAuth authorisation, read from files so nothing secret lives in the repo or in
// an environment that a kubectl describe would print.
type Credential struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
}

// LoadCredential reads the two files the deployment mounts: one holding the OAuth client, one holding
// the refresh token. Both are expected to be 0400 from a Secret, and neither is ever logged.
// LoadCredential reads the user OAuth credential from files, in whichever shape the operator actually
// produced. Both exist in this fleet and neither is obvious from the path:
//
//   - one installed-client JSON file holding client_id and client_secret, plus a token file. That is
//     what a Google desktop client export looks like and what tools/chat-user-subscription.mjs reads.
//   - three separate files, which is what a Kubernetes Secret built from --from-literal projects into
//     a mount: /etc/pi-chat-user-oauth/{client_id,client_secret,refresh_token}.
//
// secretFile is only consulted for the second shape. Choosing by content rather than by a flag means
// an operator who wires the wrong shape gets a clear message about the two files that are missing, not
// a token exchange that fails with an unhelpful `invalid_grant` three steps later.
func LoadCredential(clientFile, secretFile, tokenFile string) (Credential, error) {
	var cred Credential
	raw, err := os.ReadFile(clientFile)
	if err != nil {
		return cred, fmt.Errorf("chatevents: read oauth client file: %w", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var client struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			Type         string `json:"type"`
		}
		if err := json.Unmarshal(raw, &client); err != nil {
			return cred, fmt.Errorf("chatevents: decode oauth client file: %w", err)
		}
		cred.ClientID = strings.TrimSpace(client.ClientID)
		cred.ClientSecret = strings.TrimSpace(client.ClientSecret)
	} else {
		cred.ClientID = trimmed
	}
	if cred.ClientSecret == "" && strings.TrimSpace(secretFile) != "" {
		raw, err := os.ReadFile(secretFile)
		if err != nil {
			return cred, fmt.Errorf("chatevents: read oauth client secret file: %w", err)
		}
		cred.ClientSecret = strings.TrimSpace(string(raw))
	}

	raw, err = os.ReadFile(tokenFile)
	if err != nil {
		return cred, fmt.Errorf("chatevents: read refresh token file: %w", err)
	}
	token := strings.TrimSpace(string(raw))
	if strings.HasPrefix(token, "{") {
		var wrapper struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			return cred, fmt.Errorf("chatevents: decode refresh token file: %w", err)
		}
		token = strings.TrimSpace(wrapper.RefreshToken)
	}
	cred.RefreshToken = token

	if cred.ClientID == "" || cred.ClientSecret == "" || cred.RefreshToken == "" {
		return cred, fmt.Errorf("chatevents: credential files are incomplete (client id, client secret and refresh token are all required; expected one installed-client JSON file, or three files: id, secret, token)")
	}
	return cred, nil
}

// Renewer recreates one Workspace Events subscription on a timer.
type Renewer struct {
	Cred     Credential
	Project  string
	Topic    string
	Target   string
	Event    string
	Mask     string
	Interval time.Duration
	Log      *slog.Logger

	hc *http.Client
	// APIURL and TokenURL exist so a test can point the renewal at a fake API. Production leaves them
	// unset and gets Google's.
	APIURL   string
	TokenURL string
	// name is remembered between renewals so the steady state does not depend on the 409 probe.
	name string
}

// NewRenewer fills in the defaults this deployment uses. Interval is clamped well inside the measured
// ~4 h lifetime: a renewal that lands just before expiry is a renewal that fails when Google is slow.
func NewRenewer(cred Credential, project, topic string, interval time.Duration, log *slog.Logger) *Renewer {
	if log == nil {
		log = slog.Default()
	}
	if interval < 30*time.Minute || interval > 4*time.Hour {
		// Too short is a renewal storm against a resource Google rate-limits; too long runs past the
		// measured ~4h lifetime and the listener simply goes deaf.
		interval = DefaultInterval
	}
	return &Renewer{
		Cred:     cred,
		Project:  project,
		Topic:    topic,
		Target:   DefaultTarget,
		Event:    DefaultEvent,
		Mask:     DefaultMask,
		Interval: interval,
		Log:      log,
		APIURL:   subscriptionsAPI,
		TokenURL: tokenEndpoint,
		hc:       &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *Renewer) api() string {
	if strings.TrimSpace(r.APIURL) != "" {
		return strings.TrimSuffix(r.APIURL, "/")
	}
	return subscriptionsAPI
}

var tokenHTTP = &http.Client{Timeout: 20 * time.Second}

func (r *Renewer) tokenURL() string {
	if strings.TrimSpace(r.TokenURL) != "" {
		return r.TokenURL
	}
	return tokenEndpoint
}

// Run renews once at startup - a pod that starts must not inherit someone else's half-expired
// subscription - and then on the interval. Renewal failures are logged loudly and retried on the next
// tick: room ingestion going silent is serious, but it is not a reason to kill a bridge that is still
// answering 1:1 messages and tagged room messages perfectly well.
func (r *Renewer) Run(ctx context.Context) {
	r.renewOnce(ctx)
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.renewOnce(ctx)
		}
	}
}

func (r *Renewer) renewOnce(ctx context.Context) {
	started := time.Now()
	expire, err := r.Renew(ctx)
	if err != nil {
		r.Log.Error("room listener renewal failed; group rooms go deaf at the old expiry",
			"err", err, "interval", r.Interval.String(), "latency_ms", time.Since(started).Milliseconds())
		return
	}
	r.Log.Info("room listener renewed", "expire", expire, "latency_ms", time.Since(started).Milliseconds())
}

// Renew deletes the live subscription and creates it again, returning the new expireTime.
func (r *Renewer) accessToken(ctx context.Context) (string, error) {
	return r.Cred.accessToken(ctx, r.tokenURL())
}

func (r *Renewer) Renew(ctx context.Context) (string, error) {
	token, err := r.accessToken(ctx)
	if err != nil {
		return "", err
	}

	name := r.name
	if name == "" || strings.HasPrefix(name, "operations/") {
		// An operation name is never a subscription name, so it is treated as "we do not know the name"
		// and rediscovered. Nothing to delete is also a fine outcome: create simply succeeds.
		if created, err := r.create(ctx, token); err == nil {
			r.name = created.name
			return created.expire, nil
		} else {
			found, isDuplicate := err.(*duplicateSubscriptionError)
			if !isDuplicate {
				return "", err
			}
			name = found.name
		}
	}

	if err := r.delete(ctx, token, name); err != nil {
		// The subscription may already have expired away. Create next regardless; if both fail the
		// caller sees the create error, which is the one that describes the real problem.
		r.Log.Warn("could not delete the old room listener (it may already have expired)", "err", err)
	}
	created, err := r.create(ctx, token)
	if err != nil {
		return "", err
	}
	r.name = created.name
	return created.expire, nil
}

type createdSubscription struct {
	name   string
	expire string
}

type duplicateSubscriptionError struct {
	name string
	err  error
}

func (e *duplicateSubscriptionError) Error() string { return e.err.Error() }

// AccessToken exchanges the refresh token for an access token. It is exported because the read-only
// sibling of the bridge (cmd/pi-chat-audit) needs the same credential without a Renewer.
func (c Credential) AccessToken(ctx context.Context) (string, error) {
	return c.accessToken(ctx, tokenEndpoint)
}

func (c Credential) accessToken(ctx context.Context, endpoint string) (string, error) {
	form := url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"refresh_token": {c.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	res, err := tokenHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("chatevents: token refresh: %w", err)
	}
	defer res.Body.Close()
	// Read first, decode second: a token endpoint that answers with an HTML page (which is what a
	// consent or front-door failure looks like) must report the status and the first bytes, not just
	// "invalid character '<'".
	body, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("chatevents: token refresh: read body: %w", err)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("chatevents: token refresh (HTTP %d): decode: %w body=%q", res.StatusCode, err, strings.TrimSpace(string(body)))
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("chatevents: token refresh refused: %s %s", payload.Error, payload.ErrorDesc)
	}
	return payload.AccessToken, nil
}

func (r *Renewer) create(ctx context.Context, token string) (createdSubscription, error) {
	payload := map[string]any{
		"targetResource": r.Target,
		"eventTypes":     []string{r.Event},
		"payloadOptions": map[string]any{"includeResource": true, "fieldMask": r.Mask},
		"notificationEndpoint": map[string]string{
			"pubsubTopic": "projects/" + r.Project + "/topics/" + r.Topic,
		},
	}
	res, err := r.do(ctx, http.MethodPost, r.api()+"/subscriptions", token, payload)
	if err != nil {
		return createdSubscription{}, err
	}
	var resp struct {
		Name       string `json:"name"`
		ExpireTime string `json:"expireTime"`
		Response   struct {
			Name       string `json:"name"`
			ExpireTime string `json:"expireTime"`
		} `json:"response"`
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Reason   string            `json:"reason"`
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.body, &resp); err != nil {
		return createdSubscription{}, fmt.Errorf("chatevents: decode create response (HTTP %d): %w body=%q", res.status, err, summarize(res.body))
	}
	if res.status >= 400 {
		// A 409 is the one thing this API is generous about: the error metadata names the subscription
		// that already exists for this target, which is the only way to find it, because listing
		// subscriptions is refused outright.
		for _, detail := range resp.Error.Details {
			if existing := detail.Metadata["current_subscription"]; existing != "" {
				return createdSubscription{}, &duplicateSubscriptionError{
					name: existing,
					err:  fmt.Errorf("chatevents: subscription already exists: %s", first(resp.Error.Message, summarize(res.body))),
				}
			}
		}
		return createdSubscription{}, fmt.Errorf("chatevents: create: %d: %s", res.status, first(resp.Error.Message, summarize(res.body)))
	}
	name := first(resp.Response.Name, resp.Name)
	expire := first(resp.ExpireTime, resp.Response.ExpireTime)
	if strings.HasPrefix(name, "operations/") {
		// Creating a subscription is long-running: the create response names the *operation*, and the
		// subscription it built is inside that operation's response. A renewer that stores the operation
		// name spends every later renewal trying to DELETE /v1/operations/..., which can never succeed,
		// while the real subscription dies at its expiry. Measured on the live API: the create that
		// worked returned name "operations/CksK..." and put the expiry in the response.
		resolved, opExpire, err := r.resolveOperation(ctx, token, name)
		if err != nil {
			// Not fatal. An empty name means the next renewal rediscovers the live subscription through
			// the 409 metadata, which is the discovery method this API actually supports.
			r.Log.Warn("create returned an operation that did not resolve to a subscription name; the next renewal will rediscover it",
				"operation", name, "err", err)
			return createdSubscription{name: "", expire: first(expire, opExpire)}, nil
		}
		name, expire = resolved, first(expire, opExpire)
	}
	if name == "" {
		return createdSubscription{}, fmt.Errorf("chatevents: create returned no subscription name (%s)", summarize(res.body))
	}
	if expire == "" {
		// Long-running create: the response did not carry an expiry, so read the subscription back.
		if fresh, err := r.get(ctx, token, name); err == nil {
			expire = fresh
		}
	}
	return createdSubscription{name: name, expire: expire}, nil
}

// resolveOperation polls a long-running operation until it names the subscription it created.
func (r *Renewer) resolveOperation(ctx context.Context, token, operation string) (string, string, error) {
	for attempt := 0; attempt < 6; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", "", ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		res, err := r.do(ctx, http.MethodGet, r.api()+"/"+operation, token, nil)
		if err != nil {
			return "", "", err
		}
		if res.status >= 400 {
			return "", "", fmt.Errorf("chatevents: get operation %s: %d: %s", operation, res.status, summarize(res.body))
		}
		var op struct {
			Done     bool `json:"done"`
			Response struct {
				Name       string `json:"name"`
				ExpireTime string `json:"expireTime"`
			} `json:"response"`
		}
		if err := json.Unmarshal(res.body, &op); err != nil {
			return "", "", fmt.Errorf("chatevents: decode operation %s (HTTP %d): %w body=%q", operation, res.status, err, summarize(res.body))
		}
		if op.Response.Name != "" {
			return op.Response.Name, op.Response.ExpireTime, nil
		}
		if op.Done {
			return "", "", fmt.Errorf("chatevents: operation %s finished without naming a subscription (%s)", operation, summarize(res.body))
		}
	}
	return "", "", fmt.Errorf("chatevents: operation %s did not finish within the renewal window", operation)
}

func (r *Renewer) delete(ctx context.Context, token, name string) error {
	if strings.HasPrefix(strings.TrimSpace(name), "operations/") {
		return fmt.Errorf("chatevents: refusing to delete %q: that is a long-running operation, not a subscription", name)
	}
	res, err := r.do(ctx, http.MethodDelete, r.api()+"/"+name, token, nil)
	if err != nil {
		return err
	}
	if res.status >= 400 {
		return fmt.Errorf("chatevents: delete %s: %d: %s", name, res.status, summarize(res.body))
	}
	return nil
}

func (r *Renewer) get(ctx context.Context, token, name string) (string, error) {
	res, err := r.do(ctx, http.MethodGet, r.api()+"/"+name, token, nil)
	if err != nil {
		return "", err
	}
	if res.status >= 400 {
		return "", fmt.Errorf("chatevents: get %s: %d: %s", name, res.status, summarize(res.body))
	}
	var resp struct {
		ExpireTime string `json:"expireTime"`
	}
	if err := json.Unmarshal(res.body, &resp); err != nil {
		return "", fmt.Errorf("chatevents: decode subscription (HTTP %d): %w body=%q", res.status, err, summarize(res.body))
	}
	return resp.ExpireTime, nil
}

// apiResponse carries the status alongside the body instead of folding a 4xx into an error, because a
// 409 here carries the information the renewal needs rather than a failure to report.
type apiResponse struct {
	status int
	body   []byte
}

func (r *Renewer) do(ctx context.Context, method, endpoint, token string, payload any) (apiResponse, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return apiResponse{}, err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return apiResponse{}, fmt.Errorf("chatevents: %s %s: %w", method, endpoint, err)
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("content-type", "application/json")
	res, err := r.hc.Do(req)
	if err != nil {
		return apiResponse{}, fmt.Errorf("chatevents: %s %s: %w", method, endpoint, err)
	}
	defer res.Body.Close()
	// A subscription API error body is small. A misbehaving proxy should not be able to spend the
	// bridge's memory on it.
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil {
		return apiResponse{}, fmt.Errorf("chatevents: %s %s: read body: %w", method, endpoint, err)
	}
	return apiResponse{status: res.StatusCode, body: raw}, nil
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// summarize keeps an API error readable without dumping a whole payload into the log.
func summarize(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 400 {
		return text[:400] + "…"
	}
	if text == "" {
		return "empty response body"
	}
	return text
}
