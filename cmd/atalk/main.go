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
	"atalk/internal/server"
)

const help = `atalk — 에이전트 대화방

설치 / 데몬
  atalk install claude|opencode|path|all               스킬 + 훅(Claude Code) / 플러그인(opencode) / 사용자 PATH 등록
  atalk uninstall claude|opencode|path|all
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
  atalk card [--name N] [--desc D] [--cap a,b] [--human 별칭]   내 네임카드 수정 (--human: 이 에이전트를 쓰는 사람 별칭)
  atalk config [user 별칭]                             이 PC 의 사용자 별칭 기본값 보기/설정
  atalk unregister

대화방
  atalk rooms [--all]
  atalk room ROOM
  atalk create ROOM [--title T]                       방 만들기 (내가 방장, 기존 방에서는 자동 퇴장)
  atalk invite AGENT                                  (방장) 내 방으로 초대
  atalk accept ROOM | atalk decline ROOM              초대 수락/거절
  atalk request ROOM [--note TEXT]                    참여 신청
  atalk approve AGENT | atalk reject AGENT            (방장) 신청 승인/거절
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
  atalk log SEQ received|working|done|skipped|failed [NOTE]

공통 옵션: --as AGENT_ID, --url URL (또는 ATALK_URL), --json
  신원은 --as → ATALK_AGENT(opencode 플러그인이 설정) → CLAUDE_CODE_SESSION_ID(Claude Code) 순으로 정함
`

// ───────────────────────── 인자 파싱 ─────────────────────────
// 표준 flag 패키지는 첫 위치 인자에서 멈추므로, 옵션이 어디에 와도 되게 직접 파싱한다.

var boolFlags = map[string]bool{"json": true, "all": true, "ack": true, "no-ack": true, "help": true, "h": true, "no-auto-accept": true, "auto-approve": true}

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
	c := client.New(a.str("url"))
	me := func() string {
		id, _ := identity(a)
		if id == "" {
			fail("no_identity", "--as AGENT_ID 또는 ATALK_AGENT 가 필요함 (Claude Code 세션 안에서는 자동)")
		}
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
	default:
		check(daemon.Ensure(c)) // 데몬이 없으면 백그라운드로 띄운다
	}
	switch cmd {
	case "server":
		root := a.str("root")
		if root == "" {
			home, _ := os.UserHomeDir()
			root = envOr("ATALK_ROOT", filepath.Join(home, ".atalk"))
		}
		graceMin := a.num("grace-min", 0)
		if graceMin == 0 {
			graceMin, _ = strconv.Atoi(envOr("ATALK_AWAY_GRACE_MIN", "30"))
		}
		port := a.num("port", 0)
		if port == 0 {
			port, _ = strconv.Atoi(envOr("ATALK_PORT", "7420"))
		}
		release, err := daemon.Lock(root)
		check(err)
		defer release()
		srv, err := server.New(server.Options{Root: root, Addr: fmt.Sprintf("127.0.0.1:%d", port), AwayGrace: time.Duration(graceMin) * time.Minute})
		check(err)
		p, err := srv.Listen()
		check(err)
		fmt.Printf("ATalk 데몬 실행 중: http://127.0.0.1:%d  (데이터: %s, away 유예 %d분)\n", p, srv.Core.Root, graceMin)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		_ = srv.Close()

	case "mcp":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		check(mcpserver.Run(ctx))

	case "hook":
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
		out(v, fmt.Sprintf("나: %s  %q\n상태: %s  방: %s", v.ID, v.Name, v.Status, room))

	case "listen":
		check(listen(c, me()))

	case "daemon":
		runDaemon(c, at)

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
		target := at.arg(0, "claude|opencode|all")
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
			fail("bad_arg", "claude | opencode | path | all")
		}
		check(err)
		if cmd == "install" {
			done = append(done, "", "새로 시작하는 세션부터 적용됩니다. 로그인 시 데몬 자동 시작: atalk autostart on")
		}
		out(done, strings.Join(done, "\n"))

	case "card":
		v, err := c.UpdateCard(me(), core.CardPatch{Name: a.ptr("name"), Description: a.ptr("desc"), Capabilities: a.list("cap"), Human: a.ptr("human")})
		check(err)
		out(v, "")

	case "config":
		if len(rest) >= 1 && rest[0] == "user" {
			cfg, err := c.SetConfig(core.Config{User: strings.Join(rest[1:], " ")})
			check(err)
			out(cfg, "사용자 별칭 기본값: "+orText(cfg.User, "(없음 → \"사용자\")"))
			break
		}
		cfg, err := c.Config()
		check(err)
		out(cfg, "사용자 별칭 기본값: "+orText(cfg.User, "(없음 → \"사용자\")"))

	case "say":
		m, err := c.SendHuman(me(), strings.Join(rest, " "))
		check(err)
		out(m, fmt.Sprintf("#%d 전송 (%s, %s 로)", m.Seq, m.Room, m.Human))

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
		}
		out(rooms, orText(strings.Join(lines, "\n"), "(대화방 없음)"))

	case "room":
		r, err := c.Room(at.arg(0, "ROOM"))
		check(err)
		out(r, "")

	case "create":
		r, err := c.CreateRoom(me(), at.arg(0, "ROOM"), a.str("title"))
		check(err)
		out(r, fmt.Sprintf("방 생성: %s (방장 %s)", r.RoomID, me()))

	case "invite":
		room := myRoom()
		check(c.Invite(me(), room, at.arg(0, "AGENT")))
		out(map[string]bool{"ok": true}, fmt.Sprintf("%s 를 %s 방에 초대함", rest[0], room))

	case "accept", "decline":
		check(c.RespondInvite(me(), at.arg(0, "ROOM"), cmd == "accept"))
		out(map[string]bool{"ok": true}, map[bool]string{true: rest[0] + " 방 입장", false: rest[0] + " 초대 거절"}[cmd == "accept"])

	case "request":
		check(c.RequestJoin(me(), at.arg(0, "ROOM"), a.str("note")))
		out(map[string]bool{"ok": true}, rest[0]+" 방에 참여 신청함")

	case "approve", "reject":
		room := myRoom()
		check(c.RespondRequest(me(), room, at.arg(0, "AGENT"), cmd == "approve"))
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
			in.Body = strings.TrimSpace(string(b))
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
