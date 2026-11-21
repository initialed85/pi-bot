# pi-chat gating — offline eval for the small model

Prompts + labelled cases for the three classification flows Ed specced (2026-11-21),
run against the cheap/fast model at `sentiment-analyzer.dev.initialed85.cc`
(OpenAI-compatible `/v1/chat/completions`, model `unsloth/gemma-4-E4B-it-GGUF:IQ4_XS`).

    node eval.mjs should-respond   # gate: reply to untagged thread message? (YES/NO)
    node eval.mjs thread-request   # gate: DM wants a thread? (THREAD/INLINE)
    node eval.mjs summary          # running thread summary (<=30 words, merge not append)

Baselines (2026-11-21, tuned prompts): should-respond 22/22, thread-request 16/16,
summary 5/5. Warm latency ~230 ms per classification, ~1.0-1.5 s per summary update;
cold model first call ~1-4 s (a cold pod can exceed 15 s — keep the model warm in prod).

Design decisions baked into the prompts:
- should-respond errs conservative toward humans chatting with humans (NO),
  but treats any question/task in a pi-chat-handled topic as for the bot (YES).
- thread-request fires ONLY on explicit thread wording; "big/deep/serious topic"
  must not trigger it (the three initial misses were exactly that, now fixed).
- summary is additive-merge with a hard 30-word cap; unchanged messages pass through.

Bridge integration contract (for build-google-chat-bot):
- The bridge embeds these prompts (`internal/chatgate/prompts/`, `go:embed`). **Prompt text is owned
  here.** Edit `prompts/*.md`, re-run the eval, then `node revendor.mjs` to copy them into the bridge.
  The authoritative equality check is `internal/chatgate/prompt_drift_test.go`: it fetches these files
  from pi-bot master over HTTP, fails `go test ./...` on any mismatch, and asserts that the recorded
  evaluated revision (`VendoredPromptSource`, currently `initialed85/pi-bot@404b884`) is the one the
  numbers above were measured at. Never edit the vendored copy - it would fail their CI, not ours.
- DM (1:1): no should-respond gate - a DM is always for the bot. Untagged: ask thread-request;
  THREAD -> open thread + forward, INLINE -> answer in-line.
- Group space, TOP LEVEL, untagged: IGNORE - hard rule, no classifier call (Ed's call, 2026-11-21).
  pi-chat only ever engages in a group when it has been tagged into that thread; untagged room noise
  is never forwarded and never scored. This also keeps the model off the hot path for group chatter.
- Thread, untagged: (summary, message) -> should-respond; NO -> suppress AND update summary,
  YES -> forward with summary attached as context, update summary.
- @pi-chat: never gates. Deterministic reply.
- Classifier error/timeout (>2s): DMs answer in-line (never thread); in-thread default is
  SILENT unless tagged. Keep the small model warm so timeouts stay rare.
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
