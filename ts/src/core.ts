// ATalk 도메인 로직. 데몬 프로세스 안에서 단독으로 파일을 쓴다.
// Node 는 단일 스레드이고 여기서는 동기 fs 만 쓰므로 각 메서드는 원자적으로 실행된다.

import { EventEmitter } from "node:events";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import {
  appendJsonl, ensureDir, isValidAgentId, isValidKind, isValidRoomId, makeAgentId,
  readJson, readJsonl, repairJsonlTail, writeJsonAtomic,
} from "./fsutil.ts";
import {
  AGENT_MESSAGE_TYPES, LOG_STATES,
  type AgentCard, type AgentStatus, type AgentView, type AtalkEvent, type InboxMessage,
  type LogEntry, type LogState, type Membership, type MembershipEvent, type MembershipTable,
  type Message, type MessageType, type Pending, type Recovery, type Role, type Room,
  type RoomView, type Unread,
} from "./types.ts";

export class AtalkError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}
const bad = (code: string, msg: string) => new AtalkError(400, code, msg);
const notFound = (code: string, msg: string) => new AtalkError(404, code, msg);
const forbidden = (code: string, msg: string) => new AtalkError(403, code, msg);
const conflict = (code: string, msg: string) => new AtalkError(409, code, msg);

export function defaultIsAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return (e as NodeJS.ErrnoException).code === "EPERM";
  }
}

export interface CoreOptions {
  root: string;
  /** away 상태가 이 시간을 넘으면 강제 퇴장 (ms) */
  awayGraceMs?: number;
  isAlive?: (pid: number) => boolean;
  now?: () => Date;
}

export interface RegisterInput {
  kind: string;
  session_id?: string;
  name?: string;
  description?: string;
  capabilities?: string[];
  cwd?: string | null;
  pid?: number | null;
}

export interface PostInput {
  type?: string;
  to?: string[];
  reply_to?: number | null;
  body: string;
}

export class Atalk extends EventEmitter {
  readonly root: string;
  readonly awayGraceMs: number;
  private isAlive: (pid: number) => boolean;
  private now: () => Date;

  private agents = new Map<string, AgentCard>();
  private memberships: MembershipTable = {};
  private rooms = new Map<string, Room>();
  private lastSeq = new Map<string, number>();
  private cursors = new Map<string, Record<string, number>>();

  constructor(opts: CoreOptions) {
    super();
    this.root = path.resolve(opts.root);
    this.awayGraceMs = opts.awayGraceMs ?? 30 * 60_000;
    this.isAlive = opts.isAlive ?? defaultIsAlive;
    this.now = opts.now ?? (() => new Date());
    this.load();
  }

  // ───────────────────────── 경로 ─────────────────────────
  private get agentsDir() { return path.join(this.root, "agents"); }
  private get roomsDir() { return path.join(this.root, "rooms"); }
  private get membershipsFile() { return path.join(this.root, "memberships.json"); }
  private get membershipLog() { return path.join(this.root, "memberships.log.jsonl"); }
  private cardFile(id: string) { return path.join(this.agentsDir, `${id}.json`); }
  private roomDir(r: string) { return path.join(this.roomsDir, r); }
  private roomFile(r: string) { return path.join(this.roomDir(r), "room.json"); }
  private messagesFile(r: string) { return path.join(this.roomDir(r), "messages.jsonl"); }
  private cursorsFile(r: string) { return path.join(this.roomDir(r), "cursors.json"); }
  private logFile(r: string, agent: string) { return path.join(this.roomDir(r), "logs", `${agent}.jsonl`); }

  private ts(): string { return this.now().toISOString(); }

