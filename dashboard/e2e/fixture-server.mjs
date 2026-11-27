// Contract fixture server for the dashboard browser suite. It implements docs/investigation-api.md
// (session exchange, pages, turns, stop, ordered SSE with Last-Event-ID, evidence and diff) from the
// committed fixtures. It is test-only: the production bundle talks to pagerless-server and has no demo
// mode. Run with `npm run test:e2e` (Playwright starts it) or `node e2e/fixture-server.mjs`.

import { readFile } from "node:fs/promises";
import { createServer } from "node:http";
import { extname, join, normalize, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const dashboardRoot = resolve(fileURLToPath(new URL("..", import.meta.url)));
const distRoot = join(dashboardRoot, "dist");
const fixturePath = join(dashboardRoot, "src", "lib", "__fixtures__", "investigation.json");
const fixture = JSON.parse(await readFile(fixturePath, "utf8"));

const port = Number(process.env.PORT ?? process.env.PAGERLESS_FIXTURE_PORT ?? 18082);
const token = process.env.PAGERLESS_FIXTURE_TOKEN ?? "test-token";
const cookieName = "pagerless_session";

let state = null;
state = reset({});

function clone(value) {
  return structuredClone(value);
}

function reset(options = {}) {
  state = {
    pages: options.empty === true ? [] : clone(fixture.pages),
    details: options.empty === true ? {} : clone(fixture.details),
    events: options.empty === true ? {} : clone(fixture.events),
    diffs: options.empty === true ? {} : clone(fixture.diffs),
    evidence: clone(fixture.evidence),
    failProbe: Number(options.failProbe ?? 0),
    failRefresh: Number(options.failRefresh ?? 0),
    failCreate: Number(options.failCreate ?? 0),
    reconnectDropped: false,
    reconnectTail: options.empty === true ? [] : clone(fixture.events["p-reconnect"] ?? []).slice(4),
    probed: false,
    probeDown: options.probeDown === true,
    created: 0,
    turns: 0,
    idempotency: new Map(),
  };
  if (options.empty !== true) {
    // The reconnect page starts mid-turn: four durable events are already read, the rest arrive after a
    // forced stream drop. That is the scenario the replay cursor exists for.
    state.events["p-reconnect"] = clone(fixture.events["p-reconnect"] ?? []).slice(0, 4);
    if (state.details["p-reconnect"] !== undefined) {
      state.details["p-reconnect"].lastEventId = 4;
    }
  }
  return state;
}

function pageSummary(detail) {
  const { messages, lastEventId, ...summary } = detail;
  void messages;
  void lastEventId;
  return summary;
}

function findIndex(id) {
  return state.pages.findIndex((page) => page.id === id);
}

function touch(id) {
  const detail = state.details[id];
  if (detail === undefined) {
    return;
  }
  detail.updatedAt = new Date().toISOString();
  const summary = pageSummary(detail);
  const existing = findIndex(id);
  if (existing !== -1) {
    state.pages.splice(existing, 1);
  }
  state.pages.unshift(summary);
}

function appendEvent(id, event) {
  const list = state.events[id] ?? (state.events[id] = []);
  const seq = list.length === 0 ? 1 : list[list.length - 1].seq + 1;
  const complete = { seq, pageId: id, time: new Date().toISOString(), ...event };
  list.push(complete);
  const detail = state.details[id];
  if (detail !== undefined) {
    detail.lastEventId = seq;
    detail.updatedAt = complete.time;
  }
  return complete;
}

function failOnce(kind) {
  if (kind === "probe" && state.failProbe > 0) {
    state.failProbe -= 1;
    return true;
  }
  if (kind === "refresh" && state.failRefresh > 0) {
    state.failRefresh -= 1;
    return true;
  }
  if (kind === "create" && state.failCreate > 0) {
    state.failCreate -= 1;
    return true;
  }
  return false;
}

function sendJSON(res, status, body) {
  const text = JSON.stringify(body);
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    "Content-Length": Buffer.byteLength(text),
  });
  res.end(text);
}

function sendError(res, status, code, message) {
  sendJSON(res, status, { error: { code, message } });
}

async function readBody(req) {
  const chunks = [];
  for await (const chunk of req) {
    chunks.push(chunk);
  }
  if (chunks.length === 0) {
    return {};
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    return null;
  }
}

function authenticated(req) {
  const cookie = req.headers.cookie ?? "";
  return cookie.split(";").some((part) => part.trim() === `${cookieName}=${token}`);
}

