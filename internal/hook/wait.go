package hook

// wait: Claude Code 의 asyncRewake 훅용. 세션이 시작될 때 백그라운드로 돌다가
// 이 세션 앞으로 처리할 메시지(또는 초대)가 생기면 내용을 stderr 에 쓰고 종료 코드 2 로 끝나
// 쉬고 있는 Claude 를 깨운다. SessionStart 와 Stop(응답을 마칠 때마다) 에 걸리므로, Monitor 로 listen 을 켜 둘 필요가 없다
// (Monitor 는 30분마다 만료·재시작하며 Claude 를 깨워 토큰을 쓴다. 이 대기는 메시지가 올 때만 깨운다).
//
//	settings.json:
//	"SessionStart": [{"hooks": [{"type": "command", "command": "atalk hook claude-code wait", "async": true, "asyncRewake": true}]}]

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/daemon"
	"atalk/internal/install"
)

// Wait 는 깨울 일이 생기면 2, 아니면 0 을 돌려준다 (프로세스 종료 코드).
func Wait(kind string, stdin io.Reader, stderr io.Writer, maxWait time.Duration) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return 0
	}
	var in Input
	if json.Unmarshal(raw, &in) != nil || in.SessionID == "" || !core.IsValidKind(kind) {
		return 0
	}
	id := core.MakeAgentID(kind, in.SessionID)
	exe := "atalk"
	if !install.InUserPath(install.PathDir()) {
		exe = install.Exe()
	}
	seen := map[string]bool{}
	startRoom, known := "", false // 시작할 때 있던 방 (복구는 SessionStart 안내가 맡으므로, 그 뒤 새로 들어간 방만 알린다)
	baseline := false             // 시작할 때 이미 있던 메시지·초대·신청은 Stop 훅이 처리하므로 그 뒤 새로 온 것만 깨운다
	// 대기는 Claude 가 응답을 마칠 때마다(Stop) 새로 걸린다. 한 세션에 하나만 살아 있게, 새 대기가 시작되면 이전 대기는 끝난다.
	genFile := filepath.Join(os.TempDir(), "atalk-wait-"+id+".gen")
	gen := strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
	_ = os.WriteFile(genFile, []byte(gen), 0o644)
	for deadline := time.Now().Add(maxWait); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if b, err := os.ReadFile(genFile); err == nil && string(b) != gen {
			return 0 // 더 새 대기가 이어받음
		}
		if daemon.ListenRunning(id) { // Claude 가 listen 을 켜 두었으면 그쪽이 알림을 맡는다
			return 0
		}
		c := client.New("") // 포트가 바뀌었을 수 있으니 매번 주소를 다시 읽는다
		if c.Health() != nil {
			continue
		}
		var lines []string
		if a, err := c.Agent(id); err == nil {
			room := ""
			if a.Room != nil {
				room = *a.Room
			}
			if !known {
				startRoom, known = room, true
			} else if room != "" && room != startRoom && !seen["room:"+room] {
				// 같은 PC 에서 대기 중이라 초대와 함께 자동으로 들어감 → 깨워서 알린다
				seen["room:"+room] = true
				lines = append(lines, fmt.Sprintf("[ATalk] 초대를 받아 %s 방에 들어왔습니다 (같은 PC 에서 대기 중이라 자동 입장).", room))
				if p := c.Purpose(room); p != "" {
					lines = append(lines, p)
				}
			}
		}
		if u, err := c.Unread(id, false); err == nil {
			var forMe []core.InboxMessage
			for _, m := range u.Messages {
				if k := fmt.Sprintf("msg:%s:%d", m.Room, m.Seq); m.ForMe && !seen[k] {
					seen[k] = true
					if baseline {
						forMe = append(forMe, m)
					}
				}
			}
			if len(forMe) > 0 && u.Room != nil {
				lines = append(lines, fmt.Sprintf("[ATalk] %s 방에서 나에게 온 메시지 %d개:", *u.Room, len(forMe)))
				if p := c.Purpose(*u.Room); p != "" {
					lines = append(lines, p)
				}
				lines = append(lines, client.FormatMessages(forMe, c.Names()))
			}
		}
		if p, err := c.Pending(id); err == nil {
			for _, i := range p.Invites {
				if k := "inv:" + i.Room + ":" + i.At; !seen[k] {
					seen[k] = true
					if !baseline {
						continue
					}
					how := "사용자에게 알리고, 사용자가 원하면"
					if strings.HasSuffix(i.By, "(사람)") {
						how = "내 사용자가 화면에서 보낸 초대이므로 바로"
					}
					lines = append(lines, fmt.Sprintf("[ATalk 초대] %s %q (보낸 이 %s) — %s `%s accept %s`", i.Room, i.Title, i.By, how, exe, i.Room))
					if p := c.Purpose(i.Room); p != "" {
						lines = append(lines, p)
					}
				}
			}
			for _, q := range p.Requests {
				if k := "req:" + q.Room + ":" + q.Agent + ":" + q.At; !seen[k] {
					seen[k] = true
					if !baseline {
						continue
					}
					lines = append(lines, fmt.Sprintf("[ATalk 참여 신청] %s → %s — 사용자에게 알리고, 사용자가 원하면 `%s approve %s`", q.Agent, q.Room, exe, q.Agent))
				}
			}
		}
		baseline = true
		if len(lines) == 0 {
			continue
		}
		lines = append(lines, "",
			"atalk 스킬의 \"알림을 받았을 때\" 규칙대로 처리하세요: unread --ack → log SEQ received → 작업 → send - --type result --reply SEQ --to 보낸이 → log SEQ done.",
			"(메시지 알림은 자동입니다 — 응답을 마칠 때마다 다시 기다립니다. atalk listen 은 켜지 않아도 됩니다.)",
			client.HumanRule)
		fmt.Fprintln(stderr, strings.Join(lines, "\n"))
		return 2
	}
	return 0
}