  // ───────────────────────── 로드 / 복구 ─────────────────────────
  private load(): void {
    ensureDir(this.agentsDir);
    ensureDir(this.roomsDir);

    for (const f of fs.readdirSync(this.agentsDir)) {
      if (!f.endsWith(".json")) continue;
      try {
        const card = readJson<AgentCard | null>(path.join(this.agentsDir, f), null);
        if (card && card.id) this.agents.set(card.id, card);
      } catch { /* 깨진 카드는 무시 */ }
    }

    for (const d of fs.readdirSync(this.roomsDir, { withFileTypes: true })) {
      if (!d.isDirectory()) continue;
      const room = readJson<Room | null>(this.roomFile(d.name), null);
      if (!room) continue;
      this.rooms.set(room.room_id, room);
      repairJsonlTail(this.messagesFile(room.room_id));
      const msgs = readJsonl<Message>(this.messagesFile(room.room_id));
      this.lastSeq.set(room.room_id, msgs.reduce((m, x) => Math.max(m, x.seq ?? 0), 0));
      this.cursors.set(room.room_id, readJson<Record<string, number>>(this.cursorsFile(room.room_id), {}));
    }

    this.memberships = readJson<MembershipTable>(this.membershipsFile, {});
    this.reconcile();
  }

  /** 시작할 때 테이블 정합성 점검: 카드/방이 없는 연결 제거, 방장 없는 방 정리 */
  private reconcile(): void {
    let dirty = false;
    for (const [agent, m] of Object.entries(this.memberships)) {
      const room = this.rooms.get(m.room);
      if (!this.agents.has(agent) || !room || room.closed_at) {
        delete this.memberships[agent];
        this.logMembership({ ts: this.ts(), agent, room: m.room, event: "leave", detail: "reconcile: 카드 또는 방 없음" });
        dirty = true;
      }
    }
    if (dirty) this.saveMemberships();
    for (const room of this.rooms.values()) {
      if (room.closed_at) continue;
      const ownerM = room.owner ? this.memberships[room.owner] : undefined;
      if (!ownerM || ownerM.room !== room.room_id) this.fixOwner(room, "reconcile");
    }
  }

  // ───────────────────────── 저장 ─────────────────────────
  private saveCard(card: AgentCard) { writeJsonAtomic(this.cardFile(card.id), card); }
  private saveMemberships() { writeJsonAtomic(this.membershipsFile, this.memberships); }
  private saveRoom(room: Room) { writeJsonAtomic(this.roomFile(room.room_id), room); }
  private saveCursors(r: string) { writeJsonAtomic(this.cursorsFile(r), this.cursors.get(r) ?? {}); }
  private logMembership(e: MembershipEvent) { appendJsonl(this.membershipLog, e); }

  private emitEvent(e: AtalkEvent) { this.emit("event", e); }

  // ───────────────────────── 조회 헬퍼 ─────────────────────────
  private requireAgent(id: string): AgentCard {
    const a = this.agents.get(id);
    if (!a) throw notFound("agent_not_found", `등록되지 않은 에이전트: ${id}`);
    return a;
  }

  private requireRoom(r: string, open = true): Room {
    const room = this.rooms.get(r);
    if (!room) throw notFound("room_not_found", `없는 대화방: ${r}`);
    if (open && room.closed_at) throw conflict("room_closed", `닫힌 대화방: ${r}`);
    return room;
  }

  private requireOwner(agent: string, room: Room) {
    if (room.owner !== agent) throw forbidden("not_owner", `방장만 할 수 있음 (방장: ${room.owner})`);
  }

  private membersOf(r: string): [string, Membership][] {
    return Object.entries(this.memberships)
      .filter(([, m]) => m.room === r)
      .sort((a, b) => a[1].joined_at.localeCompare(b[1].joined_at));
  }

  private nameOf(id: string): string {
    return this.agents.get(id)?.name ?? id;
  }

  statusOf(id: string): AgentStatus {
    const m = this.memberships[id];
    if (m) return m.state;
    const card = this.agents.get(id);
    if (card?.pid && !this.isAlive(card.pid)) return "offline";
    return "waiting";
  }

  private view(card: AgentCard): AgentView {
    const m = this.memberships[card.id];
    return { ...card, status: this.statusOf(card.id), room: m?.room ?? null, role: m?.role ?? null };
  }

