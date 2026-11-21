// Package chatgate calls the small OpenAI-compatible model used for optional Chat reply gating.
package chatgate

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultEndpoint   = "https://sentiment-analyzer.dev.initialed85.cc"
	DefaultModel      = "unsloth/gemma-4-E4B-it-GGUF:IQ4_XS"
	FlowShouldRespond = "should-respond"
	FlowThreadRequest = "thread-request"
	FlowSummary       = "summary"
)

//go:embed prompts/*.md
var promptFiles embed.FS

// Classifier is deliberately small so bridge tests can make every model decision deterministic.
type Classifier interface {
	Classify(ctx context.Context, flow string, summary, sender, message string) (answer string, prompt string, latency time.Duration, err error)
}

type Client struct {
	endpoint string
	model    string
	http     *http.Client
	timeout  time.Duration
}

func New(endpoint, model string, timeout time.Duration) *Client {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = DefaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.TrimSpace(model) == "" {
		model = DefaultModel
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Client{endpoint: endpoint, model: model, http: &http.Client{}, timeout: timeout}
}

func (c *Client) Classify(ctx context.Context, flow, summary, sender, message string) (string, string, time.Duration, error) {
	prompt, err := prompt(flow, summary, sender, message)
	if err != nil {
		return "", "", 0, err
	}
	limit := 4
	if flow == FlowSummary {
		limit = 40
	}
	started := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"model":       c.model,
		"temperature": 0,
		"max_tokens":  limit,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", prompt, time.Since(started), err
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", prompt, time.Since(started), err
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return "", prompt, time.Since(started), err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", prompt, time.Since(started), fmt.Errorf("chatgate: HTTP %s", res.Status)
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return "", prompt, time.Since(started), fmt.Errorf("chatgate: decode response: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", prompt, time.Since(started), fmt.Errorf("chatgate: response contained no choices")
	}
	answer := strings.TrimSpace(response.Choices[0].Message.Content)
	if answer == "" {
		return "", prompt, time.Since(started), fmt.Errorf("chatgate: response contained no content")
	}
	return answer, prompt, time.Since(started), nil
}

// Warm asks the model for a cheap classification so the first real message does not pay cold-start
// latency. It has a deliberately longer deadline than normal gating.
func (c *Client) Warm(ctx context.Context) error {
	warmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _, _, err := c.Classify(warmCtx, FlowShouldRespond, "", "system", "ping")
	return err
}

func prompt(flow, summary, sender, message string) (string, error) {
	name := flow
	if name == "" {
		return "", fmt.Errorf("chatgate: empty flow")
	}
	data, err := promptFiles.ReadFile("prompts/" + name + ".md")
	if err != nil {
		return "", fmt.Errorf("chatgate: unknown flow %q: %w", flow, err)
	}
	return strings.NewReplacer(
		"{summary}", summary,
		"{sender}", first(sender, "a person"),
		"{message}", message,
	).Replace(string(data)), nil
}

func first(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
