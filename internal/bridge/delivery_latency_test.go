package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initialed85/pi-bot/internal/gateway"
)

// awaitPost waits for a Chat post whose text contains want, and fails the test if none arrives. The
// point of these tests is timing, so they cannot assume a turn has already delivered.
func awaitPost(t *testing.T, chat *fakeChat, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, text := range chat.postedTexts() {
			if strings.Contains(text, want) {
				return text
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no Chat post containing %q within %v (posts: %v)", want, timeout, chat.postedTexts())
	return ""
}

// TestWatch_AnswersAtTheEndOfTheRun is the minutes-long wait Ed described on 2026-11-26: the reply was
// fully written, and then Pi spent that time compacting its context before the turn settled. The bridge
// used to treat agent_settled as the only delivery signal, so a finished answer sat in the gateway
// behind a "working" glyph for as long as that took.
//
// The run ending is now a delivery signal of its own: the text ships, and the watch keeps polling in
// case Pi continues.
func TestWatch_AnswersAtTheEndOfTheRun(t *testing.T) {
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b := New(chat, gw, owedConfig(filepath.Join(t.TempDir(), "turns.json")), nil)

	if err := b.Handle(context.Background(), "early-1", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()
	// The answer is written; the turn is still "running" because Pi is still settling (compaction).
	gw.setTurn(prompt.ID, gateway.Turn{
		ID:      prompt.ID,
		Status:  "running",
		Text:    "Tomorrow: clear, 23°C high, no rain.",
		Last:    "Tomorrow: clear, 23°C high, no rain.",
		EndedAt: time.Now().UnixMilli(),
		Inputs:  []gateway.Input{{ID: prompt.ID}},
	})

	awaitPost(t, chat, "clear, 23°C high", 2*time.Second)

	// Settling later, with the same text, must not produce a second message.
	gw.settle(prompt.ID, "Tomorrow: clear, 23°C high, no rain.")
	b.Wait()

	posts := chat.postedTexts()
	count := 0
	for _, text := range posts {
		if strings.Contains(text, "clear, 23°C high") {
			count++
		}
		if strings.Contains(text, "corrected") {
			t.Fatalf("an unchanged answer must not be corrected: %q", text)
		}
	}
	if count != 1 {
		t.Fatalf("answer posted %d times, want 1: %v", count, posts)
	}
}

// TestWatch_CorrectsAnAnswerThatChangedWhileSettling: Pi can continue after agent_end - an overflow
// retry, or queued work riding the same turn - and say something different. Delivering early means we
// told the human the first version, so the honest fix is a visible correction, not a silent edit.
func TestWatch_CorrectsAnAnswerThatChangedWhileSettling(t *testing.T) {
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b := New(chat, gw, owedConfig(filepath.Join(t.TempDir(), "turns.json")), nil)

	if err := b.Handle(context.Background(), "early-2", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()
	gw.setTurn(prompt.ID, gateway.Turn{
		ID:      prompt.ID,
		Status:  "running",
		Text:    "12 nodes.",
		Last:    "12 nodes.",
		EndedAt: time.Now().UnixMilli(),
		Inputs:  []gateway.Input{{ID: prompt.ID}},
	})
	awaitPost(t, chat, "12 nodes", 2*time.Second)

	// Pi kept going and settled on a more complete answer.
	gw.setTurn(prompt.ID, gateway.Turn{
		ID:      prompt.ID,
		Status:  "settled",
		Text:    "12 nodes: 4 control-plane, 8 agents.",
		Last:    "12 nodes: 4 control-plane, 8 agents.",
		EndedAt: time.Now().UnixMilli(),
		Inputs:  []gateway.Input{{ID: prompt.ID}},
	})
	b.Wait()

	posts := chat.postedTexts()
	corrections := 0
	for _, text := range posts {
		if strings.Contains(text, "corrected") {
			corrections++
			if !strings.Contains(text, "4 control-plane") {
				t.Fatalf("correction does not carry the settled answer: %q", text)
			}
		}
	}
	if corrections != 1 {
		t.Fatalf("got %d corrections, want exactly 1: %v", corrections, posts)
	}
}

// TestWatch_EndedWithoutTextIsNotDelivered: an early signal is only useful if there is an answer in it.
// A run that ends with no prose must not post "…done, with no text to show" before the retry that
// produces one - that placeholder is a settling-time message.
func TestWatch_EndedWithoutTextIsNotDelivered(t *testing.T) {
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b := New(chat, gw, owedConfig(filepath.Join(t.TempDir(), "turns.json")), nil)

	if err := b.Handle(context.Background(), "early-3", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()
	gw.setTurn(prompt.ID, gateway.Turn{
		ID:             prompt.ID,
		Status:         "running",
		EndedAt:        time.Now().UnixMilli(),
		AnswerRejected: "<tool_call>garbage",
		Inputs:         []gateway.Input{{ID: prompt.ID}},
	})

	time.Sleep(60 * time.Millisecond)
	if posts := chat.postedTexts(); len(posts) > 0 {
		t.Fatalf("an ended run with no prose was delivered anyway: %v", posts)
	}

	gw.settle(prompt.ID, "here is the real answer")
	b.Wait()
	if !containsSubstring(chat.postedTexts(), "real answer") {
		t.Fatalf("the settled answer never arrived: %v", chat.postedTexts())
	}
}
