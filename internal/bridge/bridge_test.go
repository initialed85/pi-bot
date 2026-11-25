package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/initialed85/pi-bot/internal/chatapi"
	"github.com/initialed85/pi-bot/internal/chatgate"
	"github.com/initialed85/pi-bot/internal/chatmsg"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// ---------------------------------------------------------------- test doubles

type posted struct {
	name string
	text string
	opts chatapi.PostOpts
}

type fakeChat struct {
	mu           sync.Mutex
	posts        []posted
	patches      map[string]string
	deletes      []string
	downloads    map[string][]byte
	ownedThreads map[string]bool
	failPost     error
	nextName     int
}

func (f *fakeChat) Post(_ context.Context, text string, opts chatapi.PostOpts) (*chatapi.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPost != nil {
		return nil, f.failPost
	}
	f.nextName++
	name := fmt.Sprintf("%s/messages/fake-%d", opts.Space, f.nextName)
	if opts.ClientID != "" {
		for _, p := range f.posts {
			if p.opts.ClientID == opts.ClientID && p.opts.Space == opts.Space {
				// Chat refuses a duplicate custom id; mirror the 409 so tests can rely on it.
				return nil, fmt.Errorf("chatapi: duplicate client id %s", opts.ClientID)
			}
		}
	}
	f.posts = append(f.posts, posted{name: name, text: text, opts: opts})
	return &chatapi.Message{Name: name, Text: text}, nil
}

func (f *fakeChat) Update(_ context.Context, name, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.patches == nil {
		f.patches = map[string]string{}
	}
	f.patches[name] = text
	return nil
}

// Get mirrors Chat's behaviour for a `client-` id: the message keeps a server-assigned name, and the
// custom id only works as a lookup handle. The bridge depends on that to find a status bubble another
// delivery of the same event already posted.
func (f *fakeChat) Get(_ context.Context, name string) (*chatapi.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(name, "client-") || strings.Contains(name, "/messages/client-") {
		id := name[strings.LastIndex(name, "/")+1:]
		for _, post := range f.posts {
			if post.opts.ClientID == id {
				return &chatapi.Message{Name: post.name, Text: post.text}, nil
			}
		}
		return nil, fmt.Errorf("chatapi: get %s: not found", name)
	}
	if text, ok := f.patches[name]; ok {
		return &chatapi.Message{Name: name, Text: text}, nil
	}
	for _, post := range f.posts {
		if post.name == name {
			return &chatapi.Message{Name: name, Text: post.text}, nil
		}
	}
	return nil, fmt.Errorf("chatapi: get %s: not found", name)
}

func (f *fakeChat) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, name)
	return nil
}

// GetSpace answers the routing question the room listener cannot answer from its own payload. The
// default here is a group room, because that is the least surprising space for an app fake: a test
// that wants a 1:1 says so by seeding ownedThreads-style data into spaces.
func (f *fakeChat) GetSpace(_ context.Context, name string) (*chatapi.SpaceInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &chatapi.SpaceInfo{Name: name, SpaceType: "ROOM"}, nil
}

// nameOf finds the first posted message with exactly this text, for asserting on bubbles.
func (f *fakeChat) nameOf(text string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, post := range f.posts {
		if post.text == text {
			return post.name
		}
	}
	return ""
}

func (f *fakeChat) deletedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deletes...)
}

// lastPatch returns the most recent patch written to the given message name, plus whether any patch
// exists at all.
func (f *fakeChat) patchOf(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.patches[name]
	return v, ok
}

func (f *fakeChat) DownloadMedia(_ context.Context, ref string, _ int64) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.downloads[ref]
	if !ok {
		return nil, "", fmt.Errorf("no such attachment %q", ref)
	}
	return data, "image/png", nil
}

func (f *fakeChat) HasBotMessage(_ context.Context, _, thread string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ownedThreads[thread], nil
}

func (f *fakeChat) lastPost() (string, chatapi.PostOpts, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) == 0 {
		return "", chatapi.PostOpts{}, false
	}
	p := f.posts[len(f.posts)-1]
	return p.text, p.opts, true
}

func (f *fakeChat) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *fakeChat) allPosts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.posts))
	for _, p := range f.posts {
		out = append(out, p.text)
	}
	return out
}

type fakeGateway struct {
	mu        sync.Mutex
	prompts   []gateway.PromptRequest
	commands  []string
	aborts    int
	turns     map[string]gateway.Turn
	state     gateway.State
	commandOK bool
	// foldInto makes every prompt join one shared turn, which is what a steer does to a live run: the
	// bridge is handed a turn id that is not its own prompt id.
	foldInto string
}

func (g *fakeGateway) Prompt(_ context.Context, req gateway.PromptRequest) (gateway.PromptResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prompts = append(g.prompts, req)
	if g.foldInto != "" {
		turn := g.turns[g.foldInto]
		turn.ID = g.foldInto
		if turn.Status == "" {
			turn.Status = "running"
		}
		turn.Inputs = append(turn.Inputs, gateway.Input{ID: req.ID, Text: req.Text})
		g.turns[g.foldInto] = turn
		return gateway.PromptResponse{TurnID: g.foldInto, Folded: true, ID: req.ID}, nil
	}
	if _, ok := g.turns[req.ID]; !ok {
		g.turns[req.ID] = gateway.Turn{ID: req.ID, Status: "running", Inputs: []gateway.Input{{ID: req.ID, Text: req.Text}}}
	}
	return gateway.PromptResponse{TurnID: req.ID, ID: req.ID}, nil
}

func (g *fakeGateway) Turn(_ context.Context, id string) (gateway.Turn, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	turn, ok := g.turns[id]
	if !ok {
		return gateway.Turn{}, gateway.ErrUnknownTurn
	}
	return turn, nil
}

func (g *fakeGateway) Abort(_ context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.aborts++
	return nil
}

func (g *fakeGateway) Command(_ context.Context, name, args string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.commandOK && name != "chat-gateway" {
		return fmt.Errorf("unknown command %q", name)
	}
	g.commands = append(g.commands, "/"+name+" "+args)
	return nil
}

func (g *fakeGateway) State(_ context.Context) (gateway.State, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state, nil
}

func (g *fakeGateway) Commands(_ context.Context) ([]gateway.Command, error) {
	return []gateway.Command{{Name: "chat-gateway", Description: "gateway status"}}, nil
}

func (g *fakeGateway) settle(id, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	turn := g.turns[id]
	turn.Status = "settled"
	turn.Text = text
	turn.Last = text
	turn.Turns = 3
	turn.Model = "rooter/test-model"
	g.turns[id] = turn
}

func (g *fakeGateway) setTurn(id string, turn gateway.Turn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.turns[id] = turn
}

func (g *fakeGateway) promptCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.prompts)
}

func (g *fakeGateway) lastPrompt() gateway.PromptRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prompts[len(g.prompts)-1]
}

// ---------------------------------------------------------------- harness

