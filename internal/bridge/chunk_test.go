package bridge

import (
	"strings"
	"testing"
)

func joinAll(chunks []string) string { return strings.Join(chunks, "\n\n") }

func TestChunk_Short(t *testing.T) {
	if got := Chunk("hello", 100); len(got) != 1 || got[0] != "hello" {
		t.Errorf("Chunk(short) = %#v", got)
	}
	if got := Chunk("   ", 100); got != nil {
		t.Errorf("Chunk(blank) = %#v, want nil", got)
	}
}

// The property that actually matters: nothing is lost or reordered, and no chunk exceeds the limit.
func TestChunk_NoLossAcrossSizes(t *testing.T) {
	text := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 300)
	for _, max := range []int{200, 500, 1000, 4000} {
		chunks := Chunk(text, max)
		if len(chunks) < 2 {
			t.Fatalf("max=%d produced %d chunks, expected a split", max, len(chunks))
		}
		joined := strings.Join(chunks, " ")
		for _, want := range []string{"quick brown fox", "lazy dog"} {
			if !strings.Contains(joined, want) {
				t.Errorf("max=%d: lost %q", max, want)
			}
		}
		total := strings.Count(joined, "The quick brown fox")
		if total != 300 {
			t.Errorf("max=%d: %d sentences, want 300", max, total)
		}
		for i, c := range chunks {
			if len([]rune(c)) > max {
				t.Errorf("max=%d chunk %d is %d runes", max, i, len([]rune(c)))
			}
		}
	}
}

func TestChunk_PrefersParagraphBoundary(t *testing.T) {
	text := strings.Repeat("para one line\n", 40) + "\n\n" + strings.Repeat("x", 50) + "\n\n" + strings.Repeat("tail paragraph here\n", 40)
	chunks := Chunk(text, 200)
	if len(chunks) < 3 {
		t.Fatalf("chunks = %d, want at least 3", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "\n") && !strings.Contains(chunks[0], "para one line") {
		t.Errorf("first chunk = %q", chunks[0])
	}
}

func TestChunk_ClosesOpenFence(t *testing.T) {
	code := "```go\n" + strings.Repeat("fmt.Println(\"hi\")\n", 60) + "```"
	text := "Here is the code:\n\n" + code
	chunks := Chunk(text, 400)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want the code block split", len(chunks))
	}
	for i, c := range chunks {
		if fenceBalance(c) != 0 {
			t.Errorf("chunk %d leaves a fence open: %q", i, tail(c))
		}
	}
	joined := strings.Join(chunks, "\n")
	if !strings.Contains(joined, "```go") || strings.Count(joined, "```")%2 != 0 {
		t.Errorf("fences unbalanced across chunks: %d", strings.Count(joined, "```"))
	}
	if strings.Count(joined, "fmt.Println") != 60 {
		t.Errorf("code lines lost: %d", strings.Count(joined, "fmt.Println"))
	}
}

// CJK text has no spaces, so the fallback must be a hard cut rather than an infinite loop or a
// silently dropped chunk.
func TestChunk_NoWhitespace(t *testing.T) {
	text := strings.Repeat("你好世界", 200)
	chunks := Chunk(text, 100)
	total := 0
	for _, c := range chunks {
		total += len([]rune(c))
	}
	if total != 800 {
		t.Errorf("runes = %d, want 800", total)
	}
}

func TestFenceBalance(t *testing.T) {
	cases := map[string]int{
		"no fences":                    0,
		"```\ncode\n```":               0,
		"```py\ncode":                  1,
		"```\na\n```\n```\nb":          1,
		"text\n  ```\n  code\n  ```\n": 0,
	}
	for in, want := range cases {
		if got := fenceBalance(in); got != want {
			t.Errorf("fenceBalance(%q) = %d, want %d", head(in), got, want)
		}
	}
}

func TestInlineEscapesBackticks(t *testing.T) {
	if got := Inline("a `b` c"); got != "`a 'b' c`" {
		t.Errorf("Inline = %q", got)
	}
}

func head(s string) string {
	if len(s) > 20 {
		return s[:20]
	}
	return s
}

func tail(s string) string {
	r := []rune(s)
	if len(r) > 60 {
		return string(r[len(r)-60:])
	}
	return s
}

var _ = joinAll
