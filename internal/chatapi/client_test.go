package chatapi

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func TestReplyOption(t *testing.T) {
	if got := replyOption(""); got != "" {
		t.Fatalf("top-level post reply option=%q, want omitted", got)
	}
	if got := replyOption("spaces/test/threads/thread-1"); got != "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" {
		t.Fatalf("threaded post reply option=%q", got)
	}
}

// liveImageRef is the `attachmentDataRef.resourceName` copied out of the bridge log for a real image
// message (spaces/m4d90aAAAAE/messages/tBiu9mGNP0M, 2026-11-27T03:12:56Z). It is the shape that
// produced "net/url: invalid control character in URL" and cost Ed his picture.
const liveImageRef = "ClxzcGFjZXMvbTRkOTBhQUFBQUUvbWVzc2FnZXMvdEJpdTltR05QME0udEJpdTltR05QME0vYXR0YWNobWVudHMvQUFUVWYtSmtBYlZ3ampOOTlKNU96Q2o3QnhnUQ=="

const liveImageName = "spaces/m4d90aAAAAE/messages/tBiu9mGNP0M.tBiu9mGNP0M/attachments/AATUf-JkAbVwjjN99J5OzCj7BxgQ"

func TestResolveResourceUnframesTheLiveReference(t *testing.T) {
	got, err := ResolveResource(liveImageRef)
	if err != nil {
		t.Fatalf("live reference rejected: %v", err)
	}
	if got != liveImageName {
		t.Fatalf("resolved=%q, want the bare resource name %q", got, liveImageName)
	}
	// The whole point: this is what DownloadMedia hands to http.NewRequestWithContext.
	if _, err := url.Parse(mediaPath + got + "?alt=media"); err != nil {
		t.Fatalf("resolved name does not build a URL: %v", err)
	}
}

func TestResolveResourceAcceptsOtherShapes(t *testing.T) {
	if got, err := ResolveResource(liveImageName); err != nil || got != liveImageName {
		t.Fatalf("plain name: got=%q err=%v", got, err)
	}
	bare := base64.StdEncoding.EncodeToString([]byte(liveImageName))
	if got, err := ResolveResource(bare); err != nil || got != liveImageName {
		t.Fatalf("unframed base64: got=%q err=%v", got, err)
	}
	// A control character in FRONT of the name is recovered by slicing to the name, because the only
	// thing that reaches the URL is the printable tail.
	leading := base64.StdEncoding.EncodeToString([]byte("\n" + liveImageName))
	if got, err := ResolveResource(leading); err != nil || got != liveImageName {
		t.Fatalf("leading-newline reference: got=%q err=%v", got, err)
	}
}

// A reference that still carries a control character inside the name must be refused, so the failure
// names the reference instead of a URL parse error landing in the prompt.
func TestResolveResourceRefusesControlCharacters(t *testing.T) {
	for _, ref := range []string{
		base64.StdEncoding.EncodeToString([]byte("spaces/m4d90aAAAAE/messages/\x01x/attachments/y")),
		base64.StdEncoding.EncodeToString([]byte("spaces/m4d90aAAAAE/messages/y/attachments/z\n")),
		"not-a-resource-name-at-all",
	} {
		got, err := ResolveResource(ref)
		if err == nil {
			t.Fatalf("reference %q accepted as %q; a control character must not reach a URL", ref, got)
		}
		if strings.IndexFunc(got, func(r rune) bool { return r < 0x20 }) >= 0 {
			t.Fatalf("reference %q returned a name with a control character: %q", ref, got)
		}
	}
}

func TestResolveResourceRefusesEmpty(t *testing.T) {
	if _, err := ResolveResource(""); err == nil {
		t.Fatal("empty reference accepted")
	}
}

// The two URL shapes are tried in order, plain name first, and a reference that cannot be resolved
// still gets whatever candidate is provable.
func TestMediaTargetsTriesBothShapes(t *testing.T) {
	got, problem := mediaTargets(liveImageRef)
	if problem != nil {
		t.Fatalf("live reference: %v", problem)
	}
	if len(got) != 2 || got[0] != liveImageName || got[1] != liveImageRef {
		t.Fatalf("targets=%q, want the resolved name then the base64 reference", got)
	}
	if got, _ := mediaTargets(liveImageName); len(got) != 1 || got[0] != liveImageName {
		t.Fatalf("plain name targets=%q, want just the name", got)
	}
	if got, problem := mediaTargets(""); len(got) != 0 || problem == nil {
		t.Fatalf("empty reference: targets=%q problem=%v, want no candidates and an error", got, problem)
	}
	// Junk that is not base64 and not a name must not become a URL path.
	if got, problem := mediaTargets("not-a-resource-name-at-all"); len(got) != 0 || problem == nil {
		t.Fatalf("junk reference: targets=%q problem=%v", got, problem)
	}
}
