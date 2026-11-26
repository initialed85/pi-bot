package chatgate

import (
	"os"
	"strings"
	"testing"
)

// The eval measures the prompt the bridge sends, not a description of it. Two of its parts are copied
// strings today - the template (guarded by TestVendoredPromptsMatchPiBotMaster) and the sentence that
// introduces a transcript - so the second one gets the same treatment here.
func TestTranscriptScaffoldingMatchesTheEvalCopy(t *testing.T) {
	canonical, err := os.ReadFile("../../eval/chat-gate/transcript-scaffolding.txt")
	if err != nil {
		t.Fatalf("the eval copy of the transcript scaffolding is missing: %v", err)
	}
	if got := strings.TrimSuffix(string(canonical), "\n"); got != TranscriptPreamble {
		t.Fatalf("bridge transcript scaffolding differs from the eval copy; re-vendor or re-run the context eval\nbridge: %q\neval:   %q", TranscriptPreamble, got)
	}
}

// Where the transcript sits in the prompt is the behaviour: a small model follows the last instruction
// most reliably, and every template ends with "Answer with exactly one word: YES or NO." Adding context
// above the template keeps that line last; adding it below would turn a one-word classifier into prose.
func TestPrompt_TranscriptGoesAboveTheTemplate(t *testing.T) {
	req := Request{
		Flow:       FlowRoomRespond,
		Summary:    "pi-chat: the left bank runs at 06:01",
		Sender:     "Ed",
		Message:    "so what's the scoop mr. bot",
		Transcript: "[03:21] Amy: I'm heading out\n[03:25] Ed: so what's the scoop mr. bot, are you working today or not?",
	}
	composed, err := prompt(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(composed, TranscriptPreamble+"\n") {
		t.Fatalf("a transcript must open with the scaffolding that marks it as context, got %q", composed[:min(len(composed), 120)])
	}
	if !strings.Contains(composed, "[03:25] Ed: so what's the scoop") {
		t.Fatal("the transcript did not reach the prompt at all")
	}
	if !strings.Contains(composed, "so what's the scoop mr. bot") {
		t.Fatal("the message itself disappeared behind the transcript")
	}
	// The message line must come after the transcript: "the message below" is only true one way round.
	if strings.Index(composed, "[03:21] Amy") > strings.Index(composed, "Message (from Ed)") {
		t.Fatal("the transcript was rendered after the message it is supposed to precede")
	}
	if tail := strings.TrimRight(composed, "\n"); !strings.HasSuffix(tail, "Answer with exactly one word: YES or NO.") {
		t.Fatalf("the one-word instruction is no longer the last line the model reads: %q", tail[max(0, len(tail)-80):])
	}
}

// Without a window the prompt must be byte-identical to the prompt measured before context existed, so
// a read that fails cannot quietly change behaviour that has already been evaluated.
func TestPrompt_NoTranscriptIsTheMeasuredPrompt(t *testing.T) {
	template, err := PromptText(FlowRoomRespond)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer("{summary}", "none", "{sender}", "Ed", "{message}", "are you still running that job?").Replace(template)
	got, err := prompt(Request{Flow: FlowRoomRespond, Summary: "none", Sender: "Ed", Message: "are you still running that job?"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("an empty transcript changed the prompt; the no-context path is no longer the evaluated one")
	}
}
