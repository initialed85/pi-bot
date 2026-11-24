#!/usr/bin/env node
// Sizes the reply-gate timeout from measurement instead of from the old 2s ceiling.
// The live gate truncates at 2s, so its own logs CANNOT tell you the true latency distribution -
// every slow call is reported as exactly 2000ms. This script calls with a 60s ceiling and reports
// the real percentiles, then shows what each candidate ceiling would have cost.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";

const BASE = process.env.GATE_MODEL_URL || "https://sentiment-analyzer.dev.initialed85.cc";
const MODEL = process.env.GATE_MODEL || "unsloth/gemma-4-E4B-it-GGUF:IQ4_XS";
const REPS = Number(process.env.LATENCY_REPS || 2);
const here = dirname(new URL(import.meta.url).pathname);
const flows = process.argv.slice(2).filter((f) => !f.startsWith("--"));
const flowsUse = flows.length ? flows : ["should-respond", "thread-request", "summary"];

async function call(flow, tpl, c) {
  const prompt = tpl.replaceAll("{summary}", c.summary ?? "").replaceAll("{sender}", c.sender ?? "a person").replaceAll("{message}", c.message);
  const t0 = Date.now();
  try {
    const res = await fetch(`${BASE}/v1/chat/completions`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ model: MODEL, temperature: 0, max_tokens: flow === "summary" ? 40 : 4, messages: [{ role: "user", content: prompt }] }),
      signal: AbortSignal.timeout(60000),
    });
    if (!res.ok) return { ms: Date.now() - t0, err: `http ${res.status}` };
    const j = await res.json();
    const out = (j.choices?.[0]?.message?.content ?? "").trim();
    return { ms: Date.now() - t0, out, empty: out === "" };
  } catch (e) { return { ms: Date.now() - t0, err: e.message }; }
}

const pctl = (a, p) => a.length ? a.sort((x, y) => x - y)[Math.min(a.length - 1, Math.floor((a.length - 1) * p))] : 0;
const all = [];
console.error("warming...");
const w0 = Date.now();
const warm = await call("should-respond", readFileSync(join(here, "prompts", "should-respond.md"), "utf8"), { message: "ping" });
console.log(`cold warm-up call: ${warm.ms}ms ${warm.err ?? warm.out}`);

for (const flow of flowsUse) {
  const tpl = readFileSync(join(here, "prompts", flow + ".md"), "utf8");
  const cases = readFileSync(join(here, "dataset", flow + ".jsonl"), "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
  const ms = [], errs = [];
  for (let r = 0; r < REPS; r++) {
    for (const c of cases) {
      const res = await call(flow, tpl, c);
      if (res.err || res.empty) errs.push(`${res.err ?? "empty"} @ ${res.ms}ms`); else ms.push(res.ms);
      process.stderr.write(`  ${flow} rep${r + 1} ${res.ms}ms\n`);
    }
  }
  all.push(...ms);
  console.log(`\n${flow}: n=${ms.length} errors=${errs.length}`);
  if (errs.length) console.log("   " + errs.slice(0, 5).join(", "));
  console.log(`   p50=${pctl(ms, 0.5)}ms p90=${pctl(ms, 0.9)}ms p95=${pctl(ms, 0.95)}ms p99=${pctl(ms, 0.99)}ms max=${Math.max(...ms)}ms`);
}
console.log(`\nALL FLOWS n=${all.length} p50=${pctl(all, 0.5)}ms p90=${pctl(all, 0.9)}ms p95=${pctl(all, 0.95)}ms p99=${pctl(all, 0.99)}ms max=${Math.max(...all)}ms`);
for (const ceil of [2000, 4000, 6000, 8000, 10000]) {
  const cut = all.filter((m) => m > ceil).length;
  console.log(`  ceiling ${(ceil / 1000).toFixed(0)}s -> ${(all.length - cut) / all.length * 100}% of calls succeed first try`);
}
console.log(`\nworst wall-clock with attempts sharing a budget: budget 8s/12s/15s is what a message can block the bridge for.`);
