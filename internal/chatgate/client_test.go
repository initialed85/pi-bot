package chatgate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientClassifyUsesEvaluatedPromptAndContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			Messages  []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "test-model" || body.MaxTokens != 4 || len(body.Messages) != 1 {
			t.Fatalf("request body=%+v", body)
		}
		if !strings.Contains(body.Messages[0].Content, `Latest message (from Ed): "what's up"`) {
			t.Fatalf("prompt=%q", body.Messages[0].Content)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" YES "}}]}`))
	}))
	defer server.Close()

	client := New(server.URL, "test-model", time.Second)
	answer, prompt, _, err := client.Classify(context.Background(), Request{Flow: FlowShouldRespond, Summary: "a topic", Sender: "Ed", Message: "what's up"})
	if err != nil || answer != "YES" || prompt == "" {
		t.Fatalf("answer=%q prompt=%q err=%v", answer, prompt, err)
	}
}

func TestClientClassifyRejectsUnknownFlowAndHTTPError(t *testing.T) {
	client := New("http://127.0.0.1:1", "test-model", 20*time.Millisecond)
	if _, _, _, err := client.Classify(context.Background(), Request{Message: "m"}); err == nil {
		t.Fatal("unknown flow succeeded")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer server.Close()
	client = New(server.URL, "test-model", time.Second)
	if _, _, _, err := client.Classify(context.Background(), Request{Flow: FlowSummary, Message: "message"}); err == nil {
		t.Fatal("HTTP error succeeded")
	}
}

func TestWarmRetriesAndMarksWarmOnlyAfterSuccess(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, "cold", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"YES"}}]}`))
	}))
	defer server.Close()

	client := New(server.URL, "test-model", 30*time.Millisecond)
	client.warmBackoff = []time.Duration{0}
	if client.IsWarm() {
		t.Fatal("client marked warm before warm-up")
	}
	if err := client.Warm(context.Background()); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if attempts != 2 || !client.IsWarm() {
		t.Fatalf("attempts=%d warm=%v, want two attempts and warm=true", attempts, client.IsWarm())
	}
}

// IsWarm is a claim about this client, not about the server: an idle endpoint drops the model long after
// the client last asked, so a keep-alive loop needs the variant that never skips.
func TestKeepWarmAsksTheServerAgainEvenWhenAlreadyWarm(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"YES"}}]}`))
	}))
	defer server.Close()

	client := New(server.URL, "test-model", 30*time.Millisecond)
	client.warmBackoff = []time.Duration{0}
	if err := client.Warm(context.Background()); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	if attempts != 1 || !client.IsWarm() {
		t.Fatalf("after warm-up attempts=%d warm=%v", attempts, client.IsWarm())
	}
	// Warm() is the idempotent one; that is what makes it useless as a heartbeat.
	if err := client.Warm(context.Background()); err != nil || attempts != 1 {
		t.Fatalf("Warm() re-asked the server: attempts=%d err=%v", attempts, err)
	}
	if err := client.KeepWarm(context.Background()); err != nil {
		t.Fatalf("keep-warm: %v", err)
	}
	if attempts != 2 || !client.IsWarm() {
		t.Fatalf("KeepWarm did not re-ask and re-mark warm: attempts=%d warm=%v", attempts, client.IsWarm())
	}
}

func TestWarmFailureDoesNotMarkWarm(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "still cold", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := New(server.URL, "test-model", 30*time.Millisecond)
	client.warmBackoff = []time.Duration{0, 0}
	if err := client.Warm(context.Background()); err == nil {
		t.Fatal("warm-up unexpectedly succeeded")
	}
	if client.IsWarm() {
		t.Fatal("failed warm-up marked client warm")
	}
}

// The schedule is the whole point of the retry design: patient only on the later attempts, so a
// cold model gets woken by the first one and still answers on the second.
func TestClassifyAttemptScheduleDoublesAndDefaultIsThreeSeconds(t *testing.T) {
	client := New("http://127.0.0.1:1", "test-model", 0)
	if client.timeout != 3*time.Second {
		t.Fatalf("default first-attempt timeout=%s, want 3s", client.timeout)
	}
	want := []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second}
	if fmt.Sprint(client.schedule) != fmt.Sprint(want) {
		t.Fatalf("schedule=%v, want %v", client.schedule, want)
	}
}

func TestClassifyRetriesAfterTransportFailureAndReportsTotalWait(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "cold model", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"YES"}}]}`))
	}))
	defer server.Close()

	client := New(server.URL, "test-model", 0).WithSchedule(200*time.Millisecond, 200*time.Millisecond)
	started := time.Now()
	answer, _, latency, err := client.Classify(context.Background(), Request{Flow: FlowShouldRespond, Sender: "a person", Message: "hello"})
	if err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if answer != "YES" || calls.Load() != 2 {
		t.Fatalf("answer=%q calls=%d, want YES after 2 calls", answer, calls.Load())
	}
	if elapsed := time.Since(started); elapsed < client.retryGap || latency < client.retryGap {
		t.Fatalf("elapsed=%s latency=%s, want at least the %s gap between attempts", elapsed, latency, client.retryGap)
	}
}

