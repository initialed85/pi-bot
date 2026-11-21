#!/usr/bin/env node
// Re-vendor the canonical prompts into the bridge's go:embed directory.
//
// The AUTHORITATIVE drift check is internal/chatgate/prompt_drift_test.go, which compares
// the embedded copies against pi-bot master over HTTP and fails `go test ./...` on mismatch.
// This script is only the write side of that loop, so a prompt edit here has one obvious
// way to land in the binary. Equality itself is asserted in exactly one place.
//
//   node revendor.mjs          # copy prompts/*.md -> internal/chatgate/prompts/
//
// Then bump the evaluated revision recorded in internal/chatgate (VendoredPromptSource)
// and re-run: go test ./internal/chatgate/ && node eval.mjs <name>
import { readFileSync, readdirSync, writeFileSync, mkdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

const here = dirname(new URL(import.meta.url).pathname);
const dest = resolve(here, "..", "..", "internal", "chatgate", "prompts");
mkdirSync(dest, { recursive: true });
for (const f of readdirSync(join(here, "prompts")).filter((x) => x.endsWith(".md"))) {
  writeFileSync(join(dest, f), readFileSync(join(here, "prompts", f)));
  console.log(`vendored ${f}`);
}
console.log(`\nnow: go test ./internal/chatgate/  (asserts equality) and re-run the eval.`);
