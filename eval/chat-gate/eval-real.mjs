#!/usr/bin/env node
// Real-transcript eval: classifies the ACTUAL bridged Chat messages in this agent's
// pi session log, with no dataset written to disk (transcripts stay private).
//
// It is a no-false-positive test for thread-request: in the observed stream Ed never
// asks for a thread, so every real message should be INLINE. A prompt that threads
// these would spam him with unwanted topics.
//
// Also runs should-respond over the same real messages using the thread they actually
// sit in as the summary - there every message WAS for the bot, so anything but YES is
// a missed reply.
//
// Usage: node eval-real.mjs [session.jsonl ...]
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

const BASE = process.env.GATE_MODEL_URL || "https://sentiment-analyzer.dev.initialed85.cc";
const MODEL = process.env.GATE_MODEL || "unsloth/gemma-4-E4B-it-GGUF:IQ4_XS";
const DIR = process.env.PI_SESSION_DIR || "/root/.pi/agent/sessions/--workspace-pi-bot--";

function here(p) { return new URL(p, import.meta.url).pathname; }
const threadTpl = readFileSync(here("prompts/thread-request.md"), "utf8");
const respondTpl = readFileSync(here("prompts/should-respond.md"), "utf8");

const files = process.argv.slice(2).length
  ? process.argv.slice(2)
  : readdirSync(DIR).map((f) => join(DIR, f)).filter((f) => statSync(f).isFile() && f.endsWith(".jsonl"));

function extract(path) {
  const out = [];
  for (const line of readFileSync(path, "utf8").split("\n")) {
    let j; try { j = JSON.parse(line); } catch { continue; }
    const m = j.message || {};
    if (m.role !== "user") continue;
    let c = m.content;
    if (Array.isArray(c)) c = c.map((x) => x?.text ?? "").join(" ");
    if (typeof c !== "string" || !c.includes("[pi-chat:")) continue;
    const body = c.split("[chat bridge")[0];
    const text = body.replace(/^\s*\[pi-chat:[0-9a-f]+\][^:]*:\s*/, "").trim();
    if (text) out.push({ ts: j.timestamp, text });
  }
  return out;
}

const msgs = files.flatMap(extract);
if (!msgs.length) { console.error("no bridged messages found"); process.exit(1); }

async function run(prompt, maxTokens = 4) {
  for (let a = 0; a < 2; a++) {
    try {
      const res = await fetch(`${BASE}/v1/chat/completions`, {
        method: "POST", headers: { "content-type": "application/json" },
        body: JSON.stringify({ model: MODEL, temperature: 0, max_tokens: maxTokens, messages: [{ role: "user", content: prompt }] }),
        signal: AbortSignal.timeout(60000),
      });
      if (!res.ok) throw new Error(`http ${res.status}`);
      return ((await res.json()).choices?.[0]?.message?.content ?? "").trim();
    } catch (e) { if (a === 1) return `<ERROR ${e.message}>`; }
  }
}

let threadWrong = 0, respondWrong = 0;
for (let i = 0; i < msgs.length; i++) {
  const m = msgs[i];
  const threadAns = await run(threadTpl.replaceAll("{message}", m.text));
  // Real context: what the bot itself last said, truncated, stands in for the summary.
  const prior = msgs.slice(Math.max(0, i - 3), i).map((p) => p.text).join(" | ");
  const respondAns = await run(
    respondTpl.replaceAll("{summary}", prior || "start of the conversation")
      .replaceAll("{sender}", "Robin").replaceAll("{message}", m.text), 4);
  const tFail = threadAns.toUpperCase() !== "INLINE";
  const rFail = respondAns.toUpperCase() !== "YES";
  if (tFail) threadWrong++;
  if (rFail) respondWrong++;
  if (tFail || rFail)
    console.log(`FAIL ${m.ts?.slice(11, 19)}  thread=${threadAns} respond=${respondAns}  "${m.text.slice(0, 70)}"`);
}
console.log(`\n== real transcripts: ${msgs.length} bridged messages`);
console.log(`   thread-request: ${msgs.length - threadWrong}/${msgs.length} INLINE (wanted all INLINE; ${threadWrong} would have opened threads)`);
console.log(`   should-respond: ${msgs.length - respondWrong}/${msgs.length} YES (wanted all YES; ${respondWrong} would have been missed replies)`);
