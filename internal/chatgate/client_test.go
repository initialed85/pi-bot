package chatgate

import (
	"context"
	"encoding/json"
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
	answer, prompt, _, err := client.Classify(context.Background(), FlowShouldRespond, "a topic", "Ed", "what's up")
	if err != nil || answer != "YES" || prompt == "" {
		t.Fatalf("answer=%q prompt=%q err=%v", answer, prompt, err)
	}
}

func TestClientClassifyRejectsUnknownFlowAndHTTPError(t *testing.T) {
	client := New("http://127.0.0.1:1", "test-model", 20*time.Millisecond)
	if _, _, _, err := client.Classify(context.Background(), "unknown", "", "", ""); err == nil {
		t.Fatal("unknown flow succeeded")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer server.Close()
	client = New(server.URL, "test-model", time.Second)
	if _, _, _, err := client.Classify(context.Background(), FlowSummary, "", "", "message"); err == nil {
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

func TestClassifyKeepsTwoSecondDefaultAndDoesNotRetry(t *testing.T) {
	client := New("http://127.0.0.1:1", "test-model", 0)
	if client.timeout != 2*time.Second {
		t.Fatalf("default timeout=%s, want 2s", client.timeout)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()
	client = New(server.URL, "test-model", 30*time.Millisecond)
	started := time.Now()
	if _, _, _, err := client.Classify(context.Background(), FlowShouldRespond, "", "", "message"); err == nil {
		t.Fatal("classification unexpectedly succeeded")
	}
	if calls.Load() != 1 {
		t.Fatalf("classification calls=%d, want one (no user-call retry)", calls.Load())
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("classification took %s, timeout was not enforced", elapsed)
	}
}
