// Package bridge turns Google Chat events into Pi turns and Pi turns back into Chat messages.
//
// The whole design follows from one constraint: a Chat app must answer an interaction quickly, but a
// real Pi run takes minutes. So we never try to answer inline. The Pub/Sub message is acked as soon
// as it is dispatched, a glyph bubble (👀, then 🤔) appears in the thread immediately to show the
// bridge has the message and Pi is working, and the only thing left behind when the run settles is
// the model's answer text. Chat's real reactions cannot carry that state for an app: the reactions
// endpoint refuses service-account auth outright (spike/FINDINGS.md), so the bubble the bridge owns
// is what carries it, and in the default mode the bridge deletes it once the answer is posted.
package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/initialed85/pi-bot/internal/chatapi"
	"github.com/initialed85/pi-bot/internal/chatgate"
	"github.com/initialed85/pi-bot/internal/chatmsg"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// Chat is the slice of chatapi the bridge uses; faked in tests.
type Chat interface {
	Post(ctx context.Context, text string, opts chatapi.PostOpts) (*chatapi.Message, error)
	Update(ctx context.Context, name, text string) error
	Get(ctx context.Context, name string) (*chatapi.Message, error)
	Delete(ctx context.Context, name string) error
	DownloadMedia(ctx context.Context, resourceRef string, maxBytes int64) ([]byte, string, error)
	HasBotMessage(ctx context.Context, space, thread string) (bool, error)
	// GetSpace is how the room listener learns whether a space is a 1:1 or a group room: its own payload
	// does not say, and the routing policy differs between the two.
	GetSpace(ctx context.Context, name string) (*chatapi.SpaceInfo, error)
}

// Gateway is the slice of gateway.Client the bridge uses; faked in tests.
type Gateway interface {
	Prompt(ctx context.Context, req gateway.PromptRequest) (gateway.PromptResponse, error)
	Turn(ctx context.Context, id string) (gateway.Turn, error)
	Abort(ctx context.Context) error
	Command(ctx context.Context, name, args string) error
	State(ctx context.Context) (gateway.State, error)
	Commands(ctx context.Context) ([]gateway.Command, error)
}

// Person is one sender the conversation window mentioned, in the shape the bridge needs: enough to match
// a room event's bare users/<id> to an email, and enough to tell whether pi-chat itself has posted.
type Person struct {
	ID          string
	Email       string
	DisplayName string
	Bot         bool
}

// RoomContext is what surrounds a message: a rendered transcript and the people in it.
type RoomContext struct {
	Transcript string
	People     []Person
}

// Conversation reads the messages around one event. It is the same object that answers "who sent this"
// and "has pi-chat spoken in this thread", because all three come from one window of one space and the
// Chat API is the slowest thing in the pipeline - reading it three times per message is slower than the
// classifier call the message is waiting on.
//
// The result types are local, like SenderIdentity next to IdentityResolver, so the bridge can be tested
// without a Chat credential and does not import the subscription package or a Google client to ask what
// was said.
type Conversation interface {
	Context(ctx context.Context, space, thread string) (RoomContext, error)
}

// SenderIdentity is what Chat published about a user id: the email the allowlist is written in, and the
// name to put on a prompt so a shared session knows whose request it is honouring.
type SenderIdentity struct {
	Email       string
	DisplayName string
}

// IdentityResolver is the lookup the room path needs and its own payload cannot provide. It is an
// interface rather than a concrete client so the bridge can be tested without a Chat credential, and so
// the resolver can be replaced if Google ever ships the email in the event after all.
type IdentityResolver interface {
	Resolve(ctx context.Context, space, userID string) (SenderIdentity, bool, error)
}

type Config struct {
	// Allowlist is the set of sender emails permitted to drive Pi. Empty means deny everything: the
	// bot is reachable by anyone Google lets add it to a space, so the default is closed.
	Allowlist []string
	// RequireMentionInRooms is retained for config compatibility. Group top-level messages are now
	// always ignored unless they mention the app; untagged follow-ups are gated only inside a thread
	// where the app was previously mentioned.
	RequireMentionInRooms bool
	// GateRoomRootUntagged is the original bool form of GateRoomRootMode and is retained so existing
	// configs keep compiling. true means "enforce", false means "ignore". Prefer GateRoomRootMode.
	GateRoomRootUntagged bool
	// GateRoomRootMode decides what a new top-level, untagged group message does, independently of
	// ReplyGateMode:
	//   "ignore" (default) - no model call at all; the message is dropped, which is the historical rule.
	//   "dry-run"          - the room classifier is consulted and its verdict is logged, but a would-be
	//                        answer is dropped, so reviewing the feature is not itself the noise.
	//   "enforce"          - the verdict is acted on. "classify" is accepted as the original name.
	// The separation is the point: a room can be trialled in dry-run while mentions, DMs and tagged
	// threads keep enforcing under ReplyGateMode. Setting the whole bridge to dry-run to trial one flow
	// would silently un-suppress the flows that already gate, i.e. answer messages the gate had been
	// keeping quiet. Room gating is inert when ReplyGateMode is off, because that is what creates the
	// classifier.
	GateRoomRootMode string
	// StatusMode decides what happens to the status bubble a turn gets:
	//   "patch" (default)  - the bubble is rewritten in place, so one message carries 👀 -> 🤔 ->
	//                        the answer. Nothing is deleted, which is what Ed asked for after
	//                        watching the alternative on a phone.
	//   "bubble"           - the bubble is deleted once the answer is posted, leaving only the answer
	//                        in the thread, at the cost of a message appearing then vanishing.
	//   "none"             - no status bubble at all; the answer appears when it lands, with no
	//                        feedback before it.
	StatusMode string

	// DeliverAs decides what happens to a plain Chat message that arrives while Pi is already running.
	// "followUp" (default) queues it behind the run so each message gets its own answer, which is what
	// people expect from a chat; "steer" folds it into the run in progress so the work bends to it.
	// /steer and /queue choose for themselves and ignore this.
	DeliverAs string
	// Eyes is the glyph posted the instant the bridge accepts a message. Set it to "none" to disable
	// the status bubble entirely; leaving it unset means "use the default", which keeps this field
	// workable from an environment where an empty value is indistinguishable from an absent one.
	Eyes string
	// Thinking replaces Eyes while Pi is actually working, and goes away with the status bubble.
	Thinking string
	// PollInterval is how often the bridge asks Pi about the turn.
	PollInterval time.Duration
	// MaxWait bounds a watch. Past it the bridge stops watching and says so; the Pi run is left
	// alone, because aborting someone's long job because a chat got impatient is the wrong default.
	MaxWait time.Duration
	// MaxChars is the per-message text limit used for chunking. The API rejects anything over 32,000
	// characters on both create and patch (measured, see spike/FINDINGS.md), and mentions/links count
	// as citations against that budget, so the default leaves headroom.
	MaxChars int
	// ImageMaxBytes and MaxImages bound inbound media so one 20MB upload cannot wedge the session.
	ImageMaxBytes int64
	MaxImages     int
	// PrefixWithSender names who a prompt came from, which matters when multiple people share a
	// session and Pi needs to know whose request it is honouring.
	PrefixWithSender bool

	// ReplyGateMode is off, dry-run, or enforce. The application and library default to off so a
	// dev-only classifier never becomes an unexpected production dependency.
	ReplyGateMode     string
	ReplyGate         chatgate.Classifier
	ReplyGateEndpoint string
	ReplyGateModel    string

	// GateKeepAlive is how often the bridge re-warms the classifier endpoint in the background. The point
	// is not the model's comfort: an idle inference server drops the model, and the next person's message
	// then pays that load inside the answering path - measured 17982ms for a room decision after ~70
	// minutes idle, against 203-394ms when the endpoint had only been idle ~11 minutes. Zero inherits the
	// default; a negative value warms once at boot and never again.
	GateKeepAlive time.Duration

	// Identity turns a room event's users/<id> into an email when the event carries none - which is all
	// of them. Measured on the deployed listener (2026-11-25T07:00Z, logged verbatim): the sender object
	// a room message ships is exactly {"name":"users/<id>","type":"HUMAN"}, while the allowlist is
	// written in emails. Nil means room messages are only identifiable from a message that *did* carry an
	// email (a mention or a 1:1) since this process started, which makes a freshly rolled pod deaf until
	// somebody tags it.
	Identity IdentityResolver
	// Conversation reads the window of messages a room message landed in, under the human credential that
	// already authorises the room listener. It replaces two things that were each worse: an
	// app-credential thread-ownership read that answers 403 and whose failure dropped messages unseen, and
	// a classifier judging a single bare line. Nil keeps the pre-context behaviour.
	Conversation Conversation
	// GateContextMessages and GateContextChars bound what the classifier is shown. They are bounded
	// because the decision is made by a small model on a warm-but-not-free endpoint: an unbounded
	// transcript is how a 300ms judgement turns into a timeout, and the oldest lines of a 200-message
	// room are the least relevant thing in it.
	GateContextMessages int
	GateContextChars    int
	// IdentitySeed binds known user ids to emails at startup so identity survives a redeploy. Format is
	// "users/<id>=email" or "<legacyId>=email", comma separated (see ParseIdentitySeed). Not a secret:
	// a Chat user id is not a credential, and the email on the other side is already in the allowlist.
	IdentitySeed     map[string]string
	ReplyGateTimeout time.Duration
	// ReplyGateAttempts is the per-attempt deadline schedule for one classification. Empty means
	// chatgate's default, which doubles the first deadline: a slow call is almost always a cold
	// model, and the attempt that wakes it up is not the attempt that should be timed out.
	ReplyGateAttempts []time.Duration
}

