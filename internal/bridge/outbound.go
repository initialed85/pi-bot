package bridge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/initialed85/pi-bot/internal/chatapi"
	"github.com/initialed85/pi-bot/internal/chatid"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// Outbound is pi-chat speaking first: a post the agent asked for that did not arrive as an answer to a
// Chat message. The three reasons it exists are the three ways this process can wake up with something
// to say - a schedule firing, an auto-resume after a pod restart that interrupted somebody's work, and a
// peer drawing the session into action through the team room.
//
// It is opt-in by the space, not by a flag: `PI_CHAT_OUTBOUND_SPACES` empty means the loop never starts
// and every request is refused. An agent that can post into a room unprompted needs the same explicit
// grant the reply gate took four days of evidence to earn.
type outboundWindow struct {
	mu sync.Mutex
	at []time.Time
}

// allow admits one post if fewer than max were admitted inside the last hour. The point is not throughput
// - Chat is not the bottleneck - it is that a confused agent should not be able to narrate itself into a
// room at a hundred messages an hour.
func (w *outboundWindow) allow(max int, now time.Time) bool {
	if w == nil || max <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.at[:0]
	for _, t := range w.at {
		if now.Sub(t) < time.Hour {
			kept = append(kept, t)
		}
	}
	w.at = kept
	if len(w.at) >= max {
		return false
	}
	w.at = append(w.at, now)
	return true
}

// used tells a refusal how full the hour already is, so the message says something checkable.
func (w *outboundWindow) used() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.at)
}

// outboundAllowed reports whether an agent-initiated post may land in space. Entries are matched as
// space names (`spaces/AAA…`); "*" is the only wildcard and it means every space the bot has been added
// to, which is a choice nobody should make by accident - so it must be spelled.
func outboundAllowed(spaces []string, space string) bool {
	space = strings.TrimSpace(space)
	if space == "" {
		return false
	}
	for _, allowed := range spaces {
		allowed = strings.TrimSpace(allowed)
		if allowed == "*" || allowed == space {
			return true
		}
	}
	return false
}

// outboxClient is the gateway's outbound surface, asserted rather than added to Gateway: the real client
// has it, and every test fake can keep compiling instead of growing methods it will never use. A gateway
// build without it simply has no outbox, which is the honest reading of an older Pi pod.
type outboxClient interface {
	Outbox(ctx context.Context) ([]gateway.Outbound, error)
	AckOutbox(ctx context.Context, id string, ack gateway.OutboxAck) error
}

func (b *Bridge) outbox() outboxClient {
	if c, ok := b.gw.(outboxClient); ok {
		return c
	}
	return nil
}

// startOutbound runs the drain loop on the same context as the watches, so a rollout that is finishing
// owed answers also gets one last look at the outbox rather than dropping a queued post on the floor.
func (b *Bridge) startOutbound(ctx context.Context) {
	interval := b.cfg.OutboundInterval
	if interval <= 0 {
		interval = DefaultConfig().OutboundInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.drainOutbox(ctx)
			}
		}
	}()
	b.log.Info("outbound posting enabled", "spaces", len(b.cfg.OutboundSpaces), "every", interval, "per_hour", b.cfg.OutboundPerHour)
}

// drainOutbox posts everything the gateway has queued and returns how many landed. It is called by the
// loop and directly by tests. A gateway that is mid-restart answers with an error; that is a normal
// state for a poll and gets one warning, not a stack trace per second.
func (b *Bridge) drainOutbox(ctx context.Context) int {
	if len(b.cfg.OutboundSpaces) == 0 {
		return 0
	}
	client := b.outbox()
	if client == nil {
		return 0
	}
	items, err := client.Outbox(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("outbox read failed", "err", err)
		}
		return 0
	}
	posted := 0
	for _, item := range items {
		if b.postOutbound(ctx, client, item) {
			posted++
		}
	}
	return posted
}

