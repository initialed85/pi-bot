package bridge

import "strings"

// Chunk splits a reply into pieces that fit Chat's message text limit, preferring paragraph
// boundaries, then line boundaries, then spaces. It never drops or reorders characters.
//
// The fence handling is not cosmetic: a Pi answer with a long code block would otherwise produce one
// chunk that opens ``` and a next chunk that starts with naked monospace, which Chat renders as a
// wall of confusing text. A chunk that ends inside a fence gets a closing fence added, and the next
// one is reopened - so total text length is advisory, not exact, and the limit is checked against
// what will actually be sent.
func Chunk(text string, max int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if max < 200 {
		max = 200 // a limit this small would make every code block split mid-fence
	}
	runes := []rune(text)
	if len(runes) <= max {
		return []string{text}
	}

	var out []string
	rest := runes
	for len(rest) > max {
		window := rest[:max]
		cut := bestBreak(window)
		chunk := strings.TrimRight(string(window[:cut]), " \t\n")
		rest = dropLeadingSpace(rest[cut:])

		// A chunk that ends inside a fence is rewritten as two balanced halves: the code gets a
		// closing fence and the next chunk reopens one. If the closing fence will not fit, whole
		// lines are handed back to rest until it does - moving the boundary, never deleting text.
		if fenceBalance(chunk) > 0 {
			for len([]rune(chunk))+4 > max && strings.ContainsRune(chunk, '\n') {
				r := []rune(chunk)
				i := lastIndexByte(r, '\n')
				rest = append(append([]rune{}, r[i:]...), rest...)
				chunk = strings.TrimRight(string(r[:i]), " \t\n")
			}
			if fenceBalance(chunk) > 0 && len([]rune(chunk))+4 <= max {
				chunk = chunk + "\n```"
				rest = append(append([]rune{}, []rune("```\n")...), rest...)
			}
		}
		if strings.TrimSpace(chunk) != "" {
			out = append(out, chunk)
		}
	}
	if tail := strings.TrimSpace(string(rest)); tail != "" {
		if n := len(out); n > 0 && fenceBalance(out[n-1]) > 0 {
			out[n-1] += "\n```"
			tail = "```\n" + tail
		}
		out = append(out, tail)
	}
	return out
}

// bestBreak returns a rune index <= len(window) to cut at.
func bestBreak(window []rune) int {
	if i := lastIndexSeq(window, "\n\n"); i > 0 {
		return i + 2
	}
	if i := lastIndexByte(window, '\n'); i > 0 {
		return i + 1
	}
	if i := lastIndexByte(window, ' '); i > 0 {
		return i + 1
	}
	return len(window)
}

func lastIndexByte(runes []rune, b byte) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == rune(b) {
			return i
		}
	}
	return -1
}

// lastIndexSeq finds the last occurrence of a two-rune sequence, returning its start index.
func lastIndexSeq(runes []rune, seq string) int {
	r := []rune(seq)
	for i := len(runes) - len(r); i >= 0; i-- {
		match := true
		for j := range r {
			if runes[i+j] != r[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func dropLeadingSpace(runes []rune) []rune {
	i := 0
	for i < len(runes) && (runes[i] == '\n' || runes[i] == ' ' || runes[i] == '\t' || runes[i] == '\r') {
		i++
	}
	return runes[i:]
}

// fenceBalance counts ``` fences, returning >0 when the text ends inside an open block.
func fenceBalance(text string) int {
	lines := strings.Split(text, "\n")
	open := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		if open == 0 {
			open = 1
		} else {
			open = 0
		}
	}
	return open
}

// Inline wraps a short status line so Chat shows it as code-ish monospace without markdown surprises.
func Inline(text string) string {
	return "`" + strings.ReplaceAll(text, "`", "'") + "`"
}
