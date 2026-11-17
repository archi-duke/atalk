// ATalk opencode 플러그인 — `atalk install opencode` 가 설치한 파일입니다. 직접 고치지 말고 다시 설치하세요.
//
// - 세션을 ATalk 에 opencode_<세션ID> 로 등록 (opencode 프로세스 PID 로 생존 판단)
// - bash 도구 환경에 ATALK_AGENT 를 넣어서 atalk 스킬/CLI 가 신원을 알게 함
// - 나에게 온 메시지가 있고 세션이 쉬고 있으면 세션에 직접 넣고, 답변을 result 로 자동 게시
//   (task/chat 에만 자동 답변 → 에이전트끼리 끝없이 주고받지 않음)
import type { Plugin } from "@opencode-ai/plugin"

const ATALK = "{{ATALK}}"
const BASE = (process.env.ATALK_URL ?? "http://127.0.0.1:7420").replace(/\/+$/, "")
const POLL_MS = 3000
const SKIP = "[SKIP]"

type Msg = { seq: number; type: string; from: string; to?: string[]; body: string; for_me: boolean; reply_to?: number | null }
type State = { id: string; busy: boolean; working: boolean; seen: Set<string> }

const agentId = (sid: string) => "opencode_" + (sid.replace(/[^\p{L}\p{N}_-]/gu, "-").slice(0, 100) || "unknown")
const enc = encodeURIComponent

async function api(method: string, path: string, body?: unknown): Promise<any> {
  const res = await fetch(BASE + path, {
    method,
    headers: body === undefined ? undefined : { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(15000),
  })
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
      return `#${m.seq} [${m.type}] ${n(m.from)}${to}${re}${mark}\n${m.body}`
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
      await api("POST", "/agents", body)
      sessions.set(sid, { id: agentId(sid), busy: false, working: false, seen: new Set() })
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
    for (const i of p.invites ?? []) {
      const k = `inv:${i.room}:${i.at}`
      if (!st.seen.has(k)) st.seen.add(k), lines.push(`ATalk 초대: ${i.room} — "/atalk accept ${i.room}" 으로 수락`)
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
      let prompt = `[ATalk] ${room} 방에서 새 메시지가 왔습니다. 나(${st.id})에게 온 메시지를 처리하세요.\n\n${format(msgs, names)}\n\n`
      prompt += replyTo
        ? `작업을 마친 뒤 마지막 답변에 결과 본문을 쓰세요. 마지막 답변이 자동으로 대화방에 result 로 게시되므로 atalk send 로 따로 보내지 마세요. 답할 필요가 없으면 ${SKIP} 만 출력하세요.`
        : `이 메시지들은 결과/상태 알림입니다. 참고만 하고 ${SKIP} 를 출력하세요 (자동 게시되지 않습니다).`
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

  const timer = setInterval(() => {
    for (const sid of sessions.keys()) void pump(sid)
  }, POLL_MS)

  return {
    event: async ({ event }) => {
      try {
        const e: any = event
        const p = e.properties ?? {}
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