func DefaultConfig() Config {
	return Config{
		PollInterval:          2 * time.Second,
		StatusMode:            "patch",
		DeliverAs:             "followUp",
		Eyes:                  "\U0001f440",
		Thinking:              "\U0001f914",
		MaxWait:               30 * time.Minute,
		MaxChars:              30000,
		ImageMaxBytes:         4 << 20,
		MaxImages:             4,
		GateRoomRootUntagged:  false,
		GateRoomRootMode:      "ignore",
		RequireMentionInRooms: true,
		PrefixWithSender:      true,
		ReplyGateMode:         "off",
		ReplyGateTimeout:      3 * time.Second,
		GateKeepAlive:         5 * time.Minute,
		GateContextMessages:   12,
		GateContextChars:      1500,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.PollInterval > 0 {
		d.PollInterval = c.PollInterval
	}
	if c.StatusMode != "" {
		d.StatusMode = strings.ToLower(strings.TrimSpace(c.StatusMode))
	}
	switch d.StatusMode {
	case "bubble", "patch", "none":
	default:
		// New() logs this; a typo in a pod env var should not quietly change what the family sees.
		d.StatusMode = DefaultConfig().StatusMode
	}

	switch strings.ToLower(strings.TrimSpace(c.DeliverAs)) {
	case "followup", "queue", "queued":
		d.DeliverAs = "followUp"
	case "steer", "fold", "interrupt":
		d.DeliverAs = "steer"
	default:
		d.DeliverAs = DefaultConfig().DeliverAs
	}
	if c.Eyes != "" {
		d.Eyes = glyphOrNone(c.Eyes)
	}
	if c.Thinking != "" {
		d.Thinking = glyphOrNone(c.Thinking)
	}
	if c.MaxWait > 0 {
		d.MaxWait = c.MaxWait
	}
	if c.MaxChars >= 200 {
		d.MaxChars = c.MaxChars
	}
	if c.ImageMaxBytes > 0 {
		d.ImageMaxBytes = c.ImageMaxBytes
	}
	if c.MaxImages > 0 {
		d.MaxImages = c.MaxImages
	}
	mode := strings.ToLower(strings.TrimSpace(c.ReplyGateMode))
	switch mode {
	case "off", "dry-run", "enforce":
		d.ReplyGateMode = mode
	default:
		d.ReplyGateMode = DefaultConfig().ReplyGateMode
	}
	if c.ReplyGateTimeout > 0 {
		d.ReplyGateTimeout = c.ReplyGateTimeout
	}
	// 0 means "not set" so a manifest that says nothing still keeps the endpoint warm. Negative is the
	// operator spelling for "warm at boot only".
	if c.GateKeepAlive != 0 {
		d.GateKeepAlive = c.GateKeepAlive
	}
	d.ReplyGateAttempts = c.ReplyGateAttempts
	d.ReplyGate = c.ReplyGate
	// Copied for the same reason as the bool above, and this pair is the reason that comment exists:
	// withDefaults starts from DefaultConfig() and rebuilds field by field, so a field added to Config
	// without a line here is silently nil in every bridge that sets it. Identity was dropped exactly that
	// way and the room lookup looked like a no-op that did nothing. TestWithDefaultsCarriesEveryConfigField
	// now catches the class rather than the instance.
	d.Identity = c.Identity
	d.Conversation = c.Conversation
	d.GateContextMessages = c.GateContextMessages
	if d.GateContextMessages <= 0 {
		d.GateContextMessages = DefaultConfig().GateContextMessages
	}
	d.GateContextChars = c.GateContextChars
	if d.GateContextChars <= 0 {
		d.GateContextChars = DefaultConfig().GateContextChars
	}
	d.IdentitySeed = c.IdentitySeed
	// A bool has no sentinel to test against, so it is copied rather than defaulted: leaving it out
	// silently drops the operator's setting.
	d.GateRoomRootUntagged = c.GateRoomRootUntagged
	roomMode := strings.ToLower(strings.TrimSpace(c.GateRoomRootMode))
	switch roomMode {
	case "":
		// Only the legacy bool was set.
		if c.GateRoomRootUntagged {
			roomMode = "enforce"
		} else {
			roomMode = "ignore"
		}
	case "classify", "on", "true":
		roomMode = "enforce"
	case "off", "false":
		roomMode = "ignore"
	case "ignore", "dry-run", "enforce":
	default:
		roomMode = DefaultConfig().GateRoomRootMode
	}
	d.GateRoomRootMode = roomMode
	d.ReplyGateEndpoint = c.ReplyGateEndpoint
	d.ReplyGateModel = c.ReplyGateModel
	d.Allowlist = c.Allowlist
	d.RequireMentionInRooms = c.RequireMentionInRooms
	d.PrefixWithSender = c.PrefixWithSender
	return d
}

type Bridge struct {
	chat Chat
	gw   Gateway
	cfg  Config
	log  *slog.Logger

	mu          sync.Mutex
	seen        map[string]struct{}
	seenQ       []string
	watches     map[string]*watch             // space -> in-flight watch, so /stop can reach it
	delivered   map[string]bool               // gateway turn id -> its answer has already been posted
	summaries   map[string]string             // thread -> <=30-word running summary for the reply gate
	roomAnswers map[string]string             // space -> the last thing pi-chat said there, for the room gate
	piThreads   map[string]struct{}           // group threads where a message mentioned the app
	spaces      map[string]*chatapi.SpaceInfo // space name -> Chat's own description of it, for routing
	// people maps a Chat user id (`users/<legacyId>`, or the bare legacyId) to an email. It is learned at
	// runtime, not configured: a message that mentions the app arrives on the app credential with an email
	// and on the room listener with only an id, so the tagged message teaches the bridge who the id is.
	// Room answers therefore depend on a person having tagged the app at least once since the last roll.
	people    map[string]string
	conv      Conversation
	gate      chatgate.Classifier
	lastSpace string
	watchers  sync.WaitGroup
}

type watch struct {
	// promptID is the Chat message this watch exists for; turnID is the gateway turn that will carry its
	// answer. They match in the common case and differ when our prompt folded into a run that was
	// already going - which is precisely the case that would otherwise post one answer twice.
	promptID string
	turnID   string
	space    string
	thread   string
	replyID  string // deterministic client- id, so a redelivery cannot fork a second reply
	statusID string // likewise for the status bubble, so a redelivery reuses the one already up

	// queued is how long the session took to start on the prompt after the bridge dispatched it. It is
	// the part of "after" that no amount of model speed can fix, so it gets its own number instead of
	// hiding inside the total.
	queued time.Duration
}

func New(chat Chat, gw Gateway, cfg Config, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	defaults := DefaultConfig()
	if mode := strings.ToLower(strings.TrimSpace(cfg.StatusMode)); mode != "" {
		switch mode {
		case "bubble", "patch", "none":
		default:
			log.Warn("unrecognised status mode, falling back to the default", "value", cfg.StatusMode, "using", defaults.StatusMode)
		}
	}
	switch strings.ToLower(strings.TrimSpace(cfg.GateRoomRootMode)) {
	case "", "ignore", "off", "false", "dry-run", "classify", "on", "true", "enforce":
	default:
		log.Warn("unrecognised room root gate mode, falling back to ignoring room messages",
			"value", cfg.GateRoomRootMode, "valid", "ignore|dry-run|enforce")
	}
	resolved := cfg.withDefaults()
	var gate chatgate.Classifier = resolved.ReplyGate
	if resolved.ReplyGateMode != "off" && gate == nil {
		gate = chatgate.New(resolved.ReplyGateEndpoint, resolved.ReplyGateModel, resolved.ReplyGateTimeout).
			WithSchedule(resolved.ReplyGateAttempts...)
	}
	b := &Bridge{
		chat:        chat,
		gw:          gw,
		cfg:         resolved,
		log:         log,
		seen:        map[string]struct{}{},
		watches:     map[string]*watch{},
		delivered:   map[string]bool{},
		summaries:   map[string]string{},
		roomAnswers: map[string]string{},
		piThreads:   map[string]struct{}{},
		gate:        gate,
		conv:        resolved.Conversation,
	}
	// Identity bindings are the one piece of state a redeploy otherwise loses, and the room path needs
	// them from the very first message. Seeding them from config is what makes a cold pod able to answer
	// a room at all; runtime learning (bindIdentity) still adds to it.
	if len(resolved.IdentitySeed) > 0 {
		b.people = map[string]string{}
		for id, email := range resolved.IdentitySeed {
			for _, key := range identityAliases(id) {
				b.people[key] = email
			}
		}
		b.log.Info("room identity seeded", "people", len(resolved.IdentitySeed))
	}
	if warmer, ok := gate.(interface{ Warm(context.Context) error }); ok {
		keepAlive := resolved.GateKeepAlive
		if keepAlive == 0 {
			keepAlive = DefaultConfig().GateKeepAlive
		}
		// The keeper also has to re-warm rather than skip: Warm() short-circuits once it has succeeded
		// once, and "this client succeeded an hour ago" is not evidence the server still holds the model.
		keeper, canKeep := gate.(interface{ KeepWarm(context.Context) error })
		go func() {
			if err := warmer.Warm(context.Background()); err != nil {
				log.Warn("reply gate warm-up failed", "err", err)
			} else {
				log.Info("reply gate warm")
			}
			if keepAlive < 0 || !canKeep {
				return
			}
			ticker := time.NewTicker(keepAlive)
			defer ticker.Stop()
			for range ticker.C {
				if err := keeper.KeepWarm(context.Background()); err != nil {
					log.Warn("reply gate keep-warm failed", "err", err)
					continue
				}
				log.Info("reply gate kept warm", "every", keepAlive)
			}
		}()
	}
	return b
}

// canonicalTurn answers "which gateway turn carries this prompt's reply". Normally it is the id we sent;
// when the prompt folded into a run already in progress it is that run's id, and polling our own id
// instead would watch a turn that never moves.
func canonicalTurn(resp gateway.PromptResponse, fallback string) string {
	if resp.TurnID == "" {
		return fallback
	}
	return resp.TurnID
}

// SeenLen reports the dedupe window size (used by tests and /debug).
func (b *Bridge) SeenLen() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.seen)
}

