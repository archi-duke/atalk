package server

// 헤드리스 (창 없이 도는 에이전트, 지금은 Claude Code): 카드에 Headless 가 표시된 세션에 나에게 온 메시지가 오면
// 데몬이 그 세션으로 한 턴을 숨김 실행한다 (claude -p --resume <세션>, 지시는 표준 입력으로). 그 턴 안에서 훅이 그대로 돌아
// 처리 기록·CLI 출력 기록·결과 보고가 창이 있을 때와 같다. 한 세션은 한 곳에서만 돈다: 헤드리스 턴이 도는 동안에는
// CLI 로 열지 않고(409), CLI 창이 떠 있는 세션은 헤드리스가 아니다(창이 등록하면 Headless 가 풀림).

import (
	"encoding/json"
	"errors"
	"io"
	"slices"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"atalk/internal/client"
	"atalk/internal/core"
	"atalk/internal/daemon"
	"atalk/internal/procs"
)

const headlessMaxText = 20000

type headlessRunner struct {
	mu       sync.Mutex
	running  map[string]bool // 에이전트 id → 턴 실행 중
	spawning map[string]int  // 작업 폴더 → 헤드리스로 띄우는 중 (등록 전이라 id 를 모름)
	lastSeq  map[string]int  // 에이전트 id → 마지막으로 넘긴 메시지 번호 (처리하지 못해도 같은 메시지로 다시 돌지 않게)
	toCLI    map[string]bool // 턴이 도는 중에 [CLI로 열기]를 누름 → 턴이 끝나면 연다
	cmds     map[string]*exec.Cmd // 에이전트 id → 도는 턴의 프로세스 (급할 때 중단)
	stopped  map[string]bool      // 사람이 중단시킨 턴
	resume   map[string]string    // 중단하고 창을 열 때 첫 입력 (이어서 하게 할 때)
}

func (h *headlessRunner) init() {
	if h.running == nil {
		h.running, h.spawning, h.lastSeq, h.toCLI = map[string]bool{}, map[string]int{}, map[string]int{}, map[string]bool{}
		h.cmds, h.stopped, h.resume = map[string]*exec.Cmd{}, map[string]bool{}, map[string]string{}
	}
}

func (h *headlessRunner) isRunning(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	return h.running[id]
}

// headlessLog 는 헤드리스 턴의 출력이 쌓이는 파일 (작업 보기·문제 확인용).
func headlessLog(id string) string {
	return filepath.Join(os.TempDir(), "atalk-headless", strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(id)+".log")
}

// HeadlessKinds 는 숨김(헤드리스)으로 돌릴 수 있는 에이전트 종류.
var HeadlessKinds = []string{"claude-code", "codex", "opencode"}

