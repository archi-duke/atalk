// ATalk MCP 서버 (stdio). 어떤 에이전트 도구든 MCP 만 지원하면 대화방에 참여할 수 있다.
// 내 agent id: ATALK_AGENT 환경 변수 → 없으면 atalk_attach / atalk_register 호출로 정해진다.
// 이 프로세스는 에이전트가 살아 있는 동안만 살아 있으므로, 자기 PID 를 비정상 종료 감지용으로 등록한다.

import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { AtalkClient, formatMessages } from "./client.ts";

const client = new AtalkClient();
let self: string | null = process.env.ATALK_AGENT ?? null;

const server = new McpServer({ name: "atalk", version: "0.1.0" });

const text = (s: string) => ({ content: [{ type: "text" as const, text: s }] });
const json = (v: unknown) => text(JSON.stringify(v, null, 2));
const me = () => {
  if (!self) throw new Error("내 agent id 가 정해지지 않음. 먼저 atalk_attach(agent_id) 또는 atalk_register 를 호출하세요.");
  return self;
};

function tool<S extends z.ZodRawShape>(name: string, description: string, shape: S, fn: (a: z.objectOutputType<S, z.ZodTypeAny>) => Promise<ReturnType<typeof text>>) {
  server.registerTool(name, { description, inputSchema: shape }, (async (args: any) => {
    try {
      return await fn(args);
    } catch (e) {
      return { ...text(`오류: ${(e as Error).message}`), isError: true };
    }
  }) as any);
}

async function names(): Promise<Record<string, string>> {
  try {
    const { agents } = await client.agents();
    return Object.fromEntries(agents.map((a: any) => [a.id, a.name]));
  } catch {
    return {};
  }
}

tool("atalk_attach", "이미 등록된 내 네임카드(agent id)에 이 MCP 연결을 붙인다. 세션 시작 시 안내받은 id 를 넣는다.",
  { agent_id: z.string() },
  async ({ agent_id }) => {
    const { agent } = await client.agent(agent_id);
    self = agent.id;
    await client.register({ kind: agent.kind, session_id: agent.session_id, pid: process.pid });
    return json({ attached: agent.id, status: agent.status, room: agent.room });
  });

tool("atalk_register", "네임카드를 등록(또는 갱신)하고 이 연결의 내 id 로 삼는다. session_id 를 모르면 생략하면 새로 만든다.",
  {
    kind: z.string().describe("에이전트 종류: claude-code | codex | gemini-cli | opencode | custom ..."),
    session_id: z.string().optional(),
    name: z.string().optional(),
    description: z.string().optional().describe("어떤 작업을 하는 에이전트인지"),
    capabilities: z.array(z.string()).optional().describe("할 수 있는 일 목록"),
  },
  async (a) => {
    const r = await client.register({ ...a, cwd: process.cwd(), pid: process.pid });
    self = r.agent.id;
    return json(r);
  });

tool("atalk_update_card", "내 네임카드의 이름/설명/능력을 수정한다.",
  { name: z.string().optional(), description: z.string().optional(), capabilities: z.array(z.string()).optional() },
  async (a) => json(await client.updateCard(me(), a)));

tool("atalk_list_agents", "등록된 에이전트(네임카드) 목록. status 로 거를 수 있다 (waiting = 초대 가능).",
  { status: z.enum(["waiting", "in_room", "away", "offline"]).optional(), kind: z.string().optional() },
  async (a) => json((await client.agents(a)).agents.map((x: any) => ({
    id: x.id, name: x.name, kind: x.kind, status: x.status, room: x.room, description: x.description, capabilities: x.capabilities,
  }))));

tool("atalk_list_rooms", "열려 있는 대화방 목록", {}, async () =>
  json((await client.rooms()).rooms.map((r: any) => ({
    room_id: r.room_id, title: r.title, owner: r.owner, members: r.members.map((m: any) => m.agent), last_seq: r.last_seq,
  }))));

tool("atalk_create_room", "대화방을 만들고 방장이 된다. 다른 방에 있었다면 자동으로 나온다.",
  { room_id: z.string().describe("문자/숫자/-/_ 1~64자"), title: z.string().optional() },
  async ({ room_id, title }) => json((await client.createRoom(me(), room_id, title)).room));

tool("atalk_invite", "(방장) 에이전트를 내 방에 초대한다.", { target: z.string().describe("초대할 agent id") },
  async ({ target }) => {
    const room = await client.myRoom(me());
    await client.invite(me(), room, target);
    return text(`${target} 를 ${room} 방에 초대함`);
  });

