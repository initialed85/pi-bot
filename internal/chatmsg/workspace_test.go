package chatmsg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Workspace fixtures are reconstructed from the measured envelope attributes and the Chat Message
// resource shape, not copied out of a live delivery: the subscription was drained before the Go
// consumer existed. The behaviour they encode (sender.type present, message names carrying thread
// position, ce-subject carrying the resource) is measured; the exact byte layout is the part the first
// live capture must reconcile. ParseWorkspace is deliberately tolerant of both body shapes for that
// reason, and a fixture captured from a real event should be dropped in beside these and asserted
// against the same expectations.

func workspaceFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("fixture %s is not valid JSON", name)
	}
	return raw
}

var workspaceAttrs = map[string]string{
	"ce-type":        WorkspaceMessageCreated,
	"ce-specversion": "1.0",
	"ce-source":      "//chat.googleapis.com/spaces/-",
	"ce-time":        "2026-11-25T02:15:38Z",
	"ce-id":          "103902620694999538355",
}

func TestParseWorkspace_RoomUntaggedAtThreadRoot(t *testing.T) {
	ev, err := ParseWorkspace(workspaceFixture(t, "workspace_room_untagged.json"), workspaceAttrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Kind != KindMessage || ev.Source != "workspace" {
		t.Fatalf("kind=%q source=%q", ev.Kind, ev.Source)
	}
	if ev.MessageName != "spaces/AAAA1pNBS1A/messages/kQ7mZ2pXr1o.kQ7mZ2pXr1o" {
		t.Errorf("message name = %q", ev.MessageName)
	}
	if ev.Space != "spaces/AAAA1pNBS1A" {
		t.Errorf("space = %q", ev.Space)
	}
	if ev.UserEmail != "initialed85@gmail.com" {
		t.Errorf("sender email = %q", ev.UserEmail)
	}
	if ev.SenderType != "HUMAN" {
		t.Errorf("sender type = %q, want HUMAN (the loop guard depends on this field arriving)", ev.SenderType)
	}
	if !ev.HumanSender() {
		t.Error("human sender read as a bot")
	}
	if ev.Mentioned {
		t.Error("untagged message parsed as mentioned")
	}
	if ev.Prompt() != "the garage door has been open since this morning" {
		t.Errorf("prompt = %q", ev.Prompt())
	}
	// A room root message gets a synthetic thread named after itself, which the bridge must not mistake
	// for "this is a reply inside an existing thread".
	if ev.Thread != "spaces/AAAA1pNBS1A/threads/kQ7mZ2pXr1o" {
		t.Errorf("thread = %q", ev.Thread)
	}
	if ev.DM {
		t.Error("a ROOM space parsed as a DM")
	}
}

func TestParseWorkspace_ReplyInsideAnExistingThread(t *testing.T) {
	ev, err := ParseWorkspace(workspaceFixture(t, "workspace_room_thread_reply.json"), workspaceAttrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Thread != "spaces/AAAA1pNBS1A/threads/xK9vB2cDd3e" {
		t.Fatalf("thread = %q, want the existing thread the reply landed in", ev.Thread)
	}
	if ev.Space != "spaces/AAAA1pNBS1A" {
		t.Errorf("space = %q", ev.Space)
	}
	if ev.UserEmail != "amy.annisbrown@gmail.com" {
		t.Errorf("sender email = %q", ev.UserEmail)
	}
}

func TestParseWorkspace_BotMessageIsNotHuman(t *testing.T) {
	ev, err := ParseWorkspace(workspaceFixture(t, "workspace_room_bot.json"), workspaceAttrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.HumanSender() {
		t.Fatalf("BOT sender accepted as human (sender type %q) - the loop guard would answer the bot's own reply", ev.SenderType)
	}
}

func TestParseWorkspace_AppGeneratedCardIsNotHuman(t *testing.T) {
	body := []byte(`{"message":{"name":"spaces/AAAA1pNBS1A/messages/a1.b2","text":"updated card","sender":{"name":"users/1","type":"APP-GENERATED"}}}`)
	ev, err := ParseWorkspace(body, workspaceAttrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.HumanSender() {
		t.Error("APP-GENERATED sender accepted as human")
	}
}

func TestParseWorkspace_MentionCarriesAnnotations(t *testing.T) {
	ev, err := ParseWorkspace(workspaceFixture(t, "workspace_room_mention.json"), workspaceAttrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.Mentioned {
		t.Fatal("mention annotation lost")
	}
	// The listener hands mentions back to the app path, so the mention-stripped remainder still matters:
	// whatever does the answering should not be handed the "@pi-chat" prefix.
	if ev.Prompt() != "what did the garage log say overnight" {
		t.Errorf("prompt = %q", ev.Prompt())
	}
}

func TestParseWorkspace_SubjectFillsAMissingName(t *testing.T) {
	attrs := map[string]string{
		"ce-type":    WorkspaceMessageCreated,
		"ce-subject": "//chat.googleapis.com/spaces/AAAA1pNBS1A/messages/zZ9yX8wV7u.zZ9yX8wV7u",
	}
	ev, err := ParseWorkspace(workspaceFixture(t, "workspace_subject_only.json"), attrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.MessageName != "spaces/AAAA1pNBS1A/messages/zZ9yX8wV7u.zZ9yX8wV7u" {
		t.Fatalf("message name = %q, want the name taken from ce-subject", ev.MessageName)
	}
	if ev.Space != "spaces/AAAA1pNBS1A" {
		t.Errorf("space = %q", ev.Space)
	}
	// No type in this payload: an absent sender type must not be read as "a bot wrote this", or a
	// listener that omits the field would be silently deaf.
	if !ev.HumanSender() {
		t.Error("absent sender type treated as a bot")
	}
}

func TestParseWorkspace_BareResourceShape(t *testing.T) {
	// The other shape Workspace has shipped: the resource fields at the top level.
	body := []byte(`{"name":"spaces/AAAA1pNBS1A/messages/c3.d4","text":"boot is in the hallway","sender":{"email":"initialed85@gmail.com","type":"HUMAN"}}`)
	ev, err := ParseWorkspace(body, workspaceAttrs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.MessageName != "spaces/AAAA1pNBS1A/messages/c3.d4" || ev.Prompt() != "boot is in the hallway" {
		t.Fatalf("bare resource not parsed: %+v", ev)
	}
}

func TestParseWorkspace_RejectsAnEventWithNothingInIt(t *testing.T) {
	if _, err := ParseWorkspace([]byte(`{"message":{"text":"no name anywhere"}}`), map[string]string{"ce-type": WorkspaceMessageCreated}); err == nil {
		t.Fatal("expected an error when neither the body nor the attributes identify the message")
	}
	if _, err := ParseWorkspace([]byte(`not json`), workspaceAttrs); err == nil {
		t.Fatal("expected an error on unparseable JSON")
	}
}
