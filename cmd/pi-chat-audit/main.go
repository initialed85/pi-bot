// pi-chat-audit answers one question, offline: if the room listener were live right now, what would
// the reply gate have decided about the messages it has already received?
//
// It is an evaluator, not a component. Nothing is posted to Chat, Pi is never asked anything, and no
// Workspace Events subscription is created or modified. The decision path it runs is the production
// one - bridge.HandleWorkspace, the real chatgate classifier, the same allowlist and the same space
// lookup - so a verdict here is a verdict the bridge would have made, which is the only honest way to
// review a gate before it speaks in a room with other people in it.
//
// It reads the backlog of the user-authorised Workspace Events pull subscription using the same user
// OAuth credential that authorises the subscription (chatevents.LoadCredential), because that is the
// identity whose view of the rooms is being reviewed. Pub/Sub REST pull + acknowledge; spaces.get REST
// for the space-type lookup. Messages are acked, so a review consumes the backlog it reviews - run it
// deliberately, not as a side effect of curiosity.
//
// Privacy: participant message text is printed by default, because a gate review that cannot see the
// text is not a review. This tool's output is a terminal, not a cluster log, and nothing it prints is
// committed. PI_CHAT_AUDIT_REDACT=on prints `<redacted>` for anything not sent by an allowlisted
// sender. The bridge in the cluster never prints participant text at all.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/initialed85/pi-bot/internal/bridge"
	"github.com/initialed85/pi-bot/internal/chatapi"
	"github.com/initialed85/pi-bot/internal/chatevents"
	"github.com/initialed85/pi-bot/internal/chatgate"
	"github.com/initialed85/pi-bot/internal/chatmsg"
	"github.com/initialed85/pi-bot/internal/gateway"
)

const (
	defaultClientFile = "/root/oauth-client.json"
	defaultTokenFile  = "/root/chat-user-token.json"
	chatAPI           = "https://chat.googleapis.com/v1"
	pubsubAPI         = "https://pubsub.googleapis.com/v1"
)

// ---------------------------------------------------------------- Chat, but it cannot post anything

// userChat is the read half of the Chat API over the user's own token. Post is refused by
// construction: the whole point of this tool is that it decides without speaking.
type userChat struct {
	token func(context.Context) (string, error)
	calls int
}

func (c *userChat) Post(context.Context, string, chatapi.PostOpts) (*chatapi.Message, error) {
	return nil, errors.New("audit: posting is disabled")
}

func (c *userChat) Update(context.Context, string, string) error {
	return errors.New("audit: updating is disabled")
}

func (c *userChat) Get(context.Context, string) (*chatapi.Message, error) {
	return nil, errors.New("audit: reading a message is not part of a gate review")
}

func (c *userChat) Delete(context.Context, string) error {
	return errors.New("audit: deleting is disabled")
}

func (c *userChat) DownloadMedia(context.Context, string, int64) ([]byte, string, error) {
	return nil, "", errors.New("audit: media is not part of a gate review")
}

// HasBotMessage is a room-eligibility question the bridge asks before answering. The audit answers
// "nothing posted yet here", which is true for a review that cannot post, and keeps the message on
// the path a real one would have taken.
func (c *userChat) HasBotMessage(context.Context, string, string) (bool, error) {
	return false, nil
}

// GetSpace is the lookup the listener genuinely needs: an event does not say whether its space is a
// 1:1 or a group room, and the routing rule differs. The user token answers this one; the app
// credential does not report membership at all.
func (c *userChat) GetSpace(ctx context.Context, name string) (*chatapi.SpaceInfo, error) {
	c.calls++
	var resp struct {
		Name            string `json:"name"`
		SpaceType       string `json:"spaceType"`
		SingleUserBotDm bool   `json:"singleUserBotDm"`
		MembershipCount struct {
			JoinedDirectHumanUserCount int `json:"joinedDirectHumanUserCount"`
			JoinedHumanUserCount       int `json:"joinedHumanUserCount"`
		} `json:"membershipCount"`
	}
	endpoint := chatAPI + "/" + url.PathEscape(name)
	if err := getJSON(ctx, c.token, endpoint, &resp); err != nil {
		return nil, err
	}
	return &chatapi.SpaceInfo{
		Name:            firstOf(resp.Name, name),
		SpaceType:       resp.SpaceType,
		SingleUserBotDM: resp.SingleUserBotDm,
	}, nil
}

// ---------------------------------------------------------------- Gateway, but Pi is never asked

type noGateway struct{}

