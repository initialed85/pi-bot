package chatevents

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The payloads below are transcriptions of what the real Chat API answered on 2026-11-26 under Ed's
// OAuth credential, including the two shapes that are easy to get wrong: the list comes back newest
// first, and a space created long ago has a 2013 message sitting at the start of every ascending read.

type historyHarness struct {
	t *testing.T
	// pages maps "list" / "root" to the JSON each read answers, captured so the test can assert on the
	// query the reader actually built.
	queries  []string
	paths    []string
	listBody string
	rootBody string
	listCode int
	rootCode int
	server   *httptest.Server
	token    *httptest.Server
}

func newHistoryHarness(t *testing.T) (*historyHarness, *History) {
	t.Helper()
	h := &historyHarness{t: t, listCode: 200, rootCode: 200}
	h.token = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fake","expires_in":3600,"token_type":"Bearer"}`)
	}))
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.queries = append(h.queries, r.URL.RawQuery)
		h.paths = append(h.paths, r.URL.Path)
		w.Header().Set("content-type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/messages") {
			if h.listCode != 200 {
				http.Error(w, "forbidden", h.listCode)
				return
			}
			_, _ = io.WriteString(w, h.listBody)
			return
		}
		if h.rootCode != 200 {
			http.Error(w, "gone", h.rootCode)
			return
		}
		_, _ = io.WriteString(w, h.rootBody)
	}))
	hr := NewHistory(Credential{ClientID: "cid", ClientSecret: "secret", RefreshToken: "refresh"},
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithTokenURL(h.token.URL).
		WithChatEndpoint(h.server.URL)
	return h, hr
}

func (h *historyHarness) close() {
	h.server.Close()
	h.token.Close()
}

func since(minutes int) string {
	return time.Now().UTC().Add(-time.Duration(minutes) * time.Minute).Format("2006-01-02T15:04:05.000000Z")
}

func msgJSON(id, thread, sender, email, display, senderType, text, when string) string {
	parts := []string{`"name":"` + id + `"`}
	if thread != "" {
		parts = append(parts, `"thread":{"name":"`+thread+`"}`)
	}
	parts = append(parts,
		`"sender":{"name":"`+sender+`","email":"`+email+`","displayName":"`+display+`","type":"`+senderType+`"}`,
		`"text":{"textSegments":[{"text":"`+text+`"}]}`,
		`"createTime":"`+when+`"`)
	return "{" + strings.Join(parts, ",") + "}"
}

// Newest-first on the wire, oldest-first out: a transcript that inverts the conversation is worse than
// no transcript, because the classifier reads the conclusion before the question.
func TestHistory_AsksWithDescOrderAndReturnsOldestFirst(t *testing.T) {
	h, hr := newHistoryHarness(t)
	defer h.close()
	h.listBody = `{"messages":[` +
		msgJSON("spaces/S/messages/c.c", "spaces/S/threads/c", "users/3", "amy@example.com", "Amy Beech", "HUMAN", "third", since(2)) + `,` +
		msgJSON("spaces/S/messages/b.b", "spaces/S/threads/b", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "second", since(10)) + `,` +
		msgJSON("spaces/S/messages/a.a", "spaces/S/threads/a", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "first", "2013-05-22T05:54:07.020039Z") +
		`]}`

	c, err := hr.Context(context.Background(), "spaces/S", "")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if len(h.queries) != 1 {
		t.Fatalf("a space window should cost exactly one chat read, got %v", h.queries)
	}
	if !strings.Contains(h.queries[0], "orderBy=createTime+desc") && !strings.Contains(h.queries[0], "orderBy=createTime%20desc") {
		t.Fatalf("read did not ask for newest first: query=%q", h.queries[0])
	}
	if !strings.Contains(h.queries[0], "messages/createTime") || !strings.Contains(h.queries[0], "messages/thread") {
		t.Fatalf("read did not ask for the fields the window depends on: %q", h.queries[0])
	}
	if len(c.Messages) != 2 {
		t.Fatalf("got %d messages, want the 2013 one trimmed by MaxAge: %+v", len(c.Messages), c.Messages)
	}
	if c.Messages[0].Text != "second" || c.Messages[1].Text != "third" {
		t.Fatalf("window is not oldest first: %q then %q", c.Messages[0].Text, c.Messages[1].Text)
	}
}

