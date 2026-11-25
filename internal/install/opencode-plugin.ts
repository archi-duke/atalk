// ATalk opencode 플러그인 — `atalk install opencode` 가 설치한 파일입니다. 직접 고치지 말고 다시 설치하세요.
//
// - 세션을 ATalk 에 opencode_<세션ID> 로 등록 (opencode 프로세스 PID 로 생존 판단)
// - bash 도구 환경에 ATALK_AGENT 를 넣어서 atalk 스킬/CLI 가 신원을 알게 함
// - 나에게 온 메시지가 있고 세션이 쉬고 있으면 세션에 직접 넣고, 답변을 result 로 자동 게시
//   (task/chat 에만 자동 답변 → 에이전트끼리 끝없이 주고받지 않음)
// - 세션 요청은 데몬의 중계(/relay/<agent>/…)를 거친다: 세션이 다른 PC 데몬에 연결돼 있으면(atalk connect) 그쪽으로
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

type State = { id: string; busy: boolean; working: boolean; seen: Set<string>; echoPending?: boolean; introRoom?: string }

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
  if (!res.ok || j.ok === false) {
    const err: any = new Error(j?.error?.message ?? `HTTP ${res.status}`)
    err.code = j?.error?.code
    throw err
  }
  return j
}

// apiAs 는 에이전트 대신 데몬을 부른다. 데몬의 중계(/relay/<agent>/…)를 거치므로, 그 에이전트가 다른 PC 데몬에
// 연결돼 있으면(bash 에서 atalk connect) 이 PC 데몬이 원격 데몬으로 전달하고, 아니면 이 PC 데몬이 처리한다.
let relayMissing = false // 중계가 없는 예전 데몬이면 직접 부른다
async function apiAs(agent: string, method: string, path: string, body?: unknown): Promise<any> {
  if (!relayMissing) {
    try {
      return await api(method, `/relay/${enc(agent)}${path}`, body)
    } catch (e: any) {
      if (e?.code !== "no_route" || !String(e?.message ?? "").includes("/relay/")) throw e
      relayMissing = true
    }
  }
  return api(method, path, body)
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

const SPAWN = process.env.ATALK_SPAWN ?? "" // atalk spawn 으로 연 창이면 연결 표식
let spawnUsed = false

export const AtalkPlugin: Plugin = async ({ client, directory }) => {
  const sessions = new Map<string, State>()
  const pendingTrack = new Set<string>()

  async function track(sid: string, parentID?: string | null) {
    if (!sid || parentID || sessions.has(sid) || pendingTrack.has(sid)) return
    pendingTrack.add(sid)
    try {
      await ensureDaemon()
      // 임시 등록: 사람이 이 세션에 입력하거나 방에 들어갈 때 확정되어 그때 이전 세션의 이름표·자리를 넘겨받는다.
      // (시작하자마자 만든 빈 세션이 이전 세션을 밀어내지 않게. 이전 세션이 다시 쓰이면 쓰지 않은 빈 세션 카드는 정리된다)
      const body: any = { kind: "opencode", session_id: sid, cwd: directory, pid: process.pid, provisional: true }
      if (process.env.ATALK_NAME) body.name = process.env.ATALK_NAME
      if (process.env.ATALK_DESCRIPTION) body.description = process.env.ATALK_DESCRIPTION
      if (process.env.ATALK_CAPABILITIES) body.capabilities = process.env.ATALK_CAPABILITIES.split(",").map((s) => s.trim()).filter(Boolean)
      const reg = await apiAs(agentId(sid), "POST", "/agents", body)
      sessions.set(sid, { id: agentId(sid), busy: false, working: false, seen: new Set() })
      // 새 세션은 같은 폴더의 이전 세션 이름표만 이어받는다 (데몬이 등록할 때 처리). 방에는 입장 명령을 받을 때만 들어간다.
      const inherited = !!reg.inherited_from
      if (reg.previous_room)
        await client.tui.showToast({ body: { message: `ATalk: 이전 세션은 ${reg.previous_room} 방에 있었습니다. 들어가려면 "atalk request ${reg.previous_room}"`, variant: "info" } }).catch(() => {})
      // 처음 등록했고 이어받은 이름표도 없으면 기본 이름 (폴더명-세션ID끝자리). 나중에 바꾼 이름은 덮어쓰지 않는다.
      if (reg.created && !inherited && !process.env.ATALK_NAME) {
        const name = `${directory.split(/[\\/]/).filter(Boolean).pop() ?? "opencode"}-${sid.slice(-5)}`
        await apiAs(agentId(sid), "PATCH", `/agents/${enc(agentId(sid))}`, { name }).catch(() => {})
      }
    } catch {
      // 데몬이 없으면 다음 이벤트 때 다시 시도
    } finally {
      pendingTrack.delete(sid)
    }
  }

  // 사람이 입력한 말(CLI 입력 기록)에 대한 응답을 기록한다 (알림 없음, 기록할 입력이 없으면 데몬이 무시)
  async function echoReply(sid: string, st: State) {
    const r: any = await client.session.messages({ path: { id: sid } })
    const list: any[] = r?.data ?? []
    // 마지막 사용자 입력 이후 응답에 쓴 텍스트를 모두 순서대로 (도구 호출·결과는 뺌)
    const texts: string[] = []
    for (let i = list.length - 1; i >= 0; i--) {
      const m = list[i]
      if (m?.info?.role === "user") break
      if (m?.info?.role !== "assistant") continue
      const text = (m.parts ?? []).filter((p: any) => p.type === "text" && !p.synthetic && p.text).map((p: any) => p.text).join("\n").trim()
      if (text) texts.unshift(text)
    }
    if (texts.length) await apiAs(st.id, "POST", `/agents/${enc(st.id)}/echo`, { texts, reply: true, final: true })
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
    const p = await apiAs(st.id, "GET", `/agents/${enc(st.id)}/pending`)
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
        inRoom = !!(await apiAs(st.id, "GET", `/agents/${enc(st.id)}`)).agent?.room
      } catch {}
    }
    for (const i of p.invites ?? []) {
      const k = `inv:${i.room}:${i.at}`
      if (st.seen.has(k)) continue
      st.seen.add(k)
      if (auto && !inRoom) {
        try {
          await apiAs(st.id, "POST", `/rooms/${enc(i.room)}/invites/respond`, { agent: st.id, accept: true })
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

  // 초대 수락·신청 승인으로 새로 들어간 방에서는 다른 일보다 먼저 인사·자기소개를 한다 (데몬의 intro 표시).
  // 세션이 쉬고 있을 때 한 번 시키고, 답변을 그 방에 chat 으로 올린다 (방에 글을 쓰면 데몬이 표시를 끈다).
  async function introduce(st: State, sid: string): Promise<boolean> {
    const a: any = (await apiAs(st.id, "GET", `/agents/${enc(st.id)}`)).agent
    const room: string = a?.room ?? ""
    if (!a?.intro || !room || st.introRoom === room) return false
    const status: any = await client.session.status()
    const s = status?.data?.[sid]
    if (s && s.type !== "idle") return false // 쉬면 다시 시도
    st.introRoom = room
    let purpose = ""
    try {
      const d = (await apiAs(st.id, "GET", `/rooms/${enc(room)}`)).room?.description
      if (d) purpose = `[${room} 방 목적] ${d}\n`
    } catch {}
    const prompt = `[ATalk] (규칙) ${room} 방에 새로 들어왔습니다.\n${purpose}다른 일보다 먼저 방에 인사하면서 자기소개를 하세요 — 내 이름(${a.name}), 지금 하는 일(작업 폴더·프로젝트), 이 방 목적에 맞게 내가 할 수 있는 일을 3~5줄로.\n마지막 답변이 그대로 방에 인사로 게시되므로 소개 글만 쓰세요. atalk send 를 쓰지 마세요.`
    const r: any = await client.session.prompt({ path: { id: sid }, body: { parts: [{ type: "text", text: prompt }] } })
    const text = (r?.data?.parts ?? []).filter((p: any) => p.type === "text" && !p.synthetic && p.text).map((p: any) => p.text).join("\n").trim()
    if (text && text !== SKIP) await apiAs(st.id, "POST", `/rooms/${enc(room)}/messages`, { agent: st.id, type: "chat", body: text })
    return true
  }

  async function pump(sid: string) {
    const st = sessions.get(sid)
    if (!st || st.working || st.busy) return
    st.working = true
    let work: Msg[] = []
    let room = ""
    try {
      await notifyPending(st)
      try {
        if (await introduce(st, sid)) return // 받은 메시지는 다음 차례에
      } catch {}
      const u = await apiAs(st.id, "GET", `/agents/${enc(st.id)}/unread`)
      const msgs: Msg[] = u.messages ?? []
      if (!msgs.length || !u.room) return
      room = u.room
      const last = msgs[msgs.length - 1].seq
      work = msgs.filter((m) => m.for_me)
      if (!work.length) {
        await apiAs(st.id, "POST", `/agents/${enc(st.id)}/ack`, { seq: last })
        return
      }
      const status: any = await client.session.status()
      const s = status?.data?.[sid]
      if (s && s.type !== "idle") return // 사람이나 다른 작업이 쓰는 중 → 끝나면 session.idle 로 다시 옴

      await apiAs(st.id, "POST", `/agents/${enc(st.id)}/ack`, { seq: last })
      const log = (m: Msg, state: string, note = "") => apiAs(st.id, "POST", `/rooms/${enc(room)}/logs`, { agent: st.id, seq: m.seq, state, note }).catch(() => {})
      for (const m of work) await log(m, "received", "플러그인이 수신")

      const asks = work.filter((m) => m.type === "task" || m.type === "chat")
      const replyTo = asks.length ? asks[asks.length - 1].seq : 0
      const askers = [...new Set(asks.map((m) => m.from))]
      const names: Record<string, string> = {}
      try {
        for (const a of (await apiAs(st.id, "GET", "/agents")).agents ?? []) names[a.id] = a.name
      } catch {}
      let purpose = ""
      try {
        const d = (await apiAs(st.id, "GET", `/rooms/${enc(room)}`)).room?.description
        if (d) purpose = `[${room} 방 목적] ${d}\n`
      } catch {}
      let prompt = `[ATalk] ${room} 방에서 새 메시지가 왔습니다. 나(${st.id})에게 온 메시지를 처리하세요.\n${purpose}\n${format(msgs, names)}\n\n${HUMAN_RULE}\n\n`
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
        const members = new Set(((await apiAs(st.id, "GET", `/rooms/${enc(room)}`)).room?.members ?? []).map((m: any) => m.agent))
        to = askers.filter((a) => members.has(a))
      } catch {}
      const posted = await apiAs(st.id, "POST", `/rooms/${enc(room)}/messages`, { agent: st.id, type: "result", to, reply_to: replyTo, body: reply })
      for (const m of work) await log(m, "done", `결과 #${posted.message?.seq ?? "?"}`)
    } catch (e: any) {
      if (room) for (const m of work) await apiAs(st.id, "POST", `/rooms/${enc(room)}/logs`, { agent: st.id, seq: m.seq, state: "failed", note: String(e?.message ?? e).slice(0, 300) }).catch(() => {})
    } finally {
      st.working = false
    }
  }

  // 일반 TUI 로 실행하면 첫 메시지 전에는 세션이 없어서 ATalk 에 보이지 않는다.
  // 그래서 시작하자마자 세션을 만들어 등록하고 TUI 를 그 세션으로 옮긴다.
  // (-c/--session 으로 이어서 열거나 run/serve 등 다른 명령이면 하지 않는다)
  let foreignActivity = false
  const argv = process.argv.slice(1).map(String)
  // -s/--session ID 나 -c/--continue 로 이전 세션을 이어서 열면, 시작하자마자 그 세션을 등록·확정한다
  // → 그 세션이 있던 대화방으로 바로 복귀 (원칙: 이전 세션으로 복귀하면 그때 있던 방으로)
  const argSession = (() => {
    for (let i = 0; i < argv.length; i++) {
      if ((argv[i] === "-s" || argv[i] === "--session") && argv[i + 1]) return argv[i + 1]
      if (argv[i].startsWith("--session=")) return argv[i].slice("--session=".length)
    }
    return ""
  })()
  const argContinue = argv.some((a) => a === "-c" || a === "--continue" || a.startsWith("--continue"))
  if (argSession || argContinue) {
    setTimeout(async () => {
      for (const wait of [0, 2000, 4000]) {
        await new Promise((res) => setTimeout(res, wait))
        try {
          let sid = argSession
          if (!sid) { // --continue: 이 폴더에서 가장 최근에 쓴 최상위 세션
            const r: any = await client.session.list()
            const list: any[] = (r?.data ?? []).filter((s: any) => !s.parentID)
            list.sort((a: any, b: any) => (b.time?.updated ?? 0) - (a.time?.updated ?? 0))
            sid = list[0]?.id ?? ""
          }
          if (!sid) return
          await track(sid)
          const st = sessions.get(sid)
          if (!st) continue
          await apiAs(st.id, "POST", `/agents/${enc(st.id)}/activate`, {}).catch(() => {})
          return
        } catch {}
      }
    }, 1500)
  }
  // atalk spawn 으로 연 창: 시작하면 세션 하나를 정해(있으면 그것, 없으면 새로) 표식을 쓰고(띄운 에이전트·이름·방),
  // 받은 첫 지시를 그 세션에 직접 보낸 뒤 창(TUI)도 그 세션을 보여 주게 한다. (opencode --prompt 는 입력되지 않을 때가 있음)
  if (SPAWN) {
    // 서버·창이 준비되는 데 몇 초 걸리고, 창(TUI)이 켜지며 세션을 하나 만든다 → 2초마다 다시 시도:
    // 창이 만든 세션이 있으면 그것을, 8초가 지나도 없으면 새로 만들어 쓴다. 표식을 쓰는 데 성공해야 끝난다.
    void (async () => {
      for (let i = 0; i < 20 && !spawnUsed; i++) {
        await new Promise((res) => setTimeout(res, 2000))
        try {
          let sid = [...sessions.keys()][0] ?? ""
          if (!sid && i >= 3) {
            const r: any = await client.session.create({ body: { title: process.env.ATALK_NAME ?? "ATalk" } })
            sid = r?.data?.id ?? ""
            if (sid) await track(sid)
          }
          const st = sid ? sessions.get(sid) : undefined
          if (!st) continue
          const res = await apiAs(st.id, "POST", `/agents/${enc(st.id)}/spawn`, { token: SPAWN })
          spawnUsed = true
          await client.tui.publish({ body: { type: "tui.session.select", properties: { sessionID: sid } } as any }).catch(() => {})
          if (res?.prompt) await client.session.prompt({ path: { id: sid }, body: { parts: [{ type: "text", text: res.prompt }] } })
        } catch (e: any) {
          if (e?.code === "spawn_expired" || String(e?.message ?? e).includes("spawn_expired")) return // 표식이 없거나 지남 — 다시 해도 소용없음
        }
      }
    })()
  }
  const skipEager = ["run", "serve", "web", "attach", "acp", "mcp", "export", "import", "-c", "--continue", "-s", "--session", "--prompt"]
  if (!SPAWN && !argv.some((a) => skipEager.includes(a) || a.startsWith("--session=") || a.startsWith("--continue"))) {
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
    const me = (await apiAs(agent, "GET", `/agents/${enc(agent)}`)).agent
    if (!me?.room) throw new Error("대화방에 들어가 있지 않습니다 (/atalk join ROOM)")
    const room = me.room
    const names: Record<string, string> = {}
    for (const a of (await apiAs(agent, "GET", "/agents")).agents ?? []) names[a.id] = a.name
    const msg = (await apiAs(agent, "POST", `/rooms/${enc(room)}/say`, { agent, text })).message
    return `[ATalk] ${room} #${msg.seq} → ${msg.to?.length ? msg.to.map((id: string) => names[id] ?? id).join(", ") : "방 전체"}\n${msg.human}@${names[agent] ?? agent}: ${msg.body}`
  }

  // TUI 플러그인(atalk-tui.ts)이 이 프로세스에서 이전 세션으로 복귀시킨 카드를 찾아 메시지 전달 대상에 넣는다
  async function adoptResumed() {
    try {
      const list: any[] = (await api("GET", "/agents?kind=opencode")).agents ?? []
      for (const a of list) {
        if (a.pid === process.pid && !a.provisional && a.session_id && !sessions.has(a.session_id)) await track(a.session_id)
      }
    } catch {}
  }

  const timer = setInterval(() => {
    for (const sid of sessions.keys()) void pump(sid)
    void adoptResumed()
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
            if (st) {
              const busy = p.status?.type !== "idle"
              if (busy !== st.busy) apiAs(st.id, "POST", `/agents/${enc(st.id)}/activity`, { busy }).catch(() => {}) // 작업 중 표시
              st.busy = busy
            } else await trackById(p.sessionID)
            break
          }
          case "session.idle": {
            const st = sessions.get(p.sessionID)
            if (st) {
              if (st.busy) apiAs(st.id, "POST", `/agents/${enc(st.id)}/activity`, { busy: false }).catch(() => {})
              st.busy = false
              if (st.echoPending) {
                st.echoPending = false
                await echoReply(p.sessionID, st).catch(() => {})
              }
              void pump(p.sessionID)
            } else await trackById(p.sessionID)
            break
          }
          // 사람의 확인이 필요함 (도구 허가·질문): 대화방에 알리고 "확인 필요" 표시. 답하면 풀림
          case "permission.asked": {
            const st = sessions.get(p.sessionID)
            if (st) {
              const what = Array.isArray(p.patterns) && p.patterns.length ? ": " + p.patterns.join(", ") : ""
              apiAs(st.id, "POST", `/agents/${enc(st.id)}/need-input`, { text: `${p.permission ?? "도구"} 실행 허가${what}` }).catch(() => {})
            }
            break
          }
          case "question.asked": {
            const st = sessions.get(p.sessionID)
            if (st) {
              const text = (p.questions ?? []).map((q: any) => (q.question ?? "") + (q.options?.length ? " (" + q.options.map((o: any) => o.label).join(" / ") + ")" : "")).join("\n")
              if (text.trim()) apiAs(st.id, "POST", `/agents/${enc(st.id)}/need-input`, { text }).catch(() => {})
            }
            break
          }
          case "permission.replied":
          case "question.replied":
          case "question.rejected": {
            const st = sessions.get(p.sessionID)
            if (st) apiAs(st.id, "POST", `/agents/${enc(st.id)}/need-input`, { text: "" }).catch(() => {})
            break
          }
          case "session.deleted": {
            const st = sessions.get(p.info?.id)
            if (st) {
              sessions.delete(p.info.id)
              await apiAs(st.id, "POST", `/agents/${enc(st.id)}/away`, { reason: "session_deleted" }).catch(() => {})
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
      if (!tp) return
      if (raw && !raw.startsWith("[ATalk]")) {
        // 사람이 입력함 → 이 세션 등록(이전 세션이면 그때 있던 방으로 복귀) + 임시 세션 확정
        await trackById(input.sessionID)
        const st0 = sessions.get(input.sessionID)
        if (st0) await apiAs(st0.id, "POST", `/agents/${enc(st0.id)}/activate`, {}).catch(() => {})
      }
      if (!raw.startsWith(HUMAN_PREFIX)) {
        // 사람이 입력한 말을 방에 대화 기록으로 남긴다 (알림 없음). 플러그인이 넣은 "[ATalk]" 메시지·/명령은 빼고.
        if (raw && !raw.startsWith("[ATalk]") && !raw.startsWith("/")) {
          const st = sessions.get(input.sessionID)
          if (st) {
            st.echoPending = true // 응답이 끝나면(session.idle) 그 응답도 기록
            apiAs(st.id, "POST", `/agents/${enc(st.id)}/echo`, { text: raw }).catch(() => {})
          }
        }
        return
      }
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
      for (const st of sessions.values()) await apiAs(st.id, "POST", `/agents/${enc(st.id)}/away`, { reason: "opencode_exit" }).catch(() => {})
    },
  }
}
