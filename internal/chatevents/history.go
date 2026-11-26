// history.go reads the conversation a room message arrived in, under Ed's OAuth credential.
//
// Why this exists is a measurement, not a preference. Before it, the bridge could see the message that
// triggered it and nothing else, which produced two failures that are both visible in the deployed logs:
//
//   - The classifier judged a bare line. "so what's the scoop mr. bot, are you working today or not?"
//     is a question to pi-chat; stripped of the thread it sits in, it looks like anything.
//   - Thread ownership was recovered with `chatapi.HasBotMessage`, an app-credential read the shipped
//     app has no scope for: it answers 403 ACCESS_TOKEN_SCOPE_INSUFFICIENT, the lookup returned an
//     error, and `groupMessageEligible` failed closed - the message was dropped before any model call.
//     (2026-11-26T03:25:08Z, verbatim in the bridge log.)
//
// What the human credential can do, probed against the real API on 2026-11-26 (see the shapes asserted
// in history_test.go, all four measured rather than guessed):
//
//	GET /v1/spaces/<s>/messages?orderBy=createTime desc&pageSize=N     -> 200, newest first
//	GET /v1/spaces/<s>/messages?filter=createTime > "..."              -> 200
//	GET /v1/spaces/<s>/messages/<leaf>.<leaf>                           -> 200, a thread's root message
//	GET /v1/spaces/<s>/messages?filter=thread = "spaces/<s>/threads/T" -> 400 Invalid filter query
//	GET /v1/spaces/<s>/threads/T/messages                               -> 404 HTML
//
// So a thread cannot be listed directly and cannot be filtered for. What works is the space window plus
// client-side selection by thread name, and the root fetched by name - Chat names a top-level message
// `<id>.<id>` and its thread `<id>`, which makes a thread's root addressable from the thread name alone.
// That is how "context when we get @'d into a thread" is satisfied even when the root is days old: the
// root of spaces/qi7VbgAAAAE/threads/3fbZOFDt3zI is from 2026-11-23 while the message needing it is
// from 2026-11-26.
//
// One read serves identity, ownership and context. That is deliberate: the endpoint is warm and the
// model is cheap, but the Chat API is neither, and a per-message pipeline that made three reads per
// message would be slower than the classifier it is feeding.
package chatevents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// historyFields is the partial response the context window needs. Asking for the whole message
	// (no fields param) also returns avatar blobs and formatted text and triples the bytes.
	historyFields  = "messages/name,messages/thread,messages/sender,messages/text,messages/createTime"
	messageFields  = "name,thread,sender,text,createTime"
	historyWindow  = 20
	historyMaxAge  = 48 * time.Hour
	historyCacheOK = 5 * time.Second
)

// Message is one Chat message as the human credential sees it.
type Message struct {
	// Name is the resource name, "spaces/<s>/messages/<id>".
	Name string
	// Thread is "spaces/<s>/threads/<id>", empty for a top-level message Chat never threaded.
	Thread string
	// Sender is a users/<id>; Email and DisplayName are what Chat published alongside it.
	Sender      string
	Email       string
	DisplayName string
	// Bot is sender.type != HUMAN: pi-chat's own posts and other integrations.
	Bot        bool
	Text       string
	CreateTime time.Time
}

// Context is the window a decision is made on.
type Context struct {
	// Messages is oldest first, so a transcript reads the way the conversation happened.
	Messages []Message
	// Root is the message that opened the thread, when one was asked for and found. It is kept apart
	// because it can be much older than the window: a thread opened nine months ago is still the thing
	// a reply in it is about.
	Root *Message
	// ReadAt is when the window came back, so a cached read can be judged stale.
	ReadAt time.Time
}

// Transcript renders the window for the classifier: short, ordered, attributed, one line each.
// Names come from Chat's own displayName, so the model can tell who is talking without being handed
// anyone's email address.
func (c Context) Transcript(limit int, maxChars int) string {
	var b strings.Builder
	msgs := c.Messages
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	if c.Root != nil && (limit == 0 || len(msgs) == 0 || msgs[0].Name != c.Root.Name) {
		writeLine(&b, *c.Root)
		b.WriteString("…\n")
	}
	for _, m := range msgs {
		writeLine(&b, m)
	}
	out := b.String()
	if maxChars > 0 && len(out) > maxChars {
		out = out[len(out)-maxChars:]
		if nl := strings.IndexByte(out, '\n'); nl >= 0 && nl < len(out)-1 {
			out = out[nl+1:]
		}
	}
	return out
}