// fastConfig is the harness default. It runs bubble mode on purpose - that is the mode with the extra
// moving part (post the answer, then retire the status message), so it gets the coverage - while
// TestHandle_PatchModeReusesOneMessage covers the shipped default, where one message is rewritten in
// place. Chunking behaviour is the same in both.
func fastConfig() Config {
	return Config{
		Allowlist:             []string{"owner@example.com"},
		RequireMentionInRooms: true,
		PrefixWithSender:      true,
		StatusMode:            "bubble",
		PollInterval:          10 * time.Millisecond,
		MaxWait:               2 * time.Second,
		MaxChars:              4000,
		ImageMaxBytes:         1 << 20,
		MaxImages:             4,
	}
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	// Reuse the parser fixtures; they live one package over.
	b, err := os.ReadFile(filepath.Join("..", "chatmsg", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// settleFixture rewrites a fixture's message name so a test can drive N distinct prompts.
func withText(t *testing.T, raw []byte, text string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	chatObj := doc["chat"].(map[string]any)
	payload := chatObj["messagePayload"].(map[string]any)
	msg := payload["message"].(map[string]any)
	msg["text"] = text
	msg["argumentText"] = text
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

// withMessageName rewrites a fixture's message (and thread) name so one fixture can produce several
// distinct prompts without the bridge's message-name dedupe swallowing them.
func withMessageName(t *testing.T, raw []byte, name string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg := doc["chat"].(map[string]any)["messagePayload"].(map[string]any)["message"].(map[string]any)
	msg["name"] = name
	if thread, ok := msg["thread"].(map[string]any); ok {
		space := strings.Split(name, "/messages/")[0]
		thread["name"] = space + "/threads/" + strings.Split(name, "/messages/")[1]
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}

func countPostsContaining(posts []string, needle string) int {
	n := 0
	for _, text := range posts {
		if strings.Contains(text, needle) {
			n++
		}
	}
	return n
}

func newTestBridge(t *testing.T) (*Bridge, *fakeChat, *fakeGateway) {
	t.Helper()
	chat := &fakeChat{downloads: map[string][]byte{}}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model", Commands: 21}}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(chat, gw, fastConfig(), log), chat, gw
}

// ---------------------------------------------------------------- tests

func TestHandle_DMTextPrompt(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "what is 12*13?")
	if err := b.Handle(context.Background(), "psm-1", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if gw.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1", gw.promptCount())
	}
	prompt := gw.lastPrompt()
	if prompt.Text != "Owner Person <owner@example.com>: what is 12*13?" {
		t.Errorf("prompt text = %q, want sender-prefixed body", prompt.Text)
	}
	if prompt.Space != "spaces/m4d90aAAAAE" {
		t.Errorf("space = %q", prompt.Space)
	}
	gw.settle(prompt.ID, "156")
	b.Wait()
	text, opts, ok := chat.lastPost()
	if !ok {
		t.Fatalf("no chat post after settle")
	}
	if text != "156" {
		t.Errorf("final text = %q, want the answer and nothing else", text)
	}
	if opts.Space != "spaces/m4d90aAAAAE" {
		t.Errorf("reply space = %q", opts.Space)
	}
	if opts.Thread == "" {
		t.Errorf("reply must stay in the source thread, got %q", opts.Thread)
	}
	if !b.cfg.PrefixWithSender {
		t.Errorf("config lost PrefixWithSender through withDefaults")
	}
}

// Eyes go up the moment the bridge takes the message, so even a one-word answer produces two posts -
// the point is that the status bubble does not survive it.
func TestHandle_FastAnswerRetiresTheStatusBubble(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	b.cfg.PollInterval = 5 * time.Millisecond
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "hi")
	if err := b.Handle(context.Background(), "id-fast", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	gw.settle(gw.lastPrompt().ID, "hello")
	b.Wait()

	posts := chat.allPosts()
	if len(posts) != 2 {
		t.Fatalf("posts = %d, want [eyes, answer]: %#v", len(posts), posts)
	}
	if posts[0] != b.cfg.Eyes {
		t.Errorf("first post = %q, want the eyes glyph %q", posts[0], b.cfg.Eyes)
	}
	if posts[1] != "hello" {
		t.Errorf("answer post = %q, want exactly the model text", posts[1])
	}
	bubble := chat.nameOf(b.cfg.Eyes)
	if bubble == "" {
		t.Fatalf("no status bubble to retire")
	}
	if !hasName(chat.deletedNames(), bubble) {
		t.Errorf("status bubble %s was left in the thread; deletes = %v", bubble, chat.deletedNames())
	}
}

// While Pi works the bubble says 🤔 and nothing else: no partial answers, no step counts, no timings,
// no model names. Those live in the bridge log and the terminal.
func TestHandle_SlowAnswerShowsThinkingGlyph(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "long task")
	if err := b.Handle(context.Background(), "id-slow", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := gw.lastPrompt().ID
	time.Sleep(60 * time.Millisecond)
	gw.setTurn(id, gateway.Turn{ID: id, Status: "running", Turns: 5, Streaming: "counting numbers", Progress: "bash, write"})
	time.Sleep(60 * time.Millisecond)
	gw.settle(id, "done counting")
	b.Wait()

	bubble := chat.nameOf(b.cfg.Eyes)
	if bubble == "" {
		t.Fatalf("no eyes bubble was posted: %#v", chat.allPosts())
	}
	if got, ok := chat.patchOf(bubble); !ok || got != b.cfg.Thinking {
		t.Errorf("status bubble = %q, want the thinking glyph %q", got, b.cfg.Thinking)
	}
	text, _, _ := chat.lastPost()
	if text != "done counting" {
		t.Errorf("answer post = %q", text)
	}
	if !hasName(chat.deletedNames(), bubble) {
		t.Errorf("thinking bubble left behind: deletes = %v", chat.deletedNames())
	}
	for _, post := range chat.allPosts() {
		if strings.Contains(post, "counting numbers") || strings.Contains(post, "bash, write") {
			t.Errorf("Pi's internal progress leaked into Chat: %q", post)
		}
		if strings.Contains(post, "rooter/test-model") {
			t.Errorf("model name leaked into Chat: %q", post)
		}
	}
}

// StatusMode=patch is the quiet alternative: the eyes bubble is rewritten into the answer instead of
// deleted, so a thread gets one message from start to finish.
func TestHandle_PatchModeReusesOneMessage(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	b.cfg.StatusMode = "patch"
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "patch me")
	if err := b.Handle(context.Background(), "id-patch", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := gw.lastPrompt().ID
	time.Sleep(40 * time.Millisecond)
	gw.settle(id, "patched answer")
	b.Wait()

	posts := chat.allPosts()
	if len(posts) != 1 {
		t.Fatalf("posts = %d, want only the status bubble: %#v", len(posts), posts)
	}
	got, ok := chat.patchOf(chat.nameOf(b.cfg.Eyes))
	if !ok || got != "patched answer" {
		t.Errorf("bubble patched to %q (found=%v), want the answer", got, ok)
	}
	if len(chat.deletedNames()) != 0 {
		t.Errorf("patch mode must not delete anything: %v", chat.deletedNames())
	}
}

func TestHandle_IgnoresUnlistedSender(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "hello there")
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	chatObj := doc["chat"].(map[string]any)
	chatObj["messagePayload"].(map[string]any)["message"].(map[string]any)["sender"] = map[string]any{"email": "stranger@example.org"}
	blob, _ := json.Marshal(doc)
	if err := b.Handle(context.Background(), "id-stranger", blob); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 || chat.postCount() != 0 {
		t.Errorf("unlisted sender must be silent: prompts=%d posts=%d", gw.promptCount(), chat.postCount())
	}
}

// An unmentioned message in a shared room is background noise; replying to it would make the bot
// unusable in a family space.
func TestHandle_IgnooresRoomMessageWithoutMention(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := fixtureBytes(t, "room_mention.json")
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	msg := doc["chat"].(map[string]any)["messagePayload"].(map[string]any)["message"].(map[string]any)
	msg["annotations"] = []any{}
	msg["text"] = "did you feed the dog"
	msg["argumentText"] = "did you feed the dog"
	blob, _ := json.Marshal(doc)
	if err := b.Handle(context.Background(), "id-nomention", blob); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 || chat.postCount() != 0 {
		t.Errorf("unmentioned room message must be ignored: prompts=%d posts=%d", gw.promptCount(), chat.postCount())
	}
}

func TestHandle_RoomMentionPrompt(t *testing.T) {
	b, _, gw := newTestBridge(t)
	b.cfg.Allowlist = append(b.cfg.Allowlist, "member@example.com")
	if err := b.Handle(context.Background(), "id-mention", fixtureBytes(t, "room_mention.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if gw.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1", gw.promptCount())
	}
	prompt := gw.lastPrompt()
	if prompt.Text != "Member Person <member@example.com>: hey what's up" {
		t.Errorf("prompt = %q, want mention stripped", prompt.Text)
	}
	if prompt.Space != "spaces/qi7VbgAAAAE" {
		t.Errorf("space = %q", prompt.Space)
	}
	gw.settle(prompt.ID, "nope")
	b.Wait()
}

func TestHandle_DuplicateIsDeduped(t *testing.T) {
	b, _, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "once only")
	for i := 0; i < 3; i++ {
		if err := b.Handle(context.Background(), fmt.Sprintf("dup-%d", i), raw); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if gw.promptCount() != 1 {
		t.Errorf("prompts = %d, want 1 after three deliveries of the same Chat message", gw.promptCount())
	}
	gw.settle(gw.lastPrompt().ID, "ok")
	b.Wait()
}

func TestHandle_HelpCommandDoesNotPromptPi(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "/help")
	if err := b.Handle(context.Background(), "id-help", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Errorf("prompts = %d, want 0 for a bridge command", gw.promptCount())
	}
	text, _, ok := chat.lastPost()
	if !ok {
		t.Fatalf("no help reply")
	}
	if want := "/steer <text>"; !contains(text, want) {
		t.Errorf("help text missing %q: %s", want, text)
	}
}

func TestHandle_StopCommandAborts(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "/stop")
	if err := b.Handle(context.Background(), "id-stop", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.aborts != 1 {
		t.Errorf("aborts = %d, want 1", gw.aborts)
	}
	if text, _, _ := chat.lastPost(); !contains(text, "Aborting") {
		t.Errorf("reply = %q", text)
	}
}

func TestHandle_UnknownCommandFallsBackToPi(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	gw.commandOK = true
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "/compact summarize please")
	if err := b.Handle(context.Background(), "id-compact", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if len(gw.commands) != 1 || gw.commands[0] != "/compact summarize please" {
		t.Errorf("commands = %#v, want the passthrough to reach Pi", gw.commands)
	}
	if gw.promptCount() != 0 {
		t.Errorf("a command must not also be sent as a prompt")
	}
	_ = chat
}

func TestHandle_ImageAttachmentIsForwarded(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := fixtureBytes(t, "image_message.json")
	ev, err := chatmsg.Parse(raw)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(ev.Attachments) == 0 {
		t.Fatalf("fixture has no attachments")
	}
	ref := ev.Attachments[0].ResourceRef
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	chat.mu.Lock()
	chat.downloads[ref] = png
	chat.mu.Unlock()

	if err := b.Handle(context.Background(), "id-img", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if gw.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1", gw.promptCount())
	}
	prompt := gw.lastPrompt()
	if len(prompt.Images) != 1 {
		t.Fatalf("images = %d, want 1 (ref %q)", len(prompt.Images), ref)
	}
	if prompt.Images[0].MimeType != "image/png" {
		t.Errorf("mimeType = %q", prompt.Images[0].MimeType)
	}
	if got, _ := base64.StdEncoding.DecodeString(prompt.Images[0].Data); len(got) != len(png) {
		t.Errorf("image bytes = %d, want %d", len(got), len(png))
	}
	gw.settle(prompt.ID, "a picture of a thing")
	b.Wait()
}

// An attachment we cannot download must not swallow the message: the text still reaches Pi with a
// note, because "here is a photo" with no complaint is worse than "here is a photo (unreadable)".
func TestHandle_UndownloadableAttachmentStillPrompts(t *testing.T) {
	b, _, gw := newTestBridge(t)
	raw := fixtureBytes(t, "image_message.json")
	if err := b.Handle(context.Background(), "id-badimg", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()
	if !contains(prompt.Text, "couldn't read attachment") {
		t.Errorf("prompt = %q, want a note about the failed attachment", prompt.Text)
	}
	gw.settle(prompt.ID, "ok")
	b.Wait()
}

func TestHandle_AddedToSpaceGreets(t *testing.T) {
	b, chat, _ := newTestBridge(t)
	if err := b.Handle(context.Background(), "id-add", fixtureBytes(t, "dm_added.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	text, opts, ok := chat.lastPost()
	if !ok {
		t.Fatalf("no greeting posted")
	}
	if opts.Space != "spaces/m4d90aAAAAE" {
		t.Errorf("greeting space = %q", opts.Space)
	}
	if !contains(text, "@pi-chat") {
		t.Errorf("greeting = %q, want usage hint", text)
	}
}

func TestHandle_GatewayUnreachableReplies(t *testing.T) {
	b, chat, _ := newTestBridge(t)
	gwDown := &deadGateway{}
	b.gw = gwDown
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "anyone there?")
	if err := b.Handle(context.Background(), "id-dead", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	text, _, ok := chat.lastPost()
	if !ok || !contains(text, "Couldn't reach Pi") {
		t.Errorf("expected an honest failure reply, got %q ok=%v", text, ok)
	}
}

func TestHandle_TurnVanishesReportsRestart(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "gone")
	if err := b.Handle(context.Background(), "id-gone", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := gw.lastPrompt().ID
	gw.mu.Lock()
	delete(gw.turns, id)
	gw.mu.Unlock()
	b.Wait()
	bubble := chat.nameOf(b.cfg.Eyes)
	text, ok := chat.patchOf(bubble)
	if !ok || !contains(text, "no record of that turn") {
		t.Errorf("expected the restart notice in the status bubble, got %q ok=%v", text, ok)
	}
	if len(chat.deletedNames()) != 0 {
		t.Errorf("a failed turn must leave its notice visible, not delete it: %v", chat.deletedNames())
	}
}

func TestHandle_AbortedTurn(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "stop that")
	if err := b.Handle(context.Background(), "id-abort", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := gw.lastPrompt().ID
	gw.setTurn(id, gateway.Turn{ID: id, Status: "aborted", Text: "partial work"})
	b.Wait()
	text, ok := chat.patchOf(chat.nameOf(b.cfg.Eyes))
	if !ok || !contains(text, "Stopped") {
		t.Errorf("abort notice = %q (found=%v)", text, ok)
	}
	if len(chat.deletedNames()) != 0 {
		t.Errorf("abort notice should stay visible: %v", chat.deletedNames())
	}
}

// An error is a ❌ line on the bubble, not a paragraph about the bridge.
func TestHandle_ErrorTurnShowsCrossNotice(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "explode")
	if err := b.Handle(context.Background(), "id-err", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := gw.lastPrompt().ID
	gw.setTurn(id, gateway.Turn{ID: id, Status: "error", Error: "model upstream died"})
	b.Wait()
	text, ok := chat.patchOf(chat.nameOf(b.cfg.Eyes))
	if !ok || !strings.HasPrefix(text, "❌") {
		t.Errorf("error notice = %q (found=%v), want it to start with the cross", text, ok)
	}
	if !contains(text, "model upstream died") {
		t.Errorf("error notice should carry the reason: %q", text)
	}
}

func TestHandle_MaxWaitExpiry(t *testing.T) {
	b, chat, _ := newTestBridge(t)
	b.cfg.MaxWait = 120 * time.Millisecond
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "forever")
	if err := b.Handle(context.Background(), "id-wait", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	// Never settle.
	time.Sleep(400 * time.Millisecond)
	b.Wait()
	chat.mu.Lock()
	defer chat.mu.Unlock()
	if len(chat.patches) == 0 {
		t.Fatalf("expected the watch to finish the status bubble it posted")
	}
	var last string
	for _, v := range chat.patches {
		last = v
	}
	if !strings.Contains(last, "stopped watching") {
		t.Errorf("final patch = %q, want a give-up notice", last)
	}
	if !strings.Contains(last, "/stop") {
		t.Errorf("give-up notice should tell the user how to intervene: %q", last)
	}
}

// Chat caps a message at 32,000 characters (measured in spike/FINDINGS.md), so a long Pi answer has to
// arrive as follow-up messages in the same thread, each with its own deterministic custom id.
func TestSend_ChunksLongAnswer(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	b.cfg.MaxChars = 500
	raw := withText(t, fixtureBytes(t, "dm_message.json"), "tell me a very long story")
	if err := b.Handle(context.Background(), "id-chunk", raw); err != nil {
		t.Fatalf("handle: %v", err)
	}
	id := gw.lastPrompt().ID
	long := strings.Repeat("a sentence that goes on and on for a while. ", 40)
	gw.settle(id, long)
	b.Wait()

	chat.mu.Lock()
	posts := chat.posts
	ids := map[string]bool{}
	for _, post := range posts {
		ids[post.opts.ClientID] = true
		if post.opts.Thread == "" {
			t.Errorf("a chunk lost the thread: %+v", post.opts)
		}
		if post.opts.Space != "spaces/m4d90aAAAAE" {
			t.Errorf("a chunk went to the wrong space: %+v", post.opts)
		}
	}
	chat.mu.Unlock()

	if len(ids) != len(posts) {
		t.Errorf("client ids collided across chunks: %d unique for %d posts", len(ids), len(posts))
	}
	if len(posts) < 4 {
		t.Fatalf("posts = %d, want eyes + at least 3 chunks of a 1759 character answer at MaxChars=500", len(posts))
	}
	if !hasName(chat.deletedNames(), chat.nameOf(b.cfg.Eyes)) {
		t.Errorf("status bubble left behind after a chunked answer")
	}

	// Reassemble the story to prove nothing was dropped. Chunking trims boundary whitespace and adds
	// continuation headers, so compare collapsed text rather than bytes.
	chat.mu.Lock()
	var pieces []string
	for _, post := range chat.posts {
		if post.text == b.cfg.Eyes {
			continue
		}
		pieces = append(pieces, dropContinued(post.text))
		if len([]rune(post.text)) > b.cfg.MaxChars+40 {
			t.Errorf("a chunk is over the limit: %d runes", len([]rune(post.text)))
		}
	}
	chat.mu.Unlock()

	if got, want := collapse(strings.Join(pieces, " ")), collapse(long); got != want {
		t.Errorf("reassembled story differs from the answer:\n got %d chars: %s...\nwant %d chars", len(got), firstChars(got, 60), len(want))
	}
	// Only overflow parts carry a header, so the first chunk must read as the answer.
	if strings.HasPrefix(pieces[0], "*(continued") {
		t.Errorf("first chunk carries a continuation header: %q", firstChars(pieces[0], 40))
	}
}

// A redelivery must not stack a second status bubble on the same turn: Chat refuses the duplicate
// client- id, and the bridge recovers the bubble it already posted by looking that id up.
func TestStatusBubble_DuplicatePostIsReused(t *testing.T) {
	b, chat, _ := newTestBridge(t)
	w := &watch{
		turnID:   "deadbeef",
		space:    "spaces/m4d90aAAAAE",
		thread:   "spaces/m4d90aAAAAE/threads/abc",
		replyID:  clientID("reply-deadbeef"),
		statusID: clientID("status-deadbeef"),
	}
	first := b.openStatus(w)
	if first == "" {
		t.Fatalf("no status bubble posted")
	}
	second := b.openStatus(w)
	if second != first {
		t.Errorf("redelivery created a second bubble %q instead of reusing %q", second, first)
	}
	if chat.postCount() != 1 {
		t.Errorf("posts = %d, want 1", chat.postCount())
	}

	// StatusMode=none means the user asked for no status at all, and nothing is posted.
	b.cfg.StatusMode = "none"
	if got := b.openStatus(&watch{turnID: "other", space: "spaces/m4d90aAAAAE", statusID: clientID("status-other")}); got != "" {
		t.Errorf("StatusMode=none still posted %q", got)
	}
	// Eyes=none is the other way to ask for the same thing.
	b.cfg.StatusMode = "bubble"
	b.cfg.Eyes = glyphOrNone("none")
	if b.cfg.Eyes != "" {
		t.Errorf("glyphOrNone(\"none\") = %q, want disabled", b.cfg.Eyes)
	}
	if got := b.openStatus(&watch{turnID: "third", space: "spaces/m4d90aAAAAE", statusID: clientID("status-third")}); got != "" {
		t.Errorf("Eyes=none still posted %q", got)
	}
}

// dropContinued removes the "(continued n/m)" header the bridge adds to overflow messages.
func dropContinued(text string) string {
	if strings.HasPrefix(text, "*(continued") {
		if i := strings.Index(text, "*\n\n"); i >= 0 {
			return text[i+3:]
		}
	}
	return text
}

func collapse(text string) string { return strings.Join(strings.Fields(text), " ") }

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func firstChars(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func TestNormalizeImageType(t *testing.T) {
	cases := map[string]string{
		"image/png":                "image/png",
		"image/jpeg; charset=x":    "image/jpeg",
		"IMAGE/WebP":               "image/webp",
		"application/pdf":          "",
		"":                         "",
		"text/plain":               "",
		"image/svg+xml":            "",
		"image/heic":               "",
		"application/octet-stream": "",
	}
	for in, want := range cases {
		if got := normalizeImageType(in); got != want {
			t.Errorf("normalizeImageType(%q) = %q, want %q", in, got, want)
		}
	}
}

// A plain message and a /steer are different things and Pi needs to be told which one it is getting.
func TestHandle_DeliveryModeFollowsTheCommand(t *testing.T) {
	b, _, gw := newTestBridge(t)
	if err := b.Handle(context.Background(), "id-plain", withText(t, fixtureBytes(t, "dm_message.json"), "check the disk")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := gw.lastPrompt().DeliverAs; got != "followUp" {
		t.Errorf("plain message DeliverAs = %q, want followUp so it queues behind any run in progress", got)
	}

	steer := withText(t, fixtureBytes(t, "dm_message.json"), "/steer use the other branch")
	steer = withMessageName(t, steer, "spaces/m4d90aAAAAE/messages/steer0000001.steer0000001")
	if err := b.Handle(context.Background(), "id- steer", steer); err != nil {
		t.Fatalf("handle steer: %v", err)
	}
	if got := gw.lastPrompt().DeliverAs; got != "steer" {
		t.Errorf("/steer DeliverAs = %q, want steer", got)
	}

	queue := withText(t, fixtureBytes(t, "dm_message.json"), "/queue second thing")
	queue = withMessageName(t, queue, "spaces/m4d90aAAAAE/messages/queue0000001.queue0000001")
	if err := b.Handle(context.Background(), "id-queue", queue); err != nil {
		t.Fatalf("handle queue: %v", err)
	}
	if got := gw.lastPrompt().DeliverAs; got != "followUp" {
		t.Errorf("/queue DeliverAs = %q, want followUp", got)
	}
	b.Wait()
}

// The bug this pins: two Chat prompts folded into one run used to each post that run's answer, so the
// thread got the same reply twice. Only the first watch to reach the turn posts.
func TestHandle_FoldedPromptsShareOneAnswer(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	b.cfg.PollInterval = 5 * time.Millisecond
	gw.foldInto = "run-in-progress"

	first := withText(t, fixtureBytes(t, "dm_message.json"), "count from one to five")
	if err := b.Handle(context.Background(), "fold-1", first); err != nil {
		t.Fatalf("handle first: %v", err)
	}
	second := withText(t, fixtureBytes(t, "dm_message.json"), "/steer stop, talk about trains instead")
	second = withMessageName(t, second, "spaces/m4d90aAAAAE/messages/steer0000002.steer0000002")
	if err := b.Handle(context.Background(), "fold-2", second); err != nil {
		t.Fatalf("handle second: %v", err)
	}

	// settle() keeps the inputs the fake appended when both prompts folded in, which is what the bridge
	// uses to tell "this answer is ours" from "some other run settled".
	gw.setTurn("run-in-progress", gateway.Turn{
		ID:     "run-in-progress",
		Status: "settled",
		Text:   "trains: one, two, three",
		Last:   "trains: one, two, three",
		Inputs: []gateway.Input{{ID: gw.prompts[0].ID}, {ID: gw.prompts[1].ID}},
	})
	b.Wait()

	if n := countPostsContaining(chat.allPosts(), "trains: one, two, three"); n != 1 {
		t.Errorf("answer posted %d times, want exactly 1 (posts: %v)", n, chat.allPosts())
	}
}

// A settled turn that does not carry our prompt is not our answer. Posting it would attribute somebody
// else's run to this user's message; saying nothing would look like the message was ignored.
func TestFinish_DoesNotDeliverAnAnswerThatIsNotOurs(t *testing.T) {
	b, chat, gw := newTestBridge(t)
	w := &watch{
		promptID: "mine",
		turnID:   "somebody-elses-run",
		space:    "spaces/m4d90aAAAAE",
		thread:   "spaces/m4d90aAAAAE/threads/abc",
		replyID:  clientID("reply-mine"),
		statusID: clientID("status-mine"),
	}
	gw.setTurn("somebody-elses-run", gateway.Turn{
		ID:     "somebody-elses-run",
		Status: "settled",
		Text:   "an answer to a different question",
		Last:   "an answer to a different question",
		Inputs: []gateway.Input{{ID: "not-mine"}},
	})
	b.finish(w, "", gw.turns["somebody-elses-run"], time.Second)

	if n := countPostsContaining(chat.allPosts(), "an answer to a different question"); n != 0 {
		t.Errorf("posted another run's answer %d times: %v", n, chat.allPosts())
	}
	if n := countPostsContaining(chat.allPosts(), "without picking your message up"); n != 1 {
		t.Errorf("dropped-message notice posted %d times, want 1: %v", n, chat.allPosts())
	}
}

// The shipped message protocol, pinned. Changing the default is a decision about what Ed sees in a
// thread, so it has to be made here rather than by accident.
func TestDefaultMessageProtocol(t *testing.T) {
	d := DefaultConfig()
	if d.StatusMode != "patch" {
		t.Errorf("StatusMode = %q, want patch (one message carries 👀 -> 🤔 -> answer)", d.StatusMode)
	}
	if d.Eyes != "\U0001f440" || d.Thinking != "\U0001f914" {
		t.Errorf("glyphs = %q/%q, want eyes and thinking", d.Eyes, d.Thinking)
	}
	if d.MaxChars > 32000 || d.MaxChars < 20000 {
		t.Errorf("MaxChars = %d, want headroom under Chat's measured 32,000 character cap", d.MaxChars)
	}
	if len(d.Allowlist) != 0 {
		t.Errorf("default allowlist is not empty: %v", d.Allowlist)
	}
	if !d.RequireMentionInRooms {
		t.Error("rooms must require a mention by default")
	}
	if d.DeliverAs != "followUp" {
		t.Errorf("DeliverAs = %q, want followUp: a message sent mid-run should get its own answer", d.DeliverAs)
	}
	if q := (Config{DeliverAs: "bogus"}).withDefaults(); q.DeliverAs != "followUp" {
		t.Errorf("unknown DeliverAs survived as %q", q.DeliverAs)
	}
	if q := (Config{DeliverAs: "followup"}).withDefaults(); q.DeliverAs != "followUp" {
		t.Errorf("DeliverAs %q not normalised to the gateway's spelling %q", "followup", q.DeliverAs)
	}
	// An unset StatusMode must not silently become "none".
	got := (Config{PollInterval: time.Second}).withDefaults()
	if got.StatusMode != "patch" || got.Eyes == "" {
		t.Errorf("withDefaults emptied the protocol: mode=%q eyes=%q", got.StatusMode, got.Eyes)
	}
	if bad := (Config{StatusMode: "bananas"}).withDefaults(); bad.StatusMode != "patch" {
		t.Errorf("unknown StatusMode survived as %q", bad.StatusMode)
	}
}

func TestClientIDRules(t *testing.T) {
	id := clientID("reply-spaces/m4d90aAAAAE/messages/abc.abc")
	if len(id) > 63 {
		t.Errorf("client id %q is %d chars, Chat allows 63", id, len(id))
	}
	if id[:7] != "client-" {
		t.Errorf("client id %q must begin with client-", id)
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '-' {
			t.Errorf("client id %q contains %q, Chat allows only lowercase alphanumerics and hyphens", id, r)
			break
		}
	}
	if clientID("a") == clientID("b") {
		t.Errorf("client ids collide")
	}
}

type deadGateway struct{}

func (d *deadGateway) Prompt(context.Context, gateway.PromptRequest) (gateway.PromptResponse, error) {
	return gateway.PromptResponse{}, fmt.Errorf("dial tcp 127.0.0.1:8090: connect: connection refused")
}
func (d *deadGateway) Turn(context.Context, string) (gateway.Turn, error) {
	return gateway.Turn{}, fmt.Errorf("connection refused")
}
func (d *deadGateway) Abort(context.Context) error { return fmt.Errorf("connection refused") }
func (d *deadGateway) Command(context.Context, string, string) error {
	return fmt.Errorf("connection refused")
}
func (d *deadGateway) State(context.Context) (gateway.State, error) {
	return gateway.State{}, fmt.Errorf("connection refused")
}
func (d *deadGateway) Commands(context.Context) ([]gateway.Command, error) {
	return nil, fmt.Errorf("connection refused")
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func TestFinalText_RejectsLeakedToolCallMarkup(t *testing.T) {
	// Exactly what reached the Chat thread on 2026-11-20.
	leaked := "<tool_call> <function=memory_recover_placeholder> </function> </tool_call>"
	if got := finalText(gateway.Turn{Status: "settled", Text: leaked, AnswerRejected: leaked}); strings.Contains(got, "tool_call") {
		t.Fatalf("leaked scaffolding was posted to Chat: %q", got)
	} else if !strings.Contains(got, "tool-call scaffolding") {
		t.Fatalf("expected an honest empty-answer notice, got %q", got)
	}

	// An answer that quotes tool syntax on purpose must survive intact.
	quoted := "Call `reload_pi` at the end of your turn — it looks like <tool_call><function=reload_pi></function> — and it reloads extensions."
	got := finalText(gateway.Turn{Status: "settled", Text: quoted})
	if !strings.Contains(got, "reload_pi") || !strings.Contains(got, "reloads extensions") {
		t.Fatalf("a real answer that mentions tool syntax got mangled: %q", got)
	}

	// Markup wrapped around a genuine answer keeps the prose.
	mixed := finalText(gateway.Turn{Status: "settled", Text: "<tool_call>\n<function=lookup>\n</function>\n</tool_call>\nThe bridge is running on digest a467ba20."})
	if !strings.Contains(mixed, "digest a467ba20") || strings.Contains(mixed, "tool_call") {
		t.Fatalf("prose should be kept and markup dropped, got %q", mixed)
	}

	// An aborted turn whose only text was scaffolding still reports the stop cleanly.
	if stopped := finalText(gateway.Turn{Status: "aborted", Text: leaked, AnswerRejected: leaked}); stopped != "⏹ Stopped." {
		t.Fatalf("aborted turn leaked scaffolding: %q", stopped)
	}
}

type gateCall struct {
	flow, summary, sender, message string
}

type fakeGate struct {
	mu      sync.Mutex
	answers map[string]string
	errs    map[string]error
	calls   []gateCall
}

func (g *fakeGate) Classify(_ context.Context, flow, summary, sender, message string) (string, string, time.Duration, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, gateCall{flow: flow, summary: summary, sender: sender, message: message})
	if err := g.errs[flow]; err != nil {
		return "", "", 0, err
	}
	return g.answers[flow], "prompt:" + flow, 0, nil
}

func (g *fakeGate) callsFor(flow string) []gateCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []gateCall
	for _, call := range g.calls {
		if call.flow == flow {
			out = append(out, call)
		}
	}
	return out
}

func gateEvent(dm, mentioned bool) *chatmsg.Event {
	return &chatmsg.Event{
		Kind: chatmsg.KindMessage, Space: "spaces/test", Thread: "spaces/test/threads/thread-1",
		SpaceType: "ROOM", DM: dm, Mentioned: mentioned, UserEmail: "owner@example.com",
		UserDisplayName: "Owner", Text: "hello", Argument: "hello", MessageName: "spaces/test/messages/m1.thread-1",
	}
}

// roomRootEvent is a new top-level message in a shared room: Chat gives it a thread equal to the space
// and a messageId.messageId name, which is what isTopLevel has to recognise.
func roomRootEvent() *chatmsg.Event {
	return &chatmsg.Event{
		Kind: chatmsg.KindMessage, Space: "spaces/room", Thread: "spaces/room",
		SpaceType: "ROOM", UserEmail: "owner@example.com", UserDisplayName: "Owner",
		Text: "what's the weather tomorrow", Argument: "what's the weather tomorrow",
		MessageName: "spaces/room/messages/root.root",
	}
}

func TestReplyGate_DMThreadAndInlineRouting(t *testing.T) {
	for _, test := range []struct {
		name, answer, wantThread string
	}{
		{name: "thread", answer: "THREAD", wantThread: ""},
		{name: "inline", answer: "INLINE", wantThread: "spaces/test/threads/thread-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := &fakeGate{answers: map[string]string{chatgate.FlowThreadRequest: test.answer}, errs: map[string]error{}}
			b := New(&fakeChat{}, &fakeGateway{}, Config{ReplyGateMode: "enforce", ReplyGate: gate}, nil)
			got, suppress := b.replyGate(context.Background(), gateEvent(true, false), "hello")
			if suppress || got != test.wantThread {
				t.Fatalf("route=%q suppress=%v, want route=%q", got, suppress, test.wantThread)
			}
			if len(gate.callsFor(chatgate.FlowThreadRequest)) != 1 {
				t.Fatalf("calls=%v", gate.calls)
			}
		})
	}
}

func TestReplyGate_MentionBypassesAndDryRunDoesNotChangeRouting(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowThreadRequest: "THREAD"}, errs: map[string]error{}}
	b := New(&fakeChat{}, &fakeGateway{}, Config{ReplyGateMode: "enforce", ReplyGate: gate}, nil)
	got, suppress := b.replyGate(context.Background(), gateEvent(true, true), "hello")
	if suppress || got != "spaces/test/threads/thread-1" || len(gate.calls) != 0 {
		t.Fatalf("mention should bypass gate: route=%q suppress=%v calls=%d", got, suppress, len(gate.calls))
	}

	dryGate := &fakeGate{answers: map[string]string{chatgate.FlowThreadRequest: "THREAD"}, errs: map[string]error{}}
	dry := New(&fakeChat{}, &fakeGateway{}, Config{ReplyGateMode: "dry-run", ReplyGate: dryGate}, nil)
	got, suppress = dry.replyGate(context.Background(), gateEvent(true, false), "hello")
	if suppress || got != "spaces/test/threads/thread-1" {
		t.Fatalf("dry-run changed today's route: route=%q suppress=%v", got, suppress)
	}
}

func TestReplyGate_ThreadSuppressionStillUpdatesSummary(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{
		chatgate.FlowSummary:       "weather tomorrow remains the open question",
		chatgate.FlowShouldRespond: "NO",
	}, errs: map[string]error{}}
	cfg := Config{ReplyGateMode: "enforce", ReplyGate: gate, RequireMentionInRooms: false}
	b := New(&fakeChat{}, &fakeGateway{}, cfg, nil)
	got, suppress := b.replyGate(context.Background(), gateEvent(false, false), "mum says she'll bring the esky")
	if !suppress || got == "" {
		t.Fatalf("expected silent suppression in thread, route=%q suppress=%v", got, suppress)
	}
	if gotSummary := b.summaryFor("spaces/test/threads/thread-1"); gotSummary != "weather tomorrow remains the open question" {
		t.Fatalf("summary=%q", gotSummary)
	}
	if calls := gate.callsFor(chatgate.FlowShouldRespond); len(calls) != 1 || calls[0].summary == "" {
		t.Fatalf("should-respond did not receive running summary: %+v", calls)
	}
}

func TestReplyGate_InvalidClassifierTokenFallsBack(t *testing.T) {
	for _, test := range []struct {
		dm           bool
		wantSuppress bool
	}{
		{dm: true},
		// An answer that is not an exact token means the model went off-prompt, which is the same
		// uncertainty as a transport failure: in a shared thread it silences rather than guesses.
		{dm: false, wantSuppress: true},
	} {
		gate := &fakeGate{answers: map[string]string{
			chatgate.FlowThreadRequest: "THREAD because...",
			chatgate.FlowShouldRespond: "NO because...",
		}, errs: map[string]error{}}
		b := New(&fakeChat{}, &fakeGateway{}, Config{ReplyGateMode: "enforce", ReplyGate: gate, RequireMentionInRooms: false}, nil)
		got, suppress := b.replyGate(context.Background(), gateEvent(test.dm, false), "hello")
		wantThread := "spaces/test/threads/thread-1"
		if suppress != test.wantSuppress || got != wantThread {
			t.Fatalf("dm=%v route=%q suppress=%v, want route=%q suppress=%v", test.dm, got, suppress, wantThread, test.wantSuppress)
		}
	}
}

func TestReplyGate_Fallbacks(t *testing.T) {
	for _, test := range []struct {
		name         string
		dm           bool
		wantSuppress bool
		wantThread   string
		wantAction   string
	}{
		// A 1:1 message is addressed by construction, so a failed classifier still gets the default
		// route. In a shared thread an undecided message is silenced: tagging is the escape hatch.
		{name: "dm classifier failure", dm: true, wantThread: "spaces/test/threads/thread-1", wantAction: "fallback-inline"},
		{name: "thread classifier failure", dm: false, wantSuppress: true, wantThread: "spaces/test/threads/thread-1", wantAction: "fallback-suppress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := &fakeGate{answers: map[string]string{}, errs: map[string]error{
				chatgate.FlowThreadRequest: fmt.Errorf("timeout"),
				chatgate.FlowShouldRespond: fmt.Errorf("HTTP 503"),
			}}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			b := New(&fakeChat{}, &fakeGateway{}, Config{ReplyGateMode: "enforce", ReplyGate: gate, RequireMentionInRooms: false}, logger)
			got, suppress := b.replyGate(context.Background(), gateEvent(test.dm, false), "hello")
			if suppress != test.wantSuppress || got != test.wantThread {
				t.Fatalf("route=%q suppress=%v, want route=%q suppress=%v", got, suppress, test.wantThread, test.wantSuppress)
			}
			if !strings.Contains(logs.String(), "action="+test.wantAction) {
				t.Fatalf("logs=%q, want action=%s", logs.String(), test.wantAction)
			}
		})
	}
}

