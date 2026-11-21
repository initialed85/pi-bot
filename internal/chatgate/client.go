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
	"sync"
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

// VendoredPromptSource identifies the evaluated prompt revision copied into this binary.
// Prompt text is owned by pi-bot; re-vendor after changing it there and re-run its eval.
const VendoredPromptSource = "initialed85/pi-bot@c34f21af50966d310b3427531f093f5f7afa4c81"

// PromptText returns the exact source template shipped in the bridge.
func PromptText(flow string) (string, error) {
	data, err := promptFiles.ReadFile("prompts/" + flow + ".md")
	if err != nil {
		return "", fmt.Errorf("chatgate: unknown flow %q: %w", flow, err)
	}
	return string(data), nil
}

// Classifier is deliberately small so bridge tests can make every model decision deterministic.
type Classifier interface {
	Classify(ctx context.Context, flow string, summary, sender, message string) (answer string, prompt string, latency time.Duration, err error)
}

type Client struct {
	endpoint    string
	model       string
	http        *http.Client
	timeout     time.Duration
	warmBackoff []time.Duration
	warmMu      sync.RWMutex
	warm        bool
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
	return &Client{endpoint: endpoint, model: model, http: &http.Client{}, timeout: timeout, warmBackoff: []time.Duration{5 * time.Second, 10 * time.Second}}
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
// latency. It retries warm-up only: user-facing Classify calls never retry and retain their 2s
// deadline. The backoff plus three 2s attempts fits comfortably inside the 30s warm-up budget.
func (c *Client) Warm(ctx context.Context) error {
	if c.IsWarm() {
		return nil
	}
	warmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var last error
	attempts := len(c.warmBackoff) + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(c.warmBackoff[attempt-1])
			select {
			case <-warmCtx.Done():
				timer.Stop()
				return warmCtx.Err()
			case <-timer.C:
			}
		}
		_, _, _, last = c.Classify(warmCtx, FlowShouldRespond, "", "system", "ping")
		if last == nil {
			c.warmMu.Lock()
			c.warm = true
			c.warmMu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("chatgate: warm-up failed after %d attempts: %w", attempts, last)
}

// IsWarm reports whether a warm-up attempt has actually succeeded.
func (c *Client) IsWarm() bool {
	c.warmMu.RLock()
	defer c.warmMu.RUnlock()
	return c.warm
}

func prompt(flow, summary, sender, message string) (string, error) {
	name := flow
	if name == "" {
		return "", fmt.Errorf("chatgate: empty flow")
	}
	template, err := PromptText(name)
	if err != nil {
		return "", err
	}
	return strings.NewReplacer(
		"{summary}", summary,
		"{sender}", first(sender, "a person"),
		"{message}", message,
	).Replace(template), nil
}

func first(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
