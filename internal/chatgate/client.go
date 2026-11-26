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
	// FlowRoomRespond is the harder question: should pi-chat speak up at all over a new top-level room
	// message it was never added to. It is separate from FlowShouldRespond, which decides inside a
	// thread pi-chat is already part of.
	FlowRoomRespond   = "room-respond"
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

// Request is one gate decision. It is a struct rather than five strings because the decision now has a
// conversation in it, and a call site that reads `Classify(ctx, flow, summary, sender, message, "")`
// cannot tell you which of those the empty string was.
type Request struct {
	// Flow selects the prompt template: FlowShouldRespond, FlowRoomRespond, FlowThreadRequest, FlowSummary.
	Flow string
	// Summary is the running thread summary, or for a room message what pi-chat last said there.
	Summary string
	// Sender is a display name, never an email address: the classifier does not need to know who is
	// writing in order to tell whether they are writing to it.
	Sender string
	// Message is the text being decided on, exactly as it arrived.
	Message string
	// Transcript is the conversation the message sits in, oldest first, as Google Chat published it.
	// Empty means the bridge could not read it, which is a different state from a room with nothing in it.
	Transcript string
}

// Classifier is deliberately small so bridge tests can make every model decision deterministic.
type Classifier interface {
	Classify(ctx context.Context, req Request) (answer string, prompt string, latency time.Duration, err error)
}

type Client struct {
	endpoint    string
	model       string
	http        *http.Client
	timeout     time.Duration
	schedule    []time.Duration
	retryGap    time.Duration
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
		timeout = 3 * time.Second
	}
	return &Client{
		endpoint:    endpoint,
		model:       model,
		http:        &http.Client{},
		timeout:     timeout,
		schedule:    defaultSchedule(timeout),
		retryGap:    300 * time.Millisecond,
		warmBackoff: []time.Duration{5 * time.Second, 10 * time.Second},
	}
}

// defaultSchedule doubles each attempt: 3s then 6s then 12s. Measured latency is bimodal, not a
// distribution with a tail: about 90% of calls finish under 250ms and the stragglers are a cold
// model taking 3-20s (eval/chat-gate/latency.mjs). A single generous deadline would make every
// message wait for the worst case, while a single tight one throws away the cold calls entirely.
// Later attempts being more patient is what lets the first attempt wake the model up and the
// second one answer with it.
func defaultSchedule(timeout time.Duration) []time.Duration {
	return []time.Duration{timeout, 2 * timeout, 4 * timeout}
}

// WithSchedule overrides the attempt deadlines. Empty or non-positive input leaves the default.
func (c *Client) WithSchedule(deadlines ...time.Duration) *Client {
	picked := make([]time.Duration, 0, len(deadlines))
	for _, d := range deadlines {
		if d > 0 {
			picked = append(picked, d)
		}
	}
	if len(picked) > 0 {
		c.schedule = picked
	}
	return c
}

// Classify asks the model, retrying within the attempt schedule. It reports the total time spent
// across every attempt, because that is the number the bridge actually waits for.
func (c *Client) Classify(ctx context.Context, req Request) (string, string, time.Duration, error) {
	prompt, err := prompt(req)
	if err != nil {
		return "", "", 0, err
	}
	limit := 4
	if req.Flow == FlowSummary {
		limit = 40
	}
	body, err := json.Marshal(map[string]any{
		"model":       c.model,
		"temperature": 0,
		"max_tokens":  limit,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
	})
	if err != nil {
		return "", prompt, 0, err
	}
	schedule := c.schedule
	if len(schedule) == 0 {
		schedule = defaultSchedule(c.timeout)
	}
	started := time.Now()
	var last error
	for attempt, deadline := range schedule {
		if attempt > 0 {
			gap := time.NewTimer(c.retryGap)
			select {
			case <-ctx.Done():
				gap.Stop()
				return "", prompt, time.Since(started), ctx.Err()
			case <-gap.C:
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, deadline)
		answer, err := c.attempt(attemptCtx, body)
		cancel()
		if err == nil {
			return answer, prompt, time.Since(started), nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = fmt.Errorf("chatgate: no attempts configured")
	}
	return "", prompt, time.Since(started), last
}

// attempt is a single HTTP round trip against the classifier.
func (c *Client) attempt(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("chatgate: HTTP %s", res.Status)
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("chatgate: decode response: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("chatgate: response contained no choices")
	}
	answer := strings.TrimSpace(response.Choices[0].Message.Content)
	if answer == "" {
		return "", fmt.Errorf("chatgate: response contained no content")
	}
	return answer, nil
}

// Warm asks the model for a cheap classification so the first real message does not pay cold-start
// latency. Warm-up is where the long patience belongs: it may spend its whole 30s budget on one
// cold model, whereas a user-facing call must not hold a message for that long.
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
		_, _, _, last = c.Classify(warmCtx, Request{Flow: FlowShouldRespond, Sender: "system", Message: "ping"})
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

// KeepWarm runs the same patient warm-up as Warm but never skips it. IsWarm is only a claim about what
// this client once saw: an inference server that has been idle for an hour has usually dropped the model
// long after this client stopped asking. Measured on the shipped endpoint, the first call after ~70
// minutes idle cost 17982ms, after ~11 minutes idle it cost 203-394ms.
func (c *Client) KeepWarm(ctx context.Context) error {
	c.warmMu.Lock()
	c.warm = false
	c.warmMu.Unlock()
	return c.Warm(ctx)
}

func prompt(req Request) (string, error) {
	name := req.Flow
	if name == "" {
		return "", fmt.Errorf("chatgate: empty flow")
	}
	template, err := PromptText(name)
	if err != nil {
		return "", err
	}
	composed := strings.NewReplacer(
		"{summary}", req.Summary,
		"{sender}", first(req.Sender, "a person"),
		"{message}", req.Message,
	).Replace(template)
	if strings.TrimSpace(req.Transcript) == "" {
		return composed, nil
	}
	// The conversation goes before the template, not after it. The templates end with
	// "Answer with exactly one word: YES or NO.", and that last line is what a small model is most
	// reliably guided by; burying it under a transcript is how a one-word classifier starts writing
	// paragraphs. The template text itself is vendored from the eval and stays byte-identical, so
	// adding context here does not silently invalidate the measured prompt.
	return "Conversation in this room so far, oldest first, in [time] who: what form. " +
		"It is context for judging the message below, not itself a message to answer.\n" +
		strings.TrimRight(req.Transcript, "\n") + "\n\n" + composed, nil
}

func first(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
