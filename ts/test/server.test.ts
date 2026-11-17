import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createServer } from "../src/server.ts";
import { AtalkClient } from "../src/client.ts";

test("HTTP: 등록 → 방 → 초대 → wait 가 메시지 도착 시 깨어남 → 처리 기록", async () => {
  // ATALK_TEST_URL 이 있으면 외부 데몬(예: Go 구현)을 대상으로 같은 시나리오를 돌린다 (API 호환성 검사)
  const external = process.env.ATALK_TEST_URL;
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "atalk-http-"));
  const srv = external ? null : createServer({ root, port: 0 });
  const base = external ?? `http://127.0.0.1:${await srv!.listen()}`;
  const c = new AtalkClient(base);
  try {
    const a = (await c.register({ kind: "claude-code", session_id: "s1", name: "Lead" })).agent.id;
    const b = (await c.register({ kind: "codex", session_id: "s2", name: "Reviewer" })).agent.id;
    await c.createRoom(a, "한글방", "테스트");
    await c.invite(a, "한글방", b);

    // b 는 초대 대기 중 → wait 은 invite 이벤트로 깨어남
    const pending = (await c.pending(b)).invites;
    assert.equal(pending[0].room, "한글방");
    await c.respondInvite(b, "한글방", true);

    const waiting = c.wait(b, 10);
    await new Promise((r) => setTimeout(r, 100));
    await c.post(a, "한글방", { type: "task", to: [b], body: "PR 리뷰" });
    const w = await waiting;
    assert.equal(w.reason, "message");
    const task = w.messages.find((m: any) => m.for_me);
    assert.equal(task.body, "PR 리뷰");

    await c.log(b, "한글방", task.seq, "received");
    await c.post(b, "한글방", { type: "result", reply_to: task.seq, body: "LGTM" });
    await c.log(b, "한글방", task.seq, "done", "리뷰 완료");
    const logs = (await c.logs("한글방")).logs;
    assert.deepEqual(logs.map((l: any) => l.state), ["received", "done"]);

    // wait 타임아웃
    const t = await c.wait(b, 1);
    assert.equal(t.reason, "timeout");

    // 오류 형식
    await assert.rejects(c.post(b, "없는방", { body: "x" }), /없는 대화방/);

    // 뷰어
    const html = await fetch(`${base}/`).then((r) => r.text());
    assert.match(html, /ATalk/);
  } finally {
    await srv?.close();
  }
});
