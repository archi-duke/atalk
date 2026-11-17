// ATalk 데몬: 파일을 쓰는 유일한 프로세스. REST + SSE + long-poll wait.

import http from "node:http";
import { Atalk, AtalkError } from "./core.ts";
import type { AtalkEvent } from "./types.ts";
import { VIEWER_HTML } from "./viewer.ts";

export interface ServerOptions {
  root: string;
  port?: number;
  host?: string;
  awayGraceMs?: number;
  sweepMs?: number;
}

type Handler = (ctx: Ctx) => unknown | Promise<unknown>;
interface Ctx {
  req: http.IncomingMessage;
  res: http.ServerResponse;
  params: string[];
  query: URLSearchParams;
  body: any;
}

export function createServer(opts: ServerOptions) {
  const core = new Atalk({ root: opts.root, awayGraceMs: opts.awayGraceMs });
  const routes: { method: string; re: RegExp; h: Handler }[] = [];
  const route = (method: string, pattern: string, h: Handler) => {
    const re = new RegExp("^" + pattern.replace(/:[a-z]+/g, "([^/]+)") + "/?$");
    routes.push({ method, re, h });
  };

  // ── SSE 구독자 ──
  const agentStreams = new Map<string, Set<http.ServerResponse>>();
  const viewerStreams = new Set<http.ServerResponse>();
  const sse = (res: http.ServerResponse, ev: AtalkEvent) => {
    res.write(`event: ${ev.type}\ndata: ${JSON.stringify(ev.data)}\n\n`);
  };
  core.on("event", (ev: AtalkEvent) => {
    for (const res of viewerStreams) sse(res, ev);
    for (const a of ev.to) for (const res of agentStreams.get(a) ?? []) sse(res, ev);
  });
  const openSse = (res: http.ServerResponse) => {
    res.writeHead(200, {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache",
      connection: "keep-alive",
    });
    res.write(": connected\n\n");
    const ping = setInterval(() => res.write(": ping\n\n"), 25_000);
    res.on("close", () => clearInterval(ping));
  };

  const actor = (b: any): string => {
    const a = b?.agent;
    if (!a || typeof a !== "string") throw new AtalkError(400, "missing_agent", "요청 본문에 agent(내 agent id)가 필요함");
    return a;
  };
  const bool = (v: string | null) => v === "1" || v === "true";

  // ── 디스커버리 ──
  route("GET", "/health", () => ({ root: core.root }));
  route("GET", "/agents", ({ query }) => ({
    agents: core.listAgents({ status: query.get("status") ?? undefined, kind: query.get("kind") ?? undefined }),
  }));
  route("POST", "/agents", ({ body }) => core.register(body));
  route("GET", "/agents/:id", ({ params }) => ({ agent: core.getAgent(params[0]) }));
  route("PATCH", "/agents/:id", ({ params, body }) => ({ agent: core.updateCard(params[0], body ?? {}) }));
  route("DELETE", "/agents/:id", ({ params }) => (core.unregister(params[0]), {}));
  route("POST", "/agents/:id/away", ({ params, body }) => (core.markAway(params[0], body?.reason ?? "away"), { agent: core.getAgent(params[0]) }));
  route("POST", "/agents/:id/leave", ({ params, body }) => (core.leave(params[0], body?.reason ?? "퇴장"), { agent: core.getAgent(params[0]) }));
  route("POST", "/agents/:id/takeover", ({ params, body }) => ({ recovery: core.takeover(params[0], String(body?.from ?? "")) }));
  route("GET", "/agents/:id/unread", ({ params, query }) => core.unread(params[0], bool(query.get("ack"))));
  route("POST", "/agents/:id/ack", ({ params, body }) => ({ cursor: core.ack(params[0], body?.seq) }));
  route("GET", "/agents/:id/pending", ({ params }) => core.pending(params[0]));

  // long-poll: 나에게 온 메시지가 생기거나, 초대/신청/멤버십 이벤트가 오거나, 타임아웃까지 대기
  route("GET", "/agents/:id/wait", ({ params, query, req }) => {
    const id = params[0];
    core.getAgent(id);
    const timeoutMs = Math.min(Math.max(Number(query.get("timeout") ?? 60), 1), 600) * 1000;
    const ack = bool(query.get("ack"));
    const result = (reason: string) => ({ reason, ...core.unread(id, ack), pending: core.pending(id) });
    if (core.hasWork(id)) return result("message");
    return new Promise((resolve) => {
      let done = false;
      const finish = (reason: string) => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        core.off("event", onEvent);
        resolve(result(reason));
      };
      const onEvent = (ev: AtalkEvent) => {
        if (!ev.to.includes(id)) return;
        if (ev.type === "message") {
          if (core.hasWork(id)) finish("message");
        } else finish(ev.type);
      };
      const timer = setTimeout(() => finish("timeout"), timeoutMs);
      core.on("event", onEvent);
      req.on("close", () => { if (!done) { done = true; clearTimeout(timer); core.off("event", onEvent); } });
    });
  });

  route("GET", "/agents/:id/stream", ({ params, res }) => {
    const id = params[0];
    core.getAgent(id);
    openSse(res);
    if (!agentStreams.has(id)) agentStreams.set(id, new Set());
    agentStreams.get(id)!.add(res);
    res.on("close", () => agentStreams.get(id)?.delete(res));
    return SKIP;
  });

  // ── 대화방 ──
  route("GET", "/rooms", ({ query }) => ({ rooms: core.listRooms({ include_closed: bool(query.get("include_closed")) }) }));
  route("POST", "/rooms", ({ body }) => ({ room: core.createRoom(actor(body), body) }));
  route("GET", "/rooms/:r", ({ params }) => ({ room: core.getRoom(params[0]) }));
  route("POST", "/rooms/:r/invites", ({ params, body }) => ({ room: core.invite(actor(body), params[0], String(body.target ?? "")) }));
  route("POST", "/rooms/:r/invites/respond", ({ params, body }) => ({ room: core.respondInvite(actor(body), params[0], body.accept !== false) }));
  route("POST", "/rooms/:r/requests", ({ params, body }) => ({ room: core.requestJoin(actor(body), params[0], body.note ?? "") }));
  route("POST", "/rooms/:r/requests/respond", ({ params, body }) => ({
    room: core.respondRequest(actor(body), params[0], String(body.target ?? ""), body.approve !== false),
  }));
  route("GET", "/rooms/:r/messages", ({ params, query }) => ({
    messages: core.readMessages(params[0], Number(query.get("after") ?? 0), Number(query.get("limit") ?? 200)),
  }));
  route("POST", "/rooms/:r/messages", ({ params, body }) => ({ message: core.post(actor(body), params[0], body) }));
  route("GET", "/rooms/:r/logs", ({ params, query }) => ({ logs: core.readLogs(params[0], query.get("agent") ?? undefined) }));
  route("POST", "/rooms/:r/logs", ({ params, body }) => ({ log: core.log(actor(body), params[0], body) }));

  // ── 뷰어 ──
  route("GET", "/events", ({ res }) => {
    openSse(res);
    viewerStreams.add(res);
    res.on("close", () => viewerStreams.delete(res));
    return SKIP;
  });
  route("GET", "/", ({ res }) => {
    res.writeHead(200, { "content-type": "text/html; charset=utf-8" });
    res.end(VIEWER_HTML);
    return SKIP;
  });

  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url ?? "/", "http://localhost");
    const pathname = url.pathname.split("/").map((s) => { try { return decodeURIComponent(s); } catch { return s; } });
    const decoded = pathname.join("/");
    try {
      let matched = false;
      for (const r of routes) {
        if (r.method !== req.method) continue;
        // 경로 조각을 디코딩한 뒤 매칭 (한글 방 이름 지원). 조각 안의 '/' 는 isValid* 검사에서 걸러진다
        const m = r.re.exec(decoded);
        if (!m) continue;
        matched = true;
        const body = req.method === "GET" || req.method === "DELETE" ? {} : await readBody(req);
        const out = await r.h({ req, res, params: m.slice(1), query: url.searchParams, body });
        if (out !== SKIP) send(res, 200, { ok: true, ...(out as object) });
        break;
      }
      if (!matched) send(res, 404, { ok: false, error: { code: "no_route", message: `${req.method} ${url.pathname}` } });
    } catch (e) {
      if (e instanceof AtalkError) send(res, e.status, { ok: false, error: { code: e.code, message: e.message } });
      else {
        console.error(e);
        send(res, 500, { ok: false, error: { code: "internal", message: (e as Error).message } });
      }
    }
  });

  const sweepTimer = setInterval(() => {
    try { core.sweep(); } catch (e) { console.error("sweep 실패", e); }
  }, opts.sweepMs ?? 10_000);
  sweepTimer.unref();

  return {
    core,
    server,
    listen(): Promise<number> {
      return new Promise((resolve) => {
        server.listen(opts.port ?? 7420, opts.host ?? "127.0.0.1", () => {
          resolve((server.address() as any).port);
        });
      });
    },
    async close() {
      clearInterval(sweepTimer);
      for (const set of agentStreams.values()) for (const r of set) r.end();
      for (const r of viewerStreams) r.end();
      server.closeAllConnections();
      await new Promise((r) => server.close(() => r(null)));
    },
  };
}

const SKIP = Symbol("skip");

function send(res: http.ServerResponse, status: number, data: unknown) {
  if (res.headersSent) return;
  res.writeHead(status, { "content-type": "application/json; charset=utf-8" });
  res.end(JSON.stringify(data));
}

function readBody(req: http.IncomingMessage): Promise<any> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let size = 0;
    req.on("data", (c: Buffer) => {
      size += c.length;
      if (size > 1_000_000) {
        reject(new AtalkError(413, "too_large", "요청 본문이 너무 큼 (최대 1MB)"));
        req.destroy();
      } else chunks.push(c);
    });
    req.on("end", () => {
      const text = Buffer.concat(chunks).toString("utf8");
      if (!text) return resolve({});
      try { resolve(JSON.parse(text)); } catch { reject(new AtalkError(400, "bad_json", "JSON 본문을 해석할 수 없음")); }
    });
    req.on("error", reject);
  });
}
