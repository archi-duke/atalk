// Package hook 은 에이전트 도구의 훅 어댑터다. Claude Code 와 Codex(0.166+) 는 훅 입출력 형식이 같아서
// 하나의 구현으로 둘 다 지원한다.
//
//	session-start → 네임카드 자동 등록 (+ 복구 정보 / 내 agent id 를 컨텍스트로 주입)
//	stop          → 나에게 온 새 메시지가 있으면 종료를 막고 이어서 처리하게 함 ({"decision":"block"})
//	session-end   → away 처리 (resume 으로 돌아오면 복귀)
//
// 데몬이 꺼져 있거나 오류가 나도 아무것도 출력하지 않고 0 으로 끝난다 (세션을 방해하지 않음).
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/daemon"
	"atalk/internal/install"
	"atalk/internal/procs"
)

type Input struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	HookEventName  string `json:"hook_event_name"`
	Source         string `json:"source"` // SessionStart: startup | resume | clear | compact | fork
	StopHookActive bool   `json:"stop_hook_active"`
	Reason         string `json:"reason"` // SessionEnd
}

// Run 은 stdin 에서 훅 입력을 읽고 stdout 에 훅 출력을 쓴다.
func Run(kind, event string, stdin io.Reader, stdout io.Writer) {
	defer func() { _ = recover() }() // 어떤 경우에도 세션을 깨지 않는다
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return
	}
	var in Input
	if json.Unmarshal(raw, &in) != nil || in.SessionID == "" || !core.IsValidKind(kind) {
		return
	}
	c := client.New("")
	if event == "session-start" {
		_ = daemon.Ensure(c) // 데몬이 없으면 백그라운드로 띄운다
	}
	if c.Health() != nil {
		return
	}
	var out any
	switch event {
	case "session-start":
		out = sessionStart(c, kind, in)
	case "stop":
		out = stop(c, kind, in)
	case "session-end":
		_ = c.Away(core.MakeAgentID(kind, in.SessionID), "session_end:"+orDefault(in.Reason, "exit"))
	}
	if out != nil {
		_ = json.NewEncoder(stdout).Encode(out)
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func envPtr(k string) *string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return &v
	}
	return nil
}

