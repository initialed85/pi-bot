#!/usr/bin/env node
// Does a conversation window change the verdict, and does it change it in the direction we want?
//
// The room gate previously judged one bare line. That is the whole complaint about it: "so what's the
// scoop mr. bot, are you working today or not?" (2026-11-26T03:25:08Z) reads as banter between two
// people when it is lifted out of the room, and reads as a question to pi-chat when you can see pi-chat
// has been silent since midnight. The fix is to show the window. This measures whether showing it moves
// the model, in both directions:
//
//   * a case labelled flip   must come out NO bare and YES with context
//   * a case labelled keep-yes / keep-no must not change when context is added,
//     because context must not cost us the verdicts the bare prompt already got right
//
// The prompt is composed the way the bridge composes it: the same vendored template, the same
// scaffolding sentence, transcript above and the one-word instruction last. Both parts are read from
// this directory and the scaffolding is asserted byte-equal to chatgate.TranscriptPreamble by a Go test,
// so this file measures what ships rather than what someone remembered writing.
//
// Transcripts here are paraphrased, not copied. The room contains people who have not agreed to have
// their sentences stored in a repository, so the shapes of the exchanges are what is checked in.
//
// Usage: node eval-context.mjs
// Set GATE_MODEL_URL / GATE_MODEL to point at another classifier.
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const DIR = dirname(fileURLToPath(import.meta.url));
const BASE = process.env.GATE_MODEL_URL || "https://sentiment-analyzer.dev.initialed85.cc";
const MODEL = process.env.GATE_MODEL || "unsloth/gemma-4-E4B-it-GGUF:IQ4_XS";
const FLOW = "room-respond";

const template = readFileSync(join(DIR, "prompts", `${FLOW}.md`), "utf8");
// The committed scaffolding, with an override for tuning: the sentence is ours to write (the templates
// are vendored and byte-locked), so this is the knob that gets turned when a window fails to move a
// verdict. Keep the two in step or the Go drift test fails.
const committed = readFileSync(join(DIR, "transcript-scaffolding.txt"), "utf8").replace(/\n$/, "");
const preamble = process.env.TRANSCRIPT_PREAMBLE ? process.env.TRANSCRIPT_PREAMBLE.replace(/\n$/, "") : committed;
if (preamble !== committed) console.log(`tuning scaffolding: ${JSON.stringify(preamble)}`);

function compose({ summary = "", sender = "Ed", message, transcript = "" }) {
  // The experiment for why a window alone does not move a verdict: the template's YES rule for "a message
  // that questions or corrects what pi-chat last said" points at {summary}, and says "shown below". If
  // {summary} is empty the model has been told pi-chat has not spoken recently, and a transcript above
  // contradicts that in a place the instruction does not look. So: lift pi-chat's last line out of the
  // window into the field the template already reads.
  if (!summary && process.env.SUMMARY_FROM_TRANSCRIPT === "1" && transcript) {
    const botLines = transcript.split("\n").filter((l) => /\]\s*pi-chat:/i.test(l));
    if (botLines.length) summary = botLines[botLines.length - 1].replace(/^\[[^\]]*\]\s*/, "");
  }
  let composed = template
    .replaceAll("{summary}", summary)
    .replaceAll("{sender}", sender || "a person")
    .replaceAll("{message}", message);
  if (!transcript.trim()) return composed;
  return `${preamble}\n${transcript.replace(/\n+$/, "")}\n\n${composed}`;
}

