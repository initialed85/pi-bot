#!/usr/bin/env node
// chat-user-subscription.mjs - manage the user-authorised Chat room listener.
//
// Why this exists: the Chat app's own credential only receives messages that mention the app, so
// ordinary ("untagged") room traffic is invisible to the bridge. A Workspace Events subscription
// created with a HUMAN OAuth token sees every room that human belongs to, delivered to a Pub/Sub
// topic. Measured on 2026-11-24 against a consumer @gmail.com space:
//   - target //chat.googleapis.com/spaces/- means "every space this user is in"
//   - payload is CloudEvents 1.0; body carries name/text/sender/annotations/thread when the
//     fieldMask asks for them
//   - the fieldMask is fixed at creation: PATCH payloadOptions.fieldMask returns 400, so change
//     the payload shape by delete + create
//   - subscriptions expire ~4h after createTime, so a renewal loop is mandatory
//   - events include the bot's OWN messages (sender.type === "BOT"), so a consumer must loop-guard
//
// Credentials are read from files, never embedded, and never written into the repo:
//   PIBOT_OAUTH_CLIENT  default /root/oauth-client.json      {client_id, client_secret}
//   PIBOT_OAUTH_TOKEN   default /root/chat-user-token.json   {refresh_token}
// Both should be 0600 and live outside any git checkout.
//
// Usage:
//   node tools/chat-user-subscription.mjs create [--target T] [--topic TOPIC] [--mask M]
//   node tools/chat-user-subscription.mjs show --name NAME
//   node tools/chat-user-subscription.mjs renew            # delete + recreate, same shape
//   node tools/chat-user-subscription.mjs delete --name NAME
//
// There is no working list: GET /v1/subscriptions?pageSize=N is refused with
// FILTER_PARSING_FAILURE, and adding a filter did not help either. Names come from the create
// response, or from a 409 whose error metadata carries `current_subscription`.
//   node tools/chat-user-subscription.mjs pull [--topic TOPIC] [--sub SUB] [--wait 120] [--raw]
//                                             # --ensure-sub creates the pull subscription first,
//                                             # because Pub/Sub only delivers to subscribers that
//                                             # already existed when the message was published

import fs from 'node:fs';

const args = process.argv.slice(2);
const cmd = args[0] ?? 'list';
const flag = (n, d) => { const i = args.indexOf('--' + n); return i >= 0 ? args[i + 1] : d; };

const PROJECT = flag('project', 'home-ops-513116');
const TOPIC = flag('topic', 'pi-chat-user-events');
const SUB_NAME = flag('topic-sub', 'pi-chat-user-events-sub');
const TARGET = flag('target', '//chat.googleapis.com/spaces/-');
const MASK = flag('mask', 'message.name,message.text,message.sender,message.annotations,message.thread');
const PUSH_MEMBER = 'serviceAccount:chat-api-push@system.gserviceaccount.com';
const EVENT = 'google.workspace.chat.message.v1.created';

async function auth() {
  const client = JSON.parse(fs.readFileSync(process.env.PIBOT_OAUTH_CLIENT ?? '/root/oauth-client.json', 'utf8'));
  const tf = JSON.parse(fs.readFileSync(process.env.PIBOT_OAUTH_TOKEN ?? '/root/chat-user-token.json', 'utf8'));
  const r = await fetch('https://oauth2.googleapis.com/token', {
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ client_id: client.client_id, client_secret: client.client_secret, refresh_token: tf.refresh_token, grant_type: 'refresh_token' }),
  });
  const j = await r.json();
  if (!j.access_token) throw new Error(`token refresh refused: ${JSON.stringify(j).slice(0, 300)}`);
  return { authorization: 'Bearer ' + j.access_token, 'content-type': 'application/json' };
}

