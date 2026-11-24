#!/usr/bin/env node
// Does Google let a *user*-authorised Chat read see unmentioned room messages, where the
// app credential is refused? That is the question this answers, because the user path is the
// one that might not need a paid Workspace organization with an admin to approve an app.
//
// Run it on your OWN machine (it needs a browser for consent and a loopback port for the
// redirect). It never prints a token and never writes one anywhere except a 0600 local file.
//
//   node chat-user-oauth-probe.mjs --client ./oauth-client.json
//   node chat-user-oauth-probe.mjs --client ./oauth-client.json --space spaces/AAQAxKMXxjk
//   node chat-user-oauth-probe.mjs --client ./oauth-client.json --include-group spaces/AAAA1pNBS1A \
//     --topic pi-chat-probe
//   node chat-user-oauth-probe.mjs ... --include-group spaces/AAAA1pNBS1A --topic pi-chat-probe --create-subscription
//
// What the official docs say as of 2026-11-24 (developers.google.com/workspace/events/guides/events-chat),
// which is why the verdict has to come from this probe rather than from the earlier refusals:
//   * A Space subscription accepts USER authentication, and the authorizing user only has to be a
//     member of the space "through their Google Workspace or Google Account" - a plain consumer
//     Google Account counts, so a Workspace organization is not required for THIS target resource.
//   * "All spaces for a user" (//chat.googleapis.com/spaces/-) is user-auth only.
//   * The Customer target resource is the one that needs an organization, and it demands at least
//     one Enterprise SKU license plus administrator approval. That route is dead for us.
//   * Requirements that are ours to satisfy, not Google's to refuse: the project must have billing
//     enabled, the Workspace Events API and the Chat API must be enabled, and each event type needs
//     its scope (chat.messages.readonly is a Restricted scope, so an unverified client must be in
//     testing mode with the user listed as a test user).
//   * A subscription needs notificationEndpoint.pubsubTopic, so validate-only attempts still have
//     to name a topic that exists. --ensure-topic creates a throwaway one with the same user
//     token (no gcloud, no console). Never point it at the bridge's live topic; that would feed
//     real room traffic into the gate, so the probe refuses the name rather than trusting the
//     operator to remember.
// ./oauth-client.json is what you downloaded from the console when you created the OAuth
// client, or hand-written: {"client_id":"...","client_secret":"..."}
//
// --include-group reads a room with other people in it using YOUR authority. Only pass it if
// you have decided that is acceptable; the default set of probes is spaces you created alone.
import fs from "node:fs";
import http from "node:http";
import crypto from "node:crypto";
import { exec } from "node:child_process";

const args = process.argv.slice(2);
const flag = (name, dflt) => {
  const i = args.indexOf("--" + name);
  return i >= 0 ? args[i + 1] : dflt;
};

const CLIENT_PATH = flag("client", "./oauth-client.json");
const PORT = Number(flag("port", 8484));
const REDIRECT = `http://localhost:${PORT}`;
const TOKEN_PATH = flag("token", "./chat-user-token.json");
const EXTRA_SPACE = flag("space", null);
const GROUP = flag("include-group", null);
// Required for any subscription attempt: Workspace Events rejects a subscription with no
// notification endpoint, and a validation refusal is not the same answer as an authority refusal.
const TOPIC_PROJECT = flag("topic-project", "home-ops-513116");
const TOPIC = flag("topic", null);
const CREATE = args.includes("--create-subscription");

const SCOPES = [
  "https://www.googleapis.com/auth/chat.messages.readonly",
  "https://www.googleapis.com/auth/chat.spaces.readonly",
  // The events API has NO scope of its own. Google refused the whole consent request with
  // invalid_scope, invalid=[.../workspaceevents], and subscriptions.create documents that it is
  // authorised by the chat.* scopes above. So the Chat scopes do double duty: read and subscribe.
  "https://www.googleapis.com/auth/pubsub",
];

// The bridge's live topic. Pointing a Workspace Events subscription at it would deliver real
// room traffic into the reply gate before anyone has told the room, so this name is refused.
const LIVE_TOPIC = flag("live-topic", "pi-chat-events");

function readJson(p) {
  return JSON.parse(fs.readFileSync(p, "utf8"));
}