func (noGateway) Prompt(context.Context, gateway.PromptRequest) (gateway.PromptResponse, error) {
	return gateway.PromptResponse{}, errors.New("audit: Pi is not asked")
}

func (noGateway) Turn(context.Context, string) (gateway.Turn, error) {
	return gateway.Turn{}, errors.New("audit: no turns")
}

func (noGateway) Abort(context.Context) error { return nil }

func (noGateway) Command(context.Context, string, string) error {
	return errors.New("audit: slash commands are not run by a gate review")
}

func (noGateway) State(context.Context) (gateway.State, error) {
	return gateway.State{Idle: true}, nil
}

func (noGateway) Commands(context.Context) ([]gateway.Command, error) { return nil, nil }

// ---------------------------------------------------------------- verdicts

type verdict struct {
	space       string
	sender      string
	text        string
	path        string
	flow        string
	answer      string
	prompt      string
	latency     time.Duration
	err         error
	wouldAnswer bool
}

type review struct {
	verdicts []verdict
	textLog  bool
}

// answeredYes mirrors bridge.exactGateToken for the flows this review exercises, so a verdict here
// means the same thing it means in the bridge: an answer counts only when it is exactly the token the
// bridge accepts.
func answeredYes(flow, answer string) bool {
	token := strings.ToUpper(strings.TrimSpace(answer))
	switch flow {
	case chatgate.FlowRoomRespond, chatgate.FlowShouldRespond:
		return token == "YES"
	case chatgate.FlowThreadRequest:
		return token == "INLINE" || token == "THREAD"
	default:
		return false
	}
}

type recordingGate struct {
	inner  *chatgate.Client
	review *review
}

func (g *recordingGate) Classify(ctx context.Context, flow, summary, sender, message string) (string, string, time.Duration, error) {
	answer, prompt, latency, err := g.inner.Classify(ctx, flow, summary, sender, message)
	g.review.record(flow, sender, message, answer, prompt, latency, err)
	return answer, prompt, latency, err
}

func (r *review) record(flow, sender, message, answer, prompt string, latency time.Duration, err error) {
	verdict := "answer"
	switch {
	case err != nil:
		verdict = "gate error -> no reply (the bridge refuses to guess)"
	case !answeredYes(flow, answer):
		verdict = "stay quiet"
	}
	line := fmt.Sprintf("  gate %s -> %q (%s) => %s", flow, answer, latency.Round(10*time.Millisecond), verdict)
	if err != nil {
		line += fmt.Sprintf(" err=%v", err)
	}
	fmt.Println(line)
	if len(r.verdicts) == 0 {
		return
	}
	last := &r.verdicts[len(r.verdicts)-1]
	last.flow, last.answer, last.prompt, last.latency, last.err = flow, answer, prompt, latency, err
	last.wouldAnswer = err == nil && answeredYes(flow, answer)
}

// ---------------------------------------------------------------- Pub/Sub pull over REST

type pulled struct {
	ackID      string
	attributes map[string]string
	data       []byte
	delivered  time.Time
}

type puller struct {
	project string
	sub     string
	token   func(context.Context) (string, error)
}

func (p *puller) pull(ctx context.Context, max int) ([]pulled, error) {
	var resp struct {
		ReceivedMessages []struct {
			AckID   string `json:"ackId"`
			Message struct {
				Data        string            `json:"data"`
				Attributes  map[string]string `json:"attributes"`
				PublishTime time.Time         `json:"publishTime"`
			} `json:"message"`
		} `json:"receivedMessages"`
	}
	body := map[string]any{"maxMessages": max, "returnImmediately": false}
	endpoint := fmt.Sprintf("%s/projects/%s/subscriptions/%s:pull", pubsubAPI, p.project, p.sub)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, jsonBody(body))
	if err != nil {
		return nil, err
	}
	if err := withToken(ctx, p.token, req); err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("pull: %s: %s", res.Status, readLimited(res))
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("pull: decode: %w", err)
	}
	out := make([]pulled, 0, len(resp.ReceivedMessages))
	for _, rec := range resp.ReceivedMessages {
		data, err := base64.StdEncoding.DecodeString(rec.Message.Data)
		if err != nil {
			data = []byte(rec.Message.Data)
		}
		out = append(out, pulled{ackID: rec.AckID, attributes: rec.Message.Attributes, data: data, delivered: rec.Message.PublishTime})
	}
	return out, nil
}

