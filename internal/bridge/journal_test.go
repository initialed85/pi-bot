package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initialed85/pi-bot/internal/gateway"
)

// owedConfig is the shape every journal test shares: fast polling, a short answering budget, and a
// writable journal in the test's own temp dir.
func owedConfig(path string) Config {
	return Config{
		Allowlist:    []string{"owner@example.com"},
		PollInterval: 5 * time.Millisecond,
		MaxWait:      2 * time.Second,
		DrainWait:    2 * time.Second,
		JournalFile:  path,
		Eyes:         "none",
		MaxChars:     4000,
	}
}

// TestDrain_DeliversAnAnswerThatLandsAfterTheSignal is the failure Ed hit on 2026-11-26: a rollout
// arrived mid-turn, the watcher died with the receive context, and Chat got "the bridge stopped while
// that was running" over a question whose answer was being written two seconds later.
//
// A watch now runs on the shutdown context rather than the message's context, so the answer is
// delivered inside the drain window and the notice is never posted.
func TestDrain_DeliversAnAnswerThatLandsAfterTheSignal(t *testing.T) {
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b := New(chat, gw, owedConfig(filepath.Join(t.TempDir(), "turns.json")), nil)

	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Handle(ctx, "drain-1", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()

	// The answer lands after the shutdown signal - which is the entire point of draining.
	go func() {
		time.Sleep(30 * time.Millisecond)
		gw.settle(prompt.ID, "the answer, delivered after the signal")
	}()
	cancel()

	if !b.Drain(time.Second) {
		t.Fatal("drain gave up before an answer that had already been produced was posted")
	}
	b.Wait()

	posts := chat.postedTexts()
	if !containsSubstring(posts, "delivered after the signal") {
		t.Fatalf("the late answer was never posted: %v", posts)
	}
	for _, text := range posts {
		if strings.Contains(text, "bridge is restarting") || strings.Contains(text, "stopped while") {
			t.Fatalf("a turn that was answered must not also get an abandonment notice: %q", text)
		}
	}
}

// TestDrain_KeepsAnUnfinishedTurnOwed covers the other half: when the window really does run out, the
// turn is not forgotten, it is left in the journal so the next process can finish the job.
func TestDrain_KeepsAnUnfinishedTurnOwed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.json")
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b := New(chat, gw, owedConfig(path), nil)

	if err := b.Handle(context.Background(), "owed-1", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()

	if b.Drain(10 * time.Millisecond) {
		t.Fatal("drain claimed a still-running turn had finished")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the owed turn was not written to the journal: %v", err)
	}
	if !strings.Contains(string(raw), prompt.ID) {
		t.Fatalf("journal does not name the owed turn: %s", raw)
	}
	// The journal routes an answer; it is not a transcript store. Nothing Pi produced belongs in it.
	if strings.Contains(string(raw), "answer") {
		t.Fatalf("journal holds more than routing data: %s", raw)
	}
}

// ------------------------------------------------------ test-only helpers

func (f *fakeChat) postedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.posts))
	for _, p := range f.posts {
		out = append(out, p.text)
	}
	return out
}

func (f *fakeChat) postedSpaces() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.posts))
	for _, p := range f.posts {
		out = append(out, p.opts.Space)
	}
	return out
}

// dropTurn stands in for the Pi session losing its own record, which is what happens when the gateway
// restarts and the bridge comes back to collect an answer that no longer exists anywhere.
func (g *fakeGateway) dropTurn(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.turns, id)
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// TestRecover_PostsAnAnswerThePreviousProcessOwed is the whole reason the journal exists: the answer
// exists, the conversation that asked for it is still there, and only a restart stood between them.
func TestRecover_PostsAnAnswerThePreviousProcessOwed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.json")
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}

	first := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	b1 := New(first, gw, owedConfig(path), nil)
	if err := b1.Handle(context.Background(), "owed-2", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	prompt := gw.lastPrompt()
	b1.Drain(10 * time.Millisecond) // the process dies with the turn still running

	// Pi finished it after all. The old process will never post that.
	gw.settle(prompt.ID, "the answer that survived the restart")

	second := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	b2 := New(second, gw, owedConfig(path), nil)
	b2.Recover(context.Background())
	b2.Wait()

	posts := second.postedTexts()
	if !containsSubstring(posts, "the answer that survived the restart") {
		t.Fatalf("a recovered answer was never posted: %v", posts)
	}
	// Delivered means delivered: the record must not stay owed and answer the same question twice.
	if raw, err := os.ReadFile(path); err != nil {
		t.Fatalf("journal unreadable after recovery: %v", err)
	} else if strings.Contains(string(raw), prompt.ID) {
		t.Fatalf("the delivered turn is still owed, so it would be posted again: %s", raw)
	}
	if prompt.Space != "" && !containsSubstring(second.postedSpaces(), prompt.Space) {
		t.Errorf("answer did not go back to the conversation that asked: posted %v, asked in %q",
			second.postedSpaces(), prompt.Space)
	}
}

// TestRecover_StillRunningTurnIsWatchedAgain: a restart does not abandon a live run, it re-attaches to
// it, and the answer goes to the original conversation.
func TestRecover_StillRunningTurnIsWatchedAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.json")
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b1 := New(&fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}, gw, owedConfig(path), nil)
	if err := b1.Handle(context.Background(), "owed-3", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	prompt := gw.lastPrompt()
	b1.Drain(10 * time.Millisecond)

	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	b2 := New(chat, gw, owedConfig(path), nil)
	b2.Recover(context.Background())
	// Pi settles after recovery started, the way a long job does.
	go func() {
		time.Sleep(30 * time.Millisecond)
		gw.settle(prompt.ID, "finished after the bridge came back")
	}()
	b2.Wait()

	if !containsSubstring(chat.postedTexts(), "finished after the bridge came back") {
		t.Fatalf("re-attaching to a running turn did not deliver its answer: %v", chat.postedTexts())
	}
}

// TestRecover_TurnPiDoesNotKnowAboutIsTold, not swallowed: silence reads like a bot ignoring a person.
func TestRecover_TurnPiDoesNotKnowAboutIsTold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.json")
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b1 := New(&fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}, gw, owedConfig(path), nil)
	if err := b1.Handle(context.Background(), "owed-4", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	prompt := gw.lastPrompt()
	b1.Drain(10 * time.Millisecond)

	gw.dropTurn(prompt.ID) // the Pi session itself restarted; its record is gone

	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	b2 := New(chat, gw, owedConfig(path), nil)
	b2.Recover(context.Background())
	b2.Wait()

	posts := chat.postedTexts()
	if !containsSubstring(posts, "I lost that question") {
		t.Fatalf("a lost turn was not reported to the conversation it belonged to: %v", posts)
	}
	if !strings.Contains(posts[len(posts)-1], prompt.ID[:8]) {
		t.Errorf("the notice must carry the turn id so it can be traced in the log: %q", posts[len(posts)-1])
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), prompt.ID) {
		t.Fatalf("an unanswerable turn stayed owed and would be reported again: %s", raw)
	}
}