func TestGroupTopLevelGateIsHardAndTaggedThreadsRemainEligible(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{
		chatgate.FlowSummary:       "a thread topic",
		chatgate.FlowShouldRespond: "YES",
	}, errs: map[string]error{}}
	b := New(&fakeChat{}, &fakeGateway{}, Config{ReplyGateMode: "dry-run", ReplyGate: gate, RequireMentionInRooms: false}, nil)
	outside := gateEvent(false, false)
	outside.MessageName = "spaces/test/messages/top.top"
	outside.Thread = "spaces/test/threads/top"
	if b.groupMessageEligible(context.Background(), outside) {
		t.Fatal("new group thread was pre-authorized")
	}
	// The hard top-level rule is checked before replyGate, so this path must not call the model.
	if b.groupMessageEligible(context.Background(), outside) || len(gate.calls) != 0 {
		t.Fatalf("untagged top-level message was not ignored: calls=%v", gate.calls)
	}

	tagged := gateEvent(false, true)
	b.markPiThread(tagged.Thread)
	followUp := gateEvent(false, false)
	followUp.MessageName = "spaces/test/messages/follow.thread-1"
	if !b.groupMessageEligible(context.Background(), followUp) {
		t.Fatal("tagged thread was not recorded")
	}
	if _, suppress := b.replyGate(context.Background(), followUp, followUp.Prompt()); suppress {
		t.Fatal("eligible untagged thread was suppressed in dry-run")
	}
	if len(gate.callsFor(chatgate.FlowShouldRespond)) != 1 {
		t.Fatalf("eligible thread did not reach should-respond: %+v", gate.calls)
	}
}