// The API refuses to filter by thread (400, measured), so selection is client-side - and the root that
// gives a reply something to be a reply to comes from the name Chat derives for it.
func TestHistory_NarrowsToAThreadAndFetchesItsRootByName(t *testing.T) {
	h, hr := newHistoryHarness(t)
	defer h.close()
	h.listBody = `{"messages":[` +
		msgJSON("spaces/S/messages/y.y", "spaces/S/threads/T", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "asked in thread", since(3)) + `,` +
		msgJSON("spaces/S/messages/z.z", "spaces/S/threads/OTHER", "users/3", "amy@example.com", "Amy Beech", "HUMAN", "unrelated thread", since(2)) +
		`]}`
	h.rootBody = msgJSON("spaces/S/messages/T.T", "spaces/S/threads/T", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "the original request", "2026-11-23T07:14:56.464575Z")

	c, err := hr.Context(context.Background(), "spaces/S", "spaces/S/threads/T")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if len(c.Messages) != 1 || c.Messages[0].Text != "asked in thread" {
		t.Fatalf("thread narrowing leaked another thread: %+v", c.Messages)
	}
	if c.Root == nil || c.Root.Text != "the original request" {
		t.Fatalf("root missing: %+v", c.Root)
	}
	last := h.paths[len(h.paths)-1]
	if !strings.HasSuffix(last, "/messages/T.T") {
		t.Fatalf("root not fetched by derived name: paths=%v", h.paths)
	}
	tr := c.Transcript(20, 4000)
	if !strings.HasPrefix(tr, "[") || !strings.Contains(tr, "the original request") || !strings.Contains(tr, "asked in thread") {
		t.Fatalf("transcript dropped the root or the reply: %q", tr)
	}
	if strings.Index(tr, "the original request") > strings.Index(tr, "asked in thread") {
		t.Fatalf("transcript puts the reply before the root it replies to: %q", tr)
	}
}

func TestHistory_OwnershipComesFromTheWindow(t *testing.T) {
	h, hr := newHistoryHarness(t)
	defer h.close()
	h.listBody = `{"messages":[` +
		msgJSON("spaces/S/messages/m2.m2", "spaces/S/threads/T", "users/1", "", "pi-chat", "BOT", "🤔", since(1)) + `,` +
		msgJSON("spaces/S/messages/m1.m1", "spaces/S/threads/T", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "hey", since(4)) +
		`]}`
	h.rootBody = msgJSON("spaces/S/messages/T.T", "spaces/S/threads/T", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "root", since(30))

	owned, err := hr.HasBotMessage(context.Background(), "spaces/S", "spaces/S/threads/T")
	if err != nil {
		t.Fatalf("ownership: %v", err)
	}
	if !owned {
		t.Fatal("a thread pi-chat has posted in is not owned")
	}

	h.listBody = `{"messages":[` + msgJSON("spaces/S/messages/m1.m1", "spaces/S/threads/U", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "human only", since(1)) + `]}`
	owned, err = hr.HasBotMessage(context.Background(), "spaces/S", "spaces/S/threads/U")
	if err != nil {
		t.Fatalf("ownership 2: %v", err)
	}
	if owned {
		t.Fatal("a thread with no bot message is not owned")
	}
	if hr.Calls() != 3 {
		t.Fatalf("calls=%d, want one shared space window read and one root read per thread, queries=%v", hr.Calls(), h.queries)
	}
	// A repeat on the same key must not reach the API again; this is the difference between a message
	// costing two Chat reads and costing four because the mention path and the gate path each asked.
	if _, err := hr.HasBotMessage(context.Background(), "spaces/S", "spaces/S/threads/T"); err != nil {
		t.Fatalf("repeat ownership: %v", err)
	}
	if hr.Calls() != 3 {
		t.Fatalf("repeat read was not served from cache: calls=%d", hr.Calls())
	}
}

