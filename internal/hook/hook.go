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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	c := client.ForAgent(core.MakeAgentID(kind, in.SessionID)) // 다른 PC 의 데몬에 연결된 세션이면 그쪽으로 (atalk connect)
	if event == "session-start" || (event == "prompt" && strings.HasPrefix(strings.TrimSpace(in.Prompt), client.HumanPrefix)) {
		_ = daemon.Ensure(c) // 데몬이 없으면 백그라운드로 띄운다
	}
	id := core.MakeAgentID(kind, in.SessionID)
	// CLI 입력·응답·진행 줄 기록은 데몬이 없어도 outbox 에 남겨 두고 나중에 보낸다 (outbox.go)
	healthy := c.Health() == nil
	if healthy {
		flush(c, id, 0) // 전에 못 보낸 기록부터
	}
	// 시작 훅을 놓친 세션 (Codex: 훅을 신뢰하기 전에 연 세션은 SessionStart 가 이미 지나감) → 입력·응답 끝에서 등록
	if healthy && (event == "prompt" || event == "stop") {
		var ce *client.Error
		if _, err := c.Agent(id); errors.As(err, &ce) && ce.Code == "agent_not_found" {
			_ = sessionStart(c, kind, in)
			if event == "prompt" && !core.IsSystemPrompt(in.Prompt) {
				_ = c.Activate(id) // 사람이 입력했으므로 임시 등록을 바로 확정
			}
		}
	}
	var out any
	switch event {
	case "session-start":
		if healthy {
			out = sessionStart(c, kind, in)
		}
	case "tool": // PostToolUse (백그라운드): 지금까지 쓴 응답 문장(흰색 ●)을 실시간으로 기록 + 작업 중 신호 갱신. 도구 줄(녹색 ●)·출력은 남기지 않음
		if healthy {
			_ = c.Activity(id, true)
		}
		texts, p, wake := turnInfo(in.TranscriptPath)
		if len(texts) > 0 {
			enqueue(c, id, client.EchoEvent{Reply: true, Texts: texts, Final: ptrBool(false), Prompt: p, Wake: wake})
		}
	case "stop":
		echoTurnEnd(c, id, in)
		if healthy {
			out = stop(c, kind, in)
			// 응답이 끝났으면 대기. 처리할 방 메시지가 있어 Claude 를 계속 일하게 하면(block) 작업 중 유지
			_ = c.Activity(id, out != nil)
		}
	case "prompt":
		echoPrompt(c, id, in)
		if healthy {
			out = prompt(c, kind, in)
		}
	case "session-end":
		echoTurnEnd(c, id, in) // Esc 로 중단한 턴의 남은 문장
		if healthy {
			_ = c.Activity(id, false)
			_ = c.Away(id, "session_end:"+orDefault(in.Reason, "exit"))
		}
	}
	if out != nil {
		_ = json.NewEncoder(stdout).Encode(out)
	}
}

func ptrBool(b bool) *bool { return &b }

// echoTurnEnd 는 턴이 끝날 때(Stop) 이번 턴의 응답 문장 중 아직 기록 안 된 것을 남기고 그 입력을 닫는다 (기록할 입력이 없으면 데몬이 무시).
// transcript 에 중간 문장이 빠지거나 늦게 저장되는 경우가 있어, 훅이 주는 마지막 응답은 항상 덧붙인다.
// SessionEnd 에도 불러, Esc 로 중단해 Stop 이 오지 않은 턴의 남은 문장을 남긴다.
func echoTurnEnd(c *client.Client, id string, in Input) {
	texts, p, wake := turnInfo(in.TranscriptPath)
	if last := strings.TrimSpace(in.LastAssistantMessage); last != "" && !slices.Contains(texts, last) && !strings.Contains(strings.Join(texts, "\n"), last) {
		texts = append(texts, last)
	}
	if len(texts) == 0 && p == "" {
		return
	}
	enqueue(c, id, client.EchoEvent{Reply: true, Texts: texts, Final: ptrBool(true), Prompt: p, Wake: wake})
}

