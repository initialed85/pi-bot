#!/usr/bin/env node
// chat-space-history.mjs - read a room's real history as the human account, and run the shipped
// room gate verdict over it.
//
// Why this exists: the only way to judge "should pi-bot have answered that?" is to see messages that
// were really written in a real conversation. Staged test messages are performative - people write
// differently when they know they are being measured - and waiting for them stalls the decision. Chat
// API `spaces.messages.list` under the SAME user OAuth credential that authorises the Workspace Events
// subscription returns the room's actual history, so the verdicts can be built today.
//
// This is read-only. It never posts anything, and it does not touch the live Pub/Sub subscription, so
// unlike the audit replay it cannot steal messages from the running bridge.
//
// Credentials come from files, never embedded, never committed:
//   PIBOT_OAUTH_CLIENT  default /root/oauth-client.json      {client_id, client_secret}
//   PIBOT_OAUTH_TOKEN   default /root/chat-user-token.json   {refresh_token}
//
// Usage:
//   node tools/chat-space-history.mjs list [--space AAAA1pNBS1A] [--pageSize 50]
//   node tools/chat-space-history.mjs judge [--space AAAA1pNBS1A] [--pageSize 50] [--gate URL]
//
// Privacy: text is printed for allowlisted senders only (PIBOT_ALLOWLIST, default Ed and Amy); other
// participants' text is replaced with a description unless --show-text is passed explicitly. Verdicts
// are reported to Ed as summaries, not as transcripts of other people's messages.

import fs from 'node:fs';

const args = process.argv.slice(2);
const cmd = args[0] ?? 'list';
const flag = (n, d) => { const i = args.indexOf('--' + n); return i >= 0 ? args[i + 1] : d; };

const SPACE = flag('space', 'AAAA1pNBS1A');
const PAGE_SIZE = Number(flag('pageSize', '50'));
const GATE = flag('gate', process.env.PI_CHAT_REPLY_GATE_URL ?? 'https://sentiment-analyzer.dev.initialed85.cc');
const MODEL = flag('model', process.env.PI_CHAT_REPLY_GATE_MODEL ?? 'unsloth/gemma-4-E4B-it-GGUF:IQ4_XS');
const PROMPT_FILE = flag('prompt', new URL('../eval/chat-gate/prompts/room-respond.md', import.meta.url).pathname);
const SHOW_TEXT = args.includes('--show-text');
const ALLOW = (process.env.PIBOT_ALLOWLIST ?? 'initialed85@gmail.com,amy.annisbrown@gmail.com')
  .split(',').map((s) => s.trim().toLowerCase()).filter(Boolean);

const BOT_EMAIL = 'pi-chat@chat.googleapis.com';
const BOT_IDS = new Set(['AAAAAAAACrg', 'AAAAAAACrg', 'pi-chat']);

async function auth() {
  const client = JSON.parse(fs.readFileSync(process.env.PIBOT_OAUTH_CLIENT ?? '/root/oauth-client.json', 'utf8'));
  const tf = JSON.parse(fs.readFileSync(process.env.PIBOT_OAUTH_TOKEN ?? '/root/chat-user-token.json', 'utf8'));
  const r = await fetch('https://oauth2.googleapis.com/token', {
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({
      client_id: client.client_id,
      client_secret: client.client_secret,
      refresh_token: tf.refresh_token,
      grant_type: 'refresh_token',
    }),
  });
  const j = await r.json();
  if (!j.access_token) throw new Error(`token refresh refused: ${JSON.stringify(j).slice(0, 300)}`);
  return { authorization: 'Bearer ' + j.access_token, 'content-type': 'application/json' };
}

async function listMessages(H) {
  // messages.list has no readMask (it is refused as an unknown query parameter); the fields that come
  // back are fixed, so nothing is masked here.
  const params = new URLSearchParams({
    pageSize: String(PAGE_SIZE),
    orderBy: flag('orderBy', 'createTime desc'),
  });
  const url = `https://chat.googleapis.com/v1/spaces/${SPACE}/messages?${params}`;
  const r = await fetch(url, { headers: H });
  const body = await r.text();
  if (!r.ok) {
    throw new Error(`messages.list ${r.status}: ${body.slice(0, 500)}`);
  }
  return JSON.parse(body);
}

function isBot(m) {
  const sender = m.sender ?? {};
  if (String(sender.name ?? '').includes('/bots/')) return true;
  if ((sender.type ?? '').toUpperCase() === 'BOT') return true;
  const email = (sender.email ?? '').toLowerCase();
  return email === BOT_EMAIL || BOT_IDS.has(email);
}

// The bridge treats a message as an untagged room root only when it is not addressed to the app and
// not inside a thread. The same predicate is applied here so the verdicts describe the shipped path.
function mentionsApp(m) {
  for (const a of m.annotations ?? []) {
    const u = a?.mention?.user ?? {};
    if (String(u.name ?? '').includes('/users/') && (u.name ?? '').includes('/users/AAAAAAAACrg')) return true;
    if ((u.email ?? '').toLowerCase() === BOT_EMAIL) return true;
  }
  const text = m.text ?? '';
  return /pi-?chat/i.test(text) && (text.includes('@') || /@\S*pi-chat/i.test(text));
}

