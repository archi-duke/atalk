package server

// Codex 깨우기: Codex 훅은 쉬고 있는 세션을 깨우지 못한다 (Claude Code 의 asyncRewake 같은 것이 없음).
// 대신 열린 Codex TUI 는 `codex queue --thread <세션> --message <글>` 로 넣은 메시지를 받으면 바로 새 턴을 시작하므로,
// 에이전트 쪽 PC 의 데몬이 쉬고 있는 Codex 에이전트를 주기적으로 보고 나에게 온 메시지가 있으면 대기열로 넣는다.
// 작업 중일 때 온 메시지는 Stop 훅이 턴 끝에서 이어서 처리하게 하므로 여기서는 쉬고 있을 때만 넣는다.

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/install"
)

// codexIdleFor 는 쉬기 시작한 뒤 대기열에 넣기까지 기다리는 시간 (Stop 훅이 같은 메시지를 처리하는 중일 수 있어서).
const codexIdleFor = 3 * time.Second

// codexMaxText 는 대기열 메시지 최대 길이 (Windows 명령줄 한계 32767 자 안쪽).
const codexMaxText = 20000

// CodexWaker 는 데몬이 Codex 에이전트마다 부른다. Queue 는 실제로 codex queue 를 실행한다 (시험에서는 바꿔 끼움).
type CodexWaker struct {
	Queue func(sessionID, text string) error
	Now   func() time.Time

	mu        sync.Mutex
	idleSince map[string]time.Time
	seen      map[string][]string // 에이전트별로 이미 알린 초대·신청·인사 규칙
}

func (w *CodexWaker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Check 는 Codex 에이전트 a 가 쉬고 있고 새로 알릴 것이 있으면 대기열에 넣는다. 넣었으면 true.
// c 는 그 에이전트가 쓰는 데몬 (원격 연결이면 원격 데몬).
func (w *CodexWaker) Check(c *client.Client, a core.AgentView) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.idleSince == nil {
		w.idleSince, w.seen = map[string]time.Time{}, map[string][]string{}
	}
	if a.Kind != "codex" || a.Busy || a.Status == "offline" || a.Pid == nil {
		delete(w.idleSince, a.ID)
		return false, nil
	}
	since, ok := w.idleSince[a.ID]
	if !ok {
		w.idleSince[a.ID] = w.now()
		return false, nil
	}
	if w.now().Sub(since) < codexIdleFor {
		return false, nil
	}

	u, err := c.Unread(a.ID, false)
	if err != nil {
		return false, err
	}
	var forMe []core.InboxMessage
	last := 0
	for _, m := range u.Messages {
		last = max(last, m.Seq)
		if m.ForMe {
			forMe = append(forMe, m)
		}
	}
	seen := w.seen[a.ID]
	var keys, extra []string
	if p, err := c.Pending(a.ID); err == nil {
		for _, i := range p.Invites {
			if k := "inv:" + i.Room + ":" + i.At; !slices.Contains(seen, k) {
				keys, extra = append(keys, k), append(extra, fmt.Sprintf("[초대] %s %q (보낸 이 %s) → atalk accept %s | decline %s", i.Room, i.Title, i.By, i.Room, i.Room))
			}
		}
		for _, q := range p.Requests {
			if k := "req:" + q.Room + ":" + q.Agent + ":" + q.At; !slices.Contains(seen, k) {
				keys, extra = append(keys, k), append(extra, fmt.Sprintf("[참여 신청] %s → %s %s → atalk approve %s | reject %s", q.Agent, q.Room, q.Note, q.Agent, q.Agent))
			}
		}
	}
	intro := ""
	if a.Intro && a.Room != nil {
		if k := "intro:" + *a.Room; !slices.Contains(seen, k) {
			intro = *a.Room
			keys = append(keys, k)
		}
	}
	if len(forMe) == 0 && len(keys) == 0 {
		return false, nil
	}
	room := ""
	if u.Room != nil {
		room = *u.Room
	} else if a.Room != nil {
		room = *a.Room
	}
	var text string
	if len(forMe) > 0 || len(extra) > 0 {
		text = c.Notice(room, forMe, extra, false)
	}
	if intro != "" {
		exe := "atalk"
		if !install.InUserPath(install.PathDir()) {
			exe = install.Exe()
		}
		text = strings.TrimSpace(client.IntroRule(exe, intro) + "\n\n" + text) // 인사 먼저 ("[ATalk" 로 시작해야 CLI 입력 기록에서 빠짐)
	}
	if r := []rune(text); len(r) > codexMaxText {
		text = string(r[:codexMaxText]) + "\n… (길어서 줄임: atalk unread 로 전체를 보세요)"
	}
	if err := w.Queue(a.SessionID, text); err != nil {
		return false, err
	}
	if len(forMe) > 0 {
		_ = c.Ack(a.ID, last)
		_ = c.Delivered(a.ID, room, client.ForMeSeqs(forMe)) // Codex 가 일하는 동안 그 메시지가 "처리 중" 으로 보이게
	}
	seen = append(seen, keys...)
	if len(seen) > 200 {
		seen = seen[len(seen)-200:]
	}
	w.seen[a.ID] = seen
	delete(w.idleSince, a.ID) // 이제 일을 시작함 (UserPromptSubmit 훅이 작업 중으로 바꿈)
	return true, nil
}