func writeLine(b *strings.Builder, m Message) {
	who := m.DisplayName
	if who == "" {
		who = m.Sender
	}
	if m.Bot {
		who += " (bot)"
	}
	when := m.CreateTime.UTC().Format("15:04")
	text := strings.Join(strings.Fields(m.Text), " ")
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	fmt.Fprintf(b, "[%s] %s: %s\n", when, who, text)
}

// History reads room context under a human OAuth credential, with a small cache: a mention and a gate
// decision on the same message should cost one Chat read, not two.
type History struct {
	cred Credential
	http *http.Client
	log  *slog.Logger
	// ChatEndpoint is a test seam; production wants the real API root.
	ChatEndpoint string
	// Window is how many recent messages one read asks for, and MaxAge is how much of them is worth
	// sending to a classifier. Both bounds matter: a 20-message window of a room that has been quiet
	// since 2013 is not context, it is an archive.
	Window  int
	MaxAge  time.Duration
	MaxChar int

	tokenURL string
	mu       sync.Mutex
	windows  map[string]windowEntry
	roots    map[string]rootEntry
	calls    int
}

func NewHistory(cred Credential, log *slog.Logger) *History {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &History{
		cred:         cred,
		http:         &http.Client{Timeout: 20 * time.Second},
		log:          log,
		ChatEndpoint: chatAPI,
		Window:       historyWindow,
		MaxAge:       historyMaxAge,
		MaxChar:      4000,
		tokenURL:     "",
		windows:      map[string]windowEntry{},
		roots:        map[string]rootEntry{},
	}
}

// windowEntry is a cached space window; readAt drives the TTL.
type windowEntry struct {
	messages []Message
	readAt   time.Time
}

// rootEntry is a cached thread root. found=false is cached too: a thread whose root was deleted, or is
// simply not addressable, would otherwise cost a failed GET on every message in it.
type rootEntry struct {
	message Message
	found   bool
	readAt  time.Time
}

// WithTokenURL points the OAuth exchange somewhere else. Tests only.
func (h *History) WithTokenURL(url string) *History { h.tokenURL = url; return h }

// WithChatEndpoint points the Chat read somewhere else. Tests only.
func (h *History) WithChatEndpoint(url string) *History { h.ChatEndpoint = url; return h }

// Calls counts API reads, so a test can prove one message costs one read.
func (h *History) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// Context returns the recent window of a space, oldest first, narrowed to a thread when one is given.
//
// Narrowing happens client-side because the API refuses to filter by thread (400, measured). A thread
// that is older than the window still gets its root, fetched by name, so the classifier is never
// reading a reply with nothing to reply to.
//
// The space window is cached per space rather than per space+thread, because one arriving message needs
// it twice: once to find out who sent it, since a room event ships a bare users/<id> with no email, and
// once to build the transcript. Two reads of the same twenty messages would make the context the slowest
// part of a decision that has three seconds to spend - one window read measured 1.6s against the real
// API on 2026-11-26.
func (h *History) Context(ctx context.Context, space, thread string) (Context, error) {
	if space == "" {
		return Context{}, fmt.Errorf("chatevents: history needs a space")
	}
	window, err := h.window(ctx, space)
	if err != nil {
		return Context{}, err
	}
	c := Context{ReadAt: window.readAt}
	if thread == "" {
		c.Messages = window.messages
		return c, nil
	}
	for _, m := range window.messages {
		if m.Thread == thread {
			c.Messages = append(c.Messages, m)
		}
	}
	// The thread may be entirely outside the window. Its root is still the context that matters, and an
	// empty transcript is honest about a quiet thread; a stale one is not.
	if root, ok := h.root(ctx, space, thread); ok {
		c.Root = &root
		if len(c.Messages) == 0 {
			c.Messages = []Message{root}
		}
	}
	return c, nil
}

