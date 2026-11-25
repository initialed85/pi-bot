// Command pi-bot-bridge is the Google Chat half of pi-bot: it subscribes to the Pub/Sub topic
// Google Chat publishes for the app, and drives a live Pi session through the pi-chat-gateway
// extension's HTTP surface, reporting each turn back into the Chat thread as one self-editing
// message.
//
// Pub/Sub rather than an HTTPS webhook because our public DNS returns private addresses, so Google
// cannot reach us. It also happens to fit the work: a Chat webhook must answer in under 30 seconds
// and Pi often takes minutes; an event queue plus a background reply loop has no such ceiling.
//
// Configuration is env-only so the same image runs as a local probe or a pod:
//
//	PI_CHAT_GATEWAY_URL        required, e.g. http://pi-gateway.internal:8090
//	PI_CHAT_GATEWAY_TOKEN      required, must match PI_CHAT_GATEWAY_TOKEN in the Pi pod
//	PI_CHAT_KEY_FILE           service account key with chat.bot (defaults to GOOGLE_APPLICATION_CREDENTIALS)
//	PI_CHAT_PROJECT            GCP project of the subscription (required)
//	PI_CHAT_SUBSCRIPTION       Pub/Sub subscription (required; default pi-chat-events-sub)
//	PI_CHAT_ALLOWLIST          comma-separated sender emails; "*" for anyone; empty denies all
//	PI_CHAT_HTTP_ADDR          listen address for /healthz and /readyz (default :8091)
//	PI_CHAT_MAX_CHARS          Chat message text limit for chunking (default 30000; Chat itself refuses
//	                           more than 32000 on both create and patch)
//	PI_CHAT_DELIVER_AS         followUp | steer (default followUp: a message sent while Pi is working
//	                           queues behind the run and gets its own answer; steer folds it into the
//	                           run that is going)
//	PI_CHAT_STATUS_MODE        patch | bubble | none (default patch: one message carries 👀 -> 🤔 ->
//	                           the answer; bubble posts the answer separately and deletes the status
//	                           message; none posts no status at all)
//	PI_CHAT_EYES               status glyph posted on receipt (default 👀; "none" disables the bubble)
//	PI_CHAT_THINKING           status glyph while Pi works (default 🤔; "none" skips the edit)
//	PI_CHAT_POLL_SECONDS       gateway turn poll interval (default 2)
//	PI_CHAT_MAX_WAIT_MINUTES   give up watching after this (default 30)
//	PI_CHAT_IMAGE_MAX_MB       per-attachment download cap (default 4)
//	PI_CHAT_MAX_IMAGES         attachments forwarded per message (default 4)
//	PI_CHAT_REPLY_GATE_MODE    off|dry-run|enforce (default off; dry-run logs, enforce suppresses/reroutes)
//	PI_CHAT_REPLY_GATE_URL     OpenAI-compatible gate endpoint (default sentiment-analyzer.dev...)
//	PI_CHAT_REPLY_GATE_MODEL   gate model name
//	PI_CHAT_REPLY_GATE_TIMEOUT_SECONDS  classifier deadline for the first attempt (default 3)
//	PI_CHAT_REPLY_GATE_ATTEMPT_MS       per-attempt deadlines, comma separated (default 3000,6000,12000)
//	PI_CHAT_GATE_ROOM_ROOT       ignore|classify (default ignore): send untagged new room messages to the room classifier
//	PI_CHAT_USER_LISTENER        off|on (default off): also consume the user-authorised room listener subscription
//	PI_CHAT_USER_SUBSCRIPTION    Pub/Sub subscription fed by the Workspace Events listener (default pi-chat-user-events-sub)
//	PI_CHAT_USER_TOPIC           the topic that listener publishes to, used when renewing (default pi-chat-user-events)
//	PI_CHAT_USER_RENEW           on|off (default on when the listener is on): recreate the Workspace Events subscription on a timer
//	PI_CHAT_USER_RENEW_MINUTES   renewal interval, must stay inside the measured ~4h subscription lifetime (default 180)
//	PI_CHAT_USER_OAUTH_CLIENT_FILE  file holding {client_id, client_secret} for the human OAuth grant (default /etc/pi-chat-user/oauth-client.json)
//	PI_CHAT_USER_OAUTH_TOKEN_FILE   file holding the refresh token, bare or {"refresh_token": …} (default /etc/pi-chat-user/refresh-token)
//	PI_CHAT_LOG_LEVEL          debug|info|warn|error (default info)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/pubsub"
	"github.com/initialed85/pi-bot/internal/bridge"
	"github.com/initialed85/pi-bot/internal/chatapi"
	"github.com/initialed85/pi-bot/internal/chatevents"
	"github.com/initialed85/pi-bot/internal/gateway"
	"google.golang.org/api/option"
)