// One message should not cost the pipeline several Chat reads: the identity lookup and the gate path ask
// for the same space window within a second of each other, and only the thread root is specific to one.
func TestHistory_CachesTheWindowBriefly(t *testing.T) {
	h, hr := newHistoryHarness(t)
	defer h.close()
	h.listBody = `{"messages":[` + msgJSON("spaces/S/messages/m.m", "spaces/S/threads/T", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "hi", since(1)) + `]}`
	h.rootBody = msgJSON("spaces/S/messages/T.T", "spaces/S/threads/T", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "root", since(20))
	if _, err := hr.Context(context.Background(), "spaces/S", "spaces/S/threads/T"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := hr.Context(context.Background(), "spaces/S", "spaces/S/threads/T"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if hr.Calls() != 2 {
		t.Fatalf("calls=%d, want one list read and one root read, got queries=%v", hr.Calls(), h.queries)
	}
}

// A read that fails must be an error the caller can see, not an empty window that quietly looks like a
// room with nothing in it. That distinction is what turned a 403 into dropped messages.
func TestHistory_FailedReadIsAnError(t *testing.T) {
	h, hr := newHistoryHarness(t)
	defer h.close()
	h.listCode = http.StatusForbidden
	h.listBody = ""
	if _, err := hr.Context(context.Background(), "spaces/S", ""); err == nil {
		t.Fatal("a 403 read returned no error")
	} else if !strings.Contains(err.Error(), "http 403") {
		t.Fatalf("error hides the status: %v", err)
	}
}

// The root read is a separate permission surface and can 404 for a thread whose root was deleted. The
// window is still useful on its own.
func TestHistory_MissingRootDoesNotLoseTheWindow(t *testing.T) {
	h, hr := newHistoryHarness(t)
	defer h.close()
	h.listBody = `{"messages":[` + msgJSON("spaces/S/messages/m.m", "spaces/S/threads/T", "users/2", "ed@example.com", "initialed85 _", "HUMAN", "reply", since(1)) + `]}`
	h.rootCode = http.StatusNotFound
	c, err := hr.Context(context.Background(), "spaces/S", "spaces/S/threads/T")
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if c.Root != nil {
		t.Fatalf("root invented for a 404: %+v", c.Root)
	}
	if len(c.Messages) != 1 {
		t.Fatalf("window lost with the root: %+v", c.Messages)
	}
}

func TestHistory_DecodesBothTextShapes(t *testing.T) {
	var plain rawMessage
	if err := json.Unmarshal([]byte(`{"name":"spaces/S/messages/a.a","text":"bare string"}`), &plain); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := plain.parse().Text; got != "bare string" {
		t.Fatalf("plain string text decoded as %q", got)
	}
	var structured rawMessage
	if err := json.Unmarshal([]byte(`{"name":"spaces/S/messages/b.b","text":{"textSegments":[{"text":"one "},{"text":"two"}]}}`), &structured); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := structured.parse().Text; got != "one two" {
		t.Fatalf("segments joined as %q", got)
	}
}

// The classifier is a small model with a short budget; a transcript that grows with the room is how a
// 400ms decision becomes a timeout.
func TestHistory_TranscriptIsBoundedAndKeepsTheNewest(t *testing.T) {
	c := Context{}
	for i := 0; i < 40; i++ {
		c.Messages = append(c.Messages, Message{
			Text:        strings.Repeat("x", 180),
			CreateTime:  time.Now().Add(time.Duration(i) * time.Minute),
			DisplayName: "person" + string(rune('a'+i%26)),
		})
	}
	tr := c.Transcript(20, 1200)
	if len(tr) > 1200 {
		t.Fatalf("transcript %d chars, over the 1200 budget", len(tr))
	}
	if !strings.Contains(tr, "person"+string(rune('a'+39%26))) {
		t.Fatalf("transcript dropped the newest message: %q", tr[len(tr)-200:])
	}
}