function scheduleNewTurn(pageId, turnId, message) {
  const detail = state.details[pageId];
  const start = Date.now();
  const suffix = String(++state.turns);
  const steps = [
    { after: 120, event: { turnId, type: "tool.started", data: { callId: `call-${suffix}`, tool: "resources_list", arguments: { kind: "Deployment", namespace: "shop" } } } },
    { after: 240, event: { turnId, type: "tool.finished", data: { callId: `call-${suffix}`, tool: "resources_list", status: "ok", durationMs: 61 } } },
    {
      after: 360,
      event: {
        turnId,
        type: "answer",
        data: {
          messageId: `m-${suffix}`,
          content: `Investigation answer for: ${message}`,
          citations: [{ callId: `call-${suffix}`, pointer: "/items/0" }],
        },
      },
      message: {
        id: `m-${suffix}`,
        turnId,
        role: "assistant",
        content: `Investigation answer for: ${message}`,
        citations: [{ callId: `call-${suffix}`, pointer: "/items/0" }],
      },
    },
    { after: 500, event: { turnId, type: "turn.finished", data: { status: "completed" } } },
  ];
  for (const step of steps) {
    setTimeout(() => {
      if (state.details[pageId] !== detail) {
        return;
      }
      if (step.event.type === "turn.finished") {
        detail.status = "completed";
        detail.activeTurnId = null;
      }
      if (step.message !== undefined) {
        detail.messages.push({ ...step.message, createdAt: new Date().toISOString() });
      }
      appendEvent(pageId, step.event);
      touch(pageId);
    }, Date.now() - start + step.after);
  }
}

function handleSSE(req, res) {
  const url = new URL(req.url, "http://fixture.invalid");
  const match = /^\/api\/v1\/pages\/([^/]+)\/events$/.exec(url.pathname);
  if (match === null) {
    sendError(res, 404, "not_found", "unknown stream");
    return;
  }
  const pageId = decodeURIComponent(match[1]);
  if (state.details[pageId] === undefined) {
    sendError(res, 404, "not_found", "page does not exist");
    return;
  }

  if (pageId === "p-reconnect" && state.reconnectDropped === false) {
    state.reconnectDropped = true;
    res.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
    });
    res.write(": connected\n\n");
    // The connection is lost before the rest of the turn is durable.
    setTimeout(() => {
      res.destroy();
    }, 30);
    return;
  }

  if (pageId === "p-reconnect" && state.reconnectTail.length > 0) {
    for (const event of state.reconnectTail) {
      (state.events[pageId] ?? (state.events[pageId] = [])).push(event);
    }
    state.reconnectTail = [];
    const detail = state.details[pageId];
    if (detail !== undefined) {
      detail.status = "completed";
      detail.activeTurnId = null;
    }
  }

  res.writeHead(200, {
    "Content-Type": "text/event-stream",
    "Cache-Control": "no-cache, no-transform",
    Connection: "keep-alive",
    "X-Accel-Buffering": "no",
  });
  res.write(": connected\n\n");

  const lastEvent = req.headers["last-event-id"];
  const rawCursor = typeof lastEvent === "string" && lastEvent !== "" ? lastEvent : (url.searchParams.get("after") ?? "0");
  let cursor = Number(rawCursor);
  if (!Number.isFinite(cursor) || cursor < 0) {
    cursor = 0;
  }

  const writeEvent = (event) => {
    res.write(`id: ${event.seq}\n`);
    res.write(`data: ${JSON.stringify(event)}\n\n`);
  };

  let closed = false;
  const finish = () => {
    closed = true;
    clearInterval(timer);
    clearInterval(heartbeat);
  };

  const deliver = () => {
    if (closed) {
      return;
    }
    const events = state.events[pageId] ?? [];
    if (state.reconnectDropped === false && pageId === "p-reconnect" && events.length > 4) {
      // Drop the first connection mid-turn exactly once. The browser reconnects with Last-Event-ID and the
      // replay must resume at the next durable sequence.
      state.reconnectDropped = true;
      for (const event of events.filter((event) => event.seq > cursor && event.seq <= 4)) {
        writeEvent(event);
        cursor = event.seq;
      }
      res.destroy();
      finish();
      return;
    }
    for (const event of events.filter((event) => event.seq > cursor)) {
      writeEvent(event);
      cursor = event.seq;
    }
  };

  deliver();
  const timer = setInterval(deliver, 60);
  const heartbeat = setInterval(() => {
    if (!closed) {
      res.write(": keep-alive\n\n");
    }
  }, 5_000);
  req.on("close", finish);
  res.on("close", finish);
}

