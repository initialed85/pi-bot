// Package gateway is a client for the pi-chat-gateway HTTP surface that lives inside a live Pi
// session (see gateway/index.ts). It is the only way the bridge talks to Pi.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnknownTurn means the session has no record of that turn: either Pi restarted (or the session
// was reloaded) before answering, or the id never made it in. The bridge reports this distinctly
// from a timeout, because "your Pi restarted" and "your Pi is slow" want different replies.
var ErrUnknownTurn = errors.New("gateway: unknown turn")

type Image struct {
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

type PromptRequest struct {
	// ID is our turn id; the gateway keys /v1/turns/<canonicalId> on it and folds our id into whichever
	// turn ends up carrying the reply, so poll the turnId it hands back rather than assuming our own.
	ID     string  `json:"id"`
	Text   string  `json:"text"`
	Images []Image `json:"images,omitempty"`
	// DeliverAs is "followUp" to queue behind a run in progress (our prompt then gets its own turn) or
	// "steer" to fold into it (our prompt rides that turn's answer).
	DeliverAs string `json:"deliverAs,omitempty"`
	User      string `json:"user,omitempty"`
	Space     string `json:"space,omitempty"`
	ReplyTo   string `json:"replyTo,omitempty"`
}

type PromptResponse struct {
	TurnID string `json:"turnId"`
	Folded bool   `json:"folded"`
	ID     string `json:"id"`
	Error  string `json:"error"`
}

type Input struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	User    string `json:"user"`
	Space   string `json:"space"`
	ReplyTo string `json:"replyTo"`
	At      int64  `json:"at"`
}

type Turn struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // running | settled | aborted | error
	Source    string `json:"source"`
	Inputs    []Input
	Streaming string `json:"streaming"`
	Last      string `json:"last"`
	Text      string `json:"text"`
	Turns     int    `json:"turns"`
	Parts     int    `json:"parts"`
	Model     string `json:"model"`
	Error     string `json:"error"`
	Progress  string `json:"progress"`
	// AnswerRejected is set when the gateway rejected the final assistant message as leaked
	// tool-call markup. Gateways that predate that fix omit the field.
	AnswerRejected string `json:"answerRejected"`
	CreatedAt      int64  `json:"createdAt"`
	EndedAt        int64  `json:"endedAt"`
	SettledAt      int64  `json:"settledAt"`
	Tools          []struct {
		Name    string `json:"name"`
		Phase   string `json:"phase"`
		IsError bool   `json:"isError"`
		Summary string `json:"summary"`
	} `json:"tools"`
}

// HasInput reports whether this turn carries the prompt with the given id, which is how the bridge
// tells "this answer is mine" from "some other run settled and I happened to be polling it".
func (t *Turn) HasInput(id string) bool {
	for _, input := range t.Inputs {
		if input.ID == id {
			return true
		}
	}
	return false
}

func (t *Turn) Settled() bool {
	switch t.Status {
	case "settled", "aborted", "error":
		return true
	}
	return false
}

// Ended reports that Pi's low-level run is over and its final message is prose: the answer exists.
// The turn is not settled yet - auto-compaction, a retry, or queued follow-up can still be running -
// so an answer posted on this signal may need correcting. It should not be withheld: the settling
// work has been measured at minutes, during which a Chat thread sits on a glyph over a written answer.
func (t *Turn) Ended() bool {
	return t.EndedAt > 0 && !t.Settled() && t.Text != ""
}

type State struct {
	OK            bool   `json:"ok"`
	Version       string `json:"version"`
	Idle          bool   `json:"idle"`
	HasPending    bool   `json:"hasPendingMessages"`
	SessionID     string `json:"sessionId"`
	SessionName   string `json:"sessionName"`
	Cwd           string `json:"cwd"`
	Model         string `json:"model"`
	ThinkingLevel string `json:"thinkingLevel"`
	ActiveTurn    string `json:"activeTurn"`
	Turns         int    `json:"turns"`
	Commands      int    `json:"commands"`
	ContextUsage  struct {
		Tokens        int     `json:"tokens"`
		ContextWindow int     `json:"contextWindow"`
		Percent       float64 `json:"percent"`
	} `json:"contextUsage"`
	Error string `json:"error"`
}

type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}

type Client struct {
	base  string
	token string
	hc    *http.Client
}

func New(baseURL, token string) *Client {
	base := strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(base, "http") {
		base = "http://" + base
	}
	return &Client{base: base, token: token, hc: &http.Client{Timeout: 90 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("gateway: encode %s: %w", path, err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("gateway: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("gateway: read %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrUnknownTurn
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("gateway: %s %s: HTTP %d %s", method, path, resp.StatusCode, firstNonEmpty(e.Error, strings.TrimSpace(string(raw))))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("gateway: decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) Prompt(ctx context.Context, req PromptRequest) (PromptResponse, error) {
	var out PromptResponse
	err := c.do(ctx, http.MethodPost, "/v1/prompt", req, &out)
	if err != nil {
		return out, err
	}
	if out.TurnID == "" {
		out.TurnID = req.ID
	}
	return out, nil
}

// Turn fetches one turn. Returns ErrUnknownTurn when the session does not know the id.
func (c *Client) Turn(ctx context.Context, id string) (Turn, error) {
	var out Turn
	err := c.do(ctx, http.MethodGet, "/v1/turns/"+id, nil, &out)
	return out, err
}

func (c *Client) Abort(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/abort", map[string]any{}, nil)
}

func (c *Client) Command(ctx context.Context, name, args string) error {
	return c.do(ctx, http.MethodPost, "/v1/command", map[string]string{"name": name, "args": args}, nil)
}

func (c *Client) State(ctx context.Context) (State, error) {
	var out State
	err := c.do(ctx, http.MethodGet, "/v1/state", nil, &out)
	return out, err
}

func (c *Client) Commands(ctx context.Context) ([]Command, error) {
	var out struct {
		Commands []Command `json:"commands"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/commands", nil, &out)
	return out.Commands, err
}

// Health is the one unauthenticated endpoint; used for readiness.
func (c *Client) Health(ctx context.Context) error {
	var out struct {
		OK bool `json:"ok"`
	}
	return c.do(ctx, http.MethodGet, "/healthz", nil, &out)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
