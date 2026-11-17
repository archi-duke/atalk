// 훅/MCP 프로세스에서 "에이전트 본체" 프로세스 PID 를 찾는다 (비정상 종료 감지용).
// 훅은 에이전트 → 셸 → node(훅) 형태로 실행되므로, 부모를 거슬러 올라가며 셸/래퍼를 건너뛴다.

import { execFileSync } from "node:child_process";

const WRAPPERS = /^(bash|sh|zsh|dash|fish|cmd|powershell|pwsh|conhost|env|timeout|npx|npm|winpty|wsl|git-bash|mintty)(\.exe)?$/i;

interface Proc { pid: number; ppid: number; name: string }

function listProcesses(): Map<number, Proc> {
  const map = new Map<number, Proc>();
  if (process.platform === "win32") {
    const out = execFileSync("powershell.exe", [
      "-NoProfile", "-NonInteractive", "-Command",
      "Get-CimInstance Win32_Process | ForEach-Object { \"$($_.ProcessId)`t$($_.ParentProcessId)`t$($_.Name)\" }",
    ], { encoding: "utf8", timeout: 8000, windowsHide: true });
    for (const line of out.split(/\r?\n/)) {
      const [pid, ppid, name] = line.split("\t");
      if (pid && ppid) map.set(Number(pid), { pid: Number(pid), ppid: Number(ppid), name: name ?? "" });
    }
  } else {
    const out = execFileSync("ps", ["-A", "-o", "pid=,ppid=,comm="], { encoding: "utf8", timeout: 5000 });
    for (const line of out.split("\n")) {
      const m = /^\s*(\d+)\s+(\d+)\s+(.*)$/.exec(line);
      if (m) map.set(Number(m[1]), { pid: Number(m[1]), ppid: Number(m[2]), name: m[3].split("/").pop() ?? "" });
    }
  }
  return map;
}

/**
 * ATALK_AGENT_PID 가 있으면 그것을, 없으면 부모 체인에서 셸/래퍼가 아닌 첫 프로세스를 돌려준다.
 * 찾지 못하면 null (그러면 비정상 종료 감지는 안 되고 SessionEnd 훅/유예 시간에만 의존).
 */
export function findAgentPid(): number | null {
  const env = Number(process.env.ATALK_AGENT_PID);
  if (Number.isInteger(env) && env > 0) return env;
  try {
    const procs = listProcesses();
    let pid = process.ppid;
    for (let i = 0; i < 12 && pid > 1; i++) {
      const p = procs.get(pid);
      if (!p) return null;
      if (!WRAPPERS.test(p.name)) return p.pid;
      pid = p.ppid;
    }
  } catch {
    // 프로세스 목록을 못 얻으면 포기
  }
  return null;
}
