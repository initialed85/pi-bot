package bridge

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initialed85/pi-bot/internal/chatid"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// outboundGateway is the gateway fake with an outbox bolted on. It embeds the shared fake rather than
// growing one in bridge_test.go, so every existing test keeps a gateway that has no outbound surface -
// which is also what an older Pi pod looks like, and what the nil-assertion path is for.
type outboundGateway struct {
	*fakeGateway

	mu    sync.Mutex
	items []gateway.Outbound
	acked []outboundAck
	reads int
}

type outboundAck struct {
	id          string
	ok          bool
	messageName string
	error       string
	permanent   bool
}

func (g *outboundGateway) Outbox(_ context.Context) ([]gateway.Outbound, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads++
	out := make([]gateway.Outbound, len(g.items))
	copy(out, g.items)
	return out, nil
}

func (g *outboundGateway) AckOutbox(_ context.Context, id string, ack gateway.OutboxAck) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.acked = append(g.acked, outboundAck{id: id, ok: ack.OK, messageName: ack.Message, error: ack.Failure, permanent: ack.Permanent})
	if ack.OK || ack.Permanent {
		for i, item := range g.items {
			if item.ID == id {
				g.items = append(g.items[:i], g.items[i+1:]...)
				break
			}
		}
	}
	return nil
}

func (g *outboundGateway) queue(items ...gateway.Outbound) {
	g.mu.Lock()
	g.items = append(g.items, items...)
	g.mu.Unlock()
}

func (g *outboundGateway) acknowledgements() []outboundAck {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]outboundAck, len(g.acked))
	copy(out, g.acked)
	return out
}

func (g *outboundGateway) remaining() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.items)
}

// outboundBridge wires a bridge that has been granted one space, which is the only configuration in
// which any of this code runs at all.
func outboundBridge(t *testing.T, spaces ...string) (*Bridge, *fakeChat, *outboundGateway) {
	t.Helper()
	b, chat, gw := newTestBridge(t)
	ob := &outboundGateway{fakeGateway: gw}
	b.gw = ob
	b.cfg.OutboundSpaces = spaces
	b.outboundRate = &outboundWindow{}
	return b, chat, ob
}

func TestOutboundPostsIntoAnAllowedSpace(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	gw.queue(gateway.Outbound{ID: "ob-1", Space: "spaces/GRANTED", Text: "The rollout finished; your answer is in the thread.", Reason: "resume"})

	if posted := b.drainOutbox(context.Background()); posted != 1 {
		t.Fatalf("posted = %d, want 1", posted)
	}
	text, opts, ok := chat.lastPost()
	if !ok {
		t.Fatalf("nothing was posted")
	}
	if opts.Space != "spaces/GRANTED" {
		t.Errorf("space = %q", opts.Space)
	}
	if opts.Thread != "" {
		t.Errorf("thread = %q, want empty so it lands at the space root", opts.Thread)
	}
	if opts.ClientID != clientID("outbound-ob-1") {
		t.Errorf("client id = %q, want the deterministic one for this outbox entry", opts.ClientID)
	}
	refs := chatid.Refs(text)
	if len(refs) != 1 {
		t.Fatalf("posted notice carries no turn marker: %q", text)
	}
	if !strings.HasPrefix(text, chatid.Token(refs[0])) {
		t.Errorf("marker is not at the start: %q", text)
	}
	if !strings.Contains(text, "rollout finished") {
		t.Errorf("text lost the message: %q", text)
	}
	acks := gw.acknowledgements()
	if len(acks) != 1 || !acks[0].ok || acks[0].messageName == "" {
		t.Errorf("acks = %+v, want one success carrying the Chat message name", acks)
	}
	if gw.remaining() != 0 {
		t.Errorf("outbox still holds %d entries after a delivery", gw.remaining())
	}
}

func TestOutboundHonoursAThreadTarget(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	gw.queue(gateway.Outbound{ID: "ob-2", Space: "spaces/GRANTED", Thread: "spaces/GRANTED/threads/T1", Text: "Meeting moved to 4."})

	b.drainOutbox(context.Background())
	_, opts, ok := chat.lastPost()
	if !ok {
		t.Fatalf("nothing was posted")
	}
	if opts.Thread != "spaces/GRANTED/threads/T1" {
		t.Errorf("thread = %q, want the one the caller named", opts.Thread)
	}
}