tool("atalk_respond_invite", "받은 초대를 수락/거절한다. 수락하면 현재 방에서 나와 그 방에 들어간다.",
  { room: z.string(), accept: z.boolean() },
  async ({ room, accept }) => (await client.respondInvite(me(), room, accept), text(accept ? `${room} 입장` : `${room} 초대 거절`)));

tool("atalk_request_join", "대화방 방장에게 참여 신청을 보낸다.", { room: z.string(), note: z.string().optional() },
  async ({ room, note }) => (await client.requestJoin(me(), room, note), text(`${room} 방에 참여 신청함`)));

tool("atalk_respond_request", "(방장) 참여 신청을 승인/거절한다.", { target: z.string(), approve: z.boolean() },
  async ({ target, approve }) => {
    const room = await client.myRoom(me());
    await client.respondRequest(me(), room, target, approve);
    return text(`${target} 신청 ${approve ? "승인" : "거절"}`);
  });

tool("atalk_pending", "나에게 온 초대와, 내가 방장인 방에 온 참여 신청", {}, async () => json(await client.pending(me())));

tool("atalk_post", "내 방에 메시지를 쓴다. 일을 맡길 땐 type=task 와 to 를, 결과는 type=result 와 reply_to 를 쓴다.",
  {
    body: z.string(),
    type: z.enum(["chat", "task", "result", "status"]).optional(),
    to: z.array(z.string()).optional().describe("받는 agent id 들. 비우면 방 전체"),
    reply_to: z.number().int().optional().describe("답하는 메시지의 seq"),
  },
  async (a) => {
    const room = await client.myRoom(me());
    const { message } = await client.post(me(), room, a);
    return text(`#${message.seq} 전송 (${room})`);
  });

tool("atalk_unread", "내 방의 안 읽은 메시지. ack=true 면 읽음 처리한다.", { ack: z.boolean().optional() },
  async ({ ack }) => {
    const r = await client.unread(me(), ack ?? true);
    return text(r.messages.length ? formatMessages(r.messages, await names()) : "(안 읽은 메시지 없음)");
  });

tool("atalk_wait", "나에게 온 메시지(또는 초대/신청)가 올 때까지 기다린다. 할 일이 없을 때 이걸 반복 호출해 대기한다.",
  { timeout_sec: z.number().int().min(1).max(600).optional() },
  async ({ timeout_sec }) => {
    const r = await client.wait(me(), timeout_sec ?? 120, true);
    const pend = [
      ...r.pending.invites.map((i: any) => `[초대] ${i.room} "${i.title}" (보낸 이 ${i.by}) → atalk_respond_invite`),
      ...r.pending.requests.map((q: any) => `[참여 신청] ${q.agent} → ${q.room} ${q.note ?? ""} → atalk_respond_request`),
    ];
    const body = formatMessages(r.messages, await names());
    return text([`(${r.reason})`, body, pend.join("\n")].filter(Boolean).join("\n\n"));
  });

tool("atalk_read", "내 방(또는 지정한 방)의 대화 기록", { room: z.string().optional(), after: z.number().int().optional(), limit: z.number().int().optional() },
  async ({ room, after, limit }) => {
    const r = await client.messages(room ?? (await client.myRoom(me())), after ?? 0, limit ?? 100);
    return text(formatMessages(r.messages, await names()) || "(메시지 없음)");
  });

tool("atalk_log", "받은 메시지를 어떻게 처리하고 있는지 기록한다 (received → working → done/failed/skipped).",
  { seq: z.number().int(), state: z.enum(["received", "working", "done", "skipped", "failed"]), note: z.string().optional() },
  async ({ seq, state, note }) => {
    const room = await client.myRoom(me());
    await client.log(me(), room, seq, state, note);
    return text(`기록: #${seq} ${state}`);
  });

tool("atalk_leave", "지금 방에서 나온다 (대기 상태로 돌아감).", {}, async () => (await client.leave(me()), text("퇴장함")));

tool("atalk_takeover", "재개 후 세션 ID 가 바뀌었을 때, 죽은 이전 세션의 방 자리를 이어받는다.", { from: z.string().describe("이전 agent id") },
  async ({ from }) => {
    const { recovery } = await client.takeover(me(), from);
    return text(`이어받음: ${recovery.room} 방 (${recovery.role})\n미완료 작업: ${recovery.unfinished.map((u: any) => "#" + u.seq).join(", ") || "없음"}\n\n최근 대화:\n${formatMessages(recovery.recent ?? [])}`);
  });

await server.connect(new StdioServerTransport());
process.stdin.on("end", () => process.exit(0));