async function handleApi(req, res) {
  const url = new URL(req.url, "http://fixture.invalid");
  const path = url.pathname;

  if (req.method === "POST" && path === "/api/v1/session") {
    const body = await readBody(req);
    if (body === null || body.token !== token) {
      sendError(res, 401, "unauthenticated", "the operator token was not accepted");
      return;
    }
    res.writeHead(204, {
      "Set-Cookie": `${cookieName}=${token}; HttpOnly; SameSite=Strict; Path=/`,
    });
    res.end();
    return;
  }

  if (!authenticated(req)) {
    sendError(res, 401, "unauthenticated", "sign in first");
    return;
  }

  if (req.method === "GET" && path === "/api/v1/pages") {
    if (state.probeDown) {
      sendError(res, 503, "unavailable", "the investigation service is starting");
      return;
    }
    if (!state.probed) {
      state.probed = true;
      if (failOnce("probe")) {
        sendError(res, 503, "unavailable", "the investigation service is starting");
        return;
      }
    } else if (failOnce("refresh")) {
      sendError(res, 503, "unavailable", "history refresh failed");
      return;
    }
    sendJSON(res, 200, { pages: state.pages });
    return;
  }

  if (req.method === "POST" && path === "/api/v1/pages") {
    if (failOnce("create")) {
      sendError(res, 503, "unavailable", "the investigation service is starting");
      return;
    }
    const body = await readBody(req);
    if (body === null || typeof body.message !== "string" || body.message.trim() === "") {
      sendError(res, 400, "invalid_request", "message is required");
      return;
    }
    const key = req.headers["idempotency-key"];
    if (typeof key !== "string" || key === "") {
      sendError(res, 400, "invalid_request", "Idempotency-Key is required");
      return;
    }
    const id = `p-created-${++state.created}`;
    const turnId = `t-created-${state.created}`;
    const now = new Date().toISOString();
    state.details[id] = {
      id,
      title: body.message,
      status: "running",
      updatedAt: now,
      activeTurnId: turnId,
      ...(typeof body.namespace === "string" ? { namespace: body.namespace } : {}),
      ...(typeof body.target === "object" && body.target !== null
        ? { target: [body.target.kind, body.target.namespace, body.target.name].filter(Boolean).join("/") }
        : {}),
      lastEventId: 1,
      messages: [{ id: `m-user-${id}`, turnId, role: "user", content: body.message, createdAt: now, citations: [] }],
    };
    state.events[id] = [
      { seq: 1, pageId: id, turnId, type: "turn.started", time: now, data: {} },
    ];
    state.diffs[id] = { files: [] };
    state.pages.unshift(pageSummary(state.details[id]));
    scheduleNewTurn(id, turnId, body.message);
    sendJSON(res, 201, { pageId: id, turnId });
    return;
  }

  const pageMatch = /^\/api\/v1\/pages\/([^/]+)$/.exec(path);
  if (req.method === "GET" && pageMatch !== null) {
    const id = decodeURIComponent(pageMatch[1]);
    const detail = state.details[id];
    if (detail === undefined) {
      sendError(res, 404, "not_found", "page does not exist");
      return;
    }
    sendJSON(res, 200, detail);
    return;
  }

  const eventsMatch = /^\/api\/v1\/pages\/([^/]+)\/events$/.exec(path);
  if (req.method === "GET" && eventsMatch !== null) {
    if ((req.headers.accept ?? "").includes("text/event-stream")) {
      handleSSE(req, res);
      return;
    }
    const id = decodeURIComponent(eventsMatch[1]);
    const after = Number(url.searchParams.get("after") ?? "0");
    const list = (state.events[id] ?? []).filter((event) => event.seq > after);
    sendJSON(res, 200, { events: list });
    return;
  }

  const turnsMatch = /^\/api\/v1\/pages\/([^/]+)\/turns$/.exec(path);
  if (req.method === "POST" && turnsMatch !== null) {
    const id = decodeURIComponent(turnsMatch[1]);
    const detail = state.details[id];
    if (detail === undefined) {
      sendError(res, 404, "not_found", "page does not exist");
      return;
    }
    if (detail.activeTurnId !== null) {
      sendError(res, 409, "page_busy", "a turn is already active");
      return;
    }
    const body = await readBody(req);
    if (body === null || typeof body.message !== "string" || body.message.trim() === "") {
      sendError(res, 400, "invalid_request", "message is required");
      return;
    }
    const key = req.headers["idempotency-key"];
    if (typeof key !== "string" || key === "") {
      sendError(res, 400, "invalid_request", "Idempotency-Key is required");
      return;
    }
    const turnId = `t-reply-${++state.turns}`;
    const now = new Date().toISOString();
    detail.messages.push({ id: `m-user-${turnId}`, turnId, role: "user", content: body.message, createdAt: now, citations: [] });
    detail.status = "running";
    detail.activeTurnId = turnId;
    appendEvent(id, { turnId, type: "turn.started", data: {} });
    touch(id);
    scheduleNewTurn(id, turnId, body.message);
    sendJSON(res, 201, { pageId: id, turnId });
    return;
  }

  const stopMatch = /^\/api\/v1\/pages\/([^/]+)\/stop$/.exec(path);
  if (req.method === "POST" && stopMatch !== null) {
    const id = decodeURIComponent(stopMatch[1]);
    const detail = state.details[id];
    if (detail === undefined) {
      sendError(res, 404, "not_found", "page does not exist");
      return;
    }
    if (detail.activeTurnId !== null) {
      const turnId = detail.activeTurnId;
      detail.status = "stopped";
      detail.activeTurnId = null;
      appendEvent(id, { turnId, type: "turn.finished", data: { status: "stopped" } });
      touch(id);
    }
    sendJSON(res, 200, { status: detail.status });
    return;
  }

  const evidenceMatch = /^\/api\/v1\/pages\/([^/]+)\/evidence\/([^/]+)$/.exec(path);
  if (req.method === "GET" && evidenceMatch !== null) {
    const callId = decodeURIComponent(evidenceMatch[2]);
    const evidence = state.evidence[callId];
    if (evidence === undefined) {
      sendError(res, 404, "not_found", "evidence does not exist");
      return;
    }
    sendJSON(res, 200, evidence);
    return;
  }

  const diffMatch = /^\/api\/v1\/pages\/([^/]+)\/diff$/.exec(path);
  if (req.method === "GET" && diffMatch !== null) {
    const id = decodeURIComponent(diffMatch[1]);
    sendJSON(res, 200, state.diffs[id] ?? { files: [] });
    return;
  }

  sendError(res, 404, "not_found", "unknown route");
}