type settings struct {
	project      string
	subscription string
	gatewayURL   string
	gatewayToken string
	keyFile      string
	httpAddr     string
	allowlist    []string
	cfg          bridge.Config
	logLevel     string

	userListener        bool
	userSubscription    string
	userTopic           string
	userRenew           bool
	userRenewEvery      time.Duration
	userOAuthClientFile string
	userOAuthSecretFile string
	userOAuthTokenFile  string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pi-bot-bridge:", err)
		os.Exit(1)
	}
}

func run() error {
	s, err := loadSettings()
	if err != nil {
		return err
	}
	logger := newLogger(s.logLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	chat, err := chatapi.New(ctx, s.keyFile)
	if err != nil {
		return err
	}
	gw := gateway.New(s.gatewayURL, s.gatewayToken)
	b := bridge.New(chat, gw, s.cfg, logger.With("component", "bridge"))

	if err := awaitGateway(ctx, gw, logger); err != nil {
		// Not fatal: Pi may be mid-restart, and the subscription is where the messages wait anyway.
		logger.Warn("gateway not reachable at startup", "err", err)
	}

	httpServer := serveHealth(ctx, logger, s.httpAddr, gw, b)

	// One key file, two services: the same app service account holds pubsub.subscriber (to read the
	// Chat events) and chat.bot (to answer), so the bridge never needs workload identity or ADC.
	client, err := pubsub.NewClient(ctx, s.project, option.WithCredentialsFile(s.keyFile))
	if err != nil {
		return fmt.Errorf("pubsub client: %w", err)
	}
	sub := client.Subscription(s.subscription)
	// No sub.Exists() check on purpose: that needs pubsub.subscriptions.get, and the app service
	// account holds only roles/pubsub.subscriber (consume). Receive reports a missing subscription or
	// a missing grant clearly enough, and staying within consume keeps the pod least-privilege.
	_ = sub
	sub.ReceiveSettings = pubsub.ReceiveSettings{
		// One Chat turn at a time into one Pi session keeps the ordering honest; Pi itself queues
		// extra prompts, so parallelism here would only interleave two Chat threads confusingly.
		MaxExtension:  50 * time.Minute,
		NumGoroutines: 2,
	}
	logger.Info("listening for chat events",
		"project", s.project,
		"subscription", s.subscription,
		"gateway", s.gatewayURL,
		"allowlist", strings.Join(s.allowlist, ","),
		"maxChars", s.cfg.MaxChars)

	errCh := make(chan error, 2)
	go func() {
		if err := sub.Receive(ctx, func(mctx context.Context, m *pubsub.Message) {
			handleReceive(mctx, logger, b, m)
		}); err != nil && ctx.Err() == nil {
			errCh <- fmt.Errorf("pubsub receive on %s: %w (check the subscription exists and the SA has roles/pubsub.subscriber)", s.subscription, err)
			return
		}
		errCh <- nil
	}()

	// The room listener runs beside the app path, not instead of it. A failure here is deliberately not
	// fatal: the app path still answers 1:1 messages and tagged room messages, and killing the pod over
	// a listener Google may have simply expired would trade a working half for a broken whole. It is
	// retried, and the renewal loop is what usually fixes it.
	if s.userListener {
		go receiveRoomListener(ctx, logger, client, s, b)
		logger.Info("listening for room messages authorised by a human account",
			"subscription", s.userSubscription,
			"renew_every", s.userRenewEvery.String(),
			"renewal", fmt.Sprintf("%v", s.userRenew))
	}
	if s.userListener && s.userRenew {
		cred, err := chatevents.LoadCredential(s.userOAuthClientFile, s.userOAuthSecretFile, s.userOAuthTokenFile)
		if err != nil {
			logger.Error("room listener credential unavailable; group rooms stay unanswered",
				"client_file", s.userOAuthClientFile, "token_file", s.userOAuthTokenFile, "err", err)
		} else {
			renewer := chatevents.NewRenewer(cred, s.project, s.userTopic, s.userRenewEvery,
				logger.With("component", "room-listener"))
			go renewer.Run(ctx)
		}
	}

	select {
	case err := <-errCh:
		stop()
		if err != nil {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutting down; waiting for in-flight turns")
	}

	// Give in-flight turns a moment to post a final message rather than leaving a "working…" bubble.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { b.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		logger.Warn("exiting with turns still running")
	}
	if httpServer != nil {
		_ = httpServer.Shutdown(context.Background())
	}
	_ = client.Close()
	logger.Info("stopped")
	return nil
}

// receiveRoomListener consumes the subscription that the human-authorised Workspace Events listener
// feeds, and keeps trying: a Workspace Events subscription expires on its own schedule, so a Receive
// failure is usually "the subscription went away between the renewal and now", which the renewal loop
// resolves a few minutes later.
func receiveRoomListener(ctx context.Context, logger *slog.Logger, client *pubsub.Client, s settings, b *bridge.Bridge) {
	for {
		if ctx.Err() != nil {
			return
		}
		userSub := client.Subscription(s.userSubscription)
		userSub.ReceiveSettings = pubsub.ReceiveSettings{
			MaxExtension:  50 * time.Minute,
			NumGoroutines: 1,
		}
		err := userSub.Receive(ctx, func(mctx context.Context, m *pubsub.Message) {
			handleWorkspaceReceive(mctx, logger, b, m)
		})
		if ctx.Err() != nil {
			return
		}
		logger.Error("room listener receive failed; app path unaffected",
			"subscription", s.userSubscription, "err", err,
			"hint", "check the subscription exists, the renewal is running, and the app service account has roles/pubsub.subscriber on it")
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func handleWorkspaceReceive(ctx context.Context, logger *slog.Logger, b *bridge.Bridge, m *pubsub.Message) {
	// Acked whatever happens, for the same reason as the app path: an event that does not parse is not
	// going to parse better on redelivery, and Pi failures are already reported into the thread.
	if err := b.HandleWorkspace(ctx, m.ID, m.Attributes, m.Data); err != nil {
		logger.Error("room listener event failed", "id", m.ID,
			"attrs", truncate(fmt.Sprint(m.Attributes), 200),
			"data", truncate(string(m.Data), 400), "err", err)
	}
	m.Ack()
}

func handleReceive(ctx context.Context, logger *slog.Logger, b *bridge.Bridge, m *pubsub.Message) {
	// Every path through here ends in an ack: a message we cannot parse is not going to get any
	// better on redelivery, and a Pi failure is already reported into the Chat thread.
	err := b.Handle(ctx, m.ID, m.Data)
	if err != nil {
		logger.Error("event handling failed", "id", m.ID, "err", err, "data", truncate(string(m.Data), 400))
		m.Ack()
		return
	}
	m.Ack()
}

func awaitGateway(ctx context.Context, gw *gateway.Client, logger *slog.Logger) error {
	deadline := time.Now().Add(30 * time.Second)
	var last error
	for {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		last = gw.Health(callCtx)
		cancel()
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return last
		}
		logger.Info("waiting for pi gateway", "err", last)
		select {
		case <-ctx.Done():
			return last
		case <-time.After(2 * time.Second):
		}
	}
}

func serveHealth(ctx context.Context, logger *slog.Logger, addr string, gw *gateway.Client, b *bridge.Bridge) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		callCtx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		state, err := gw.State(callCtx)
		w.Header().Set("content-type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pi": state})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pi-bot-bridge: Google Chat -> Pi. See /healthz and /readyz\n"))
	})
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("health server listening", "addr", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	return server
}