  // ───────────────────────── 에이전트 (디스커버리) ─────────────────────────
  register(input: RegisterInput): { agent: AgentView; created: boolean; recovery: Recovery | null } {
    if (!input || !isValidKind(String(input.kind ?? ""))) {
      throw bad("invalid_kind", "kind 는 소문자/숫자/- 로 된 1~32자여야 함 (예: claude-code)");
    }
    const sid = input.session_id ? String(input.session_id) : randomUUID();
    const id = makeAgentId(input.kind, sid);
    const now = this.ts();
    const existing = this.agents.get(id);

    let card: AgentCard;
    if (existing) {
      card = { ...existing, updated_at: now };
      if (input.name !== undefined) card.name = String(input.name);
      if (input.description !== undefined) card.description = String(input.description);
      if (input.capabilities !== undefined) card.capabilities = input.capabilities.map(String);
      if (input.cwd !== undefined) card.cwd = input.cwd;
      // 살아 있는 PID 는 유지한다 (훅이 찾은 정확한 PID 를 MCP 등이 덮어쓰지 않게)
      if (input.pid && (!card.pid || !this.isAlive(card.pid))) card.pid = input.pid;
    } else {
      card = {
        id, kind: input.kind, session_id: sid,
        name: input.name ? String(input.name) : `${input.kind}-${sid.slice(0, 8)}`,
        description: input.description ? String(input.description) : "",
        capabilities: (input.capabilities ?? []).map(String),
        cwd: input.cwd ?? null,
        pid: input.pid ?? null,
        registered_at: now, updated_at: now,
      };
    }
    this.agents.set(id, card);
    this.saveCard(card);

    let recovery: Recovery | null = null;
    const m = this.memberships[id];
    if (m) {
      if (m.state === "away") this.markBack(id);
      recovery = this.buildRecovery(id);
    }
    this.emitEvent({ type: "agent", to: [], data: { agent: id, action: existing ? "update" : "register" } });
    return { agent: this.view(card), created: !existing, recovery };
  }

  updateCard(id: string, patch: Partial<Pick<AgentCard, "name" | "description" | "capabilities">>): AgentView {
    const card = this.requireAgent(id);
    if (patch.name !== undefined) card.name = String(patch.name);
    if (patch.description !== undefined) card.description = String(patch.description);
    if (patch.capabilities !== undefined) card.capabilities = patch.capabilities.map(String);
    card.updated_at = this.ts();
    this.saveCard(card);
    this.emitEvent({ type: "agent", to: [], data: { agent: id, action: "update" } });
    return this.view(card);
  }

  unregister(id: string): void {
    this.requireAgent(id);
    if (this.memberships[id]) this.leaveInternal(id, "leave", "등록 해제");
    this.agents.delete(id);
    fs.rmSync(this.cardFile(id), { force: true });
    this.emitEvent({ type: "agent", to: [], data: { agent: id, action: "unregister" } });
  }

  getAgent(id: string): AgentView {
    return this.view(this.requireAgent(id));
  }

  listAgents(filter: { status?: string; kind?: string } = {}): AgentView[] {
    return [...this.agents.values()]
      .map((c) => this.view(c))
      .filter((v) => (!filter.status || v.status === filter.status) && (!filter.kind || v.kind === filter.kind))
      .sort((a, b) => a.registered_at.localeCompare(b.registered_at));
  }

  // ───────────────────────── 대화방 ─────────────────────────
  createRoom(agent: string, input: { room_id: string; title?: string; max_messages?: number }): RoomView {
    this.requireAgent(agent);
    const r = String(input?.room_id ?? "");
    if (!isValidRoomId(r)) throw bad("invalid_room_id", "방 ID 는 문자/숫자/-/_ 로 된 1~64자여야 함");
    if (this.rooms.has(r) || fs.existsSync(this.roomDir(r))) throw conflict("room_exists", `이미 있는 방: ${r}`);

    const room: Room = {
      room_id: r,
      title: input.title ? String(input.title) : r,
      owner: agent,
      created_at: this.ts(),
      closed_at: null,
      invites: [],
      requests: [],
      policy: { max_messages: input.max_messages ?? 1000 },
    };
    ensureDir(path.join(this.roomDir(r), "logs"));
    this.rooms.set(r, room);
    this.lastSeq.set(r, 0);
    this.cursors.set(r, {});
    this.saveRoom(room);
    this.saveCursors(r);
    fs.writeFileSync(this.messagesFile(r), "");
    this.joinInternal(agent, r, "owner");
    this.emitEvent({ type: "room", to: [], data: { room: r, action: "create" } });
    return this.roomView(room);
  }

