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

// Two eras of failure handling live in these logs, and the labels are the only way to tell them
// apart. Before the retry change, enforce FAILED OPEN: any classifier error answered inline
// (`fallback-inline`) and the legacy dry-run label `fallback-silent` meant "would have stayed quiet".
// From the retry change onward, a classifier that cannot decide answers a 1:1 message inline but
// SILENCES a shared-thread message (`fallback-suppress`). `fallback-suppress` is a failure, not a
// clean NO - do not fold it into the suppression count, because it means the gate never decided.
const WOULD_SUPPRESS = new Set(["suppress", "dry-run-suppress"]);
const FAILED = new Set([
  "fallback-silent", "dry-run-fallback-silent",
  "fallback-inline", "dry-run-fallback-inline",
  "fallback-suppress", "dry-run-fallback-suppress",
]);
const rows = decisions.map((d) => {
  // The prompt is a rendered multi-line string; the message is the line after
  // the "Latest message..." header, up to the next blank line.
  const lines = String(d.prompt ?? "").split("\n");
  // The message line, whatever the prompt version. Live logs proved this extractor stale: the prompts
  // now name the author on the same line (`Message (from a@b): "..."`), so a header-then-body reader
  // found a header, then a blank line, and rendered an empty table - a review tool that shows no
  // messages is worse than no review tool, because it looks like the gate judged nothing.
  const startIdx = lines.findIndex((l) => /^(Latest message|Message)\b/.test(l.trim()));
  let text = "";
  let fromPrompt = "";
  if (startIdx >= 0) {
    const body = [lines[startIdx].trim()];
    for (const l of lines.slice(startIdx + 1)) {
      const t = l.trim();
      if (t === "" || /^(Answer with exactly|Thread summary:|Pi-chat.s most recent)/.test(t)) break;
      body.push(t);
    }
    const joined = body.join(" ");
    const quoted = joined.match(/"([\s\S]*)"/);
    text = (quoted ? quoted[1] : joined.replace(/^[^:]*:\s*/, "")).trim();
    // Attribution without waiting for a redeploy: the prompt carries the sender's email, and the
    // deployed log line does not, so a per-person room review can be built from the prompt itself.
    fromPrompt = joined.match(/\(from ([^)]+@[^)]+)\)/)?.[1] ?? "";
  }
  const s = lines.findIndex((l) => /^Thread summary:/.test(l));
  return {
    time: (d.time ?? "").slice(11, 19), thread: (d.thread ?? "").split("/").pop(),
    flow: d.flow, answer: d.answer, ms: d.latency_ms, action: d.action,
    // Pre-2026-11-25 logs do not carry these; the room verdicts then have to be attributed by hand
    // against the bridge's "room sender identified" line, which shares the thread.
    by: d.user ? String(d.user).split("@")[0] : fromPrompt.split("@")[0],
    space: d.space ?? "",
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

// The room flows are the ones that put text in front of other people, so they get their own breakdown by
// person: an untagged message from one allowlisted person answering is a different decision from another.
const room = rows.filter((r) => r.flow === "room-respond");
if (room.length) {
  const people = new Map();
  for (const r of room) {
    const k = r.by || "unattributed";
    const p = people.get(k) ?? { would: [], silent: [] };
    (r.suppresses ? p.silent : p.would).push(r);
    people.set(k, p);
  }
  console.log(`\n== room-respond judgements, by person (dry-run: judged, never posted)`);
  for (const [person, p] of people) {
    console.log(`   ${person}: ${p.would.length} would answer, ${p.silent.length} would stay silent`);
    for (const r of p.would) console.log(`     WOULD ANSWER [${r.time}] in ${r.space || "unknown space"}: "${r.message}"`);
    for (const r of p.silent) console.log(`     silent     [${r.time}] "${r.message}"`);
  }
}
const fb = rows.filter((r) => r.failed);
if (fb.length) console.log(`   ${fb.length} classifier FAILURE(S)/timeout(s) - the gate did not decide; pre-retry logs answered those inline, current enforce silences them unless the message was a 1:1:\n` +
  fb.map((r) => `     [${r.time}] ${r.flow} -> ${r.action} after ${r.ms}ms`).join("\n"));
const slow = rows.filter((r) => (r.ms ?? 0) >= 1500);
if (slow.length) console.log(`\n   ${slow.length} call(s) >=1500ms - a cold model; the current schedule is 3s then 6s then 12s, so these are retried rather than cut off at 2s`);
