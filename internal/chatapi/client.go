// Package chatapi wraps the Google Chat operations the bridge needs, all as the app's own service
// account: posting and editing text messages, and downloading the bytes of an attachment a user
// uploaded.
//
// The media flow is the one documented in spike/FINDINGS.md, not the one the docs imply: the
// downloadUri inside a Pub/Sub event is a Chat *web app* redirect that serves HTML to a non-browser
// client, so the bytes come from the media API instead -
// GET https://chat.googleapis.com/v1/media/{base64 attachmentDataRef.resourceName}?alt=media -
// using the same chat.bot credential.
package chatapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	chat "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
	apithttp "google.golang.org/api/transport/http"
)

const (
	chatScope = "https://www.googleapis.com/auth/chat.bot"
	mediaPath = "https://chat.googleapis.com/v1/media/"
)

type Client struct {
	svc  *chat.Service
	http *http.Client
}

// New builds the Chat service and an authenticated HTTP client from a service account key file.
func New(ctx context.Context, keyFile string) (*Client, error) {
	svc, err := chat.NewService(ctx, option.WithCredentialsFile(keyFile), option.WithScopes(chatScope))
	if err != nil {
		return nil, fmt.Errorf("chatapi: create chat service: %w", err)
	}
	client, _, err := apithttp.NewClient(ctx, option.WithCredentialsFile(keyFile), option.WithScopes(chatScope))
	if err != nil {
		return nil, fmt.Errorf("chatapi: create media client: %w", err)
	}
	client.Timeout = 2 * time.Minute
	return &Client{svc: svc, http: client}, nil
}

// PostOpts carry the Chat-side knobs for one reply.
type PostOpts struct {
	// Space is "spaces/…".
	Space string
	// Thread is "spaces/…/threads/…"; empty starts a new thread.
	Thread string
	// ClientID is a `client-…` custom message id. Pass a deterministic one to make a reply
	// idempotent across Pub/Sub redeliveries: Google refuses to create it twice.
	ClientID string
}

type Message struct {
	Name string
	Text string
}

func (c *Client) Post(ctx context.Context, text string, opts PostOpts) (*Message, error) {
	msg := &chat.Message{Text: text, MarkupSyntax: "MARKUP_SYNTAX_MARKDOWN"}
	if opts.Thread != "" {
		msg.Thread = &chat.Thread{Name: opts.Thread}
	}
	call := c.svc.Spaces.Messages.Create(opts.Space, msg)
	if opts.ClientID != "" {
		call = call.MessageId(opts.ClientID)
	}
	// REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD keeps the answer in the thread the question came from and
	// quietly starts one where threading is unsupported, instead of failing the post.
	call = call.MessageReplyOption("REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD")
	created, err := call.Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("chatapi: post to %s: %w", opts.Space, err)
	}
	return &Message{Name: created.Name, Text: created.Text}, nil
}

// Update replaces the text of a message we already posted. The bridge posts a placeholder and keeps
// patching it, so a five-minute Pi run reads as one self-editing message rather than twenty
// notifications in a chat that expects 30-second answers.
func (c *Client) Update(ctx context.Context, name, text string) error {
	msg := &chat.Message{Text: text, MarkupSyntax: "MARKUP_SYNTAX_MARKDOWN"}
	if _, err := c.svc.Spaces.Messages.Patch(name, msg).UpdateMask("text").Context(ctx).Do(); err != nil {
		return fmt.Errorf("chatapi: patch %s: %w", name, err)
	}
	return nil
}

// Get looks up a message by resource name - including by `client-…` custom id - so a bridge that
// restarted mid-turn can find and finish the placeholder it still owns.
// Delete removes a message the app authored. The bridge uses it to retire its own status bubble once
// the real answer is safely posted, which is how state (eyes, thinking) stays transient.
func (c *Client) Delete(ctx context.Context, name string) error {
	if _, err := c.svc.Spaces.Messages.Delete(name).Context(ctx).Do(); err != nil {
		return fmt.Errorf("chatapi: delete %s: %w", name, err)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, name string) (*Message, error) {
	got, err := c.svc.Spaces.Messages.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("chatapi: get %s: %w", name, err)
	}
	return &Message{Name: got.Name, Text: got.Text}, nil
}

// HasBotMessage checks Chat's durable history to recover thread ownership after a bridge restart.
// The app-authored message is identified by sender type BOT; no local state volume is required.
func (c *Client) HasBotMessage(ctx context.Context, space, thread string) (bool, error) {
	if space == "" || thread == "" {
		return false, nil
	}
	filter := "thread.name = " + thread
	page, err := c.svc.Spaces.Messages.List(space).Filter(filter).PageSize(100).Context(ctx).Do()
	if err != nil {
		return false, fmt.Errorf("chatapi: list thread %s: %w", thread, err)
	}
	for _, message := range page.Messages {
		if message != nil && message.Sender != nil && message.Sender.Type == "BOT" {
			return true, nil
		}
	}
	return false, nil
}

// AttachmentMeta is what spaces.messages.attachments.get says about one attachment. The event
// payload carries most of this already; the call exists to confirm the attachment is real and to
// pick up fields the event sometimes omits.
//
// Note DownloadUri here is still the human-facing URL: the API docs say plainly that "Chat apps
// shouldn't use this URL to download attachment content", which is why bytes come from DownloadMedia.
type AttachmentMeta struct {
	Name        string
	ContentType string
	ContentName string
	DownloadURI string
	Source      string
	ResourceRef string
}

func (c *Client) AttachmentMetadata(ctx context.Context, name string) (*AttachmentMeta, error) {
	got, err := c.svc.Spaces.Messages.Attachments.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("chatapi: attachments.get %s: %w", name, err)
	}
	meta := &AttachmentMeta{
		Name:        got.Name,
		ContentType: got.ContentType,
		ContentName: got.ContentName,
		DownloadURI: got.DownloadUri,
		Source:      got.Source,
	}
	if got.AttachmentDataRef != nil {
		meta.ResourceRef = got.AttachmentDataRef.ResourceName
	}
	return meta, nil
}

// DownloadMedia fetches attachment bytes. resourceName is the base64
// `attachmentDataRef.resourceName` from the event, used verbatim in the URL - it is itself the
// base64 of "spaces/…/messages/…/attachments/…".
func (c *Client) DownloadMedia(ctx context.Context, resourceName string, maxBytes int64) ([]byte, string, error) {
	name, err := ResolveResource(resourceName)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaPath+name+"?alt=media", nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("chatapi: download media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, "", fmt.Errorf("chatapi: download media: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("chatapi: read media: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("chatapi: attachment exceeds %d byte limit", maxBytes)
	}
	return data, resp.Header.Get("content-type"), nil
}

// ResolveResource accepts either the base64 reference from an event or a plain resource name.
func ResolveResource(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("chatapi: empty attachment reference")
	}
	if strings.HasPrefix(ref, "spaces/") {
		return ref, nil
	}
	raw, err := base64.StdEncoding.DecodeString(ref)
	if err != nil {
		return "", fmt.Errorf("chatapi: decode attachment ref: %w", err)
	}
	return string(raw), nil
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
