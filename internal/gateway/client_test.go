package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests pin the wire contract between the bridge and gateway/index.ts. They do not exercise
// the extension itself (that is TypeScript); they make sure the Go client speaks the same JSON, so
// a rename on either side fails here instead of in a chat thread at 11pm.
func stubGateway(t *testing.T, wantAuth string, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			if got := r.Header.Get("authorization"); got != "Bearer "+wantAuth {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, wantAuth)
}

func TestClient_PromptAndTurn(t *testing.T) {
	c := stubGateway(t, "tok", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/prompt":
			var got PromptRequest
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got.ID != "abc" || got.Text == "" {
				t.Errorf("prompt body = %+v", got)
			}
			if len(got.Images) != 1 || got.Images[0].MimeType != "image/png" {
				t.Errorf("images = %+v", got.Images)
			}
			if got.DeliverAs != "steer" {
				t.Errorf("deliverAs = %q, want steer", got.DeliverAs)
			}
			w.WriteHeader(http.StatusAccepted)
			// queued/compacting are why an answer can be slow without anything being wrong. The bridge logs
			// them so "two minutes" is diagnosable as a serialised queue or a context rewrite, not a mystery.
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "turnId": "abc", "folded": true, "queued": true, "compacting": true})
		case r.URL.Path == "/v1/turns/abc":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "abc", "status": "settled", "text": "the answer", "turns": 4,
				"model": "rooter/beamster", "settledAt": 123,
				"inputs": []map[string]any{{"id": "abc", "user": "Ed <e@x>", "text": "hi"}},
				"tools":  []map[string]any{{"name": "bash", "phase": "end", "isError": false}},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	resp, err := c.Prompt(context.Background(), PromptRequest{
		ID:        "abc",
		Text:      "hi",
		DeliverAs: "steer",
		Images:    []Image{{Data: "AA==", MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.TurnID != "abc" || !resp.Folded {
		t.Errorf("prompt resp = %+v", resp)
	}
	if !resp.Queued || !resp.Compacting {
		t.Errorf("the gateway's hold reason did not survive decoding: %+v", resp)
	}

	turn, err := c.Turn(context.Background(), "abc")
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if !turn.Settled() || turn.Text != "the answer" || turn.Turns != 4 || turn.Model != "rooter/beamster" {
		t.Errorf("turn = %+v", turn)
	}
	if len(turn.Inputs) != 1 || turn.Inputs[0].User != "Ed <e@x>" {
		t.Errorf("inputs = %+v", turn.Inputs)
	}
	if len(turn.Tools) != 1 || turn.Tools[0].Name != "bash" {
		t.Errorf("tools = %+v", turn.Tools)
	}
}

// A 404 on a turn is a distinct outcome the bridge messages differently from a timeout: the Pi
// session restarted and lost the run. So it has to come back as ErrUnknownTurn, not a generic error.
func TestClient_UnknownTurnMapsToSentinel(t *testing.T) {
	c := stubGateway(t, "tok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unknown turn", "id": "gone"})
	})
	_, err := c.Turn(context.Background(), "gone")
	if !errors.Is(err, ErrUnknownTurn) {
		t.Fatalf("err = %v, want ErrUnknownTurn", err)
	}
}

func TestClient_StateAndCommands(t *testing.T) {
	c := stubGateway(t, "tok", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/state":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"idle": false, "hasPendingMessages": true, "sessionName": "pi-chat",
				"model": "rooter/beamster", "thinkingLevel": "high", "commands": 21, "compacting": true,
				"contextUsage": map[string]any{"tokens": 50000, "contextWindow": 131072, "percent": 38.1},
			})
		case "/v1/commands":
			_ = json.NewEncoder(w).Encode(map[string]any{"commands": []map[string]string{{"name": "chat-gateway", "description": "status"}}})
		case "/v1/abort":
			if r.Method != http.MethodPost {
				t.Errorf("abort should be POST, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "aborted": true})
		case "/v1/command":
			var got struct {
				Name string `json:"name"`
				Args string `json:"args"`
			}
			_ = json.NewDecoder(r.Body).Decode(&got)
			if got.Name != "model" || got.Args != "rooter/x" {
				t.Errorf("command = %+v", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	state, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.Idle || !state.HasPending || state.SessionName != "pi-chat" || state.Commands != 21 {
		t.Errorf("state = %+v", state)
	}
	if state.ContextUsage.Tokens != 50000 {
		t.Errorf("contextUsage = %+v", state.ContextUsage)
	}
	// The watch consults this before it tells a human their message is "queued behind other work", which
	// during a compaction is simply the wrong sentence.
	if !state.Compacting {
		t.Errorf("compacting did not survive decoding: %+v", state)
	}
	cmds, err := c.Commands(context.Background())
	if err != nil || len(cmds) != 1 || cmds[0].Name != "chat-gateway" {
		t.Errorf("commands = %+v err=%v", cmds, err)
	}
	if err := c.Abort(context.Background()); err != nil {
		t.Errorf("abort: %v", err)
	}
	if err := c.Command(context.Background(), "model", "rooter/x"); err != nil {
		t.Errorf("command: %v", err)
	}
}

func TestClient_BadToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer right" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "wrong")
	if _, err := c.State(context.Background()); err == nil {
		t.Fatalf("expected an auth error")
	} else if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v", err)
	}
}

func TestClient_ErrorBodyIsSurfaced(t *testing.T) {
	c := stubGateway(t, "tok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"text or images required"}`))
	})
	_, err := c.Prompt(context.Background(), PromptRequest{ID: "x"})
	if err == nil || !strings.Contains(err.Error(), "text or images required") {
		t.Errorf("err = %v, want the gateway's own message so it can be shown in chat", err)
	}
}

func TestNew_NormalizesBareHost(t *testing.T) {
	c := New("pi-gateway.internal:8090", "t")
	if c.base != "http://pi-gateway.internal:8090" {
		t.Errorf("base = %q", c.base)
	}
}
