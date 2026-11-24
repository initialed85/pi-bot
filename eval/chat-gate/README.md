# pi-chat gating — offline eval for the small model

Prompts + labelled cases for the three classification flows Ed specced (2026-11-21),
run against the cheap/fast model at `sentiment-analyzer.dev.initialed85.cc`
(OpenAI-compatible `/v1/chat/completions`, model `unsloth/gemma-4-E4B-it-GGUF:IQ4_XS`).

    node eval.mjs should-respond   # gate: reply to untagged thread message? (YES/NO)
    node eval.mjs thread-request   # gate: DM wants a thread? (THREAD/INLINE)
    node eval.mjs summary          # running thread summary (<=30 words, merge not append)

Baselines (2026-11-21): should-respond 28/30 (for-bot 10/10, not-for-bot 12/12,
coworker-thread 6/8), thread-request 16/16, summary 5/5. Real-transcript replay:
37/38 bridged messages answered in-line, 1 opened a thread. Warm latency ~200-300 ms
per classification, ~1.0-1.5 s per summary update; cold model first call ~1-4 s (a cold
pod can exceed 15 s - keep the model warm in prod). During the hermes backfill window
(until 2026-11-22) the same calls measured 3.7-5.1 s with timeouts; do not size the 2 s
deadline from those numbers.

Borderline cases are not reproducible: Gemma at temperature 0 flipped the same input
between YES and NO on repeat runs while the GGUF server was batching with other traffic.
Treat a single live suppression as a sample, not as proof of a rule.

Design decisions baked into the prompts:
- should-respond decides on **who a message is addressed to**, not on who could answer it: a question
  pi-chat happens to be able to field is NO when the summary shows it belongs to an exchange between
  people, and YES when it reacts to, corrects, or continues something pi-chat said or produced (including
  bare acknowledgements like "dir*" or "yeah gotcha"). This replaced a "default YES" rule that scored
  12/12 on family banter but 3/8 in a multi-party technical thread, i.e. interrupted coworkers ~60% of
  the time. The current version keeps 12/12 there but still leaks 2/8 on group-directed questions
  ("anyone got a spare 4090"); every attempt to tighten that traded away replies genuinely aimed at
  pi-chat, so the looser side wins - silence reads as breakage.
- thread-request fires ONLY on explicit thread wording; "big/deep/serious topic"
  must not trigger it (the three initial misses were exactly that, now fixed).
- summary is additive-merge with a hard 30-word cap; unchanged messages pass through.

Bridge integration contract (for build-google-chat-bot):
- **`initialed85/pi-bot` has never been registered in Woodpecker** (home-ops confirmed 2026-11-21 by reading
  `woodpecker-postgres`: no row in `repos`, and zero pipelines for `5e16eb3`, `54426e0`, `5b75505` or `f13fc7f`).
  So `.woodpecker/ci.yaml` and `.woodpecker/build.yaml` are inert for this repo, and every image so far has been
  built and pushed by hand from the build lane. Nothing enforces the checks below automatically - run them.
  Activating the repo needs a human Woodpecker login (same flow `mcbot` needed as repo id 35).
- The bridge embeds these prompts (`internal/chatgate/prompts/`, `go:embed`). **Prompt text is owned
  here.** Edit `prompts/*.md`, re-run the eval, then `node revendor.mjs` to copy them into the bridge.
  The authoritative equality check is `internal/chatgate/prompt_drift_test.go`: it fetches these files
  from pi-bot master over HTTP, fails `go test ./...` on any mismatch, and asserts that the recorded
  evaluated revision (`VendoredPromptSource`, currently `initialed85/pi-bot@c34f21a`, vendored in
  `f13fc7f`) is the one the numbers above were measured at. That revision is pinned in three places
  (`client.go`, a `Contains` check and an `Example` output in the test), so a re-vendor bumps all three.
  Never edit the vendored copy - it would fail their CI, not ours.
- DM (1:1): no should-respond gate - a DM is always for the bot. Untagged: ask thread-request;
  THREAD -> open thread + forward, INLINE -> answer in-line.
