package chatmsg

import (
	"os"
	"path/filepath"
	"testing"
)

// The fixtures under testdata/ are real Pub/Sub payloads from the spike, with emails, display
// names, avatars, config tokens and attachment URLs replaced by example.invalid values. Structure
// is untouched, which is the whole point: this is what Google actually sends an add-on app.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParse_DMMessage(t *testing.T) {
	ev, err := Parse(fixture(t, "dm_message.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Kind != KindMessage {
		t.Errorf("Kind = %q, want message", ev.Kind)
	}
	if got, want := ev.Space, "spaces/m4d90aAAAAE"; got != want {
		t.Errorf("Space = %q, want %q", got, want)
	}
	if got, want := ev.Thread, "spaces/m4d90aAAAAE/threads/9yxcykCFS-k"; got != want {
		t.Errorf("Thread = %q, want %q", got, want)
	}
	if got, want := ev.UserEmail, "owner@example.com"; got != want {
		t.Errorf("UserEmail = %q, want %q", got, want)
	}
	if got, want := ev.Prompt(), "g'day"; got != want {
		t.Errorf("Prompt() = %q, want %q", got, want)
	}
	if ev.Mentioned {
		t.Errorf("Mentioned = true in a DM with no annotation")
	}
	if ev.Slash {
		t.Errorf("Slash = true for %q", ev.Prompt())
	}
	if !ev.DM {
		t.Errorf("DM = false for a singleUserBotDm space")
	}
	if got, want := ev.TimeZoneName, "Australia/Perth"; got != want {
		t.Errorf("TimeZoneName = %q, want %q", got, want)
	}
	if ev.EventTime.IsZero() {
		t.Errorf("EventTime is zero")
	}
}

