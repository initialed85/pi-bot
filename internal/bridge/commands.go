package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/initialed85/pi-bot/internal/chatmsg"
	"github.com/initialed85/pi-bot/internal/gateway"
)

// Slash commands are handled here rather than by Chat's appCommands, because an app forced into
// Workspace add-on mode never receives appCommandPayload (spike/FINDINGS.md). Chat hands us
// "@pi-chat /status" as text with the mention stripped into argumentText, which is all we need.
//
// Anything not in this list is forwarded to Pi as a command, so the extension commands defined in
// the session (/compact, /model names, /todo-style skills) work without the bridge knowing about
// them.

func (b *Bridge) onCommand(ctx context.Context, ev *chatmsg.Event) error {
	name := strings.ToLower(strings.TrimPrefix(ev.Command, "/"))
	args := ev.CommandArgs

	switch name {
	case "help", "?":
		b.reply(ctx, ev, b.helpText())
		return nil
	case "status", "state":
		b.reply(ctx, ev, b.statusText(ctx))
		return nil
	case "commands", "skills":
		b.reply(ctx, ev, b.commandsText(ctx))
		return nil
	case "stop", "abort", "cancel", "esc":
		if err := b.gw.Abort(ctx); err != nil {
			b.reply(ctx, ev, fmt.Sprintf("⚠️ Couldn't abort: %v", err))
			return nil
		}
		b.reply(ctx, ev, "⏹ Aborting the current run. Pi keeps its context; a new message starts a fresh turn.")
		return nil
	case "steer", "interrupt":
		if strings.TrimSpace(args) == "" {
			b.reply(ctx, ev, "Usage: `/steer <text>` - pushes a correction into the run that is currently going.")
			return nil
		}
		return b.promptNow(ctx, ev, args, "steer")
	case "queue", "followup", "later":
		if strings.TrimSpace(args) == "" {
			b.reply(ctx, ev, "Usage: `/queue <text>` - waits until the current run finishes, then sends this.")
			return nil
		}
		return b.promptNow(ctx, ev, args, "followUp")
	default:
		// Pass through to Pi, minus the leading slash.
		trimmed := strings.TrimPrefix(ev.Command, "/")
		if err := b.gw.Command(ctx, trimmed, args); err != nil {
			b.reply(ctx, ev, fmt.Sprintf("⚠️ `%s` did not work: %v\n\n%s", ev.Command, err, "Only commands this session defines are available - see /commands."))
			return nil
		}
		b.reply(ctx, ev, fmt.Sprintf("Ran `%s` in Pi. It does not produce a chat answer; send a message to talk to it.", ev.Command))
		return nil
	}
}

// promptNow sends a prompt and starts watching it, used by /steer and /queue where the plain
// mention path would otherwise pick the delivery mode itself.
func (b *Bridge) promptNow(ctx context.Context, ev *chatmsg.Event, text, deliverAs string) error {
	prompt := text
	if b.cfg.PrefixWithSender {
		prompt = fmt.Sprintf("%s <%s>: %s", ev.Who(), ev.UserEmail, prompt)
	}
	turnID := shortID(ev.MessageName)
	req := gateway.PromptRequest{
		ID:        turnID,
		Text:      prompt,
		DeliverAs: deliverAs,
		User:      fmt.Sprintf("%s <%s>", ev.Who(), ev.UserEmail),
		Space:     ev.Space,
		ReplyTo:   ev.MessageName,
	}
	resp, err := b.gw.Prompt(ctx, req)
	if err != nil {
		b.reply(ctx, ev, fmt.Sprintf("⚠️ Couldn't reach Pi: %v", err))
		return nil
	}
	b.track(ctx, turnID, canonicalTurn(resp, turnID), ev)
	return nil
}

func (b *Bridge) helpText() string {
	lines := []string{
		"*pi-chat* - a Pi session you can talk to from Chat.",
		"",
		"Send text (with `@pi-chat` in a room) and it goes to Pi. The only message left behind is the answer - there is no acknowledgement glyph.",
		"",
		"**Bridge commands**",
		"`/help` this list",
		"`/status` what Pi is doing, model, context use",
		"`/commands` commands this Pi session actually has",
		"`/steer <text>` push a correction into the run that is going now",
		"`/queue <text>` add a follow-up for when it finishes",
		"`/stop` abort the current run",
		"",
		"A plain message sent while Pi is working queues behind that run, so each message gets its own answer. One sent within a minute of the message that STARTED it folds into that run instead, so a typo and the correction after it get a single answer; `/steer` and `/queue` choose for one message at a time. `PI_CHAT_BURST_SECONDS` moves or disables that window.",
		"",
		"**Passed through to Pi**",
		"`/new`, `/compact`, `/model <id>`, `/thinking <level>`, `/name <text>`, `/resume` and any skill command - whatever this session defines.",
		"",
		"Images work too: attach them to the message and Pi sees them.",
	}
	return strings.Join(lines, "\n")
}

func (b *Bridge) statusText(ctx context.Context) string {
	state, err := b.gw.State(ctx)
	if err != nil {
		return fmt.Sprintf("⚠️ Can't reach Pi: %v", err)
	}
	_, running := b.Watched(b.LastSpace())
	status := "idle"
	if !state.Idle {
		status = "running"
	}
	if state.HasPending {
		status += " (queued messages)"
	}
	// Deliberately short, and with no plumbing (session ids, turn ids, command counts): on a phone
	// this is a status line, not a dashboard. The numbers that matter for deciding what to do next are
	// whether it is busy and how full the context is.
	parts := []string{
		fmt.Sprintf("*Pi is %s*", status),
		fmt.Sprintf("model: %s", firstNonEmptyStr(state.Model, "unknown")),
		fmt.Sprintf("thinking: %s", firstNonEmptyStr(state.ThinkingLevel, "-")),
		fmt.Sprintf("context: %d%% used (%s tokens)", int(state.ContextUsage.Percent), fmtInt(state.ContextUsage.Tokens)),
	}
	if running {
		parts = append(parts, "a run from this chat is still going")
	}
	return strings.Join(parts, "\n")
}

func (b *Bridge) commandsText(ctx context.Context) string {
	cmds, err := b.gw.Commands(ctx)
	if err != nil {
		return fmt.Sprintf("⚠️ Can't reach Pi: %v", err)
	}
	if len(cmds) == 0 {
		return "This Pi session has no extension commands."
	}
	var lines []string
	for _, c := range cmds {
		desc := c.Description
		if len(desc) > 60 {
			desc = desc[:60] + "…"
		}
		lines = append(lines, fmt.Sprintf("`/%s` %s", c.Name, desc))
	}
	return strings.Join([]string{"*Commands in this Pi session:*", "", strings.Join(lines, "\n"), "", "Send `/name args` to run one."}, "\n")
}

// LastSpace is the most recent space that started a turn, which is the one /status should describe.
func (b *Bridge) LastSpace() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastSpace
}

func fmtInt(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1024)
}
