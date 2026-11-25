// Package chatmsg parses the JSON that Google Chat publishes to Cloud Pub/Sub for our app.
//
// The shape matters more than the docs suggest. Documented examples show a flat `Event`
// (`{"type": "MESSAGE", "message": {...}}`), but an app created on a personal Gmail account is
// forced into "Workspace add-on" mode, and add-on events arrive wrapped:
//
//	{"chat": {"eventTime": ..., "user": {...}, "messagePayload": {"message": {...}, "space": {...}}},
//	 "commonEventObject": {...}}
//
// with no top-level `type` at all: the payload is selected by which key is present
// (messagePayload, addedToSpacePayload, removedFromSpacePayload, appCommandPayload,
// cardClickPayload). Both shapes are parsed here so the bridge keeps working if the app is
// recreated under a Workspace domain, and the tests cover both.
package chatmsg

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Kind is what happened, normalised across the wrapped and flat shapes.
type Kind string

const (
	KindMessage        Kind = "message"
	KindAddedToSpace   Kind = "added_to_space"
	KindRemovedToSpace Kind = "removed_from_space"
	KindAppCommand     Kind = "app_command"
	KindCardClick      Kind = "card_click"
	KindUnknown        Kind = "unknown"
)

// Attachment is one uploaded file on a message. ResourceRef is the base64
// `attachmentDataRef.resourceName`, which is what the media download endpoint wants;
// DownloadURI is deliberately not trusted (see FINDINGS: it serves the Chat web app).
type Attachment struct {
	Name        string
	ResourceRef string
	ContentName string
	ContentType string
	Source      string
}

// Event is the normalised view of one Chat event.
type Event struct {
	Kind Kind

	EventTime time.Time

	Space     string // "spaces/…"
	Thread    string // "spaces/…/threads/…"
	SpaceType string
	DM        bool

	MessageName string // "spaces/…/messages/…"
	Text        string // as the sender wrote it, including any "@pi-chat " prefix
	Argument    string // Chat's mention-stripped remainder
	Silent      bool

	// Identity used for allowlisting. Chat gives us two: `chat.user` (who triggered the
	// interaction) and `message.sender` (who wrote the text). They agree for real messages, but the
	// sender is the one that authored the prompt, so it wins when it carries an email.
	UserID          string
	UserEmail       string
	UserDisplayName string
	// UserLegacyID is Chat's stable numeric user id (`UserProfile.legacyId`). It is the only identity a
	// Workspace Events room listener reliably carries: the human-shaped UserProfile it ships has a legacy
	// id and a display name and no email, while the app credential ships an email. The bridge uses one to
	// name the other - see Bridge.bindIdentity.
	UserLegacyID string
	// SenderRaw is the sender object exactly as it arrived, kept for diagnosis. When identity resolution
	// fails this is the only record of what Google actually sent, which beats arguing about the schema.
	SenderRaw json.RawMessage
	// IdentityMismatch flags the two disagreeing, which the bridge treats as a hard no: a bot that
	// runs shell commands should not guess whose permissions apply.
	IdentityMismatch bool

	Mentioned   bool // an annotation says a bot/user was mentioned
	Slash       bool // Argument begins with "/"
	Command     string
	CommandArgs string

	// Source records which path delivered this event: "app" for the Chat app's own credential (only
	// messages that tag the app, plus everything in a 1:1 with it) or "workspace" for the
	// user-authorised room listener (everything the authorising human can see). Routing depends on it.
	Source string
	// SenderType is Google's own answer to "who wrote this": HUMAN, BOT or APP-GENERATED. Only the
	// room listener carries it, and it is the loop guard for a listener that also receives the bot's
	// replies.
	SenderType string
	// Type is the raw event type when one exists (the CloudEvents ce-type on the room listener).
	Type string

	Attachments  []Attachment
	Locale       string
	TimeZoneName string

	Raw json.RawMessage
}

