#!/usr/bin/env node
// Pull the deployed bridge's reply-gate decisions and render the table Ed reviews
// before flipping the gate to enforce.
//
//   node gate-logs.mjs [--tail 5000] [--since 24h] [--context home-dev] [--ns slopyphus] [--json]
//
// Reads live pod logs via kubectl (authoritative; no dependency on log shipping).
// Dry-run logs every decision it WOULD have made, so the "would have suppressed"
// column is the actual risk of enforce - that is the list to read.
import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";

const args = process.argv.slice(2);
const flag = (name, dflt) => {
  const i = args.indexOf("--" + name);
  return i >= 0 ? args[i + 1] : dflt;
};
const ctx = flag("context", "home-dev");
const ns = flag("ns", "slopyphus");
const tail = Number(flag("tail", 5000));
const since = flag("since", "24h");

let raw;
if (args.includes("--stdin")) {
  raw = readFileSync(0, "utf8");
} else {
  const out = spawnSync("kubectl", ["--context", ctx, "-n", ns, "logs", "deploy/pi-bot-bridge",
    `--tail=${tail}`, `--since=${since}`, "--all-containers"], { encoding: "utf8", maxBuffer: 64 << 20 });
  if (out.status !== 0) {
    console.error("kubectl logs failed:", (out.stderr || "").slice(0, 400));
    process.exit(1);
  }
  raw = out.stdout;
}

const decisions = [], ignored = [];
let warm = null;
for (const line of raw.split("\n")) {
  if (!line.startsWith("{")) continue;
  let j; try { j = JSON.parse(line); } catch { continue; }
  if (j.msg === "reply gate") decisions.push(j);
  else if (j.msg === "group message outside a pi-chat thread; ignoring") ignored.push(j);
  else if (j.msg === "reply gate warm-up failed") warm = `warm-up FAILED: ${j.err}`;
  else if (j.msg === "reply gate warm") warm = "warm-up ok";
}

const WOULD_SUPPRESS = new Set(["suppress", "fallback-silent", "dry-run-suppress", "dry-run-fallback-silent"]);
const rows = decisions.map((d) => {
  // The prompt is a rendered multi-line string; the message is the line after
  // the "Latest message..." header, up to the next blank line.
  const lines = String(d.prompt ?? "").split("\n");
  const i = lines.findIndex((l) => /^Latest message/.test(l));
  let text = "";
  if (i >= 0) text = lines.slice(i + 1).find((l) => l.trim() !== "") ?? "";
  const s = lines.findIndex((l) => /^Thread summary:/.test(l));
  return {
    time: (d.time ?? "").slice(11, 19), thread: (d.thread ?? "").split("/").pop(),
    flow: d.flow, answer: d.answer, ms: d.latency_ms, action: d.action,
    message: text.trim().slice(0, 70),
    suppresses: WOULD_SUPPRESS.has(d.action),
    reroutes: /(^|-)thread$/.test(d.action ?? ""),
    summary: s >= 0 ? lines.slice(s + 1).find((l) => l.trim() !== "")?.trim().slice(0, 60) ?? "" : "",
  };
});

if (flag("json", null) !== null) { console.log(JSON.stringify({ warm, rows, ignoredCount: ignored.length }, null, 2)); process.exit(0); }

console.log(`warm-up state: ${warm ?? "no warm-up line in the window"}`);
console.log(`group top-level messages ignored with zero model calls: ${ignored.length}`);
if (!rows.length) {
  console.log(`\nNo gate decisions in this window. Expected: an untagged DM (flow=thread-request) and`);
  console.log(`untagged follow-ups inside a thread pi-chat was tagged into (flow=should-respond).`);
  process.exit(0);
}
console.log(`\ntime     flow              answer   ms     action                 message`);
for (const r of rows)
  console.log(`${r.time}  ${r.flow.padEnd(17)} ${(r.answer ?? "").padEnd(8)} ${String(r.ms).padStart(4)}  ${(r.action ?? "").padEnd(20)} ${r.message}`);
const would = rows.filter((r) => r.suppresses);
const rerouted = rows.filter((r) => r.reroutes);
console.log(`\n== ${rows.length} decisions`);
if (would.length) {  console.log(`   ${would.length} would have gone SILENT under enforce:`);
  for (const r of would) console.log(`     [${r.time}] "${r.message}"  (summary: ${r.summary})`);
} else {
  console.log(`   none would have gone silent under enforce`);
}
if (rerouted.length) {
  console.log(`   ${rerouted.length} would have been MOVED into a new side thread instead of replying in place:`);
  for (const r of rerouted) console.log(`     [${r.time}] "${r.message}"`);
}
const answered = rows.filter((r) => !r.suppresses && !r.reroutes).length;
console.log(`   (${answered} answered in place)`);
const slow = rows.filter((r) => r.ms >= 1500);
if (slow.length) console.log(`\n   ${slow.length} call(s) >=1500ms - approaching the 2s deadline (enforce would fall back on these)`);
