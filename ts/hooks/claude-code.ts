// Claude Code 훅 어댑터.
//   SessionStart → 네임카드 자동 등록 (+ 복구 정보 / 내 agent id 를 컨텍스트로 주입)
//   Stop         → 나에게 온 새 메시지가 있으면 종료를 막고 메시지를 이어서 처리하게 함
//   SessionEnd   → away 처리 (resume 으로 돌아오면 복귀)
// 사용: node <atalk>/hooks/claude-code.ts session-start|stop|session-end
// 데몬이 꺼져 있으면 아무것도 하지 않고 조용히 끝난다 (세션을 방해하지 않음).

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { AtalkClient, formatMessages } from "../src/client.ts";
import { makeAgentId } from "../src/fsutil.ts";
import { findAgentPid } from "../src/procs.ts";

const KIND = "claude-code";
const client = new AtalkClient();

interface HookInput {
  session_id: string;
  cwd?: string;
  hook_event_name?: string;
  source?: string; // SessionStart: startup | resume | clear | compact
  stop_hook_active?: boolean;
  reason?: string; // SessionEnd
}

async function readStdin(): Promise<HookInput> {
  const chunks: Buffer[] = [];
  for await (const c of process.stdin) chunks.push(c as Buffer);
  return JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
}

function emit(obj: unknown) {
  process.stdout.write(JSON.stringify(obj));
}

/** Stop 훅에서 같은 초대/신청을 매번 알리지 않도록 알린 목록을 기억 */
function notifiedFile(id: string) {
  return path.join(os.tmpdir(), `atalk-hook-${id}.json`);
}

async function sessionStart(input: HookInput) {
  const cwd = input.cwd ?? process.cwd();
  const r = await client.register({
    kind: KIND,
    session_id: input.session_id,
    cwd,
    pid: findAgentPid(),
    name: process.env.ATALK_NAME ?? undefined,
    description: process.env.ATALK_DESCRIPTION ?? undefined,
    capabilities: process.env.ATALK_CAPABILITIES?.split(",").map((s) => s.trim()).filter(Boolean),
  });
  const a = r.agent;
  const lines = [
    `[ATalk] 이 세션은 에이전트 대화방(ATalk)에 등록되어 있습니다.`,
    `- 내 agent id: ${a.id}  (이름 "${a.name}", 상태 ${a.status})`,
    `- atalk MCP 도구를 쓰기 전에 atalk_attach(agent_id="${a.id}") 를 한 번 호출하세요.`,
    `- 대화방에서 받은 메시지를 처리할 때는 atalk_log 로 received → working → done 을 기록하고, 결과는 atalk_post(type="result", reply_to=seq) 로 남기세요.`,
    `- 나에게 온 메시지가 없으면 대화를 억지로 이어가지 마세요.`,
  ];
  if (r.recovery) {
    const rec = r.recovery;
    lines.push(``, `[ATalk 복구] ${rec.room} 방으로 복귀했습니다 (역할 ${rec.role}).`);
    if (rec.unfinished.length) {
      lines.push(`처리하다 만 작업:`);
      for (const u of rec.unfinished) lines.push(`  #${u.seq} (${u.state}) ${u.message ? u.message.body.slice(0, 200) : ""}`);
    }
    if (rec.unread.length) lines.push(`놓친 메시지:`, formatMessages(rec.unread));
  } else if (input.source === "startup" && !r.created) {
    lines.push(`- 이전에 등록된 카드를 다시 사용합니다.`);
  }
  if (!r.recovery) {
    // 같은 폴더의 죽은 claude-code 세션이 방에 남아 있으면 이어받기를 안내
    const { agents } = await client.agents({ kind: KIND, status: "away" });
    const mine = agents.filter((x: any) => x.id !== a.id && x.cwd && path.resolve(x.cwd) === path.resolve(cwd));
    for (const x of mine) lines.push(`- 같은 폴더의 이전 세션 ${x.id} 가 ${x.room} 방에 자리 비움 상태로 남아 있습니다. 이어받으려면 atalk_takeover(from="${x.id}").`);
  }
  emit({ hookSpecificOutput: { hookEventName: "SessionStart", additionalContext: lines.join("\n") } });
}

async function stop(input: HookInput) {
  const id = makeAgentId(KIND, input.session_id);
  const unread = await client.unread(id, false).catch(() => null);
  if (!unread) return;
  const forMe = unread.messages.filter((m: any) => m.for_me);

  // 새로 들어온 초대/신청 (이전에 알린 건 제외)
  const pend = await client.pending(id).catch(() => ({ invites: [], requests: [] }));
  const file = notifiedFile(id);
  let seen: string[] = [];
  try { seen = JSON.parse(fs.readFileSync(file, "utf8")); } catch { /* 처음 */ }
  const fresh = [
    ...pend.invites.map((i: any) => ({ key: `inv:${i.room}:${i.at}`, text: `[초대] ${i.room} "${i.title}" (보낸 이 ${i.by}) → atalk_respond_invite` })),
    ...pend.requests.map((q: any) => ({ key: `req:${q.room}:${q.agent}:${q.at}`, text: `[참여 신청] ${q.agent} → ${q.room} ${q.note ?? ""} → atalk_respond_request` })),
  ].filter((x) => !seen.includes(x.key));

  if (!forMe.length && !fresh.length) return;

  if (forMe.length) await client.ack(id, unread.messages[unread.messages.length - 1].seq);
  if (fresh.length) fs.writeFileSync(file, JSON.stringify([...seen, ...fresh.map((x) => x.key)].slice(-200)));

  const parts = [`[ATalk] ${unread.room ?? ""} 방에서 새 메시지가 왔습니다. 처리하세요.`];
  if (unread.messages.length) {
    const { agents } = await client.agents().catch(() => ({ agents: [] }));
    parts.push(formatMessages(unread.messages, Object.fromEntries(agents.map((x: any) => [x.id, x.name]))));
  }
  if (fresh.length) parts.push(fresh.map((x) => x.text).join("\n"));
  parts.push(`처리 순서: atalk_log(seq, "received") → 작업 → atalk_post(type="result", reply_to=seq) → atalk_log(seq, "done"). 답할 필요가 없으면 atalk_log(seq, "skipped").`);
  emit({ decision: "block", reason: parts.join("\n\n") });
}

async function sessionEnd(input: HookInput) {
  const id = makeAgentId(KIND, input.session_id);
  await client.away(id, `session_end:${input.reason ?? "exit"}`).catch(() => {});
}

const event = process.argv[2];
try {
  const input = await readStdin();
  if (input.session_id) {
    await client.health();
    if (event === "session-start") await sessionStart(input);
    else if (event === "stop") await stop(input);
    else if (event === "session-end") await sessionEnd(input);
  }
} catch {
  // 데몬이 꺼져 있거나 오류가 나도 세션은 계속되어야 한다
}
// process.exit() 를 부르지 않는다: Windows 에서 fetch 소켓 정리 중 exit 하면 libuv assertion 이 난다
process.exitCode = 0;
