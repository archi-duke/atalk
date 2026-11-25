package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTurnTexts(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := `{"type":"user","message":{"role":"user","content":"파일 몇 개야?"}}`
	meta := `{"type":"user","isMeta":true,"message":{"role":"user","content":"<system-reminder>"}}`
	toolUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`
	toolRes := `{"type":"user","message":{"content":[{"type":"tool_result","content":"42"}]}}`
	interim := `{"type":"assistant","message":{"content":[{"type":"text","text":"세어 보겠습니다."}]}}`
	final := `{"type":"assistant","message":{"content":[{"type":"text","text":"42개입니다."}]}}`

	notice := `{"type":"user","message":{"content":[{"type":"text","text":"<task-notification>\n<task-id>x</task-id>\n</task-notification>"}]}}`
	texts, prompt := turnTexts(write(final, user, interim, toolUse, toolRes, notice, final, meta))
	if prompt != "파일 몇 개야?" {
		t.Fatalf("이번 턴을 시작한 입력: %q", prompt)
	}
	got := strings.Join(texts, "|")
	if got != "세어 보겠습니다.|42개입니다." {
		t.Fatalf("이번 턴의 응답 문장 전부(이전 턴 제외, 중간 알림은 경계 아님): %q", got)
	}
	// 마지막 입력 뒤에 응답 텍스트가 없으면 이전 턴 답을 가져오면 안 됨
	// 작업 도중 보낸 말(queued_command 첨부)도 경계: 그 뒤 문장만, prompt 는 그 말
	queued := `{"type":"attachment","attachment":{"type":"queued_command","prompt":"빠지는 메시지가 있네"}}`
	if got, p := turnTexts(write(user, interim, toolUse, toolRes, queued, final)); p != "빠지는 메시지가 있네" || strings.Join(got, "|") != "42개입니다." {
		t.Fatalf("끼어든 말 경계: %q %q", got, p)
	}
	if got, _ := turnTexts(write(user, final, user, toolUse, toolRes)); len(got) != 0 {
		t.Fatalf("이전 턴 답을 가져옴: %q", got)
	}
	// ATalk 알림으로 깨어난 턴: 알림이 경계, wake = 알림의 메시지 번호 (대기 훅 알림·응답 끝 훅 알림(isMeta) 모두)
	wakeN := `{"type":"user","message":{"role":"user","content":"<task-notification>\n</task-notification>\n[ATalk] r 방에서 나에게 온 메시지 1개:\n#278 [chat] 사용자@마스터 (사람) → 마스터\n반영해봐"}}`
	if got, p, w := turnInfo(write(user, final, wakeN, interim, toolUse, toolRes, final)); p != "" || len(w) != 1 || w[0] != 278 || strings.Join(got, "|") != "세어 보겠습니다.|42개입니다." {
		t.Fatalf("알림 턴: %q %q %v", got, p, w)
	}
	stopN := `{"type":"user","isMeta":true,"message":{"role":"user","content":"Stop hook feedback:\n[ATalk] r 방에서 새 메시지가 왔습니다.\n\n#229 [result] 맥 → 마스터 (re #228)\n네\n\n#231 [result] 코덱스 → 마스터\n네"}}`
	if got, _, w := turnInfo(write(user, interim, stopN, final)); len(w) != 2 || w[1] != 231 || strings.Join(got, "|") != "42개입니다." {
		t.Fatalf("응답 끝 훅 알림 턴: %q %v", got, w)
	}
	if _, p, w := turnInfo(write(wakeN, final, user, final)); p != "파일 몇 개야?" || w != nil {
		t.Fatalf("사람 입력 턴은 wake 없음: %q %v", p, w)
	}
	if a, _ := turnTexts(""); len(a) != 0 {
		t.Fatal("경로 없음")
	}
	if a, _ := turnTexts(filepath.Join(dir, "없음.jsonl")); len(a) != 0 {
		t.Fatal("경로 없음")
	}
}

// Codex 대화 기록(rollout): 마지막 입력 이후의 응답 문장, ATalk 알림 입력은 wake.
func TestCodexTurn(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "r.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	meta := `{"type":"session_meta","payload":{"session_id":"x"}}`
	user := func(s string) string {
		return `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"type":"text","text":"` + s + `"}]}}}`
	}
	sys := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions"}]}}`
	say := func(s string) string {
		return `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + s + `"}]}}`
	}
	tool := `{"type":"response_item","payload":{"type":"function_call","name":"exec"}}`
	got, p, w := turnInfo(write(meta, user("이전 질문"), say("이전 답"), sys, user("SATAM 평가 다시 진행 해봐"), say("시작합니다"), tool, say("완료했습니다")))
	if p != "SATAM 평가 다시 진행 해봐" || w != nil || strings.Join(got, "|") != "시작합니다|완료했습니다" {
		t.Fatalf("사람 입력 턴: %q %q %v", got, p, w)
	}
	got, p, w = turnInfo(write(meta, user("질문"), say("답"), user(`[ATalk] r 방에서 나에게 온 메시지 1개:\n#42 [chat] 대장@마스터 (사람) → codex\n확인해`), say("확인했습니다")))
	if p != "" || len(w) != 1 || w[0] != 42 || strings.Join(got, "|") != "확인했습니다" {
		t.Fatalf("알림 턴: %q %q %v", got, p, w)
	}
}

func TestNeedInputTexts(t *testing.T) {
	if s := needInputText(Input{NotificationType: "permission_prompt", Message: "Claude needs your permission to use Bash"}); s == "" {
		t.Fatal("허가 요청")
	}
	if s := needInputText(Input{NotificationType: "idle_prompt", Message: "Claude is waiting for your input"}); s != "" {
		t.Fatal("오래 쉬는 중은 확인 요청 아님")
	}
	if s := askText([]byte(`{"questions":[{"question":"어느 쪽?","options":[{"label":"A"},{"label":"B"}]}]}`)); s != "어느 쪽? (A / B)" {
		t.Fatalf("질문: %q", s)
	}
	if s := permissionText(Input{ToolName: "shell", ToolInput: []byte(`{"command":["git","push"]}`)}); s != "shell 실행 허가: git push" {
		t.Fatalf("Codex 허가: %q", s)
	}
}

// 허가 알림: 곧 쓸 도구를 기억해 두었다가 "Claude needs your permission" 대신 도구·명령으로.
func TestPermissionFromPreTool(t *testing.T) {
	id := "claude-code_test-pretool"
	rememberTool(id, Input{ToolName: "Bash", ToolInput: []byte(`{"command":"git push origin main","description":"push"}`)})
	lt, ok := recentTool(id)
	if !ok || permissionText(Input{ToolName: lt.ToolName, ToolInput: lt.ToolInput}) != "Bash 실행 허가: git push origin main" {
		t.Fatalf("%+v", lt)
	}
	if s := permissionText(Input{ToolName: "Write", ToolInput: []byte(`{"file_path":"C:/x/a.go","content":"…"}`)}); s != "Write 실행 허가: C:/x/a.go" {
		t.Fatalf("%q", s)
	}
}