const dedupeWindow = 4096

// queuePatience is how many answering budgets a single watch may span. One budget is spent waiting in
// the gateway queue, and the full one is re-armed when the run actually starts; this is the ceiling on
// a prompt that never starts at all.
const queuePatience = 3

// glyphOrNone maps the explicit "off" spellings of a status glyph to a disabled glyph.
func glyphOrNone(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none", "off", "false", "0":
		return ""
	}
	return v
}

// markSeen records an id and reports whether it is new. Pub/Sub is at-least-once and Google retries
// webhooks that look slow, so every path into Pi goes through here.
func (b *Bridge) markSeen(id string) bool {
	if id == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[id]; ok {
		return false
	}
	b.seen[id] = struct{}{}
	b.seenQ = append(b.seenQ, id)
	for len(b.seenQ) > dedupeWindow {
		old := b.seenQ[0]
		b.seenQ = b.seenQ[1:]
		delete(b.seen, old)
	}
	return true
}

// Handle processes one Pub/Sub message body. It returns as soon as the work is dispatched: the
// caller acks, and the reply loop runs in the background. Returning an error means "could not even
// parse this", which the caller logs and acks anyway (redelivery will not help).
func (b *Bridge) Handle(ctx context.Context, id string, data []byte) error {
	ev, err := chatmsg.Parse(data)
	if err != nil {
		return err
	}
	dedupe := ev.MessageName
	if dedupe == "" {
		dedupe = id
	}
	if !b.markSeen(dedupe) {
		b.log.Info("dup", "id", id, "message", dedupe)
		return nil
	}
	b.log.Info("event", "kind", ev.Kind, "space", ev.Space, "user", ev.UserEmail, "text", truncate(ev.Prompt(), 90), "atts", len(ev.Attachments))
	b.bindIdentity(ev)

	switch ev.Kind {
	case chatmsg.KindAddedToSpace:
		b.onAddedToSpace(ctx, ev)
		return nil
	case chatmsg.KindRemovedToSpace:
		b.log.Info("removed from space", "space", ev.Space)
		return nil
	case chatmsg.KindCardClick, chatmsg.KindAppCommand:
		if !b.allowed(ev) {
			return nil
		}
		b.reply(ctx, ev, "That interaction type isn't wired up yet - talk to me in text.")
		return nil
	case chatmsg.KindMessage:
		return b.onMessage(ctx, ev)
	default:
		b.log.Warn("ignoring unknown event", "kind", ev.Kind)
		return nil
	}
}

// identityKeys returns every way this event names its human: the resource name, the bare legacy id, and
// whichever of the two Chat happened to fill. It defers to identityAliases so a seed, a binding and a
// lookup all agree on what "the same person" means - two hand-written lists of aliases is how those
// three quietly stop agreeing.
func identityKeys(ev *chatmsg.Event) []string {
	keys := make([]string, 0, 4)
	for _, k := range []string{ev.UserID, ev.UserLegacyID} {
		if k == "" {
			continue
		}
		keys = append(keys, identityAliases(k)...)
	}
	return keys
}

// bindIdentity pairs a Chat user id with the email that arrived with it, for the benefit of the room
// listener, whose sender object carries no email at all. Only a message that has an email can teach
// this, so in practice the @-mentions and DMs are what populate the map: a person who has never tagged
// the app since the last roll has no identity the room path can use, and that is stated in the log
// rather than guessed around.
func (b *Bridge) bindIdentity(ev *chatmsg.Event) {
	if ev.UserEmail == "" {
		return
	}
	keys := identityKeys(ev)
	if len(keys) == 0 {
		return
	}
	b.mu.Lock()
	if b.people == nil {
		b.people = map[string]string{}
	}
	for _, k := range keys {
		b.people[k] = ev.UserEmail
	}
	b.mu.Unlock()
}

