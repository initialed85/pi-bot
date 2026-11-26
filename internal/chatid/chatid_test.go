package chatid

import (
	"strings"
	"testing"
)

func TestMarkPutsTheTokenAtTheStart(t *testing.T) {
	got := Mark("CD8996C5deadbeef", "⚠️ The bridge is restarting while that was running.")
	if !strings.HasPrefix(got, "⟦pi-chat cd8996c5⟧ ") {
		t.Fatalf("marker must lead the message, lowercased and shortened, got %q", got)
	}
	if !strings.HasSuffix(got, "running.") {
		t.Errorf("the notice text must survive untouched: %q", got)
	}
}

func TestMarkWithoutATurnIDIsTheTextAlone(t *testing.T) {
	if got := Mark("", "⚠️ bad"); got != "⚠️ bad" {
		t.Fatalf("an untaggable notice must not become a bare token: %q", got)
	}
	if got := Token("   "); got != "" {
		t.Fatalf("Token(%q) = %q, want empty", "   ", got)
	}
}

// TestStripLeavesProseAlone is the reason the token is made of ⟦ and ⟧ rather than the old
// "(id …)" form: a strip rule has to be safe on text a human typed, including text that talks about ids.
func TestStripLeavesProseAlone(t *testing.T) {
	cases := map[string]string{
		"⟦pi-chat cd8996c5⟧ what happened to that one?": "what happened to that one?",
		// Only the form the bridge emits is a marker. Being strict here is what makes the rule safe: a
		// hand-typed lookalike is a person's words, and we do not get to delete those.
		"look at ⟦PI-CHAT ABCDEF01⟧ please":       "look at ⟦PI-CHAT ABCDEF01⟧ please",
		"(id 12345) is not a marker":              "(id 12345) is not a marker",
		"the pi-chat app is slow":                 "the pi-chat app is slow",
		"pi-chat cd8996c5 without brackets stays": "pi-chat cd8996c5 without brackets stays",
	}
	for in, want := range cases {
		if got := Strip(in); got != want {
			t.Errorf("Strip(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefsReadsEveryMarkerInTheOrderAppearing(t *testing.T) {
	refs := Refs("⟦pi-chat aaaabbbb⟧ and ⟦pi-chat ccccdddd⟧ - what about those?")
	if len(refs) != 2 || refs[0] != "aaaabbbb" || refs[1] != "ccccdddd" {
		t.Fatalf("refs = %v", refs)
	}
	if got := Refs("nothing marked here"); got != nil {
		t.Fatalf("ordinary prose produced refs: %v", got)
	}
}

// TestRefsDoesNotInventIds keeps the token from matching anything a person could type: a fragment that
// is not bracketed, or bracketed but not hex, is text.
func TestRefsDoesNotInventIds(t *testing.T) {
	for _, in := range []string{
		"⟦pi-chat zzzzzzzz⟧",
		"⟦pi-chat 12⟧",
		"[pi-chat cd8996c5]",
		"pi-chat cd8996c5",
	} {
		if got := Refs(in); len(got) != 0 {
			t.Errorf("Refs(%q) = %v, want none", in, got)
		}
	}
}