func TestRoomRootUntaggedStaysIgnoredWithoutTheFeatureFlag(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	for _, test := range []struct {
		mode    string
		mention bool // RequireMentionInRooms: the shipped combination is true.
	}{
		{mode: "off", mention: true},
		{mode: "dry-run"},
		{mode: "enforce"},
	} {
		b := New(&fakeChat{}, &fakeGateway{}, Config{ReplyGateMode: test.mode, ReplyGate: gate, RequireMentionInRooms: test.mention}, nil)
		if b.groupMessageEligible(context.Background(), roomRootEvent()) {
			t.Fatalf("mode %s: room gating pre-authorized without the flag", test.mode)
		}
	}
	if len(gate.calls) != 0 {
		t.Fatalf("default behaviour made model calls: %+v", gate.calls)
	}
}

func TestConfigHonoursOperatorValuesThatHaveNoSentinel(t *testing.T) {
	// DeliverAs and GateRoomRootUntagged are read from the operator's config, not from the pristine
	// defaults: a pod that sets PI_CHAT_DELIVER_AS=steer must actually steer.
	steer := New(&fakeChat{}, &fakeGateway{}, Config{DeliverAs: "steer"}, nil)
	if steer.cfg.DeliverAs != "steer" {
		t.Fatalf("DeliverAs=%q", steer.cfg.DeliverAs)
	}
	if got := New(&fakeChat{}, &fakeGateway{}, Config{DeliverAs: "fold"}, nil).cfg.DeliverAs; got != "steer" {
		t.Fatalf("DeliverAs alias=%q", got)
	}
	if got := New(&fakeChat{}, &fakeGateway{}, Config{DeliverAs: "nonsense"}, nil).cfg.DeliverAs; got != "followUp" {
		t.Fatalf("unknown DeliverAs=%q, want the default", got)
	}
	if got := New(&fakeChat{}, &fakeGateway{}, Config{GateRoomRootUntagged: true}, nil).cfg.GateRoomRootUntagged; !got {
		t.Fatal("GateRoomRootUntagged was dropped by withDefaults")
	}
}