// resolveIdentity fills in the email a room message is missing from a binding learned earlier, and
// reports which key matched so the log says where an identity came from instead of presenting a
// inference as a fact.
func (b *Bridge) resolveIdentity(ev *chatmsg.Event) (string, bool) {
	if ev.UserEmail != "" {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, k := range identityKeys(ev) {
		if email, ok := b.people[k]; ok {
			ev.UserEmail = email
			return k, true
		}
	}
	return "", false
}

// roomIdentitySources name where an identity came from, so the log never presents an inference as a
// fact read off the message.
const (
	identityFromTaggedMessage = "tagged-message"
	identityFromSpaceHistory  = "space-message-history"
	identityFromConversation  = "conversation-window"
	identityFromSeed          = "seed"
)

// resolveRoomIdentity fills in the email a room message is missing, in the cheapest order available:
// a binding learned from a message that carried an email, then one configured at startup, then a read of
// the space's own recent messages under the human credential that already authorises the listener.
//
// It fails closed and says so. A lookup that errors (scope missing, subscription gone, Google being
// Google) must not be mistaken for "that person is unknown", and neither may be mistaken for permission.
func (b *Bridge) resolveRoomIdentity(ctx context.Context, ev *chatmsg.Event) (string, string, bool) {
	if ev.UserEmail != "" {
		return "", "", false
	}
	if key, ok := b.resolveIdentity(ev); ok {
		source := identityFromTaggedMessage
		if b.seededIdentity(key, ev.UserEmail) {
			source = identityFromSeed
		}
		return key, source, true
	}

	resolver := b.cfg.Identity
	// The conversation window answers "who is users/<id>" from the same read that answers "what was said
	// around this message", and it is the only source that works for a person who has never tagged the app.
	// It goes before the dedicated lookup because the lookup would be a second read of the same space.
	if b.conv != nil {
		if email, key, ok := b.identityFromWindow(ctx, ev); ok {
			ev.UserEmail = email
			b.bindIdentity(ev)
			return key, identityFromConversation, true
		}
	}
	if resolver == nil {
		return "", "", false
	}
	userID := ev.UserID
	if userID == "" {
		userID = ev.UserLegacyID
		if userID != "" && !strings.HasPrefix(userID, "users/") {
			userID = "users/" + userID
		}
	}
	if userID == "" {
		return "", "", false
	}

	identity, found, err := resolver.Resolve(ctx, ev.Space, userID)
	if err != nil {
		b.log.Warn("room identity lookup failed; the message is refused rather than guessed around",
			"space", ev.Space, "id", userID, "err", err)
		return userID, "", false
	}
	if !found || identity.Email == "" {
		return userID, "", false
	}

	ev.UserEmail = identity.Email
	if ev.UserDisplayName == "" {
		ev.UserDisplayName = identity.DisplayName
	}
	b.bindIdentityKey(userID, identity.Email)
	return userID, identityFromSpaceHistory, true
}

// bindIdentityKey records a pairing learned from a lookup so the next message from the same person costs
// nothing.
func (b *Bridge) bindIdentityKey(userID, email string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.people == nil {
		b.people = map[string]string{}
	}
	for _, key := range identityAliases(userID) {
		b.people[key] = email
	}
}

func (b *Bridge) seededIdentity(key, email string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	seeded, ok := b.cfg.IdentitySeed[key]
	return ok && seeded == email
}

// ParseIdentitySeed reads the "users/<id>=email,users/<otherId>=email" form an operator can put in an
// environment variable or a manifest. Anything malformed is reported rather than half applied, because a
// silently dropped binding shows up later as a room message being refused for no visible reason.
func ParseIdentitySeed(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, email, ok := strings.Cut(part, "=")
		id, email = strings.TrimSpace(id), strings.TrimSpace(email)
		if !ok || id == "" || email == "" || !strings.Contains(email, "@") {
			return nil, fmt.Errorf("bridge: identity seed %q wants the form users/<id>=someone@example.com", part)
		}
		out[id] = email
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// identityAliases returns both spellings a Chat user id arrives with, so a seed written one way matches a
// message written the other.
func identityAliases(userID string) []string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil
	}
	out := []string{userID}
	if bare, ok := strings.CutPrefix(userID, "users/"); ok && bare != "" {
		out = append(out, bare)
	} else if !strings.Contains(userID, "/") {
		out = append(out, "users/"+userID)
	}
	return out
}

func (b *Bridge) allowed(ev *chatmsg.Event) bool {
	if ev.UserEmail == "" {
		// No identity to check (a bot-authored or malformed event) is not permission to run Pi.
		return false
	}
	if ev.IdentityMismatch {
		b.log.Warn("sender and envelope identity disagree; refusing", "sender", ev.UserEmail, "space", ev.Space)
		return false
	}
	for _, allowed := range b.cfg.Allowlist {
		if allowed == "*" {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(allowed), ev.UserEmail) {
			return true
		}
	}
	return false
}

func (b *Bridge) onAddedToSpace(ctx context.Context, ev *chatmsg.Event) {
	text := "Pi's here. Mention me with @pi-chat, or just message me in this space.\n\nTry `/help` for commands."
	if _, err := b.chat.Post(ctx, text, chatapi.PostOpts{Space: ev.Space, ClientID: clientID("hello-" + ev.Space)}); err != nil {
		b.log.Warn("greeting failed", "space", ev.Space, "err", err)
	}
}

func (b *Bridge) onMessage(ctx context.Context, ev *chatmsg.Event) error {
	if !b.allowed(ev) {
		b.log.Warn("rejected sender", "email", ev.UserEmail, "space", ev.Space)
		// Deliberately silent: a bot that answers strangers tells them the door is here.
		return nil
	}
	if !ev.DM && ev.Mentioned {
		b.markPiThread(ev.Thread)
	}
	if !b.groupMessageEligible(ctx, ev) {
		b.log.Info("group message outside a pi-chat thread; ignoring", "space", ev.Space, "thread", ev.Thread)
		return nil
	}
	if ev.Prompt() == "" && len(ev.Attachments) == 0 {
		b.reply(ctx, ev, "I got an empty message.")
		return nil
	}

	if ev.Slash {
		return b.onCommand(ctx, ev)
	}

	// Read once, here, so the classifier and Pi see the same conversation and the Chat API is asked once.
	// A mention needs it as much as an untagged message does - arguably more, since being @'d into the
	// middle of a thread is the case where the thing being asked about is not in the message.
	if b.conv != nil && !ev.DM {
		if err := b.readContext(ctx, ev); err != nil {
			b.log.Warn("no conversation window for this message", "space", ev.Space, "thread", ev.Thread, "err", err)
		}
	}
	prompt := ev.Prompt()
	// Pi gets the same window the classifier judged on. A mention that lands three messages into a
	// thread pi-chat has not seen is otherwise a question about a request the bridge has never read: the
	// root of spaces/qi7VbgAAAAE/threads/3fbZOFDt3zI is from 2026-11-23 and asks for a fortnightly pay
	// schedule, and the message that woke pi-chat on 2026-11-26 was "still no non-mentions then i guess".
	if ev.Conversation != "" && !ev.DM {
		prompt = "Conversation in this room so far, oldest first, read from Google Chat just now. " +
			"The new message to act on is quoted after it.\n" + ev.Conversation + "\nNew message: " + prompt
	}
	replyThread, suppress := b.replyGate(ctx, ev, prompt)
	if suppress {
		return nil
	}
	if b.cfg.PrefixWithSender {
		prompt = fmt.Sprintf("%s <%s>: %s", ev.Who(), ev.UserEmail, prompt)
	}
	if len(prompt) > 16000 {
		prompt = prompt[:16000] + "\n… [truncated by the bridge]"
	}

	images, notes := b.ingestAttachments(ctx, ev)
	if notes != "" {
		if prompt == "" {
			prompt = notes
		} else {
			prompt += "\n\n" + notes
		}
	}

	turnID := shortID(ev.MessageName)
	req := gateway.PromptRequest{
		ID:        turnID,
		Text:      prompt,
		Images:    images,
		DeliverAs: b.cfg.DeliverAs,
		User:      fmt.Sprintf("%s <%s>", ev.Who(), ev.UserEmail),
		Space:     ev.Space,
		ReplyTo:   ev.MessageName,
	}
	resp, err := b.gw.Prompt(ctx, req)
	if err != nil {
		b.log.Error("prompt failed", "turn", turnID, "err", err)
		b.reply(ctx, ev, fmt.Sprintf("⚠️ Couldn't reach Pi: %v", err))
		return nil
	}
	if resp.Folded {
		b.log.Info("prompt folded into the run already going", "prompt", turnID, "turn", canonicalTurn(resp, turnID))
	}
	b.trackThread(ctx, turnID, canonicalTurn(resp, turnID), ev, replyThread)
	return nil
}