func (p *puller) ack(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	endpoint := fmt.Sprintf("%s/projects/%s/subscriptions/%s:acknowledge", pubsubAPI, p.project, p.sub)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, jsonBody(map[string]any{"ackIds": ids}))
	if err != nil {
		return err
	}
	if err := withToken(ctx, p.token, req); err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("ack: %s: %s", res.Status, readLimited(res))
	}
	return nil
}

// ---------------------------------------------------------------- http helpers

type tokenSource struct {
	cred  chatevents.Credential
	mu    sync.Mutex
	token string
	until time.Time
}

func (t *tokenSource) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Before(t.until) {
		return t.token, nil
	}
	token, err := t.cred.AccessToken(ctx)
	if err != nil {
		return "", err
	}
	t.token = token
	// Google access tokens live an hour; refresh early rather than discovering it mid-review.
	t.until = time.Now().Add(45 * time.Minute)
	return token, nil
}

func withToken(ctx context.Context, token func(context.Context) (string, error), req *http.Request) error {
	value, err := token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "Bearer "+value)
	req.Header.Set("content-type", "application/json")
	return nil
}

func getJSON(ctx context.Context, token func(context.Context) (string, error), endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if err := withToken(ctx, token, req); err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("GET %s: %s: %s", endpoint, res.Status, readLimited(res))
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decode: %w", endpoint, err)
	}
	return nil
}

func jsonBody(v any) *strings.Reader {
	encoded, err := json.Marshal(v)
	if err != nil {
		// Only reached for a literal map built in this file; a failure here is a bug in the tool, not
		// an operational condition.
		panic(err)
	}
	return strings.NewReader(string(encoded))
}

func readLimited(res *http.Response) string {
	buf := make([]byte, 2048)
	n, _ := res.Body.Read(buf)
	return strings.TrimSpace(string(buf[:n]))
}

