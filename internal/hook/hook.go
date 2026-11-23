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
	Prompt         string `json:"prompt"` // UserPromptSubmit
	TranscriptPath string `json:"transcript_path"`
	// LastAssistantMessage 는 Stop 훅에 오는 마지막 응답 (Claude Code 버전에 따라 없을 수 있어 transcript 로 대신 읽는다)
	LastAssistantMessage string `json:"last_assistant_message"`
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
	if event == "session-start" || (event == "prompt" && strings.HasPrefix(strings.TrimSpace(in.Prompt), client.HumanPrefix)) {
		_ = daemon.Ensure(c) // 데몬이 없으면 백그라운드로 띄운다
	}
	if c.Health() != nil {
		return
	}
	var out any
	switch event {
	case "session-start":
		out = sessionStart(c, kind, in)
	case "tool": // PostToolUse (백그라운드): 지금까지 쓴 응답 문장을 실시간으로 기록
		if texts := turnTexts(in.TranscriptPath); len(texts) > 0 {
			_ = c.EchoReply(core.MakeAgentID(kind, in.SessionID), texts, false)
		}
	case "stop":
		out = stop(c, kind, in)
	case "prompt":
		out = prompt(c, kind, in)
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
	// 새 세션은 이전 세션의 이름표만 이어받는다. 방에는 사용자가 들어가라고 할 때만 들어간다.
	if r.InheritedFrom != "" {
		lines = append(lines, fmt.Sprintf("- 같은 폴더의 이전 세션(%s) 이름표를 이어받았습니다.", r.InheritedFrom))
	}
	if r.PreviousRoom != "" {
		lines = append(lines, fmt.Sprintf("- 이전 세션은 %s 방에 있었습니다. 지금은 방에 들어가 있지 않습니다. 사용자가 그 방에 들어가라고 하면 `%s request %s` 로 승인 없이 원래 자리로 돌아갑니다. 먼저 들어가지 마세요.", r.PreviousRoom, exe, r.PreviousRoom))
	}
	rec, title := r.Recovery, "[ATalk 복구] %s 방으로 복귀했습니다 (역할 %s)."
	if rec != nil {
		lines = append(lines, "", fmt.Sprintf(title, rec.Room, rec.Role))
		if p := c.Purpose(rec.Room); p != "" {
			lines = append(lines, p)
		}
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
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": strings.Join(lines, "\n")}}
}

func stop(c *client.Client, kind string, in Input) any {
	id := core.MakeAgentID(kind, in.SessionID)
	// CLI 입력을 기록했으면 이번 턴의 응답 문장 중 아직 기록 안 된 것을 남기고 턴을 닫는다 (기록할 입력이 없으면 데몬이 무시).
	// transcript 에 중간 문장이 빠지는 경우가 있어, 훅이 주는 마지막 응답이 없으면 덧붙인다.
	texts := turnTexts(in.TranscriptPath)
	if last := strings.TrimSpace(in.LastAssistantMessage); last != "" && !slices.Contains(texts, last) && !strings.Contains(strings.Join(texts, "\n"), last) {
		texts = append(texts, last)
	}
	_ = c.EchoReply(id, texts, true)
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
	if p := c.Purpose(room); p != "" {
		parts = append(parts, p)
	}
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
	parts = append(parts, client.HumanRule)
	parts = append(parts, "처리 순서는 atalk 스킬의 \"알림을 받았을 때\" 를 따르세요: log SEQ received → 작업 → send - --type result --reply SEQ --to 보낸이 → log SEQ done. result/status 에는 답장하지 마세요. 답할 필요가 없으면 log SEQ skipped.")
	return map[string]any{"decision": "block", "reason": strings.Join(parts, "\n\n")}
}

// prompt 는 UserPromptSubmit 훅. 입력이 ">>" 로 시작하면 사람이 대화방에 직접 하는 말이므로
// 모델을 거치지 않고 원문 그대로 방에 올리고, 모델로는 보내지 않는다(block).
func prompt(c *client.Client, kind string, in Input) any {
	text := strings.TrimSpace(in.Prompt)
	if !strings.HasPrefix(text, client.HumanPrefix) {
		// 방에 있으면 사람이 입력한 말을 대화 기록으로 남긴다 (/명령 제외, 알림 없음)
		if text != "" && !strings.HasPrefix(text, "/") {
			_ = c.EchoPrompt(core.MakeAgentID(kind, in.SessionID), text)
		}
		return listenReminder(c, kind, in)
	}
	text = strings.TrimSpace(strings.TrimPrefix(text, client.HumanPrefix))
	block := func(reason string) any { return map[string]any{"decision": "block", "reason": reason} }
	if text == "" {
		return block("[ATalk] 사용법: >> 보낼 말   (특정 멤버에게: >> @이름 보낼 말)")
	}
	id := core.MakeAgentID(kind, in.SessionID)
	m, err := c.SendHuman(id, text)
	if err != nil {
		return block("[ATalk] 보내지 못함: " + err.Error())
	}
	me := id
	if a, err := c.Agent(id); err == nil {
		me = a.Name
	}
	to := "방 전체"
	if len(m.To) > 0 {
		names := c.Names()
		ns := make([]string, len(m.To))
		for i, t := range m.To {
			ns[i] = orDefault(names[t], t)
		}
		to = strings.Join(ns, ", ")
	}
	return block(fmt.Sprintf("[ATalk] %s #%d → %s\n%s@%s: %s", m.Room, m.Seq, to, m.Human, me, m.Body))
}

// turnTexts 는 Claude Code transcript(JSONL) 에서 마지막 사람 입력 이후에 쓴 응답 문장들(CLI 의 흰색 ●)을 순서대로 꺼낸다.
// 도구 호출·결과는 뺀다. 작업 도중 끼어든 알림(<task-notification> 등)은 경계로 보지 않는다.
func turnTexts(path string) []string {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const tail = 4 << 20 // 끝에서 4MB 만 읽는다
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		_, _ = f.Seek(fi.Size()-tail, io.SeekStart)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(b), "\n")
	var texts []string // 뒤에서부터 모은다
	done := func() []string {
		slices.Reverse(texts)
		return texts
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type    string `json:"type"`
			IsMeta  bool   `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) != nil {
			continue
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		var str string
		if json.Unmarshal(e.Message.Content, &str) == nil {
			blocks = append(blocks, struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{"text", str})
		} else {
			_ = json.Unmarshal(e.Message.Content, &blocks)
		}
		switch e.Type {
		case "assistant":
			var parts []string
			for _, bl := range blocks {
				if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
					parts = append(parts, bl.Text)
				}
			}
			if len(parts) > 0 {
				texts = append(texts, strings.TrimSpace(strings.Join(parts, "\n")))
			}
		case "user":
			if e.IsMeta {
				continue
			}
			for _, bl := range blocks {
				if bl.Type == "text" && !core.IsSystemPrompt(bl.Text) { // 사람이 입력한 프롬프트 → 여기까지가 이번 턴
					return done()
				}
			}
		}
	}
	return done()
}

// listenReminder: 방에 있는 Claude 세션의 메시지 알림(atalk listen)이 꺼져 있으면, 사용자의 아무 입력에나
// "먼저 listen 을 켜라" 는 안내를 붙인다. 다시 시작한 Claude 는 첫 입력 전에는 스스로 listen 을 켤 수 없어서다.
// listen 이 이미 돌고 있으면(잠금 파일이 잡혀 있으면) 아무것도 하지 않는다.
func listenReminder(c *client.Client, kind string, in Input) any {
	if kind != "claude-code" { // Monitor 도구는 Claude Code 에만 있음
		return nil
	}
	id := core.MakeAgentID(kind, in.SessionID)
	if daemon.ListenRunning(id) {
		return nil
	}
	a, err := c.Agent(id)
	if err != nil || a.Room == nil {
		return nil
	}
	exe := "atalk"
	if !install.InUserPath(install.PathDir()) {
		exe = install.Exe()
	}
	msg := fmt.Sprintf("[ATalk] 이 세션은 %s 방에 있지만 메시지 알림(atalk listen)이 꺼져 있습니다. "+
		"사용자 요청을 처리하기 전에 먼저 Monitor 도구로 `%s listen` 을 실행하세요 (timeout_ms 1800000, 설명 \"ATalk %s 방 알림\"). "+
		"켠 뒤 `%s unread --ack` 로 그동안 온 메시지를 확인하고 atalk 스킬의 처리 규칙대로 처리한 다음, 사용자 요청을 이어서 처리하세요.",
		*a.Room, exe, *a.Room, exe)
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": msg}}
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
