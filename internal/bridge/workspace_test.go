package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/initialed85/pi-bot/internal/chatapi"
	"github.com/initialed85/pi-bot/internal/chatgate"
	"github.com/initialed85/pi-bot/internal/chatmsg"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// ---------------------------------------------------------------- room listener test setup

var listenerAttrs = map[string]string{
	"ce-type":    chatmsg.WorkspaceMessageCreated,
	"ce-subject": "//chat.googleapis.com/spaces/AAAA1pNBS1A",
	"ce-time":    "2026-11-25T02:15:38Z",
}

func listenerFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "chatmsg", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

// listenerChat is a fakeChat whose spaces are declared per test, because whether a space is a 1:1 or a
// group room is exactly the question the listener has to answer and it cannot answer it from its own
// payload.
type listenerChat struct {
	fakeChat
	infos map[string]*chatapi.SpaceInfo
	err   error
	calls int
}

func (c *listenerChat) GetSpace(_ context.Context, name string) (*chatapi.SpaceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	if info, ok := c.infos[name]; ok {
		return info, nil
	}
	return &chatapi.SpaceInfo{Name: name, SpaceType: "ROOM"}, nil
}

func listenerBridge(cfg Config) (*Bridge, *listenerChat, *fakeGateway) {
	chat := &listenerChat{
		fakeChat: fakeChat{downloads: map[string][]byte{}, patches: map[string]string{}},
		infos: map[string]*chatapi.SpaceInfo{
			"spaces/AAAA1pNBS1A": {Name: "spaces/AAAA1pNBS1A", SpaceType: "ROOM"},
		},
	}
	gw := &fakeGateway{turns: map[string]gateway.Turn{}, state: gateway.State{Idle: true, Model: "rooter/test-model"}}
	cfg.Eyes = "none"           // no status bubble in these tests: they assert on answers, not on glyphs
	cfg.PrefixWithSender = true // production default: a shared room prompt must name its author
	cfg.PollInterval = 10 * time.Millisecond
	cfg.MaxWait = 2 * time.Second
	cfg.MaxChars = 4000
	cfg.Allowlist = []string{"initialed85@gmail.com", "amy.annisbrown@gmail.com"}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(chat, gw, cfg, log), chat, gw
}