- Group space, TOP LEVEL, untagged: IGNORE - hard rule, no classifier call (Ed's call, 2026-11-21).
  pi-chat only ever engages in a group when it has been tagged into that thread; untagged room noise
  is never forwarded and never scored. This also keeps the model off the hot path for group chatter.
- Thread, untagged: (summary, message) -> should-respond; NO -> suppress AND update summary,
  YES -> forward with summary attached as context, update summary.
- @pi-chat: never gates. Deterministic reply.
- Classifier error/timeout: **fail-closed in shared threads, default-route in a 1:1** (Ed's call,
  2026-11-24: "failed closed i think, if you mean 'classifier failed, can't figure out if this is for
  me or no'"). This replaced fail-open (`54426e0`). In a 1:1 the message is addressed by construction,
  so a failed call still answers inline - the only safe fallback there is the route, not a guess about
  whether it was wanted. In a shared thread an undecided message is silenced, because a wrong guess
  puts unasked-for text in front of other people. An answer that is not an exact token counts as
  undecided too. The escape hatch is tagging: a mention never reaches the gate, so @pi-chat always
  gets an answer.
- Timeout sizing: `latency.mjs` measures with a 60s ceiling, because the old 2s deadline CENSORED its
  own evidence - every slow call in the logs reads exactly 2000ms. Measured 2026-11-24: should-respond
  p50 173ms / p95 3632ms / max 20405ms; thread-request p50 200ms / p95 261ms / max 6269ms; summary
  p50 1019ms (it asks for 40 tokens). Bimodal, not a tail: fast calls are fast and the stragglers are
  a cold model. The gate therefore tries 3s, then 6s, then 12s, 300ms apart, and the summary call is
  capped at 4s so enrichment cannot eat the decision's budget. Keep the small model warm.
- Rollout: dry-run flag first (log decisions, suppress nothing), then enforce.

## Running the batches without freezing the chat

pi-chat is backed by ONE pi session, and that session is also the thing answering these evals. A
synchronous `node eval.mjs` run holds the turn for minutes (22-38 classifier calls), so any Chat message
that arrives waits for the whole batch: on 2026-11-21 a 20-minute eval loop delayed Ed's message by
~43 minutes and the bridge had to keep polling a turn that had not started. While Chat is live, run
batches detached and end the turn:

    nohup node eval.mjs should-respond > /tmp/eval-sr.log 2>&1 &

Then read `/tmp/eval-sr.log` in a later turn. `eval-real.mjs` takes several minutes for the same reason.

## Reviewing live decisions

`gate-logs.mjs` renders the deployed bridge's dry-run decisions as a table, plus the
only list that matters before enforce: what it *would* have suppressed, what it would
have rerouted into a side thread, and any call approaching the 2s deadline.

    node eval/chat-gate/gate-logs.mjs --since 24h        # live pod logs via kubectl
    node eval/chat-gate/gate-logs.mjs --stdin < logs.txt # or pasted lines
    node eval/chat-gate/gate-logs.mjs --json             # machine-readable

Size the gate timeout with `latency.mjs`. It exists because the deployed logs cannot answer that
question: a 2s deadline truncates every slow call, so the logs report hundreds of suspiciously
identical 2000ms decisions and hide whether the real latency was 2.1s or 20s.

```bash
node eval/chat-gate/latency.mjs                      # all three flows, 2 reps, 60s ceiling
LATENCY_REPS=4 node eval/chat-gate/latency.mjs should-respond   # one flow, deeper percentiles
```

It prints p50/p90/p95/p99/max per flow, then what fraction of calls each candidate ceiling would
have let through on the first try. Run it detached: it hammers the same small model chat is using.

It reads `kubectl logs deploy/pi-bot-bridge` in the dev cluster directly rather than
depending on log shipping. Top-level group messages ignored *without* a model call are
counted separately - those should be numerous and cost nothing.

**A roll destroys evidence.** `logs deploy/...` only sees the *current* pod, and pods are
deleted on rollout, so everything before the last restart is unrecoverable - there is no log
shipping to fall back on. Capture the window you care about *before* rolling (`kubectl logs
pi-bot-bridge-<old-pod> > /tmp/pre-roll.jsonl`), and keep such captures outside this repo:
live transcripts are personal data and this repository is public.

## What the gate can actually see in a group room (as of 2026-11-21)

Two transport facts, both verified against the deployed credential, mean `should-respond`
currently has no way to run in a room at all:

1. **Untagged room messages never reach the bridge.** Only DMs and @-mentions arrive as push
   events; a mention skips the gate by design, so it logs no decision. Zero group messages
   produced a gate line all day.
2. **Thread ownership does not survive a restart.** The lookup added in `5e16eb3`
   (`HasBotMessage`) calls `spaces.messages.list`, and that read is permanently refused for
   this app - `chat.bot` gets a 403 and the wider `chat.app.messages.readonly` scopes return
   "requires administrator approval ... requires a Google Workspace organization". Ownership
   is therefore in-process only, and a roll makes pi-chat forget the threads it was tagged
   into, whose untagged follow-ups then get ignored without a model call.

The one route that might unblock room traffic without a paid Workspace organization is
*user* authorisation instead of the app credential - Google lists user auth as supported for
space event subscriptions, and every refusal so far came from the app credential. Probe it with
`tools/chat-user-oauth-probe.mjs` (runs on a laptop, needs an OAuth client + one consent click).
If it returns 200s where the app gets 403/500, room-wide listening becomes technically possible
but means the bot reads the room as Ed, which is a disclosure decision before it is a feature.

Consequence for any review: **an empty decision table in enforce is the expected result, not
evidence the gate is well-behaved.** "No suppressions" today means "the gate was never
consulted", so group behaviour must be judged from offline eval (`eval.mjs`, `eval-real.mjs`)
until ingress and ownership are fixed. Untagged DM follow-ups produce no line either - a DM is
always answered and the `thread-request` gate fires only for unmentioned ones.