  private roomView(room: Room): RoomView {
    return {
      ...room,
      members: this.membersOf(room.room_id).map(([agent, m]) => ({
        agent, name: this.nameOf(agent), role: m.role, state: m.state,
      })),
      last_seq: this.lastSeq.get(room.room_id) ?? 0,
    };
  }

  getRoom(r: string): RoomView {
    return this.roomView(this.requireRoom(r, false));
  }

  listRooms(opts: { include_closed?: boolean } = {}): RoomView[] {
    return [...this.rooms.values()]
      .filter((r) => opts.include_closed || !r.closed_at)
      .sort((a, b) => a.created_at.localeCompare(b.created_at))
      .map((r) => this.roomView(r));
  }

  invite(owner: string, r: string, target: string): RoomView {
    const room = this.requireRoom(r);
    this.requireOwner(owner, room);
    this.requireAgent(target);
    if (this.memberships[target]?.room === r) throw conflict("already_member", `${target} 는 이미 이 방 멤버`);
    if (room.invites.some((i) => i.agent === target && i.status === "pending")) {
      throw conflict("already_invited", `${target} 는 이미 초대됨`);
    }
    room.invites.push({ agent: target, by: owner, at: this.ts(), status: "pending" });
    this.saveRoom(room);
    this.emitEvent({ type: "invite", to: [target], data: { room: r, title: room.title, by: owner } });
    return this.roomView(room);
  }

  respondInvite(agent: string, r: string, accept: boolean): RoomView {
    const room = this.requireRoom(r);
    const inv = room.invites.find((i) => i.agent === agent && i.status === "pending");
    if (!inv) throw notFound("no_invite", `${r} 방에서 받은 초대가 없음`);
    inv.status = accept ? "accepted" : "declined";
    this.saveRoom(room);
    if (accept) this.joinInternal(agent, r, "member");
    else if (room.owner) this.emitEvent({ type: "invite", to: [room.owner], data: { room: r, agent, declined: true } });
    return this.roomView(room);
  }

  requestJoin(agent: string, r: string, note = ""): RoomView {
    const room = this.requireRoom(r);
    this.requireAgent(agent);
    if (this.memberships[agent]?.room === r) throw conflict("already_member", "이미 이 방 멤버");
    if (room.requests.some((q) => q.agent === agent && q.status === "pending")) {
      throw conflict("already_requested", "이미 참여 신청함");
    }
    room.requests.push({ agent, note: String(note ?? ""), at: this.ts(), status: "pending" });
    this.saveRoom(room);
    if (room.owner) this.emitEvent({ type: "request", to: [room.owner], data: { room: r, agent, note } });
    return this.roomView(room);
  }

  respondRequest(owner: string, r: string, target: string, approve: boolean): RoomView {
    const room = this.requireRoom(r);
    this.requireOwner(owner, room);
    const req = room.requests.find((q) => q.agent === target && q.status === "pending");
    if (!req) throw notFound("no_request", `${target} 의 참여 신청이 없음`);
    req.status = approve ? "approved" : "rejected";
    this.saveRoom(room);
    if (approve) this.joinInternal(target, r, "member");
    else this.emitEvent({ type: "request", to: [target], data: { room: r, rejected: true } });
    return this.roomView(room);
  }

  leave(agent: string, reason = "퇴장"): void {
    this.requireAgent(agent);
    if (!this.memberships[agent]) throw conflict("not_in_room", "들어가 있는 방이 없음");
    this.leaveInternal(agent, "leave", reason);
  }