// window returns the cached recent messages of a space, reading Google only when the cache is cold.
func (h *History) window(ctx context.Context, space string) (windowEntry, error) {
	h.mu.Lock()
	if e, ok := h.windows[space]; ok && time.Since(e.readAt) <= historyCacheOK {
		h.mu.Unlock()
		return e, nil
	}
	h.mu.Unlock()

	messages, err := h.read(ctx, space, "")
	if err != nil {
		return windowEntry{}, err
	}
	e := windowEntry{messages: messages, readAt: time.Now().UTC()}
	h.mu.Lock()
	if len(h.windows) > 64 {
		h.windows = map[string]windowEntry{}
	}
	h.windows[space] = e
	h.mu.Unlock()
	return e, nil
}

// root fetches, or remembers, the message that opened a thread. A thread whose root cannot be read is
// remembered as not found, so a deleted root does not cost a failed GET on every message in the thread.
func (h *History) root(ctx context.Context, space, thread string) (Message, bool) {
	key := space + "|" + thread
	h.mu.Lock()
	if e, ok := h.roots[key]; ok && time.Since(e.readAt) <= historyCacheOK {
		h.mu.Unlock()
		return e.message, e.found
	}
	h.mu.Unlock()

	message, found, err := h.rootOf(ctx, space, thread)
	if err != nil {
		// Losing a root read is not worth losing the window over, but it is not remembered either: the
		// next message in the thread should get the chance to find it.
		h.log.Debug("thread root read failed", "thread", thread, "err", err)
		return Message{}, false
	}
	h.mu.Lock()
	if len(h.roots) > 256 {
		h.roots = map[string]rootEntry{}
	}
	h.roots[key] = rootEntry{message: message, found: found, readAt: time.Now().UTC()}
	h.mu.Unlock()
	return message, found
}

// HasBotMessage answers, from the same window, whether pi-chat already speaks in this thread. It
// replaces the app-credential read that 403s in production and whose failure dropped messages unseen.
func (h *History) HasBotMessage(ctx context.Context, space, thread string) (bool, error) {
	c, err := h.Context(ctx, space, thread)
	if err != nil {
		return false, err
	}
	for _, m := range c.Messages {
		if m.Bot {
			return true, nil
		}
	}
	return false, nil
}

func (h *History) read(ctx context.Context, space, filter string) ([]Message, error) {
	token, err := h.cred.accessToken(ctx, h.tokenURLOrDefault())
	if err != nil {
		return nil, fmt.Errorf("chatevents: history token: %w", err)
	}
	// "desc" is the whole point: the API answers ascending by default, and the first 20 messages of a
	// space opened in 2013 are 2013. The query is assembled by hand, not with url.Values, so that the
	// logged request keeps the same readable `fields` shape that was verified against Google live;
	// escaping commas and slashes is accepted, but nobody comparing the two would recognise them.
	query := "pageSize=" + strconv.Itoa(h.Window) + "&orderBy=" + escapeQueryValue("createTime desc") +
		"&fields=" + historyFields
	if filter != "" {
		query += "&filter=" + escapeQueryValue(filter)
	}
	endpoint := fmt.Sprintf("%s/%s/messages?%s", strings.TrimRight(h.ChatEndpoint, "/"),
		strings.TrimPrefix(space, "/"), query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("chatevents: history request: %w", err)
	}
	req.Header.Set("authorization", "Bearer "+token)

	body, err := h.do(req, "history "+space)
	if err != nil {
		return nil, err
	}
	var page struct {
		Messages []rawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("chatevents: history decode %s: %w: answered with %q", space, err, snippet(body))
	}
	out := make([]Message, 0, len(page.Messages))
	cutoff := time.Now().UTC().Add(-h.MaxAge)
	for _, raw := range page.Messages {
		m := raw.parse()
		if m.Name == "" {
			continue
		}
		out = append(out, m)
	}
	// The API answers newest first; a transcript is easier to read, and to reason about, oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	// Drop the ancient tail. Bounded by MaxAge so a room nobody has used since 2013 contributes nothing.
	for len(out) > 0 && out[0].CreateTime.Before(cutoff) {
		out = out[1:]
	}
	return out, nil
}

