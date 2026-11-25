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