  pending(agent: string): Pending {
    this.requireAgent(agent);
    const out: Pending = { invites: [], requests: [] };
    for (const room of this.rooms.values()) {
      if (room.closed_at) continue;
      for (const i of room.invites) {
        if (i.agent === agent && i.status === "pending") out.invites.push({ ...i, room: room.room_id, title: room.title });
      }
      if (room.owner === agent) {
        for (const q of room.requests) if (q.status === "pending") out.requests.push({ ...q, room: room.room_id });
      }
    }
    return out;
  }

  // ───────────────────────── 입장 / 퇴장 (내부) ─────────────────────────
  /** 세션 하나 = 방 하나. 다른 방에 있으면 먼저 그 방에서 나온다 */
  private joinInternal(agent: string, r: string, role: Role): void {
    const cur = this.memberships[agent];
    if (cur?.room === r) {
      if (cur.state === "away") this.markBack(agent);
      return;
    }
    if (cur) this.leaveInternal(agent, "move", `${r} 방으로 이동`);

    const now = this.ts();
    this.memberships[agent] = { room: r, role, state: "in_room", joined_at: now, last_seen: now, away_since: null, away_reason: null };
    this.saveMemberships();

    const cursors = this.cursors.get(r)!;
    cursors[agent] = this.lastSeq.get(r) ?? 0; // 입장 이후 메시지부터 읽음
    this.saveCursors(r);

    // 이 방에 걸려 있던 내 초대/신청 정리
    const room = this.rooms.get(r)!;
    let touched = false;
    for (const i of room.invites) if (i.agent === agent && i.status === "pending") { i.status = "accepted"; touched = true; }
    for (const q of room.requests) if (q.agent === agent && q.status === "pending") { q.status = "approved"; touched = true; }
    if (touched) this.saveRoom(room);

    this.logMembership({ ts: now, agent, room: r, event: "join", detail: role });
    this.systemMessage(r, `${this.nameOf(agent)} 입장${role === "owner" ? " (방장)" : ""}`);
    this.emitEvent({ type: "membership", to: [agent], data: { agent, room: r, action: "join", role } });
  }

  private leaveInternal(agent: string, event: "leave" | "move" | "expire", reason: string): void {
    const m = this.memberships[agent];
    if (!m) return;
    delete this.memberships[agent];
    this.saveMemberships();

    const cursors = this.cursors.get(m.room);
    if (cursors) {
      delete cursors[agent];
      this.saveCursors(m.room);
    }
    this.logMembership({ ts: this.ts(), agent, room: m.room, event, detail: reason });
    this.systemMessage(m.room, `${this.nameOf(agent)} 퇴장 (${reason})`);
    this.emitEvent({ type: "membership", to: [agent], data: { agent, room: m.room, action: event, reason } });

    const room = this.rooms.get(m.room);
    if (room && room.owner === agent) this.fixOwner(room, "방장 퇴장");
  }

  /** 방장이 없거나 나갔으면 가장 먼저 들어온 멤버(in_room 우선)에게 넘기고, 아무도 없으면 방을 닫는다 */
  private fixOwner(room: Room, why: string): void {
    const members = this.membersOf(room.room_id);
    const next = members.find(([, m]) => m.state === "in_room") ?? members[0];
    if (next) {
      const [agent, m] = next;
      room.owner = agent;
      m.role = "owner";
      for (const [a, mm] of members) if (a !== agent) mm.role = "member";
      this.saveMemberships();
      this.saveRoom(room);
      this.logMembership({ ts: this.ts(), agent, room: room.room_id, event: "owner_transfer", detail: why });
      this.systemMessage(room.room_id, `방장 변경 → ${this.nameOf(agent)} (${why})`);
      this.emitEvent({ type: "membership", to: [agent], data: { agent, room: room.room_id, action: "owner" } });
    } else {
      room.owner = null;
      room.closed_at = this.ts();
      for (const i of room.invites) if (i.status === "pending") i.status = "cancelled";
      for (const q of room.requests) if (q.status === "pending") q.status = "cancelled";
      this.saveRoom(room);
      this.logMembership({ ts: this.ts(), agent: "-", room: room.room_id, event: "close", detail: why });
      this.systemMessage(room.room_id, `대화방 닫힘 (남은 멤버 없음)`, true);
      this.emitEvent({ type: "room", to: [], data: { room: room.room_id, action: "close" } });
    }
  }

