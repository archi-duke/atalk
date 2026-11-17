// 데몬 HTTP 클라이언트 (CLI, MCP 서버, 훅이 공통으로 쓴다)

export const DEFAULT_URL = process.env.ATALK_URL ?? "http://127.0.0.1:7420";

export class ClientError extends Error {
  code: string;
  status: number;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export class AtalkClient {
  readonly base: string;
  constructor(base = DEFAULT_URL) {
    this.base = base.replace(/\/$/, "");
  }

  async call<T = any>(method: string, path: string, body?: unknown, timeoutMs = 15_000): Promise<T> {
    let res: Response;
    try {
      res = await fetch(this.base + path, {
        method,
        headers: body ? { "content-type": "application/json" } : undefined,
        body: body ? JSON.stringify(body) : undefined,
        signal: AbortSignal.timeout(timeoutMs),
      });
    } catch (e) {
      throw new ClientError(0, "unreachable", `ATalk 데몬에 연결할 수 없음 (${this.base}): ${(e as Error).message}`);
    }
    const data: any = await res.json().catch(() => ({}));
    if (!res.ok || data.ok === false) {
      throw new ClientError(res.status, data.error?.code ?? "http_error", data.error?.message ?? `HTTP ${res.status}`);
    }
    return data as T;
  }

  private e(s: string) { return encodeURIComponent(s); }

  health() { return this.call("GET", "/health", undefined, 2_000); }
  register(input: Record<string, unknown>) { return this.call("POST", "/agents", input); }
  agents(q: { status?: string; kind?: string } = {}) {
    const p = new URLSearchParams(Object.entries(q).filter(([, v]) => v) as [string, string][]);
    return this.call("GET", `/agents${p.size ? "?" + p : ""}`);
  }
  agent(id: string) { return this.call("GET", `/agents/${this.e(id)}`); }
  updateCard(id: string, patch: Record<string, unknown>) { return this.call("PATCH", `/agents/${this.e(id)}`, patch); }
  unregister(id: string) { return this.call("DELETE", `/agents/${this.e(id)}`); }
  away(id: string, reason: string) { return this.call("POST", `/agents/${this.e(id)}/away`, { reason }); }
  leave(id: string, reason?: string) { return this.call("POST", `/agents/${this.e(id)}/leave`, { reason }); }
  takeover(id: string, from: string) { return this.call("POST", `/agents/${this.e(id)}/takeover`, { from }); }
  unread(id: string, ack = false) { return this.call("GET", `/agents/${this.e(id)}/unread${ack ? "?ack=1" : ""}`); }
  ack(id: string, seq: number) { return this.call("POST", `/agents/${this.e(id)}/ack`, { seq }); }
  pending(id: string) { return this.call("GET", `/agents/${this.e(id)}/pending`); }
  wait(id: string, timeoutSec = 60, ack = true) {
    return this.call("GET", `/agents/${this.e(id)}/wait?timeout=${timeoutSec}${ack ? "&ack=1" : ""}`, undefined, (timeoutSec + 10) * 1000);
  }

  rooms(includeClosed = false) { return this.call("GET", `/rooms${includeClosed ? "?include_closed=1" : ""}`); }
  room(r: string) { return this.call("GET", `/rooms/${this.e(r)}`); }
  createRoom(agent: string, room_id: string, title?: string) { return this.call("POST", "/rooms", { agent, room_id, title }); }
  invite(agent: string, r: string, target: string) { return this.call("POST", `/rooms/${this.e(r)}/invites`, { agent, target }); }
  respondInvite(agent: string, r: string, accept: boolean) { return this.call("POST", `/rooms/${this.e(r)}/invites/respond`, { agent, accept }); }
  requestJoin(agent: string, r: string, note?: string) { return this.call("POST", `/rooms/${this.e(r)}/requests`, { agent, note }); }
  respondRequest(agent: string, r: string, target: string, approve: boolean) {
    return this.call("POST", `/rooms/${this.e(r)}/requests/respond`, { agent, target, approve });
  }
  messages(r: string, after = 0, limit = 200) { return this.call("GET", `/rooms/${this.e(r)}/messages?after=${after}&limit=${limit}`); }
  post(agent: string, r: string, msg: { body: string; type?: string; to?: string[]; reply_to?: number | null }) {
    return this.call("POST", `/rooms/${this.e(r)}/messages`, { agent, ...msg });
  }
  log(agent: string, r: string, seq: number, state: string, note?: string) {
    return this.call("POST", `/rooms/${this.e(r)}/logs`, { agent, seq, state, note });
  }
  logs(r: string, agent?: string) { return this.call("GET", `/rooms/${this.e(r)}/logs${agent ? "?agent=" + this.e(agent) : ""}`); }

  /** 내 방 ID (없으면 에러) */
  async myRoom(id: string): Promise<string> {
    const { agent } = await this.agent(id);
    if (!agent.room) throw new ClientError(409, "not_in_room", "들어가 있는 방이 없음");
    return agent.room;
  }
}

/** 에이전트에게 보여 줄 메시지 텍스트 */
export function formatMessages(msgs: any[], names: Record<string, string> = {}): string {
  const n = (id: string) => names[id] ?? id;
  return msgs.map((m) => {
    if (m.type === "system") return `#${m.seq} [system] ${m.body}`;
    const to = m.to?.length ? ` → ${m.to.map(n).join(", ")}` : "";
    const re = m.reply_to ? ` (re #${m.reply_to})` : "";
    const mark = m.for_me === false ? " (참고용: 나에게 온 것 아님)" : "";
    return `#${m.seq} [${m.type}] ${n(m.from)}${to}${re}${mark}\n${m.body}`;
  }).join("\n\n");
}
