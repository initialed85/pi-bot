# Room ingestion brief: the user-authorised listener

Status: capability proven end-to-end, and the consumer is written (pi-bot@master, `internal/bridge`
`HandleWorkspace`). Staged behind `PI_CHAT_USER_LISTENER`, which is **off** in the cluster until the
OAuth credential is a Secret. Write against bridge digest `sha256:a73d34cd…` (pi-bot@1986bbf) or
later; `PI_CHAT_GATE_ROOM_ROOT` is unset in the live manifest, which means the room gate is compiled
in but currently set to ignore.

## Why this path exists

Measured on Ed's consumer `@gmail.com` account, in the pi-chat space `spaces/AAQAxKMXxjk`:

- The Chat **app** credential receives only messages that mention the app. Two untagged messages
  posted on 2026-11-25 (`hi mate` 01:23:44Z, `yo` 01:23:56Z) produced **zero**
  bridge log lines, while a real mention in the same space arrived within seconds.
- A Workspace Events subscription authorised by a **human OAuth token** delivered those same
  untagged messages to Pub/Sub within ~2 seconds. That is the only way ordinary room traffic
  reaches us.

## Live resources (as of 2026-11-25T03:35Z)

| What | Value |
|---|---|
| Workspace Events subscription | `subscriptions/chat-spaces-czotOjExMzA3MDM5NDE5MTQwODMxMTAyNDoxMDM5MDI2MjA2OTQ5OTk1MzgzNTU` |
| Target | `//chat.googleapis.com/spaces/-` — every space the authorizing user belongs to |
| Event type | `google.workspace.chat.message.v1.created` |
| Topic | `projects/home-ops-513116/topics/pi-chat-user-events` |
| Pull subscription | `pi-chat-user-events-sub`, granted `roles/pubsub.subscriber` to `pi-chat-bot@home-ops-513116` |
| Field mask | `message.name,message.text,message.sender,message.annotations,message.thread,message.space` |
| Authority | `users/113070394191408311024` (Ed's consumer account) |

Tooling: `tools/chat-user-subscription.mjs` (`create` / `show` / `renew` / `delete` / `pull`),
pi-bot@master `0d7916f` plus the pull-parser fix `4da1985`. It reads credentials from
`PIBOT_OAUTH_CLIENT` and `PIBOT_OAUTH_TOKEN` file paths, defaulting to `/root/oauth-client.json`
and `/root/chat-user-token.json`.

## Cross-room coverage: proven 2026-11-25

- Group room `spaces/AAAA1pNBS1A` (friends, 4 members incl. pi-chat): untagged messages and a
  threaded reply from Amy arrived within 1–2 s, carrying `sender.email` for both humans.
- 1:1 DM `spaces/m4d90aAAAAE`: arrived too, and it answers inline in the live bridge.
- Mentions arrive on **both** topics; dedupe on `message.name` is therefore load-bearing.

## Payload contract

CloudEvents 1.0 over Pub/Sub. The `ce-*` attributes and `publishTime` are on the **Pub/Sub envelope**
(`rec.message.attributes`), the JSON body is `rec.message.data`. Reading `ce-*` out of the decoded
body prints `undefined` for every field while delivery is in fact working — this bug cost a test
cycle, do not reintroduce it.

```
attributes: ce-type=google.workspace.chat.message.v1.created
            ce-subject=//chat.googleapis.com/spaces/AAQAxKMXxjk
            ce-id=spaces/<space>/spaceEvents/<id>
body:       message.name        spaces/<space>/messages/<thread>.<message>
            message.sender      { name: users/<id>, email, type: HUMAN | BOT }
            message.text        plain text
            message.annotations  [{ type: USER_MENTION, startIndex, length,
                                    userMention: { user: { name, type }, type: MENTION } }]
            message.thread      { name: spaces/<space>/threads/<thread> }
```

Untagged messages simply omit `annotations`. A real mention carries `USER_MENTION` naming the bot,
so tag-vs-untagged routing works on this path exactly as it does on the app path.

**The field mask is not validated.** `validateOnly` create accepted
`message.name,not_a_field,banana,message.thread`. The subscription *did* deliver `sender.email` (a
field never named in the mask) but never `message.space`. Assume the mask is advisory: request what
you want, and prove each field by observing a delivery, not by trusting the mask or the API's
silence.

## Hard constraints, all measured

1. **Loop guard is mandatory.** The bot's own messages arrive as events. Observed: pi-chat's `👀`
   acknowledgement published with `sender.name = users/103902620694999538355`, `sender.type = BOT`.
   Without filtering, the bot answers itself. Filter on `sender.type != "HUMAN"`, and treat the
   known bot id `users/103902620694999538355` as a second, explicit guard.
2. **Dedupe against the existing app path.** A mention is delivered twice: once via
   `pi-chat-events` (app trigger) and once via `pi-chat-user-events` (user subscription). Key dedupe
   on `message.name`, which is identical on both. The ownership decision runs **before** the dedupe
   mark: a message the app path owns must not consume the key, or the app copy is dropped as a
   duplicate and a tagged message goes unanswered.
3. **Field mask is frozen at creation.** `PATCH` on `payloadOptions.fieldMask` returns
   `400 INVALID_FIELD_MASK`. Changing the payload shape means delete + create.
4. **Renewal every ~3 h.** `expireTime` is set 4 h after `createTime` and is *not* a reliable
   guard-rail: one subscription reported `EXPIRES 19:41Z` and was still delivering at 01:23Z, then
   returned `403 SUBSCRIPTION_ACCESS_DENIED` minutes later. Renew = delete + create; the created
   name is deterministic for a given (space, authorizer) pair.
5. **There is no usable `list`.** `GET /v1/subscriptions` fails with `FILTER_PARSING_FAILURE` even
   with a filter. Discover the name from a create response, or from the `409` error metadata field
   `current_subscription` — the latter is how the renewer finds a subscription it did not create.
6. **Pull subscription must exist before publishing.** Pub/Sub only delivers to subscriptions that
   already existed at publish time. This looked exactly like "the feature does not work" once.
7. **Secrets.** The refresh token and OAuth client secret currently live on my pod's ephemeral
   `/root` (`/workspace` is the only NFS mount). They must become a Kubernetes Secret in `slopyphus`
   mounted by the bridge, and must never enter this repo, which is public.
8. **Privacy.** `spaces/-` now covers rooms containing other people. Do not log participant message
   text at INFO. Both the tool and `HandleWorkspace` log participant bodies as `<redacted>` while
   still processing them.
9. **A space's type is not in the event.** The listener must ask `spaces.get` to know whether a
   space is a 1:1 DM (answers inline) or a group room (answered only through the room gate). The
   Chat read scope does not list membership, so membership is not knowable at all; that is why the
   room gate — not a room-type rule — is the privacy boundary.

## What is implemented

- `internal/chatmsg/workspace.go` — CloudEvents parse from envelope attributes + body.
- `internal/bridge/workspace.go` — `HandleWorkspace`: loop guard → ownership (mention/DM go to the
  app path) → dedupe → `onMessage`, which is the same entry point the app path uses, so the allowlist,
  the room gate and the reply policy are not reimplemented and cannot drift.
- `internal/chatevents/subscription.go` — credential load, in-process renewal (delete + create, 3 h
  cadence), name discovery through the 409 metadata, `expireTime` logged on every renewal.
- `internal/chatapi/space.go` — `spaces.get` with a cached per-bridge lookup.
- Wiring in `cmd/pi-bot/main.go`: `PI_CHAT_USER_LISTENER` gates the whole feature; the refresh token
  and OAuth client are **file paths**, never environment variables, because an env var is printed by
  a crashed subprocess, a `kubectl describe` of the pod spec and a stray `set -x`.

Wiring, in manifest terms (the shape home-ops needs; the Secret object is theirs to decide):

```yaml
- name: PI_CHAT_USER_LISTENER
  value: "on"          # keep "off" until pi-chat-user-oauth exists
- name: PI_CHAT_USER_OAUTH_CLIENT_FILE
  value: /etc/pi-chat-user/oauth-client.json
- name: PI_CHAT_USER_OAUTH_TOKEN_FILE
  value: /etc/pi-chat-user/refresh-token
- name: PI_CHAT_USER_RENEW_EVERY
  value: "180m"
volumeMounts:
  - name: pi-chat-user-oauth
    mountPath: /etc/pi-chat-user
    readOnly: true
```

## What "done" looks like

- [x] Untagged message in a subscribed room reaches the bridge at all.
- [x] Bot-authored events produce no turn (unit test on the captured real payload).
- [x] A mentioned message arriving on both paths is answered exactly once (unit test).
- [x] Renewal runs in-process and reports its own expiry (unit test against a fake API).
- [ ] Live end-to-end with `PI_CHAT_USER_LISTENER=on`: one untagged room message → classifier asked
      in dry-run → **real classifier verdicts on real text reviewed** → then `PI_CHAT_GATE_ROOM_ROOT`
      moved to `classify` and `PI_CHAT_REPLY_GATE_MODE=enforce` for that room.
- [ ] Participant message text verified absent from live INFO logs.
- [ ] No secret in this repo, and none in the manifest as an env var.

## Still unknown (do not assume)

- Whether `message.space` ever arrives. The mask requests it; no delivery has carried it. Until it
  does, `spaces.get` is the only way to know a space's type, and it costs an API call per new space.
- Whether consent (Testing-mode client, Ed as test user, restricted `chat.messages.readonly` scope)
  has any expiry that outlives the subscription. The client is stuck at "Testing" with no publish
  button, so the consent story is not a normal one.
- Subscription count limits per user, and whether Pub/Sub retention on `pi-chat-user-events` should
  be shortened (currently 7 days) so unattended participant messages do not sit in the topic.
- How many untagged messages a day actually arrive in the friends room: that number is the real
  measure of how noisy the room gate's false positives would be, and it is only knowable in dry-run.