// appPathCopy builds the app-credential version of the same Chat message, so a test can prove what
// happens when one message really does arrive on both topics.
func appPathCopy(name, text, argument string) []byte {
	payload := map[string]any{
		"chat": map[string]any{
			"eventTime": "2026-11-25T02:19:11.220Z",
			"messagePayload": map[string]any{
				"message": map[string]any{
					"name":         name,
					"text":         text,
					"argumentText": argument,
					"annotations": []any{map[string]any{
						"type":        "USER_MENTION",
						"userMention": map[string]any{"type": "MENTION", "user": map[string]any{"name": "users/1", "type": "BOT"}},
					}},
					"sender": map[string]any{"email": "initialed85@gmail.com", "displayName": "Ed Beech", "type": "HUMAN"},
				},
				"space": map[string]any{"name": "spaces/AAAA1pNBS1A", "spaceType": "ROOM"},
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

// ---------------------------------------------------------------- tests

// TestHandleWorkspace_BotReplyIsNotAnswered is the loop guard. The room listener delivers pi-chat's own
// messages; answering those is an infinite conversation the bot has with itself in a shared room, in
// front of other people.
func TestHandleWorkspace_BotReplyIsNotAnswered(t *testing.T) {
	b, chat, gw := listenerBridge(Config{})
	if err := b.HandleWorkspace(context.Background(), "psm-bot", listenerAttrs, listenerFixture(t, "workspace_room_bot.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0: a bot-authored room message must never reach Pi", gw.promptCount())
	}
	if len(chat.posts) != 0 {
		t.Fatalf("posts = %d, want 0", len(chat.posts))
	}
}

// TestHandleWorkspace_MentionSurvivesTheListenerCopy is the dedupe ordering rule. If the listener copy
// of a tagged message consumed the dedupe key, the app copy - the one with the envelope identity and
// the slash-command payload - would be dropped as a duplicate and a tagged message would go unanswered.
func TestHandleWorkspace_MentionSurvivesTheListenerCopy(t *testing.T) {
	b, chat, gw := listenerBridge(Config{RequireMentionInRooms: true})
	mentionName := "spaces/AAAA1pNBS1A/messages/mD2hV6yLp9f.mD2hV6yLp9f"

	if err := b.HandleWorkspace(context.Background(), "psm-listener", listenerAttrs, listenerFixture(t, "workspace_room_mention.json")); err != nil {
		t.Fatalf("listener handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0: the listener must not answer a message the app path also delivers", gw.promptCount())
	}

	if err := b.Handle(context.Background(), "psm-app", appPathCopy(mentionName, "@pi-chat what did the garage log say overnight", " what did the garage log say overnight")); err != nil {
		t.Fatalf("app handle: %v", err)
	}
	prompt := gw.lastPrompt()
	gw.settle(prompt.ID, "the log says nothing opened after 23:00")
	b.Wait()
	if gw.promptCount() != 1 {
		t.Fatalf("prompts = %d, want exactly 1: the tagged message must survive its own listener copy", gw.promptCount())
	}
	if !strings.Contains(prompt.Text, "garage log") {
		t.Errorf("prompt = %q", prompt.Text)
	}
	if !strings.Contains(prompt.Text, "initialed85@gmail.com") {
		t.Errorf("sender identity lost from the prompt: %q", prompt.Text)
	}
	text, _, ok := chat.lastPost()
	if !ok || !strings.Contains(text, "nothing opened") {
		t.Errorf("reply not posted: %q ok=%v", text, ok)
	}
}

// TestHandleWorkspace_UntaggedRoomRootStaysIgnoredByDefault keeps the rollout staged: turning the
// listener on is not the same as deciding to answer untagged room chatter, and the flag that decides
// that (PI_CHAT_GATE_ROOM_ROOT) is still off in the deployment.
func TestHandleWorkspace_UntaggedRoomRootStaysIgnoredByDefault(t *testing.T) {
	b, chat, gw := listenerBridge(Config{RequireMentionInRooms: true})
	if err := b.HandleWorkspace(context.Background(), "psm-1", listenerAttrs, listenerFixture(t, "workspace_room_untagged.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0 while the room gate is off", gw.promptCount())
	}
	if len(chat.posts) != 0 {
		t.Fatalf("posts = %d, want 0", len(chat.posts))
	}
}

// TestHandleWorkspace_RoomClassifierIsAskedInDryRun proves the listener can be trialled without the
// trial becoming the noise: the classifier is consulted, the would-be answer is logged and dropped.
func TestHandleWorkspace_RoomClassifierIsAskedInDryRun(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, chat, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "dry-run",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
	})
	if err := b.HandleWorkspace(context.Background(), "psm-2", listenerAttrs, listenerFixture(t, "workspace_room_untagged.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	calls := gate.callsFor(chatgate.FlowRoomRespond)
	if len(calls) != 1 {
		t.Fatalf("room classifier calls = %d, want 1: %+v", len(calls), gate.calls)
	}
	if calls[0].message != "the garage door has been open since this morning" {
		t.Errorf("classifier saw %q", calls[0].message)
	}
	if calls[0].sender == "" {
		t.Error("classifier was not told who wrote the message")
	}
	if gw.promptCount() != 0 || len(chat.posts) != 0 {
		t.Fatalf("dry-run answered: prompts=%d posts=%d", gw.promptCount(), len(chat.posts))
	}
}

// TestHandleWorkspace_RoomAnswerLandsInItsOwnThread is the enforce behaviour Ed's routing policy asks
// for: an untagged room message that the classifier judges worth answering is answered in the thread
// Chat created for it, not bolted onto the room root.
func TestHandleWorkspace_RoomAnswerLandsInItsOwnThread(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, chat, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootUntagged:  true,
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
		PrefixWithSender:      true,
	})
	if err := b.HandleWorkspace(context.Background(), "psm-3", listenerAttrs, listenerFixture(t, "workspace_room_untagged.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()
	if prompt.Space != "spaces/AAAA1pNBS1A" {
		t.Errorf("prompt space = %q", prompt.Space)
	}
	if !strings.HasPrefix(prompt.Text, "Ed Beech <initialed85@gmail.com>:") {
		t.Errorf("room prompt must name its author, got %q", prompt.Text)
	}
	gw.settle(prompt.ID, "the door sensor last reported open at 04:12")
	b.Wait()
	text, opts, ok := chat.lastPost()
	if !ok {
		t.Fatalf("no reply posted")
	}
	if text != "the door sensor last reported open at 04:12" {
		t.Errorf("reply = %q", text)
	}
	// Chat threads a new room message with a thread named after itself, and the answer belongs in that
	// thread - not at the room root, where it would read as commentary on whoever spoke last.
	if opts.Thread != "spaces/AAAA1pNBS1A/threads/kQ7mZ2pXr1o" {
		t.Errorf("reply thread = %q, want the thread Chat created for the message", opts.Thread)
	}
	if opts.Space != "spaces/AAAA1pNBS1A" {
		t.Errorf("reply space = %q", opts.Space)
	}
}

// TestHandleWorkspace_RoomMessageIsSilentWhenTheClassifierSaysNo is the intrusion guard: in a shared
// room, an unasked-for message costs nothing.
func TestHandleWorkspace_RoomMessageIsSilentWhenTheClassifierSaysNo(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "NO"}, errs: map[string]error{}}
	b, chat, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootUntagged:  true,
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
	})
	if err := b.HandleWorkspace(context.Background(), "psm-4", listenerAttrs, listenerFixture(t, "workspace_room_thread_reply.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0 when the classifier says no", gw.promptCount())
	}
	if len(chat.posts) != 0 {
		t.Fatalf("posts = %d, want 0", len(chat.posts))
	}
}

