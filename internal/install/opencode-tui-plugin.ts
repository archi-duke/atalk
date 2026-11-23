// ATalk opencode TUI 플러그인 — `atalk install opencode` 가 설치한 파일입니다. 직접 고치지 말고 다시 설치하세요.
//
// 원칙: 이전 대화방 세션으로 복귀하면 그때 있던 방으로 들어간다.
// opencode 는 TUI 에서 세션을 고르기만 해서는 서버 플러그인에 알려 주지 않는다. 그래서 TUI 에서 지금 열린 세션을 보고,
// 그 세션이 예전에 ATalk 에 등록됐던(확정된) 세션이면 바로 다시 등록해 그 방으로 복귀시킨다.
// 처음 보는 세션이나 쓰지 않은 임시 세션은 건드리지 않는다 (켜자마자 만든 빈 세션이 이전 세션을 밀어내지 않게).
import { readFileSync } from "node:fs"
import { join } from "node:path"

const CONFIG_DIR = process.env.ATALK_CONFIG_DIR ?? "{{ATALK_CONFIG}}"
function baseUrl() {
  if (process.env.ATALK_URL) return process.env.ATALK_URL.replace(/\/+$/, "")
  try {
    const p = readFileSync(join(CONFIG_DIR, "port.txt"), "utf8").trim()
    if (/^\d+$/.test(p)) return `http://127.0.0.1:${p}`
  } catch {}
  return "http://127.0.0.1:7420"
}
const agentId = (sid: string) => "opencode_" + (sid.replace(/[^\p{L}\p{N}_-]/gu, "-").slice(0, 100) || "unknown")
const enc = encodeURIComponent

async function api(method: string, path: string, body?: unknown): Promise<any> {
  const res = await fetch(baseUrl() + path, {
    method,
    headers: body ? { "content-type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  })
  return res.json()
}

export default {
  id: "atalk-tui",
  tui: async (tuiApi: any) => {
    let last = ""
    const check = async () => {
      try {
        const r = tuiApi?.route?.current
        const sid: string = r?.name === "session" ? String(r?.params?.sessionID ?? "") : ""
        if (!sid || sid === last) return
        last = sid
        const card = (await api("GET", `/agents/${enc(agentId(sid))}`))?.agent
        if (!card || card.provisional) return // 처음 보는 세션·쓰지 않은 임시 세션은 그대로 둔다
        if (card.room && card.status === "in_room") return // 이미 방에 있음
        // 예전에 쓰던 세션으로 복귀 → 다시 등록 (데몬이 그 방 자리·그때 있던 방으로 돌려보낸다)
        const reg = await api("POST", "/agents", { kind: "opencode", session_id: sid, cwd: process.cwd(), pid: process.pid })
        const room = reg?.agent?.room
        if (room) tuiApi?.ui?.toast?.({ message: `ATalk: ${room} 방으로 복귀했습니다`, variant: "info" })
      } catch {}
    }
    setInterval(check, 1000)
  },
}
