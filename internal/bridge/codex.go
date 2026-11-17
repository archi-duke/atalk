package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Codex 는 codex exec / exec resume 로 헤드리스 세션을 실행한다.
// 브리지가 세션을 독점해서 한 번에 하나씩 실행하므로 동시 실행 충돌이 없다.
// 사람이 같은 세션을 TUI 로 동시에 여는 것은 지원하지 않는다 (사람이 쓰는 세션은 훅 방식을 쓴다).
type Codex struct {
	Bin       string   // 기본 "codex"
	SessionID string   // 이어서 쓸 기존 세션(thread) ID. 비우면 Start 에서 새로 만든다
	Cwd       string   // codex 를 실행할 폴더
	ExecArgs  []string // codex exec 에 추가로 넘길 인자 (예: -s read-only, -m gpt-5)
	Timeout   time.Duration
}

func (d *Codex) Kind() string { return "codex" }

func (d *Codex) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "codex"
}

func (d *Codex) Start(ctx context.Context, intro string) (string, error) {
	if d.SessionID != "" {
		return d.SessionID, nil
	}
	_, tid, err := d.exec(ctx, nil, intro)
	if err != nil {
		return "", err
	}
	if tid == "" {
		return "", errors.New("codex 출력에서 thread id 를 찾지 못함")
	}
	d.SessionID = tid
	return tid, nil
}

func (d *Codex) Busy(context.Context) (bool, error) { return false, nil }

func (d *Codex) Alive(context.Context) error {
	_, err := exec.LookPath(d.bin())
	return err
}

func (d *Codex) Run(ctx context.Context, prompt string) (string, error) {
	reply, _, err := d.exec(ctx, []string{"resume", d.SessionID}, prompt)
	return reply, err
}

// exec 는 codex exec [sub...] --json -o FILE - 를 실행하고 (마지막 답변, thread id) 를 돌려준다.
// 프롬프트는 stdin 으로 넘긴다 (명령줄 길이/인용 문제 회피).
func (d *Codex) exec(ctx context.Context, sub []string, prompt string) (string, string, error) {
	timeout := d.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := os.CreateTemp("", "atalk-codex-*.txt")
	if err != nil {
		return "", "", err
	}
	out.Close()
	defer os.Remove(out.Name())

	args := append([]string{"exec"}, sub...)
	args = append(args, "--json", "--skip-git-repo-check", "-o", out.Name())
	if len(sub) == 0 { // resume 은 처음 세션의 설정(sandbox 등)을 이어받는다
		args = append(args, d.ExecArgs...)
	}
	args = append(args, "-")
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	if d.Cwd != "" {
		cmd.Dir = d.Cwd
	}
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	tid, lastMsg := "", ""
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "thread.started":
			tid = jsonStr(ev, "thread_id")
		case "item.completed":
			if jsonStr(ev, "item", "type") == "agent_message" {
				lastMsg = jsonStr(ev, "item", "text")
			}
		}
	}
	if runErr != nil {
		return "", tid, fmt.Errorf("codex %s 실패: %v: %s", strings.Join(args[:min(2, len(args))], " "), runErr, tail(stderr.String(), 500))
	}
	reply := lastMsg
	if b, err := os.ReadFile(out.Name()); err == nil && strings.TrimSpace(string(b)) != "" {
		reply = string(b)
	}
	return reply, tid, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