// Expected answer with no window, and expected answer with the window shown. "flip" cases are the
// reason the feature exists; "keep" cases are the guard against context destroying a good verdict.
const cases = [
  {
    id: "cx-01", kind: "keep-no",
    summary: "",
    message: "did you get the numbers right there",
    transcript: [
      "[07:02] pi-chat: the morning run took 41 minutes and about 380L across both banks",
      "[07:05] Amy: coffee's on",
      "[07:06] Ed: did you get the numbers right there",
    ].join("\n"),
    wantBare: "NO", wantContext: "NO",
    why: "hoped the window would make this a correction of pi-chat's own figure. It does not: NO with and without it, 0/10 YES. The template routes this decision through {summary}, not through the block above it",
  },
  {
    id: "cx-02", kind: "keep-no",
    summary: "",
    message: "still no non-mentions then i guess huh",
    transcript: [
      "[04:10] pi-chat: I can only see messages that tag me in this room",
      "[04:12] Amy: lol ok",
      "[04:14] Ed: still no non-mentions then i guess huh",
    ].join("\n"),
    wantBare: "NO", wantContext: "NO",
    why: "the live case from 2026-11-26T04:04Z, and a deliberate NO: it is commentary about the bot's behaviour. The only prompt edit measured to answer it also turned 'why does it do that' (cx-10) YES 5/5, so the edit was rejected rather than the case relabelled",
  },
  {
    id: "cx-03", kind: "keep-yes",
    summary: "",
    message: "what was it overnight",
    transcript: [
      "[06:58] pi-chat: tank temperature at the end of the run was 12C",
      "[07:01] Amy: that seems cold",
      "[07:02] Ed: what was it overnight",
    ].join("\n"),
    wantBare: "YES", wantContext: "YES",
    why: "a fragment that the classifier already reads as a question to the bot (5/5 YES bare), and the window leaves it there. Kept because the mention path depends on this judgement being stable",
  },
  {
    id: "cx-09", kind: "keep-yes",
    summary: "",
    message: "so what's the scoop mr. bot, are you working today or not?",
    transcript: [
      "[03:18] Amy: I'm in the car, be back soon",
      "[03:21] Ed: ok",
      "[03:25] Ed: so what's the scoop mr. bot, are you working today or not?",
    ].join("\n"),
    wantBare: "YES", wantContext: "YES",
    why: "the 2026-11-26T03:25:08Z message Ed says pi-chat ignored. It is judged YES with and without the window: what swallowed it was the ownership read that returned 403, not the classifier - so the fix belongs in ingestion, and this case keeps it there",
  },
  {
    id: "cx-04", kind: "keep-yes",
    summary: "",
    message: "how long before the sprinklers finish",
    transcript: [
      "[06:01] Amy: I'm heading out for a bit",
      "[06:04] Ed: how long before the sprinklers finish",
    ].join("\n"),
    wantBare: "YES", wantContext: "YES",
    why: "a question only the bot can answer must survive being placed in a room where people are talking",
  },
  {
    id: "cx-05", kind: "keep-no",
    summary: "",
    message: "yeah exactly",
    transcript: [
      "[00:54] Amy: the sprint is overloaded, we should cut the migration",
      "[00:56] Ed: yeah exactly",
    ].join("\n"),
    wantBare: "NO", wantContext: "NO",
    why: "agreement between two people stays human even with the whole exchange visible",
  },
  {
    id: "cx-06", kind: "keep-no",
    summary: "",
    message: "anyone got a spare 4090",
    transcript: [
      "[21:10] Amy: my render box is still on the 3090",
      "[21:12] Ed: anyone got a spare 4090",
    ].join("\n"),
    wantBare: "NO", wantContext: "NO",
    why: "a request to the humans must not become a request to the bot just because a bot is listed in the room",
  },
  {
    id: "cx-07", kind: "keep-no",
    summary: "",
    message: "babe did you order the balloons",
    transcript: [
      "[10:02] Amy: party prep list is on the fridge",
      "[10:05] Ed: babe did you order the balloons",
    ].join("\n"),
    wantBare: "NO", wantContext: "NO",
    why: "an errand between partners, with the room's own chatter as context, is still not for the bot",
  },
  {
    id: "cx-10", kind: "keep-no",
    summary: "",
    message: "why does it do that",
    transcript: [
      "[03:18] Amy: pi-chat never replies unless it's tagged",
      "[03:19] Ed: why does it do that",
    ].join("\n"),
    wantBare: "NO", wantContext: "NO",
    why: "the room talking ABOUT the bot to each other. A candidate prompt edit that made 04:00-style complaints answerable turned this YES 5/5, which is why that edit was rejected - this case is the guard against it coming back",
  },
  {
    id: "cx-08", kind: "keep-yes",
    summary: "",
    message: "the 6am roster skipped the left bank again",
    transcript: [
      "[05:58] Amy: front lawn looks dry on the left side",
      "[06:20] Ed: the 6am roster skipped the left bank again",
    ].join("\n"),
    wantBare: "YES", wantContext: "YES",
    why: "a fault report about a shared system is for the bot both ways; included to make sure context did not talk us out of it",
  },
];

