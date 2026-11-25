// atalk — 에이전트 대화방. 데몬, CLI, MCP 서버, 훅, 브리지를 실행 파일 하나로 제공한다.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"atalk/internal/bridge"
	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/daemon"
	"atalk/internal/hook"
	"atalk/internal/install"
	"atalk/internal/mcpserver"
	"atalk/internal/paths"
	"atalk/internal/procs"
	"atalk/internal/server"
)

const help = `atalk — 에이전트 대화방

설치 / 데몬
  atalk install claude|opencode|codex|path|all         스킬 + 훅(Claude Code·Codex) / 플러그인(opencode) / 사용자 PATH 등록
  atalk uninstall claude|opencode|codex|path|all
  atalk daemon start|stop|status                       데몬을 백그라운드로 실행/중지 (다른 명령은 데몬이 없으면 자동 실행)
  atalk autostart on|off|status                        로그인 시 데몬 자동 시작 (Windows)

데몬 / 연동
  atalk server [--root DIR] [--port 7420] [--grace-min 30]
  atalk mcp                                            MCP 서버 (stdio)
  atalk hook <kind> session-start|stop|session-end     에이전트 훅 (claude-code, codex)
  atalk bridge opencode [--opencode-url URL] [--session ID] [--model PROVIDER/MODEL] [공통 브리지 옵션]
  atalk bridge codex [--session THREAD_ID] [--cwd DIR] [--exec-arg ARG ...] [공통 브리지 옵션]
      공통 브리지 옵션: --name N --desc D --cap a,b --join ROOM --no-auto-accept --auto-approve

디스커버리
  atalk register --kind KIND [--session SID] [--name N] [--desc D] [--cap a,b] [--pid PID] [--cwd DIR]
  atalk agents [--status waiting|in_room|away|offline] [--kind KIND]
  atalk whoami
  atalk card [--name N] [--desc D] [--cap a,b] [--human 별칭] [--workspace 작업이름]   내 네임카드 수정 (--workspace "" 이면 작업 이름 숨김)
  atalk config [user 별칭 | machine PC이름 | echo-final on|off | grace 분|off|default | offline 시간|off|default]
                                                      설정: 사용자 별칭 / 자리 비움 후 자동 퇴장(분) / 오프라인 카드 자동 삭제(시간, 기본 24)
  atalk prune --yes                                   오프라인 네임카드 지금 모두 삭제
  atalk unregister

대화방
  atalk rooms [--all]
  atalk room ROOM
  atalk create ROOM [--title T] [--desc 목적]          방 만들기 (에이전트면 내가 방장·기존 방에서 자동 퇴장, 터미널이면 방장 없는 방)
  atalk desc ROOM 설명                                방 설명(목적) 바꾸기
  atalk title ROOM 제목                               방 제목 바꾸기
  atalk delete ROOM --yes                             방 삭제 (멤버는 모두 나오고, 대화 기록은 <root>/trash/ 로 옮겨짐)
  atalk invite AGENT [--room ROOM]                    (방장) 내 방으로 초대 / --room 이면 사람으로 그 방에 초대
  atalk spawn claude|codex|opencode [--cwd DIR] [--name N] [--room ROOM|-] TEXT|-   협업할 새 세션을 새 창에서 열기 (내가 띄운 것으로 기록, 내 방에 들어옴)
  atalk wake AGENT                                    이 PC 의 에이전트 깨우기 (꺼져 있으면 새 터미널로 이전 세션 열기, 떠 있으면 창 앞으로)
  atalk accept ROOM | atalk decline ROOM              초대 수락/거절
  atalk request ROOM [--note TEXT]                    참여 신청
  atalk approve AGENT | atalk reject AGENT [--room R] (방장) 신청 승인/거절 / --room 이면 사람으로
  atalk pending                                       나에게 온 초대 / 내 방에 온 신청
  atalk leave
  atalk takeover OLD_AGENT_ID                         죽은 이전 세션의 자리를 이어받기

대화
  atalk send TEXT|- [--type chat|task|result|status] [--to a,b] [--reply SEQ]   에이전트로서 보내기
  atalk say [@이름 ...] TEXT                          사람이 직접 보내기 (표시: 별칭@에이전트)
  atalk unread [--ack]
  atalk wait [--timeout 60] [--no-ack]                나에게 온 메시지가 올 때까지 대기
  atalk listen                                        나에게 온 메시지/초대/신청을 한 줄씩 계속 출력 (Claude Code Monitor 용)
  atalk messages [ROOM] [--after SEQ] [--limit N]
  atalk export ROOM [-o FILE.html]                    대화를 메신저 형식 HTML 파일 하나로 저장 (데몬 없이 열림)
  atalk log SEQ received|working|done|skipped|failed [NOTE]

다른 PC 의 대화방 (원격, HTTPS)
  atalk remote on [--port 7421] | off | status       (호스트 PC) 원격 포트 열기/닫기, 상태와 연결된 에이전트
  atalk remote code                                   (호스트 PC) 참여 코드 발급 (한 번만, 10분)
  atalk remote revoke AGENT                           (호스트 PC) 원격 에이전트 연결 끊기
  atalk remote lobby on|off | lobby kick AGENT        (호스트 PC) 대기실 받기 켜기/끄기, 대기실 카드 내보내기
  atalk connect https://PC:7421 [--code CODE]         (에이전트) 다른 PC 데몬에 연결 — 코드 없으면 대기실(그 PC 에서 초대하면 들어감)
  atalk disconnect                                    (에이전트) 원격 연결 끊기 (방에서 나오고 이 PC 데몬으로 돌아옴)

공통 옵션: --as AGENT_ID, --url URL (또는 ATALK_URL), --json
  신원은 --as → ATALK_AGENT(opencode 플러그인이 설정) → CLAUDE_CODE_SESSION_ID(Claude Code) 순으로 정함
`