// replyGate decides whether an untagged message should reach Pi and which Chat thread should receive
// it. Dry-run deliberately returns today's route and never suppresses a prompt.
func (b *Bridge) replyGate(ctx context.Context, ev *chatmsg.Event, message string) (string, bool) {
	replyThread := ev.Thread
	flow := chatgate.FlowShouldRespond
	if ev.DM {
		flow = chatgate.FlowThreadRequest
	} else if isTopLevel(ev) || (b.conv != nil && ev.ContextRead && !ev.ThreadHasBot) {
		// Either a new room message, or one inside a thread the window shows pi-chat has never spoken in.
		// The second case is an intrusion question, not a continuation one, and should-respond's prompt
		// assumes the bot is already a party to the thread - asking it anyway is how a bot barges into two
		// people arguing.
		flow = chatgate.FlowRoomRespond
	}
	// The room flow answers to its own switch, so trialling a room in dry-run cannot turn the flows
	// that already gate into no-ops.
	mode := b.cfg.ReplyGateMode
	if flow == chatgate.FlowRoomRespond {
		mode = b.cfg.GateRoomRootMode
	}
	if b.gate == nil || mode == "off" || mode == "ignore" || ev.Mentioned {
		return replyThread, false
	}
	// The window is read here rather than earlier so a message that is never going to be classified - a
	// mention, or a room with gating switched off - costs no Chat read at all.
	if b.conv != nil && !ev.DM {
		if err := b.readContext(ctx, ev); err != nil {
			b.log.Warn("classifying without conversation context", "space", ev.Space, "thread", ev.Thread, "err", err)
		}
	}
	started := time.Now()
	summary := b.summaryFor(ev.Thread)
	if flow == chatgate.FlowRoomRespond {
		// A new room message has no thread to summarise. What it does have is whatever pi-chat last said
		// in this room, which is what "that seems cold, what was it overnight?" is referring to.
		summary = b.roomAnswerFor(ev.Space)
	} else if !ev.DM {
		// The running summary is enrichment, not a decision. It gets its own short deadline so a
		// slow summary cannot eat the budget the actual respond/ignore call needs.
		summaryCtx, cancelSummary := context.WithTimeout(ctx, 4*time.Second)
		answer, _, _, err := b.gate.Classify(summaryCtx, chatgate.Request{Flow: chatgate.FlowSummary, Summary: summary, Sender: ev.Who(), Message: message, Transcript: ev.Conversation})
		cancelSummary()
		if err == nil {
			summary = summaryWords(answer)
			b.setSummary(ev.Thread, summary)
		}
	}
	answer, prompt, _, err := b.gate.Classify(ctx, chatgate.Request{Flow: flow, Summary: summary, Sender: ev.Who(), Message: message, Transcript: ev.Conversation})
	if err == nil && flow != chatgate.FlowSummary {
		if _, ok := exactGateToken(answer, flow); !ok {
			err = fmt.Errorf("invalid %s classifier answer %q", flow, answer)
		}
	}
	action := "dry-run-forward"
	if err != nil {
		// The classifier could not decide. In a 1:1 with the app the message is addressed by
		// construction, so the fallback there is simply the default route. Everywhere else an
		// undecided message gets no reply: guessing would put unasked-for text in front of other
		// people. A mention never reaches this path, so tagging remains the guaranteed way to get
		// an answer when the small model is having a bad day.
		if ev.DM {
			action = "fallback-inline"
		} else {
			action = "fallback-suppress"
		}
	} else if strings.EqualFold(firstWord(answer), "NO") && !ev.DM {
		action = "suppress"
	} else if ev.DM && strings.EqualFold(firstWord(answer), "THREAD") {
		action = "thread"
	} else if ev.DM {
		action = "inline"
	} else {
		action = "forward"
	}
	decision := action
	loggedAction := action
	if mode == "dry-run" {
		loggedAction = "dry-run-" + action
	}
	// user and space are on this line because reviewing a dry-run means answering "who would the bot have
	// replied to, and where", and a room judgement with no author is not reviewable - it is a count.
	b.log.Info("reply gate", "thread", ev.Thread, "space", ev.Space, "user", ev.UserEmail, "flow", flow, "context_chars", len(ev.Conversation), "thread_has_bot", ev.ThreadHasBot, "prompt", prompt, "answer", answer, "latency_ms", time.Since(started).Milliseconds(), "action", loggedAction)
	if flow == chatgate.FlowRoomRespond && mode != "enforce" {
		// Dry-run measures the room classifier without putting new text in a shared room: a would-be
		// room answer is logged and dropped, so reviewing the feature cannot itself be the noise.
		return replyThread, true
	}
	if mode == "enforce" {
		switch decision {
		case "suppress", "fallback-suppress":
			return replyThread, true
		case "forward":
			if flow == chatgate.FlowRoomRespond && (replyThread == "" || replyThread == ev.Space) {
				// Chat normally gives a new room message its own thread, and the answer belongs there, so
				// the room root stays clean. Only when it did not is the answer a root-level post.
				replyThread = ""
			}
		case "thread":
			// ev.Thread is the DM's synthetic space thread. Empty means create a new Chat thread.
			replyThread = ""
		case "inline", "fallback-inline":
			// Keep the existing DM route: the bridge treats the DM space as its inline thread.
		}
	}
	return replyThread, false
}

func (b *Bridge) groupMessageEligible(ctx context.Context, ev *chatmsg.Event) bool {
	if ev.DM || ev.Mentioned {
		return true
	}
	// Preserve the old no-gate behaviour byte-for-byte. The persisted ownership lookup is only
	// active when reply gating is explicitly enabled.
	if b.cfg.ReplyGateMode == "off" {
		return !b.cfg.RequireMentionInRooms || ev.SpaceType == "DM"
	}
	// A new top-level message is never a continuation, even though Chat gives it a synthetic thread
	// name. Historically that meant no model call at all; with room gating on it reaches the room
	// classifier instead, which decides whether answering would be an intrusion.
	if isTopLevel(ev) {
		return b.cfg.GateRoomRootMode == "dry-run" || b.cfg.GateRoomRootMode == "enforce"
	}
	if b.isPiThread(ev.Thread) {
		return true
	}
	if b.conv != nil {
		// The window answers the ownership question the app credential cannot: its senders carry
		// pi-chat's own posts, and reading it costs one call that is also feeding the classifier.
		if err := b.readContext(ctx, ev); err != nil {
			b.log.Warn("no conversation window; the message goes to the classifier rather than being dropped unseen",
				"space", ev.Space, "thread", ev.Thread, "err", err)
			return true
		}
		if ev.ThreadHasBot {
			b.markPiThread(ev.Thread)
			return true
		}
		// A thread pi-chat has never spoken in is an intrusion decision rather than a continuation one, so
		// it is put to the room classifier - the prompt that weighs "would answering this be barging in" -
		// instead of being ignored on a guess. That is what makes "so what's the scoop mr. bot, are you
		// working today or not?" recoverable while leaving two people mid-argument alone.
		return b.cfg.GateRoomRootMode == "dry-run" || b.cfg.GateRoomRootMode == "enforce"
	}
	owned, err := b.chat.HasBotMessage(ctx, ev.Space, ev.Thread)
	if err != nil {
		b.log.Warn("thread ownership lookup failed", "space", ev.Space, "thread", ev.Thread, "err", err)
		return false
	}
	if owned {
		b.markPiThread(ev.Thread)
	}
	return owned
}