func TestRoomRootGateAnswersInANewThreadAndStaysSilentOnNo(t *testing.T) {
	for _, test := range []struct {
		name         string
		thread       string
		answer       string
		wantThread   string
		wantSuppress bool
	}{
		// Chat gives a new room message its own thread, and the answer belongs in it so the room root
		// stays clean. Only when no thread resource came with the event does the answer go root-level.
		{name: "yes in the message's own thread", thread: "spaces/room/threads/root-thread", answer: "YES", wantThread: "spaces/room/threads/root-thread"},
		{name: "yes with no thread resource", thread: "spaces/room", answer: "YES", wantThread: ""},
		{name: "no", thread: "spaces/room/threads/root-thread", answer: "NO", wantThread: "spaces/room/threads/root-thread", wantSuppress: true},
		{name: "off prompt", thread: "spaces/room/threads/root-thread", answer: "YES probably", wantThread: "spaces/room/threads/root-thread", wantSuppress: true},
		{name: "classifier failure", thread: "spaces/room/threads/root-thread", answer: "", wantThread: "spaces/room/threads/root-thread", wantSuppress: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: test.answer}, errs: map[string]error{}}
			if test.answer == "" {
				gate.errs[chatgate.FlowRoomRespond] = fmt.Errorf("HTTP 503")
			}
			b := New(&fakeChat{}, &fakeGateway{}, Config{
				ReplyGateMode: "enforce", ReplyGate: gate, RequireMentionInRooms: false,
				GateRoomRootUntagged: true,
			}, nil)
			ev := roomRootEvent()
			ev.Thread = test.thread
			if !b.groupMessageEligible(context.Background(), ev) {
				t.Fatal("room root was not eligible with the flag on")
			}
			got, suppress := b.replyGate(context.Background(), ev, ev.Prompt())
			if suppress != test.wantSuppress || got != test.wantThread {
				t.Fatalf("route=%q suppress=%v, want route=%q suppress=%v", got, suppress, test.wantThread, test.wantSuppress)
			}
			if len(gate.callsFor(chatgate.FlowSummary)) != 0 {
				t.Fatalf("a room root message has no thread to summarise: %+v", gate.calls)
			}
			calls := gate.callsFor(chatgate.FlowRoomRespond)
			if len(calls) != 1 {
				t.Fatalf("room gate calls=%+v", gate.calls)
			}
		})
	}
}

