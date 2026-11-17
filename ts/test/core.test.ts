import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { Atalk } from "../src/core.ts";
import type { Message } from "../src/types.ts";

let root: string;
let clock: number;
let alive: Set<number>;
const mk = () => new Atalk({ root, awayGraceMs: 60_000, isAlive: (p) => alive.has(p), now: () => new Date(clock) });

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "atalk-test-"));
  clock = Date.parse("2026-11-17T00:00:00Z");
  alive = new Set([100, 200, 300]);
});

function setup(core: Atalk) {
  const a = core.register({ kind: "claude-code", session_id: "aaa", name: "A", pid: 100 }).agent.id;
  const b = core.register({ kind: "codex", session_id: "bbb", name: "B", pid: 200 }).agent.id;
  const c = core.register({ kind: "gemini-cli", session_id: "ccc", name: "C", pid: 300 }).agent.id;
  return { a, b, c };
}

test("네임카드는 agents/<kind>_<session>.json 으로 저장되고 waiting 상태로 시작", () => {
  const core = mk();
  const { a } = setup(core);
  assert.equal(a, "claude-code_aaa");
  assert.ok(fs.existsSync(path.join(root, "agents", "claude-code_aaa.json")));
  assert.equal(core.getAgent(a).status, "waiting");
  assert.equal(core.listAgents({ status: "waiting" }).length, 3);
});

test("방 생성 → 초대 → 수락 → 메시지 → unread(for_me) → ack", () => {
  const core = mk();
  const { a, b, c } = setup(core);
  core.createRoom(a, { room_id: "api", title: "API 작업" });
  assert.equal(core.getAgent(a).role, "owner");
  assert.throws(() => core.invite(b, "api", c), /방장만/);
  core.invite(a, "api", b);
  assert.equal(core.pending(b).invites.length, 1);
  core.respondInvite(b, "api", true);
  assert.equal(core.getAgent(b).status, "in_room");
  assert.deepEqual(core.getRoom("api").members.map((m) => m.agent), [a, b]);

  const m = core.post(a, "api", { type: "task", to: [b], body: "리뷰해 줘" });
  const ub = core.unread(b);
  assert.deepEqual(ub.messages.filter((x) => x.for_me).map((x) => x.seq), [m.seq]);
  assert.ok(core.hasWork(b));
  core.unread(b, true);
  assert.equal(core.hasWork(b), false);
  assert.equal(core.unread(a).messages.filter((x) => x.for_me).length, 0, "내가 보낸 메시지는 unread 에 없음");

  // 멤버가 아니면 쓰기 불가, 받는 사람도 멤버여야 함
  assert.throws(() => core.post(c, "api", { body: "끼어들기" }), /멤버가 아님/);
  assert.throws(() => core.post(a, "api", { body: "x", to: [c] }), /멤버가 아님/);
});

test("참여 신청 → 방장 승인", () => {
  const core = mk();
  const { a, c } = setup(core);
  core.createRoom(a, { room_id: "docs" });
  core.requestJoin(c, "docs", "문서 도와줄게요");
  assert.equal(core.pending(a).requests.length, 1);
  core.respondRequest(a, "docs", c, true);
  assert.equal(core.getAgent(c).room, "docs");
  assert.equal(core.pending(a).requests.length, 0);
});

test("세션 하나 = 방 하나: 다른 방에 들어가면 이전 방에서 자동 퇴장, 방장은 넘어감", () => {
  const core = mk();
  const { a, b, c } = setup(core);
  core.createRoom(a, { room_id: "r1" });
  core.invite(a, "r1", b);
  core.respondInvite(b, "r1", true);
  core.createRoom(c, { room_id: "r2" });
  core.invite(c, "r2", a);
  core.respondInvite(a, "r2", true); // a 는 r1 방장이었음

  assert.equal(core.getAgent(a).room, "r2");
  assert.equal(core.getRoom("r1").owner, b, "남은 멤버에게 방장 이전");
  assert.deepEqual(core.getRoom("r1").members.map((m) => m.agent), [b]);
  const table = JSON.parse(fs.readFileSync(path.join(root, "memberships.json"), "utf8"));
  assert.deepEqual(Object.keys(table).sort(), [a, b, c].sort());
  // 네임카드에는 방 정보가 없다
  const card = JSON.parse(fs.readFileSync(path.join(root, "agents", `${a}.json`), "utf8"));
  assert.equal("room" in card, false);
});

test("마지막 멤버가 나가면 방이 닫힘", () => {
  const core = mk();
  const { a } = setup(core);
  core.createRoom(a, { room_id: "solo" });
  core.leave(a);
  assert.ok(core.getRoom("solo").closed_at);
  assert.equal(core.listRooms().length, 0);
  assert.equal(core.getAgent(a).status, "waiting");
  assert.throws(() => core.requestJoin(a, "solo"), /닫힌/);
});

