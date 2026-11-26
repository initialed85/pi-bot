package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initialed85/pi-bot/internal/chatgate"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// TestHandle_QuotedNoticeIsStrippedAndResolved is the behaviour Ed asked for on 2026-11-26, looking at a
// posted notice that ended in "(id cd8996c5)": a marker we print is a marker that comes back through the
// room window, and a model will echo what it can see. So the marker is a token nobody types, the bridge
// removes it before anything is judged or sent, and what it MEANS is resolved instead - a quoted id
// becomes "this is the question that notice was about", which is the only reading that helps.
func TestHandle_QuotedNoticeIsStrippedAndResolved(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowThreadRequest: "YES"}, errs: map[string]error{}}
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	cfg := owedConfig(filepath.Join(t.TempDir(), "turns.json"))
	cfg.ReplyGateMode = "enforce"
	cfg.ReplyGate = gate
	b := New(chat, gw, cfg, nil)

	// One ordinary turn, delivered, so the bridge has a real record of a real question.
	if err := b.Handle(context.Background(), "tok-1", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	first := gw.lastPrompt()
	asked := first.Text
	gw.settle(first.ID, "12 nodes, all Ready.")
	b.Wait()

	id := strings.ToLower(first.ID)
	if len(id) > 8 {
		id = id[:8]
	}
	marker := "⟦pi-chat " + id + "⟧"
	raw := withMessageName(t, withText(t, fixtureBytes(t, "dm_message.json"), marker+" what happened to that one?"),
		"spaces/test/messages/tok-2.tok-2")
	if err := b.Handle(context.Background(), "tok-2", raw); err != nil {
		t.Fatalf("second handle: %v", err)
	}
	second := gw.lastPrompt()
	b.Wait()

	if strings.Contains(second.Text, "⟦") || strings.Contains(second.Text, "pi-chat "+id) {
		t.Errorf("the marker reached Pi: %q", second.Text)
	}
	if !strings.Contains(second.Text, "what happened to that one?") {
		t.Errorf("the person's own words were removed with the marker: %q", second.Text)
	}
	if !strings.Contains(second.Text, "That turn asked:") {
		t.Errorf("a quoted id was not resolved into the question it belongs to: %q", second.Text)
	}
	// The resolved note names the original ask. `asked` is what the bridge put in the journal, which
	// carries its own scaffolding, so the check is that the note quotes from it rather than inventing.
	if !strings.Contains(asked, "\n") && !strings.Contains(second.Text, asked) {
		t.Errorf("the resolved question does not match what was actually asked: %q", second.Text)
	}

	// And the classifier judged the human's line, not our bookkeeping.
	calls := gate.callsFor(chatgate.FlowThreadRequest)
	if len(calls) < 2 {
		t.Fatalf("expected a gate call per message, got %d", len(calls))
	}
	last := calls[len(calls)-1]
	if strings.Contains(last.message, "⟦") {
		t.Errorf("the gate judged a message carrying a turn marker: %q", last.message)
	}
	if !strings.Contains(last.message, "what happened to that one?") {
		t.Errorf("the gate did not see the human's words: %q", last.message)
	}
}

// TestHandle_UnknownTurnReferenceSaysSo: a marker from before this bridge's records is still worth
// telling Pi about, as an explicit "we have no record", rather than a mystery fragment in the prompt.
func TestHandle_UnknownTurnReferenceSaysSo(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowThreadRequest: "YES"}, errs: map[string]error{}}
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	cfg := owedConfig(filepath.Join(t.TempDir(), "turns.json"))
	cfg.ReplyGateMode = "enforce"
	cfg.ReplyGate = gate
	b := New(chat, gw, cfg, nil)

	raw := withText(t, fixtureBytes(t, "dm_message.json"), "⟦pi-chat deadbeef⟧ any news?")
	if err := b.Handle(context.Background(), "tok-3", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()
	b.Wait()

	if strings.Contains(prompt.Text, "⟦") {
		t.Errorf("an unresolvable marker was forwarded verbatim: %q", prompt.Text)
	}
	if !strings.Contains(prompt.Text, "deadbeef") {
		t.Errorf("the id should still be named so a human can trace it: %q", prompt.Text)
	}
	if !strings.Contains(prompt.Text, "has no record of") {
		t.Errorf("an unknown reference was not stated as unknown: %q", prompt.Text)
	}
}

// TestNoticeCarriesTheTurnMarker pins the shape of what goes OUT, because that is what makes the strip
// rule safe: the marker leads the message, is lowercase, and is the only place the token appears.
func TestNoticeCarriesTheTurnMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.json")
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	b1 := New(&fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}, gw, owedConfig(path), nil)
	if err := b1.Handle(context.Background(), "tok-4", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("first handle: %v", err)
	}
	prompt := gw.lastPrompt()
	b1.Drain(10 * time.Millisecond)

	gw.dropTurn(prompt.ID) // the Pi session restarted; the answer is gone
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	b2 := New(chat, gw, owedConfig(path), nil)
	b2.Recover(context.Background())
	b2.Wait()

	posts := chat.postedTexts()
	if len(posts) == 0 {
		t.Fatal("a lost turn produced no notice at all")
	}
	token := "⟦pi-chat " + strings.ToLower(prompt.ID)[:8] + "⟧"
	if !strings.HasPrefix(posts[len(posts)-1], token) {
		t.Fatalf("notice %q must start with %s", posts[len(posts)-1], token)
	}
}