  // ───────────────────────── away / 복귀 / 이어받기 ─────────────────────────
  markAway(agent: string, reason = "away"): void {
    const m = this.memberships[agent];
    if (!m || m.state === "away") return;
    m.state = "away";
    m.away_since = this.ts();
    m.away_reason = reason;
    this.saveMemberships();
    this.logMembership({ ts: this.ts(), agent, room: m.room, event: "away", detail: reason });
    this.systemMessage(m.room, `${this.nameOf(agent)} 자리 비움 (${reason})`);
  }

  private markBack(agent: string): void {
    const m = this.memberships[agent];
    if (!m || m.state !== "away") return;
    m.state = "in_room";
    m.away_since = null;
    m.away_reason = null;
    m.last_seen = this.ts();
    this.saveMemberships();
    this.logMembership({ ts: this.ts(), agent, room: m.room, event: "back" });
    this.systemMessage(m.room, `${this.nameOf(agent)} 복귀`);
  }

  private buildRecovery(agent: string, recentCount = 0): Recovery | null {
    const m = this.memberships[agent];
    if (!m) return null;
    const unread = this.unread(agent, false).messages;
    // 처리하다 만 작업: 마지막 상태가 received/working 인 메시지
    const last = new Map<number, LogState>();
    for (const e of readJsonl<LogEntry>(this.logFile(m.room, agent))) last.set(e.seq, e.state);
    const unfinishedSeqs = [...last].filter(([, s]) => s === "received" || s === "working");
    let unfinished: Recovery["unfinished"] = [];
    let recent: Message[] | undefined;
    if (unfinishedSeqs.length || recentCount) {
      const all = readJsonl<Message>(this.messagesFile(m.room));
      const bySeq = new Map(all.map((x) => [x.seq, x]));
      unfinished = unfinishedSeqs.map(([seq, state]) => ({ seq, state, message: bySeq.get(seq) ?? null }));
      if (recentCount) recent = all.slice(-recentCount);
    }
    return { room: m.room, role: m.role, unread, unfinished, ...(recent ? { recent } : {}) };
  }

  /**
   * 새 세션이 죽은 이전 세션의 자리를 이어받는다 (재개 시 세션 ID 가 바뀐 경우).
   * 조건: 같은 kind, 같은 cwd(둘 다 있을 때), 이전 세션이 away 이거나 프로세스가 죽었음, 새 세션은 방이 없음.
   */
  takeover(newId: string, oldId: string): Recovery {
    const nu = this.requireAgent(newId);
    const old = this.requireAgent(oldId);
    if (newId === oldId) throw bad("same_agent", "자기 자신은 이어받을 수 없음");
    if (nu.kind !== old.kind) throw forbidden("kind_mismatch", "같은 종류의 에이전트만 이어받을 수 있음");
    if (nu.cwd && old.cwd && path.resolve(nu.cwd) !== path.resolve(old.cwd)) {
      throw forbidden("cwd_mismatch", "같은 작업 폴더의 세션만 이어받을 수 있음");
    }
    const m = this.memberships[oldId];
    if (!m) throw conflict("not_in_room", `${oldId} 는 들어가 있는 방이 없음`);
    const oldDead = m.state === "away" || (old.pid !== null && !this.isAlive(old.pid));
    if (!oldDead) throw conflict("still_alive", `${oldId} 가 아직 살아 있음`);
    if (this.memberships[newId]) throw conflict("already_in_room", "새 세션이 이미 다른 방에 있음. 먼저 나와야 함");

    const now = this.ts();
    delete this.memberships[oldId];
    this.memberships[newId] = { ...m, state: "in_room", away_since: null, away_reason: null, last_seen: now };
    this.saveMemberships();

    const cursors = this.cursors.get(m.room)!;
    cursors[newId] = cursors[oldId] ?? this.lastSeq.get(m.room) ?? 0;
    delete cursors[oldId];
    this.saveCursors(m.room);

    const room = this.rooms.get(m.room)!;
    if (room.owner === oldId) {
      room.owner = newId;
      this.saveRoom(room);
    }
    // 이전 세션의 처리 기록을 이어받아야 미완료 작업이 보인다
    const oldLog = this.logFile(m.room, oldId);
    if (fs.existsSync(oldLog)) {
      for (const e of readJsonl<LogEntry>(oldLog)) appendJsonl(this.logFile(m.room, newId), { ...e, agent: newId });
    }

    this.logMembership({ ts: now, agent: newId, room: m.room, event: "takeover", detail: `from ${oldId}` });
    this.systemMessage(m.room, `${this.nameOf(newId)} 가 ${this.nameOf(oldId)} 세션을 이어받음`);
    this.emitEvent({ type: "membership", to: [newId], data: { agent: newId, room: m.room, action: "takeover", from: oldId } });
    return this.buildRecovery(newId, 30)!;
  }

