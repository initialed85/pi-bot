// pi-chat-gateway: an HTTP front door for the Pi session it is loaded into.
//
// This is deliberately NOT `pi --mode rpc`. RPC spawns its own process, and the whole point of the
// Google Chat bridge is that the person in the ttyd pane and the person in the chat thread are
// talking to *one* brain: same session file, same context, same Escape key. So the control surface
// lives inside the session as an extension, and the bridge is just another client of it.
//
// The server runs in-process because everything it needs (`pi.sendUserMessage`, `ctx.abort()`,
// `ctx.isIdle()`) is only reachable from inside the Pi process. It starts on `session_start` - not
// from the factory - because extension factories also run in invocations that never open a session
// (pi --list-models, print mode), where a listening socket would be a leak.
//
// It is inert unless PI_CHAT_GATEWAY_TOKEN is set, which is what keeps it safe to have this package
// installed on agents that are not the Chat-facing one: no token, no listener. Bind defaults to
// loopback; the pod sets PI_CHAT_GATEWAY_BIND=0.0.0.0 and keeps the port inside the cluster
// (ClusterIP Service, no Ingress) with the bearer token as the only gate.
//
// Wire contract (all JSON, `Authorization: Bearer $PI_CHAT_GATEWAY_TOKEN` except /healthz):
//   GET  /healthz          liveness, unauthenticated
//   GET  /v1/state         session + busy/idle summary
//   GET  /v1/commands      slash commands available to /v1/command
//   POST /v1/prompt        {id?, text, images?, deliverAs?, user?, space?, replyTo?} -> {turnId, folded, queued}
//   POST /v1/abort         abort the active run
//   POST /v1/command       {name, args} -> run a slash command in this session
//   GET  /v1/turns         recent turns (newest first)
//   GET  /v1/turns/:id     one turn: status, streamed text, tool trail, final text
//
// The bridge polls /v1/turns/:id rather than holding an SSE stream. Polling survives a bridge
// restart (the turn is still there), needs no reconnect/resync logic, and Chat's own rate limits
// make sub-second progress updates pointless anyway.

import { randomUUID, timingSafeEqual } from "node:crypto";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

const VERSION = "0.1.0";
const TOKEN = process.env.PI_CHAT_GATEWAY_TOKEN || "";
const PORT = Number(process.env.PI_CHAT_GATEWAY_PORT) || 8090;
const BIND = process.env.PI_CHAT_GATEWAY_BIND || "127.0.0.1";
const FORCE_OFF = process.env.PI_CHAT_GATEWAY === "0";
const MAX_BODY_BYTES = 32 * 1024 * 1024; // base64 images inlined in the prompt
const MAX_IMAGES = 8;
const TURN_CAP = 100;
const TOOL_TRAIL_CAP = 60;
const SUMMARY_CHARS = 240;

type TurnStatus = "queued" | "running" | "settled" | "aborted" | "error";

interface TurnInput {
  id: string;
  text: string;
  user?: string;
  space?: string;
  replyTo?: string;
  at: number;
  /** "chat" for prompts from the bridge, "pane" for a run a human started in the terminal. */
  source: "chat" | "pane";
}

interface ToolStep {
  id: string;
  name: string;
  phase: "start" | "end";
  summary?: string;
  isError?: boolean;
  at: number;
}

interface Turn {
  id: string;
  status: TurnStatus;
  inputs: TurnInput[];
  tools: ToolStep[];
  /** Text of the assistant message currently streaming. */
  streaming: string;
  /** Joined text of every assistant message this run produced, in order. */
  parts: string[];
  /** The last assistant message, which is normally the answer worth replying to. */
  last: string;
  /** The last assistant message that is actually prose; what the bridge may post. */
  answer?: string;
  /** Set when the final message was rejected as leaked tool-call markup, for the trail. */
  answerRejected?: string;
  /** Turn-id fragments this message quoted from a bridge notice, newest run first. */
  refs?: string[];
  turns: number;
  createdAt: number;
  startedAt?: number;
  /**
   * When the low-level run ended. Pi can still auto-compact, retry, or pick up queued work after this,
   * so it is not the end of the turn - but the assistant's final message is written by then, and a chat
   * bridge that waits for `agent_settled` makes a human watch "working" for minutes over an answer that
   * already exists. The bridge delivers at this mark and corrects itself if the text later changes.
   */
  endedAt?: number;
  settledAt?: number;
  model?: string;
  error?: string;
  /** Prompt content held for a queued follow-up, sent only once the session is idle. */
  content?: any[];
}

