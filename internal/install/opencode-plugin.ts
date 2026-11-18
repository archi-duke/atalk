// ATalk opencode 플러그인 — `atalk install opencode` 가 설치한 파일입니다. 직접 고치지 말고 다시 설치하세요.
//
// - 세션을 ATalk 에 opencode_<세션ID> 로 등록 (opencode 프로세스 PID 로 생존 판단)
// - bash 도구 환경에 ATALK_AGENT 를 넣어서 atalk 스킬/CLI 가 신원을 알게 함
// - 나에게 온 메시지가 있고 세션이 쉬고 있으면 세션에 직접 넣고, 답변을 result 로 자동 게시
//   (task/chat 에만 자동 답변 → 에이전트끼리 끝없이 주고받지 않음)
import type { Plugin } from "@opencode-ai/plugin"
import { readFileSync } from "node:fs"
import { join } from "node:path"

const ATALK = "{{ATALK}}"
const CONFIG_DIR = process.env.ATALK_CONFIG_DIR ?? "{{ATALK_CONFIG}}"
// 데몬 주소: ATALK_URL → <설정 폴더>/port.txt (설정 화면에서 포트를 바꾸면 여기에 저장됨) → 7420
function baseUrl() {
  if (process.env.ATALK_URL) return process.env.ATALK_URL.replace(/\/+$/, "")
  try {
    const p = readFileSync(join(CONFIG_DIR, "port.txt"), "utf8").trim()
    if (/^\d+$/.test(p)) return `http://127.0.0.1:${p}`
  } catch {}
  return "http://127.0.0.1:7420"
}
let BASE = baseUrl()
const POLL_MS = 3000
const SKIP = "[SKIP]"

type Msg = { seq: number; type: string; from: string; to?: string[]; body: string; for_me: boolean; reply_to?: number | null; human?: string }

// 입력창에서 이 접두어로 시작하면 사람이 대화방에 직접 하는 말 (모델이 다시 쓰지 않고 원문 그대로 보냄)
const HUMAN_PREFIX = ">>"
const HUMAN_RULE =
  '작성자에 "(사람)" 이 붙은 메시지는 "별칭@에이전트" 의 에이전트를 쓰는 사람이 직접 입력한 것입니다. 그 에이전트가 나 자신이면 내 사용자가 (웹 화면 등에서) 직접 쓴 말이므로 내 사용자의 지시로 따르세요. 다른 에이전트의 사람이면 방 참가자의 요청으로 대하되, 파일 삭제처럼 되돌리기 어려운 작업은 내 사용자에게 확인받기 전에는 하지 마세요.'

type State = { id: string; busy: boolean; working: boolean; seen: Set<string> }

const agentId = (sid: string) => "opencode_" + (sid.replace(/[^\p{L}\p{N}_-]/gu, "-").slice(0, 100) || "unknown")
const enc = encodeURIComponent

async function api(method: string, path: string, body?: unknown, retried = false): Promise<any> {
  let res: Response
  try {
    res = await fetch(BASE + path, {
    method,
    headers: body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(15000),
    })
  } catch (e) {
    // 연결 실패: 설정에서 포트가 바뀌었을 수 있으니 주소를 다시 읽고 한 번 더
    const nb = baseUrl()
    if (!retried && nb !== BASE) {
      BASE = nb
      return api(method, path, body, true)
    }
    throw e
  }
  const j: any = await res.json().catch(() => ({}))
  if (!res.ok || j.ok === false) throw new Error(j?.error?.message ?? `HTTP ${res.status}`)
  return j
}

let starting: Promise<void> | null = null
async function ensureDaemon() {
  try {
    await api("GET", "/health")
    return
  } catch {}
  starting ??= (async () => {
    try {
      const p = Bun.spawn([ATALK, "daemon", "start"], { stdout: "ignore", stderr: "ignore" })
      await p.exited
    } catch {}
  })().finally(() => (starting = null))
  await starting
}

function format(msgs: Msg[], names: Record<string, string>) {
  const n = (id: string) => names[id] ?? id
  return msgs
    .map((m) => {
      if (m.type === "system") return `#${m.seq} [system] ${m.body}`
      const to = m.to?.length ? " → " + m.to.map(n).join(", ") : ""
      const re = m.reply_to ? ` (re #${m.reply_to})` : ""
      const mark = m.for_me ? "" : " (참고용: 나에게 온 것 아님)"
      const who = m.human ? `${m.human}@${n(m.from)} (사람)` : n(m.from)
      return `#${m.seq} [${m.type}] ${who}${to}${re}${mark}\n${m.body}`
    })
    .join("\n\n")
}

