package server

import (
	"os"
	"path/filepath"
	"testing"
)

// 이어받기: 대화 기록의 마지막 권한 모드
func TestLastPermissionMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	p := filepath.Join(dir, "projects", "C--x", "s1.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(`{"type":"permission-mode","permissionMode":"default","sessionId":"s1"}
{"type":"user","message":{"content":"x"}}
{"type":"permission-mode","permissionMode":"bypassPermissions","sessionId":"s1"}
{"type":"assistant","message":{"content":[]}}
`), 0o644)
	if m := lastPermissionMode("s1"); m != "bypassPermissions" {
		t.Fatalf("%q", m)
	}
	if lastPermissionMode("none") != "" {
		t.Fatal("없는 세션")
	}
}
