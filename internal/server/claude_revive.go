package server

// Claude 되살리기 (에이전트 쪽 PC): Claude Code 는 메시지 대기 훅(asyncRewake)이 살아 있을 때만 밖에서 깨울 수 있다.
// 대기 훅이 끝난 세션(🔕 — 12시간 제한이 지났거나 Esc 로 중단함)에 나에게 온 메시지가 오면,
// 데몬이 그 창을 닫고(Claude 와 그 셸) 새 터미널 창에서 같은 세션을 이어서 연다 (claude --resume <세션> "<메시지 알림>").
// 쉬는 동안에는 아무것도 하지 않으므로 토큰을 쓰지 않는다. 사람이 그 창을 쓰는 중이면 안 되므로
// 대기 훅 프로세스가 정말 없고, 마지막 대화(transcript 의 마지막 주고받음)가 reviveQuiet 전일 때만 한다.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/procs"
)

const (
	reviveQuiet    = 10 * time.Minute // 대화 기록이 이만큼 조용해야 (사람이 그 창을 쓰는 중이 아님)
	reviveMsgAge   = 30 * time.Second // 메시지가 이만큼 기다렸는데도 전달되지 않았을 때 (대기 훅이 막 다시 걸리는 중일 수 있음)
	reviveCooldown = 10 * time.Minute // 한 세션을 연달아 되살리지 않게
	reviveMaxText  = 8000             // 명령줄로 넘기는 알림 최대 길이
)

type claudeReviver struct {
	mu      sync.Mutex
	last    map[string]time.Time
	invited map[string]bool // 이미 그 초대로 되살린 것 (답하지 않은 초대로 10분마다 다시 열지 않게)
}

// freshInvites: 아직 그 초대로 되살리지 않은 오래된 초대. r.mu 를 잡고 부른다.
func (r *claudeReviver) freshInvites(c *client.Client, id string) (out []core.PendingInvite) {
	for _, i := range oldInvites(c, id) {
		if !r.invited[id+"\n"+i.Room+"\n"+i.At] {
			out = append(out, i)
		}
	}
	return out
}

// waitHookAlive 는 이 세션의 메시지 대기 훅(atalk hook claude-code wait)이 살아 있는지. 대기 훅이 쓰는 표식 파일의 PID 로 본다.
func waitHookAlive(id string) bool {
	b, err := os.ReadFile(filepath.Join(os.TempDir(), "atalk-wait-"+id+".gen"))
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(b))
	pid, err := strconv.Atoi(s[strings.LastIndex(s, "-")+1:])
	return err == nil && pid > 0 && core.DefaultIsAlive(pid)
}

// claudeTranscript 는 Claude Code 세션의 대화 기록 파일 (~/.claude/projects/*/<세션>.jsonl). 못 찾으면 "".
func claudeTranscript(sessionID string) string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	m, _ := filepath.Glob(filepath.Join(dir, "projects", "*", sessionID+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}

// turnEnded 는 대화 기록의 마지막 주고받음이 끝난 턴인지: 마지막이 도구를 부르지 않은 응답이거나, Esc 로 중단함.
// 도구 호출·도구 결과·사람 입력으로 끝나 있으면 아직 일하는 중이다. at 은 그 마지막 주고받음의 시각 (없으면 0).
// 파일 수정 시각은 쓰지 않는다: Claude 는 쉬는 중에도 요약(away_summary) 같은 항목을 덧붙인다.
func turnEnded(path string) (ended bool, at time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return false, at
	}
	defer f.Close()
	const tail = 512 << 10
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, 0)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var e struct {
			Type    string `json:"type"`
			TS      string `json:"timestamp"`
			IsMeta  bool   `json:"isMeta"`
			Compact bool   `json:"isCompactSummary"`         // 대화 요약 (턴이 아님)
			Only    bool   `json:"isVisibleInTranscriptOnly"` // 기록에만 보이는 항목
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(lines[i]), &e) != nil || e.IsMeta || e.Compact || e.Only || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		c := string(e.Message.Content)
		at, _ = time.Parse(time.RFC3339Nano, e.TS)
		if e.Type == "assistant" {
			return !strings.Contains(c, `"tool_use"`), at
		}
		return strings.Contains(c, "[Request interrupted by user"), at
	}
	return false, at
}

