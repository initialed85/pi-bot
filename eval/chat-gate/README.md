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
- Classifier error/timeout: **fail-open since `54426e0`** - a gate that silently ignores a mid-thread
  message because a GPU is busy is worse than an occasional unwanted reply. DMs answer in-line (never
  thread); in enforce the in-thread fallback is ANSWER INLINE, while a clean NO still suppresses. Keep
  the small model warm so timeouts stay rare.
- Rollout: dry-run flag first (log decisions, suppress nothing), then enforce.

## Reviewing live decisions

`gate-logs.mjs` renders the deployed bridge's dry-run decisions as a table, plus the
only list that matters before enforce: what it *would* have suppressed, what it would
have rerouted into a side thread, and any call approaching the 2s deadline.

    node eval/chat-gate/gate-logs.mjs --since 24h        # live pod logs via kubectl
    node eval/chat-gate/gate-logs.mjs --stdin < logs.txt # or pasted lines
    node eval/chat-gate/gate-logs.mjs --json             # machine-readable

It reads `kubectl logs deploy/pi-bot-bridge` in the dev cluster directly rather than
depending on log shipping. Top-level group messages ignored *without* a model call are
counted separately - those should be numerous and cost nothing.
