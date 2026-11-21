#!/usr/bin/env node
// Drift guard: the bridge embeds copies of these prompts (internal/chatgate/prompts/).
// Prompt text is owned by eval/chat-gate/prompts/; the vendored copy must match exactly,
// otherwise the measured accuracy no longer describes what ships.
//
//   node check-vendored.mjs            # compare, exit 1 on drift
//   node check-vendored.mjs --write     # re-vendor from the canonical copies
import { readFileSync, readdirSync, existsSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

const here = dirname(new URL(import.meta.url).pathname);
const canonicalDir = join(here, "prompts");
const repoRoot = resolve(here, "..", "..");
const vendoredDir = join(repoRoot, "internal", "chatgate", "prompts");
const write = process.argv.includes("--write");

if (!existsSync(vendoredDir)) {
  console.error(`vendored prompt dir not found: ${vendoredDir}`);
  process.exit(2);
}

const files = readdirSync(canonicalDir).filter((f) => f.endsWith(".md"));
let drift = 0;
for (const f of files) {
  const src = readFileSync(join(canonicalDir, f));
  const destPath = join(vendoredDir, f);
  if (write) { writeFileSync(destPath, src); console.log(`vendored ${f}`); continue; }
  if (!existsSync(destPath)) { console.log(`MISSING  internal/chatgate/prompts/${f}`); drift++; continue; }
  const dst = readFileSync(destPath);
  if (src.equals(dst)) console.log(`ok       ${f} (${src.length} bytes)`);
  else { console.log(`DRIFT    ${f}: canonical ${src.length}B vs vendored ${dst.length}B`); drift++; }
}
if (drift) {
  console.error(`\n${drift} prompt file(s) out of sync. Prompts are owned by eval/chat-gate/prompts/:
  edit there, re-run the eval (node eval.mjs <name>), then: node check-vendored.mjs --write
  Never edit internal/chatgate/prompts/ directly.`);
  process.exit(1);
}
console.log(`\n== all ${files.length} vendored prompts match canonical`);
