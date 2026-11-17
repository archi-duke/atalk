// atalk CLI. 사람이 직접 쓰거나, MCP 를 못 쓰는 에이전트가 쉘 명령으로 쓴다.
// 내 agent id 는 --as 또는 ATALK_AGENT 환경 변수로 지정한다.

import os from "node:os";
import path from "node:path";
import { parseArgs } from "node:util";
import { AtalkClient, ClientError, formatMessages } from "./client.ts";

const HELP = `atalk — 에이전트 대화방

데몬
  atalk server [--root DIR] [--port 7420] [--grace-min 30]

디스커버리
  atalk register --kind KIND [--session SID] [--name N] [--desc D] [--cap a,b] [--pid PID] [--cwd DIR]
  atalk agents [--status waiting|in_room|away|offline] [--kind KIND]
  atalk whoami
  atalk card [--name N] [--desc D] [--cap a,b]          내 네임카드 수정
  atalk unregister

대화방
  atalk rooms [--all]
  atalk room ROOM
  atalk create ROOM [--title T]                       방 만들기 (내가 방장, 기존 방에서는 자동 퇴장)
  atalk invite AGENT                                  (방장) 내 방으로 초대
  atalk accept ROOM | atalk decline ROOM              초대 수락/거절
  atalk request ROOM [--note TEXT]                    참여 신청
  atalk approve AGENT | atalk reject AGENT            (방장) 신청 승인/거절
  atalk pending                                       나에게 온 초대 / 내 방에 온 신청
  atalk leave
  atalk takeover OLD_AGENT_ID                         죽은 이전 세션의 자리를 이어받기

대화
  atalk send TEXT [--type chat|task|result|status] [--to a,b] [--reply SEQ]
  atalk unread [--ack]
  atalk wait [--timeout 60] [--no-ack]                나에게 온 메시지가 올 때까지 대기
  atalk messages [ROOM] [--after SEQ] [--limit N]
  atalk log SEQ received|working|done|skipped|failed [NOTE]

공통 옵션: --as AGENT_ID (또는 ATALK_AGENT), --url URL (또는 ATALK_URL), --json
`;

const { values: o, positionals: pos } = parseArgs({
  allowPositionals: true,
  strict: false,
  options: {
    as: { type: "string" }, url: { type: "string" }, json: { type: "boolean" },
    root: { type: "string" }, port: { type: "string" }, "grace-min": { type: "string" },
    kind: { type: "string" }, session: { type: "string" }, name: { type: "string" }, desc: { type: "string" },
    cap: { type: "string" }, pid: { type: "string" }, cwd: { type: "string" }, status: { type: "string" },
    all: { type: "boolean" }, title: { type: "string" }, note: { type: "string" }, type: { type: "string" },
    to: { type: "string" }, reply: { type: "string" }, ack: { type: "boolean" }, "no-ack": { type: "boolean" },
    timeout: { type: "string" }, after: { type: "string" }, limit: { type: "string" }, help: { type: "boolean", short: "h" },
  },
});

const str = (v: unknown) => (typeof v === "string" ? v : undefined);
const list = (v: unknown) => (typeof v === "string" && v ? v.split(",").map((s) => s.trim()).filter(Boolean) : undefined);
const client = new AtalkClient(str(o.url));
const me = () => {
  const id = str(o.as) ?? process.env.ATALK_AGENT;
  if (!id) throw new ClientError(0, "no_identity", "--as AGENT_ID 또는 ATALK_AGENT 가 필요함");
  return id;
};
const out = (data: unknown, text?: string) => console.log(o.json || text === undefined ? JSON.stringify(data, null, 2) : text);