/** `[pi-chat:<id>]` marker the bridge puts in every prompt so a user message can be traced back. */
const MARKER = /\[pi-chat:([A-Za-z0-9_-]{3,40})\]/g;

/**
 * `⟦pi-chat <hex>⟧` - the marker the bridge puts at the front of every notice it posts (see
 * internal/chatid). It never occurs in typed prose, which is what makes removing it from message text
 * safe. The bridge strips it and resolves it against its turn journal before Pi ever sees it; this is
 * the second line of defence, and it keeps the reference on the turn record so /status can say which
 * earlier turn a person was asking about.
 */
const TURN_TOKEN = /\u27e6pi-chat ([0-9a-fA-F]{4,40})\u27e7/g;

/** Per-message reminder of the bridge's forwarding rule, appended after the user's text. */
const CHAT_RULE = "[chat bridge: only your final text block is posted back to Chat - intermediates are dropped. End with a self-contained answer.]";

function makeTurn(id: string, input: TurnInput): Turn {
  return { id, status: "running", inputs: [input], tools: [], streaming: "", parts: [], last: "", turns: 0, createdAt: Date.now() };
}

export default function activate(pi: ExtensionAPI) {
  if (FORCE_OFF || !TOKEN) return;

  const turns = new Map<string, Turn>();
  const order: string[] = [];
  // Chat messages that arrived while a run was going. Pi has no per-message turn to hand us when a
  // follow-up is delivered mid-run - it folds it into the loop that is already streaming - so the queue
  // lives here and is drained only when the session is genuinely idle. That is what gives each Chat
  // message its own answer instead of one combined reply.
  const pending: Turn[] = [];
  let active: Turn | undefined;
  let ctxRef: ExtensionContext | undefined;
  let server: ReturnType<typeof createServer> | undefined;
  let paneRuns = 0;

  // ---------------------------------------------------------------- turn bookkeeping

  function note(turn: Turn) {
    if (!order.includes(turn.id)) order.push(turn.id);
    turns.set(turn.id, turn);
    while (order.length > TURN_CAP) {
      const dead = order.shift();
      if (dead) turns.delete(dead);
    }
  }

  // pump sends the oldest queued Chat message, but only into an idle session. Until then the turn stays
  // "queued" and the bridge simply keeps waiting, which is the honest state.
  function pump() {
    if (!pending.length || active) return;
    const ctx = ctxRef;
    if (!ctx || !ctx.isIdle()) return;
    const turn = pending.shift()!;
    turn.status = "running";
    turn.startedAt = Date.now();
    active = turn;
    try {
      pi.sendUserMessage(turn.content || []);
    } catch (error) {
      turn.status = "error";
      turn.error = error instanceof Error ? error.message : String(error);
      turn.settledAt = Date.now();
    }
    note(turn);
  }

  function textOf(message: any): string {
    const content = message?.content;
    if (typeof content === "string") return content;
    if (!Array.isArray(content)) return "";
    return content
      .filter((part: any) => part?.type === "text" && typeof part.text === "string")
      .map((part: any) => part.text)
      .join("");
  }

  // Some models emit tool-call scaffolding as ordinary text instead of issuing a real tool call.
  // Seen live on 2026-11-20: the Chat thread received "<tool_call> <function=...> ..."
  // verbatim because that markup was the final assistant message of the run. It is never an
  // answer, so it must not be handed to the bridge as one.
  const CONTROL_MARKUP =
    /<\s*(\/\s*)?(tool_call|tool_use|function_calls|function|parameter|parameter_name|parameter_value|antml:\w+)\b[^>]*\/?>|<\s*\|\s*tool_call\s*\|\s*>|<\s*function=|\b\|\|\|tool_call\|\|\|/i;

  function looksLikeControlMarkup(text: string): boolean {
    const trimmed = text.trim();
    if (!trimmed) return false;
    if (!CONTROL_MARKUP.test(trimmed)) return false;
    // A genuine answer may quote tool syntax on purpose ("call `reload_pi`, it looks like this"),
    // so only treat the block as scaffolding when the markup dominates it.
    const stripped = trimmed.replace(CONTROL_MARKUP.global ? new RegExp(CONTROL_MARKUP.source, "gi") : CONTROL_MARKUP, "");
    const prose = stripped.replace(/[\s<>\/=|"'.,-]+/g, "").length;
    return prose < trimmed.replace(/[\s<>\/=|"'.,-]+/g, "").length * 0.5;
  }

  function idsOf(text: string): string[] {
    const found: string[] = [];
    for (const match of text.matchAll(MARKER)) found.push(match[1]);
    return found;
  }

  function currentModel(): string | undefined {
    const model = ctxRef?.model as any;
    if (!model) return undefined;
    return model.id ? `${model.provider ?? "model"}/${model.id}` : String(model);
  }

  // A run a human started by typing in the pane is not a Chat turn, and must not have its output
  // mailed into a thread. It gets recorded as a "pane" turn so /v1/state can still explain it.
  function openPaneTurn(): Turn {
    const turn = makeTurn(`pane-${++paneRuns}`, { id: `pane-${paneRuns}`, text: "", source: "pane", at: Date.now() });
    note(turn);
    return turn;
  }

  // ---------------------------------------------------------------- event wiring

  const off: Array<() => void> = [];

  // agent_settled covers the common case; the tick covers a settled event we missed (a run started from
  // the pane, a restart, an abort that did not report) so a queued message is never stranded.
  const pumpTimer = setInterval(pump, 1000);
  off.push(() => clearInterval(pumpTimer));

  off.push(
    pi.on("session_start", async (_event, ctx) => {
      ctxRef = ctx;
      // Re-entrant start (session switch, /reload): keep exactly one listener.
      if (server) return;
      server = createServer(handle);
      server.on("error", (error: NodeJS.ErrnoException) => {
        if (error.code === "EADDRINUSE") ctx.ui.notify(`pi-chat-gateway: port ${PORT} already in use`, "error");
      });
      server.listen(PORT, BIND, () => {
        ctx.ui.notify(`pi-chat-gateway listening on ${BIND}:${PORT}`, "info");
      });
    }),
  );

  off.push(
    pi.on("session_shutdown", async () => {
      const closing = server;
      server = undefined;
      if (!closing) return;
      try {
        closing.closeAllConnections?.();
      } catch {
        // best effort
      }
      closing.close();
    }),
  );

  // Every handler refreshes ctxRef rather than trusting the one it was handed: the session object
  // changes on /new and on session switch, and a stale ctx.abort() would abort nothing.
  off.push(
    pi.on("agent_start", async (_event, ctx) => {
      ctxRef = ctx;
      if (!active) active = openPaneTurn();
      active.startedAt ||= Date.now();
      active.model = currentModel();
    }),
  );

  off.push(
    pi.on("message_start", async (event, ctx) => {
      ctxRef = ctx;
      const message: any = event.message;
      if (message?.role !== "user") return;
      const raw = textOf(message);
      const refs = [...raw.matchAll(TURN_TOKEN)].map((m) => m[1].toLowerCase());
      const ids = idsOf(raw);
      if (ids.length === 0) return;
      // The marker tells us which Chat message this prompt is. The POST /v1/prompt handler has
      // already claimed it in both paths (new turn while idle, folded steer while busy), so the
      // usual outcome here is a no-op. The fall-through covers a human typing a marked prompt into
      // the pane by hand: claim it, because its answer is still something somebody is waiting for.
      if (ids.some((id) => turns.has(id))) return;
      const queuedTurn = turns.get(ids[0]);
      if (queuedTurn && queuedTurn !== active) {
        // A follow-up queued at POST time is now running as its own agent run. Adopt the turn we made
        // for it instead of opening a duplicate, so its answer stays attributed to the Chat message that
        // asked for it - that attribution is what keeps two prompts from being answered as one.
        queuedTurn.status = "running";
        queuedTurn.startedAt ||= Date.now();
        active = queuedTurn;
        for (const id of ids) {
          turns.set(id, queuedTurn);
          if (!order.includes(id)) order.push(id);
        }
        note(queuedTurn);
        return;
      }
      const input: TurnInput = { id: ids[0], text: cleanTurnTokens(raw.replace(MARKER, "")).trim(), source: "chat", at: Date.now() };
      const target = active && !ctx.isIdle() ? active : undefined;
      if (target) {
        target.inputs.push(input);
        note(target);
      } else {
        const turn = makeTurn(input.id, input);
        turn.startedAt = Date.now();
        note(turn);
        active = turn;
      }
      const owner = active!;
      if (refs.length) {
        owner.refs = Array.from(new Set([...(owner.refs ?? []), ...refs]));
      }
      for (const id of ids) {
        turns.set(id, owner);
        if (!order.includes(id)) order.push(id);
      }
    }),
  );

  off.push(
    pi.on("message_update", async (event, ctx) => {
      ctxRef = ctx;
      if (!active || event.message?.role !== "assistant") return;
      active.streaming = textOf(event.message);
    }),
  );

  off.push(
    pi.on("message_end", async (event, ctx) => {
      ctxRef = ctx;
      if (!active || event.message?.role !== "assistant") return;
      const text = textOf(event.message).trim();
      active.streaming = "";
      if (!text) return;
      active.parts.push(text);
      // `last` stays the raw final message (the pane and GET /v1/turns/:id should see what the
      // model actually produced); `answer` is what the bridge is allowed to post.
      active.last = text;
      if (!looksLikeControlMarkup(text)) active.answer = text;
      else active.answerRejected = text;
    }),
  );

  off.push(
    pi.on("turn_end", async (event, ctx) => {
      ctxRef = ctx;
      if (!active) return;
      active.turns = event.turnIndex + 1;
    }),
  );

  off.push(
    pi.on("tool_execution_start", async (event, ctx) => {
      ctxRef = ctx;
      if (!active) return;
      pushTool(active, { id: event.toolCallId, name: event.toolName, phase: "start", summary: summarise(event.args), at: Date.now() });
    }),
  );

  off.push(
    pi.on("tool_execution_end", async (event, ctx) => {
      ctxRef = ctx;
      if (!active) return;
      pushTool(active, {
        id: event.toolCallId,
        name: event.toolName,
        phase: "end",
        summary: summarise(event.result),
        isError: event.isError,
        at: Date.now(),
      });
    }),
  );

  off.push(
    pi.on("agent_end", async (_event, ctx) => {
      ctxRef = ctx;
      // First end only: the bridge has already posted whatever this run's last message said, and a
      // later change is handled as a correction, not a second timestamp.
      if (!active || active.endedAt) return;
      active.endedAt = Date.now();
      pump();
    }),
  );

  off.push(
    pi.on("agent_settled", async (event, ctx) => {
      ctxRef = ctx;
      const turn = active;
      active = undefined;
      if (!turn) return;
      turn.settledAt = Date.now();
      // An aborted run still settles; the distinction matters because the bridge should say
      // "stopped" rather than present a half answer as an answer.
      turn.status = event.aborted ? "aborted" : "settled";
      note(turn);
      pump();
    }),
  );

  off.push(
    pi.on("model_select", async (_event, ctx) => {
      ctxRef = ctx;
      if (active) active.model = currentModel();
    }),
  );

  function pushTool(turn: Turn, step: ToolStep) {
    turn.tools.push(step);
    if (turn.tools.length > TOOL_TRAIL_CAP) turn.tools.splice(0, turn.tools.length - TOOL_TRAIL_CAP);
  }

  function summarise(value: any): string {
    let text: string;
    if (typeof value === "string") text = value;
    else if (value && Array.isArray(value.content)) {
      text = value.content
        .filter((part: any) => part?.type === "text")
        .map((part: any) => part.text)
        .join(" ");
      if (!text && typeof value.isError === "boolean") text = JSON.stringify(value.details ?? {}).slice(0, 120);
    } else {
      try {
        text = JSON.stringify(value);
      } catch {
        text = String(value);
      }
    }
    text = (text || "").replace(/\s+/g, " ").trim();
    return text.length > SUMMARY_CHARS ? `${text.slice(0, SUMMARY_CHARS)}…` : text;
  }

  // ---------------------------------------------------------------- HTTP

  function authorized(request: IncomingMessage): boolean {
    const value = request.headers.authorization || "";
    // RFC 7235 makes the scheme name case-insensitive. Matching the literal "Bearer " would 401 a client
    // that sent "bearer", and on a gate whose only auth is a token, a 401 reads like a wrong token - which
    // sends whoever is debugging into checking the Secret instead of the header.
    const space = value.indexOf(" ");
    if (space < 0 || value.slice(0, space).toLowerCase() !== "bearer") return false;
    const provided = Buffer.from(value.slice(space + 1));
    const expected = Buffer.from(TOKEN);
    return provided.length === expected.length && timingSafeEqual(provided, expected);
  }

  function sendJson(response: ServerResponse, status: number, value: unknown) {
    const body = JSON.stringify(value);
    response.writeHead(status, { "content-type": "application/json; charset=utf-8", "content-length": Buffer.byteLength(body) });
    response.end(body);
  }

  async function readBody(request: IncomingMessage): Promise<any> {
    const chunks: Buffer[] = [];
    let size = 0;
    for await (const chunk of request) {
      size += (chunk as Buffer).length;
      if (size > MAX_BODY_BYTES) throw new Error("request body too large");
      chunks.push(chunk as Buffer);
    }
    if (!chunks.length) return {};
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  }

  function turnView(turn: Turn) {
    const progress = turn.tools.slice(-3).map((step) => `${step.name}${step.phase === "end" && step.isError ? " (error)" : ""}`);
    return {
      id: turn.id,
      status: turn.status,
      source: turn.inputs[0]?.source ?? "pane",
      inputs: turn.inputs.map((input) => ({ id: input.id, user: input.user, space: input.space, replyTo: input.replyTo, at: input.at, text: input.text })),
      createdAt: turn.createdAt,
      startedAt: turn.startedAt,
      endedAt: turn.endedAt,
      refs: turn.refs,
      settledAt: turn.settledAt,
      turns: turn.turns,
      model: turn.model,
      error: turn.error,
      streaming: turn.streaming,
      last: turn.last,
      // What the bridge should post as the answer: the last assistant message that is real prose,
      // falling back to earlier prose in the run (some models answer across several messages).
      // If every assistant block is scaffolding, text is empty and the bridge reports that honestly.
      text: turn.answer || turn.parts.filter((part) => !looksLikeControlMarkup(part)).join("\n\n"),
      answerRejected: turn.answerRejected,
      parts: turn.parts.length,
      progress: progress.join(", "),
      tools: turn.tools,
    };
  }

  async function handle(request: IncomingMessage, response: ServerResponse) {
    const url = request.url || "";
    const method = request.method || "GET";
    if (method === "GET" && (url === "/healthz" || url === "/v1/healthz")) {
      sendJson(response, 200, { ok: true, version: VERSION, enabled: true });
      return;
    }
    if (!authorized(request)) {
      sendJson(response, 401, { error: "unauthorized" });
      return;
    }

    if (method === "GET" && url === "/v1/state") {
      const ctx = ctxRef;
      sendJson(response, 200, {
        ok: true,
        version: VERSION,
        idle: ctx ? ctx.isIdle() : true,
        hasPendingMessages: ctx ? ctx.hasPendingMessages() : false,
        sessionId: ctx?.sessionManager.getSessionId(),
        sessionName: pi.getSessionName(),
        cwd: ctx?.cwd,
        model: currentModel(),
        thinkingLevel: ctx?.thinkingLevel,
        activeTurn: active?.id,
        turns: order.length,
        commands: pi.getCommands().length,
        contextUsage: ctx?.getContextUsage ? safe(() => ctx.getContextUsage()) : undefined,
      });
      return;
    }

    if (method === "GET" && url === "/v1/commands") {
      sendJson(response, 200, { commands: pi.getCommands().map((command: any) => ({ name: command.name, description: command.description, kind: command.kind })) });
      return;
    }

    if (method === "GET" && url === "/v1/turns") {
      sendJson(response, 200, { turns: order.slice(-20).reverse().map((id) => turnView(turns.get(id)!)).filter(Boolean) });
      return;
    }

    if (method === "GET" && url.startsWith("/v1/turns/")) {
      const id = decodeURIComponent(url.slice("/v1/turns/".length));
      const turn = turns.get(id);
      if (!turn) {
        // The bridge distinguishes "gone" (Pi restarted) from "not finished" (still running), so say which.
        sendJson(response, 404, { error: "unknown turn", id });
        return;
      }
      sendJson(response, 200, turnView(turn));
      return;
    }

    if (method === "POST" && url === "/v1/prompt") {
      const body = await readBody(request);
      const text = typeof body.text === "string" ? body.text.trim() : "";
      const images = Array.isArray(body.images) ? body.images : [];
      if (!text && images.length === 0) {
        sendJson(response, 400, { error: "text or images required" });
        return;
      }
      if (images.length > MAX_IMAGES) {
        sendJson(response, 400, { error: `too many images (max ${MAX_IMAGES})` });
        return;
      }
      const id = typeof body.id === "string" && /^[A-Za-z0-9_-]{3,40}$/.test(body.id) ? body.id : randomUUID().slice(0, 8);
      const deliverAs = body.deliverAs === "followUp" ? "followUp" : "steer";
      // The marker is what lets message_start tie this prompt back to a Chat message. Keeping it in
      // the visible text is on purpose: whoever is watching the pane can see which chat id is running.
      // The trailer makes the bridge's forwarding rule self-advertising: only the turn's final
      // assistant text block is posted back to Chat (see turn.last), so the answer must be
      // self-contained. Agents that never read AGENTS.md still learn the invariant at chat time.
      // Kept on its own line so turnView's MARKER stripping leaves user text clean.
      const promptText = `[pi-chat:${id}] ${text || "(image)"}\n${CHAT_RULE}`;
      const content: any[] = [{ type: "text", text: promptText }];
      for (const image of images) {
        if (typeof image?.data !== "string" || typeof image?.mimeType !== "string") {
          sendJson(response, 400, { error: "each image needs data and mimeType" });
          return;
        }
        content.push({ type: "image", data: image.data, mimeType: image.mimeType });
      }

      const turn = makeTurn(id, {
        id,
        text,
        user: typeof body.user === "string" ? body.user : undefined,
        space: typeof body.space === "string" ? body.space : undefined,
        replyTo: typeof body.replyTo === "string" ? body.replyTo : undefined,
        at: Date.now(),
        source: "chat",
      });
      const busy = Boolean(active) && !ctxRef?.isIdle();
      // Only a steer joins the run that is going. A follow-up waits for it, so it keeps its own turn -
      // without that, every Chat message sent while Pi was busy got the same single answer.
      const folding = busy && deliverAs === "steer";
      const queued = busy && deliverAs === "followUp";
      turn.content = content;
      note(turn);
      try {
        if (folding) {
          // Streaming: Pi needs to be told how to queue it. A steer lands between tool calls, which is
          // the Chat UX people expect ("stop, use the other branch").
          pi.sendUserMessage(content, { deliverAs });
          if (!active!.inputs.some((existing) => existing.id === id)) active!.inputs.push(turn.inputs[0]);
          turns.set(id, active!);
          if (!order.includes(id)) order.push(id);
        } else if (queued) {
          turn.status = "queued";
          pending.push(turn);
        } else {
          pi.sendUserMessage(content);
          active = turn;
          turn.startedAt = Date.now();
        }
      } catch (error) {
        turn.status = "error";
        turn.error = error instanceof Error ? error.message : String(error);
        turn.settledAt = Date.now();
        sendJson(response, 500, { error: turn.error, id });
        return;
      }
      sendJson(response, 202, { ok: true, turnId: folding ? active!.id : id, folded: folding, id, queued });
      return;
    }

    if (method === "POST" && url === "/v1/abort") {
      const ctx = ctxRef;
      if (!ctx || ctx.isIdle()) {
        sendJson(response, 200, { ok: true, aborted: false, reason: "idle" });
        return;
      }
      ctx.abort();
      if (active) {
        active.status = "aborted";
        active.settledAt = Date.now();
      }
      sendJson(response, 200, { ok: true, aborted: true });
      return;
    }

    if (method === "POST" && url === "/v1/command") {
      const body = await readBody(request);
      const name = typeof body.name === "string" ? body.name.replace(/^\//, "").trim() : "";
      if (!/^[A-Za-z0-9:_-]{1,40}$/.test(name)) {
        sendJson(response, 400, { error: "invalid command name" });
        return;
      }
      const args = typeof body.args === "string" ? body.args.trim() : "";
      const line = args ? `/${name} ${args}` : `/${name}`;
      try {
        // Without expandPromptTemplates, sendUserMessage does not dispatch extension commands.
        pi.sendUserMessage(line, { expandPromptTemplates: true });
      } catch (error) {
        sendJson(response, 400, { error: error instanceof Error ? error.message : String(error) });
        return;
      }
      sendJson(response, 202, { ok: true, command: line });
      return;
    }

    sendJson(response, 404, { error: "not found", url });
  }

  function safe<T>(fn: () => T): T | undefined {
    try {
      return fn();
    } catch {
      return undefined;
    }
  }

  // A command so the person in the pane can check the door without kubectl exec-ing curl.
  pi.registerCommand("chat-gateway", {
    description: "Show the pi-chat gateway status, recent turns, and last Chat prompt",
    handler: async (_args, ctx) => {
      ctxRef = ctx;
      const recent = order.slice(-5).reverse();
      const lines = recent.map((id) => {
        const turn = turns.get(id)!;
        const input = turn.inputs[0];
        return `${turn.id} ${turn.status}${turn.inputs.length > 1 ? ` (+${turn.inputs.length - 1} steers)` : ""} ${turn.turns} turns: ${cleanPrompt(input?.text || "")}`;
      });
      ctx.ui.notify(
        `pi-chat-gateway ${BIND}:${PORT} · ${ctx.isIdle() ? "idle" : "busy"}${active ? ` · turn ${active.id}` : ""}${lines.length ? `\n${lines.join("\n")}` : ""}`,
        "info",
      );
    },
  });

  pi.registerCommand("chat-abort", {
    description: "Abort the run the Chat bridge is currently waiting on",
    handler: async (_args, ctx) => {
      ctxRef = ctx;
      if (ctx.isIdle()) {
        ctx.ui.notify("pi-chat-gateway: nothing running", "info");
        return;
      }
      ctx.abort();
      ctx.ui.notify("pi-chat-gateway: aborted", "info");
    },
  });
}

function cleanPrompt(text: string): string {
  return cleanTurnTokens(text.replace(MARKER, "")).replace(/\s+/g, " ").trim().slice(0, 80);
}

/** Removes every bridge turn marker from text, leaving whatever a person wrote around it intact. */
function cleanTurnTokens(text: string): string {
  return text.replace(TURN_TOKEN, "").replace(/[ \t]{2,}/g, " ");
}