// rootOf fetches the message that opened a thread. Chat names a top-level message `<id>.<id>` and the
// thread it starts `<id>`, so the root is derivable without another list call.
func (h *History) rootOf(ctx context.Context, space, thread string) (Message, bool, error) {
	leaf := thread
	if i := strings.LastIndexByte(thread, '/'); i >= 0 {
		leaf = thread[i+1:]
	}
	if leaf == "" {
		return Message{}, false, nil
	}
	token, err := h.cred.accessToken(ctx, h.tokenURLOrDefault())
	if err != nil {
		return Message{}, false, fmt.Errorf("chatevents: root token: %w", err)
	}
	url := fmt.Sprintf("%s/%s/messages/%s.%s?fields=%s", strings.TrimRight(h.ChatEndpoint, "/"),
		strings.TrimPrefix(space, "/"), leaf, leaf, messageFields)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Message{}, false, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	body, err := h.do(req, "root "+thread)
	if err != nil {
		return Message{}, false, err
	}
	var raw rawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return Message{}, false, fmt.Errorf("chatevents: root decode %s: %w", thread, err)
	}
	m := raw.parse()
	if m.Name == "" {
		return Message{}, false, nil
	}
	return m, true, nil
}

func (h *History) do(req *http.Request, what string) ([]byte, error) {
	res, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chatevents: %s: %w", what, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("chatevents: %s read body: %w", what, err)
	}
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chatevents: %s: http %d: %s", what, res.StatusCode, snippet(body))
	}
	return body, nil
}

// escapeQueryValue percent-encodes only what a query value cannot carry raw. Kept minimal so the
// request logged for a history read looks like the one that was verified against the live API.
func escapeQueryValue(s string) string {
	if !strings.ContainsAny(s, " \"&=+#%<>") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ':
			b.WriteString("%20")
		case '"':
			b.WriteString("%22")
		case '&':
			b.WriteString("%26")
		case '=':
			b.WriteString("%3D")
		case '+':
			b.WriteString("%2B")
		case '#':
			b.WriteString("%23")
		case '%':
			b.WriteString("%25")
		case '<':
			b.WriteString("%3C")
		case '>':
			b.WriteString("%3E")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (h *History) tokenURLOrDefault() string {
	if h.tokenURL != "" {
		return h.tokenURL
	}
	return tokenEndpoint
}

// rawMessage is the wire shape. Text is decoded tolerantly because the REST layer has answered both
// `"text": "…"` and the structured `{"textSegments":[…]}` form depending on the fields requested, and
// a decoder that picks one loses the other silently.
type rawMessage struct {
	Name   string `json:"name"`
	Thread struct {
		Name string `json:"name"`
	} `json:"thread"`
	Sender struct {
		Name        string `json:"name"`
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
		Type        string `json:"type"`
	} `json:"sender"`
	Text       json.RawMessage `json:"text"`
	CreateTime time.Time       `json:"createTime"`
}

func (r rawMessage) parse() Message {
	m := Message{
		Name:        r.Name,
		Thread:      r.Thread.Name,
		Sender:      r.Sender.Name,
		Email:       strings.TrimSpace(r.Sender.Email),
		DisplayName: r.Sender.DisplayName,
		Bot:         r.Sender.Type != "" && !strings.EqualFold(r.Sender.Type, "HUMAN"),
		CreateTime:  r.CreateTime,
	}
	if len(r.Text) > 0 {
		var plain string
		if err := json.Unmarshal(r.Text, &plain); err == nil {
			m.Text = plain
		} else {
			var structured struct {
				TextSegments []struct {
					Text string `json:"text"`
				} `json:"textSegments"`
			}
			if err := json.Unmarshal(r.Text, &structured); err == nil {
				parts := make([]string, 0, len(structured.TextSegments))
				for _, s := range structured.TextSegments {
					parts = append(parts, s.Text)
				}
				m.Text = strings.Join(parts, "")
			}
		}
	}
	return m
}