async function main() {
  const [cmd, ...args] = pos;
  if (!cmd || o.help) return console.log(HELP);

  switch (cmd) {
    case "server": {
      const { createServer } = await import("./server.ts");
      const root = path.resolve(str(o.root) ?? process.env.ATALK_ROOT ?? path.join(os.homedir(), ".atalk"));
      const graceMin = Number(str(o["grace-min"]) ?? process.env.ATALK_AWAY_GRACE_MIN ?? 30);
      const srv = createServer({ root, port: Number(str(o.port) ?? process.env.ATALK_PORT ?? 7420), awayGraceMs: graceMin * 60_000 });
      const port = await srv.listen();
      console.log(`ATalk 데몬 실행 중: http://127.0.0.1:${port}  (데이터: ${root}, away 유예 ${graceMin}분)`);
      const stop = () => srv.close().then(() => process.exit(0));
      process.on("SIGINT", stop);
      process.on("SIGTERM", stop);
      return;
    }
    case "register": {
      const r = await client.register({
        kind: str(o.kind), session_id: str(o.session), name: str(o.name), description: str(o.desc),
        capabilities: list(o.cap), pid: o.pid ? Number(o.pid) : undefined, cwd: str(o.cwd) ?? process.cwd(),
      });
      return out(r, `${r.created ? "등록" : "갱신"}: ${r.agent.id}  (상태 ${r.agent.status})\n` +
        (r.recovery ? `복구: ${r.recovery.room} 방, 안 읽은 메시지 ${r.recovery.unread.length}, 미완료 작업 ${r.recovery.unfinished.length}` : ""));
    }
    case "agents": {
      const r = await client.agents({ status: str(o.status), kind: str(o.kind) });
      return out(r, r.agents.map((a: any) =>
        `${a.status.padEnd(8)} ${a.id}  "${a.name}"${a.room ? `  @${a.room}${a.role === "owner" ? "(방장)" : ""}` : ""}` +
        `${a.capabilities.length ? `  [${a.capabilities.join(", ")}]` : ""}${a.description ? `\n         ${a.description}` : ""}`,
      ).join("\n") || "(등록된 에이전트 없음)");
    }
    case "whoami": {
      const r = await client.agent(me());
      return out(r);
    }
    case "card": {
      const r = await client.updateCard(me(), { name: str(o.name), description: str(o.desc), capabilities: list(o.cap) });
      return out(r);
    }
    case "unregister": return out(await client.unregister(me()), "등록 해제됨");
    case "rooms": {
      const r = await client.rooms(!!o.all);
      return out(r, r.rooms.map((x: any) =>
        `${x.room_id}${x.closed_at ? " (닫힘)" : ""}  "${x.title}"  방장 ${x.owner ?? "-"}  멤버 ${x.members.length}  메시지 ${x.last_seq}`,
      ).join("\n") || "(대화방 없음)");
    }
    case "room": return out(await client.room(need(args[0], "ROOM")));
    case "create": {
      const r = await client.createRoom(me(), need(args[0], "ROOM"), str(o.title));
      return out(r, `방 생성: ${r.room.room_id} (방장 ${me()})`);
    }
    case "invite": {
      const room = await client.myRoom(me());
      await client.invite(me(), room, need(args[0], "AGENT"));
      return out({ ok: true }, `${args[0]} 를 ${room} 방에 초대함`);
    }
    case "accept":
    case "decline": {
      const r = await client.respondInvite(me(), need(args[0], "ROOM"), cmd === "accept");
      return out(r, cmd === "accept" ? `${args[0]} 방 입장` : `${args[0]} 초대 거절`);
    }
    case "request": {
      await client.requestJoin(me(), need(args[0], "ROOM"), str(o.note));
      return out({ ok: true }, `${args[0]} 방에 참여 신청함`);
    }
    case "approve":
    case "reject": {
      const room = await client.myRoom(me());
      await client.respondRequest(me(), room, need(args[0], "AGENT"), cmd === "approve");
      return out({ ok: true }, `${args[0]} 신청 ${cmd === "approve" ? "승인" : "거절"}`);
    }
    case "pending": {
      const r = await client.pending(me());
      return out(r, [
        ...r.invites.map((i: any) => `초대: ${i.room} "${i.title}" (보낸 이 ${i.by})  → atalk accept ${i.room}`),
        ...r.requests.map((q: any) => `신청: ${q.agent} → ${q.room}${q.note ? ` "${q.note}"` : ""}  → atalk approve ${q.agent}`),
      ].join("\n") || "(대기 중인 초대/신청 없음)");
    }
    case "leave": return out(await client.leave(me()), "퇴장함");
    case "takeover": {
      const r = await client.takeover(me(), need(args[0], "OLD_AGENT_ID"));
      return out(r, `이어받음: ${r.recovery.room} 방\n미완료 작업 ${r.recovery.unfinished.length}건\n\n${formatMessages(r.recovery.recent ?? [])}`);
    }
    case "send": {
      const room = await client.myRoom(me());
      const r = await client.post(me(), room, {
        body: need(args.join(" "), "TEXT"), type: str(o.type), to: list(o.to), reply_to: o.reply ? Number(o.reply) : undefined,
      });
      return out(r, `#${r.message.seq} 전송 (${room})`);
    }
    case "unread": {
      const r = await client.unread(me(), !!o.ack);
      return out(r, r.messages.length ? formatMessages(r.messages) : "(안 읽은 메시지 없음)");
    }
    case "wait": {
      const r = await client.wait(me(), Number(str(o.timeout) ?? 60), !o["no-ack"]);
      const pend = [...r.pending.invites.map((i: any) => `[초대] ${i.room}`), ...r.pending.requests.map((q: any) => `[신청] ${q.agent} → ${q.room}`)];
      return out(r, `(${r.reason})\n${formatMessages(r.messages)}${pend.length ? "\n\n" + pend.join("\n") : ""}`.trim());
    }
    case "messages": {
      const room = args[0] ?? (await client.myRoom(me()));
      const r = await client.messages(room, Number(str(o.after) ?? 0), Number(str(o.limit) ?? 200));
      return out(r, formatMessages(r.messages) || "(메시지 없음)");
    }
    case "log": {
      const room = await client.myRoom(me());
      const r = await client.log(me(), room, Number(need(args[0], "SEQ")), need(args[1], "STATE"), args.slice(2).join(" ") || undefined);
      return out(r, `기록: #${args[0]} ${args[1]}`);
    }
    default:
      console.error(`알 수 없는 명령: ${cmd}\n`);
      console.log(HELP);
      process.exitCode = 2;
  }
}

function need(v: string | undefined, what: string): string {
  if (!v) throw new ClientError(0, "missing_arg", `${what} 가 필요함`);
  return v;
}

main().catch((e) => {
  console.error(e instanceof ClientError ? `오류 [${e.code}] ${e.message}` : e);
  process.exitCode = 1;
});
