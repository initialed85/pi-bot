// Package chatid is the one place that knows the bridge's visible turn marker.
//
// A notice in Chat has to say which turn it is talking about, because "what happened to that one?" is a
// question people actually ask, and the log line that answers it is found by turn id. But anything the
// bridge writes into a room also comes back through the conversation window on the next message, where a
// model will happily echo it - so the marker needs to be (a) trivially recognisable, (b) impossible to
// produce by typing, and (c) removable without touching a human's words.
//
// The marker is a bracketed token placed at the START of a bridge notice:
//
//	⟦pi-chat cd8996c5⟧ ⚠️ The bridge is restarting while that was running.
//
// ⟦ (U+27E6) and ⟧ (U+27E7) are mathematical bracket characters that Google Chat renders as-is and
// nobody types by accident, which is what makes "strip it, and only strip it" safe on text a person
// wrote. It replaces the earlier `(id cd8996c5)` suffix: natural-looking enough that a model repeats it,
// and ambiguous enough that "id" appearing in ordinary prose looked like a marker to the strip rule.
package chatid

import (
	"regexp"
	"strings"
)

const (
	prefix = "⟦pi-chat "
	suffix = "⟧"
	// idLen is how much of a turn id is shown. Enough to be unique in practice, short enough that the
	// marker is not the loudest thing in the message.
	idLen = 8
)

// tokenRe matches a marker anywhere in a string. Deliberately tolerant on length and case: the point is
// that a marker always parses, not that a human can craft one that survives stripping.
var tokenRe = regexp.MustCompile(`⟦pi-chat ([0-9a-fA-F]{4,40})⟧`)

// Mark returns text with the marker for turnID in front of it. An empty turn id or empty text is passed
// through, because a notice without content is a bug and should stay visible.
func Mark(turnID, text string) string {
	token := Token(turnID)
	if token == "" || text == "" {
		return text
	}
	return token + " " + text
}

// Token renders the marker for a turn id, or "" when there is nothing to mark.
func Token(turnID string) string {
	id := strings.ToLower(strings.TrimSpace(turnID))
	if id == "" {
		return ""
	}
	if len(id) > idLen {
		id = id[:idLen]
	}
	return prefix + id + suffix
}

// Strip removes every marker from text, including one a person pasted into a message. Callers that need
// the ids first should use Refs, which reads them before this runs.
func Strip(text string) string {
	if !strings.Contains(text, prefix) {
		return text
	}
	return strings.TrimSpace(tokenRe.ReplaceAllString(text, " "))
}

// Refs returns the id fragments a message points at, in the order they appear. This is what lets the
// bridge answer "that one you posted yesterday" by resolving the fragment against the turns it knows.
func Refs(text string) []string {
	if !strings.Contains(text, prefix) {
		return nil
	}
	var out []string
	for _, m := range tokenRe.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	return out
}
