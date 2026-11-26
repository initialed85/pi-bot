// senders.go answers the one question the room listener cannot answer from its own payload.
//
// Measured on the deployed listener (2026-11-25T07:00Z, logged verbatim by the bridge): a room event's
// sender object is exactly
//
//	{"name":"users/108049318480764095606","type":"HUMAN"}
//
// no email, no displayName, no legacyId. The allowlist is written in emails, so a room message is
// unactionable unless that id is turned into one. What does *not* work with a human OAuth credential,
// all measured rather than assumed:
//
//	chat.googleapis.com/v1/users/<id>                 -> 404 (no such method for this credential)
//	chat.googleapis.com/v1/users/<id>?legacyId=<id>   -> 404
//	chat.googleapis.com/v1/spaces/<s>/members          -> 403 insufficient authentication scopes
//
// What does work is the read the bridge already uses for history review, under the same credential:
//
//	GET chat.googleapis.com/v1/spaces/<space>/messages?pageSize=25&orderBy=createTime desc&fields=messages/name,messages/sender
//		200, and each sender carries name AND email AND displayName together, e.g.
//		{"name":"users/113070394191408311024","email":"initialed85@gmail.com","displayName":"initialed85 _","type":"HUMAN"}
//
// `orderBy=createTime desc` is load-bearing and was not there originally. Measured 2026-11-26: the API
// answers ascending, so a 25-message read of a space created in 2013 returns 2013 - the lookup was
// comparing a room member's id against the oldest messages in the room and could only ever miss. The
// deployed symptom is that every room sender resolved `via=seed` and none ever `via=lookup`.
//
// So identity is resolved by looking at recent messages *in the space the message landed in* and taking
// the email that Chat itself published next to that user id, then caching it. The scope is no wider than
// the subscription already is: a Workspace Events subscription on this human's spaces already delivers
// every message in them to this process; this is the same data, read once, with the text discarded.
package chatevents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	chatAPI        = "https://chat.googleapis.com/v1"
	messagesFields = "messages/name,messages/sender"
	// missCooldown bounds how often an unidentified user can cost an API call. Room traffic is light and
	// humans repeat, but "one lookup per message from someone who is never going to be allowlisted" is a
	// way to make an outbound call per message for no benefit.
	missCooldown = 5 * time.Minute
	lookupTTL    = 24 * time.Hour
)

// SenderIdentity is what Chat published about a user id in the space it was asked about.
type SenderIdentity struct {
	UserID      string
	Email       string
	DisplayName string
}

// SenderLookup turns a users/<id> into an email using a human OAuth credential, and remembers.
type SenderLookup struct {
	cred     Credential
	http     *http.Client
	log      *slog.Logger
	tokenURL string
	// ChatEndpoint is a test seam and a self-documented constant: the base is the API version root, and
	// the space path is appended. Getting the base wrong is the failure mode that already bit this
	// package once (a wrong base produced an HTML 404 and a decode error, not a clear message).
	ChatEndpoint string
	// PageSize bounds one read. 25 is what a single page of a busy room covers; the point is a recent
	// message from the same person, not a full history scan.
	PageSize int

	mu     sync.Mutex
	byID   map[string]SenderIdentity
	cached map[string]time.Time
	calls  int
}

// NewSenderLookup builds the lookup from the same credential the room subscription is renewed with.
func NewSenderLookup(cred Credential, log *slog.Logger) *SenderLookup {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &SenderLookup{
		cred:         cred,
		http:         &http.Client{Timeout: 20 * time.Second},
		log:          log,
		ChatEndpoint: chatAPI,
		PageSize:     25,
		byID:         map[string]SenderIdentity{},
		cached:       map[string]time.Time{},
	}
}

// WithTokenURL points the OAuth exchange somewhere else. Only tests use it; production wants the real
// Google endpoint the renewal already uses.
func (s *SenderLookup) WithTokenURL(url string) *SenderLookup {
	s.tokenURL = url
	return s
}

// WithChatEndpoint points the Chat read somewhere else. Only tests use it.
func (s *SenderLookup) WithChatEndpoint(url string) *SenderLookup {
	s.ChatEndpoint = url
	return s
}