  /**
   * 주기적 점검: in_room 인데 프로세스가 죽었으면 away, away 가 유예 시간을 넘으면 강제 퇴장.
   */
  sweep(): void {
    const now = this.now().getTime();
    for (const [agent, m] of Object.entries(this.memberships)) {
      const card = this.agents.get(agent);
      if (!card) {
        this.leaveInternal(agent, "expire", "카드 없음");
        continue;
      }
      if (m.state === "in_room" && card.pid && !this.isAlive(card.pid)) {
        this.markAway(agent, "process_exit");
      } else if (m.state === "away" && m.away_since && now - Date.parse(m.away_since) > this.awayGraceMs) {
        this.leaveInternal(agent, "expire", "자리 비움 유예 시간 초과");
      }
    }
  }

  // ───────────────────────── 메시지 ─────────────────────────
  private appendMessage(r: string, msg: Omit<Message, "seq" | "id" | "ts" | "room">): Message {
    const seq = (this.lastSeq.get(r) ?? 0) + 1;
    const full: Message = { seq, id: randomUUID(), ts: this.ts(), room: r, ...msg };
    appendJsonl(this.messagesFile(r), full);
    this.lastSeq.set(r, seq);
    const members = this.membersOf(r).map(([a]) => a).filter((a) => a !== msg.from);
    this.emitEvent({ type: "message", to: members, data: full });
    return full;
  }

  private systemMessage(r: string, body: string, force = false): void {
    const room = this.rooms.get(r);
    if (!room || (room.closed_at && !force)) return;
    this.appendMessage(r, { from: "system", type: "system", to: [], reply_to: null, body });
  }

  post(agent: string, r: string, input: PostInput): Message {
    this.requireAgent(agent);
    const room = this.requireRoom(r);
    const m = this.memberships[agent];
    if (!m || m.room !== r) throw forbidden("not_member", `${r} 방 멤버가 아님`);
    const type = (input.type ?? "chat") as MessageType;
    if (!(AGENT_MESSAGE_TYPES as readonly string[]).includes(type)) {
      throw bad("invalid_type", `type 은 ${AGENT_MESSAGE_TYPES.join("|")} 중 하나`);
    }
    const body = String(input.body ?? "");
    if (!body.trim()) throw bad("empty_body", "메시지 본문이 비어 있음");
    const to = (input.to ?? []).map(String);
    for (const t of to) {
      if (this.memberships[t]?.room !== r) throw bad("invalid_to", `받는 대상 ${t} 는 이 방 멤버가 아님`);
    }
    if ((this.lastSeq.get(r) ?? 0) >= room.policy.max_messages) {
      throw new AtalkError(429, "room_limit", `방 메시지 한도(${room.policy.max_messages}) 초과. 방장이 한도를 올리거나 새 방을 만들어야 함`);
    }
    const reply_to = input.reply_to == null ? null : Number(input.reply_to);

    m.last_seen = this.ts();
    if (m.state === "away") this.markBack(agent);
    else this.saveMemberships();
    return this.appendMessage(r, { from: agent, type, to, reply_to, body });
  }