/** Loopback redirect handler: resolves with the code Google sends back. */
function waitForCode() {
  return new Promise((resolve, reject) => {
    const srv = http.createServer((req, res) => {
      const u = new URL(req.url, REDIRECT);
      if (u.pathname !== "/") {
        res.writeHead(404).end("nope");
        return;
      }
      if (u.searchParams.get("error")) {
        res.writeHead(400, { "content-type": "text/plain" }).end("Consent failed - see terminal.");
        reject(new Error(u.searchParams.get("error_description") || u.searchParams.get("error")));
        srv.close();
        return;
      }
      const code = u.searchParams.get("code");
      res.writeHead(200, { "content-type": "text/plain" }).end(
        "Consent captured. Close this tab and go back to the terminal.",
      );
      resolve(code);
      srv.close();
    });
    srv.on("error", reject);
    srv.listen(PORT, "127.0.0.1", () => console.log(`listening on ${REDIRECT} for the redirect`));
  });
}

async function exchange(client, code) {
  const r = await fetch("https://oauth2.googleapis.com/token", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      client_id: client.client_id,
      client_secret: client.client_secret,
      code,
      grant_type: "authorization_code",
      redirect_uri: REDIRECT,
    }),
  });
  const j = await r.json();
  if (!j.refresh_token) throw new Error("token exchange failed: " + JSON.stringify(j).slice(0, 300));
  fs.writeFileSync(
    TOKEN_PATH,
    JSON.stringify({ refresh_token: j.refresh_token, scope: j.scope, obtainedAt: new Date().toISOString() }, null, 2),
    { mode: 0o600 },
  );
  // The granted scope is part of the report the transport owner needs: a scope Google silently
  // dropped from consent changes what a later 403 means.
  console.log("granted scope: " + (j.scope || "(not returned)"));
  console.log(`refresh token written to ${TOKEN_PATH} (0600). Do not paste it into Chat.`);
  return j.refresh_token;
}

async function accessToken(client, refreshToken) {
  const r = await fetch("https://oauth2.googleapis.com/token", {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      client_id: client.client_id,
      client_secret: client.client_secret,
      refresh_token: refreshToken,
      grant_type: "refresh_token",
    }),
  });
  const j = await r.json();
  if (!j.access_token) throw new Error("refresh failed: " + JSON.stringify(j).slice(0, 300));
  return j;
}

async function get(token, url) {
  const r = await fetch(url, { headers: { authorization: "Bearer " + token } });
  const t = await r.text();
  return { status: r.status, statusText: r.statusText, body: t.replace(/\s+/g, " ").slice(0, 400) };
}

/**
 * Create the throwaway topic if it is missing (idempotent PUT), under the SAME user credential
 * that creates the subscription. Refuses the bridge's live topic outright: a topic that already
 * feeds the gate is not a test target, and the subscription would start delivering real messages.
 */
async function ensureTopic(token, client) {
  if (TOPIC === LIVE_TOPIC) {
    console.log(`\nrefusing --topic ${TOPIC}: that is the bridge's live topic. Pick another name.`);
    process.exit(2);
  }
  const url = `https://pubsub.googleapis.com/v1/projects/${TOPIC_PROJECT}/topics/${TOPIC}`;
  const r = await fetch(url, { method: "PUT", headers: { authorization: "Bearer " + token, "content-type": "application/json" }, body: "{}" });
  const t = (await r.text()).replace(/\s+/g, " ").slice(0, 300);
  console.log(`\n[${r.status}] pubsub PUT topics/${TOPIC} (create if missing)`);
  console.log("     " + t);
  if (!r.ok) throw new Error("topic creation refused: " + t.slice(0, 200));
  console.log("     topic exists. Pub/Sub bills throughput only, first 10 GiB/month free; an idle topic costs nothing.");
  void client;
}

const EVENTS_FILTER = 'eventTypes:"google.workspace.chat.message.v1.created"';

