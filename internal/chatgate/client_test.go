package chatgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