const mime = new Map([
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".css", "text/css; charset=utf-8"],
  [".png", "image/png"],
  [".svg", "image/svg+xml"],
  [".json", "application/json; charset=utf-8"],
  [".woff2", "font/woff2"],
]);

async function serveStatic(req, res, pathname) {
  const relative = pathname === "/" ? "index.html" : pathname.slice(1);
  const safe = normalize(relative).replace(/^(\.\.(\/|\\|$))+/, "");
  let file = join(distRoot, safe);
  try {
    const data = await readFile(file);
    res.writeHead(200, { "Content-Type": mime.get(extname(file)) ?? "application/octet-stream" });
    res.end(data);
    return;
  } catch {
    // SPA routes fall back to the shell.
    file = join(distRoot, "index.html");
    try {
      const data = await readFile(file);
      res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
      res.end(data);
    } catch {
      res.writeHead(404, { "Content-Type": "text/plain; charset=utf-8" });
      res.end("the dashboard build is missing; run npm run build");
    }
  }
}

const server = createServer((req, res) => {
  const url = new URL(req.url ?? "/", "http://fixture.invalid");

  if (url.pathname === "/__control/reset" && req.method === "POST") {
    void readBody(req).then((body) => {
      reset(body ?? {});
      sendJSON(res, 200, { ok: true });
    });
    return;
  }

  if (url.pathname === "/readyz") {
    sendJSON(res, 200, { status: "ready" });
    return;
  }

  if (url.pathname.startsWith("/api/")) {
    void handleApi(req, res).catch((cause) => {
      if (!res.headersSent) {
        sendError(res, 500, "internal", cause instanceof Error ? cause.message : String(cause));
      } else {
        res.destroy();
      }
    });
    return;
  }

  void serveStatic(req, res, url.pathname);
});

server.listen(port, "127.0.0.1", () => {
  process.stdout.write(`dashboard contract fixture listening on http://127.0.0.1:${port}\n`);
});