function inThread(m) {
  const t = m.thread?.name ?? '';
  const own = m.name ?? '';
  if (!t) return false;
  // thread.name is "<space>/threads/<thread>"; a root message's own name ends with the same thread id.
  const threadId = t.split('/').pop();
  return !own.endsWith(threadId);
}

function describeMessage(m) {
  const sender = m.sender ?? {};
  const email = (sender.email ?? sender.name ?? 'unknown').toLowerCase();
  // Redaction is a property of the OUTPUT, not of the judgement: the gate has to see the real text or
  // the verdict describes nothing. What leaves this terminal is described, not transcribed.
  const text = (m.text ?? '').trim();
  const allowed = ALLOW.includes(email);
  return {
    name: m.name,
    sender: email,
    type: sender.type ?? 'USER',
    bot: isBot(m),
    createTime: m.createTime,
    inThread: inThread(m),
    mentionsApp: mentionsApp(m),
    thread: m.thread?.name ?? '',
    text,
    shown: SHOW_TEXT || allowed ? text : `<participant not on the allowlist: ${email}>`,
  };
}

async function callGate(prompt) {
  // Byte-for-byte the shape the bridge sends (internal/chatgate/client.go): one user message, no system
  // turn, temperature 0, four tokens, same model id. Anything else measures a different system.
  const r = await fetch(`${GATE}/v1/chat/completions`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      model: MODEL,
      temperature: 0,
      max_tokens: 4,
      messages: [{ role: 'user', content: prompt }],
    }),
    signal: AbortSignal.timeout(30000),
  });
  const raw = await r.text();
  if (!r.ok) throw new Error(`gate ${r.status}: ${raw.slice(0, 200)}`);
  const j = JSON.parse(raw);
  return (j.choices?.[0]?.message?.content ?? '').trim();
}

// The room gate's only context is what pi-chat last said in the room, kept to 30 words - the same
// truncation bridge.summaryWords applies.
function summaryWords(value) {
  const words = String(value ?? '').trim().split(/\s+/).filter(Boolean);
  return words.slice(0, 30).join(' ');
}

function fillTemplate(template, { summary, sender, message }) {
  return template
    .replaceAll('{summary}', summary ?? '')
    .replaceAll('{sender}', sender || 'a person')
    .replaceAll('{message}', message ?? '');
}

async function main() {
  const H = await auth();
  const res = await listMessages(H);
  const messages = (res.messages ?? []).map(describeMessage);
  console.log(`space ${SPACE}: ${messages.length} messages${res.nextPageToken ? ' (first page)' : ''}`);

  if (cmd === 'list') {
    for (const m of messages) {
      console.log(`\n${m.createTime}  ${m.sender}  ${m.bot ? '[BOT] ' : ''}${m.inThread ? '[thread] ' : '[root]  '}${m.mentionsApp ? '[mention] ' : ''}${m.shown}`);
    }
    return 0;
  }

  if (cmd !== 'judge') throw new Error(`unknown command ${cmd}`);
  const template = fs.readFileSync(PROMPT_FILE, 'utf8');
  const ordered = [...messages].reverse(); // oldest first, so "what the bot last said" walks forward in time
  const rows = [];
  for (let i = 0; i < ordered.length; i++) {
    const m = ordered[i];
    if (m.bot) continue;
    if (m.mentionsApp) { rows.push({ ...m, verdict: 'skipped: addressed to the app (app path answers, the gate is not consulted)' }); continue; }
    if (m.inThread) { rows.push({ ...m, verdict: 'skipped: inside a thread (enforce path: unanswered)' }); continue; }
    let summary = '';
    for (let j = i - 1; j >= 0; j--) {
      if (ordered[j].bot) { summary = summaryWords(ordered[j].text); break; }
    }
    try {
      const verdict = await callGate(fillTemplate(template, { summary, sender: m.sender, message: m.text }));
      rows.push({ ...m, verdict: verdict || '<empty>' });
    } catch (err) {
      rows.push({ ...m, verdict: `gate error: ${err.message}` });
    }
  }
  console.log(`\njudged ${rows.length} untagged human messages`);
  console.log(`prompt: ${PROMPT_FILE}\ngate: ${GATE} model: ${MODEL} max_tokens=4 temperature=0 (the shipped request shape)`);
  for (const r of rows) {
    console.log(`\n${r.createTime}  ${r.sender}\n  ${r.shown}\n  -> ${r.verdict}`);
  }
  return 0;
}

main().then((code) => process.exit(code ?? 0)).catch((err) => {
  console.error(String(err?.message ?? err).slice(0, 700));
  process.exit(1);
});