// Resolve returns the email Chat associates with userID in the given space. found is false when the
// space's recent messages do not contain that user id - which is a normal outcome, not an error, and
// must keep the caller failing closed.
func (s *SenderLookup) Resolve(ctx context.Context, space, userID string) (SenderIdentity, bool, error) {
	userID = strings.TrimSpace(userID)
	if space == "" || userID == "" {
		return SenderIdentity{}, false, fmt.Errorf("chatevents: sender lookup needs a space and a user id, got space=%q user=%q", space, userID)
	}

	keys := identityAliases(userID)
	if id, ok := s.cachedIdentity(keys); ok {
		return id, true, nil
	}
	if s.inCooldown(space, keys) {
		return SenderIdentity{}, false, nil
	}

	token, err := s.cred.accessToken(ctx, s.tokenURLOrDefault())
	if err != nil {
		return SenderIdentity{}, false, err
	}

	url := fmt.Sprintf("%s/%s/messages?pageSize=%d&orderBy=createTime%%20desc&fields=%s",
		strings.TrimRight(s.ChatEndpoint, "/"), strings.TrimPrefix(space, "/"), s.PageSize, messagesFields)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SenderIdentity{}, false, fmt.Errorf("chatevents: sender lookup request: %w", err)
	}
	req.Header.Set("authorization", "Bearer "+token)

	res, err := s.http.Do(req)
	if err != nil {
		s.noteMiss(space, keys)
		return SenderIdentity{}, false, fmt.Errorf("chatevents: sender lookup %s: %w", space, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return SenderIdentity{}, false, fmt.Errorf("chatevents: sender lookup read body: %w", err)
	}
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if res.StatusCode != http.StatusOK {
		// Snippet, not body: Google answers a scope or permission problem with an HTML page, and an HTML
		// page in a log field is how a log line turns into a mess.
		s.noteMiss(space, keys)
		return SenderIdentity{}, false, fmt.Errorf("chatevents: sender lookup %s: http %d: %s", space, res.StatusCode, snippet(body))
	}

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
	if err := json.Unmarshal(body, &page); err != nil {
		// Naming the status and the first bytes is the whole point. This package has already been bitten
		// once by a wrong base URL: Google's front door answered HTML, JSON decoding failed, and the log
		// said "invalid character '<'" for a problem that was really "you asked the wrong path".
		return SenderIdentity{}, false, fmt.Errorf("chatevents: sender lookup decode %s: %w: answered http %d with %q", space, err, res.StatusCode, snippet(body))
	}

	for _, m := range page.Messages {
		id := SenderIdentity{UserID: m.Sender.Name, Email: strings.TrimSpace(m.Sender.Email), DisplayName: m.Sender.DisplayName}
		if id.UserID == "" || id.Email == "" {
			// A sender with no email is exactly the shape the listener ships; caching it would teach the
			// bridge nothing and would be cached as a fact.
			continue
		}
		s.remember(id)
	}

	for _, alias := range keys {
		if id, ok := s.cachedIdentity([]string{alias}); ok {
			s.log.Info("sender identity resolved from the space's own message history",
				"id", alias, "user", id.Email, "space", space)
			return id, true, nil
		}
	}

	s.noteMiss(space, keys)
	return SenderIdentity{}, false, nil
}

func (s *SenderLookup) tokenURLOrDefault() string {
	if strings.TrimSpace(s.tokenURL) != "" {
		return s.tokenURL
	}
	return tokenEndpoint
}

// identityAliases returns every spelling a user id might arrive with: the resource name and the bare
// legacy id. The listener ships "users/<id>"; app-credential messages sometimes ship only one of the
// two, and the room path has been observed shipping a bare legacyId in some deliveries.
func identityAliases(userID string) []string {
	out := []string{userID}
	if bare, ok := strings.CutPrefix(userID, "users/"); ok && bare != "" {
		out = append(out, bare)
	} else if !strings.Contains(userID, "/") {
		out = append(out, "users/"+userID)
	}
	return out
}

func (s *SenderLookup) remember(id SenderIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, alias := range identityAliases(id.UserID) {
		s.byID[alias] = id
		s.cached[alias] = time.Now()
	}
}

func (s *SenderLookup) cachedIdentity(keys []string) (SenderIdentity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		id, ok := s.byID[key]
		if !ok {
			continue
		}
		if !s.cached[key].IsZero() && time.Since(s.cached[key]) > lookupTTL {
			// Not a permanent fact: an account can be deleted or an email can change. A day is old enough
			// that a busy bridge does not re-read for no reason and young enough to notice a change.
			delete(s.byID, key)
			delete(s.cached, key)
			continue
		}
		return id, true
	}
	return SenderIdentity{}, false
}

func (s *SenderLookup) inCooldown(space string, keys []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := missKey(space, keys)
	if at, ok := s.cached[key]; ok && time.Since(at) < missCooldown {
		return true
	}
	return false
}

func (s *SenderLookup) noteMiss(space string, keys []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached[missKey(space, keys)] = time.Now()
}

func missKey(space string, keys []string) string {
	return "miss:" + space + ":" + strings.Join(keys, ",")
}

// Calls reports how many Chat reads this lookup has actually made. Tests use it to prove the cache is
// load bearing rather than decorative.
func (s *SenderLookup) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	return text
}