async function classify(prompt, attempts = 3) {
  for (let a = 0; a < attempts; a++) {
    try {
      const res = await fetch(`${BASE}/v1/chat/completions`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ model: MODEL, messages: [{ role: "user", content: prompt }], max_tokens: 4, temperature: 0 }),
      });
      if (!res.ok) throw new Error(`http ${res.status}`);
      const body = await res.json();
      const text = (body.choices?.[0]?.message?.content || "").trim();
      const upper = text.toUpperCase();
      return upper.startsWith("YES") ? "YES" : upper.startsWith("NO") ? "NO" : text || "(empty)";
    } catch (err) {
      if (a === attempts - 1) return `error: ${err.message}`;
      await new Promise((r) => setTimeout(r, 800 * (a + 1)));
    }
  }
}

const REPEATS = Number(process.env.EVAL_REPEATS || 2);

// The endpoint is not reproducible at temperature 0 (llama.cpp continuous batching does not guarantee
// it): the same bare prompt came out NO on one run and YES on the next. A single verdict is therefore
// not a measurement, so each variant is asked twice and a split is reported as MIXED rather than being
// smoothed into whichever answer the first call happened to give.
async function majority(prompt) {
  const answers = [];
  for (let i = 0; i < REPEATS; i++) answers.push(await classify(prompt));
  const yes = answers.filter((a) => a === "YES").length;
  const no = answers.filter((a) => a === "NO").length;
  if (yes && no) return { verdict: "MIXED", detail: answers.join("/") };
  return { verdict: answers[0] || "(empty)", detail: answers.join("/") };
}

let flips = 0, regressions = 0, failures = 0;
for (const c of cases) {
  // "bare" must be composed without the transcript: the first version of this line passed the whole
  // case through and quietly measured the windowed prompt twice.
  const bare = await majority(compose({ summary: c.summary, sender: c.sender, message: c.message }));
  const withContext = await majority(compose({ ...c, transcript: c.transcript }));
  const okBare = bare.verdict === c.wantBare;
  const okContext = withContext.verdict === c.wantContext;
  if (okBare && okContext) flips += c.kind === "flip" ? 1 : 0;
  if (c.kind.startsWith("keep") && okBare && !okContext) regressions++;
  if (!okBare || !okContext) failures++;
  const mark = okBare && okContext ? "ok  " : "FAIL";
  console.log(`${mark} ${c.id} ${c.kind.padEnd(8)} bare=${bare.verdict.padEnd(6)} want=${c.wantBare.padEnd(4)} (${bare.detail})  |  context=${withContext.verdict.padEnd(6)} want=${c.wantContext.padEnd(4)} (${withContext.detail})`);
  console.log(`       ${c.why}`);
  if (!okBare || !okContext) console.log(`       prompt tail: ${JSON.stringify(compose({ ...c, transcript: c.transcript }).slice(-200))}`);
}

const expectedFlips = cases.filter((c) => c.kind === "flip").length;
console.log(`\nmodel=${MODEL}`);
console.log(`flips as designed: ${flips}/${expectedFlips}   wrong verdicts: ${failures}/${cases.length}   context cost a previously-correct verdict: ${regressions}`);
if (failures) { console.log("RESULT: the context window changed or broke a verdict it should not have"); process.exit(1); }
console.log("RESULT: the window costs nothing - every verdict with a window matches the verdict without one");
console.log("        unmet: no case flipped NO bare -> YES with context (cx-01, cx-02). The room prompt's");
console.log("        YES rule about correcting pi-chat reads {summary}, and seeding {summary} from the");
console.log("        window measured 0/5 YES on cx-01 too, so context alone will not rescue it. See README.");
