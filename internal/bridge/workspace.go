// workspace.go is the consumer side of the user-authorised room listener.
//
// Why a second path exists: the Chat app's own credential is only told about messages that tag it,
// plus everything in a 1:1 with it. Ordinary conversation in a group room is invisible to it. A
// Workspace Events subscription created with a human OAuth token sees every message in every space
// that human belongs to, and pushes it to a Pub/Sub topic the bridge can read. That is the difference
// between "pi-chat answers when tagged" and "pi-chat answers when it is being talked at".
//
// Three rules make the second path safe to run beside the first, and all three are measured rather
// than assumed (internal/bridge/room-ingestion.md has the evidence):
//
//  1. Loop guard. The listener delivers the bot's own replies, sender.type BOT. Without a guard the
//     bridge answers itself, forever.
//  2. Ownership. Mentions and 1:1 messages already arrive on the app path with the envelope identity,
//     slash commands and card payloads. The listener copy is strictly worse information, so it is
//     handed back to the app path *before* it consumes the dedupe key - consuming it first would make
//     the good copy look like a duplicate and silently drop a message the app would have answered.
//  3. Dedupe. A tagged message really does arrive twice, on both topics. The same bounded seen set
//     that absorbs Pub/Sub redeliveries absorbs the cross-path duplicate, because both paths key on
//     the Chat message name.
package bridge

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/initialed85/pi-bot/internal/chatapi"
	"github.com/initialed85/pi-bot/internal/chatmsg"
)

// HandleWorkspace processes one Pub/Sub message from the room listener. attrs are the Pub/Sub message
// attributes, which is where CloudEvents puts ce-type and ce-subject; data is the event body.
//
// Like Handle, it returns as soon as the work is dispatched, and an error means "could not parse this"
// - the caller logs it and acks, because redelivery will not make it parseable.
func (b *Bridge) HandleWorkspace(ctx context.Context, id string, attrs map[string]string, data []byte) error {
	ev, err := chatmsg.ParseWorkspace(data, attrs)
	if err != nil {
		return err
	}
	if ev.Kind != chatmsg.KindMessage {
		b.log.Info("workspace event is not a message", "type", ev.Type, "space", ev.Space)
		return nil
	}

	// Rule 1: the loop guard. Google ships exactly two non-human sender types (BOT for a bot's own
	// messages, APP-GENERATED for a card a bot updated), and a room listener receives both, including
	// the ones this bridge just posted.
	if !ev.HumanSender() {
		b.log.Info("loop guard: room listener delivered a non-human message",
			"sender_type", ev.SenderType, "space", ev.Space, "message", shortID(ev.MessageName))
		return nil
	}

	// Rule 2: ownership, checked before dedupe on purpose (see the package comment).
	if ev.Mentioned {
		b.log.Info("mentioned message left to the app path",
			"space", ev.Space, "thread", ev.Thread, "message", shortID(ev.MessageName))
		return nil
	}
	if info := b.spaceInfo(ctx, ev.Space); info.DM() {
		b.log.Info("1:1 message left to the app path",
			"space", ev.Space, "message", shortID(ev.MessageName), "space_type", info.SpaceType)
		return nil
	}

	// Rule 3: dedupe, against every path the bridge has.
	dedupe := ev.MessageName
	if dedupe == "" {
		dedupe = id
	}
	if !b.markSeen(dedupe) {
		b.log.Info("dup", "id", id, "message", dedupe, "path", "workspace")
		return nil
	}

	// From here the message is the listener's alone: nobody tags the app in an ordinary room sentence,
	// so this is the only copy pi-chat will ever see of it. Everything after this point is the same
	// policy the gated app path runs - allowlist, room eligibility, the room classifier.
	// Rule 4: identity. The room listener's sender object has no email, and the allowlist is written in
	// emails, so a room message is only actionable if this person has been identified earlier by a
	// message that carried one (a mention or a DM on the app credential).
	if key, source, ok := b.resolveRoomIdentity(ctx, ev); ok {
		b.log.Info("room sender identified", "id", key, "via", source, "user", ev.UserEmail, "space", ev.Space)
	}

	b.log.Info("room event",
		"path", "workspace", "space", ev.Space, "thread", ev.Thread,
		"user", ev.UserEmail, "sender_type", ev.SenderType,
		"position", messagePosition(ev),
		"text", truncate(ev.Prompt(), 90), "atts", len(ev.Attachments))
	if ev.UserEmail == "" {
		// Log exactly what Google sent rather than guessing at a schema, including the id that the lookup
		// tried. This line is what turns "the listener has no identity" from an interpretation into a
		// record, and it is the line that proved the sender object is {"name":"users/<id>","type":"HUMAN"}.
		b.log.Warn("room message carries no identity the allowlist can use",
			"space", ev.Space, "message", shortID(ev.MessageName),
			"user_name", ev.UserID, "legacy_id", ev.UserLegacyID,
			"display_name", ev.UserDisplayName, "sender_json", truncate(compactJSON(ev.SenderRaw), 240))
	}
	return b.onMessage(ctx, ev)
}

// compactJSON flattens a raw JSON value onto one line for a log field, so the sender object a room
// event carried is readable in one line instead of an unreadable escaped blob.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	out, err := json.Marshal(v)
	if err != nil {
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	return string(out)
}

// spaceInfo caches one Chat space so the routing question "is this a 1:1 or a room" is answered by
// Chat rather than by whatever the subscription's fieldMask happened to include. A space's type does
// not change, so the cache never needs invalidating; a failed lookup is not cached, because the most
// likely cause is a transient API error and the next message should get another chance.
func (b *Bridge) spaceInfo(ctx context.Context, space string) *chatapi.SpaceInfo {
	if space == "" {
		return nil
	}
	b.mu.Lock()
	if info, ok := b.spaces[space]; ok {
		b.mu.Unlock()
		return info
	}
	b.mu.Unlock()

	info, err := b.chat.GetSpace(ctx, space)
	if err != nil {
		b.log.Warn("space lookup failed; treating the space as a room", "space", space, "err", err)
		return nil
	}
	b.mu.Lock()
	if b.spaces == nil {
		b.spaces = map[string]*chatapi.SpaceInfo{}
	}
	b.spaces[space] = info
	b.mu.Unlock()
	return info
}

// messagePosition names where a message sits, because the routing policy is written in those words:
// Chat's message name ends "<id>.<id>" for a message at the thread root and "<id>.<threadId>" for a
// reply inside an existing thread.
func messagePosition(ev *chatmsg.Event) string {
	if isTopLevel(ev) {
		if ev.Mentioned {
			return "room root, tagged"
		}
		return "room root, untagged"
	}
	return "inside thread"
}
