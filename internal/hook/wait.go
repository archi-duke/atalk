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
	var c *client.Client
	connKey := ""
	inRoom := false // 마지막으로 본 상태가 방 안인지 (대기 갱신 여부)
	for deadline := time.Now().Add(maxWait); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if b, err := os.ReadFile(genFile); err == nil && string(b) != gen {
			return 0 // 더 새 대기가 이어받음
		}
		if daemon.ListenRunning(id) { // Claude 가 listen 을 켜 두었으면 그쪽이 알림을 맡는다
			return 0
		}
		// 포트가 바뀌었거나 원격에 연결했을 수 있으니 매번 다시 읽는다. 원격 연결은 TLS 연결을 다시 맺지 않게 그대로 쓴다
		if rc := client.LoadConn(id); rc == nil {
			c, connKey = client.New(""), ""
		} else if k := rc.URL + "\n" + rc.Token; c == nil || k != connKey {
			c, connKey = client.NewRemote(rc), k
		}
		if c.Health() != nil {
			continue
		}
		flush(c, id, 0) // 데몬이 없을 때 쌓인 CLI 기록을 보낸다 (outbox.go)
		var lines []string
		intro := "" // 새로 들어와 아직 인사·자기소개 전인 방
		if a, err := c.Agent(id); err == nil {
			if a.Intro && a.Room != nil {
				intro = *a.Room
			}
			room := ""
			inRoom = a.Room != nil
			if a.Room != nil {
				room = *a.Room
			}
			if !known {
				startRoom, known = room, true
			} else if room != "" && room != startRoom && !seen["room:"+room] {
				// 방에 들어감 → 깨워서 알린다. 이유는 여기서 알 수 없다: 같은 PC 초대 자동 입장(초대는 바로 사라짐)일 수도,
				// 내 참여 신청이 승인된 것일 수도 있으므로 단정하지 않는다
				seen["room:"+room] = true
				lines = append(lines, fmt.Sprintf("[ATalk] %s 방에 들어왔습니다 (참여 신청이 승인됐거나, 대기 중에 받은 초대로 자동 입장).", room))
				if p := c.Purpose(room); p != "" {
					lines = append(lines, p)
				}
			}
		}
		if u, err := c.UnreadWait(id); err == nil {
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
				_ = c.Delivered(id, *u.Room, client.ForMeSeqs(forMe)) // 깨우면서 전달 → "received"
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
		if intro != "" {
			lines = append(lines, "", client.IntroRule(exe, intro))
		}
		lines = append(lines, "",
			"atalk 스킬의 \"알림을 받았을 때\" 규칙대로 처리하세요: unread --ack → log SEQ received → 작업 → send - --type result --reply SEQ --to 보낸이 → log SEQ done.",
			"(메시지 알림은 자동입니다 — 응답을 마칠 때마다 다시 기다립니다. atalk listen 은 켜지 않아도 됩니다.)",
			client.ShowRule,
			client.HumanRule)
		fmt.Fprintln(stderr, strings.Join(lines, "\n"))
		return 2
	}
	// 제한 시간이 다 됨: 그냥 끝나면 Claude Code 를 깨울 길이 없어져(🔕) 방에서 메시지가 와도 먹통이 된다.
	// 방에 있으면 짧은 갱신 턴으로 한 번 깨운다 — 그 턴이 끝나면(Stop) 대기가 새로 걸린다. 비용: 제한 시간마다 짧은 턴 하나.
	if b, err := os.ReadFile(genFile); inRoom && err == nil && string(b) == gen && !daemon.ListenRunning(id) {
		fmt.Fprintln(stderr, KeepaliveNotice+" 메시지 대기 시간이 다 되어 대기를 다시 겁니다. 새 메시지는 없습니다 — 아무 도구도 쓰지 말고 \"대기 갱신\" 한 마디로 응답을 끝내세요.")
		return 2
	}
	return 0
}

// KeepaliveNotice 는 대기 갱신 턴을 여는 알림의 머리말 (응답 기록의 턴 경계로도 쓴다).
const KeepaliveNotice = "[ATalk 대기 갱신]"