// TestHandleWorkspace_DMIsLeftToTheAppPath keeps 1:1 behaviour exactly as it is measured today: the app
// already sees every message in its own DM, so the listener adding a second copy of it would only
// create a race over which path answers.
func TestHandleWorkspace_DMIsLeftToTheAppPath(t *testing.T) {
	b, chat, gw := listenerBridge(Config{GateRoomRootUntagged: true, ReplyGateMode: "enforce", ReplyGate: &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}})
	b.mu.Lock()
	b.spaces = map[string]*chatapi.SpaceInfo{"spaces/m4d90aAAAAE": {Name: "spaces/m4d90aAAAAE", SpaceType: "DM", SingleUserBotDM: true}}
	b.mu.Unlock()

	body := []byte(`{"message":{"name":"spaces/m4d90aAAAAE/messages/d1.d1","text":"what is 12*13?","sender":{"email":"initialed85@gmail.com","displayName":"Ed Beech","type":"HUMAN"}}}`)
	if err := b.HandleWorkspace(context.Background(), "psm-dm", listenerAttrs, body); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0: 1:1 messages belong to the app path", gw.promptCount())
	}
	// The listener copy must not consume the dedupe key, so the app's own copy still works.
	appCopy := []byte(fmt.Sprintf(`{"chat":{"messagePayload":{"message":{"name":"spaces/m4d90aAAAAE/messages/d1.d1","text":"what is 12*13?","argumentText":"what is 12*13?","sender":{"email":"initialed85@gmail.com","displayName":"Ed Beech","type":"HUMAN"}},"space":{"name":"spaces/m4d90aAAAAE","spaceType":"DM","singleUserBotDm":true}}}}`))
	if err := b.Handle(context.Background(), "psm-dm-app", appCopy); err != nil {
		t.Fatalf("app handle: %v", err)
	}
	if gw.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1: the app path must still answer the same message", gw.promptCount())
	}
	_ = chat
}

// TestHandleWorkspace_UnknownSenderEmailIsRejected keeps the allowlist in charge even on the path where
// Google decides what identity to ship.
func TestHandleWorkspace_UnknownSenderEmailIsRejected(t *testing.T) {
	b, _, gw := listenerBridge(Config{GateRoomRootUntagged: true, ReplyGateMode: "enforce", ReplyGate: &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}})
	body := []byte(`{"message":{"name":"spaces/AAAA1pNBS1A/messages/x1.x1","text":"excuse me, what can you do?","sender":{"name":"spaces/999","displayName":"Stranger","type":"HUMAN"}}}`)
	if err := b.HandleWorkspace(context.Background(), "psm-stranger", listenerAttrs, body); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0: no email means no permission", gw.promptCount())
	}
}

// TestHandleWorkspace_SpaceLookupIsCached stops the listener from paying an API call per message in a
// busy room.
func TestHandleWorkspace_SpaceLookupIsCached(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "NO"}, errs: map[string]error{}}
	b, chat, _ := listenerBridge(Config{GateRoomRootUntagged: true, ReplyGateMode: "enforce", ReplyGate: gate})
	for i := 0; i < 3; i++ {
		body := []byte(fmt.Sprintf(`{"message":{"name":"spaces/AAAA1pNBS1A/messages/m%d.m%d","text":"ordinary room message %d","sender":{"email":"initialed85@gmail.com","type":"HUMAN"}}}`, i, i, i))
		if err := b.HandleWorkspace(context.Background(), fmt.Sprintf("psm-%d", i), listenerAttrs, body); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	b.Wait()
	chat.mu.Lock()
	calls := chat.calls
	chat.mu.Unlock()
	if calls != 1 {
		t.Fatalf("space lookups = %d, want 1: a space's type does not change, so ask once", calls)
	}
}

// TestHandleWorkspace_SpaceLookupFailureStillRoutesAsRoom is the safe direction to fail in: not knowing
// a space's type must not silently turn a group room into a DM that always answers inline.
func TestHandleWorkspace_SpaceLookupFailureStillRoutesAsRoom(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "NO"}, errs: map[string]error{}}
	b, chat, gw := listenerBridge(Config{GateRoomRootUntagged: true, ReplyGateMode: "enforce", ReplyGate: gate})
	chat.mu.Lock()
	chat.err = fmt.Errorf("chatapi: get space: 503 unavailable")
	chat.mu.Unlock()

	body := []byte(`{"message":{"name":"spaces/AAAA1pNBS1A/messages/e1.e1","text":"is it still raining there","sender":{"email":"initialed85@gmail.com","type":"HUMAN"}}}`)
	if err := b.HandleWorkspace(context.Background(), "psm-lookup", listenerAttrs, body); err != nil {
		t.Fatalf("handle: %v", err)
	}
	b.Wait()
	if len(gate.callsFor(chatgate.FlowRoomRespond)) != 1 {
		t.Fatalf("gate calls = %v, want the room classifier consulted", gate.calls)
	}
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0: the classifier said no", gw.promptCount())
	}
}