func firstOf(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func envDefault(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// ---------------------------------------------------------------- main

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pi-chat-audit: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	project := envDefault("PI_CHAT_PROJECT", "home-ops-513116")
	topicName := envDefault("PI_CHAT_USER_EVENT_TOPIC", "pi-chat-user-events")
	subName := envDefault("PI_CHAT_USER_EVENT_SUB", "pi-chat-user-events-sub")
	clientFile := envDefault("PIBOT_OAUTH_CLIENT", defaultClientFile)
	tokenFile := envDefault("PIBOT_OAUTH_TOKEN", defaultTokenFile)
	// Only used when the credential is the flat three-file shape; the JSON client file carries its own
	// secret and this stays unread.
	secretFile := envDefault("PI_CHAT_USER_OAUTH_SECRET_FILE", "")
	allowlistRaw := envDefault("PI_CHAT_ALLOWLIST", "initialed85@gmail.com,amy.annisbrown@gmail.com")
	runFor, err := time.ParseDuration(envDefault("PI_CHAT_AUDIT_RUN_SECONDS", "90s"))
	if err != nil {
		return fmt.Errorf("PI_CHAT_AUDIT_RUN_SECONDS: %w", err)
	}
	limit := 30
	if err := json.Unmarshal([]byte(envDefault("PI_CHAT_AUDIT_LIMIT", "30")), &limit); err != nil {
		limit = 30
	}
	redact := strings.EqualFold(envDefault("PI_CHAT_AUDIT_REDACT", "off"), "on")

	cred, err := chatevents.LoadCredential(clientFile, secretFile, tokenFile)
	if err != nil {
		return err
	}
	tokens := &tokenSource{cred: cred}
	source := tokens.get

	allow := map[string]bool{}
	allowlist := []string{}
	for _, email := range strings.Split(allowlistRaw, ",") {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		allowlist = append(allowlist, email)
		allow[email] = true
	}

	// The bridge's own INFO lines are the record being reviewed: they say what the listener decided
	// and why. Keeping them at Warn level would hide exactly the thing this tool exists to show.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := &review{textLog: !redact}
	chat := &userChat{token: source}

	cfg := bridge.Config{
		Eyes:                  "none",
		StatusMode:            "none",
		Allowlist:             allowlist,
		RequireMentionInRooms: true,
		// The two settings that make this review worth running: the room classifier is consulted for
		// untagged room messages, and its verdict is treated as binding rather than logged alongside
		// an answer that happens anyway.
		GateRoomRootMode: "enforce",
		ReplyGateMode:    "enforce",
		ReplyGate: &recordingGate{
			inner: chatgate.New(
				envDefault("PI_CHAT_REPLY_GATE_URL", chatgate.DefaultEndpoint),
				envDefault("PI_CHAT_REPLY_GATE_MODEL", chatgate.DefaultModel),
				8*time.Second,
			).WithSchedule(6*time.Second, 8*time.Second),
		},
		// Deliberately short: a gate review never expects an answer from Pi, and a bridge that waits
		// for one here would just stall the review.
		MaxWait:      200 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
		MaxChars:     4000,
	}
	b := bridge.New(chat, noGateway{}, cfg, log)

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	fmt.Printf("audit: project=%s topic=%s sub=%s run=%s limit=%d redact=%v\n", project, topicName, subName, runFor, limit, redact)
	fmt.Printf("allowlist=%v gate=%s model=%s\n", allowlist,
		envDefault("PI_CHAT_REPLY_GATE_URL", chatgate.DefaultEndpoint),
		envDefault("PI_CHAT_REPLY_GATE_MODEL", chatgate.DefaultModel))

	started := time.Now()
	acks := []string{}
	seen := 0
	for {
		if ctx.Err() != nil || seen >= limit {
			break
		}
		batch, err := pullerFor(project, subName, source).pull(ctx, 5)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			fmt.Printf("pull failed: %v\n", err)
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			continue
		}
		if len(batch) == 0 {
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, rec := range batch {
			seen++
			attrs := rec.attributes
			ev, parseErr := chatmsg.ParseWorkspace(rec.data, attrs)
			if parseErr != nil {
				fmt.Printf("\nskipped: %v\n", parseErr)
				acks = append(acks, rec.ackID)
				continue
			}
			sender := firstOf(ev.UserEmail, ev.SenderType+" "+ev.UserID)
			text := ev.Text
			if !r.textLog && !allow[strings.ToLower(ev.UserEmail)] {
				text = "<redacted>"
			}
			path := "room-untagged"
			switch {
			case !ev.HumanSender():
				path = "bot-authored (loop guard drops it)"
			case ev.Mentioned:
				path = "mention (the app path owns it)"
			case ev.DM:
				path = "1:1 (the app path owns it)"
			}
			fmt.Printf("\n%s  space %s  sender %s\n  text: %q\n  path: %s\n",
				rec.delivered.UTC().Format(time.RFC3339), ev.Space, sender, text, path)
			r.verdicts = append(r.verdicts, verdict{space: ev.Space, sender: sender, text: text, path: path})
			if handleErr := b.HandleWorkspace(ctx, rec.ackID, attrs, rec.data); handleErr != nil {
				fmt.Printf("  bridge: %v\n", handleErr)
			}
			acks = append(acks, rec.ackID)
		}
		if err := pullerFor(project, subName, source).ack(ctx, acks); err != nil {
			fmt.Printf("ack failed (messages will be redelivered): %v\n", err)
		} else {
			acks = nil
		}
	}
	if len(acks) > 0 {
		if err := pullerFor(project, subName, source).ack(context.Background(), acks); err != nil {
			fmt.Printf("final ack failed: %v\n", err)
		}
	}

	fmt.Printf("\n--- %d messages reviewed in %s, %d space lookups\n", len(r.verdicts), time.Since(started).Round(time.Millisecond), chat.calls)
	answered, quiet, errored := 0, 0, 0
	bySpace := map[string]int{}
	for _, v := range r.verdicts {
		bySpace[v.space]++
		switch {
		case v.err != nil:
			errored++
		case v.wouldAnswer:
			answered++
		default:
			quiet++
		}
	}
	fmt.Printf("--- gate verdicts: answer=%d quiet=%d gate-error=%d\n", answered, quiet, errored)
	spaces := make([]string, 0, len(bySpace))
	for s := range bySpace {
		spaces = append(spaces, s)
	}
	sort.Strings(spaces)
	for _, s := range spaces {
		fmt.Printf("    %s: %d\n", s, bySpace[s])
	}
	fmt.Println("--- the prompts the gate actually judged (truncated), so the judgement can be reviewed:")
	shown := 0
	for _, v := range r.verdicts {
		if v.prompt == "" {
			continue
		}
		shown++
		if shown > 10 {
			break
		}
		prompt := v.prompt
		if len([]rune(prompt)) > 500 {
			prompt = string([]rune(prompt)[:500]) + "…"
		}
		fmt.Printf("\n[%s] %s -> %s\n%s\n", v.space, v.sender, v.answer, prompt)
	}
	return nil
}

func pullerFor(project, sub string, token func(context.Context) (string, error)) *puller {
	return &puller{project: project, sub: sub, token: token}
}
