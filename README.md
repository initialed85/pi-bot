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
PI_CHAT_REPLY_GATE_TIMEOUT_SECONDS=3            # deadline for the first attempt
PI_CHAT_REPLY_GATE_ATTEMPT_MS=3000,6000,12000   # every attempt; unset means double the previous one
PI_CHAT_GATE_ROOM_ROOT=ignore      # ignore | dry-run | classify: what an untagged new room message gets
```

In `enforce` mode, direct messages can be routed inline or into a new thread, and untagged messages
inside a group thread can be suppressed. An untagged group top-level message is ignored before any
model call unless `PI_CHAT_GATE_ROOM_ROOT` says otherwise: `classify` (synonym: `enforce`) sends it to the
separate `room-respond` classifier, which answers in the thread Chat created for that message and only
when it judges the message is something pi-chat is being asked about. `dry-run` consults the same
classifier and logs the verdict without ever posting, so a room can be trialled without the trial itself
becoming the noise.

That room switch is deliberately separate from `PI_CHAT_REPLY_GATE_MODE`, and the reason is a trap that was
in the original design: there used to be one gate mode, so trialling a room meant setting the whole bridge
to `dry-run`, and `dry-run` never suppresses anything. Group-thread messages the gate had been silencing
would have started being answered for the length of the trial, i.e. reviewing the feature would have
changed the behaviour people were already relying on. `PI_CHAT_REPLY_GATE_MODE=enforce` plus
`PI_CHAT_GATE_ROOM_ROOT=dry-run` is the shape that measures a room while every flow that already gates
keeps gating. `TestRoomDryRunDoesNotDowngradeFlowsThatAlreadyGate` is the test for that.
An untagged group thread is eligible only if it already contains an @pi-chat mention.
If the classifier cannot decide - transport failure, timeout after every attempt, or an answer that is
not an exact token - a 1:1 message still gets the default inline route (it is addressed by construction)
and a shared-thread message is left unanswered. A mention never reaches the gate, so tagging is always
the way to force an answer. Do not enable `enforce` until dry-run logs have been reviewed.
The prompts are embedded under `internal/chatgate/prompts/` and are intentionally kept identical to
the evaluated prompts in the companion `pi-bot` package. `go test ./...` fetches the upstream prompt
files and fails on drift, forcing a re-vendor and fresh eval; set `PI_CHAT_GATE_PROMPT_DRIFT_WARN=1`
only for an intentionally offline CI run.

One caveat governs all of the above: with the **app** credential, untagged room messages do not arrive
as push events at all - only DMs and @-mentions do (measured, see `eval/chat-gate/README.md`). Room
gating only has something to gate once the user-authorised room listener below is delivering.

## Room listener (the user-authorised ingestion path)

The Chat app credential hears mentions and its own DMs, and nothing else. A Workspace Events
subscription authorised by a human OAuth token hears every message in every room that human belongs
to, which is the only way untagged room traffic exists for us. Enabling it is a privacy decision, not
just a deployment one: `spaces/-` sees rooms containing other people.

```bash
PI_CHAT_USER_LISTENER=off              # off | on (default off)
PI_CHAT_USER_TOPIC=pi-chat-user-events
PI_CHAT_USER_SUBSCRIPTION=pi-chat-user-events-sub
PI_CHAT_USER_RENEW=on                  # on | off (default on whenever the listener is on)
PI_CHAT_USER_RENEW_MINUTES=180         # subscriptions live ~4h (measured twice)
PI_CHAT_USER_OAUTH_CLIENT_FILE=/etc/pi-chat-user-oauth/client_id
PI_CHAT_USER_OAUTH_SECRET_FILE=/etc/pi-chat-user-oauth/client_secret
PI_CHAT_USER_OAUTH_TOKEN_FILE=/etc/pi-chat-user-oauth/refresh_token
```

The credential is read from files, never from env vars: a secret in an env var shows up in
`kubectl describe pod`, in a crashed subprocess dump and in a stray `set -x`. Both file shapes work -
one installed-client JSON file, or three plain files from a `--from-literal` Secret - and the reader
decides which by content.

What the listener does with a message is deliberately narrow, in this order:

1. **Loop guard.** Google delivers pi-chat's own replies as events with `sender.type = BOT`. Answering
   those is a conversation the bot has with itself in front of other people.
2. **Ownership.** Mentions and 1:1 DMs are left to the app path, which already receives them with the
   envelope identity a slash command needs. That decision runs *before* the dedupe mark, otherwise the
   listener copy consumes the key and the app copy is dropped as a duplicate - a tagged message going
   unanswered is the failure mode, and it is tested.
3. **Dedupe** on `message.name`, identical on both paths.
4. **`onMessage`**, the same entry point the app path uses, so the allowlist, room eligibility and the
   reply gate are not reimplemented here and cannot drift into a second, looser policy.

The subscription expires on a timer and the API refuses to list subscriptions, so the renewer discovers
the live name from the `409` error metadata and logs a new `expireTime` on every renewal. Renewal
failure logs at ERROR with the next attempt scheduled; a listener that has gone deaf is never silent.

`internal/bridge/room-ingestion.md` is the operational brief: measured constraints, the payload
contract, the mask that is not actually validated, and what is still unknown.

### Reviewing the gate before it speaks

```bash
go build -o /tmp/pi-chat-audit ./cmd/pi-chat-audit
PI_CHAT_AUDIT_RUN_SECONDS=300s /tmp/pi-chat-audit
```

`cmd/pi-chat-audit` replays the user-events backlog through the real decision path - the real
`HandleWorkspace`, the real classifier, the same allowlist and space lookup - and prints what the gate
would have decided. It cannot post: its Chat implementation returns errors for Post, Update and Delete,
and its gateway refuses to ask Pi. It reads with the same user OAuth credential that authorises the
subscription, and it acknowledges what it reviews, so run it deliberately.

## Layout

```
cmd/pi-bot/   main: config, Pub/Sub consumer, health endpoints, graceful drain
cmd/pi-chat-audit/    offline room-gate review: replays the backlog, decides without speaking
internal/chatmsg/     event parsing (add-on shape, flat shape, Workspace Events CloudEvents), fixtures in testdata/
internal/chatapi/     Chat API client: post, patch, get, delete, media download, space type
internal/chatevents/  Workspace Events subscription renewal and the user OAuth credential
internal/gateway/     HTTP client for the Pi gateway
internal/bridge/      allowlist, dedupe, commands, images, turn watching, chunking, room listener
gateway/index.ts      Pi extension: in-process HTTP control of the live session
```

## Tests

```bash
go test ./...
```

The bridge tests drive the real event fixtures with faked Chat and gateway clients; the gateway
extension is exercised against a live `pi --mode rpc` session.
