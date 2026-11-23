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
//   node chat-user-oauth-probe.mjs --client ./oauth-client.json --include-group spaces/AAAA1pNBS1A
//
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

const SCOPES = [
  "https://www.googleapis.com/auth/chat.messages.readonly",
  "https://www.googleapis.com/auth/chat.spaces.readonly",
  // The events API is a different service and needs its own scope.
  "https://www.googleapis.com/auth/workspaceevents",
];

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
  fs.writeFileSync(TOKEN_PATH, JSON.stringify({ refresh_token: j.refresh_token, scope: j.scope }, null, 2), {
    mode: 0o600,
  });
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
  return j.access_token;
}

async function get(token, url) {
  const r = await fetch(url, { headers: { authorization: "Bearer " + token } });
  const t = await r.text();
  return { status: r.status, body: t.replace(/\s+/g, " ").slice(0, 400) };
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

  const token = await accessToken(client, refreshToken);
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
    const we = await fetch("https://workspaceevents.googleapis.com/v1/subscriptions", {
      method: "POST",
      headers: { authorization: "Bearer " + token, "content-type": "application/json" },
      body: JSON.stringify({
        targetResource: `//chat.googleapis.com/${sp}`,
        eventTypes: ["google.workspace.chat.message.v1.created"],
        payloadOptions: { includeResource: true, fieldMask: "message.name,message.text" },
        validateOnly: true,
      }),
    });
    console.log(`  workspaceevents validateOnly: ${we.status} ${(await we.text()).replace(/\s+/g, " ").slice(0, 260)}`);
  }

  console.log(`
Read this way:
  * 200 on messages.list / spaceEvents with YOUR credential, while the app credential gets
    403 or 500, means room-wide listening is reachable without paying for Workspace - the
    bot would be reading as you, which is a disclosure decision, not just a technical one.
  * A refusal that still names "Google Workspace organization" or "administrator approval"
    means the user path is closed for a consumer account too, and this whole idea is dead.
  * 403 PERMISSION_DENIED naming a scope means consent did not include it; rerun with
    --reconsent after adding the scope in the console.`);
}

function report(label, r) {
  const mark = r.status === 200 ? "OK  " : `${r.status}`;
  console.log(`\n[${mark}] ${label}`);
  console.log("     " + r.body.slice(0, 260));
}

main().catch((e) => {
  console.error("\nfailed:", String(e.message || e).slice(0, 400));
  process.exit(1);
});