// headlessCmd 는 종류별로 창 없이 한 턴을 실행하는 명령. sid 가 있으면 그 세션을 이어서, 지시는 표준 입력(또는 인자)으로.
// mode 는 권한 모드 (Claude: --permission-mode 그대로 / Codex: 샌드박스 범위로 / opencode: 모두 허용이면 --auto).
func headlessCmd(kind, sid, prompt, mode string) (*exec.Cmd, error) {
	switch kind {
	case "claude-code":
		exe, err := exec.LookPath("claude")
		if err != nil {
			return nil, errors.New("claude 를 찾을 수 없음: " + err.Error())
		}
		args := []string{"-p"}
		if mode != "" { // 창이 없어 허가를 물을 수 없음 — 설정한 권한 모드로
			args = append(args, "--permission-mode", mode)
		}
		if sid != "" {
			args = append(args, "--resume", sid)
		}
		cmd := exec.Command(exe, args...)
		cmd.Stdin = strings.NewReader(prompt)
		return cmd, nil
	case "codex":
		// codex exec 은 실행 중에 사람에게 묻지 않는다 → 정한 범위(샌드박스) 안에서만. 막히면 에이전트가 방에 묻고(atalk ask) 턴을 끝낸다
		args := []string{"exec", "--skip-git-repo-check"} // 작업 폴더가 git 저장소가 아니어도 (창으로 열 때는 이미 신뢰한 폴더)
		switch mode {
		case "bypassPermissions":
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		case "plan":
			args = append(args, "-s", "read-only")
		default:
			args = append(args, "-s", "workspace-write")
		}
		if sid != "" {
			args = append(args, "resume", sid)
		}
		args = append(args, "-") // 지시는 표준 입력으로
		cmd, err := procs.CodexCommand(args...)
		if err != nil {
			return nil, errors.New("codex 를 찾을 수 없음: " + err.Error())
		}
		cmd.Stdin = strings.NewReader(prompt)
		return cmd, nil
	case "opencode":
		exe, err := exec.LookPath("opencode")
		if err != nil {
			return nil, errors.New("opencode 를 찾을 수 없음: " + err.Error())
		}
		// npm 의 opencode.cmd 를 거치면 지시문이 cmd.exe 로 해석된다 → 같은 폴더의 실제 실행 파일을 직접
		if ext := strings.ToLower(filepath.Ext(exe)); ext == ".cmd" || ext == ".bat" {
			real := filepath.Join(filepath.Dir(exe), "node_modules", "opencode-ai", "bin", "opencode.exe")
			if _, err := os.Stat(real); err != nil {
				return nil, errors.New("opencode 실행 파일을 찾지 못함 (" + real + ")")
			}
			exe = real
		}
		args := []string{"run"}
		if sid != "" {
			args = append(args, "--session", sid)
		}
		if mode == "bypassPermissions" {
			args = append(args, "--auto")
		}
		return exec.Command(exe, append(args, prompt)...), nil
	}
	return nil, errors.New(kind + " 는 숨김(헤드리스)을 지원하지 않음")
}