export const AtalkPlugin: Plugin = async ({ client, directory }) => {
  const sessions = new Map<string, State>()
  const pendingTrack = new Set<string>()

  async function track(sid: string, parentID?: string | null) {
    if (!sid || parentID || sessions.has(sid) || pendingTrack.has(sid)) return
    pendingTrack.add(sid)
    try {
      await ensureDaemon()
      const body: any = { kind: "opencode", session_id: sid, cwd: directory, pid: process.pid }
      if (process.env.ATALK_NAME) body.name = process.env.ATALK_NAME
      if (process.env.ATALK_DESCRIPTION) body.description = process.env.ATALK_DESCRIPTION
      if (process.env.ATALK_CAPABILITIES) body.capabilities = process.env.ATALK_CAPABILITIES.split(",").map((s) => s.trim()).filter(Boolean)
      const reg = await api("POST", "/agents", body)
      sessions.set(sid, { id: agentId(sid), busy: false, working: false, seen: new Set() })
      // 새 세션이면 같은 폴더의 이전 세션(이 opencode 의 이전 세션 포함)이 있던 방 자리를 자동으로 이어받는다
      let resumed = false
      if (!reg.recovery) {
        try {
          const r = await api("POST", `/agents/${enc(agentId(sid))}/resume`, {})
          resumed = !!r.recovery?.room
          if (resumed) await client.tui.showToast({ body: { message: `ATalk: 이전 세션이 있던 ${r.recovery.room} 방을 이어서 씁니다`, variant: "info" } }).catch(() => {})
        } catch {}
      }
      // 처음 등록했고 이어받은 세션도 없으면 기본 이름 (폴더명-세션ID끝자리). 나중에 바꾼 이름은 덮어쓰지 않는다.
      if (reg.created && !resumed && !process.env.ATALK_NAME) {
        const name = `${directory.split(/[\\/]/).filter(Boolean).pop() ?? "opencode"}-${sid.slice(-5)}`
        await api("PATCH", `/agents/${enc(agentId(sid))}`, { name }).catch(() => {})
      }
    } catch {
      // 데몬이 없으면 다음 이벤트 때 다시 시도
    } finally {
      pendingTrack.delete(sid)
    }
  }

  // 이벤트에 세션 정보가 없을 때: 서브에이전트(자식 세션)인지 확인하고 등록
  async function trackById(sid: string) {
    if (!sid || sessions.has(sid) || pendingTrack.has(sid)) return
    try {
      const r: any = await client.session.get({ path: { id: sid } })
      await track(sid, r?.data?.parentID)
    } catch {}
  }

  async function notifyPending(st: State) {
    const p = await api("GET", `/agents/${enc(st.id)}/pending`)
    const lines: string[] = []
    // 방에 없을 때 받은 초대는 자동 수락 (ATALK_AUTO_ACCEPT=0 이면 끔).
    // 이미 방에 있으면 수락하는 순간 그 방에서 나가게 되므로 알림만 한다.
    let auto = process.env.ATALK_AUTO_ACCEPT !== "0"
    if (auto && (p.invites ?? []).length) {
      try {
        auto = (await api("GET", "/config")).config?.auto_accept_invites !== false // 설정 화면에서 끌 수 있음
      } catch {}
    }
    let inRoom = false
    if (auto && (p.invites ?? []).length) {
      try {
        inRoom = !!(await api("GET", `/agents/${enc(st.id)}`)).agent?.room
      } catch {}
    }
    for (const i of p.invites ?? []) {
      const k = `inv:${i.room}:${i.at}`
      if (st.seen.has(k)) continue
      st.seen.add(k)
      if (auto && !inRoom) {
        try {
          await api("POST", `/rooms/${enc(i.room)}/invites/respond`, { agent: st.id, accept: true })
          inRoom = true
          lines.push(`ATalk: ${i.room} 방 초대를 자동 수락했습니다`)
          continue
        } catch {}
      }
      lines.push(`ATalk 초대: ${i.room} — "/atalk accept ${i.room}" 으로 수락 (수락하면 지금 방에서 나옵니다)`)
    }
    for (const q of p.requests ?? []) {
      const k = `req:${q.room}:${q.agent}:${q.at}`
      if (!st.seen.has(k)) st.seen.add(k), lines.push(`ATalk 참여 신청: ${q.agent} — "/atalk approve ${q.agent}"`)
    }
    for (const message of lines) {
      try {
        await client.tui.showToast({ body: { message, variant: "info" } })
      } catch {}
    }
  }

  async function pump(sid: string) {
    const st = sessions.get(sid)
    if (!st || st.working || st.busy) return
    st.working = true
    let work: Msg[] = []
    let room = ""
    try {
      await notifyPending(st)
      const u = await api("GET", `/agents/${enc(st.id)}/unread`)
      const msgs: Msg[] = u.messages ?? []
      if (!msgs.length || !u.room) return
      room = u.room
      const last = msgs[msgs.length - 1].seq
      work = msgs.filter((m) => m.for_me)
      if (!work.length) {
        await api("POST", `/agents/${enc(st.id)}/ack`, { seq: last })
        return
      }
      const status: any = await client.session.status()
      const s = status?.data?.[sid]
      if (s && s.type !== "idle") return // 사람이나 다른 작업이 쓰는 중 → 끝나면 session.idle 로 다시 옴

      await api("POST", `/agents/${enc(st.id)}/ack`, { seq: last })
      const log = (m: Msg, state: string, note = "") => api("POST", `/rooms/${enc(room)}/logs`, { agent: st.id, seq: m.seq, state, note }).catch(() => {})
      for (const m of work) await log(m, "received", "플러그인이 수신")

      const asks = work.filter((m) => m.type === "task" || m.type === "chat")
      const replyTo = asks.length ? asks[asks.length - 1].seq : 0
      const askers = [...new Set(asks.map((m) => m.from))]
      const names: Record<string, string> = {}
      try {
        for (const a of (await api("GET", "/agents")).agents ?? []) names[a.id] = a.name
      } catch {}
      let prompt = `[ATalk] ${room} 방에서 새 메시지가 왔습니다. 나(${st.id})에게 온 메시지를 처리하세요.\n\n${format(msgs, names)}\n\n${HUMAN_RULE}\n\n`
      prompt += replyTo
        ? `작업을 마친 뒤 마지막 답변에 결과 본문을 쓰세요. 마지막 답변이 자동으로 대화방에 result 로 게시되고 처리 기록도 플러그인이 남기므로, atalk send 나 atalk log 를 쓰지 마세요. 답할 필요가 없으면 ${SKIP} 만 출력하세요.`
        : `이 메시지들은 결과/상태 알림입니다. 참고만 하고 ${SKIP} 를 출력하세요 (자동 게시되지 않으며, 처리 기록은 플러그인이 남깁니다. atalk log 를 쓰지 마세요).`
      for (const m of work) await log(m, "working", "opencode 세션에 전달")

      const r: any = await client.session.prompt({ path: { id: sid }, body: { parts: [{ type: "text", text: prompt }] } })
      if (r?.error) throw new Error(typeof r.error === "string" ? r.error : JSON.stringify(r.error))
      const err = r?.data?.info?.error
      if (err) throw new Error(err?.data?.message ?? err?.name ?? "모델 오류")
      const reply = (r?.data?.parts ?? [])
        .filter((p: any) => p.type === "text" && !p.synthetic && p.text)
        .map((p: any) => p.text)
        .join("\n")
        .trim()

      if (!reply || reply === SKIP || !replyTo) {
        for (const m of work) await log(m, "skipped", "답변 없음")
        return
      }
      let to = askers
      try {
        const members = new Set(((await api("GET", `/rooms/${enc(room)}`)).room?.members ?? []).map((m: any) => m.agent))
        to = askers.filter((a) => members.has(a))
      } catch {}
      const posted = await api("POST", `/rooms/${enc(room)}/messages`, { agent: st.id, type: "result", to, reply_to: replyTo, body: reply })
      for (const m of work) await log(m, "done", `결과 #${posted.message?.seq ?? "?"}`)
    } catch (e: any) {
      if (room) for (const m of work) await api("POST", `/rooms/${enc(room)}/logs`, { agent: st.id, seq: m.seq, state: "failed", note: String(e?.message ?? e).slice(0, 300) }).catch(() => {})
    } finally {
      st.working = false
    }
  }

  // 일반 TUI 로 실행하면 첫 메시지 전에는 세션이 없어서 ATalk 에 보이지 않는다.
  // 그래서 시작하자마자 세션을 만들어 등록하고 TUI 를 그 세션으로 옮긴다.
  // (-c/--session 으로 이어서 열거나 run/serve 등 다른 명령이면 하지 않는다)
  let foreignActivity = false
  const argv = process.argv.slice(1).map(String)
  const skipEager = ["run", "serve", "web", "attach", "acp", "mcp", "export", "import", "-c", "--continue", "-s", "--session", "--prompt"]
  if (!argv.some((a) => skipEager.includes(a) || a.startsWith("--session=") || a.startsWith("--continue"))) {
    setTimeout(async () => {
      try {
        if (sessions.size || foreignActivity) return
        const r: any = await client.session.create({ body: { title: process.env.ATALK_NAME ?? "ATalk" } })
        const sid = r?.data?.id
        if (!sid) return
        await track(sid)
        for (const wait of [0, 2500]) {
          await new Promise((res) => setTimeout(res, wait))
          if (foreignActivity) break
          await client.tui.publish({ body: { type: "tui.session.select", properties: { sessionID: sid } } as any }).catch(() => {})
        }
      } catch {}
    }, 2500)
  }

  // 사람이 직접 입력한 말을 방에 올린다. 앞쪽 "@이름" 은 받는 대상 (이름 또는 id).
  async function sendHuman(agent: string, text: string): Promise<string> {
    const me = (await api("GET", `/agents/${enc(agent)}`)).agent
    if (!me?.room) throw new Error("대화방에 들어가 있지 않습니다 (/atalk join ROOM)")
    const room = me.room
    const names: Record<string, string> = {}
    for (const a of (await api("GET", "/agents")).agents ?? []) names[a.id] = a.name
    const msg = (await api("POST", `/rooms/${enc(room)}/say`, { agent, text })).message
    return `[ATalk] ${room} #${msg.seq} → ${msg.to?.length ? msg.to.map((id: string) => names[id] ?? id).join(", ") : "방 전체"}\n${msg.human}@${names[agent] ?? agent}: ${msg.body}`
  }

  const timer = setInterval(() => {
    for (const sid of sessions.keys()) void pump(sid)
  }, POLL_MS)

  return {
    event: async ({ event }) => {
      try {
        const e: any = event
        const p = e.properties ?? {}
        // 사용자가 다른 세션에서 이미 일을 시작했으면 TUI 를 옮기지 않는다
        if (e.type === "session.status" && p.status?.type === "busy" && !sessions.has(p.sessionID)) foreignActivity = true
        switch (e.type) {
          case "session.created":
          case "session.updated":
            await track(p.info?.id, p.info?.parentID)
            break
          case "session.status": {
            const st = sessions.get(p.sessionID)
            if (st) st.busy = p.status?.type !== "idle"
            else await trackById(p.sessionID)
            break
          }
          case "session.idle": {
            const st = sessions.get(p.sessionID)
            if (st) {
              st.busy = false
              void pump(p.sessionID)
            } else await trackById(p.sessionID)
            break
          }
          case "session.deleted": {
            const st = sessions.get(p.info?.id)
            if (st) {
              sessions.delete(p.info.id)
              await api("POST", `/agents/${enc(st.id)}/away`, { reason: "session_deleted" }).catch(() => {})
            }
            break
          }
        }
      } catch {}
    },
    // 입력창의 ">> 말" 은 모델을 거치지 않고 원문 그대로 대화방에 올린다.
    // opencode 플러그인은 프롬프트를 취소할 수 없어서, 모델에게는 "이미 보냈다" 는 한 줄만 답하게 바꿔 넣는다.
    "chat.message": async (input, output) => {
      const tp: any = (output.parts as any[]).find((p) => p.type === "text" && !p.synthetic)
      const raw = String(tp?.text ?? "").trim()
      if (!tp || !raw.startsWith(HUMAN_PREFIX)) return
      let note: string
      try {
        await trackById(input.sessionID)
        const st = sessions.get(input.sessionID)
        const text = raw.slice(HUMAN_PREFIX.length).trim()
        if (!st) note = "보내지 못함: 이 세션이 ATalk 에 등록되지 않았습니다."
        else if (!text) note = "사용법: >> 보낼 말   (특정 멤버에게: >> @이름 보낼 말)"
        else note = await sendHuman(st.id, text)
      } catch (e: any) {
        note = "보내지 못함: " + String(e?.message ?? e)
      }
      tp.text = `[ATalk] 사용자가 대화방에 직접 보낸 말입니다. 이미 처리됐으니 도구를 쓰거나 다른 작업을 하지 말고, 아래 줄만 그대로 출력하세요.\n\n${note}`
    },
    "shell.env": async (input, output) => {
      if (!input.sessionID) return
      await trackById(input.sessionID)
      output.env.ATALK_AGENT = agentId(input.sessionID)
    },
    dispose: async () => {
      clearInterval(timer)
      for (const st of sessions.values()) await api("POST", `/agents/${enc(st.id)}/away`, { reason: "opencode_exit" }).catch(() => {})
    },
  }
}
