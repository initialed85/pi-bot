#!/usr/bin/env node
// Pull the deployed bridge's reply-gate decisions and render the table Ed reviews
// before flipping the gate to enforce.
//
//   node gate-logs.mjs [--tail 5000] [--since 24h] [--context home-dev] [--ns slopyphus] [--json]
//                       [--pod <name>] [--archive <path-append-jsonl>]
//
// `--pod` reads one specific pod (use it on the OLD pod before a roll, whose logs are then
// destroyed); `--archive` appends the raw lines to a file OUTSIDE this repo so a rollout cannot
// delete the evidence. Live transcripts are personal data - never archive them into a public tree.
//
// Reads live pod logs via kubectl (authoritative; no dependency on log shipping).
// Dry-run logs every decision it WOULD have made, so the "would have suppressed"
// column is the actual risk of enforce - that is the list to read.
import { spawnSync } from "node:child_process";
import { appendFileSync, readFileSync } from "node:fs";

const args = process.argv.slice(2);
const flag = (name, dflt) => {
  const i = args.indexOf("--" + name);
  return i >= 0 ? args[i + 1] : dflt;
};
const ctx = flag("context", "home-dev");
const ns = flag("ns", "slopyphus");
const tail = Number(flag("tail", 5000));
const since = flag("since", "24h");
const pod = flag("pod", null);
const archive = flag("archive", null);

let raw;
if (args.includes("--stdin")) {
  raw = readFileSync(0, "utf8");
} else {
  const target = pod ? `pod/${pod}` : "deploy/pi-bot-bridge";
  const out = spawnSync("kubectl", ["--context", ctx, "-n", ns, "logs", target,
    `--tail=${tail}`, `--since=${since}`, "--all-containers"], { encoding: "utf8", maxBuffer: 64 << 20 });
  if (out.status !== 0) {
    console.error("kubectl logs failed:", (out.stderr || "").slice(0, 400));
    process.exit(1);
  }
  raw = out.stdout;
}

if (archive) appendFileSync(archive, raw.endsWith("\n") ? raw : raw + "\n");

const decisions = [], ignored = [];
let warm = null, earliest = null;
for (const line of raw.split("\n")) {
  if (!line.startsWith("{")) continue;
  let j; try { j = JSON.parse(line); } catch { continue; }
  if (j.time && (earliest === null || Date.parse(j.time) < earliest)) earliest = Date.parse(j.time);
  if (j.msg === "reply gate") decisions.push(j);
  else if (j.msg === "group message outside a pi-chat thread; ignoring") ignored.push(j);
  else if (j.msg === "reply gate warm-up failed") warm = `warm-up FAILED: ${j.err}`;
  else if (j.msg === "reply gate warm") warm = "warm-up ok";
}

// Only a clean NO suppresses. Since 54426e0 the bridge FAILS OPEN on classifier
// errors, so the legacy dry-run label `fallback-silent` means "classifier could not
// decide" - enforce answers inline for that. Do not treat it as suppression.
const WOULD_SUPPRESS = new Set(["suppress", "dry-run-suppress"]);
const FAILED = new Set(["fallback-silent", "dry-run-fallback-silent", "fallback-inline", "dry-run-fallback-inline"]);
const rows = decisions.map((d) => {
  // The prompt is a rendered multi-line string; the message is the line after
  // the "Latest message..." header, up to the next blank line.
  const lines = String(d.prompt ?? "").split("\n");
  // should-respond renders `Latest message from <who>:`; thread-request renders
  // `Message: "..."` (which can wrap over lines); summary uses the should-respond shape.
  const i = lines.findIndex((l) => /^Latest message/.test(l));
  let text = "";
  if (i >= 0) {
    const body = [];
    for (const l of lines.slice(i + 1)) { if (l.trim() === "") break; body.push(l.trim()); }
    text = body.join(" ");
  } else {
    const m = lines.findIndex((l) => /^Message: "/.test(l));
    if (m >= 0) {
      const body = [];
      for (const l of lines.slice(m)) { if (/^Answer with exactly/.test(l)) break; body.push(l.trim()); }
      text = body.join(" ").replace(/^Message: "/, "").replace(/"$/, "");
    }
  }
  const s = lines.findIndex((l) => /^Thread summary:/.test(l));
  return {
    time: (d.time ?? "").slice(11, 19), thread: (d.thread ?? "").split("/").pop(),
    flow: d.flow, answer: d.answer, ms: d.latency_ms, action: d.action,
    message: text.trim().slice(0, 70),
    suppresses: WOULD_SUPPRESS.has(d.action),
    failed: FAILED.has(d.action),
    reroutes: /(^|-)thread$/.test(d.action ?? ""),
    fallback: /(^|-)fallback-/.test(d.action ?? ""),
    summary: s >= 0 ? lines.slice(s + 1).find((l) => l.trim() !== "")?.trim().slice(0, 60) ?? "" : "",
  };
});

if (flag("json", null) !== null) { console.log(JSON.stringify({ warm, rows, ignoredCount: ignored.length }, null, 2)); process.exit(0); }

console.log(`warm-up state: ${warm ?? "no warm-up line in the window"}`);
console.log(`group top-level messages ignored with zero model calls: ${ignored.length}`);
const windowMs = (() => {
  const m = /^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?$/.exec(since);
  if (!m) return null;
  return ((+m[1] || 0) * 86400 + (+m[2] || 0) * 3600 + (+m[3] || 0) * 60) * 1000;
})();
if (windowMs && earliest !== null && Date.now() - earliest > windowMs * 0.9) {
  // logs really do cover the window
} else if (windowMs && earliest !== null) {
  const covered = Math.round((Date.now() - earliest) / 60000);
  console.log(`\nEVIDENCE BOUNDARY: asked for ${since}, but these logs only reach back ~${covered} min.`);
  console.log("Anything older is gone - the pod that held it was deleted, and there is no log shipping.");
  console.log("A quiet table therefore means 'the gate was never consulted', not 'the gate behaved'.");
}
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
const fb = rows.filter((r) => r.failed);
if (fb.length) console.log(`   ${fb.length} classifier FAILURE(S)/timeout(s) - the gate did not decide; since 54426e0 enforce answers anyway (fail open):\n` +
  fb.map((r) => `     [${r.time}] ${r.flow} -> ${r.action} after ${r.ms}ms`).join("\n"));
const slow = rows.filter((r) => r.ms >= 1500);
if (slow.length) console.log(`\n   ${slow.length} call(s) >=1500ms - near the 2s deadline; slow, but fail-open means these still get answered`);
