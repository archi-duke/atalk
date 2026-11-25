//go:build windows

package procs

import (
	"strings"
	"testing"
)

// 새 창 스크립트: 환경 변수(spawn 표식)를 먼저 정하고, 인자의 작은따옴표는 두 번 써서 그대로 전달한다.
func TestPSScriptEnv(t *testing.T) {
	s := psScript([]string{"codex", "--no-daemon", "it's"}, map[string]string{"ATALK_SPAWN": "ab'c"})
	if !strings.HasPrefix(s, "$env:ATALK_SPAWN = 'ab''c'\n& 'codex' '--no-daemon' 'it''s'") {
		t.Fatalf("%q", s)
	}
}
