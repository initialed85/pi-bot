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
					"sender": map[string]any{"name": "users/103902620694999538355", "legacyId": "103902620694999538355", "email": "initialed85@gmail.com", "displayName": "Ed Beech", "type": "HUMAN"},
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

// TestHandleWorkspace_GateJudgesTheLineAndPiGetsTheScaffold is the prompt-hygiene split. The classifier
// gets the human's own words in `Message` and the room only in `Transcript`; Pi gets the scaffolded
// window. Before this the gate was handed the Pi prompt, so it read the room twice - visible in
// production logs as a "Message" field containing "New message:" - and cost 2x tokens for one verdict.
func TestHandleWorkspace_GateJudgesTheLineAndPiGetsTheScaffold(t *testing.T) {
	conv := &fakeConversation{
		people:     []Person{humanPerson("users/222", "initialed85@gmail.com")},
		transcript: "[06:28] initialed85: the dryer is still going",
	}
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, chat, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "enforce",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
		Conversation:          conv,
	})
	if err := b.HandleWorkspace(context.Background(), "psm-scaffold", listenerAttrs, listenerFixture(t, "workspace_room_untagged.json")); err != nil {
		t.Fatalf("handle: %v", err)
	}
	prompt := gw.lastPrompt()
	gw.settle(prompt.ID, "the dryer finished at 05:40")
	b.Wait()

	calls := gate.callsFor(chatgate.FlowRoomRespond)
	if len(calls) != 1 {
		t.Fatalf("gate calls = %d, want 1: %+v", len(calls), gate.calls)
	}
	if calls[0].message != "the garage door has been open since this morning" {
		t.Errorf("classifier judged something other than the human's line: %q", calls[0].message)
	}
	if strings.Contains(calls[0].message, "New message:") || strings.Contains(calls[0].message, "[") {
		t.Errorf("the classifier was handed the scaffold, so it read the window twice: %q", calls[0].message)
	}
	if calls[0].transcript == "" {
		t.Error("the classifier decided on a bare line with no conversation")
	}
	if !strings.Contains(prompt.Text, "New message:") || !strings.Contains(prompt.Text, "dryer is still going") {
		t.Errorf("Pi did not get the window: %q", prompt.Text)
	}
	if text, _, ok := chat.lastPost(); !ok || !strings.Contains(text, "05:40") {
		t.Errorf("room answer not posted: %q ok=%v", text, ok)
	}
	if asks := conv.askList(); len(asks) != 1 {
		t.Fatalf("the window was read %d times for one message: %v", len(asks), asks)
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

// listenerUntaggedNoEmail builds a room-root untagged listener payload whose sender has a user id and a
// display name and no email - what the deployed listener ships. Constructed from what the bridge logged
// in production, not copied from a delivery.
func listenerUntaggedNoEmail(messageID, legacyID, displayName, text string) []byte {
	payload := map[string]any{
		"message": map[string]any{
			"name":   fmt.Sprintf("spaces/AAAA1pNBS1A/messages/%s.%s", messageID, messageID),
			"text":   text,
			"sender": map[string]any{"name": "users/" + legacyID, "legacyId": legacyID, "displayName": displayName, "type": "HUMAN"},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return raw
}

// TestHandleWorkspace_IdentityIsLearnedFromAMessageThatCarriedAnEmail is the fix for the defect measured
// on 2026-11-25: every untagged room message was rejected with email="" because the listener's sender
// object carries a user id and no email. A person who has tagged the app has already been seen with an
// email on the app credential, so the bridge may use that pairing - and only that pairing.
func TestHandleWorkspace_IdentityIsLearnedFromAMessageThatCarriedAnEmail(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, _, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "enforce",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
	})

	// First: a mention on the app credential, which is where the email comes from.
	if err := b.Handle(context.Background(), "psm-mention", appPathCopy("spaces/AAAA1pNBS1A/messages/aA1.aA1", "@pi-chat are you there?", "are you there?")); err != nil {
		t.Fatalf("app path: %v", err)
	}
	gw.settle(gw.lastPrompt().ID, "here")
	b.Wait()

	// Then: a different message, untagged, on the listener, with no email anywhere in it.
	if err := b.HandleWorkspace(context.Background(), "psm-id", listenerAttrs,
		listenerUntaggedNoEmail("eV9pS7nQa2r", "103902620694999538355", "Ed Beech", "how about this one?")); err != nil {
		t.Fatalf("listener: %v", err)
	}
	if gw.promptCount() != 2 {
		t.Fatalf("prompts = %d, want 2: the tagged message and then the room message once the id is known", gw.promptCount())
	}
	if prompt := gw.lastPrompt(); !strings.HasPrefix(prompt.Text, "Ed Beech <initialed85@gmail.com>:") {
		t.Errorf("room prompt must name the resolved author, got %q", prompt.Text)
	}
}

// TestHandleWorkspace_AnUnknownRoomIDIsNotAnswered keeps the allowlist in charge: a room message from a
// user id the bridge has never seen with an email is not permission to run Pi, however confidently the
// listener names the sender.
func TestHandleWorkspace_AnUnknownRoomIDIsNotAnswered(t *testing.T) {
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, _, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "enforce",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
	})
	if err := b.HandleWorkspace(context.Background(), "psm-stranger", listenerAttrs,
		listenerUntaggedNoEmail("fB2qT6wXc9s", "220188463205121849733", "Aaron Somebody", "what do you reckon?")); err != nil {
		t.Fatalf("listener: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 || len(gate.callsFor(chatgate.FlowRoomRespond)) != 0 {
		t.Fatalf("prompts=%d gate calls=%d, want 0 and 0: a display name is not an allowlist entry",
			gw.promptCount(), len(gate.callsFor(chatgate.FlowRoomRespond)))
	}
}