// wire types: only the fields we use. Everything else is dropped by encoding/json.
type wireEnvelope struct {
	Type      string          `json:"type"`
	EventTime string          `json:"eventTime"`
	User      *wireUser       `json:"user"`
	Message   *wireMessage    `json:"message"`
	Space     *wireSpace      `json:"space"`
	Chat      json.RawMessage `json:"chat"`
	Common    *wireCommon     `json:"commonEventObject"`
	Card      json.RawMessage `json:"cardClickPayload"`
}

type wireAddon struct {
	EventTime               string       `json:"eventTime"`
	User                    *wireUser    `json:"user"`
	MessagePayload          *wirePayload `json:"messagePayload"`
	AddedToSpacePayload     *wirePayload `json:"addedToSpacePayload"`
	RemovedFromSpacePayload *wirePayload `json:"removedFromSpacePayload"`
	AppCommandPayload       *wirePayload `json:"appCommandPayload"`
	CardClickPayload        *wirePayload `json:"cardClickPayload"`
}

type wirePayload struct {
	Message    *wireMessage `json:"message"`
	Space      *wireSpace   `json:"space"`
	User       *wireUser    `json:"user"`
	CreateTime string       `json:"createTime"`
	// appCommandPayload carries {"input": "/thing arg"}; cardClickPayload carries {"action": {...}}.
	Input  string          `json:"input"`
	Action json.RawMessage `json:"action"`
}

type wireCommon struct {
	HostApp  string `json:"hostApp"`
	Platform string `json:"platform"`
	Locale   string `json:"userLocale"`
	TZ       struct {
		ID string `json:"id"`
	} `json:"timeZone"`
}

type wireUser struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Type        string `json:"type"`
	LegacyID    string `json:"legacyId"`
}

type wireSpace struct {
	Name          string `json:"name"`
	SpaceType     string `json:"spaceType"`
	Type          string `json:"type"`
	SingleUserBot bool   `json:"singleUserBotDm"`
	SpaceURI      string `json:"spaceUri"`
	Members       struct {
		JoinedDirectHumanUserCount int `json:"joinedDirectHumanUserCount"`
	} `json:"membershipCount"`
}

type wireMessage struct {
	Name   string      `json:"name"`
	Thread *wireThread `json:"thread"`
	// Sender is kept raw on purpose. Chat fills it differently on the two transports - the app puts an
	// email in it, the room listener puts a legacyId and a displayName - and json cannot decode one key
	// into two struct fields (it decodes it into neither). Decoding it by hand keeps the typed view and
	// the original bytes, which is what identity resolution needs.
	Sender       json.RawMessage `json:"sender"`
	Space        *wireSpace      `json:"space"`
	Text         string          `json:"text"`
	ArgumentText string          `json:"argumentText"`
	Annotations  []wireAnnotation
	Attachment   []wireAttachment
	SlashCommand json.RawMessage `json:"slashCommand"`
	Silent       bool            `json:"silent"`
	CreateTime   string          `json:"createTime"`
}

type wireThread struct {
	Name string `json:"name"`
}

type wireAnnotation struct {
	Type        string `json:"type"`
	UserMention struct {
		Type string    `json:"type"`
		User *wireUser `json:"user"`
	} `json:"userMention"`
}

type wireAttachment struct {
	Name              string `json:"name"`
	AttachmentDataRef struct {
		ResourceName string `json:"resourceName"`
	} `json:"attachmentDataRef"`
	ContentName string `json:"contentName"`
	ContentType string `json:"contentType"`
	Source      string `json:"source"`
}

