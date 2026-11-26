package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/initialed85/pi-bot/internal/chatgate"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// quietLogger keeps a test's output to real problems, which is what the journal's warnings are.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func readDecisions(t *testing.T, dir string) []gateDecision {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, decisionsFileName))
	if err != nil {
		t.Fatalf("open decisions: %v", err)
	}
	defer f.Close()
	var out []gateDecision
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var d gateDecision
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, d)
	}
	return out
}

// TestRecordDecisionSurvivesANewJournal is the whole point of the file: the pod that made the judgement
// is gone by the time anyone wants to review it, and the volume is not.
func TestRecordDecisionSurvivesANewJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "turns.json")
	j := openJournal(path, quietLogger())
	j.recordDecision(gateDecision{Flow: chatgate.FlowRoomRespond, Action: "suppress", Mode: "enforce",
		Space: "spaces/abc", Thread: "spaces/abc/threads/x", PromptID: "cd8996c5", ContextChars: 259, LatencyMS: 412})

	// A fresh journal from the same path models a restarted pod on the same volume.
	j2 := openJournal(path, quietLogger())
	j2.recordDecision(gateDecision{Flow: chatgate.FlowShouldRespond, Action: "forward", Mode: "enforce",
		PromptID: "aaaa1111", ContextChars: 63, LatencyMS: 1503})

	got := readDecisions(t, dir)
	if len(got) != 2 {
		t.Fatalf("expected 2 recorded decisions, got %d", len(got))
	}
	if got[0].Action != "suppress" || got[1].Action != "forward" || got[1].LatencyMS != 1503 {
		t.Fatalf("decisions = %+v", got)
	}
	if got[0].TS.IsZero() {
		t.Errorf("a decision without a timestamp is not evidence about when anything happened")
	}
}

// TestRecordDecisionCarriesNoMessageText: this file outlives the pod, so it must not accumulate other
// people's sentences. Verdicts and measurements only.
func TestRecordDecisionCarriesNoMessageText(t *testing.T) {
	dir := t.TempDir()
	j := openJournal(filepath.Join(dir, "turns.json"), quietLogger())
	j.recordDecision(gateDecision{Flow: chatgate.FlowRoomRespond, Action: "suppress", Mode: "dry-run",
		Space: "spaces/abc", Thread: "spaces/abc/threads/x", PromptID: "cd8996c5"})

	raw, err := os.ReadFile(filepath.Join(dir, decisionsFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, leak := range []string{"is the bot here", "password", "http", "@"} {
		if strings.Contains(strings.ToLower(string(raw)), leak) {
			t.Errorf("the record carries %q, which is not a verdict: %s", leak, raw)
		}
	}
}

func TestRecordDecisionRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, decisionsFileName)
	// Pretend a long-lived pod already filled the budget.
	big := make([]byte, maxDecisionFile+64)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	j := openJournal(filepath.Join(dir, "turns.json"), quietLogger())
	j.recordDecision(gateDecision{Flow: chatgate.FlowRoomRespond, Action: "suppress", Mode: "enforce"})

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("the previous record was discarded instead of rotated: %v", err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() > 4096 {
		t.Fatalf("current record size = %v (err %v), want a fresh small file", st, err)
	}
	if got := readDecisions(t, dir); len(got) != 1 {
		t.Fatalf("after rotation the newest decision must be in the live file, got %d", len(got))
	}
}

// TestReplyGateWritesItsEvidenceOnTheWayThrough is the wiring check. Everything above tests the file; this
// tests that the decision the bridge just made is actually in it, because a hook nobody calls is how the
// last report ended up with nothing to quote.
func TestReplyGateWritesItsEvidenceOnTheWayThrough(t *testing.T) {
	dir := t.TempDir()
	// A 1:1 is judged with the thread-request vocabulary, where the tokens are THREAD and INLINE.
	gate := &fakeGate{answers: map[string]string{chatgate.FlowThreadRequest: "THREAD"}, errs: map[string]error{}}
	chat := &fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	cfg := owedConfig(filepath.Join(dir, "turns.json"))
	cfg.ReplyGateMode = "enforce"
	cfg.ReplyGate = gate
	b := New(chat, gw, cfg, nil)

	if err := b.Handle(context.Background(), "tok-9", fixtureBytes(t, "dm_message.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()

	got := readDecisions(t, dir)
	if len(got) == 0 {
		t.Fatal("an answered message left no recorded decision")
	}
	last := got[len(got)-1]
	if last.Flow != chatgate.FlowThreadRequest || last.Action != "thread" || last.Mode != "enforce" {
		t.Fatalf("recorded decision = %+v", last)
	}
	if last.PromptID == "" {
		t.Errorf("a decision with no prompt id cannot be tied back to the message: %+v", last)
	}
}

// The guard this exists for: a bridge with no journal path has no volume, and until recordDecision said
// so, every test run wrote a gate-decisions.jsonl into the package directory and one eventually got
// committed as though it were evidence from a real pod.
func TestDecisionsDoNotLandInTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	j := openJournal("", quietLogger())
	j.recordDecision(gateDecision{Flow: chatgate.FlowShouldRespond, Action: "forward", Mode: "enforce"})
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("a journal with no path wrote %d file(s) into the working directory: %v", len(entries), entries)
	}
}