// readContext fills in what the message arrived inside: the transcript, and whether pi-chat already
// speaks in this thread. One call per message; the history reader caches the space window for a few
// seconds, so the identity lookup and the gate share it.
//
// The deadline is short and separate from the message's own context on purpose: a window read measured
// 1.6s against the live API, and the gate call that needs it has 3s in total.
func (b *Bridge) readContext(ctx context.Context, ev *chatmsg.Event) error {
	if b.conv == nil || ev.Space == "" || ev.ContextRead {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	c, err := b.conv.Context(readCtx, ev.Space, ev.Thread)
	if err != nil {
		return err
	}
	ev.ContextRead = true
	ev.Conversation = c.Transcript
	for _, p := range c.People {
		if p.Bot {
			ev.ThreadHasBot = true
			break
		}
	}
	return nil
}

// identityFromWindow answers "who sent this" from the messages around it. The room listener ships a
// sender as {"name":"users/<id>","type":"HUMAN"} with no email, while messages.list in the same space
// ships the same id with an email Chat publishes next to it - so the read that provides context also
// provides the identity, for a person who has never once tagged the app.
func (b *Bridge) identityFromWindow(ctx context.Context, ev *chatmsg.Event) (string, string, bool) {
	c, err := b.conv.Context(ctx, ev.Space, "")
	if err != nil {
		b.log.Warn("conversation window unreadable, so the sender is unidentified",
			"space", ev.Space, "id", ev.UserID, "err", err)
		return "", "", false
	}
	known := map[string]bool{}
	for _, k := range identityKeys(ev) {
		known[k] = true
	}
	if len(known) == 0 {
		return "", "", false
	}
	for _, p := range c.People {
		if p.Email == "" {
			continue
		}
		for _, alias := range identityAliases(p.ID) {
			if known[alias] {
				return p.Email, p.ID, true
			}
		}
	}
	return "", "", false
}

func isTopLevel(ev *chatmsg.Event) bool {
	if ev.Thread == "" || ev.Thread == ev.Space || ev.MessageName == "" {
		return true
	}
	parts := strings.Split(ev.MessageName, "/")
	if len(parts) == 0 {
		return true
	}
	messageID := parts[len(parts)-1]
	if dot := strings.IndexByte(messageID, '.'); dot >= 0 {
		// Chat uses messageId.messageId for a top-level message and messageId.threadId for
		// a reply. The thread resource itself is not enough to distinguish those cases.
		return messageID[:dot] == messageID[dot+1:]
	}
	return true
}

func (b *Bridge) markPiThread(thread string) {
	if thread == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.piThreads[thread] = struct{}{}
}

func (b *Bridge) isPiThread(thread string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.piThreads[thread]
	return ok
}

func (b *Bridge) summaryFor(thread string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.summaries[thread]
}

func (b *Bridge) setSummary(thread, summary string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.summaries[thread] = summary
}

// roomAnswerFor is what pi-chat last said in this space, as a short quote. The room gate has no thread
// summary to work from, so this is the only context it gets about what the bot itself has been doing.
func (b *Bridge) roomAnswerFor(space string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.roomAnswers[space]
}

func (b *Bridge) noteRoomAnswer(space, text string) {
	if space == "" || strings.TrimSpace(text) == "" {
		return
	}
	snippet := summaryWords(text)
	if snippet == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roomAnswers[space] = "pi-chat just said: " + snippet
}

func summaryWords(value string) string {
	words := strings.Fields(strings.TrimSpace(value))
	if len(words) > 30 {
		words = words[:30]
	}
	return strings.Join(words, " ")
}

func firstWord(value string) string {
	words := strings.Fields(value)
	if len(words) == 0 {
		return ""
	}
	return words[0]
}

func exactGateToken(answer, flow string) (string, bool) {
	token := strings.ToUpper(strings.TrimSpace(answer))
	switch flow {
	case chatgate.FlowShouldRespond, chatgate.FlowRoomRespond:
		return token, token == "YES" || token == "NO"
	case chatgate.FlowThreadRequest:
		return token, token == "THREAD" || token == "INLINE"
	default:
		return token, false
	}
}

// track registers a turn and starts the goroutine that turns it into Chat messages. base is the
// caller's context (the Pub/Sub receive context), which is what cancels a watch on shutdown.
func (b *Bridge) track(base context.Context, promptID, turnID string, ev *chatmsg.Event) {
	b.trackThread(base, promptID, turnID, ev, ev.Thread)
}

func (b *Bridge) trackThread(base context.Context, promptID, turnID string, ev *chatmsg.Event, replyThread string) {
	w := &watch{
		promptID: promptID,
		turnID:   turnID,
		space:    ev.Space,
		thread:   replyThread,
		replyID:  clientID("reply-" + promptID),
		statusID: clientID("status-" + promptID),
	}
	b.mu.Lock()
	b.watches[ev.Space] = w
	b.lastSpace = ev.Space
	b.mu.Unlock()
	b.watchers.Add(1)
	start := time.Now()
	go func() {
		defer b.watchers.Done()
		b.watch(base, w, start)
	}()
}

// Watched reports the turn currently being reported into a space, if any.
func (b *Bridge) Watched(space string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.watches[space]
	if !ok {
		return "", false
	}
	return w.turnID, true
}

// claimTurn reports whether this is the first watch to reach a settled gateway turn.
func (b *Bridge) claimTurn(turnID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.delivered[turnID] {
		return false
	}
	b.delivered[turnID] = true
	return true
}

func (b *Bridge) forget(space string) {
	b.mu.Lock()
	delete(b.watches, space)
	b.mu.Unlock()
}

// watch polls the gateway and keeps one turn's state visible in Chat until it finishes, the wait
// deadline passes, or the bridge is shutting down. It never renders Pi's progress: no partial
// answers, no step counts, no timings, no model names. Progress belongs in the bridge log and in the
// terminal; what Chat gets is a glyph that says "I have it / Pi is working" and then the answer.
func (b *Bridge) watch(base context.Context, w *watch, start time.Time) {
	defer b.forget(w.space)

	poll, cancelPoll := context.WithTimeout(base, b.cfg.MaxWait*queuePatience)
	defer cancelPoll()

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	// The answering budget belongs to the run, and starts when the run starts. A prompt sitting in the
	// gateway queue while earlier turns finish is not this turn working.
	deadline := start.Add(b.cfg.MaxWait)

	bubble := b.openStatus(w)
	thinkingShown := false
	failures := 0
	started := false
	queuedNotice := false

	for {
		select {
		case <-base.Done():
			// The bridge is going away. Say so in the thread rather than leaving a bubble that says
			// "working" forever - it is the last thing we can tell that user.
			b.setStatus(w, bubble, "⚠️ The bridge stopped while that was running, so it can't report the answer. Open the terminal (or message again) to see what Pi produced.")
			return
		case <-poll.Done():
			if base.Err() == nil {
				if !started {
					b.setStatus(w, bubble, fmt.Sprintf("⌛️ Pi has not started on that in %s - it is queued behind other work. Send it again, or send /stop to clear the queue.", humanDuration(b.cfg.MaxWait*queuePatience)))
					b.log.Warn("queued turn never started", "turn", w.turnID, "waited", humanDuration(b.cfg.MaxWait*queuePatience))
				} else {
					b.setStatus(w, bubble, fmt.Sprintf("⌛️ Stopped watching after %s. Pi may still be running - send /status to check, or /stop to abort it.", humanDuration(b.cfg.MaxWait*queuePatience)))
					b.log.Warn("stopped watching after the patience ceiling", "turn", w.turnID, "after", humanDuration(time.Since(start)), "queued", humanDuration(w.queued))
				}
			}
			return
		case <-ticker.C:
		}

		turn, err := b.gw.Turn(poll, w.turnID)
		if errors.Is(err, gateway.ErrUnknownTurn) {
			b.setStatus(w, bubble, "⚠️ Pi has no record of that turn - the session probably restarted while it was running. Try again.")
			return
		}
		if err != nil {
			failures++
			b.log.Warn("poll failed", "turn", w.turnID, "err", err, "failures", failures)
			if failures == 5 {
				b.setStatus(w, bubble, "⚠️ Lost contact with Pi. It may still be working; check the terminal.")
			}
			if failures > 30 {
				return
			}
			continue
		}
		failures = 0

		// A queued prompt reads as a turn that exists but has not started. Separating that wait from the
		// run is the only way to tell "the model is slow" from "one busy session is serialising every
		// Chat message behind itself", which is what a 10 minute Chat answer actually is.
		if !started && turn.Status == "running" {
			started = true
			w.queued = time.Since(start)
			// The run has its own full budget from here. A message that queued behind two long turns would
			// otherwise pick up the ⌛️ notice before the agent had done anything, and its real answer would
			// arrive with nobody left to post it.
			deadline = time.Now().Add(b.cfg.MaxWait)
			b.log.Info("agent started the turn", "turn", w.turnID, "waited", humanDuration(w.queued))
		}

		if turn.Settled() {
			b.finish(w, bubble, turn, time.Since(start))
			return
		}

		if started && time.Now().After(deadline) {
			b.setStatus(w, bubble, fmt.Sprintf("⌛️ Still working, but I stopped watching after %s. Pi may still be running - send /status to check, or /stop to abort it.", b.cfg.MaxWait))
			b.log.Warn("turn outlived the watching deadline", "turn", w.turnID, "after", humanDuration(time.Since(start)), "queued", humanDuration(w.queued))
			return
		}
		if !started && !queuedNotice && time.Since(start) > b.cfg.MaxWait {
			// Better than a bubble that claims work which has not happened.
			queuedNotice = true
			b.setStatus(w, bubble, "⏳ Pi has not started on that yet - it is queued behind other work. Send /stop to clear the queue, or message again.")
			b.log.Info("turn still queued", "turn", w.turnID, "waited", humanDuration(time.Since(start)))
			continue
		}
		thinkingShown = b.showThinking(w, bubble, thinkingShown)
	}
}

// openStatus posts the eyes glyph and returns the message it lives in, or "" when status reporting is
// off or Chat refused. A refused create with our deterministic client- id is not necessarily a
// failure: it usually means a redelivery of the same event already put the bubble up, so we look that
// one up instead of posting a second one.
func (b *Bridge) openStatus(w *watch) string {
	if b.cfg.StatusMode == "none" || b.cfg.Eyes == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	posted, err := b.chat.Post(ctx, b.cfg.Eyes, chatapi.PostOpts{Space: w.space, Thread: w.thread, ClientID: w.statusID})
	if err == nil {
		b.log.Debug("status bubble posted", "turn", w.turnID, "name", posted.Name)
		return posted.Name
	}
	if existing, getErr := b.chat.Get(ctx, w.space+"/messages/"+w.statusID); getErr == nil {
		b.log.Info("status bubble already exists from an earlier delivery", "turn", w.turnID, "name", existing.Name)
		return existing.Name
	}
	b.log.Warn("status bubble post failed", "turn", w.turnID, "err", err)
	return ""
}

// showThinking moves the bubble to the thinking glyph, once, and reports whether it is there. Polls
// are cheap; editing a message that already says 🤔 is not, and Chat marks every edit, so the caller
// keeps the state instead of this function guessing from the bubble name.
func (b *Bridge) showThinking(w *watch, bubble string, already bool) bool {
	if bubble == "" || b.cfg.Thinking == "" || already {
		return already
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := b.chat.Update(ctx, bubble, b.cfg.Thinking); err != nil {
		b.log.Warn("status patch failed", "turn", w.turnID, "err", err)
		return false
	}
	b.log.Debug("status thinking", "turn", w.turnID)
	return true
}

// finish delivers the outcome of a settled turn. A failure stays visible as text in the bubble; a
// success retires the bubble so the thread is left holding only the answer.
func (b *Bridge) finish(w *watch, bubble string, turn gateway.Turn, elapsed time.Duration) {
	text := finalText(turn)

	// The turn that settled might not be ours: a session restart mid-queue, or a gateway that re-keyed
	// the run. Posting somebody else's answer as a reply to this user's message would be a lie, and
	// silence would be worse, so say the message was dropped.
	if turn.ID != w.promptID && !turn.HasInput(w.promptID) {
		b.setStatus(w, bubble, "⚠️ Pi finished a run without picking your message up. Send it again.")
		b.log.Warn("settled turn does not carry our prompt", "prompt", w.promptID, "turn", turn.ID, "status", turn.Status)
		return
	}

	// Several Chat messages can end up riding one gateway turn (a /steer, or a plain message that folded
	// into a run already going). The first watch to notice posts the answer; the others retire their own
	// glyph with a thumbs-up so the thread is not littered with copies of the same reply.
	if !b.claimTurn(turn.ID) {
		b.setStatus(w, bubble, "👍")
		b.log.Info("answer already posted by the turn this prompt joined", "prompt", w.promptID, "turn", turn.ID)
		return
	}

	switch turn.Status {
	case "aborted", "error":
		b.setStatus(w, bubble, text)
		b.log.Info("turn failed", "turn", w.turnID, "status", turn.Status, "err", firstNonEmptyStr(turn.Error, ""), "after", humanDuration(elapsed))
		return
	}

	if b.deliverAnswer(w, bubble, text) && b.cfg.StatusMode != "patch" && bubble != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := b.chat.Delete(ctx, bubble); err != nil {
			// Cosmetic failure only: the answer landed, the glyph bubble just stays behind.
			b.log.Warn("status bubble delete failed", "turn", w.turnID, "name", bubble, "err", err)
			return
		}
		b.log.Debug("status bubble retired", "turn", w.turnID, "name", bubble)
	}
	b.log.Info("replied", "turn", w.turnID, "status", turn.Status, "chars", len(text),
		"after", humanDuration(elapsed), "queued", humanDuration(w.queued), "worked", humanDuration(elapsed-w.queued))
}

// deliverAnswer writes the model's text into the thread. Chunking is forced by Chat's 32,000 character
// limit (measured in spike/FINDINGS.md), and every part gets its own deterministic client- id so a
// redelivery reuses the messages rather than forking duplicates.
func (b *Bridge) deliverAnswer(w *watch, bubble, text string) bool {
	chunks := Chunk(text, b.cfg.MaxChars)
	if len(chunks) == 0 {
		chunks = []string{"…done, with no text to show."}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i, chunk := range chunks {
		if i > 0 {
			chunk = fmt.Sprintf("*(continued %d/%d)*\n\n%s", i+1, len(chunks), chunk)
		}
		if i == 0 {
			// Remember what pi-chat said here: the room gate needs it to tell "that seems cold, what was
			// it overnight?" (aimed at the bot) from room chatter between people.
			b.noteRoomAnswer(w.space, text)
		}
		// In patch mode the bubble is the answer's first message, which saves a post and keeps one
		// continuous bubble from eyes to answer.
		if i == 0 && b.cfg.StatusMode == "patch" && bubble != "" {
			if err := b.chat.Update(ctx, bubble, chunk); err != nil {
				b.log.Error("answer patch failed", "turn", w.turnID, "err", err)
				return false
			}
			continue
		}
		posted, err := b.chat.Post(ctx, chunk, chatapi.PostOpts{Space: w.space, Thread: w.thread, ClientID: answerID(w.replyID, i, len(chunks))})
		if err != nil {
			b.log.Error("answer post failed", "turn", w.turnID, "part", i+1, "of", len(chunks), "err", err)
			return false
		}
		b.log.Debug("answer posted", "turn", w.turnID, "part", i+1, "of", len(chunks), "name", posted.Name, "chars", len(chunk))
	}
	return true
}

// Stop aborts whatever Pi is running. One Pi session per bridge, so this is deliberately global.
func (b *Bridge) Stop(ctx context.Context) error {
	return b.gw.Abort(ctx)
}

// Wait blocks until background watches finish; used on shutdown so in-flight turns still get to say
// something in the thread before the process exits.
func (b *Bridge) Wait() { b.watchers.Wait() }

// setStatus writes a failure or timeout notice, reusing the bubble when we have one.
func (b *Bridge) setStatus(w *watch, bubble, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if bubble != "" {
		if err := b.chat.Update(ctx, bubble, text); err == nil {
			return
		} else {
			b.log.Warn("status patch failed", "turn", w.turnID, "name", bubble, "err", err)
		}
	}
	if _, err := b.chat.Post(ctx, text, chatapi.PostOpts{Space: w.space, Thread: w.thread, ClientID: clientID("status-" + w.turnID)}); err != nil {
		b.log.Error("status post failed", "turn", w.turnID, "err", err)
	}
}

// answerID derives the client- id for chunk n of an answer. Single-part answers keep the plain reply
// id; overflow parts get their own so each stays idempotent on its own.
func answerID(replyID string, n, total int) string {
	if total == 1 {
		return replyID
	}
	return clientIDfmt("%s-part%d", replyID, n+1)
}

// clientIDfmt keeps a derived id inside Chat's rules (client- prefix, lowercase, <=63 chars).
func clientIDfmt(format string, args ...any) string {
	id := strings.ToLower(fmt.Sprintf(format, args...))
	id = strings.NewReplacer("_", "-", " ", "-", "/", "-", ":", "-").Replace(id)
	if !strings.HasPrefix(id, "client-") {
		id = "client-" + id
	}
	if len(id) > 63 {
		// Keep the prefix and the hash end: that is the part that makes it unique.
		id = "client-" + id[len(id)-55:]
	}
	return id
}

var controlMarkup = regexp.MustCompile(`(?i)<\s*/?\s*(tool_call|tool_use|function_calls|function|parameter|antml:\w+)\b[^>]*\/?>|<\s*function=|\|\|\|tool_call\|\|\|`)

// answerText guards the last mile: a model that emits tool-call scaffolding as plain text must not
// have it posted to a family Chat thread as the reply. The gateway filters this too; this is the
// belt for gateways that predate that fix.
func answerText(turn gateway.Turn) string {
	text := strings.TrimSpace(turn.Text)
	if text == "" {
		return ""
	}
	if !controlMarkup.MatchString(text) {
		return text
	}
	stripped := strings.TrimSpace(controlMarkup.ReplaceAllString(text, ""))
	// A real prose answer may be wrapped in a short tool block. Keep it when enough prose
	// remains; a markup-only block leaves no meaningful characters here.
	if proseWeight(stripped) >= 20 || proseWeight(stripped) >= proseWeight(text)/4 {
		return stripped
	}
	return ""
}

// proseWeight counts characters that are not markup punctuation, so an answer that merely quotes
// tool syntax is not mistaken for a leaked call.
func proseWeight(s string) int {
	count := 0
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '<', '>', '/', '=', '|', '"', '\'', '.', ',', '-', '(', ')':
		default:
			count++
		}
	}
	return count
}