// Parse turns one Pub/Sub message data field into an Event. It errors only on unparseable JSON;
// an unknown payload kind is returned as KindUnknown so the caller can log and ack rather than
// redelivering forever.
func Parse(data []byte) (*Event, error) {
	var env wireEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode chat event: %w", err)
	}

	ev := &Event{Raw: json.RawMessage(data), Source: "app", Type: env.Type}
	if env.Common != nil {
		ev.Locale = env.Common.Locale
		ev.TimeZoneName = env.Common.TZ.ID
	}

	// Wrapped add-on shape: the interesting data is under chat.<something>Payload.
	if len(env.Chat) > 0 {
		var addon wireAddon
		if err := json.Unmarshal(env.Chat, &addon); err != nil {
			return nil, fmt.Errorf("decode add-on chat: %w", err)
		}
		ev.EventTime = parseTime(firstNonEmpty(addon.EventTime, env.EventTime))
		applyUser(ev, addon.User)
		switch {
		case addon.MessagePayload != nil:
			ev.Kind = KindMessage
			applyPayload(ev, addon.MessagePayload)
		case addon.AddedToSpacePayload != nil:
			ev.Kind = KindAddedToSpace
			applyPayload(ev, addon.AddedToSpacePayload)
		case addon.RemovedFromSpacePayload != nil:
			ev.Kind = KindRemovedToSpace
			applyPayload(ev, addon.RemovedFromSpacePayload)
		case addon.AppCommandPayload != nil:
			ev.Kind = KindAppCommand
			applyPayload(ev, addon.AppCommandPayload)
			if addon.AppCommandPayload.Input != "" {
				setSlash(ev, addon.AppCommandPayload.Input)
			}
		case addon.CardClickPayload != nil:
			ev.Kind = KindCardClick
			applyPayload(ev, addon.CardClickPayload)
		default:
			ev.Kind = KindUnknown
		}
		finish(ev)
		return ev, nil
	}

	// Flat (documented) shape.
	ev.EventTime = parseTime(env.EventTime)
	applyUser(ev, env.User)
	switch strings.ToUpper(env.Type) {
	case "MESSAGE":
		ev.Kind = KindMessage
	case "ADDED_TO_SPACE":
		ev.Kind = KindAddedToSpace
	case "REMOVED_FROM_SPACE":
		ev.Kind = KindRemovedToSpace
	case "CARD_CLICKED":
		ev.Kind = KindCardClick
	default:
		ev.Kind = KindUnknown
	}
	if env.Message != nil {
		applyMessage(ev, env.Message)
	} else if env.Chat == nil && env.Type == "" {
		// Neither shape: treat as unknown rather than guessing.
		ev.Kind = KindUnknown
	}
	if env.Space != nil {
		applySpace(ev, env.Space)
	}
	finish(ev)
	return ev, nil
}

func applyUser(ev *Event, u *wireUser) {
	if u == nil {
		return
	}
	ev.UserID = u.Name
	ev.UserEmail = u.Email
	ev.UserDisplayName = u.DisplayName
	ev.UserLegacyID = u.LegacyID
	if ev.SenderType == "" {
		ev.SenderType = u.Type
	}
}

func applyPayload(ev *Event, p *wirePayload) {
	if p == nil {
		return
	}
	if p.Message != nil {
		applyMessage(ev, p.Message)
	}
	if p.Space != nil {
		applySpace(ev, p.Space)
	}
	if p.User != nil && ev.UserEmail == "" {
		applyUser(ev, p.User)
	}
}

func applyMessage(ev *Event, m *wireMessage) {
	ev.MessageName = m.Name
	if m.Thread != nil {
		ev.Thread = m.Thread.Name
	}
	if m.Space != nil {
		applySpace(ev, m.Space)
	}
	if len(ev.SenderRaw) == 0 && len(m.Sender) > 0 {
		ev.SenderRaw = append(json.RawMessage(nil), m.Sender...)
	}
	if len(m.Sender) > 0 && string(m.Sender) != "null" {
		var s wireUser
		if err := json.Unmarshal(m.Sender, &s); err == nil {
			ev.SenderType = firstNonEmpty(s.Type, ev.SenderType)
			ev.UserID = firstNonEmpty(s.Name, ev.UserID)
			ev.UserLegacyID = firstNonEmpty(s.LegacyID, ev.UserLegacyID)
			ev.UserDisplayName = firstNonEmpty(s.DisplayName, ev.UserDisplayName)
			if s.Email != "" {
				if ev.UserEmail != "" && !strings.EqualFold(ev.UserEmail, s.Email) {
					ev.IdentityMismatch = true
				}
				ev.UserEmail = s.Email
				if ev.UserDisplayName == "" {
					ev.UserDisplayName = s.DisplayName
				}
			}
		}
	}
	ev.Text = m.Text
	ev.Argument = m.ArgumentText
	ev.Silent = m.Silent
	for _, a := range m.Annotations {
		if a.Type == "USER_MENTION" {
			ev.Mentioned = true
		}
	}
	for _, a := range m.Attachment {
		ev.Attachments = append(ev.Attachments, Attachment{
			Name:        a.Name,
			ResourceRef: a.AttachmentDataRef.ResourceName,
			ContentName: a.ContentName,
			ContentType: a.ContentType,
			Source:      a.Source,
		})
	}
	if len(m.SlashCommand) > 0 && string(m.SlashCommand) != "null" {
		var sc struct {
			Name        string `json:"name"`
			Input       string `json:"input"`
			ArgumentArg string `json:"argumentArgument"`
		}
		if json.Unmarshal(m.SlashCommand, &sc) == nil {
			setSlash(ev, firstNonEmpty(sc.Input, "/"+sc.Name))
		}
	}
}

