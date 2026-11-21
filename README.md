# Google Chat → Pi bridge

`pi-bot` bridges Google Chat messages to one long-lived Pi session: send a DM to the bot, or
mention it in a space. The Pi session stays fully usable in the terminal — Chat is a second pair of
hands on the same brain, not a separate bot with its own context.

The bridge and gateway extension are built and unit-tested. Deployment is intentionally environment-
specific: provide credentials and allowlists through your platform's secret manager rather than
committing them to this repository.

## How a message travels

```
Google Chat app (pi-chat)
      → Pub/Sub topic pi-chat-events → subscription pi-chat-events-sub
      → cmd/pi-bot            (Go, this repo)
      → gateway/index.ts              (Pi extension, loaded into the Pi session)
      → the live Pi session that is also sitting in a Tmux/TUI pane
```

The bridge never answers inline: a Chat interaction must be answered fast, while a real Pi run takes
minutes. So it acks the Pub/Sub message immediately, then watches the turn in the background.

What you see in Chat, and why it looks like that:

1. A status message appears in the source thread within about a second: 👀 the moment the bridge takes
   your message, 🤔 once Pi is actually running.
2. That same message is then rewritten into the model's answer (`PI_CHAT_STATUS_MODE=patch`, the
   default), so one bubble carries 👀 → 🤔 → answer and nothing is deleted. `=bubble` posts the answer
   separately and deletes the status message; `=none` posts no status at all.
3. Failures are never silent: the status message is left holding ❌ with the reason (⏹ for `/stop`,
   ⚠️ for lost contact with Pi).
4. Progress detail — partial output, tool names, step counts, timings, tok/s, model names — is *not*
   rendered into Chat. It goes to the bridge log and the terminal.
5. Answers past 30,000 characters continue as follow-up messages in the same thread, each with its own
   deterministic `client-` id (`PI_CHAT_STATUS_MODE` aside, Chat caps text at 32,000 characters,
   measured).

Chat's real reactions are not available to us: `spaces.messages.reactions.create` refuses app
(service-account) authentication outright, and a reaction made with a user's OAuth token would read as
*that user* reacting rather than the bot. Hence the status message instead of 👀/👍/❌ on yours.

Dedupe is belt-and-braces: Pub/Sub is at-least-once, so every prompt is keyed off the Chat message
name, and replies carry deterministic `client-` message ids that Google refuses to create twice. A
redelivery cannot fork a second answer or a second status bubble.

## Commands

Bridge commands (handled by the bridge, never reaching the model): `/help`, `/status`, `/commands`,
`/steer <text>`, `/queue <text>`, `/stop`. Anything else that looks like a slash command is passed to
Pi (`/compact`, `/model <id>`, `/thinking <level>`, `/name <text>`, …), so commands defined by
extensions in the session work without the bridge knowing about them.

Slash commands arrive as plain text with `argumentText` set — this app runs in Workspace add-on mode on
a personal Gmail, and such apps never receive `appCommandPayload`.

Images work: attach them and the bridge downloads them via
`chat.spaces.messages.attachments.get` + `v1/media/{base64 resourceName}?alt=media`, then hands them to
Pi as attachments.

## Running the bridge

```bash
go build -o pi-bot-bridge ./cmd/pi-bot

PI_CHAT_GATEWAY_URL=http://127.0.0.1:8090   PI_CHAT_GATEWAY_TOKEN=… \
PI_CHAT_KEY_FILE=___secret/pi-chat-bot.json \
PI_CHAT_PROJECT=YOUR_GCP_PROJECT             PI_CHAT_SUBSCRIPTION=YOUR_SUBSCRIPTION \
PI_CHAT_ALLOWLIST=allowed-user@example.com \
./pi-bot-bridge
```

The Pi session must load the gateway extension with the same token:

```bash
pi --mode rpc -e ./gateway/index.ts        # PI_CHAT_GATEWAY_TOKEN must be set, or the ext stays inert
```

Never commit service-account JSON, gateway tokens, Pub/Sub credentials, or production Chat payloads.
Use environment variables, mounted secrets, and sanitized fixtures instead.

`/healthz` and `/readyz` are served on `PI_CHAT_HTTP_ADDR` (default `:8091`); `readyz` reports the
gateway's state so a restart can tell whether Pi is reachable and busy. Full environment list:
`go doc ./cmd/pi-bot`, or the header of `cmd/pi-bot/main.go`.

`PI_CHAT_ALLOWLIST` is closed by default — an empty allowlist denies everyone, and an unmentioned
message in a shared room is ignored. A bot that answers strangers tells them the door is here.

## Optional reply gate

The bridge includes an optional small-model reply gate for untagged messages. It is **off by
default** because the classifier is a dev-only dependency that can be slow while cold. Set dry-run
explicitly to call the OpenAI-compatible classifier, log one structured `reply gate` decision per
gated message, and preserve the existing Chat behaviour. It never gates `@pi-chat` messages.

```bash
PI_CHAT_REPLY_GATE_MODE=dry-run       # off | dry-run | enforce (application default: off)
PI_CHAT_REPLY_GATE_URL=https://sentiment-analyzer.dev.initialed85.cc
PI_CHAT_REPLY_GATE_MODEL=unsloth/gemma-4-E4B-it-GGUF:IQ4_XS
PI_CHAT_REPLY_GATE_TIMEOUT_SECONDS=2
```

In `enforce` mode, direct messages can be routed inline or into a new thread, and untagged messages
inside a group thread can be suppressed. Classifier failures fall back to inline DMs and silence
untagged group-thread messages. Do not enable `enforce` until dry-run logs have been reviewed.
The prompts are embedded under `internal/chatgate/prompts/` and are intentionally kept identical to
the evaluated prompts in the companion `pi-bot` package. `go test ./...` fetches the upstream prompt
files and fails on drift, forcing a re-vendor and fresh eval; set `PI_CHAT_GATE_PROMPT_DRIFT_WARN=1`
only for an intentionally offline CI run.

## Layout

```
cmd/pi-bot/   main: config, Pub/Sub consumer, health endpoints, graceful drain
internal/chatmsg/     event parsing (wrapped add-on shape and flat shape), fixtures in testdata/
internal/chatapi/     Chat API client: post, patch, get, delete, media download
internal/gateway/     HTTP client for the Pi gateway
internal/bridge/      allowlist, dedupe, commands, images, turn watching, chunking
gateway/index.ts      Pi extension: in-process HTTP control of the live session
```

## Tests

```bash
go test ./...
```

The bridge tests drive the real event fixtures with faked Chat and gateway clients; the gateway
extension is exercised against a live `pi --mode rpc` session.