func loadSettings() (settings, error) {
	s := settings{
		project:      os.Getenv("PI_CHAT_PROJECT"),
		subscription: envDefault("PI_CHAT_SUBSCRIPTION", "pi-chat-events-sub"),
		gatewayURL:   os.Getenv("PI_CHAT_GATEWAY_URL"),
		gatewayToken: os.Getenv("PI_CHAT_GATEWAY_TOKEN"),
		keyFile:      envDefault("PI_CHAT_KEY_FILE", os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")),
		httpAddr:     envDefault("PI_CHAT_HTTP_ADDR", ":8091"),
		logLevel:     envDefault("PI_CHAT_LOG_LEVEL", "info"),
		cfg:          bridge.DefaultConfig(),
	}
	s.allowlist = splitList(os.Getenv("PI_CHAT_ALLOWLIST"))

	if s.project == "" {
		return s, errors.New("PI_CHAT_PROJECT is required")
	}
	if s.gatewayURL == "" {
		return s, errors.New("PI_CHAT_GATEWAY_URL is required")
	}
	if s.gatewayToken == "" {
		return s, errors.New("PI_CHAT_GATEWAY_TOKEN is required (the gateway refuses to start without one either)")
	}
	if s.keyFile == "" {
		return s, errors.New("PI_CHAT_KEY_FILE or GOOGLE_APPLICATION_CREDENTIALS must point at the app service account key")
	}
	if _, err := os.Stat(s.keyFile); err != nil {
		return s, fmt.Errorf("key file %s: %w", s.keyFile, err)
	}

	s.cfg.Allowlist = s.allowlist
	if one("PI_CHAT_NO_SENDER_PREFIX") {
		s.cfg.PrefixWithSender = false
	}
	s.cfg.StatusMode = envDefault("PI_CHAT_STATUS_MODE", s.cfg.StatusMode)
	s.cfg.DeliverAs = envDefault("PI_CHAT_DELIVER_AS", s.cfg.DeliverAs)
	// The classifier is dev-only and can add seconds on a cold call, so the application is opt-in:
	// set dry-run for review, then enforce only after Ed approves the logs.
	s.cfg.ReplyGateMode = envDefault("PI_CHAT_REPLY_GATE_MODE", "off")
	s.cfg.ReplyGateEndpoint = envDefault("PI_CHAT_REPLY_GATE_URL", "https://sentiment-analyzer.dev.initialed85.cc")
	s.cfg.ReplyGateModel = envDefault("PI_CHAT_REPLY_GATE_MODEL", "unsloth/gemma-4-E4B-it-GGUF:IQ4_XS")
	s.cfg.ReplyGateTimeout = seconds("PI_CHAT_REPLY_GATE_TIMEOUT_SECONDS", 3*time.Second)
	s.cfg.ReplyGateAttempts = attemptSchedule("PI_CHAT_REPLY_GATE_ATTEMPT_MS", s.cfg.ReplyGateTimeout)
	// Room gating is a separate decision from thread gating: an untagged new room message is ignored
	// outright unless this is set to "classify", in which case the room classifier decides whether
	// answering would be an intrusion. Enforce answers in a new thread; dry-run only logs.
	s.cfg.GateRoomRootUntagged = strings.EqualFold(envDefault("PI_CHAT_GATE_ROOM_ROOT", "ignore"), "classify")

	// The room listener is a separate capability from the app path, and it is off by default: it reads
	// every message in every space the authorising human belongs to, so enabling it is an explicit
	// operator decision rather than a side effect of deploying a new image.
	s.userListener = strings.EqualFold(envDefault("PI_CHAT_USER_LISTENER", "off"), "on")
	s.userSubscription = envDefault("PI_CHAT_USER_SUBSCRIPTION", "pi-chat-user-events-sub")
	s.userTopic = envDefault("PI_CHAT_USER_TOPIC", "pi-chat-user-events")
	s.userRenew = !strings.EqualFold(envDefault("PI_CHAT_USER_RENEW", "on"), "off")
	s.userRenewEvery = minutes("PI_CHAT_USER_RENEW_MINUTES", 180*time.Minute)
	// The paths are home-ops's shipped Secret layout (pi-chat-user-oauth projected at 0400 as three
	// files). The reader works out which shape it was given, so a future Secret that ships one JSON
	// client file instead needs no manifest change.
	s.userOAuthClientFile = envDefault("PI_CHAT_USER_OAUTH_CLIENT_FILE", "/etc/pi-chat-user-oauth/client_id")
	s.userOAuthSecretFile = envDefault("PI_CHAT_USER_OAUTH_SECRET_FILE", "/etc/pi-chat-user-oauth/client_secret")
	s.userOAuthTokenFile = envDefault("PI_CHAT_USER_OAUTH_TOKEN_FILE", "/etc/pi-chat-user-oauth/refresh_token")
	if s.userRenewEvery < 30*time.Minute {
		// Renewing every couple of minutes would churn a resource Google rate-limits, and it buys nothing.
		s.userRenewEvery = 30 * time.Minute
	}
	if s.userRenewEvery > 3*time.Hour {
		// The measured lifetime is ~4h from createTime; leaving 3h of headroom keeps a renewal from
		// racing expiry.
		s.userRenewEvery = 3 * time.Hour
	}
	s.cfg.Eyes = envDefault("PI_CHAT_EYES", s.cfg.Eyes)
	s.cfg.Thinking = envDefault("PI_CHAT_THINKING", s.cfg.Thinking)
	s.cfg.PollInterval = seconds("PI_CHAT_POLL_SECONDS", s.cfg.PollInterval)

	s.cfg.MaxWait = minutes("PI_CHAT_MAX_WAIT_MINUTES", s.cfg.MaxWait)
	if v := ints("PI_CHAT_MAX_CHARS"); v > 0 {
		s.cfg.MaxChars = v
	}
	if v := ints("PI_CHAT_IMAGE_MAX_MB"); v > 0 {
		s.cfg.ImageMaxBytes = int64(v) << 20
	}
	if v := ints("PI_CHAT_MAX_IMAGES"); v > 0 {
		s.cfg.MaxImages = v
	}
	return s, nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(handler)
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func splitList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func one(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}

func seconds(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			return time.Duration(n * float64(time.Second))
		}
	}
	return fallback
}

func minutes(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			return time.Duration(n * float64(time.Minute))
		}
	}
	return fallback
}

func ints(key string) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// attemptSchedule reads the classifier's per-attempt deadlines in milliseconds. Unset leaves chatgate's
// own doubling schedule, which is built from the first-attempt timeout.
func attemptSchedule(key string, first time.Duration) []time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil
	}
	var out []time.Duration
	for _, part := range strings.Split(value, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && n > 0 {
			out = append(out, time.Duration(n)*time.Millisecond)
		}
	}
	if len(out) == 0 {
		return []time.Duration{first, 2 * first, 4 * first}
	}
	return out
}

func truncate(text string, max int) string {
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}
