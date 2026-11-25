// workspace.go parses the other kind of Chat event the bridge consumes: a Workspace Events
// subscription created with a human OAuth token, which sees every message in every space that human
// belongs to - including the ones that never mention the app, which is the whole reason this path
// exists.
//
// The shape is not the app's shape. A Chat app pushes a wrapped `chat.messagePayload`; Workspace
// Events pushes CloudEvents 1.0, where the ce-* attributes ride on the Pub/Sub envelope and the
// body carries just the fields the subscription's fieldMask asked for:
//
//	attributes: {"ce-type": "google.workspace.chat.message.v1.created",
//	           "ce-subject": "//chat.googleapis.com/spaces/AAAA1pNBS1A/messages/xyz.xyz", ...}
//	body:      {"message": {"name": ..., "text": ..., "sender": {...}, "annotations": [...]}}
//
// Measured on 2026-11-24/25 against a consumer @gmail.com space, and the parts that matter:
//   - sender.type is present and is "HUMAN" for people, "BOT" for the app's own replies, which is
//     what makes a loop guard possible at all.
//   - the fieldMask is fixed at creation, and validateOnly does NOT validate it (a mask naming a
//     field that does not exist returns 200), so a mask is only proven by what actually arrives.
//   - a top-level message and a reply inside a thread are distinguished by the message name suffix
//     (`<id>.<id>` versus `<id>.<threadId>`), exactly as in the app payload.
package chatmsg

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WorkspaceMessageCreated is the only Workspace Events Chat event type the bridge consumes.
const WorkspaceMessageCreated = "google.workspace.chat.message.v1.created"

// workspaceBody is the decoded Pub/Sub data of a Workspace Events Chat event.
//
// Two shapes are accepted. With includeResource and a message.* fieldMask the resource arrives under
// "message"; some Workspace payloads put the resource fields at the top level instead, and one of
// them will eventually be the one Google ships on a Tuesday, so both are parsed rather than guessed.
type workspaceBody struct {
	Message *wireMessage `json:"message"`
	Space   *wireSpace   `json:"space"`

	// Bare resource shape: the Message fields sit at the top level.
	wireMessage
}

// ParseWorkspace normalises one Workspace Events Chat message-created event into the same Event the
// rest of the bridge routes on, so a room message that arrived because a human authorised a listener
// is treated exactly like a room message that arrived because it tagged the app.
//
// attrs are the Pub/Sub message attributes (the CloudEvents ce-* values); they fill in anything the
// fieldMask left out, most usefully the message name when the body has none.
func ParseWorkspace(data []byte, attrs map[string]string) (*Event, error) {
	var body workspaceBody
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("decode workspace chat event: %w", err)
	}

	ev := &Event{Raw: json.RawMessage(data), Source: "workspace", Kind: KindMessage}
	ev.SenderType = strings.TrimSpace(attrs["ce-sender-type"])

	msg := body.Message
	if msg == nil && (body.Name != "" || body.Text != "") {
		// Bare resource: the embedded wireMessage picked the fields up.
		shallow := body.wireMessage
		msg = &shallow
	}
	if msg != nil {
		applyMessage(ev, msg)
	}
	if body.Space != nil {
		applySpace(ev, body.Space)
	}
	if msg != nil && msg.Space != nil {
		applySpace(ev, msg.Space)
	}

	if ev.MessageName == "" {
		ev.MessageName = workspaceResource(attrs["ce-subject"])
	}
	if ev.EventTime.IsZero() {
		ev.EventTime = parseTime(firstNonEmpty(attrs["ce-time"], attrs["publishTime"]))
	}
	if ev.MessageName == "" && ev.Space == "" {
		return nil, fmt.Errorf("workspace chat event has neither a message name nor a space (ce-subject=%q)", attrs["ce-subject"])
	}
	if ev.Type == "" {
		ev.Type = attrs["ce-type"]
	}
	finish(ev)
	return ev, nil
}

// workspaceResource strips the CloudEvents authority prefix, which is not a Chat resource name:
// "//chat.googleapis.com/spaces/AAAA1pNBS1A/messages/xyz.xyz" is the same resource Chat would hand
// back as "spaces/AAAA1pNBS1A/messages/xyz.xyz".
func workspaceResource(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(subject, "//chat.googleapis.com/"); ok {
		return rest
	}
	// Some Workspace payloads use "//.googleapis.com/…" or a bare https form.
	if rest, ok := strings.CutPrefix(subject, "//.googleapis.com/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(subject, "https://chat.googleapis.com/v1/"); ok {
		return rest
	}
	return subject
}

// HumanSender reports whether this event was written by a person. The sender type is the only thing
// standing between a room listener and an infinite conversation the bot has with itself, so the
// comparison is deliberately explicit about which values mean "a bot did this".
func (e *Event) HumanSender() bool {
	switch strings.ToUpper(strings.TrimSpace(e.SenderType)) {
	case "", "TYPE_UNSPECIFIED":
		// An absent type is not evidence of a bot. The app payload has never carried one and the
		// room listener does, so refusing here would silence the listener rather than guard it.
		return true
	case "HUMAN", "PERSON", "MEMBER":
		return true
	default:
		// BOT and APP-GENERATED are the two Google ships for bot-authored messages.
		return false
	}
}