  readMessages(r: string, after = 0, limit = 200): Message[] {
    this.requireRoom(r, false);
    const all = readJsonl<Message>(this.messagesFile(r)).filter((x) => x.seq > after);
    return all.slice(0, Math.max(1, Math.min(limit, 1000)));
  }

  /** 내 방의 cursor 이후 메시지 (내가 쓴 것 제외). for_me: 나에게 처리 책임이 있는 메시지 */
  unread(agent: string, ack = false): Unread {
    this.requireAgent(agent);
    const m = this.memberships[agent];
    if (!m) return { room: null, cursor: 0, messages: [] };
    const cursors = this.cursors.get(m.room) ?? {};
    const cursor = cursors[agent] ?? 0;
    const messages: InboxMessage[] = readJsonl<Message>(this.messagesFile(m.room))
      .filter((x) => x.seq > cursor && x.from !== agent)
      .map((x) => ({ ...x, for_me: x.type !== "system" && (x.to.length === 0 || x.to.includes(agent)) }));
    let newCursor = cursor;
    if (ack) {
      newCursor = this.lastSeq.get(m.room) ?? cursor;
      if (newCursor !== cursor) {
        cursors[agent] = newCursor;
        this.saveCursors(m.room);
      }
    }
    m.last_seen = this.ts();
    return { room: m.room, cursor: newCursor, messages };
  }

  ack(agent: string, seq: number): number {
    this.requireAgent(agent);
    const m = this.memberships[agent];
    if (!m) throw conflict("not_in_room", "들어가 있는 방이 없음");
    const cursors = this.cursors.get(m.room)!;
    const target = Math.min(Number(seq), this.lastSeq.get(m.room) ?? 0);
    if (target > (cursors[agent] ?? 0)) {
      cursors[agent] = target;
      this.saveCursors(m.room);
    }
    return cursors[agent];
  }

  // ───────────────────────── 처리 기록 ─────────────────────────
  log(agent: string, r: string, input: { seq: number; state: string; note?: string }): LogEntry {
    this.requireAgent(agent);
    this.requireRoom(r, false);
    if (this.memberships[agent]?.room !== r) throw forbidden("not_member", `${r} 방 멤버가 아님`);
    const seq = Number(input.seq);
    if (!Number.isInteger(seq) || seq < 1 || seq > (this.lastSeq.get(r) ?? 0)) throw bad("invalid_seq", `없는 메시지 번호: ${input.seq}`);
    if (!(LOG_STATES as readonly string[]).includes(input.state)) throw bad("invalid_state", `state 는 ${LOG_STATES.join("|")} 중 하나`);
    const entry: LogEntry = { ts: this.ts(), agent, seq, state: input.state as LogState, note: String(input.note ?? "") };
    appendJsonl(this.logFile(r, agent), entry);
    this.emitEvent({ type: "log", to: [], data: { room: r, ...entry } });
    return entry;
  }

  readLogs(r: string, agent?: string): LogEntry[] {
    this.requireRoom(r, false);
    const dir = path.join(this.roomDir(r), "logs");
    if (agent) {
      if (!isValidAgentId(agent)) throw bad("invalid_agent", "잘못된 agent id");
      return readJsonl<LogEntry>(this.logFile(r, agent));
    }
    if (!fs.existsSync(dir)) return [];
    return fs.readdirSync(dir)
      .filter((f) => f.endsWith(".jsonl"))
      .flatMap((f) => readJsonl<LogEntry>(path.join(dir, f)))
      .sort((a, b) => a.ts.localeCompare(b.ts));
  }

  /**
   * wait 용: 지금 바로 처리할 메시지가 있는가.
   * 대기 중인 초대/신청은 계속 남아 있으므로 여기서 보지 않는다 (새 초대/신청 이벤트가 오면 wait 이 깨어남).
   */
  hasWork(agent: string): boolean {
    return this.unread(agent, false).messages.some((x) => x.for_me);
  }
}
