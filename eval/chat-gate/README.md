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
- DM (1:1): no should-respond gate - a DM is always for the bot. Untagged: ask thread-request;
  THREAD -> open thread + forward, INLINE -> answer in-line.
- Group space, top level, untagged: OPEN QUESTION, Ed to confirm. Current bridge behaviour is to
  reply to everything; the natural extension of his intent is to gate it through should-respond too.
- Thread, untagged: (summary, message) -> should-respond; NO -> suppress AND update summary,
  YES -> forward with summary attached as context, update summary.
- @pi-chat: never gates. Deterministic reply.
- Classifier error/timeout (>2s): DMs answer in-line (never thread); in-thread default is
  SILENT unless tagged. Keep the small model warm so timeouts stay rare.
- Rollout: dry-run flag first (log decisions, suppress nothing), then enforce.