// reviveClaude 는 이 PC 의 Claude Code 에이전트 중 🔕 인데 메시지가 온 세션을 되살린다 (watchLobbies 가 3초마다 부름).
func (s *Server) reviveClaude() {
	list, err := s.Core.ListAgents("", "claude-code")
	if err != nil {
		return
	}
	for _, v := range list {
		if !v.Local || v.Pid == nil || v.Status == "offline" || v.Cwd == nil || !procs.SafeSession(v.SessionID) {
			continue
		}
		if waitHookAlive(v.ID) || !core.DefaultIsAlive(*v.Pid) {
			continue // 대기 중이면 대기 훅이 깨운다. 창이 꺼졌으면 웹의 깨우기로
		}
		var c *client.Client
		if rc := client.LoadConn(v.ID); rc != nil { // 다른 PC 데몬에 연결됨 → 메시지·상태는 그쪽에
			c = s.relayClients.get(rc)
			a, err := c.Agent(v.ID)
			if err != nil {
				continue
			}
			v.Busy, v.Room, v.NeedsInput, v.Status = a.Busy, a.Room, a.NeedsInput, a.Status
		} else {
			c = client.New("")
		}
		if v.Busy || v.NeedsInput != "" || v.Status == "offline" {
			continue
		}
		s.revive.mu.Lock()
		noInvite := v.Room == nil && len(s.revive.freshInvites(c, v.ID)) == 0
		s.revive.mu.Unlock()
		if noInvite {
			continue // 방 밖: 초대가 와 있을 때만 (방장이 다시 초대했는데 대기 훅이 끊겨 수락하지 못하는 경우)
		}
		if ok, err := s.revive.check(c, v); err != nil {
			log.Printf("claude 되살리기 실패 (%s): %v", v.ID, err)
		} else if ok {
			log.Printf("claude 되살리기: %s 를 새 창에서 이어서 엶 (메시지 대기가 끊겨 있었음)", v.ID)
		}
	}
}

func (r *claudeReviver) check(c *client.Client, v core.AgentView) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]time.Time{}
	}
	if t, ok := r.last[v.ID]; ok && time.Since(t) < reviveCooldown {
		return false, nil
	}
	u, err := c.Unread(v.ID, false)
	if err != nil {
		return false, err
	}
	var forMe []core.InboxMessage
	oldest := time.Time{}
	for _, m := range u.Messages {
		if !m.ForMe {
			continue
		}
		forMe = append(forMe, m)
		if t, err := time.Parse(time.RFC3339Nano, m.TS); err == nil && (oldest.IsZero() || t.Before(oldest)) {
			oldest = t
		}
	}
	if r.invited == nil {
		r.invited = map[string]bool{}
	}
	invites := r.freshInvites(c, v.ID)
	if (len(forMe) == 0 || oldest.IsZero() || time.Since(oldest) < reviveMsgAge) && len(invites) == 0 {
		return false, nil
	}
	tp := claudeTranscript(v.SessionID)
	if tp == "" {
		return false, nil
	}
	ended, at := turnEnded(tp)
	if !ended {
		return false, nil // 턴 도중 (오래 걸리는 명령을 실행 중 등) — 작업 중 표시는 15분이면 풀리므로 기록으로 한 번 더 본다
	}
	if at.IsZero() {
		if st, err := os.Stat(tp); err == nil {
			at = st.ModTime()
		}
	}
	if time.Since(at) < reviveQuiet {
		return false, nil // 최근에 대화가 있었음 — 사람이 그 창을 쓰는 중일 수 있다
	}
	if n := procs.Name(*v.Pid); n != "" && !strings.Contains(strings.ToLower(n), "claude") && !strings.Contains(strings.ToLower(n), "node") {
		return false, nil // PID 가 다른 프로그램에 다시 쓰였음
	}
	if st, err := os.Stat(*v.Cwd); err != nil || !st.IsDir() {
		return false, nil
	}
	room := ""
	if v.Room != nil {
		room = *v.Room
	}
	if u.Room != nil {
		room = *u.Room
	}
	var text string
	if len(forMe) > 0 && room != "" {
		text = c.Notice(room, forMe, nil, false)
	}
	for _, i := range invites {
		how := "사용자에게 알리고, 사용자가 원하면"
		if strings.HasSuffix(i.By, "(사람)") {
			how = "내 사용자가 화면에서 보낸 초대이므로 바로"
		}
		text += fmt.Sprintf("\n[ATalk 초대] %s %q (보낸 이 %s) — %s `atalk accept %s`", i.Room, i.Title, i.By, how, i.Room)
	}
	text = strings.TrimSpace(text)
	if rs := []rune(text); len(rs) > reviveMaxText {
		text = string(rs[:reviveMaxText]) + "\n… (길어서 줄임: atalk unread 로 전체를 보세요)"
	}
	text = "[ATalk] 메시지 대기가 끊겨 있어 데몬이 이 세션을 다시 열었습니다.\n" + strings.TrimPrefix(text, "[ATalk] ")
	r.last[v.ID] = time.Now()
	_ = procs.CloseAgentWindow(*v.Pid) // 예전 창은 닫는다 (Claude 만 끄면 빈 셸 창이 남음)
	if err := procs.LaunchTerminal(*v.Cwd, []string{"claude", "--resume", v.SessionID, text}); err != nil {
		return false, err
	}
	if len(forMe) > 0 && room != "" {
		_ = c.Delivered(v.ID, room, client.ForMeSeqs(forMe))
	}
	for _, i := range invites {
		r.invited[v.ID+"\n"+i.Room+"\n"+i.At] = true
	}
	return true, nil
}

// oldInvites: 이 에이전트에게 온 대기 중 초대 중 reviveMsgAge 넘게 처리되지 않은 것 (대기 훅이 막 다시 걸리는 중일 수 있으므로).
func oldInvites(c *client.Client, id string) (out []core.PendingInvite) {
	p, err := c.Pending(id)
	if err != nil {
		return nil
	}
	for _, i := range p.Invites {
		if t, err := time.Parse(time.RFC3339Nano, i.At); err == nil && time.Since(t) >= reviveMsgAge {
			out = append(out, i)
		}
	}
	return out
}
