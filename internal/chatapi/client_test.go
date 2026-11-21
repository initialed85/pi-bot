package chatapi

import "testing"

func TestReplyOption(t *testing.T) {
	if got := replyOption(""); got != "" {
		t.Fatalf("top-level post reply option=%q, want omitted", got)
	}
	if got := replyOption("spaces/test/threads/thread-1"); got != "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" {
		t.Fatalf("threaded post reply option=%q", got)
	}
}