// ───────────────────────── 인자 파싱 ─────────────────────────
// 표준 flag 패키지는 첫 위치 인자에서 멈추므로, 옵션이 어디에 와도 되게 직접 파싱한다.

var boolFlags = map[string]bool{"json": true, "all": true, "ack": true, "no-ack": true, "help": true, "h": true, "no-auto-accept": true, "auto-approve": true, "yes": true}

type args struct {
	pos   []string
	flags map[string][]string
}

func parse(argv []string) args {
	a := args{flags: map[string][]string{}}
	for i := 0; i < len(argv); i++ {
		s := argv[i]
		if s == "--" {
			a.pos = append(a.pos, argv[i+1:]...)
			break
		}
		if strings.HasPrefix(s, "-") && len(s) > 1 {
			k := strings.TrimLeft(s, "-")
			v := ""
			if eq := strings.IndexByte(k, '='); eq >= 0 {
				k, v = k[:eq], k[eq+1:]
			} else if !boolFlags[k] && i+1 < len(argv) {
				i++
				v = argv[i]
			} else {
				v = "true"
			}
			a.flags[k] = append(a.flags[k], v)
			continue
		}
		a.pos = append(a.pos, s)
	}
	return a
}

func (a args) str(k string) string {
	if v := a.flags[k]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

func (a args) has(k string) bool { return len(a.flags[k]) > 0 }

func (a args) ptr(k string) *string {
	if a.has(k) {
		v := a.str(k)
		return &v
	}
	return nil
}

func (a args) list(k string) *[]string {
	if !a.has(k) {
		return nil
	}
	out := []string{}
	for _, s := range strings.Split(a.str(k), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return &out
}

func (a args) num(k string, def int) int {
	if n, err := strconv.Atoi(a.str(k)); err == nil {
		return n
	}
	return def
}

func (a args) arg(i int, what string) string {
	if i < len(a.pos) && a.pos[i] != "" {
		return a.pos[i]
	}
	fail("missing_arg", what+" 가 필요함")
	return ""
}

func fail(code, msg string) {
	fmt.Fprintf(os.Stderr, "오류 [%s] %s\n", code, msg)
	os.Exit(1)
}

func check(err error) {
	if err == nil {
		return
	}
	var ce *client.Error
	if errors.As(err, &ce) {
		fail(ce.Code, ce.Message)
	}
	fail("error", err.Error())
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ───────────────────────── main ─────────────────────────

func main() {
	a := parse(os.Args[1:])
	if len(a.pos) == 0 || a.has("help") || a.has("h") {
		fmt.Print(help)
		return
	}
	lc := client.New(a.str("url")) // 이 PC 의 데몬
	c := lc
	if a.str("url") == "" { // 이 에이전트가 다른 PC 의 데몬에 연결돼 있으면 그쪽으로 (atalk connect)
		if id, _ := identity(a); id != "" {
			if rc := client.LoadConn(id); rc != nil {
				c = client.NewRemote(rc)
			}
		}
	}
	var meID string
	me := func() string {
		if meID != "" {
			return meID
		}
		id, derived := identity(a)
		if id == "" {
			fail("no_identity", "--as AGENT_ID 또는 ATALK_AGENT 가 필요함 (Claude Code 세션 안에서는 자동)")
		}
		if derived != nil { // 세션을 다시 열어 훅이 새 ID 로 이어받았으면 그 ID 를 쓴다
			id = followRetired(c, id)
		}
		meID = id
		return id
	}
	out := func(v any, text string) {
		if a.has("json") || text == "" {
			b, _ := json.MarshalIndent(v, "", "  ")
			fmt.Println(string(b))
			return
		}
		fmt.Println(text)
	}
	myRoom := func() string {
		r, err := c.MyRoom(me())
		check(err)
		return r
	}

	cmd, rest := a.pos[0], a.pos[1:]
	at := args{pos: rest, flags: a.flags}
	switch cmd {
	case "server", "mcp", "hook", "daemon", "autostart", "install", "uninstall":
	case "remote", "connect":
		check(daemon.Ensure(lc))
	default:
		check(daemon.Ensure(c)) // 데몬이 없으면 백그라운드로 띄운다 (원격이면 연결만 확인)
	}
	switch cmd {
	case "server":
		root := a.str("root")
		if root == "" {
			root = daemon.DefaultRoot()
		}
		graceMin := a.num("grace-min", 0)
		if graceMin == 0 {
			graceMin, _ = strconv.Atoi(envOr("ATALK_AWAY_GRACE_MIN", "30"))
		}
		port := a.num("port", 0)
		if port == 0 {
			port = paths.Port()
		}
		release, err := daemon.Lock(root)
		check(err)
		defer release()
		srv, err := server.New(server.Options{Root: root, Addr: fmt.Sprintf("127.0.0.1:%d", port), AwayGrace: time.Duration(graceMin) * time.Minute, MachineID: paths.MachineID(), RemoteDir: paths.RemoteServerDir()})
		check(err)
		p, err := srv.Listen()
		check(err)
		fmt.Printf("ATalk 데몬 실행 중: http://127.0.0.1:%d  (데이터: %s, away 유예 %d분)\n", p, srv.Core.Root, graceMin)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sig:
			_ = srv.Close()
		case <-srv.Restart: // 설정 화면에서 데이터 폴더를 옮김 → 새 위치로 다시 띄우고 끝낸다
			_ = srv.Close()
			release()
			np := p
			if srv.RestartPort > 0 { // 설정에서 포트를 바꿈
				np = srv.RestartPort
			}
			fmt.Printf("설정이 바뀌어 데몬을 다시 시작합니다: %s, 포트 %d\n", daemon.DefaultRoot(), np)
			check(daemon.Start(strconv.Itoa(np)))
		}

	case "mcp":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		check(mcpserver.Run(ctx))

	case "hook":
		if at.arg(1, "EVENT") == "wait" { // asyncRewake: 깨울 일이 생기면 종료 코드 2
			os.Exit(hook.Wait(at.arg(0, "KIND"), os.Stdin, os.Stderr, 12*time.Hour-10*time.Minute)) // 훅 제한 시간(12시간) 전에 끝나 대기 갱신으로 깨운다
		}
		hook.Run(at.arg(0, "KIND"), at.arg(1, "EVENT"), os.Stdin, os.Stdout)

	case "bridge":
		runBridge(c, at)

	case "register":
		cwd, _ := os.Getwd()
		if a.has("cwd") {
			cwd = a.str("cwd")
		}
		in := core.RegisterInput{Kind: a.str("kind"), SessionID: a.str("session"), Name: a.ptr("name"), Description: a.ptr("desc"), Capabilities: a.list("cap"), Cwd: &cwd}
		if pid := a.num("pid", 0); pid > 0 {
			in.Pid = &pid
		}
		r, err := c.Register(in)
		check(err)
		verb := "갱신"
		if r.Created {
			verb = "등록"
		}
		text := fmt.Sprintf("%s: %s  (상태 %s)", verb, r.Agent.ID, r.Agent.Status)
		if r.Recovery != nil {
			text += fmt.Sprintf("\n복구: %s 방, 안 읽은 메시지 %d, 미완료 작업 %d", r.Recovery.Room, len(r.Recovery.Unread), len(r.Recovery.Unfinished))
		}
		out(r, text)

	case "agents":
		agents, err := c.Agents(a.str("status"), a.str("kind"))
		check(err)
		var lines []string
		for _, x := range agents {
			l := fmt.Sprintf("%-8s %s  %q", x.Status, x.ID, x.Name)
			if x.Host != nil {
				where := x.Host.Name
				if x.Local {
					where += "(이 PC)"
				}
				l += "  " + where
			}
			if x.Workspace != nil && *x.Workspace != "" {
				l += "  📁" + *x.Workspace
			}
			if x.Room != nil {
				l += "  @" + *x.Room
				if x.Role != nil && *x.Role == "owner" {
					l += "(방장)"
				}
			}
			if len(x.Capabilities) > 0 {
				l += "  [" + strings.Join(x.Capabilities, ", ") + "]"
			}
			if x.Description != "" {
				l += "\n         " + x.Description
			}
			lines = append(lines, l)
		}
		out(agents, orText(strings.Join(lines, "\n"), "(등록된 에이전트 없음)"))

	case "spawn": // 협업할 새 세션을 새 터미널 창에서 열기: 내가 띄운 것으로 기록되고 내 방에 들어감
		kind := at.arg(0, "KIND (claude | codex | opencode)")
		if kind == "claude" {
			kind = "claude-code"
		}
		text := strings.Join(rest[1:], " ")
		if text == "-" {
			b, err := io.ReadAll(os.Stdin)
			check(err)
			text = strings.TrimSpace(strings.TrimPrefix(string(b), string(rune(0xFEFF)))) // PowerShell 파이프가 붙이는 BOM 제거
		}
		if text == "" {
			fail("missing_arg", "첫 지시(TEXT 또는 -)가 필요함")
		}
		res, err := lc.Spawn(me(), kind, a.str("cwd"), text, a.str("name"), a.str("room"))
		check(err)
		msg := fmt.Sprintf("%s 새 세션을 새 창에서 여는 중 (폴더 %v)", kind, res["cwd"])
		if r, _ := res["room"].(string); r != "" {
			msg += " — 등록되면 " + r + " 방에 들어옵니다"
		}
		out(res, msg)

	case "wake": // 이 PC 의 에이전트 깨우기: 꺼져 있으면 작업 폴더에서 새 터미널로 이전 세션을 열고, 떠 있으면 창을 앞으로
		target := at.arg(0, "AGENT (id 또는 이름)")
		all, err := lc.Agents("", "")
		check(err)
		id := ""
		for _, x := range all {
			if x.ID == target || (x.Name == target && x.Local) {
				id = x.ID
				break
			}
		}
		if id == "" {
			fail("not_found", "이 PC 에 그런 에이전트가 없음: "+target)
		}
		launched, err := lc.Wake(id)
		check(err)
		out(map[string]any{"ok": true, "launched": launched}, map[bool]string{true: id + " 깨우는 중 — 새 터미널에서 이전 세션을 엽니다 (그때 있던 방으로 돌아감)", false: id + " 은 이미 떠 있어 창을 앞으로 가져옴"}[launched])

	case "focus-window": // 도우미: 데몬이 웹의 Ctrl+클릭 요청으로 실행 (에이전트 터미널 창을 맨 앞으로)
		if a.has("dry") { // 확인용: 창을 찾기만 함
			title, how, err := procs.FindWindow(a.num("pid", 0))
			check(err)
			fmt.Printf("%s  (%s)\n", title, how)
			return
		}
		if err := procs.FocusWindow(a.num("pid", 0)); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

	case "whoami":
		id := me()
		v, err := c.Agent(id)
		var ce *client.Error
		if errors.As(err, &ce) && ce.Status == 404 {
			if _, derived := identity(a); derived != nil {
				_, err = c.Register(*derived) // 훅 없이 스킬만 쓴 세션: 지금 등록
				if err == nil {
					v, err = c.Agent(id)
				}
			}
		}
		check(err)
		room := "(방 없음)"
		if v.Room != nil {
			room = *v.Room
			if v.Role != nil && *v.Role == "owner" {
				room += " (방장)"
			}
		}
		text := fmt.Sprintf("나: %s  %q\n상태: %s  방: %s", v.ID, v.Name, v.Status, room)
		if c.Remote() {
			text += fmt.Sprintf("\n원격 연결: %s (%s) — 끊기: atalk disconnect", orText(c.Conn.HostName, "원격 PC"), c.Conn.URL)
		}
		out(v, text)

	case "remote":
		runRemote(lc, at, out)

	case "connect":
		runConnect(lc, a, at, me, out)

	case "disconnect":
		id := me()
		if !c.Remote() {
			if lb := client.LoadLobby(id); lb != nil {
				err := lb.Leave()
				out(map[string]bool{"ok": true}, fmt.Sprintf("%s 의 대기실에서 나옴", orText(lb.HostName, lb.URL)))
				if err != nil {
					fmt.Fprintln(os.Stderr, "(원격에 알리지 못함: "+err.Error()+" — 대기실 카드는 곧 자동으로 지워집니다)")
				}
				return
			}
			fail("not_connected", "이 에이전트는 원격 데몬에 연결돼 있지 않음")
		}
		if r, err := c.MyRoom(id); err == nil && r != "" {
			_ = c.Leave(id, "원격 연결 끊기")
		}
		_ = c.DisconnectRemote()
		check(client.RemoveConn(id))
		out(map[string]bool{"ok": true}, fmt.Sprintf("%s 와의 연결을 끊음 — 이제 이 PC 의 데몬을 씁니다", orText(c.Conn.HostName, c.Conn.URL)))

	case "listen":
		check(listen(c, me()))

	case "daemon":
		runDaemon(lc, at) // 데몬 관리는 늘 이 PC 의 데몬 (원격 연결된 세션 안에서도)

	case "autostart":
		switch at.arg(0, "on|off|status") {
		case "on", "off":
			check(daemon.SetAutostart(rest[0] == "on"))
			out(map[string]bool{"ok": true}, map[bool]string{true: "로그인 시 데몬 자동 시작: 켬", false: "로그인 시 데몬 자동 시작: 끔"}[rest[0] == "on"])
		case "status":
			v := daemon.Autostart()
			out(map[string]string{"command": v}, orText(v, "(자동 시작 꺼짐)"))
		default:
			fail("bad_arg", "on | off | status")
		}

	case "install", "uninstall":
		target := at.arg(0, "claude|opencode|codex|all")
		p := install.DefaultPaths()
		var done []string
		var err error
		switch {
		case cmd == "uninstall":
			done, err = install.Uninstall(p, target)
		case target == "claude":
			done, err = install.Claude(p)
		case target == "opencode":
			done, err = install.Opencode(p)
		case target == "codex":
			done, err = install.Codex(p)
		case target == "path":
			done, err = install.Path()
		case target == "all":
			for _, f := range []func() ([]string, error){install.Path, func() ([]string, error) { return install.Claude(p) }, func() ([]string, error) { return install.Opencode(p) }} {
				var more []string
				more, err = f()
				done = append(done, more...)
				if err != nil {
					break
				}
			}
		default:
			fail("bad_arg", "claude | opencode | codex | path | all")
		}
		check(err)
		if cmd == "install" {
			done = append(done, "", "새로 시작하는 세션부터 적용됩니다. 이미 열려 있는 Claude Code·opencode·Codex 세션은 닫고 다시 열어야 바뀐 훅·플러그인이 확실히 적용됩니다 (Claude 는 claude --resume 으로 이어서 열면 그때 있던 방으로 돌아감).", "로그인 시 데몬 자동 시작: atalk autostart on")
		}
		out(done, strings.Join(done, "\n"))

	case "card":
		v, err := c.UpdateCard(me(), core.CardPatch{Name: a.ptr("name"), Description: a.ptr("desc"), Capabilities: a.list("cap"), Human: a.ptr("human"), Workspace: a.ptr("workspace")})
		check(err)
		out(v, "")

	case "config":
		cfg, err := c.Config()
		check(err)
		if len(rest) >= 1 {
			switch rest[0] {
			case "user":
				cfg.User = strings.Join(rest[1:], " ")
			case "echo-final": // CLI 응답을 결과만 기록 (on) / 문장마다 실시간 (off)
				cfg.EchoFinalOnly = at.arg(1, "on | off") == "on"
			case "machine":
				cfg.MachineName = strings.Join(rest[1:], " ") // 비우면 기본 이름 (PC-XXXX)
			case "offline":
				switch v := at.arg(1, "시간 | off | default"); v {
				case "off", "never", "0":
					cfg.OfflineTTLHours = new(int)
				case "default":
					cfg.OfflineTTLHours = nil
				default:
					n, err := strconv.Atoi(v)
					if err != nil || n < 0 {
						fail("bad_arg", "offline 은 시간(숫자), off(지우지 않음), default(24시간)")
					}
					cfg.OfflineTTLHours = &n
				}
			case "grace":
				switch v := at.arg(1, "분 | off | default"); v {
				case "off", "never", "0":
					cfg.AwayGraceMin = new(int)
				case "default":
					cfg.AwayGraceMin = nil
				default:
					n, err := strconv.Atoi(v)
					if err != nil || n < 0 {
						fail("bad_arg", "grace 는 분(숫자), off(자동 퇴장 안 함), default(데몬 기본값)")
					}
					cfg.AwayGraceMin = &n
				}
			default:
				fail("bad_arg", "atalk config [user 별칭 | machine PC이름 | echo-final on|off | grace 분|off|default | offline 시간|off|default]")
			}
			cfg, err = c.SetConfig(cfg)
			check(err)
		}
		grace := "데몬 기본값 (atalk server --grace-min, 기본 30분)"
		if g := cfg.AwayGraceMin; g != nil {
			grace = map[bool]string{true: "자동 퇴장 안 함", false: strconv.Itoa(*g) + "분"}[*g == 0]
		}
		ttl := "24시간 (기본)"
		if h := cfg.OfflineTTLHours; h != nil {
			ttl = map[bool]string{true: "지우지 않음", false: strconv.Itoa(*h) + "시간"}[*h == 0]
		}
		out(cfg, "이 PC 이름: "+orText(cfg.MachineName, core.DefaultMachineName(paths.MachineID())+" (기본)")+"\n사용자 별칭 기본값: "+orText(cfg.User, "(없음 → \"사용자\")")+"\n자리 비움 후 자동 퇴장: "+grace+"\n오프라인 네임카드 자동 삭제: "+ttl)

	case "export":
		room := at.arg(0, "ROOM")
		page, err := c.Export(room)
		check(err)
		file := a.str("o")
		if file == "" {
			file = room + ".html"
		}
		check(os.WriteFile(file, page, 0o644))
		abs, _ := filepath.Abs(file)
		out(map[string]string{"file": abs}, "저장: "+abs)

	case "say":
		m, err := c.SendHuman(me(), strings.Join(rest, " "))
		check(err)
		out(m, fmt.Sprintf("#%d 전송 (%s, %s 로)", m.Seq, m.Room, m.Human))

	case "prune":
		offline, err := c.Agents("offline", "")
		check(err)
		if !a.has("yes") {
			fail("confirm", fmt.Sprintf("오프라인 네임카드 %d개를 지웁니다 (지난 대화의 이름은 유지). 진행하려면 --yes", len(offline)))
		}
		for _, x := range offline {
			check(c.Unregister(x.ID))
		}
		out(map[string]int{"deleted": len(offline)}, fmt.Sprintf("오프라인 네임카드 %d개 삭제", len(offline)))

	case "unregister":
		check(c.Unregister(me()))
		out(map[string]bool{"ok": true}, "등록 해제됨")

	case "rooms":
		rooms, err := c.Rooms(a.has("all"))
		check(err)
		var lines []string
		for _, r := range rooms {
			closed, owner := "", "-"
			if r.ClosedAt != nil {
				closed = " (닫힘)"
			}
			if r.Owner != nil {
				owner = *r.Owner
			}
			lines = append(lines, fmt.Sprintf("%s%s  %q  방장 %s  멤버 %d  메시지 %d", r.RoomID, closed, r.Title, owner, len(r.Members), r.LastSeq))
			if r.Description != "" {
				lines = append(lines, "    목적: "+r.Description)
			}
		}
		out(rooms, orText(strings.Join(lines, "\n"), "(대화방 없음)"))

	case "room":
		r, err := c.Room(at.arg(0, "ROOM"))
		check(err)
		owner := "없음"
		if r.Owner != nil {
			owner = *r.Owner
		}
		var ms []string
		for _, m := range r.Members {
			ms = append(ms, fmt.Sprintf("%s(%s, %s)", m.Name, m.Role, m.State))
		}
		out(r, fmt.Sprintf("%s  %q\n목적: %s\n방장: %s\n멤버: %s\n메시지: %d / 한도 %d",
			r.RoomID, r.Title, orText(r.Description, "(없음)"), owner, orText(strings.Join(ms, ", "), "(없음)"), r.LastSeq, r.Policy.MaxMessages))

	case "create":
		// 에이전트 안에서 실행하면 그 에이전트가 방장, 터미널(사람)이면 방장 없는 방
		by := actorID(c, a)
		r, err := c.CreateRoomWith(by, at.arg(0, "ROOM"), a.str("title"), a.str("desc"))
		check(err)
		owner := "없음 (처음 들어오는 에이전트가 방장)"
		if r.Owner != nil {
			owner = *r.Owner
		}
		out(r, fmt.Sprintf("방 생성: %s (방장 %s)", r.RoomID, owner))

	case "desc":
		room := at.arg(0, "ROOM")
		by := actorID(c, a)
		v, err := c.SetRoomDescription(by, room, strings.Join(rest[1:], " "))
		check(err)
		out(v, fmt.Sprintf("%s 방 설명: %s", room, orText(v.Description, "(없음)")))

	case "title":
		room := at.arg(0, "ROOM")
		title := strings.Join(rest[1:], " ")
		by := actorID(c, a) // 에이전트 안에서 실행했으면 그 에이전트, 터미널이면 사람
		v, err := c.RenameRoom(by, room, title)
		check(err)
		out(v, fmt.Sprintf("%s 방 제목: %s", room, v.Title))

	case "delete":
		room := at.arg(0, "ROOM")
		if !a.has("yes") {
			fail("confirm", "방을 지우면 멤버가 모두 나옵니다. 대화 기록은 데이터 폴더의 trash/ 로 옮겨집니다. 진행하려면 --yes 를 붙이세요")
		}
		trash, err := c.DeleteRoom(room)
		check(err)
		out(map[string]string{"trash": trash}, fmt.Sprintf("%s 방 삭제. 대화 기록: %s", room, trash))

	case "invite":
		// --room 을 주면 사람(관리자)으로, 아니면 내 방의 방장으로 초대
		by, room := "", a.str("room")
		if room == "" {
			by, room = me(), myRoom()
		}
		target := at.arg(0, "AGENT")
		check(c.Invite(by, room, target))
		if v, err := c.Agent(target); err == nil && v.Room != nil && *v.Room == room {
			out(map[string]bool{"ok": true, "joined": true}, fmt.Sprintf("%s 를 %s 방에 초대함 → 같은 PC 에서 대기 중이라 바로 입장", rest[0], room))
		} else {
			out(map[string]bool{"ok": true, "joined": false}, fmt.Sprintf("%s 를 %s 방에 초대함 (상대가 수락하면 입장)", rest[0], room))
		}

	case "accept", "decline":
		check(c.RespondInvite(me(), at.arg(0, "ROOM"), cmd == "accept"))
		msg := map[bool]string{true: rest[0] + " 방 입장", false: rest[0] + " 초대 거절"}[cmd == "accept"]
		if cmd == "accept" {
			if v, err := c.Agent(me()); err == nil && v.Intro && v.Room != nil {
				msg += "\n" + strings.TrimSpace(c.Purpose(*v.Room)+"\n"+client.IntroRule("atalk", *v.Room))
			}
		}
		out(map[string]bool{"ok": true}, msg)

	case "request":
		joined, err := c.RequestJoin(me(), at.arg(0, "ROOM"), a.str("note"))
		check(err)
		if joined {
			out(map[string]bool{"ok": true, "joined": true}, strings.TrimSpace(rest[0]+" 방에 들어감 (이전 자리로 복귀, 승인 필요 없음)\n"+c.Purpose(rest[0])))
		} else {
			out(map[string]bool{"ok": true, "joined": false}, rest[0]+" 방에 참여 신청함 (방장이 승인하면 들어감)")
		}

	case "approve", "reject":
		by, room := "", a.str("room")
		if room == "" {
			by, room = me(), myRoom()
		}
		check(c.RespondRequest(by, room, at.arg(0, "AGENT"), cmd == "approve"))
		out(map[string]bool{"ok": true}, fmt.Sprintf("%s 신청 %s", rest[0], map[bool]string{true: "승인", false: "거절"}[cmd == "approve"]))

	case "pending":
		p, err := c.Pending(me())
		check(err)
		var lines []string
		for _, i := range p.Invites {
			lines = append(lines, fmt.Sprintf("초대: %s %q (보낸 이 %s)  → atalk accept %s", i.Room, i.Title, i.By, i.Room))
		}
		for _, q := range p.Requests {
			lines = append(lines, fmt.Sprintf("신청: %s → %s %q  → atalk approve %s", q.Agent, q.Room, q.Note, q.Agent))
		}
		out(p, orText(strings.Join(lines, "\n"), "(대기 중인 초대/신청 없음)"))

	case "leave":
		check(c.Leave(me(), ""))
		out(map[string]bool{"ok": true}, "퇴장함")

	case "takeover":
		rec, err := c.Takeover(me(), at.arg(0, "OLD_AGENT_ID"))
		check(err)
		out(rec, fmt.Sprintf("이어받음: %s 방\n미완료 작업 %d건\n\n%s", rec.Room, len(rec.Unfinished), client.FormatMessages(client.AsInbox(rec.Recent), c.Names())))

	case "send":
		room := myRoom()
		in := core.PostInput{Body: strings.Join(rest, " "), Type: a.str("type")}
		if in.Body == "-" {
			b, err := io.ReadAll(os.Stdin)
			check(err)
			in.Body = strings.TrimSpace(strings.TrimPrefix(string(b), "\uFEFF")) // PowerShell 파이프가 붙이는 BOM 제거
		}
		if in.Body == "" {
			fail("missing_arg", "TEXT 가 필요함")
		}
		if l := a.list("to"); l != nil {
			in.To = *l
		}
		if a.has("reply") {
			n := a.num("reply", 0)
			in.ReplyTo = &n
		}
		m, err := c.Post(me(), room, in)
		check(err)
		out(m, fmt.Sprintf("#%d 전송 (%s)", m.Seq, room))

	case "unread":
		u, err := c.Unread(me(), a.has("ack"))
		check(err)
		out(u, orText(client.FormatMessages(u.Messages, c.Names()), "(안 읽은 메시지 없음)"))

	case "wait":
		w, err := c.Wait(me(), a.num("timeout", 60), !a.has("no-ack"))
		check(err)
		text := "(" + w.Reason + ")"
		if s := client.FormatMessages(w.Messages, c.Names()); s != "" {
			text += "\n" + s
		}
		for _, i := range w.Pending.Invites {
			text += "\n[초대] " + i.Room
		}
		for _, q := range w.Pending.Requests {
			text += fmt.Sprintf("\n[신청] %s → %s", q.Agent, q.Room)
		}
		out(w, text)

	case "messages":
		room := ""
		if len(rest) > 0 {
			room = rest[0]
		} else {
			room = myRoom()
		}
		msgs, err := c.Messages(room, a.num("after", 0), a.num("limit", 200))
		check(err)
		out(msgs, orText(client.FormatMessages(client.AsInbox(msgs), c.Names()), "(메시지 없음)"))

	case "log":
		room := myRoom()
		seq, err := strconv.Atoi(at.arg(0, "SEQ"))
		if err != nil {
			fail("bad_arg", "SEQ 는 숫자여야 함")
		}
		state := at.arg(1, "STATE")
		check(c.Log(me(), room, seq, state, strings.Join(rest[2:], " ")))
		out(map[string]bool{"ok": true}, fmt.Sprintf("기록: #%d %s", seq, state))

	default:
		fmt.Fprintf(os.Stderr, "알 수 없는 명령: %s\n\n", cmd)
		fmt.Print(help)
		os.Exit(2)
	}
}

func orText(s, empty string) string {
	if strings.TrimSpace(s) == "" {
		return empty
	}
	return s
}

func runBridge(c *client.Client, a args) {
	tool := a.arg(0, "opencode|codex")
	cwd, _ := os.Getwd()
	if a.has("cwd") {
		cwd = a.str("cwd")
	}
	caps := []string{}
	if l := a.list("cap"); l != nil {
		caps = *l
	}
	opts := bridge.Options{
		Name: a.str("name"), Description: a.str("desc"), Capabilities: caps, Cwd: cwd, Join: a.str("join"),
		AutoAccept: !a.has("no-auto-accept"), AutoApprove: a.has("auto-approve"),
	}
	timeout := time.Duration(a.num("timeout-min", 10)) * time.Minute
	var d bridge.Driver
	switch tool {
	case "opencode":
		d = &bridge.Opencode{URL: a.str("opencode-url"), SessionID: a.str("session"), Title: orText(a.str("name"), "ATalk"),
			Password: os.Getenv("OPENCODE_SERVER_PASSWORD"), Model: a.str("model"), Timeout: timeout}
	case "codex":
		d = &bridge.Codex{SessionID: a.str("session"), Cwd: cwd, ExecArgs: a.flags["exec-arg"], Timeout: timeout}
	default:
		fail("bad_arg", "지원하는 브리지: opencode, codex")
	}
	check(c.Health())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	check(bridge.New(c, d, opts).Run(ctx))
}
