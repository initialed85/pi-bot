#!/usr/bin/env node
// Offline eval for the pi-chat gating small model (sentiment-analyzer.dev).
// Usage: node eval.mjs [should-respond|thread-request|summary] [--limit N] [--dry-labels]
// Reads dataset/<name>.jsonl and prompts/<name>.md ({summary},{sender},{message} placeholders).
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";

const BASE = process.env.GATE_MODEL_URL || "https://sentiment-analyzer.dev.initialed85.cc";
const MODEL = process.env.GATE_MODEL || "unsloth/gemma-4-E4B-it-GGUF:IQ4_XS";
const which = process.argv[2] || "should-respond";
const limitArg = process.argv.indexOf("--limit");
const limit = limitArg > 0 ? Number(process.argv[limitArg + 1]) : Infinity;
const here = dirname(new URL(import.meta.url).pathname);

const tpl = readFileSync(join(here, "prompts", which + ".md"), "utf8");
const cases = readFileSync(join(here, "dataset", which + ".jsonl"), "utf8")
  .split("\n").filter(Boolean).map((l) => JSON.parse(l)).slice(0, limit);

async function classify(c, attempt = 0) {
  const prompt = tpl
    .replaceAll("{summary}", c.summary ?? "")
    .replaceAll("{sender}", c.sender ?? "a person")
    .replaceAll("{message}", c.message);
  const t0 = Date.now();
  try {
    const res = await fetch(`${BASE}/v1/chat/completions`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        model: MODEL, temperature: 0, max_tokens: which === "summary" ? 40 : 4,
        messages: [{ role: "user", content: prompt }],
      }),
      signal: AbortSignal.timeout(60000),
    });
    if (!res.ok) throw new Error(`http ${res.status}`);
    const j = await res.json();
    return { out: (j.choices?.[0]?.message?.content ?? "").trim(), ms: Date.now() - t0 };
  } catch (e) {
    if (attempt < 1) return classify(c, attempt + 1);
    return { out: `<ERROR ${e.message}>`, ms: Date.now() - t0 };
  }
}

const expected = which === "thread-request" ? ["THREAD", "INLINE"] : ["YES", "NO"];
// Warm the model (cold pods skew latency badly).
console.error("warming model...");
const warm = Date.now();
await classify({ message: "ping" });
console.error(`warm after ${Date.now() - warm}ms`);
let ok = 0, failures = [];
const bySplit = {};
const lat = [];
for (const c of cases) {
  const { out, ms } = await classify(c);
  lat.push(ms);
  const norm = out.toUpperCase();
  const pass = which === "summary"
    ? norm.split(/\s+/).length <= 30 && norm.includes((c.mustInclude ?? "").toUpperCase())
    : norm === c.label;
  const key = c.split ?? "all";
  bySplit[key] ??= { ok: 0, n: 0 };
  bySplit[key].n++;
  if (pass) { ok++; bySplit[key].ok++; } else failures.push({ id: c.id, label: c.label, got: out });
  console.log(`${pass ? "PASS" : "FAIL"} ${c.id} [want ${c.label}] -> ${out} (${ms}ms)`);
}
const mean = lat.reduce((a, b) => a + b, 0) / lat.length;
console.log(`\n== ${which}: ${ok}/${cases.length} correct (${((ok / cases.length) * 100).toFixed(1)}%), mean ${mean.toFixed(0)}ms`);
for (const [k, v] of Object.entries(bySplit))
  console.log(`   ${k}: ${v.ok}/${v.n}`);
if (failures.length) console.log("   failures:", JSON.stringify(failures));