func sessionStart(c *client.Client, kind string, in Input) any {
	cwd := orDefault(in.Cwd, mustGetwd())
	reg := core.RegisterInput{Kind: kind, SessionID: in.SessionID, Cwd: &cwd, Name: envPtr("ATALK_NAME"), Description: envPtr("ATALK_DESCRIPTION")}
	if pid := procs.FindAgentPID(); pid > 0 {
		reg.Pid = &pid
	}
	if caps := os.Getenv("ATALK_CAPABILITIES"); caps != "" {
		list := []string{}
		for _, s := range strings.Split(caps, ",") {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
		reg.Capabilities = &list
	}
	r, err := c.Register(reg)
	if err != nil {
		return nil
	}
	a := r.Agent
	exe := "atalk"
	if !install.InUserPath(install.PathDir()) {
		exe = install.Exe()
	}
	lines := []string{
		"[ATalk] 이 세션은 에이전트 대화방(ATalk)에 등록되어 있습니다.",
		fmt.Sprintf("- 내 agent id: %s  (이름 %q, 상태 %s)", a.ID, a.Name, a.Status),
		"- 대화방 조회/생성/참여는 atalk 스킬을 쓰세요. 사용자가 요청하지 않으면 대화방 작업을 먼저 시작하지 마세요.",
	}
	if kind != "claude-code" {
		lines = append(lines, fmt.Sprintf("- atalk 명령에는 --as %s 를 붙이세요 (MCP 를 쓰면 atalk_attach(agent_id=%q)).", a.ID, a.ID))
	}
	if rec := r.Recovery; rec != nil {
		lines = append(lines, "", fmt.Sprintf("[ATalk 복구] %s 방으로 복귀했습니다 (역할 %s).", rec.Room, rec.Role))
		if kind == "claude-code" {
			lines = append(lines, fmt.Sprintf("메시지 알림을 받으려면 Monitor 도구로 `%s listen` 을 실행하세요 (atalk 스킬의 \"메시지 받기\" 참고).", exe))
		}
		if len(rec.Unfinished) > 0 {
			lines = append(lines, "처리하다 만 작업:")
			for _, u := range rec.Unfinished {
				body := ""
				if u.Message != nil {
					body = truncate(u.Message.Body, 200)
				}
				lines = append(lines, fmt.Sprintf("  #%d (%s) %s", u.Seq, u.State, body))
			}
		}
		if len(rec.Unread) > 0 {
			lines = append(lines, "놓친 메시지:", client.FormatMessages(rec.Unread, c.Names()))
		}
	} else {
		// 같은 폴더의 죽은 세션이 방에 남아 있으면 이어받기를 안내
		if away, err := c.Agents("away", kind); err == nil {
			for _, x := range away {
				if x.ID != a.ID && x.Cwd != nil && samePath(*x.Cwd, cwd) && x.Room != nil {
					lines = append(lines, fmt.Sprintf("- 같은 폴더의 이전 세션 %s 가 %s 방에 자리 비움 상태로 남아 있습니다. 사용자가 원하면 `%s takeover %s` 로 이어받을 수 있습니다.", x.ID, *x.Room, exe, x.ID))
				}
			}
		}
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": strings.Join(lines, "\n")}}
}

func stop(c *client.Client, kind string, in Input) any {
	id := core.MakeAgentID(kind, in.SessionID)
	u, err := c.Unread(id, false)
	if err != nil {
		return nil
	}
	forMe := slices.ContainsFunc(u.Messages, func(m core.InboxMessage) bool { return m.ForMe })

	// 새로 들어온 초대/신청 (이전에 알린 건 제외)
	type item struct{ key, text string }
	var fresh []item
	seenFile := filepath.Join(os.TempDir(), "atalk-hook-"+id+".json")
	var seen []string
	if b, err := os.ReadFile(seenFile); err == nil {
		_ = json.Unmarshal(b, &seen)
	}
	if p, err := c.Pending(id); err == nil {
		for _, i := range p.Invites {
			k := "inv:" + i.Room + ":" + i.At
			if !slices.Contains(seen, k) {
				fresh = append(fresh, item{k, fmt.Sprintf("[초대] %s %q (보낸 이 %s) → atalk_respond_invite", i.Room, i.Title, i.By)})
			}
		}
		for _, q := range p.Requests {
			k := "req:" + q.Room + ":" + q.Agent + ":" + q.At
			if !slices.Contains(seen, k) {
				fresh = append(fresh, item{k, fmt.Sprintf("[참여 신청] %s → %s %s → atalk_respond_request", q.Agent, q.Room, q.Note)})
			}
		}
	}
	if !forMe && len(fresh) == 0 {
		return nil
	}
	if forMe {
		_ = c.Ack(id, u.Messages[len(u.Messages)-1].Seq)
	}
	if len(fresh) > 0 {
		for _, f := range fresh {
			seen = append(seen, f.key)
		}
		if len(seen) > 200 {
			seen = seen[len(seen)-200:]
		}
		b, _ := json.Marshal(seen)
		_ = os.WriteFile(seenFile, b, 0o644)
	}
	room := ""
	if u.Room != nil {
		room = *u.Room
	}
	parts := []string{fmt.Sprintf("[ATalk] %s 방에서 새 메시지가 왔습니다. 처리하세요.", room)}
	if len(u.Messages) > 0 {
		parts = append(parts, client.FormatMessages(u.Messages, c.Names()))
	}
	if len(fresh) > 0 {
		texts := make([]string, len(fresh))
		for i, f := range fresh {
			texts[i] = f.text
		}
		parts = append(parts, strings.Join(texts, "\n"))
	}
	parts = append(parts, "처리 순서는 atalk 스킬의 \"알림을 받았을 때\" 를 따르세요: log SEQ received → 작업 → send - --type result --reply SEQ --to 보낸이 → log SEQ done. result/status 에는 답장하지 마세요. 답할 필요가 없으면 log SEQ skipped.")
	return map[string]any{"decision": "block", "reason": strings.Join(parts, "\n\n")}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}

func samePath(a, b string) bool {
	pa, _ := filepath.Abs(a)
	pb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(pa), filepath.Clean(pb))
}
