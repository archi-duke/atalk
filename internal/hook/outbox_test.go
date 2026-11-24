package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/server"
)

// tempOutbox 는 테스트가 실제 임시 폴더에 outbox 를 남기지 않게 한다 (Windows 의 os.TempDir 는 TMP/TEMP, 그 밖은 TMPDIR).
func tempOutbox(t *testing.T) {
	d := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, d)
	}
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startDaemon(t *testing.T, root string, port int) *server.Server {
	t.Helper()
	srv, err := server.New(server.Options{Root: root, Addr: fmt.Sprintf("127.0.0.1:%d", port), MachineID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	return srv
}

func echoBodies(t *testing.T, c *client.Client, room string) []string {
	t.Helper()
	msgs, err := c.Messages(room, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		if m.Echo {
			out = append(out, m.Body)
		}
	}
	return out
}

// 데몬이 꺼져 있는 동안의 기록은 outbox 에 남았다가, 데몬이 돌아오면 순서대로 한 번씩만 보내진다.
func TestOutboxSurvivesDaemonDown(t *testing.T) {
	tempOutbox(t)
	root, port := t.TempDir(), freePort(t)
	c := client.New(fmt.Sprintf("http://127.0.0.1:%d", port))
	srv := startDaemon(t, root, port)
	a, err := c.Register(core.RegisterInput{Kind: "claude-code", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	id := a.Agent.ID
	if _, err := c.CreateRoom(id, "r", ""); err != nil {
		t.Fatal(err)
	}
	srv.Close() // 데몬 재시작 중

	enqueue(c, id, client.EchoEvent{Text: "빌드해 줘"})
	enqueue(c, id, client.EchoEvent{Reply: true, Texts: []string{"빌드했습니다"}, Final: ptrBool(true), Prompt: "빌드해 줘"})
	enqueue(c, "claude-code_없음", client.EchoEvent{Text: "카드 없는 에이전트"})
	if n, _ := pending(outboxDir(id)); len(n) != 2 {
		t.Fatalf("데몬이 없으면 outbox 에 남아야 함: %v", n)
	}

	srv = startDaemon(t, root, port)
	defer srv.Close()
	flush(c, id, 0)
	if n, _ := pending(outboxDir(id)); len(n) != 0 {
		t.Fatalf("보낸 뒤에는 비어야 함: %v", n)
	}
	if got := strings.Join(echoBodies(t, c, "r"), "|"); got != "빌드해 줘|빌드했습니다" {
		t.Fatalf("순서대로 기록: %q", got)
	}
	// 4xx(카드 없음)는 다시 보내도 소용없으므로 버림
	flush(c, "claude-code_없음", 0)
	if n, _ := pending(outboxDir("claude-code_없음")); len(n) != 0 {
		t.Fatal("4xx 는 버려야 함")
	}
	// 같은 eid 를 다시 보내면(응답을 못 받아 재전송) 한 번만 기록
	e := client.EchoEvent{EID: "dup-1", Text: "두 번 가는 입력"}
	_ = c.Echo(id, e)
	_ = c.Echo(id, e)
	if got := echoBodies(t, c, "r"); strings.Count(strings.Join(got, "|"), "두 번 가는 입력") != 1 {
		t.Fatalf("같은 eid 는 한 번만: %v", got)
	}
}

// Claude 훅 전체 흐름: 입력 → 도구(중간 문장, 도구 줄은 남기지 않음) → Stop(transcript 에 빠진 마지막 답도 남김).
func TestHookEchoFlow(t *testing.T) {
	tempOutbox(t)
	t.Setenv("ATALK_CONFIG_DIR", t.TempDir())
	root, port := t.TempDir(), freePort(t)
	t.Setenv("ATALK_URL", fmt.Sprintf("http://127.0.0.1:%d", port))
	srv := startDaemon(t, root, port)
	defer srv.Close()
	c := client.New("")
	if _, err := c.Register(core.RegisterInput{Kind: "claude-code", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateRoom("claude-code_s1", "r", ""); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeTr := func(lines ...string) {
		_ = os.WriteFile(tr, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	}
	run := func(event string, in map[string]any) {
		in["session_id"], in["transcript_path"] = "s1", tr
		b, _ := json.Marshal(in)
		Run("claude-code", event, bytes.NewReader(b), &bytes.Buffer{})
	}
	user := `{"type":"user","message":{"role":"user","content":"빌드해 줘"}}`
	interim := `{"type":"assistant","message":{"content":[{"type":"text","text":"빌드하겠습니다."}]}}`
	toolUse := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`

	writeTr()
	run("prompt", map[string]any{"prompt": "빌드해 줘"})
	writeTr(user, interim, toolUse)
	run("tool", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "go build ./...", "description": "빌드"}})
	// 마지막 답이 transcript 에 아직 저장되지 않았어도 Stop 의 last_assistant_message 로 남는다
	run("stop", map[string]any{"last_assistant_message": "빌드가 끝났습니다."})

	got := strings.Join(echoBodies(t, c, "r"), "|")
	if got != "빌드해 줘|빌드하겠습니다.|빌드가 끝났습니다." {
		t.Fatalf("기록: %q", got)
	}
}

// 시작 훅을 놓친 Codex 세션 (훅을 신뢰하기 전에 연 세션): 입력 훅에서 등록·확정된다.
func TestLateRegister(t *testing.T) {
	tempOutbox(t)
	t.Setenv("ATALK_CONFIG_DIR", t.TempDir())
	root, port := t.TempDir(), freePort(t)
	t.Setenv("ATALK_URL", fmt.Sprintf("http://127.0.0.1:%d", port))
	srv := startDaemon(t, root, port)
	defer srv.Close()
	c := client.New("")
	b, _ := json.Marshal(map[string]any{"session_id": "thr-late", "cwd": t.TempDir(), "prompt": "안녕"})
	Run("codex", "prompt", bytes.NewReader(b), &bytes.Buffer{})
	a, err := c.Agent("codex_thr-late")
	if err != nil || a.Provisional {
		t.Fatalf("입력 훅에서 등록·확정: %+v %v", a, err)
	}
}
