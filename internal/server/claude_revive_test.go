package server

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestWaitHookAlive(t *testing.T) {
	id := "claude-code_revive-test-" + strconv.Itoa(os.Getpid())
	f := filepath.Join(os.TempDir(), "atalk-wait-"+id+".gen")
	defer os.Remove(f)
	if waitHookAlive(id) {
		t.Fatal("표식이 없으면 대기 중 아님")
	}
	_ = os.WriteFile(f, []byte("abc-"+strconv.Itoa(os.Getpid())), 0o644)
	if !waitHookAlive(id) {
		t.Fatal("살아 있는 PID")
	}
	_ = os.WriteFile(f, []byte("abc-999999999"), 0o644)
	if waitHookAlive(id) {
		t.Fatal("죽은 PID")
	}
}

func TestClaudeTranscript(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	p := filepath.Join(dir, "projects", "C--x", "s1.jsonl")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("{}\n"), 0o644)
	if got := claudeTranscript("s1"); got != p {
		t.Fatalf("%q", got)
	}
	if claudeTranscript("s2") != "" {
		t.Fatal("없는 세션")
	}
}

func TestTurnEnded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	w := func(lines ...string) bool {
		s := ""
		for _, l := range lines {
			s += l + "\n"
		}
		_ = os.WriteFile(p, []byte(s), 0o644)
		return turnEnded(p)
	}
	user := `{"type":"user","message":{"content":"해줘"}}`
	say := `{"type":"assistant","message":{"content":[{"type":"text","text":"끝"}]}}`
	tool := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`
	res := `{"type":"user","message":{"content":[{"type":"tool_result","content":"x"}]}}`
	esc := `{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
	sys := `{"type":"system","subtype":"x"}`
	sum := `{"type":"user","isCompactSummary":true,"isVisibleInTranscriptOnly":true,"message":{"content":"This session is being continued"}}`
	if !w(user, say, sys) || w(user, tool) || w(user, tool, res) || w(user) || !w(user, tool, esc) || !w(user, say, sum) {
		t.Fatal("턴 끝 판단")
	}
}