func TestOutboundRefusesASpaceNobodyGranted(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	gw.queue(gateway.Outbound{ID: "ob-3", Space: "spaces/SOMEBODYS_ROOM", Text: "Hello from an ungranted room."})

	if posted := b.drainOutbox(context.Background()); posted != 0 {
		t.Fatalf("posted = %d, want 0", posted)
	}
	if chat.postCount() != 0 {
		t.Errorf("the bridge posted %d messages into a space it was never granted", chat.postCount())
	}
	acks := gw.acknowledgements()
	if len(acks) != 1 || acks[0].ok {
		t.Fatalf("acks = %+v, want one failure", acks)
	}
	if !strings.Contains(acks[0].error, "PI_CHAT_OUTBOUND_SPACES") {
		t.Errorf("refusal explains nothing about the gate: %q", acks[0].error)
	}
	// The refusal is permanent, so the entry must not be re-queued forever by a retry-happy drain.
	b.drainOutbox(context.Background())
	if len(gw.acknowledgements()) != 1 {
		t.Errorf("a refused post was acked %d times; it should be settled on the first refusal", len(gw.acknowledgements()))
	}
}

func TestOutboundCapsTheHour(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	b.cfg.OutboundPerHour = 2
	gw.queue(
		gateway.Outbound{ID: "ob-a", Space: "spaces/GRANTED", Text: "first"},
		gateway.Outbound{ID: "ob-b", Space: "spaces/GRANTED", Text: "second"},
		gateway.Outbound{ID: "ob-c", Space: "spaces/GRANTED", Text: "third"},
	)

	if posted := b.drainOutbox(context.Background()); posted != 2 {
		t.Fatalf("posted = %d, want 2", posted)
	}
	if chat.postCount() != 2 {
		t.Errorf("posts = %d, want 2", chat.postCount())
	}
	if gw.remaining() != 1 {
		t.Errorf("the over-cap post left the outbox; it should still be waiting, got %d remaining", gw.remaining())
	}
	// The cap is a rate, not a verdict: it lifts as the window slides.
	b.outboundRate.at[0] = time.Now().Add(-2 * time.Hour)
	if posted := b.drainOutbox(context.Background()); posted != 1 {
		t.Fatalf("after the window slid, posted = %d, want 1", posted)
	}
}

func TestOutboundRefusesTextTooLong(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	b.cfg.OutboundMaxChars = 50
	gw.queue(gateway.Outbound{ID: "ob-long", Space: "spaces/GRANTED", Text: strings.Repeat("a", 51)})

	b.drainOutbox(context.Background())
	if chat.postCount() != 0 {
		t.Errorf("a post over the character cap reached Chat")
	}
	acks := gw.acknowledgements()
	if len(acks) != 1 || acks[0].ok || !strings.Contains(acks[0].error, "over the 50 limit") {
		t.Errorf("acks = %+v, want one failure naming the limit", acks)
	}
}

// TestOutboundRepostsReuseTheClientID covers the crash that matters here: Chat takes the message, the
// ack does not reach the gateway, the claim lapses, and the bridge tries again. The deterministic client
// id is what stops that from becoming two notices in a room.
func TestOutboundRepostsReuseTheClientID(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	gw.queue(gateway.Outbound{ID: "ob-race", Space: "spaces/GRANTED", Text: "posted once, acked twice"})

	b.drainOutbox(context.Background())
	_, first, _ := chat.lastPost()
	// Simulate the lost ack: the entry is still in the gateway's outbox, so the bridge tries again.
	gw.queue(gateway.Outbound{ID: "ob-race", Space: "spaces/GRANTED", Text: "posted once, acked twice"})
	if posted := b.drainOutbox(context.Background()); posted != 1 {
		t.Fatalf("the retry reported %d posts; a duplicate client id means Chat already has it, which is a delivery", posted)
	}
	if n := chat.postCount(); n != 1 {
		t.Fatalf("posts = %d, want 1 - Chat took the second attempt as a duplicate and the bridge must not have tried a third text", n)
	}
	if _, opts, _ := chat.lastPost(); opts.ClientID != first.ClientID {
		t.Errorf("the retry used a different client id (%q vs %q); that is how a room gets two notices", opts.ClientID, first.ClientID)
	}
	acks := gw.acknowledgements()
	if len(acks) < 2 || !acks[len(acks)-1].ok {
		t.Errorf("acks = %+v, want the retry acked as delivered", acks)
	}
}

func TestOutboundMarkerResolvesWhenSomebodyQuotesIt(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	gw.queue(gateway.Outbound{ID: "ob-quote", Space: "spaces/GRANTED", Text: "Deployment done, tested the migration twice."})
	b.drainOutbox(context.Background())

	text, _, ok := chat.lastPost()
	if !ok {
		t.Fatalf("nothing was posted")
	}
	refs := chatid.Refs(text)
	if len(refs) != 1 {
		t.Fatalf("no marker on the notice: %q", text)
	}
	got := b.journal.recall(refs[0])
	if got == nil {
		t.Fatalf("quoting the notice resolves to nothing; the ring never learned about it")
	}
	if got.Space != "spaces/GRANTED" {
		t.Errorf("recall returned space %q", got.Space)
	}
	if !strings.Contains(got.Asked, "Deployment done") {
		t.Errorf("recall note = %q, want an echo of our own post", got.Asked)
	}
	// Nothing was owed, so the journal must not treat this as a turn to recover after a restart.
	if len(b.journal.pending()) != 0 {
		t.Errorf("an outbound post became an owed turn: %+v", b.journal.pending())
	}
}