func TestRoomRootGateSeesWhatPiChatLastSaidInTheRoom(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b := New(&fakeChat{}, &fakeGateway{}, Config{
		ReplyGateMode: "enforce", ReplyGate: gate, RequireMentionInRooms: false,
		GateRoomRootUntagged: true,
	}, nil)
	b.noteRoomAnswer("spaces/room", "overnight low was 4C and the shed heater ran three times")
	ev := roomRootEvent()
	ev.Text = "that seems cold, what was it overnight"
	if _, suppress := b.replyGate(context.Background(), ev, ev.Prompt()); suppress {
		t.Fatal("room gate suppressed a follow-up on its own report")
	}
	calls := gate.callsFor(chatgate.FlowRoomRespond)
	if len(calls) != 1 || !strings.Contains(calls[0].summary, "overnight low was 4C") {
		t.Fatalf("room gate ran without the bot's own last answer: %+v", calls)
	}
	if other := b.roomAnswerFor("spaces/other"); other != "" {
		t.Fatalf("room context leaked across spaces: %q", other)
	}
}

func TestRoomRootGateDryRunMeasuresWithoutPosting(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	// The room trial mode is deliberately independent of the global gate mode: the deployed bridge runs
	// enforce for mentions, DMs and tagged threads, and watching a room must not downgrade those.
	b := New(&fakeChat{}, &fakeGateway{}, Config{
		ReplyGateMode: "enforce", ReplyGate: gate, RequireMentionInRooms: false,
		GateRoomRootMode: "dry-run",
	}, nil)
	ev := roomRootEvent()
	if !b.groupMessageEligible(context.Background(), ev) {
		t.Fatal("dry-run must still measure the room classifier")
	}
	if _, suppress := b.replyGate(context.Background(), ev, ev.Prompt()); !suppress {
		t.Fatal("dry-run would have answered a room message")
	}
	if len(gate.callsFor(chatgate.FlowRoomRespond)) != 1 {
		t.Fatalf("dry-run did not measure: %+v", gate.calls)
	}
}

