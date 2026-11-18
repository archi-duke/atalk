package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/daemon"
	"atalk/internal/procs"
)

// identity 는 이 명령을 실행한 에이전트의 id 를 정한다.
//
//	--as → ATALK_AGENT (opencode 플러그인이 bash 환경에 넣음) → CLAUDE_CODE_SESSION_ID (Claude Code 가 bash 에 넣음)
//
// Claude Code 처럼 환경에서 유도한 경우에는 등록에 쓸 정보도 함께 돌려준다 (훅 없이 스킬만 쓴 세션용).
func identity(a args) (string, *core.RegisterInput) {
	if id := a.str("as"); id != "" {
		return id, nil
	}
	if id := os.Getenv("ATALK_AGENT"); id != "" {
		return id, nil
	}
	if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		cwd, _ := os.Getwd()
		in := core.RegisterInput{Kind: "claude-code", SessionID: sid, Cwd: &cwd}
		if v := os.Getenv("ATALK_NAME"); v != "" {
			in.Name = &v
		}
		pid, _ := strconv.Atoi(os.Getenv("CLAUDE_PID"))
		if pid <= 0 {
			pid = procs.FindAgentPID()
		}
		if pid > 0 {
			in.Pid = &pid
		}
		return core.MakeAgentID("claude-code", sid), &in
	}
	return "", nil
}

func runDaemon(c *client.Client, a args) {
	switch a.arg(0, "start|stop|status") {
	case "start":
		if h, err := c.HealthInfo(); err == nil {
			fmt.Printf("이미 실행 중: %s (pid %d, 데이터 %s)\n", c.Base, h.Pid, h.Root)
			return
		}
		check(daemon.Ensure(c))
		h, _ := c.HealthInfo()
		fmt.Printf("데몬 시작: %s (pid %d, 데이터 %s)\n", c.Base, h.Pid, h.Root)
	case "stop":
		check(daemon.Stop(c))
		fmt.Println("데몬 중지")
	case "status":
		if h, err := c.HealthInfo(); err == nil {
			fmt.Printf("실행 중: %s (pid %d, 데이터 %s)\n", c.Base, h.Pid, h.Root)
		} else {
			fmt.Printf("실행 중 아님 (%s)\n", c.Base)
		}
		fmt.Println("로그인 시 자동 시작:", orText(daemon.Autostart(), "꺼짐"))
	default:
		fail("bad_arg", "start | stop | status")
	}
}

// listen 은 나에게 온 메시지, 초대, 참여 신청, 방 변화를 한 줄씩 출력하며 계속 실행된다.
// Claude Code 의 Monitor 도구가 줄마다 세션을 깨우도록 만든 명령이다. 메시지를 읽음 처리하지는 않는다.
func listen(c *client.Client, id string) error {
	release, err := daemon.TryLock(daemon.ListenLockFile(id))
	if err != nil {
		fmt.Println("[ATalk] 이 세션의 listen 이 이미 실행 중입니다. 기존 알림을 계속 쓰면 됩니다.")
		return nil
	}
	defer release()
	if _, err := c.Agent(id); err != nil {
		return err
	}
	fmt.Printf("[ATalk] %s 알림 대기 시작\n", id)

	trig := make(chan struct{}, 1)
	poke := func() {
		select {
		case trig <- struct{}{}:
		default:
		}
	}
	ctx := context.Background()
	go func() {
		for {
			_ = c.SSE(ctx, "/agents/"+id+"/stream", func(string, []byte) { poke() })
			time.Sleep(3 * time.Second)
			if os.Getenv("ATALK_URL") == "" { // 설정에서 포트를 바꿨으면 새 주소로
				c.Base = client.DefaultURL()
			}
			_ = daemon.Ensure(c) // 데몬이 죽었으면 다시 띄운다
			poke()
		}
	}()
	go func() {
		for range time.Tick(30 * time.Second) {
			poke()
		}
	}()

	room, printed, first := "", 0, true
	seen := map[string]bool{}
	poke()
	for range trig {
		u, err := c.Unread(id, false)
		if err != nil {
			var ce *client.Error
			if errors.As(err, &ce) && ce.Status == 404 {
				fmt.Println("[ATalk] 이 에이전트가 등록 해제되어 listen 을 끝냅니다.")
				return nil
			}
			continue
		}
		r := ""
		if u.Room != nil {
			r = *u.Room
		}
		if r != room {
			if !first {
				if r == "" {
					fmt.Printf("[ATalk] %s 방에서 나왔습니다.\n", room)
				} else {
					fmt.Printf("[ATalk] %s 방에 들어왔습니다. %s\n", r, c.Purpose(r))
				}
			}
			room, printed = r, 0
		}
		first = false
		var names map[string]string
		for _, m := range u.Messages {
			if m.Seq <= printed {
				continue
			}
			printed = m.Seq
			if !m.ForMe {
				continue
			}
			if names == nil {
				names = c.Names()
			}
			from := m.From
			if n, ok := names[m.From]; ok {
				from = n + " (" + m.From + ")"
			}
			if m.Human != "" {
				from = m.Human + "@" + from + " [사람]"
			}
			fmt.Printf("[ATalk] %s #%d [%s] %s: %s  → atalk unread --ack 로 읽고 처리\n", room, m.Seq, m.Type, from, oneLine(m.Body, 160))
		}
		if p, err := c.Pending(id); err == nil {
			for _, i := range p.Invites {
				if k := "inv:" + i.Room + ":" + i.At; !seen[k] {
					seen[k] = true
					how := "사용자에게 알리고"
					if strings.HasSuffix(i.By, "(사람)") {
						how = "내 사용자가 화면에서 보낸 초대 → 바로"
					}
					fmt.Printf("[ATalk] 초대: %s %q (보낸 이 %s) → %s atalk accept %s %s\n", i.Room, i.Title, i.By, how, i.Room, c.Purpose(i.Room))
				}
			}
			for _, q := range p.Requests {
				if k := "req:" + q.Room + ":" + q.Agent + ":" + q.At; !seen[k] {
					seen[k] = true
					fmt.Printf("[ATalk] 참여 신청: %s → %s %q → 사용자에게 알리고 atalk approve %s\n", q.Agent, q.Room, q.Note, q.Agent)
				}
			}
		}
	}
	return nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