// postOutbound is one attempt at one message: gate it, mark it, post it, tell the gateway the result.
// The boolean is "did Chat take it", not "was it asked for", so a caller can count real deliveries.
func (b *Bridge) postOutbound(ctx context.Context, client outboxClient, o gateway.Outbound) bool {
	text := strings.TrimSpace(o.Text)
	if text == "" {
		b.failOutbound(ctx, client, o, "nothing to post", true)
		return false
	}
	if !outboundAllowed(b.cfg.OutboundSpaces, o.Space) {
		// Permanent: retrying an allowlist refusal just burns the attempt cap.
		b.failOutbound(ctx, client, o, fmt.Sprintf("space %s is not in PI_CHAT_OUTBOUND_SPACES", o.Space), true)
		return false
	}
	limit := b.cfg.OutboundMaxChars
	if limit <= 0 {
		limit = DefaultConfig().OutboundMaxChars
	}
	if len([]rune(text)) > limit {
		b.failOutbound(ctx, client, o, fmt.Sprintf("text is %d characters, over the %d limit", len([]rune(text)), limit), true)
		return false
	}
	if !b.outboundRate.allow(b.cfg.OutboundPerHour, time.Now()) {
		// Transient on purpose: the next tick is inside the hour and the post is still wanted then.
		b.log.Warn("outbound rate limited", "id", o.ID, "space", o.Space, "this_hour", b.outboundRate.used(), "per_hour", b.cfg.OutboundPerHour)
		return false
	}

	// The marker is what makes this message quotable: a person replying to it lands on the turn id, and
	// the bridge can say which question the notice was about instead of guessing from an empty thread.
	turnID := shortID("outbound-" + o.ID)
	start := time.Now()
	posted, err := b.chat.Post(ctx, chatid.Mark(turnID, text), chatapi.PostOpts{
		Space: o.Space,
		// An empty thread starts a new one at the space root, which is Ed's stated default: "assume root
		// of this chat if unset".
		Thread:   o.Thread,
		ClientID: clientID("outbound-" + o.ID),
	})
	elapsed := time.Since(start)
	if err != nil && chatapi.IsDuplicate(err) {
		// Chat already has this message: the post landed and only our ack was lost, which is exactly what
		// a claim lapse after a bridge restart looks like. Calling that a failure would either re-post it
		// (the client id refuses) or tell the agent it never arrived when a person can see it in the room.
		b.log.Info("outbound post was already in Chat", "id", o.ID, "space", o.Space, "turn", chatid.Token(turnID), "latency_ms", elapsed.Milliseconds())
		posted, err = &chatapi.Message{}, nil
	}
	if err != nil {
		b.log.Warn("outbound post failed", "id", o.ID, "space", o.Space, "thread", o.Thread != "", "err", err)
		b.failOutbound(ctx, client, o, err.Error(), false)
		return false
	}
	// Nothing was owed, but the turn is filed anyway: quoting this message back should resolve, not
	// report an unknown id. Only our own words go in the ring, never a participant's.
	b.journal.remember(&pendingTurn{
		PromptID: turnID,
		TurnID:   turnID,
		Space:    o.Space,
		Thread:   o.Thread,
		User:     "pi-chat",
		Asked:    truncateForJournal(text),
		Start:    start,
		Expires:  start.Add(outboundRecallTTL),
	})
	b.log.Info("outbound posted", "id", o.ID, "turn", chatid.Token(turnID), "space", o.Space, "thread", o.Thread != "",
		"reason", o.Reason, "chars", len([]rune(text)), "latency_ms", elapsed.Milliseconds())
	if err := client.AckOutbox(ctx, o.ID, gateway.OutboxAck{OK: true, Message: posted.Name}); err != nil && ctx.Err() == nil {
		// Chat has the message and the gateway does not know it yet. The claim lapses, the bridge posts
		// again with the same client id, and Chat refuses the duplicate - which is why the id is
		// deterministic. Say so, because this line looks like a bug in a log review.
		b.log.Warn("outbound ack failed (Chat already has the message)", "id", o.ID, "err", err)
	}
	return true
}

// failOutbound reports an attempt that did not put the message in Chat. `permanent` is the bridge's
// judgement that no retry could change the outcome - a space nobody granted, text over the cap - in which
// case the gateway settles the entry instead of burning five attempts on it.
func (b *Bridge) failOutbound(ctx context.Context, client outboxClient, o gateway.Outbound, reason string, permanent bool) {
	if client == nil {
		return
	}
	if err := client.AckOutbox(ctx, o.ID, gateway.OutboxAck{OK: false, Failure: reason, Permanent: permanent}); err != nil && ctx.Err() == nil {
		b.log.Warn("outbox failure ack failed", "id", o.ID, "err", err)
	}
}

// outboundRecallTTL is how long a posted outbound notice stays resolvable in the journal ring. It is
// in-memory either way; the bound exists so a recovery log never tries to resurrect a notice from days
// ago as though it were current.
const outboundRecallTTL = 24 * time.Hour

func truncateForJournal(text string) string {
	runes := []rune(strings.ReplaceAll(text, "\n", " "))
	if len(runes) <= 80 {
		return string(runes)
	}
	return string(runes[:77]) + "…"
}