// The whole bridge used to have one gate mode, so trialling a room meant setting dry-run globally, and
// dry-run never suppresses: group-thread messages the gate had been silencing would have been answered
// for the duration of the trial. This is the regression test for that trap.
func TestRoomDryRunDoesNotDowngradeFlowsThatAlreadyGate(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{
		chatgate.FlowRoomRespond:   "YES",
		chatgate.FlowSummary:       "garage door still open",
		chatgate.FlowShouldRespond: "NO",
	}, errs: map[string]error{}}
	b := New(&fakeChat{}, &fakeGateway{}, Config{
		ReplyGateMode: "enforce", GateRoomRootMode: "dry-run", ReplyGate: gate, RequireMentionInRooms: false,
	}, nil)
	if _, suppress := b.replyGate(context.Background(), roomRootEvent(), "anyone seen my other phone"); !suppress {
		t.Fatal("room dry-run answered in a shared room")
	}
	if _, suppress := b.replyGate(context.Background(), gateEvent(false, false), "mum says she'll bring the esky"); !suppress {
		t.Fatal("room dry-run turned should-respond suppression into an answer")
	}
	if len(gate.callsFor(chatgate.FlowShouldRespond)) != 1 {
		t.Fatalf("should-respond did not run while the room was being watched: %+v", gate.calls)
	}
}

