import fs from "node:fs";
import path from "node:path";

export function ensureDir(dir: string): void {
  fs.mkdirSync(dir, { recursive: true });
}

export function readJson<T>(file: string, fallback: T): T {
  try {
    return JSON.parse(fs.readFileSync(file, "utf8")) as T;
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === "ENOENT") return fallback;
    throw new Error(`JSON 파일을 읽을 수 없음: ${file}: ${(e as Error).message}`);
  }
}

/** 임시 파일에 쓴 뒤 rename 으로 교체 → 쓰는 도중 죽어도 원본이 깨지지 않음 */
export function writeJsonAtomic(file: string, data: unknown): void {
  ensureDir(path.dirname(file));
  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(data, null, 2) + "\n", "utf8");
  // Windows 에서는 다른 프로세스(백신, 뷰어)가 파일을 잡고 있으면 EPERM 이 날 수 있어 몇 번 재시도
  for (let i = 0; ; i++) {
    try {
      fs.renameSync(tmp, file);
      return;
    } catch (e) {
      const code = (e as NodeJS.ErrnoException).code;
      if (i >= 5 || (code !== "EPERM" && code !== "EBUSY" && code !== "EACCES")) throw e;
      const until = Date.now() + 20 * (i + 1);
      while (Date.now() < until) { /* 짧은 동기 대기 */ }
    }
  }
}

export function appendJsonl(file: string, row: unknown): void {
  ensureDir(path.dirname(file));
  fs.appendFileSync(file, JSON.stringify(row) + "\n", "utf8");
}

/** JSONL 읽기. 깨진 줄(쓰는 도중 죽어서 잘린 마지막 줄 등)은 건너뛴다 */
export function readJsonl<T>(file: string): T[] {
  let text: string;
  try {
    text = fs.readFileSync(file, "utf8");
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw e;
  }
  const out: T[] = [];
  for (const line of text.split("\n")) {
    if (!line.trim()) continue;
    try {
      out.push(JSON.parse(line) as T);
    } catch {
      // 손상된 줄은 무시
    }
  }
  return out;
}

/** 마지막 줄이 개행 없이 잘려 있으면 개행을 붙여 다음 append 가 섞이지 않게 한다 */
export function repairJsonlTail(file: string): void {
  let fd: number;
  try {
    fd = fs.openSync(file, "r+");
  } catch {
    return;
  }
  try {
    const size = fs.fstatSync(fd).size;
    if (size === 0) return;
    const buf = Buffer.alloc(1);
    fs.readSync(fd, buf, 0, 1, size - 1);
    if (buf[0] !== 0x0a) fs.writeSync(fd, "\n", size);
  } finally {
    fs.closeSync(fd);
  }
}

const RESERVED = /^(con|prn|aux|nul|com\d|lpt\d)$/i;

/** 방 ID: 유니코드 문자/숫자/-/_ 만, 1~64자, Windows 예약어 금지 */
export function isValidRoomId(id: string): boolean {
  return /^[\p{L}\p{N}_-]{1,64}$/u.test(id) && !RESERVED.test(id);
}

export function isValidKind(kind: string): boolean {
  return /^[a-z0-9][a-z0-9-]{0,31}$/.test(kind);
}

/** 세션 ID 를 파일 이름에 쓸 수 있게 정리 */
export function sanitizeSessionId(sid: string): string {
  const s = sid.replace(/[^\p{L}\p{N}_-]/gu, "-").slice(0, 100);
  return s || "unknown";
}

export function makeAgentId(kind: string, sessionId: string): string {
  return `${kind}_${sanitizeSessionId(sessionId)}`;
}

export function isValidAgentId(id: string): boolean {
  return /^[a-z0-9][a-z0-9-]{0,31}_[\p{L}\p{N}_-]{1,100}$/u.test(id);
}