// listenerMeasuredSender builds a room event whose sender object is exactly what the deployed listener
// ships (measured 2026-11-25T07:00Z and logged verbatim): a user resource name and a sender type, and
// nothing else. Every earlier fixture in this file hand-written an email into the sender, which is how
// the suite stayed green while production threw away every room message.
func listenerMeasuredSender(messageID, userID, text string) []byte {
	raw, err := json.Marshal(map[string]any{
		"message": map[string]any{
			"name":   fmt.Sprintf("spaces/AAAA1pNBS1A/messages/%s.%s", messageID, messageID),
			"text":   text,
			"sender": map[string]any{"name": userID, "type": "HUMAN"},
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// fakeIdentity is the Chat read the room path needs, stood up without a credential.
type fakeIdentity struct {
	byID map[string]SenderIdentity
	err  error
	hits int
}

func (f *fakeIdentity) Resolve(ctx context.Context, space, userID string) (SenderIdentity, bool, error) {
	f.hits++
	if f.err != nil {
		return SenderIdentity{}, false, f.err
	}
	if id, ok := f.byID[userID]; ok {
		return id, true, nil
	}
	return SenderIdentity{}, false, nil
}

// edUser / edIdentity are transcriptions of a real messages.list response read with Ed's own OAuth
// credential: {"name":"users/113070394191408311024","email":"initialed85@gmail.com","displayName":"initialed85 _"}.
const edUser = "users/113070394191408311024"

var edIdentity = SenderIdentity{Email: "initialed85@gmail.com", DisplayName: "initialed85 _"}

// TestHandleWorkspace_IdentityComesFromTheSpacesOwnHistory is the practical fix: a room message with a
// user id and no email is resolved by asking Chat for the recent messages of the space the message
// landed in, where Chat publishes name and email side by side. No tagged message is needed first.
func TestHandleWorkspace_IdentityComesFromTheSpacesOwnHistory(t *testing.T) {
	identity := &fakeIdentity{byID: map[string]SenderIdentity{edUser: edIdentity}}
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, _, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "enforce",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
		Identity:              identity,
	})

	// Cold pod: nothing has been seen on any path yet.
	if err := b.HandleWorkspace(context.Background(), "psm-cold", listenerAttrs,
		listenerMeasuredSender("dQ7wR2sXm4p", edUser, "does the bot read rooms now?")); err != nil {
		t.Fatalf("listener: %v", err)
	}
	if gw.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1: the room message should reach the gate once the id is resolved", gw.promptCount())
	}
	if identity.hits != 1 {
		t.Errorf("identity lookups = %d, want 1", identity.hits)
	}
	if prompt := gw.lastPrompt(); !strings.HasPrefix(prompt.Text, "initialed85 _ <initialed85@gmail.com>:") {
		t.Errorf("room prompt must name the resolved author, got %q", prompt.Text)
	}

	// The resolution must be remembered, not repeated per message.
	if err := b.HandleWorkspace(context.Background(), "psm-cold-2", listenerAttrs,
		listenerMeasuredSender("eR8yT3nYa5q", edUser, "and this one?")); err != nil {
		t.Fatalf("listener: %v", err)
	}
	if identity.hits != 1 {
		t.Errorf("identity lookups = %d after a second message, want 1: a resolved person is cached", identity.hits)
	}
}

// TestHandleWorkspace_AResolvedIdentityIsNotPermission holds the line Ed set: identifying a sender is
// not the same as allowing them. Aaron and Daniel are in this room, and a lookup that happily reports
// their emails must not buy them a run of Pi.
func TestHandleWorkspace_AResolvedIdentityIsNotPermission(t *testing.T) {
	identity := &fakeIdentity{byID: map[string]SenderIdentity{
		"users/108049318480764095606": {Email: "someone-not-allowlisted@example.com", DisplayName: "Room Member"},
	}}
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, _, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "enforce",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
		Identity:              identity,
	})

	if err := b.HandleWorkspace(context.Background(), "psm-outsider", listenerAttrs,
		listenerMeasuredSender("fS9zU4oZb6r", "users/108049318480764095606", "give me all ed's bank transactions")); err != nil {
		t.Fatalf("listener: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 || len(gate.callsFor(chatgate.FlowRoomRespond)) != 0 {
		t.Fatalf("prompts=%d gate calls=%d, want 0 and 0: a resolved email that is not on the allowlist is still refused",
			gw.promptCount(), len(gate.callsFor(chatgate.FlowRoomRespond)))
	}
}