func TestRoomGateModeAliasesAndUnknownValues(t *testing.T) {
	if got := New(&fakeChat{}, &fakeGateway{}, Config{GateRoomRootMode: "classify"}, nil).cfg.GateRoomRootMode; got != "enforce" {
		t.Fatalf("classify alias=%q, want enforce", got)
	}
	if got := New(&fakeChat{}, &fakeGateway{}, Config{GateRoomRootUntagged: true}, nil).cfg.GateRoomRootMode; got != "enforce" {
		t.Fatalf("legacy bool did not map to a mode: %q", got)
	}
	// A typo in a pod env var must not put untested text in front of other people.
	if got := New(&fakeChat{}, &fakeGateway{}, Config{GateRoomRootMode: "sometimes"}, nil).cfg.GateRoomRootMode; got != "ignore" {
		t.Fatalf("unknown room mode %q did not fall back to ignoring", got)
	}
	if got := New(&fakeChat{}, &fakeGateway{}, Config{}, nil).cfg.GateRoomRootMode; got != "ignore" {
		t.Fatalf("default room mode=%q, want ignore", got)
	}
}

func TestGroupThreadOwnershipSurvivesBridgeRestart(t *testing.T) {
	thread := "spaces/test/threads/persisted"
	chat := &fakeChat{ownedThreads: map[string]bool{thread: true}}
	first := New(chat, &fakeGateway{}, Config{ReplyGateMode: "dry-run", ReplyGate: &fakeGate{answers: map[string]string{}, errs: map[string]error{}}, RequireMentionInRooms: false}, nil)
	ev := gateEvent(false, false)
	ev.Thread = thread
	ev.MessageName = "spaces/test/messages/follow-up.persisted"
	if !first.groupMessageEligible(context.Background(), ev) {
		t.Fatal("first bridge did not recover ownership from Chat history")
	}
	second := New(chat, &fakeGateway{}, Config{ReplyGateMode: "dry-run", ReplyGate: &fakeGate{answers: map[string]string{}, errs: map[string]error{}}, RequireMentionInRooms: false}, nil)
	if !second.groupMessageEligible(context.Background(), ev) {
		t.Fatal("restarted bridge did not recover ownership from Chat history")
	}
}

// TestWithDefaultsCarriesEveryConfigField is a guard against a bug class this package has now produced
// twice: withDefaults starts from DefaultConfig() and rebuilds the config field by field, so adding a
// field to Config without copying it here means every bridge that sets that field gets a zero value and
// the feature quietly does nothing. Identity was dropped exactly that way - the room identity lookup was
// configured, wired and unit-tested in isolation, and nil by the time the bridge used it.
//
// The test is deliberately annoying rather than clever: it fails if a new Config field is not exercised
// with a non-zero value, because a fixture that leaves a field empty cannot notice it being dropped.
func TestWithDefaultsCarriesEveryConfigField(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{}, errs: map[string]error{}}
	identity := &fakeIdentity{byID: map[string]SenderIdentity{}}
	in := Config{
		Allowlist:             []string{"someone@example.com"},
		RequireMentionInRooms: true,
		GateRoomRootUntagged:  true,
		GateRoomRootMode:      "enforce",
		StatusMode:            "patch",
		DeliverAs:             "followUp",
		Eyes:                  "👀",
		Thinking:              "🤔",
		PollInterval:          10 * time.Millisecond,
		MaxWait:               2 * time.Second,
		MaxChars:              4000,
		ImageMaxBytes:         1 << 20,
		MaxImages:             2,
		PrefixWithSender:      true,
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
		ReplyGateEndpoint:     "http://127.0.0.1:1/v1/chat/completions",
		ReplyGateModel:        "some/model",
		Identity:              identity,
		IdentitySeed:          map[string]string{"users/1": "someone@example.com"},
		ReplyGateTimeout:      3 * time.Second,
		ReplyGateAttempts:     []time.Duration{3 * time.Second},
	}

	out := in.withDefaults()

	inValue := reflect.ValueOf(in)
	outValue := reflect.ValueOf(out)
	if inValue.NumField() != outValue.NumField() {
		t.Fatalf("Config has %d fields and withDefaults returned %d", inValue.NumField(), outValue.NumField())
	}
	fields := reflect.TypeOf(in)
	for i := 0; i < inValue.NumField(); i++ {
		name := fields.Field(i).Name
		if isZeroValue(inValue.Field(i)) {
			t.Errorf("Config.%s is not exercised by this fixture: a copy dropped in withDefaults would go unnoticed. Set it to a non-zero value.", name)
			continue
		}
		if isZeroValue(outValue.Field(i)) {
			t.Errorf("Config.%s was set and came back empty: withDefaults dropped it", name)
		}
	}
}

func isZeroValue(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	default:
		return v.IsZero()
	}
}