async function ensureTopicAndGrant(H) {
  const t = await fetch(`https://pubsub.googleapis.com/v1/projects/${PROJECT}/topics/${TOPIC}`, { method: 'PUT', headers: H, body: '{}' });
  if (t.status !== 200 && t.status !== 409) throw new Error(`topic create ${t.status}: ${(await t.text()).slice(0, 200)}`);
  const g = await fetch(`https://pubsub.googleapis.com/v1/projects/${PROJECT}/topics/${TOPIC}:getIamPolicy`, { headers: H });
  const pol = await g.json();
  const bindings = pol.bindings ?? [];
  const pub = bindings.find((b) => b.role === 'roles/pubsub.publisher');
  const already = pub?.members?.includes(PUSH_MEMBER) || pub?.bindings?.some((b) => b.member === PUSH_MEMBER);
  if (already) return 'publisher already granted';
  if (pub && Array.isArray(pub.bindings)) pub.bindings.push({ member: PUSH_MEMBER, role: 'roles/pubsub.publisher' });
  else if (pub) pub.members = [...(pub.members ?? []), PUSH_MEMBER];
  else bindings.push({ role: 'roles/pubsub.publisher', members: [PUSH_MEMBER] });
  const s = await fetch(`https://pubsub.googleapis.com/v1/projects/${PROJECT}/topics/${TOPIC}:setIamPolicy`, { method: 'POST', headers: H, body: JSON.stringify({ policy: { ...pol, bindings } }) });
  if (!s.ok) throw new Error(`grant ${s.status}: ${(await s.text()).slice(0, 200)}`);
  return 'granted chat-api-push publisher';
}

function describe(s) {
  return [
    `name: ${s.name}`,
    `target: ${s.targetResource}`,
    `topic: ${s.notificationEndpoint?.pubsubTopic}`,
    `fieldMask: ${s.payloadOptions?.fieldMask}`,
    `state: ${s.state}`,
    `authority: ${s.authority}`,
    `created: ${s.createTime}`,
    `EXPIRES: ${s.expireTime}`,
  ].join('\n  ');
}

async function create(H) {
  const grant = await ensureTopicAndGrant(H);
  const body = {
    targetResource: TARGET,
    eventTypes: [EVENT],
    payloadOptions: { includeResource: true, fieldMask: MASK },
    notificationEndpoint: { pubsubTopic: `projects/${PROJECT}/topics/${TOPIC}` },
  };
  const r = await fetch('https://workspaceevents.googleapis.com/v1/subscriptions', { method: 'POST', headers: H, body: JSON.stringify(body) });
  const j = await r.json();
  if (!r.ok) {
    const existing = j?.error?.details?.[0]?.metadata?.current_subscription;
    if (existing) {
      const v = await fetch(`https://workspaceevents.googleapis.com/v1/${existing}`, { headers: H });
      console.log(`${grant}\nsubscription already exists, shape unchanged:\n  ${describe(await v.json())}`);
      return 0;
    }
    console.log(`create ${r.status}: ${JSON.stringify(j.error ?? j).slice(0, 400)}`);
    return 1;
  }
  const name = (j.response ?? j).name;
  const v = await fetch(`https://workspaceevents.googleapis.com/v1/${name}`, { headers: H });
  console.log(`${grant}\ncreated:\n  ${describe(await v.json())}`);
  return 0;
}

async function show(H) {
  const name = flag('name', '');
  if (!name) { console.log('show needs --name'); return 1; }
  const r = await fetch(`https://workspaceevents.googleapis.com/v1/${name}`, { headers: H });
  const j = await r.json();
  if (!r.ok) { console.log(`show ${r.status}: ${JSON.stringify(j.error ?? j).slice(0, 300)}`); return 1; }
  console.log(describe(j));
  return 0;
}

async function remove(H) {
  const name = flag('name', '');
  if (!name) { console.log('delete needs --name'); return 1; }
  const r = await fetch(`https://workspaceevents.googleapis.com/v1/${name}`, { method: 'DELETE', headers: H });
  console.log(`DELETE ${name} -> ${r.status}`);
  return r.ok ? 0 : 1;
}

async function renew(H) {
  // A renewal is a delete + create: the payload shape cannot be edited on a live subscription.
  // The existing name is discovered from the 409 that create returns for a duplicate target.
  let name = flag('name', '');
  if (!name) {
    const probe = await fetch('https://workspaceevents.googleapis.com/v1/subscriptions', { method: 'POST', headers: H, body: JSON.stringify({
      targetResource: TARGET, eventTypes: [EVENT],
      payloadOptions: { includeResource: true, fieldMask: MASK },
      notificationEndpoint: { pubsubTopic: `projects/${PROJECT}/topics/${TOPIC}` },
    }) });
    const pj = await probe.json();
    if (probe.ok) { console.log('nothing to renew - created instead:\n  ' + describe((pj.response ?? pj))); return 0; }
    name = pj?.error?.details?.[0]?.metadata?.current_subscription ?? '';
    if (!name) { console.log(`renew probe ${probe.status}: ${JSON.stringify(pj.error ?? pj).slice(0, 300)}`); return 1; }
  }
  const d = await fetch(`https://workspaceevents.googleapis.com/v1/${name}`, { method: 'DELETE', headers: H });
  console.log(`renew: deleted ${name} -> ${d.status}`);
  return create(H);
}