func TestClassifyStopsAfterTheScheduleIsSpent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := New(server.URL, "test-model", 30*time.Millisecond).WithSchedule(30*time.Millisecond, 30*time.Millisecond, 30*time.Millisecond)
	started := time.Now()
	if _, _, _, err := client.Classify(context.Background(), Request{Flow: FlowShouldRespond, Message: "message"}); err == nil {
		t.Fatal("classification unexpectedly succeeded")
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d, want one per scheduled attempt", calls.Load())
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("classification took %s, the attempt schedule was not enforced", elapsed)
	}
}

// The templates end with "Answer with exactly one word: YES or NO." and a small model follows the last
// instruction it sees, so a transcript must never land below that line. This pins the composition, and
// that the vendored template text itself is left byte-identical above and below the placeholders.
func TestPromptPutsTheTranscriptAboveTheInstruction(t *testing.T) {
	template, err := PromptText(FlowRoomRespond)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	transcript := "[03:25] ed: so what's the scoop mr. bot\n[04:04] amy: he is asleep\n"
	got, err := prompt(Request{Flow: FlowRoomRespond, Summary: "earlier answer", Sender: "ed",
		Message: "still no non-mentions then", Transcript: transcript})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if !strings.HasPrefix(got, "Conversation in this room so far") || !strings.Contains(strings.SplitN(got, "\n", 2)[1], "[03:25] ed:") {
		t.Fatalf("transcript not first: %q", got[:120])
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "Answer with exactly one word: YES or NO.") {
		t.Fatalf("the one-word instruction is no longer the last thing the model reads: %q", got[len(got)-80:])
	}
	if strings.Index(got, transcript) > strings.Index(got, "Message (from ed)") {
		t.Fatal("transcript lands after the message it is context for")
	}
	body := strings.SplitN(got, "\n\n", 2)[1]
	if !strings.Contains(body, "Pi-chat's most recent answer in this room, empty if it has not spoken recently: earlier answer") {
		t.Fatalf("summary placeholder misplaced: %q", body)
	}
	if !strings.Contains(body, strings.TrimSuffix(template, "\n")[:200]) && !strings.Contains(body, template[:200]) {
		t.Fatal("template text was altered")
	}
	// No transcript must reproduce today's prompt exactly, so a context read that fails cannot change
	// the behaviour that has already been measured.
	bare, err := prompt(Request{Flow: FlowRoomRespond, Summary: "earlier answer", Sender: "ed", Message: "still no non-mentions then"})
	if err != nil {
		t.Fatalf("bare prompt: %v", err)
	}
	if strings.Contains(bare, "Conversation in this room so far") {
		t.Fatal("empty transcript still composed a header")
	}
}

func TestNewNormalizesOpenAIBaseSuffix(t *testing.T) {
	const host = "http://sentiment-analyzer.sentiment-analyzer.svc.cluster.local:9999"
	for _, in := range []string{host + "/v1", host + "/v1/", host} {
		if got := New(in, "m", time.Second).endpoint; got != host {
			t.Errorf("New(%q).endpoint = %q, want %q", in, got, host)
		}
	}
	if got := New("/v1", "m", time.Second).endpoint; got != DefaultEndpoint {
		t.Errorf("degenerate base collapsed to %q, want the shipped default", got)
	}
	if got := New("http://host.example/v1/api", "m", time.Second).endpoint; got != "http://host.example/v1/api" {
		t.Errorf("a /v1 that is not the last segment must survive, got %q", got)
	}
}

func TestClassifyPathIsSingleV1(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" NO "}}]}`))
	}))
	defer server.Close()

	answer, prompt, _, err := New(server.URL+"/v1", "test-model", time.Second).Classify(context.Background(), Request{Flow: FlowShouldRespond, Summary: "a topic", Sender: "Ed", Message: "what's up"})
	if err != nil || answer != "NO" || prompt == "" {
		t.Fatalf("answer=%q prompt=%q err=%v", answer, prompt, err)
	}
	if seen != "/v1/chat/completions" {
		t.Errorf("request path = %q, want /v1/chat/completions", seen)
	}
}