async function main() {
  const client = readJson(CLIENT_PATH);
  let refreshToken = fs.existsSync(TOKEN_PATH) ? readJson(TOKEN_PATH).refresh_token : null;

  if (!refreshToken || args.includes("--reconsent")) {
    const authUrl =
      "https://accounts.google.com/o/oauth2/v2/auth?" +
      new URLSearchParams({
        client_id: client.client_id,
        redirect_uri: REDIRECT,
        response_type: "code",
        scope: SCOPES.join(" "),
        access_type: "offline",
        prompt: "consent",
        state: crypto.randomBytes(8).toString("hex"),
      });
    console.log("\nConsent URL (opening the browser; copy this if it does not appear):\n" + authUrl + "\n");
    exec(`xdg-open ${JSON.stringify(authUrl)} 2>/dev/null || open ${JSON.stringify(authUrl)}`, () => {});
    refreshToken = await exchange(client, await waitForCode());
  }

  const at = await accessToken(client, refreshToken);
  const token = at.access_token;
  if (TOPIC) await ensureTopic(token, client);
  console.log("\nrequested scopes: " + SCOPES.join(", "));
  console.log("scope on this access token: " + (at.scope || "(not returned by Google)"));
  console.log("consent mode hint: " + (at.expires_in ? "token issued; expires_in=" + at.expires_in : "token issued"));
  if (fs.existsSync(TOKEN_PATH)) console.log("scope recorded at consent: " + (readJson(TOKEN_PATH).scope || "(none)"));
  const who = await get(token, "https://chat.googleapis.com/v1/spaces?pageSize=25");
  const seen = [...new Set((who.body.match(/spaces\/[A-Za-z0-9]+/g) || []))];
  report("spaces.list (user auth)", who);
  console.log(`  -> spaces visible to you: ${seen.length ? seen.join(", ") : "none"}`);

  const targets = [EXTRA_SPACE, GROUP].filter(Boolean);
  for (const sp of targets) {
    const one = await get(token, `https://chat.googleapis.com/v1/${sp}`);
    report(`spaces.get ${sp}`, one);
    // This is the exact call the app credential cannot make.
    const list = await get(token, `https://chat.googleapis.com/v1/${sp}/messages?pageSize=5`);
    report("spaces.messages.list", list);
    const ev = await get(
      token,
      `https://chat.googleapis.com/v1/${sp}/spaceEvents?pageSize=5&filter=${encodeURIComponent(EVENTS_FILTER)}`,
    );
    report("spaces.spaceEvents", ev);
    // validateOnly is a query parameter on create, not a field of the Subscription resource.
    const sub = {
      targetResource: `//chat.googleapis.com/${sp}`,
      eventTypes: ["google.workspace.chat.message.v1.created"],
      payloadOptions: { includeResource: true, fieldMask: "message.name,message.text" },
    };
    if (TOPIC) sub.notificationEndpoint = { pubsubTopic: `projects/${TOPIC_PROJECT}/topics/${TOPIC}` };
    const verb = CREATE ? "create (REAL - delivers events to the named topic)" : "validateOnly";
    const we = await fetch(
      `https://workspaceevents.googleapis.com/v1/subscriptions?parent=projects/${TOPIC_PROJECT}` + (CREATE ? "" : "&validateOnly=true"),
      {
        method: "POST",
        headers: { authorization: "Bearer " + token, "content-type": "application/json" },
        body: JSON.stringify(sub),
      },
    );
    const wt = (await we.text()).replace(/\s+/g, " ").slice(0, 400);
    console.log(`\n[${we.status}] workspaceevents ${verb} on ${sp}${TOPIC ? " -> topics/" + TOPIC : " (no --topic given: expect a validation refusal, which proves nothing about authority)"}`);
    console.log("     " + wt);
    if (CREATE && we.ok) console.log("     created for real. Delete it with: DELETE /v1/{name} using this same token, or it keeps delivering until it expires.");
  }

  console.log(`
Read this way:
  * 200 on messages.list / spaceEvents with YOUR credential, while the app credential gets
    403 or 500, means room-wide listening is reachable without paying for Workspace - the
    bot would be reading as you, which is a disclosure decision, not just a technical one.
  * A refusal that still names "Google Workspace organization" or "administrator approval"
    means the user path is closed for a consumer account too, and this whole idea is dead.
  * 403 PERMISSION_DENIED naming a scope means consent did not include it; rerun with
    --reconsent after adding the scope in the console.
  * A 400 naming notificationEndpoint or the topic means the request shape is wrong or the topic
    does not exist yet - that says nothing about permission. Pass --topic <an existing topic>.
  * The answer we are looking for is a 200 on validateOnly with --include-group pointing at the
    friends room. Anything naming an organization, a licence SKU or administrator approval closes
    the user route for good.`);
}

function report(label, r) {
  const mark = r.status === 200 ? "OK  " : `${r.status}`;
  // Full error status/detail matters to the transport owner, so keep the reason phrase too.
  console.log(`\n[${mark}] ${label}${r.status === 200 ? "" : " (" + (r.statusText || "") + ")"}`);
  console.log("     " + r.body.slice(0, 400));
}

main().catch((e) => {
  console.error("\nfailed:", String(e.message || e).slice(0, 400));
  process.exit(1);
});
