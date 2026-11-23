package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastAssistantText(t *testing.T) {
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

	if got := lastAssistantText(write(final, user, interim, toolUse, toolRes, final, meta)); got != "세어 보겠습니다.\n\n42개입니다." {
		t.Fatalf("이번 턴의 응답 텍스트 전부(이전 턴 제외): %q", got)
	}
	// 마지막 입력 뒤에 응답 텍스트가 없으면 이전 턴 답을 가져오면 안 됨
	if got := lastAssistantText(write(user, final, user, toolUse, toolRes)); got != "" {
		t.Fatalf("이전 턴 답을 가져옴: %q", got)
	}
	if got := lastAssistantText(""); got != "" || lastAssistantText(filepath.Join(dir, "없음.jsonl")) != "" {
		t.Fatal("경로 없음")
	}
}
