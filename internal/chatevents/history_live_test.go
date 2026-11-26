package chatevents

// The live probe for the context window. history_test.go proves the parsing and the ordering against a
// fake server that serves transcriptions of real responses; it cannot prove that Google still answers
// `orderBy=createTime desc`, that a thread root is addressable at `<leaf>.<leaf>`, or that a sender in
// the window carries an email. All three are load-bearing in the bridge and all three were measured by
// hand on 2026-11-26 - which is exactly the kind of finding that quietly stops being true.
//
//	go test ./internal/chatevents -run LiveHistoryProbe -v
//
// Read-only: two GETs of one space (the window, and one thread's root). Opt-in because it needs Ed's
// OAuth credential files mounted, and because it names real people's rooms in its output.

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveHistoryProbe(t *testing.T) {
	if strings.TrimSpace(os.Getenv("PI_CHAT_REAL_PROBE")) == "" {
		t.Skip("set PI_CHAT_REAL_PROBE=1 to read the real Chat API (read-only: two GETs of one space)")
	}
	cred, err := LoadCredential(
		envOr("PI_CHAT_OAUTH_CLIENT_FILE", "/root/oauth-client.json"),
		envOr("PI_CHAT_OAUTH_SECRET_FILE", ""),
		envOr("PI_CHAT_OAUTH_TOKEN_FILE", "/root/chat-user-token.json"),
	)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}

	space := envOr("PI_CHAT_LIVE_SPACE", "spaces/qi7VbgAAAAE")
	h := NewHistory(cred, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	window, err := h.Context(ctx, space, "")
	if err != nil {
		t.Fatalf("space window: %v", err)
	}
	t.Logf("space %s: %d messages in %s", space, len(window.Messages), time.Since(start).Round(time.Millisecond))
	if len(window.Messages) == 0 {
		t.Fatal("window empty - either the space has no recent traffic or the descending read stopped working")
	}
	for i := 1; i < len(window.Messages); i++ {
		if window.Messages[i].CreateTime.Before(window.Messages[i-1].CreateTime) {
			t.Fatalf("window is not oldest first at %d: %s before %s", i,
				window.Messages[i].CreateTime, window.Messages[i-1].CreateTime)
		}
	}

	// The window is the identity source as well as the context source: Chat publishes the email next to
	// the user id here, which the room event itself does not carry.
	resolved := 0
	for _, m := range window.Messages {
		if m.Email != "" {
			resolved++
		}
	}
	if resolved == 0 {
		t.Fatalf("no sender in the window carried an email; identity cannot come from history: %+v", window.Messages[0])
	}
	t.Logf("%d/%d senders in the window carried an email", resolved, len(window.Messages))

	// Take the newest thread that pi-chat has spoken in, which is the situation the ownership check
	// replaced a 403-ing app-credential read for.
	thread := ""
	ownedWant := false
	for i := len(window.Messages) - 1; i >= 0; i-- {
		if window.Messages[i].Thread != "" {
			thread = window.Messages[i].Thread
			ownedWant = window.Messages[i].Bot
			break
		}
	}
	if thread == "" {
		t.Log("no threaded message in the window; skipped the thread half of the probe")
		return
	}

	start = time.Now()
	inThread, err := h.Context(ctx, space, thread)
	if err != nil {
		t.Fatalf("thread window: %v", err)
	}
	t.Logf("thread %s: %d messages, root=%v in %s", thread, len(inThread.Messages), inThread.Root != nil,
		time.Since(start).Round(time.Millisecond))
	if len(inThread.Messages) == 0 {
		t.Fatalf("narrowing to a thread we just saw in the window returned nothing: %s", thread)
	}
	for _, m := range inThread.Messages {
		if m.Thread != thread {
			t.Fatalf("thread narrowing leaked %s", m.Thread)
		}
	}
	if inThread.Root == nil {
		t.Fatalf("no root for %s - the <leaf>.<leaf> naming rule has stopped holding", thread)
	}
	if !inThread.Root.CreateTime.Before(inThread.Messages[len(inThread.Messages)-1].CreateTime) {
		t.Logf("root is not older than the newest message (fine if the thread is one message deep): root=%s", inThread.Root.CreateTime)
	}
	t.Logf("root opened %s by %s: %s", inThread.Root.CreateTime, inThread.Root.DisplayName, truncate(inThread.Root.Text, 80))

	owned, err := h.HasBotMessage(ctx, space, thread)
	if err != nil {
		t.Fatalf("ownership: %v", err)
	}
	t.Logf("pi-chat speaks in %s: %v (newest message in it was authored by pi-chat: %v)", thread, owned, ownedWant)
	t.Logf("transcript:\n%s", inThread.Transcript(20, 2000))
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
