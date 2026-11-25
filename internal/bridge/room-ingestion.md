# Room ingestion brief: the user-authorised listener

Status: capability proven end-to-end on 2026-11-24/25. Code not written. Owner for the transport
work: build-google-chat-bot. Write against bridge digest `sha256:a73d34cd…` (pi-bot@1986bbf),
confirmed by home-ops as the intended pinned image; `PI_CHAT_GATE_ROOM_ROOT` is unset there, which
means the room gate is compiled in but currently set to ignore.

## Why this path exists

Measured on Ed's consumer `@gmail.com` account, in the pi-chat space `spaces/AAQAxKMXxjk`:

- The Chat **app** credential receives only messages that mention the app. Two untagged messages
  posted on 2026-11-25 (`hi mate` 01:23:44Z, `yo` 01:23:56Z) produced **zero**
  bridge log lines, while a real mention in the same space arrived within seconds.
- A Workspace Events subscription authorised by a **human OAuth token** delivered those same
  untagged messages to Pub/Sub within ~2 seconds. That is the only way ordinary room traffic
  reaches us.

## Live resources (as of 2026-11-25T01:31Z)

| What | Value |
|---|---|
| Workspace Events subscription | `subscriptions/chat-spaces-czotOjExMzA3MDM5NDE5MTQwODMxMTAyNDoxMDM5MDI2MjA2OTQ5OTk1MzgzNTU` |
| Target | `//chat.googleapis.com/spaces/-` — every space the authorizing user belongs to |
| Event type | `google.workspace.chat.message.v1.created` |
| Topic | `projects/home-ops-513116/topics/pi-chat-user-events` |
| Pull subscription | `pi-chat-user-events-sub` |
| Field mask | `message.name,message.text,message.sender,message.annotations,message.thread` |
| Authority | `users/113070394191408311024` (Ed's consumer account) |

Tooling: `tools/chat-user-subscription.mjs` (`create` / `show` / `renew` / `delete` / `pull`),
pi-bot@master `0d7916f` plus the pull-parser fix `4da1985`. It reads credentials from
`PIBOT_OAUTH_CLIENT` and `PIBOT_OAUTH_TOKEN` file paths, defaulting to `/root/oauth-client.json`
and `/root/chat-user-token.json`.

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
            message.sender      { name: users/<id>, type: HUMAN | BOT }
            message.text        plain text
            message.annotations  [{ type: USER_MENTION, startIndex, length,
                                    userMention: { user: { name, type }, type: MENTION } }]
            message.thread      { name: spaces/<space>/threads/<thread> }
```

Untagged messages simply omit `annotations`. A real mention carries `USER_MENTION` naming the bot,
so tag-vs-untagged routing works on this path exactly as it does on the app path.

## Hard constraints, all measured

1. **Loop guard is mandatory.** The bot's own messages arrive as events. Observed: pi-chat's `👀`
   acknowledgement published with `sender.name = users/103902620694999538355`, `sender.type = BOT`.
   Without filtering, the bot answers itself. Filter on `sender.type != "HUMAN"`, and treat the
   known bot id `users/103902620694999538355` as a second, explicit guard.
2. **Dedupe against the existing app path.** A mention is delivered twice: once via
   `pi-chat-events` (app trigger) and once via `pi-chat-user-events` (user subscription). Key dedupe
   on `message.name`, which is identical on both.
3. **Field mask is frozen at creation.** `PATCH` on `payloadOptions.fieldMask` returns
   `400 INVALID_FIELD_MASK`. Changing the payload shape means delete + create.
4. **Renewal every ~3 h.** `expireTime` is set 4 h after `createTime` and is *not* a reliable
   guard-rail: one subscription reported `EXPIRES 19:41Z` and was still delivering at 01:23Z, then
   returned `403 SUBSCRIPTION_ACCESS_DENIED` minutes later. Renew = delete + create; the created
   name is deterministic for a given (space, authorizer) pair.
5. **There is no usable `list`.** `GET /v1/subscriptions` fails with `FILTER_PARSING_FAILURE` even
   with a filter. Discover the name from a create response, or from the `409` error metadata field
   `current_subscription`.
6. **Pull subscription must exist before publishing.** Pub/Sub only delivers to subscriptions that
   already existed at publish time. This looked exactly like "the feature does not work" once.
7. **Secrets.** The refresh token and OAuth client secret currently live on my pod's ephemeral
   `/root` (`/workspace` is the only NFS mount). They must become a Kubernetes Secret in `slopyphus`
   mounted by the bridge, and must never enter this repo, which is public.
8. **Privacy.** `spaces/-` now covers rooms containing other people. Do not log participant message
   text at INFO. The tool prints `<participant message: text not printed>` for anything not sent by
   the account owner; the bridge should keep that behaviour in its logs while still processing the
   message.

## What "done" looks like

- Untagged message in a subscribed room → exactly one bridge turn, one reply, no second reply from
  the duplicate app-path delivery.
- pi-chat's own acknowledgement and reply events produce no turn.
- Subscription renewed before it lapses; renewal failure surfaces as a loud log line, not silence.
- Room allowlist and `PI_CHAT_GATE_ROOM_ROOT` semantics unchanged: the gate decides *whether* to
  answer, this path only decides *what we can hear*.
- No participant text in INFO logs.

## Still unknown (do not assume)

- **Cross-room coverage is unproven.** Delivery is proven in the test space only. The claim that
  `spaces/-` covers rooms with other members needs one message in a second room.
- Whether DMs and spaces behave identically under `spaces/-`.
- Whether consent (Testing-mode client, Ed as test user, restricted `chat.messages.readonly` scope)
  has any expiry that outlives the subscription.
- Subscription count limits per user, and whether Pub/Sub retention on `pi-chat-user-events` should
  be shortened to keep unattended participant messages from sitting in the topic.