func finalText(turn gateway.Turn) string {
	switch turn.Status {
	case "aborted":
		if text := answerText(turn); text != "" {
			return fmt.Sprintf("⏹ Stopped.\n\n%s", text)
		}
		return "⏹ Stopped."
	case "error":
		return fmt.Sprintf("❌ %s", firstNonEmptyStr(turn.Error, "Pi hit an error with no message"))
	}
	text := answerText(turn)
	if text == "" {
		if turn.AnswerRejected != "" {
			return "…done, but Pi only emitted tool-call scaffolding this time and left no prose to show you."
		}
		return "…done, but Pi produced no text this time."
	}
	return text
}

// ingestAttachments downloads images and returns them plus a text note about anything skipped, so
// the model is told a file exists even when we cannot show it.
func (b *Bridge) ingestAttachments(ctx context.Context, ev *chatmsg.Event) ([]gateway.Image, string) {
	var images []gateway.Image
	var skipped []string
	for _, att := range ev.Attachments {
		ref := att.ResourceRef
		if ref == "" {
			ref = att.Name
		}
		if ref == "" {
			continue
		}
		if len(images) >= b.cfg.MaxImages {
			skipped = append(skipped, fmt.Sprintf("(%d more attachments not forwarded)", len(ev.Attachments)-len(images)))
			break
		}
		data, ctype, err := b.chat.DownloadMedia(ctx, ref, b.cfg.ImageMaxBytes)
		if err != nil {
			b.log.Warn("attachment download failed", "ref", ref, "err", err)
			skipped = append(skipped, fmt.Sprintf("(couldn't read attachment %s: %v)", firstNonEmptyStr(att.ContentName, att.Name), err))
			continue
		}
		ctype = normalizeImageType(firstNonEmptyStr(att.ContentType, ctype))
		if ctype == "" {
			skipped = append(skipped, fmt.Sprintf("(attachment %s is %s, not an image - not forwarded)", firstNonEmptyStr(att.ContentName, att.Name), firstNonEmptyStr(att.ContentType, "unknown type")))
			continue
		}
		images = append(images, gateway.Image{Data: base64.StdEncoding.EncodeToString(data), MimeType: ctype})
	}
	return images, strings.Join(skipped, "\n")
}