// echoPrompt 는 사람이 입력한 말을 대화 기록으로 남긴다 (방에 있을 때, /명령·>> 제외, 알림 없음).
// 그 전에 앞 턴의 남은 문장을 보낸다: Esc 로 중단하면 Stop 이 오지 않아 마지막 문장이 빠지기 때문이다.
// 앞 턴이 아직 진행 중(이 입력이 대기열에 들어감)일 수 있으므로 앞 입력을 닫지는 않는다 — 다음 턴이 끝날 때 함께 닫힌다.
func echoPrompt(c *client.Client, id string, in Input) {
	text := strings.TrimSpace(in.Prompt)
	if text == "" || strings.HasPrefix(text, client.HumanPrefix) || strings.HasPrefix(text, "/") {
		return
	}
	if texts, p := turnTexts(in.TranscriptPath); len(texts) > 0 && p != "" && !core.SamePrompt(p, text) {
		enqueue(c, id, client.EchoEvent{Reply: true, Texts: texts, Final: ptrBool(false), Prompt: p})
	}
	enqueue(c, id, client.EchoEvent{Text: text})
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
	// 새 세션은 임시로 등록한다: 사람이 처음 입력할 때 확정되어 그때 이전 세션의 이름표·자리를 넘겨받는다.
	// (켜자마자 /resume 으로 이전 세션을 열면 이 임시 카드는 정리되고 이전 세션이 그 방으로 돌아간다)
	reg := core.RegisterInput{Kind: kind, SessionID: in.SessionID, Cwd: &cwd, Name: envPtr("ATALK_NAME"), Description: envPtr("ATALK_DESCRIPTION"), Provisional: true}
	if pid := procs.FindAgentPID(); pid > 0 {
		reg.Pid = &pid
		reg.Shared = kind == "codex" && procs.IsCodexServer(pid)
		reg.Ancestors = procs.Ancestors(pid) // 다른 에이전트가 이 세션을 실행했으면 그 에이전트를 띄운 쪽으로
	}
	reg.Spawn = os.Getenv("ATALK_SPAWN") // atalk spawn 으로 연 창
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
	if reg.Shared {
		lines = append(lines, sharedNote)
	}
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
		switch kind {
		case "claude-code":
			lines = append(lines, "메시지 알림은 자동입니다 (응답을 마칠 때마다 기다리다가 메시지가 오면 깨움). atalk listen 은 켜지 않아도 됩니다.")
		case "codex":
			lines = append(lines, "메시지 알림은 자동입니다 (쉬고 있을 때 메시지가 오면 ATalk 데몬이 이 세션에 넣어 줌). atalk listen 은 켜지 않아도 됩니다.")
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
	out := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": strings.Join(lines, "\n")}}
	if reg.Shared {
		out["systemMessage"] = sharedNote // 사람 화면에도
	}
	return out
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
		if u.Room != nil {
			_ = c.Delivered(id, *u.Room, client.ForMeSeqs(u.Messages)) // 처리 기록을 늦게 남겨도 일하는 동안 "처리 중" 으로
		}
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
	texts := make([]string, len(fresh))
	for i, f := range fresh {
		texts[i] = f.text
	}
	out := map[string]any{"decision": "block", "reason": c.Notice(room, u.Messages, texts, true)}
	// 사람 화면에도 바로 보이게 (reason 은 모델에게만 간다)
	var shown []string
	if d := client.Digest(u.Messages, c.Names()); d != "" {
		shown = append(shown, fmt.Sprintf("[ATalk] %s 방에서 받은 메시지:", room), d)
	}
	for _, f := range fresh {
		shown = append(shown, "📨 "+f.text)
	}
	if len(shown) > 0 {
		out["systemMessage"] = strings.Join(shown, "\n")
	}
	return out
}

// prompt 는 UserPromptSubmit 훅. 입력이 ">>" 로 시작하면 사람이 대화방에 직접 하는 말이므로
// 모델을 거치지 않고 원문 그대로 방에 올리고, 모델로는 보내지 않는다(block).
func prompt(c *client.Client, kind string, in Input) any {
	text := strings.TrimSpace(in.Prompt)
	if !strings.HasPrefix(text, client.HumanPrefix) { // 입력을 받아 일을 시작함 (>> 는 모델로 가지 않음)
		_ = c.Activity(core.MakeAgentID(kind, in.SessionID), true)
	}
	if text != "" && !core.IsSystemPrompt(text) { // 사람이 입력함 → 임시 세션 확정
		_ = c.Activate(core.MakeAgentID(kind, in.SessionID))
	}
	if !strings.HasPrefix(text, client.HumanPrefix) { // CLI 입력 기록은 Run 이 outbox 로 (echoPrompt)
		return nil
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
// 함께 이번 턴을 시작한 사람 입력(prompt)도 돌려준다 — 데몬이 응답을 기다리는 CLI 입력과 같은지 확인하는 데 쓴다
// (사람이 입력한 말이 대기열에 들어간 동안 끝난 앞선 턴의 문장이 그 입력의 응답으로 붙지 않게).
func turnTexts(path string) (texts []string, prompt string) {
	texts, prompt, _ = turnInfo(path)
	return
}

// noticeRe 는 ATalk 알림 본문의 메시지 줄 "#SEQ [type] ..." (대기 훅·응답 끝 훅 알림 공통).
var noticeRe = regexp.MustCompile(`(?m)^#(\d+) \[`)

// noticeSeqs 는 ATalk 알림이면 그 안의 메시지 번호들, 아니면 nil.
func noticeSeqs(text string) []int {
	if !strings.Contains(text, "[ATalk]") {
		return nil
	}
	var out []int
	for _, m := range noticeRe.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// turnInfo 는 turnTexts 에 더해, 이번 턴이 ATalk 알림으로 시작했으면 그 알림의 메시지 번호들(wake)도 돌려준다
// (사람 입력이 아니라 알림으로 깨어난 턴 — 응답을 그 메시지의 답으로 기록하는 데 쓴다).
func turnInfo(path string) (texts []string, prompt string, wake []int) {
	if path == "" {
		return nil, "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", nil
	}
	defer f.Close()
	const tail = 4 << 20 // 끝에서 4MB 만 읽는다
	if fi, err := f.Stat(); err == nil && fi.Size() > tail {
		_, _ = f.Seek(fi.Size()-tail, io.SeekStart)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, "", nil
	}
	lines := strings.Split(string(b), "\n")
	if isCodexRollout(lines) {
		return codexTurn(lines)
	}
	// texts 는 뒤에서부터 모은다
	done := func() ([]string, string, []int) {
		slices.Reverse(texts)
		return texts, prompt, wake
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type    string `json:"type"`
			IsMeta  bool   `json:"isMeta"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			// 작업 도중 사람이 보낸 말은 user 항목이 아니라 queued_command 첨부로 들어온다 (그 뒤 문장은 그 말에 대한 응답)
			Attachment struct {
				Type   string `json:"type"`
				Prompt string `json:"prompt"`
			} `json:"attachment"`
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
		case "attachment":
			if e.Attachment.Type == "queued_command" && strings.TrimSpace(e.Attachment.Prompt) != "" && !core.IsSystemPrompt(e.Attachment.Prompt) {
				prompt = e.Attachment.Prompt
				return done()
			}
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
			// ATalk 알림(대기 훅이 깨움 / 응답 끝 훅이 계속 일하게 함)으로 시작한 턴 → 그 알림이 경계, 응답은 그 메시지들에 대한 것
			for _, bl := range blocks {
				if bl.Type == "text" {
					if seqs := noticeSeqs(bl.Text); len(seqs) > 0 {
						wake = seqs
						return done()
					}
				}
			}
			if e.IsMeta {
				continue
			}
			for _, bl := range blocks {
				if bl.Type == "text" && !core.IsSystemPrompt(bl.Text) { // 사람이 입력한 프롬프트 → 여기까지가 이번 턴
					prompt = bl.Text
					return done()
				}
			}
		}
	}
	return done()
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

// sharedNote 는 Codex 가 공유 백그라운드 서버에서 돌 때의 안내 (사람에게도 보임).
const sharedNote = "[ATalk] 이 Codex 세션은 공유 백그라운드 서버에서 실행 중이라, 웹 화면에서 이 창을 찾을 수 없고(Ctrl+클릭·깨우기) 창을 닫아도 방에 남아 보입니다. ATalk 와 함께 쓰려면 `codex --no-daemon` 으로 여세요 (이어서 열기: `codex resume --no-daemon <세션>`)."