func TestParse_RoomMention(t *testing.T) {
	ev, err := Parse(fixture(t, "room_mention.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Kind != KindMessage {
		t.Errorf("Kind = %q, want message", ev.Kind)
	}
	if !ev.Mentioned {
		t.Errorf("Mentioned = false, want true for an @pi-chat mention")
	}
	if got, want := ev.Prompt(), "hey what's up"; got != want {
		t.Errorf("Prompt() = %q, want %q (mention must be stripped)", got, want)
	}
	if got, want := ev.UserEmail, "member@example.com"; got != want {
		t.Errorf("UserEmail = %q, want %q", got, want)
	}
	if got, want := ev.Space, "spaces/qi7VbgAAAAE"; got != want {
		t.Errorf("Space = %q, want %q", got, want)
	}
	if ev.DM {
		t.Errorf("DM = true for a 2-human room")
	}
}

func TestParse_ImageAttachment(t *testing.T) {
	ev, err := Parse(fixture(t, "image_message.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ev.Attachments) != 1 {
		t.Fatalf("len(Attachments) = %d, want 1", len(ev.Attachments))
	}
	a := ev.Attachments[0]
	if a.ContentType != "image/png" {
		t.Errorf("ContentType = %q, want image/png", a.ContentType)
	}
	if a.ContentName != "image.png" {
		t.Errorf("ContentName = %q, want image.png", a.ContentName)
	}
	if a.ResourceRef == "" || a.Name == "" {
		t.Errorf("ResourceRef/Name must both be present, got %q / %q", a.ResourceRef, a.Name)
	}
	if ev.Prompt() != "" {
		t.Errorf("Prompt() = %q, want empty for an image-only message", ev.Prompt())
	}
}

func TestParse_AddedToSpace(t *testing.T) {
	ev, err := Parse(fixture(t, "dm_added.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Kind != KindAddedToSpace {
		t.Errorf("Kind = %q, want added_to_space", ev.Kind)
	}
	if got, want := ev.Space, "spaces/m4d90aAAAAE"; got != want {
		t.Errorf("Space = %q, want %q", got, want)
	}
	if ev.MessageName != "" {
		t.Errorf("MessageName = %q, want empty", ev.MessageName)
	}
}

// A slash command typed as "@pi-chat /status" arrives as ordinary text; the bridge must still see
// it as a command because Chat does not run our appCommands for add-on apps.
func TestParse_SlashFromArgument(t *testing.T) {
	raw := `{
      "chat": {
        "eventTime": "2026-11-20T11:00:00.000Z",
        "user": {"name": "users/1", "email": "owner@example.com", "displayName": "Owner"},
        "messagePayload": {"message": {
          "name": "spaces/s1/messages/m1.m1",
          "thread": {"name": "spaces/s1/threads/m1"},
          "text": "@pi-chat /model rooter/beamster",
          "argumentText": " /model rooter/beamster",
          "sender": {"email": "owner@example.com"},
          "annotations": [{"type": "USER_MENTION"}]
        }, "space": {"name": "spaces/s1", "spaceType": "ROOM"}}
      }}`
	ev, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.Slash {
		t.Fatalf("Slash = false, want true for %q", ev.Argument)
	}
	if ev.Command != "/model" {
		t.Errorf("Command = %q, want /model", ev.Command)
	}
	if ev.CommandArgs != "rooter/beamster" {
		t.Errorf("CommandArgs = %q, want %q", ev.CommandArgs, "rooter/beamster")
	}
}

// The flat documented shape, in case the app is ever recreated under a Workspace domain.
func TestParse_FlatShape(t *testing.T) {
	raw := `{
      "type": "MESSAGE",
      "eventTime": "2026-11-20T11:00:00.000Z",
      "message": {
        "name": "spaces/AAAAB/messages/xxxx.yyyy",
        "text": "hello flat",
        "argumentText": "hello flat",
        "sender": {"email": "owner@example.com", "displayName": "Owner"}
      },
      "space": {"name": "spaces/AAAAB", "spaceType": "DIRECT_MESSAGE", "singleUserBotDm": true},
      "user": {"email": "owner@example.com"}
    }`
	ev, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Kind != KindMessage {
		t.Errorf("Kind = %q, want message", ev.Kind)
	}
	if ev.Prompt() != "hello flat" {
		t.Errorf("Prompt() = %q", ev.Prompt())
	}
	if ev.Space != "spaces/AAAAB" || ev.Thread != "spaces/AAAAB/threads/yyyy" {
		t.Errorf("Space/Thread = %q / %q", ev.Space, ev.Thread)
	}
	if !ev.DM {
		t.Errorf("DM = false, want true")
	}
}

func TestParse_MessageWithoutThreadDerivesThread(t *testing.T) {
	raw := `{"chat":{"user":{"email":"a@b.c"},"messagePayload":{"message":{
        "name":"spaces/s/messages/payloadname.payloadname","text":"hi","argumentText":"hi"}}}}`
	ev, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Thread != "spaces/s/threads/payloadname" {
		t.Errorf("Thread = %q, want the thread id taken from the message name suffix", ev.Thread)
	}
	if ev.Space != "spaces/s" {
		t.Errorf("Space = %q, want spaces/s derived from the message name", ev.Space)
	}
}

func TestParse_Garbage(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Errorf("expected an error on malformed JSON")
	}
	ev, err := Parse([]byte(`{"somethingElse":1}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Kind != KindUnknown {
		t.Errorf("Kind = %q, want unknown", ev.Kind)
	}
}

// Chat normally reports the same person twice (chat.user and message.sender). When they disagree we
// must know, because the bridge gates on identity.
func TestParse_IdentityMismatch(t *testing.T) {
	raw := `{"chat":{"user":{"email":"owner@example.com","name":"users/1"},
        "messagePayload":{"message":{"name":"spaces/s/messages/m.m","text":"hi",
        "argumentText":"hi","sender":{"email":"someoneelse@example.com"}}}}}`
	ev, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ev.IdentityMismatch {
		t.Errorf("IdentityMismatch = false, want true")
	}
	if ev.UserEmail != "someoneelse@example.com" {
		t.Errorf("UserEmail = %q, want the message author", ev.UserEmail)
	}
}

// In the wrapped add-on shape the sender usually has no email at all; the envelope user is then the
// only identity we have, and that must not read as a mismatch.
func TestParse_SenderWithoutEmailUsesEnvelope(t *testing.T) {
	raw := `{"chat":{"user":{"email":"owner@example.com","displayName":"Owner","name":"users/9"},
        "messagePayload":{"message":{"name":"spaces/s/messages/m.m","text":"hi","argumentText":"hi",
        "sender":{"name":"users/9","type":"HUMAN"}}}}}`
	ev, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.IdentityMismatch {
		t.Errorf("IdentityMismatch = true, want false when the sender carries no email")
	}
	if ev.UserEmail != "owner@example.com" || ev.UserDisplayName != "Owner" {
		t.Errorf("identity = %q / %q", ev.UserEmail, ev.UserDisplayName)
	}
}