test("비정상 종료: 프로세스가 죽으면 away → 재등록 시 복귀 + 놓친 메시지/미완료 작업 전달", () => {
  const core = mk();
  const { a, b } = setup(core);
  core.createRoom(a, { room_id: "w" });
  core.invite(a, "w", b);
  core.respondInvite(b, "w", true);
  const t1 = core.post(a, "w", { type: "task", to: [b], body: "작업1" });
  core.unread(b, true);
  core.log(b, "w", { seq: t1.seq, state: "working", note: "진행 중" });

  alive.delete(200); // b 크래시
  core.sweep();
  assert.equal(core.getAgent(b).status, "away");
  const t2 = core.post(a, "w", { type: "task", to: [b], body: "작업2" });

  alive.add(201);
  const r = core.register({ kind: "codex", session_id: "bbb", pid: 201 }); // resume
  assert.equal(r.agent.status, "in_room");
  assert.equal(r.agent.pid, 201, "죽은 PID 는 새 PID 로 교체");
  assert.deepEqual(r.recovery!.unfinished.map((u) => u.seq), [t1.seq]);
  assert.ok(r.recovery!.unread.some((m) => m.seq === t2.seq && m.for_me));
});

test("away 유예 시간이 지나면 강제 퇴장", () => {
  const core = mk();
  const { a, b } = setup(core);
  core.createRoom(a, { room_id: "g" });
  core.invite(a, "g", b);
  core.respondInvite(b, "g", true);
  core.markAway(b, "session_end");
  clock += 30_000;
  core.sweep();
  assert.equal(core.getAgent(b).status, "away");
  clock += 31_000;
  core.sweep();
  assert.equal(core.getAgent(b).status, "waiting");
  assert.deepEqual(core.getRoom("g").members.map((m) => m.agent), [a]);
});

test("세션 ID 가 바뀐 경우 takeover 로 자리와 처리 기록을 이어받음", () => {
  const core = mk();
  const { a, b } = setup(core);
  core.register({ kind: "claude-code", session_id: "aaa", cwd: "D:/proj" });
  core.createRoom(a, { room_id: "t" });
  core.invite(a, "t", b);
  core.respondInvite(b, "t", true);
  const task = core.post(b, "t", { type: "task", to: [a], body: "해 줘" });
  core.log(a, "t", { seq: task.seq, state: "received" });

  alive.add(101);
  const a2 = core.register({ kind: "claude-code", session_id: "aaa2", cwd: "D:/proj", pid: 101 }).agent.id;
  assert.throws(() => core.takeover(a2, a), /살아 있음/);
  alive.delete(100);
  const other = core.register({ kind: "claude-code", session_id: "zzz", cwd: "D:/other" }).agent.id;
  assert.throws(() => core.takeover(other, a), /작업 폴더/);

  const rec = core.takeover(a2, a);
  assert.equal(rec.room, "t");
  assert.equal(core.getRoom("t").owner, a2);
  assert.deepEqual(rec.unfinished.map((u) => u.seq), [task.seq]);
  assert.equal(core.getAgent(a).status, "offline");
});

test("데몬 재시작: 파일에서 상태 복구, 깨진 마지막 줄 무시, seq 이어감", () => {
  let core = mk();
  const { a, b } = setup(core);
  core.createRoom(a, { room_id: "p" });
  core.invite(a, "p", b);
  core.respondInvite(b, "p", true);
  const m1 = core.post(a, "p", { body: "hello" });
  fs.appendFileSync(path.join(root, "rooms", "p", "messages.jsonl"), '{"seq":99,"bro'); // 쓰다가 죽음

  core = mk();
  assert.equal(core.getRoom("p").owner, a);
  assert.equal(core.getAgent(b).room, "p");
  const m2 = core.post(b, "p", { body: "world" });
  assert.equal(m2.seq, m1.seq + 1);
  const msgs = core.readMessages("p");
  assert.deepEqual(msgs.slice(-2).map((m: Message) => m.body), ["hello", "world"]);
});

test("재시작 시 카드가 사라진 연결은 정리되고 방장이 넘어감", () => {
  let core = mk();
  const { a, b } = setup(core);
  core.createRoom(a, { room_id: "q" });
  core.invite(a, "q", b);
  core.respondInvite(b, "q", true);
  fs.rmSync(path.join(root, "agents", `${a}.json`));
  core = mk();
  assert.equal(core.getRoom("q").owner, b);
  assert.deepEqual(core.getRoom("q").members.map((m) => m.agent), [b]);
});

test("잘못된 방 ID / 한글 방 ID", () => {
  const core = mk();
  const { a } = setup(core);
  assert.throws(() => core.createRoom(a, { room_id: "../x" }), /방 ID/);
  assert.throws(() => core.createRoom(a, { room_id: "CON" }), /방 ID/);
  core.createRoom(a, { room_id: "리팩터링-1" });
  assert.equal(core.getAgent(a).room, "리팩터링-1");
});