func applySpace(ev *Event, s *wireSpace) {
	if s == nil {
		return
	}
	ev.Space = firstNonEmpty(s.Name, ev.Space)
	ev.SpaceType = firstNonEmpty(s.SpaceType, ev.SpaceType)
	if s.SingleUserBot {
		ev.DM = true
	}
	// `type` is DM in a 1:1 space and ROOM in a group; note the inconsistency documented in
	// FINDINGS: a 2-human space reports spaceType DIRECT_MESSAGE with type ROOM. Trust neither for
	// routing: key sessions off Space.
	if s.Type == "DM" {
		ev.DM = true
	}
}

func setSlash(ev *Event, input string) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return
	}
	ev.Slash = true
	fields := strings.Fields(input)
	ev.Command = fields[0]
	ev.CommandArgs = strings.TrimSpace(strings.TrimPrefix(input, fields[0]))
}

// finish derives the fields the bridge routes on, in one place.
func finish(ev *Event) {
	if ev.Thread == "" && ev.MessageName != "" {
		// Chat message names look like "spaces/<s>/messages/<msg>.<thread>": the suffix is the id of
		// the thread the message already belongs to. Rebuilding it as a thread resource name is what
		// lets a reply land in the same conversation.
		if parts := strings.Split(ev.MessageName, "/"); len(parts) == 4 && strings.Contains(parts[3], ".") {
			ev.Thread = parts[0] + "/" + parts[1] + "/threads/" + strings.SplitN(parts[3], ".", 2)[1]
		}
	}
	if ev.MessageName != "" && ev.Space == "" {
		// "spaces/<s>/messages/<m>" -> "spaces/<s>".
		if parts := strings.Split(ev.MessageName, "/"); len(parts) >= 3 {
			ev.Space = strings.Join(parts[:2], "/")
		}
	}
	if ev.Thread == "" && ev.Space != "" {
		ev.Thread = ev.Space // DM spaces thread every message; keep replies attached.
	}
	// Command detection for plain text: a mention message "@pi-chat /status" gives Argument
	// " /status"; a DM "/status" gives Text and Argument "/status".
	arg := strings.TrimSpace(firstNonEmpty(ev.Argument, ev.Text))
	if strings.HasPrefix(arg, "/") && !ev.Slash {
		setSlash(ev, arg)
	}
}

// Prompt is the text to hand to Pi: the mention-stripped remainder when Chat gave us one, so the
// model is not asked to parse "@pi-chat".
func (e *Event) Prompt() string {
	text := e.Argument
	if strings.TrimSpace(text) == "" {
		text = e.Text
	}
	return strings.TrimSpace(text)
}

// Who renders the sender for the prompt prefix, e.g. `Amy (member@example.com)`.
func (e *Event) Who() string {
	name := e.UserDisplayName
	if name == "" {
		name = e.UserEmail
	}
	if name == "" {
		name = e.UserID
	}
	return name
}

func (e *Event) String() string {
	return fmt.Sprintf("%s space=%s user=%s text=%q atts=%d", e.Kind, e.Space, e.UserEmail, truncate(e.Prompt(), 60), len(e.Attachments))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