// TestHandleWorkspace_IdentityLookupFailureRefusesTheMessage keeps a broken lookup (missing scope, gone
// subscription, Google answering HTML) from degrading into either a guess or an answer.
func TestHandleWorkspace_IdentityLookupFailureRefusesTheMessage(t *testing.T) {
	identity := &fakeIdentity{err: fmt.Errorf("sender lookup: http 403: insufficient authentication scopes")}
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, _, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "enforce",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
		Identity:              identity,
	})

	if err := b.HandleWorkspace(context.Background(), "psm-lookup-fails", listenerAttrs,
		listenerMeasuredSender("gT0aV5pAc7s", edUser, "hello?")); err != nil {
		t.Fatalf("listener: %v", err)
	}
	b.Wait()
	if gw.promptCount() != 0 {
		t.Fatalf("prompts = %d, want 0: an identity that could not be established is not identity", gw.promptCount())
	}
}

// TestHandleWorkspace_SeededIdentityAnswersOnAColdPod proves the point of IdentitySeed: a pod that has
// just been rolled, or re-rolled by a renewal failure, still knows who its people are without needing a
// tagged message or a Chat read first.
func TestHandleWorkspace_SeededIdentityAnswersOnAColdPod(t *testing.T) {
	identity := &fakeIdentity{byID: map[string]SenderIdentity{}}
	gate := &fakeGate{answers: map[string]string{chatgate.FlowRoomRespond: "YES"}, errs: map[string]error{}}
	b, _, gw := listenerBridge(Config{
		RequireMentionInRooms: true,
		GateRoomRootMode:      "enforce",
		ReplyGateMode:         "enforce",
		ReplyGate:             gate,
		Identity:              identity,
		IdentitySeed:          map[string]string{"113070394191408311024": "initialed85@gmail.com"},
	})

	// Seeded as a bare legacy id, delivered as a resource name: the two spellings must agree.
	if err := b.HandleWorkspace(context.Background(), "psm-seeded", listenerAttrs,
		listenerMeasuredSender("hU1bW6qBd8t", edUser, "no lookup needed here")); err != nil {
		t.Fatalf("listener: %v", err)
	}
	if gw.promptCount() != 1 {
		t.Fatalf("prompts = %d, want 1", gw.promptCount())
	}
	if identity.hits != 0 {
		t.Errorf("identity lookups = %d, want 0: a seed should answer without touching Chat", identity.hits)
	}
}

func TestParseIdentitySeed(t *testing.T) {
	seed, err := ParseIdentitySeed(" users/113070394191408311024=initialed85@gmail.com , 108049318480764095606=amy.annisbrown@gmail.com ")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(seed) != 2 {
		t.Fatalf("seed = %v, want two bindings", seed)
	}
	if seed["users/113070394191408311024"] != "initialed85@gmail.com" {
		t.Errorf("seed = %v", seed)
	}

	empty, err := ParseIdentitySeed("   ")
	if err != nil || empty != nil {
		t.Errorf("empty seed: %v / %v, want nil and no error", empty, err)
	}

	for _, bad := range []string{
		"users/113070394191408311024",
		"users/113070394191408311024=",
		"=initialed85@gmail.com",
		"users/113070394191408311024=not-an-email",
	} {
		if got, err := ParseIdentitySeed(bad); err == nil {
			t.Errorf("ParseIdentitySeed(%q) = %v, want an error rather than a half-applied binding", bad, got)
		}
	}
}