async function pull(H) {
  const wait = Number(flag('wait', 120));
  if (args.includes('--ensure-sub')) {
    const c = await fetch(`https://pubsub.googleapis.com/v1/projects/${PROJECT}/subscriptions/${SUB_NAME}`, { method: 'PUT', headers: H, body: JSON.stringify({ topic: `projects/${PROJECT}/topics/${TOPIC}` }) });
    console.log(`pull subscription ${SUB_NAME}: ${c.status === 409 ? 'exists' : c.status}`);
  }
  const deadline = Date.now() + wait * 1000;
  let round = 0;
  let total = 0;
  while (Date.now() < deadline) {
    round++;
    const r = await fetch(`https://pubsub.googleapis.com/v1/projects/${PROJECT}/subscriptions/${SUB_NAME}:pull`, { method: 'POST', headers: H, body: JSON.stringify({ maxMessages: 20, returnImmediately: false }) });
    const j = await r.json();
    const recs = j.receivedMessages ?? [];
    if (!recs.length) { await new Promise((res) => setTimeout(res, 3000)); continue; }
    for (const rec of recs) {
      const env = rec.message ?? {};
      // CloudEvents over Pub/Sub puts ce-* on the envelope attributes and the JSON body in data.
      // Reading them out of the decoded body is the mistake that prints undefined for everything.
      const a = env.attributes ?? {};
      const body = env.data ? JSON.parse(Buffer.from(env.data, 'base64').toString('utf8')) : {};
      const m = body.message ?? {};
      const human = m.sender?.type === 'HUMAN';
      // Other people's rooms are in scope now; only the account owner's own test text is printed.
      const text = human ? String(m.text ?? '').slice(0, 60) : '<participant message: text not printed>';
      console.log(`ce-type: ${a['ce-type'] ?? 'unset'} | space: ${a['ce-subject'] ?? 'unset'} | sender: ${m.sender?.name ?? 'unset'} (${m.sender?.type ?? 'unset'})`);
      console.log(`  published: ${env.publishTime ?? 'unset'} | thread: ${m.thread?.name ?? '-'} | annotations: ${(m.annotations ?? []).map((x) => x.type).join('+') || 'none'}`);
      const nm = String(m.name ?? '');
      const parts = nm.split('/').pop()?.split('.') ?? [];
      console.log(`  name: ${nm || 'unset'} | position: ${parts.length === 2 && parts[0] === parts[1] ? 'thread root' : parts.length === 2 ? 'reply inside thread' : 'unknown'}`);
      console.log(`  text: ${text}`);
      if (args.includes('--raw')) console.log('  raw envelope: ' + JSON.stringify({ attributes: a, publishTime: env.publishTime, messageId: env.messageId, orderingKey: env.orderingKey }).slice(0, 700));
      if (args.includes('--raw')) console.log('  raw body: ' + JSON.stringify(body).slice(0, 900));
      await fetch(`https://pubsub.googleapis.com/v1/projects/${PROJECT}/subscriptions/${SUB_NAME}:acknowledge`, { method: 'POST', headers: H, body: JSON.stringify({ ackIds: [rec.ackId] }) });
    }
    total += recs.length;
    console.log(`batch ${round}: ${recs.length} event(s) on ${TOPIC} (total ${total})`);
  }
  if (total) {
    console.log(`drained ${total} event(s) from ${TOPIC}; nothing further within ${wait}s`);
    return 0;
  }
  console.log(`nothing arrived on ${TOPIC} within ${wait}s`);
  return 2;
}

const H = await auth();
const fns = { create, show, delete: remove, renew, pull };
const fn = fns[cmd];
if (!fn) { console.log('unknown command: ' + cmd); process.exit(1); }
process.exit(await fn(H));