func TestOutboundStaysSilentWhenNothingIsGranted(t *testing.T) {
	// The default configuration: no spaces named. The drain must not even read the outbox, so a bridge
	// deployed without the grant is provably incapable of initiating a post.
	b, chat, gw := newTestBridge(t)
	b.gw = &outboundGateway{fakeGateway: gw}
	b.cfg.OutboundSpaces = nil
	if posted := b.drainOutbox(context.Background()); posted != 0 {
		t.Fatalf("posted = %d", posted)
	}
	if chat.postCount() != 0 {
		t.Errorf("posts = %d with no allowlist", chat.postCount())
	}
}

func TestOutboundIgnoresAGatewayWithoutAnOutbox(t *testing.T) {
	// An older Pi build has no /v1/outbox. The bridge must notice once by type assertion and move on,
	// not log an error every 1.5s for the life of the pod.
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	b.gw = gw.fakeGateway
	if posted := b.drainOutbox(context.Background()); posted != 0 {
		t.Fatalf("posted = %d", posted)
	}
	if chat.postCount() != 0 {
		t.Errorf("posts = %d", chat.postCount())
	}
}

func TestOutboundDoesNotPostEmptyText(t *testing.T) {
	b, chat, gw := outboundBridge(t, "spaces/GRANTED")
	gw.queue(gateway.Outbound{ID: "ob-empty", Space: "spaces/GRANTED", Text: "   "})
	b.drainOutbox(context.Background())
	if chat.postCount() != 0 {
		t.Errorf("a blank message was posted to Chat")
	}
	acks := gw.acknowledgements()
	if len(acks) != 1 || acks[0].ok {
		t.Errorf("acks = %+v, want one failure for the blank post", acks)
	}
}

func TestOutboundAllowedMatchesExactNamesAndTheStar(t *testing.T) {
	cases := []struct {
		granted []string
		space   string
		want    bool
	}{
		{[]string{"spaces/AAA"}, "spaces/AAA", true},
		{[]string{" spaces/AAA "}, "spaces/AAA", true},
		{[]string{"spaces/AAA"}, "spaces/BBB", false},
		{[]string{"spaces/AAA"}, "spaces/AAA/threads/T", false},
		{[]string{"*"}, "spaces/ANYTHING", true},
		{nil, "spaces/AAA", false},
		{[]string{"spaces/AAA"}, "", false},
	}
	for _, c := range cases {
		if got := outboundAllowed(c.granted, c.space); got != c.want {
			t.Errorf("outboundAllowed(%v, %q) = %v, want %v", c.granted, c.space, got, c.want)
		}
	}
}

// TestOutboundDefaultsWithEveryConfigField keeps the outbound knobs honest: withDefaults starts from
// DefaultConfig and rebuilds field by field, and a field left out of that list is silently zero in
// every real bridge while the tests still pass.
func TestOutboundConfigDefaults(t *testing.T) {
	d := Config{OutboundSpaces: []string{"spaces/AAA"}}.withDefaults()
	if len(d.OutboundSpaces) != 1 {
		t.Fatalf("OutboundSpaces dropped by withDefaults")
	}
	if d.OutboundPerHour != DefaultConfig().OutboundPerHour {
		t.Errorf("OutboundPerHour = %d", d.OutboundPerHour)
	}
	if d.OutboundMaxChars != DefaultConfig().OutboundMaxChars {
		t.Errorf("OutboundMaxChars = %d", d.OutboundMaxChars)
	}
	if d.OutboundInterval != DefaultConfig().OutboundInterval {
		t.Errorf("OutboundInterval = %v", d.OutboundInterval)
	}
	if got := (Config{OutboundPerHour: -1}).withDefaults().OutboundPerHour; got != -1 {
		t.Errorf("OutboundPerHour = %d, want an explicit -1 preserved (that is how an operator removes the cap)", got)
	}
}

func TestOutboundJournalRememberDoesNotWriteTheFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/turns.json"
	b := New(&fakeChat{downloads: map[string][]byte{}}, &fakeGateway{turns: map[string]gateway.Turn{}},
		fastConfig(), slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	b.journal = openJournal(path, b.log)
	b.journal.remember(&pendingTurn{PromptID: "abcd1234", TurnID: "abcd1234", Start: time.Now(), Expires: time.Now().Add(time.Hour)})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the recall ring wrote %s; it is in-memory by design so a post nobody owed costs no disk", path)
	}
	if b.journal.recall("abcd1234") == nil {
		t.Errorf("remember() did not make the turn recallable")
	}
}