// runTurn 은 cwd 에서 그 종류의 한 턴을 숨김 실행하고 끝날 때까지 기다린다. sid 가 있으면 그 세션을 이어서.
// started 가 있으면 프로세스를 띄운 직후 부른다 (중단할 수 있게).
func runTurn(kind, cwd, sid, prompt, logID, mode string, env map[string]string, started func(*exec.Cmd)) error {
	cmd, err := headlessCmd(kind, sid, prompt, mode)
	if err != nil {
		return err
	}
	args := cmd.Args[1:]
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "ATALK_HEADLESS=1")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	daemon.HideCmd(cmd)
	_ = os.MkdirAll(filepath.Dir(headlessLog(logID)), 0o755)
	f, err := os.OpenFile(headlessLog(logID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(f, "\n===== %s 턴 시작 (%s)\n", time.Now().Format(time.RFC3339), strings.Join(args, " "))
	cmd.Stdout, cmd.Stderr = f, f
	if err = cmd.Start(); err == nil {
		if started != nil {
			started(cmd)
		}
		err = cmd.Wait()
	}
	fmt.Fprintf(f, "===== %s 턴 끝 (%v)\n", time.Now().Format(time.RFC3339), err)
	return err
}

// runHeadless 는 이 PC 의 헤드리스 에이전트에 새로 온 메시지가 있으면 한 턴을 돌린다 (watchLobbies 가 3초마다 부름).
func (s *Server) runHeadless() {
	list, err := s.Core.ListAgents("", "")
	if err != nil {
		return
	}
	for _, v := range list {
		if !slices.Contains(HeadlessKinds, v.Kind) || !v.Local || !v.Headless || v.Cwd == nil || !procs.SafeSession(v.SessionID) {
			continue
		}
		if v.Pid != nil && core.DefaultIsAlive(*v.Pid) { // CLI 창이 떠 있으면 그 창이 받는다 (같은 세션이 두 곳에서 돌지 않게)
			continue
		}
		var c *client.Client
		if rc := client.LoadConn(v.ID); rc != nil {
			c = s.relayClients.get(rc)
			a, err := c.Agent(v.ID)
			if err != nil {
				continue
			}
			v.Busy, v.Room = a.Busy, a.Room
		} else {
			c = client.New("")
		}
		if v.Busy || v.Room == nil {
			continue
		}
		s.headless.mu.Lock()
		s.headless.init()
		skip := s.headless.running[v.ID] || s.headless.spawning[*v.Cwd] > 0
		last := s.headless.lastSeq[v.ID]
		s.headless.mu.Unlock()
		if skip {
			continue
		}
		u, err := c.Unread(v.ID, false)
		if err != nil {
			continue
		}
		var forMe []core.InboxMessage
		top := last
		for _, m := range u.Messages {
			if m.ForMe && m.Seq > last {
				forMe = append(forMe, m)
				top = max(top, m.Seq)
			}
		}
		if len(forMe) == 0 {
			continue
		}
		room := *v.Room
		if u.Room != nil {
			room = *u.Room
		}
		text := c.Notice(room, forMe, nil, false)
		if r := []rune(text); len(r) > headlessMaxText {
			text = string(r[:headlessMaxText]) + "\n… (길어서 줄임: atalk unread 로 전체를 보세요)"
		}
		s.headless.mu.Lock()
		s.headless.running[v.ID], s.headless.lastSeq[v.ID] = true, top
		s.headless.mu.Unlock()
		_ = c.Delivered(v.ID, room, client.ForMeSeqs(forMe))
		// 넘긴 메시지는 읽은 것으로 (턴 안에서 읽음 처리를 빠뜨리면 턴 끝 훅이 같은 메시지를 다시 알림)
		read := 0
		for _, m := range u.Messages {
			read = max(read, m.Seq)
		}
		_ = c.Ack(v.ID, read)
		log.Printf("헤드리스: %s 에 메시지 %d개로 한 턴 실행", v.ID, len(forMe))
		mode := s.modeFor(v)
		go func(kind, id, cwd, sid string) {
			err := runTurn(kind, cwd, sid, text, id, mode, nil, func(cmd *exec.Cmd) {
				s.headless.mu.Lock()
				s.headless.cmds[id] = cmd
				s.headless.mu.Unlock()
			})
			s.headless.mu.Lock()
			delete(s.headless.running, id)
			delete(s.headless.cmds, id)
			stopped := s.headless.stopped[id]
			delete(s.headless.stopped, id)
			open := s.headless.toCLI[id]
			delete(s.headless.toCLI, id)
			resume := s.headless.resume[id]
			delete(s.headless.resume, id)
			s.headless.mu.Unlock()
			if err != nil && !stopped {
				log.Printf("헤드리스 턴 실패 (%s): %v", id, err)
			}
			if stopped { // 사람이 중단함: 끝 훅이 오지 않으므로 작업 중 표시를 여기서 푼다
				log.Printf("헤드리스: %s 의 턴을 사람이 중단함", id)
				_ = c.Activity(id, false)
			}
			if open { // 턴이 도는 동안 [CLI로 열기]를 눌렀음
				if err := s.openCLI(id, resume); err != nil {
					log.Printf("헤드리스 → CLI 실패 (%s): %v", id, err)
				}
			}
		}(v.Kind, v.ID, *v.Cwd, v.SessionID)
	}
}

// spawnHeadless 는 atalk spawn --headless: 창 없이 새 세션의 첫 턴을 실행한다 (등록·방 입장은 표식을 보고 훅이).
func (s *Server) spawnHeadless(kind, cwd, prompt, token string) {
	s.headless.mu.Lock()
	s.headless.init()
	s.headless.spawning[cwd]++
	s.headless.mu.Unlock()
	go func() {
		if err := runTurn(kind, cwd, "", prompt, "spawn-"+token, s.headlessMode(), map[string]string{"ATALK_SPAWN": token}, nil); err != nil {
			log.Printf("헤드리스 띄우기 실패 (%s): %v", cwd, err)
		}
		s.headless.mu.Lock()
		s.headless.spawning[cwd]--
		s.headless.mu.Unlock()
	}()
}

// setHeadless 는 POST /agents/{id}/headless {on}: 켜면 CLI 창을 닫고 헤드리스로, 끄면 CLI 창으로 다시 연다.
func (s *Server) setHeadless(r *http.Request) (any, error) {
	var b struct {
		On     bool `json:"on"`
		Force  bool `json:"force"`  // 보이기: 도는 턴을 기다리지 않고 중단한 뒤 바로 연다
		Resume bool `json:"resume"` // force 와 함께: 창을 연 뒤 중단된 작업을 이어서 하게 한다
	}
	if err := readBody(r, &b); err != nil {
		return nil, err
	}
	id := r.PathValue("id")
	v, err := s.Core.GetAgent(id)
	if err != nil {
		return nil, err
	}
	if !v.Local {
		return nil, &core.Error{Status: 409, Code: "not_here", Message: "이 PC 의 에이전트만 바꿀 수 있음 (다른 PC 는 그 PC 에서)"}
	}
	if !slices.Contains(HeadlessKinds, v.Kind) {
		return nil, &core.Error{Status: 409, Code: "headless_unsupported", Message: "숨기기는 Claude Code · Codex · opencode 만 지원함"}
	}
	if v.Cwd == nil || !procs.SafeSession(v.SessionID) {
		return nil, &core.Error{Status: 409, Code: "no_cwd", Message: "작업 폴더·세션을 몰라 바꿀 수 없음"}
	}
	if b.On {
		if v.Headless {
			return map[string]any{"agent": v}, nil
		}
		if v.Busy {
			return nil, &core.Error{Status: 409, Code: "busy", Message: "일하는 중 — 끝난 뒤에 바꾸세요"}
		}
		if v.Pid != nil && core.DefaultIsAlive(*v.Pid) {
			_ = procs.CloseAgentWindow(*v.Pid)
		}
		killWaitHook(id)
		if v.HeadlessMode == "" && v.Kind == "claude-code" { // CLI 에서 쓰던 권한 모드를 그대로 이어받는다 (에이전트 목록에서 바꿀 수 있음)
			if m := lastPermissionMode(v.SessionID); m != "" {
				_, _ = s.Core.SetHeadlessMode(id, m)
			}
		}
		a, err := s.Core.SetHeadless(id, true)
		return map[string]any{"agent": a}, err
	}
	if !v.Headless {
		return map[string]any{"agent": v, "launched": false}, nil
	}
	s.headless.mu.Lock()
	s.headless.init()
	running := s.headless.running[id]
	if running { // 턴이 도는 중 → 그 턴이 끝나면 연다 (같은 세션이 두 곳에서 돌지 않게)
		s.headless.toCLI[id] = true
		if b.Force && b.Resume {
			s.headless.resume[id] = ResumePrompt
		}
	}
	s.headless.mu.Unlock()
	if running && b.Force {
		if err := s.stopTurn(id); err != nil {
			return nil, err
		}
		return map[string]any{"launched": false, "stopping": true}, nil
	}
	if running {
		return map[string]any{"launched": false, "queued": true}, nil
	}
	if v.Busy {
		return nil, &core.Error{Status: 409, Code: "busy", Message: "헤드리스 턴이 도는 중 — 끝난 뒤에 다시 하세요"}
	}
	if err := s.openCLI(id, ""); err != nil {
		return nil, err
	}
	return map[string]any{"launched": true}, nil
}

// ResumePrompt 는 중단하고 창을 열 때 이어서 하게 하는 첫 입력.
const ResumePrompt = "사용자가 작업 중이던 숨김 턴을 강제로 중단하고 이 창을 열었습니다. 중단되기 전까지 한 일을 확인하고 (도구 실행이 중간에 끊겼을 수 있음) 하던 작업을 이어서 진행하세요."

// stopTurn 은 도는 헤드리스 턴을 중단한다 (프로세스와 그 아래 도구 실행까지). 끝난 뒤 처리는 턴을 돌린 쪽이 한다.
func (s *Server) stopTurn(id string) error {
	s.headless.mu.Lock()
	s.headless.init()
	cmd := s.headless.cmds[id]
	if cmd != nil {
		s.headless.stopped[id] = true
	}
	s.headless.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return &core.Error{Status: 409, Code: "not_running", Message: "도는 숨김 턴이 없음"}
	}
	if err := procs.KillTree(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

// stopHeadless 는 POST /agents/{id}/stop: 도는 숨김 턴만 중단한다 (숨김은 그대로 — 다음 메시지가 오면 다시 돈다).
func (s *Server) stopHeadless(r *http.Request) (any, error) {
	id := r.PathValue("id")
	v, err := s.Core.GetAgent(id)
	if err != nil {
		return nil, err
	}
	if !v.Local {
		return nil, &core.Error{Status: 409, Code: "not_here", Message: "이 PC 의 에이전트만 중단할 수 있음 (다른 PC 는 그 PC 에서)"}
	}
	return map[string]any{"stopping": true}, s.stopTurn(id)
}

// openCLI 는 헤드리스 에이전트를 CLI 창으로 다시 연다 (claude --resume <세션>).
// prompt 가 있으면 첫 입력으로 넣는다 (Claude Code·Codex).
func (s *Server) openCLI(id, prompt string) error {
	v, err := s.Core.GetAgent(id)
	if err != nil {
		return err
	}
	if v.Cwd == nil || !procs.SafeSession(v.SessionID) {
		return &core.Error{Status: 409, Code: "no_cwd", Message: "작업 폴더·세션을 몰라 열 수 없음"}
	}
	if _, err := s.Core.SetHeadless(id, false); err != nil {
		return err
	}
	argv, _ := procs.ResumeCommand(v.Kind, v.SessionID)
	if prompt != "" {
		switch v.Kind {
		case "claude-code":
			argv = append(argv, prompt)
		case "codex": // 마지막 인자가 깨우기 입력
			argv[len(argv)-1] = prompt
		}
	}
	if err := procs.LaunchTerminal(*v.Cwd, argv); err != nil {
		_, _ = s.Core.SetHeadless(id, true)
		return &core.Error{Status: 500, Code: "launch_failed", Message: "터미널을 열 수 없음: " + err.Error()}
	}
	return nil
}

// killWaitHook 은 창을 닫은 세션의 메시지 대기 훅을 끝낸다 (헤드리스로 바꾸면 그 대기는 쓸모가 없음).
func killWaitHook(id string) {
	f := filepath.Join(os.TempDir(), "atalk-wait-"+id+".gen")
	b, err := os.ReadFile(f)
	if err != nil {
		return
	}
	str := strings.TrimSpace(string(b))
	if pid, err := strconv.Atoi(str[strings.LastIndex(str, "-")+1:]); err == nil && pid > 0 && core.DefaultIsAlive(pid) {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
	_ = os.WriteFile(f, []byte("headless-0"), 0o644)
}

// headlessMode 는 설정의 헤드리스 권한 모드 ("" 이면 Claude Code 설정을 따름).
func (s *Server) headlessMode() string {
	cfg, _ := s.Core.GetConfig()
	return cfg.HeadlessPermissionMode
}

// modeFor 는 헤드리스 턴의 권한 모드: 카드에 정한 모드 → 대화 기록의 마지막 모드(이어받기) → 설정.
func (s *Server) modeFor(v core.AgentView) string {
	if v.HeadlessMode != "" {
		return v.HeadlessMode
	}
	if v.Kind == "claude-code" {
		if m := lastPermissionMode(v.SessionID); m != "" {
			return m
		}
	}
	return s.headlessMode()
}

// lastPermissionMode 는 Claude Code 대화 기록에 남은 마지막 권한 모드 (CLI 에서 Shift+Tab 으로 바꾼 것 포함). 없으면 "".
func lastPermissionMode(sid string) string {
	tp := claudeTranscript(sid)
	if tp == "" {
		return ""
	}
	f, err := os.Open(tp)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 2 << 20
	if st, err := f.Stat(); err == nil && st.Size() > tail {
		_, _ = f.Seek(st.Size()-tail, 0)
	}
	b, _ := io.ReadAll(f)
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.Contains(lines[i], `"permission-mode"`) {
			continue
		}
		var e struct {
			Type string `json:"type"`
			Mode string `json:"permissionMode"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(lines[i])), &e) == nil && e.Type == "permission-mode" && slices.Contains(core.HeadlessModes, e.Mode) {
			return e.Mode
		}
	}
	return ""
}