// normalizeImageType returns a mimeType Pi can accept, or "" when the attachment is not an image.
func normalizeImageType(contentType string) string {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return ""
	}
	base, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		base = strings.SplitN(contentType, ";", 2)[0]
	}
	switch base {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
		return base
	default:
		return ""
	}
}

// reply posts a short direct answer (commands, refusals), used outside the turn watch.
func (b *Bridge) reply(ctx context.Context, ev *chatmsg.Event, text string) {
	if ev.Space == "" {
		return
	}
	if _, err := b.chat.Post(ctx, text, chatapi.PostOpts{Space: ev.Space, Thread: ev.Thread, ClientID: clientID("ack-" + ev.MessageName)}); err != nil {
		b.log.Error("reply failed", "space", ev.Space, "err", err)
	}
}

// clientID turns an arbitrary key into a Chat custom message id: lowercase alphanumerics and
// hyphens, <=63 chars, starting with "client-", and unique per space (we hash the key so length
// limits cannot collide two different replies).
func clientID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "client-pichat-" + hex.EncodeToString(sum[:])[:40]
}

// shortID is the turn id: deterministic from the Chat message so a redelivery maps to the same turn.
func shortID(messageName string) string {
	if messageName == "" {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	sum := sha256.Sum256([]byte(messageName))
	return "c" + hex.EncodeToString(sum[:])[:15]
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clip(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return strings.TrimRight(string(runes[:max]), " \n") + "\n…"
}

func truncate(text string, max int) string {
	text = strings.ReplaceAll(text, "\n", " ")
	return clip(text, max)
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh%02dm", h, m)
	}
}
