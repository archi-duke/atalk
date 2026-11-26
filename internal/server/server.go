// Package server 는 ATalk 데몬의 HTTP 계층이다. REST + SSE + long-poll wait.
// API 는 docs/DESIGN.md 6장과 TypeScript 구현(ts/src/server.ts)과 동일하다.
package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"atalk/internal/core"
)

//go:embed viewer.html
var viewerHTML []byte

// iconFS 는 아이콘 고르기 창의 후보 이미지 (이 저장소에서 그린 SVG). 고르면 화면에서 128px 이미지로 바꿔 저장한다.
//
//go:embed icons/*.svg
var iconFS embed.FS

type Options struct {
	Root       string
	Addr       string // 기본 127.0.0.1:7420
	AwayGrace  time.Duration
	SweepEvery time.Duration
	MachineID  string // 이 PC 의 ID (paths.MachineID). 없으면 core 가 임시로 만든다
	// RemoteDir 는 원격 포트의 인증서·설정 폴더 (paths.RemoteServerDir). 없으면 원격 기능을 쓰지 않는다.
	RemoteDir string
}

type Server struct {
	Core   *core.Atalk
	http   *http.Server
	ln     net.Listener
	stop   chan struct{}
	stopMu sync.Once
	// Restart 는 데이터 폴더를 옮긴 뒤 데몬을 새 위치로 다시 띄워야 할 때 신호를 받는다 (main 이 처리).
	Restart chan struct{}
	// RestartPort 가 0 이 아니면 다시 시작할 때 이 포트로 (설정에서 포트 변경)
	RestartPort int

	remoteDir    string
	remote       remoteState  // 원격 포트 (remote.go)
	relayClients relayClients // 중계 (relay.go)
	codex        CodexWaker      // Codex 깨우기 (codex_watch.go)
	revive       claudeReviver   // 대기가 끊긴 Claude 되살리기 (claude_revive.go)
	headless     headlessRunner  // 창 없이 도는 에이전트 (headless.go)
}

func New(opts Options) (*Server, error) {
	c, err := core.New(core.Options{Root: opts.Root, AwayGrace: opts.AwayGrace, MachineID: opts.MachineID})
	if err != nil {
		return nil, err
	}
	s := &Server{Core: c, stop: make(chan struct{}), Restart: make(chan struct{}, 1), remoteDir: opts.RemoteDir, codex: CodexWaker{Queue: codexQueue}}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:7420"
	}
	s.http = &http.Server{Addr: opts.Addr, Handler: localOnly(s.routes())}
	every := opts.SweepEvery
	if every == 0 {
		every = 10 * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := c.Sweep(); err != nil {
					log.Printf("sweep 실패: %v", err)
				}
			case <-s.stop:
				return
			}
		}
	}()
	return s, nil
}

// Listen 은 포트를 열고 실제 포트를 돌려준다 (Addr 의 포트가 0 이면 임의 포트).
func (s *Server) Listen() (int, error) {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return 0, err
	}
	s.ln = ln
	go func() {
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http: %v", err)
		}
	}()
	s.startRemoteFromSettings()
	if s.remoteDir != "" { // 실제 데몬: 이 PC 에이전트가 다른 PC 대기실에 있으면 확인 (lobby_watch.go)
		go s.watchLobbies(s.stop)
	}
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// ExportHTML 은 방 하나의 대화를 메신저 형식 HTML 파일 하나로 만든다.
func (s *Server) ExportHTML(roomID string) ([]byte, error) {
	c := s.Core
	room, err := c.GetRoom(roomID)
	if err != nil {
		return nil, err
	}
	msgs := []core.Message{}
	for after := 0; ; {
		page, err := c.ReadMessages(roomID, after, 1000)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, page...)
		if len(page) < 1000 {
			break
		}
		after = page[len(page)-1].Seq
	}
	logs, err := c.ReadLogs(roomID, "")
	if err != nil {
		return nil, err
	}
	all, err := c.ListAgents("", "")
	if err != nil {
		return nil, err
	}
	// 내보낸 파일은 남에게 건네기 쉬우므로 이 방과 관계있는 에이전트만, 경로·PID·PC·작업 이름 없이 넣는다
	related := map[string]bool{}
	for _, m := range room.Members {
		related[m.Agent] = true
	}
	for _, m := range msgs {
		related[m.From] = true
		for _, to := range m.To {
			related[to] = true
		}
	}
	for _, l := range logs {
		related[l.Agent] = true
	}
	agents := []core.AgentView{}
	for _, a := range all {
		if !related[a.ID] {
			continue
		}
		a.Cwd, a.Pid, a.Host, a.Workspace, a.WorkspaceID, a.Local, a.Lobby = nil, nil, nil, nil, "", false, nil
		agents = append(agents, a)
	}
	allNames, err := c.Names()
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for id, n := range allNames {
		if related[id] {
			names[id] = n
		}
	}
	data, err := json.Marshal(map[string]any{ // json.Marshal 은 < > & 를 \u003c 등으로 바꾸므로 <script> 안에 안전
		"room": room, "messages": msgs, "logs": logs, "agents": agents, "names": names,
		"exported_at": time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
	})
	if err != nil {
		return nil, err
	}
	tag := append(append([]byte("<script>window.ATALK_DATA="), data...), []byte(";</script>")...)
	return bytes.Replace(viewerHTML, []byte("<!--ATALK_DATA-->"), tag, 1), nil
}

func (s *Server) Close() error {
	s.stopMu.Do(func() { close(s.stop) })
	s.StopRemote()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.http.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		err = s.http.Close()
	}
	if cerr := s.Core.Close(); err == nil {
		err = cerr
	}
	return err
}

// ───────────────────────── 라우팅 ─────────────────────────

type handler func(w http.ResponseWriter, r *http.Request) (any, error)

// skip 은 핸들러가 응답을 직접 쓴 경우 (SSE, HTML).
type skip struct{}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			out, err := fn(w, r)
			if err != nil {
				writeErr(w, err)
				return
			}
			if _, ok := out.(skip); ok {
				return
			}
			writeOK(w, out)
		})
	}
	c := s.Core

	// ── 디스커버리 ──
	h("GET /health", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"root": c.Root, "pid": os.Getpid(), "machine": c.Machine()}, nil
	})
	h("GET /agents", func(w http.ResponseWriter, r *http.Request) (any, error) {
		a, err := c.ListAgents(r.URL.Query().Get("status"), r.URL.Query().Get("kind"))
		return map[string]any{"agents": a}, err
	})
	h("POST /agents/{id}/resume", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rec, from, err := c.ResumePrevious(r.PathValue("id"))
		return map[string]any{"recovery": rec, "from": from}, err
	})
	s.adminRoutes(h)
	s.remoteAdminRoutes(h)
	h("GET /names", func(w http.ResponseWriter, r *http.Request) (any, error) {
		names, err := c.Names()
		return map[string]any{"names": names}, err
	})
	h("GET /config", func(w http.ResponseWriter, r *http.Request) (any, error) {
		cfg, err := c.GetConfig()
		return map[string]any{"config": cfg}, err
	})
	h("PUT /config", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b core.Config
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			return nil, &core.Error{Status: 400, Code: "bad_json", Message: "JSON 본문이 필요함"}
		}
		cfg, err := c.SetConfig(b)
		return map[string]any{"config": cfg}, err
	})
	h("POST /agents", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in core.RegisterInput
		if err := readBody(r, &in); err != nil {
			return nil, err
		}
		return c.Register(in)
	})
	h("GET /agents/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		a, err := c.GetAgent(r.PathValue("id"))
		return map[string]any{"agent": a}, err
	})
	h("PATCH /agents/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var p core.CardPatch
		if err := readBody(r, &p); err != nil {
			return nil, err
		}
		a, err := c.UpdateCard(r.PathValue("id"), p)
		return map[string]any{"agent": a}, err
	})
	h("DELETE /agents/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return struct{}{}, c.Unregister(r.PathValue("id"))
	})
	// CLI 입력 기록: 사람이 에이전트 창에 입력한 말을 그 에이전트의 방에 기록만 한다 (방에 없거나 꺼져 있으면 recorded=false)
	h("POST /agents/{id}/echo", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Text  string   `json:"text"`
			Reply bool     `json:"reply"` // true 면 에이전트의 응답 (응답을 받는 중인 CLI 입력에 대한)
			Texts []string `json:"texts"` // 응답: 이번 턴에 지금까지 쓴 문장들 (새 것만 기록됨)
			Final *bool    `json:"final"` // 응답: 턴이 끝났음 (없으면 true)
			// 응답: 이번 턴을 시작한 사람 입력. 응답을 기다리는 CLI 입력과 다르면(대기열의 앞선 턴) 무시
			Prompt string `json:"prompt"`
			// EID 는 훅이 다시 보낼 때 같은 기록을 두 번 남기지 않기 위한 id (훅의 outbox)
			EID string `json:"eid"`
			// 응답: 이번 턴을 시작한 ATalk 알림의 메시지 번호들 — 사람 입력 없이 알림으로 깨어난 턴의 응답은 그 메시지의 답으로 기록
			Wake []int `json:"wake"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		id := r.PathValue("id")
		if !c.MarkEvent(id, b.EID) {
			return map[string]any{"recorded": false, "duplicate": true}, nil
		}
		if b.Reply {
			texts := b.Texts
			if len(texts) == 0 && b.Text != "" {
				texts = []string{b.Text}
			}
			if len(b.Wake) > 0 && b.Prompt == "" {
				ms, err := c.EchoWake(id, texts, b.Final == nil || *b.Final, b.Wake)
				return map[string]any{"recorded": len(ms) > 0, "messages": ms}, err
			}
			ms, err := c.EchoReply(r.PathValue("id"), texts, b.Final == nil || *b.Final, b.Prompt)
			return map[string]any{"recorded": len(ms) > 0, "messages": ms}, err
		}
		m, err := c.EchoPrompt(r.PathValue("id"), b.Text)
		return map[string]any{"recorded": m != nil, "message": m}, err
	})
	// CLI 창 앞으로: 이 PC 에서 돌고 있는 에이전트의 터미널 창을 맨 앞으로 올린다 (웹에서 Ctrl+클릭).
	// 창을 찾을 때 콘솔에 잠깐 붙어야 해서 데몬이 아니라 도우미 프로세스(atalk focus-window)가 한다.
	h("POST /agents/{id}/focus", func(w http.ResponseWriter, r *http.Request) (any, error) {
		v, err := c.GetAgent(r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		if !v.Local || v.Pid == nil || v.Status == "offline" {
			return nil, &core.Error{Status: 409, Code: "not_running_here", Message: "이 PC 에서 실행 중인 에이전트만 창을 앞으로 가져올 수 있음"}
		}
		if v.Shared {
			return nil, &core.Error{Status: 409, Code: "shared_server", Message: "이 Codex 세션은 공유 백그라운드 서버에서 실행 중이라 창을 찾을 수 없음 — codex --no-daemon 으로 열면 됨"}
		}
		return map[string]any{}, focusPid(*v.Pid)
	})
	// 깨우기: 꺼진(창을 닫은) 이 PC 에이전트를 그 작업 폴더에서 새 터미널로 다시 띄우고(이전 세션 이어서) 창을 앞으로.
	// 이미 떠 있으면 창만 앞으로. 이어서 열린 세션은 복귀 원칙대로 그때 있던 방으로 돌아간다. wake.go
	// 협업할 새 세션을 새 터미널 창에서 열기 (atalk spawn): 띄운 에이전트로 기록되고 그 방에 들어간다 (spawn.go)
	h("POST /spawn", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b SpawnInput
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		return s.spawn(b)
	})
	// 이미 등록된 세션이 spawn 표식을 씀 (opencode: 처음 일을 시작한 세션)
	h("POST /agents/{id}/spawn", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Token string `json:"token"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		a, prompt, err := c.ClaimSpawn(r.PathValue("id"), b.Token)
		if err == nil {
			a = s.linkRemoteSpawn(a) // 띄운 에이전트가 다른 PC 데몬에 연결돼 있으면 이 세션도 (opencode: 이후 중계가 그쪽으로)
		}
		return map[string]any{"agent": a, "prompt": prompt}, err
	})
	// 사람의 확인이 필요함 (도구 허가·질문): 대화방에 알리고 "확인 필요" 로 표시. text 가 "" 면 풀림
	h("POST /agents/{id}/need-input", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Text string `json:"text"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		return map[string]any{}, c.SetNeedInput(r.PathValue("id"), b.Text)
	})
	// 메시지를 에이전트에게 전달함(훅 알림·Codex 깨우기) → 그 에이전트 이름으로 "received" (이미 기록이 있으면 건너뜀)
	h("POST /agents/{id}/delivered", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Room string `json:"room"`
			Seqs []int  `json:"seqs"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		return map[string]any{}, c.LogDelivered(r.PathValue("id"), b.Room, b.Seqs)
	})
	h("POST /agents/{id}/headless", func(w http.ResponseWriter, r *http.Request) (any, error) { return s.setHeadless(r) })
	h("POST /agents/{id}/wake", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return s.wake(r.PathValue("id"))
	})
	// 임시 세션 확정: 사람이 이 세션에 처음 입력함 (이전 세션 이름표·자리를 그때 넘겨받는다)
	h("POST /agents/{id}/activate", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, c.Activate(r.PathValue("id"))
	})
	// 작업 중 표시: {busy} — 훅(Claude)·플러그인(opencode)이 일을 시작·끝낼 때
	h("POST /agents/{id}/activity", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Busy bool `json:"busy"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		return map[string]any{}, c.SetActivity(r.PathValue("id"), b.Busy)
	})
	h("POST /agents/{id}/away", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Reason string `json:"reason"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		if err := c.MarkAway(r.PathValue("id"), b.Reason); err != nil {
			return nil, err
		}
		a, err := c.GetAgent(r.PathValue("id"))
		return map[string]any{"agent": a}, err
	})
	h("POST /agents/{id}/leave", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Reason string `json:"reason"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		if err := c.Leave(r.PathValue("id"), b.Reason); err != nil {
			return nil, err
		}
		a, err := c.GetAgent(r.PathValue("id"))
		return map[string]any{"agent": a}, err
	})
	h("POST /agents/{id}/takeover", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			From string `json:"from"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		rec, err := c.Takeover(r.PathValue("id"), b.From)
		return map[string]any{"recovery": rec}, err
	})
	h("GET /agents/{id}/unread", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if boolQ(r, "wait") { // 메시지 대기 훅이 확인함 → 알림 켜짐
			c.MarkListening(r.PathValue("id"))
		}
		return c.Unread(r.PathValue("id"), boolQ(r, "ack"))
	})
	h("POST /agents/{id}/ack", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Seq int `json:"seq"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		cur, err := c.Ack(r.PathValue("id"), b.Seq)
		return map[string]any{"cursor": cur}, err
	})
	h("GET /agents/{id}/pending", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return c.Pending(r.PathValue("id"))
	})
	h("GET /agents/{id}/wait", s.wait)
	h("GET /agents/{id}/stream", func(w http.ResponseWriter, r *http.Request) (any, error) {
		id := r.PathValue("id")
		if _, err := c.GetAgent(id); err != nil {
			return nil, err
		}
		s.serveSSE(w, r, func(e core.Event) bool { return slices.Contains(e.To, id) })
		return skip{}, nil
	})

	// ── 대화방 ──
	h("GET /rooms", func(w http.ResponseWriter, r *http.Request) (any, error) {
		rooms, err := c.ListRooms(boolQ(r, "include_closed"))
		return map[string]any{"rooms": rooms}, err
	})
	h("POST /rooms", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent       string `json:"agent"`
			RoomID      string `json:"room_id"`
			Title       string `json:"title"`
			Description string `json:"description"`
			MaxMessages int    `json:"max_messages"`
		}
		if err := readBody(r, &b); err != nil { // agent 가 없으면 사람이 방장 없이 만든 방
			return nil, err
		}
		room, err := c.CreateRoomWith(b.Agent, b.RoomID, b.Title, b.Description, b.MaxMessages)
		return map[string]any{"room": room}, err
	})
	h("GET /rooms/{r}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		room, err := c.GetRoom(r.PathValue("r"))
		return map[string]any{"room": room}, err
	})
	h("PATCH /rooms/{r}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent       string  `json:"agent"` // 바꾼 에이전트 (사람이 화면에서 바꾸면 비움)
			Title       string  `json:"title"`
			Description *string `json:"description"`
			MaxMessages int     `json:"max_messages"`
			Icon        *string `json:"icon"` // 방 아이콘 ("" 이면 기본)
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		if b.Icon != nil {
			room, err := c.SetRoomIcon(r.PathValue("r"), *b.Icon)
			if err != nil || (b.Title == "" && b.Description == nil && b.MaxMessages == 0) {
				return map[string]any{"room": room}, err
			}
		}
		var room core.RoomView
		var err error
		if b.Description != nil {
			if room, err = c.SetRoomDescription(r.PathValue("r"), *b.Description, b.Agent); err != nil || (b.Title == "" && b.MaxMessages == 0) {
				return map[string]any{"room": room}, err
			}
		}
		if b.MaxMessages > 0 {
			if room, err = c.SetRoomLimit(r.PathValue("r"), b.MaxMessages); err != nil || b.Title == "" {
				return map[string]any{"room": room}, err
			}
		}
		room, err = c.RenameRoom(r.PathValue("r"), b.Title, b.Agent)
		return map[string]any{"room": room}, err
	})
	h("DELETE /rooms/{r}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		trash, err := c.DeleteRoom(r.PathValue("r"))
		return map[string]any{"trash": trash}, err
	})
	h("POST /rooms/{r}/invites", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct{ Agent, Target string }
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		room, err := c.Invite(b.Agent, r.PathValue("r"), b.Target)
		return map[string]any{"room": room}, err
	})
	h("POST /rooms/{r}/invites/respond", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent  string `json:"agent"`
			Accept *bool  `json:"accept"`
		}
		if err := readActor(r, &b, &b.Agent); err != nil {
			return nil, err
		}
		room, err := c.RespondInvite(b.Agent, r.PathValue("r"), b.Accept == nil || *b.Accept)
		return map[string]any{"room": room}, err
	})
	h("POST /rooms/{r}/requests", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct{ Agent, Note string }
		if err := readActor(r, &b, &b.Agent); err != nil {
			return nil, err
		}
		room, err := c.RequestJoin(b.Agent, r.PathValue("r"), b.Note)
		// joined: 신청 대신 바로 들어감 (자리 비움이던 내 자리, 또는 이전 세션의 자리)
		return map[string]any{"room": room, "joined": err == nil && c.RoomOf(b.Agent) == r.PathValue("r")}, err
	})
	h("POST /rooms/{r}/requests/respond", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent   string `json:"agent"`
			Target  string `json:"target"`
			Approve *bool  `json:"approve"`
		}
		if err := readBody(r, &b); err != nil { // agent 가 없으면 사람(관리자)
			return nil, err
		}
		room, err := c.RespondRequest(b.Agent, r.PathValue("r"), b.Target, b.Approve == nil || *b.Approve)
		return map[string]any{"room": room}, err
	})
	// 사람이 직접 쓴 말 (웹 뷰어 입력창, atalk say, 입력창의 ">> 말")
	h("POST /rooms/{r}/say", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent   string `json:"agent"`
			Text    string `json:"text"`
			ReplyTo *int   `json:"reply_to"` // 말풍선 답장하기
		}
		if err := readActor(r, &b, &b.Agent); err != nil {
			return nil, err
		}
		msg, err := c.SayReply(b.Agent, r.PathValue("r"), b.Text, b.ReplyTo)
		return map[string]any{"message": msg}, err
	})
	h("GET /rooms/{r}/messages", func(w http.ResponseWriter, r *http.Request) (any, error) {
		msgs, err := c.ReadMessages(r.PathValue("r"), intQ(r, "after", 0), intQ(r, "limit", 200))
		return map[string]any{"messages": msgs}, err
	})
	h("POST /rooms/{r}/messages", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent string `json:"agent"`
			core.PostInput
		}
		if err := readActor(r, &b, &b.Agent); err != nil {
			return nil, err
		}
		msg, err := c.Post(b.Agent, r.PathValue("r"), b.PostInput)
		return map[string]any{"message": msg}, err
	})
	h("GET /rooms/{r}/logs", func(w http.ResponseWriter, r *http.Request) (any, error) {
		logs, err := c.ReadLogs(r.PathValue("r"), r.URL.Query().Get("agent"))
		return map[string]any{"logs": logs}, err
	})
	h("POST /rooms/{r}/logs", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Agent string `json:"agent"`
			Seq   int    `json:"seq"`
			State string `json:"state"`
			Note  string `json:"note"`
		}
		if err := readActor(r, &b, &b.Agent); err != nil {
			return nil, err
		}
		e, err := c.Log(b.Agent, r.PathValue("r"), b.Seq, b.State, b.Note)
		return map[string]any{"log": e}, err
	})

	// ── 뷰어 ──
	h("GET /events", func(w http.ResponseWriter, r *http.Request) (any, error) {
		s.serveSSE(w, r, func(core.Event) bool { return true })
		return skip{}, nil
	})
	// 대화 내보내기: 뷰어 HTML 에 방 데이터를 넣은 파일 하나 (데몬 없이 열어 볼 수 있음)
	h("GET /rooms/{r}/export", func(w http.ResponseWriter, r *http.Request) (any, error) {
		page, err := s.ExportHTML(r.PathValue("r"))
		if err != nil {
			return nil, err
		}
		w.Header().Set("content-type", "text/html; charset=utf-8")
		if boolQ(r, "download") {
			w.Header().Set("content-disposition", "attachment; filename*=UTF-8''"+url.PathEscape(r.PathValue("r"))+".html")
		}
		_, _ = w.Write(page)
		return skip{}, nil
	})
	h("GET /icons/{name}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		b, err := iconFS.ReadFile("icons/" + r.PathValue("name"))
		if err != nil || !strings.HasSuffix(r.PathValue("name"), ".svg") {
			return nil, &core.Error{Status: 404, Code: "not_found", Message: "아이콘 없음"}
		}
		w.Header().Set("content-type", "image/svg+xml")
		w.Header().Set("cache-control", "max-age=86400")
		_, _ = w.Write(b)
		return skip{}, nil
	})
	h("GET /{$}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		_, _ = w.Write(viewerHTML)
		return skip{}, nil
	})
	// 중계: 플러그인이 에이전트 대신 부름. 원격에 연결된 에이전트면 원격 데몬으로 (relay.go)
	mux.HandleFunc("/relay/{agent}/{rest...}", s.relay(mux))
	h("/", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return nil, &core.Error{Status: 404, Code: "no_route", Message: r.Method + " " + r.URL.Path}
	})
	return mux
}

// wait: 나에게 온 메시지가 생기거나, 초대/신청/멤버십 이벤트가 오거나, 타임아웃까지 대기 (long-poll).
func (s *Server) wait(w http.ResponseWriter, r *http.Request) (any, error) {
	c, id := s.Core, r.PathValue("id")
	if _, err := c.GetAgent(id); err != nil {
		return nil, err
	}
	timeout := time.Duration(min(max(intQ(r, "timeout", 60), 1), 600)) * time.Second
	ack := boolQ(r, "ack")
	result := func(reason string) (any, error) {
		u, err := c.Unread(id, ack)
		if err != nil {
			return nil, err
		}
		p, err := c.Pending(id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"reason": reason, "room": u.Room, "cursor": u.Cursor, "messages": u.Messages, "pending": p}, nil
	}
	// 구독을 먼저 걸고 확인해야 그 사이에 온 이벤트를 놓치지 않는다
	ch := c.Subscribe()
	defer c.Unsubscribe(ch)
	if c.HasWork(id) {
		return result("message")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case e := <-ch:
			if !slices.Contains(e.To, id) {
				continue
			}
			if e.Type != "message" {
				return result(e.Type)
			}
			if c.HasWork(id) {
				return result("message")
			}
		case <-timer.C:
			return result("timeout")
		case <-r.Context().Done():
			return skip{}, nil
		case <-s.stop:
			return result("shutdown")
		}
	}
}

func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request, filter func(core.Event) bool) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, ": connected\n\n")
	if fl != nil {
		fl.Flush()
	}
	ch := s.Core.Subscribe()
	defer s.Core.Unsubscribe(ch)
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case e := <-ch:
			if !filter(e) {
				continue
			}
			b, _ := json.Marshal(e.Data)
			_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
			if err != nil {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// ───────────────────────── 요청/응답 헬퍼 ─────────────────────────

func readBody(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1_000_001))
	if err != nil {
		return err
	}
	if len(b) > 1_000_000 {
		return &core.Error{Status: 413, Code: "too_large", Message: "요청 본문이 너무 큼 (최대 1MB)"}
	}
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return &core.Error{Status: 400, Code: "bad_json", Message: "JSON 본문을 해석할 수 없음: " + err.Error()}
	}
	return nil
}

func readActor(r *http.Request, v any, agent *string) error {
	if err := readBody(r, v); err != nil {
		return err
	}
	if *agent == "" {
		return &core.Error{Status: 400, Code: "missing_agent", Message: "요청 본문에 agent(내 agent id)가 필요함"}
	}
	return nil
}

func boolQ(r *http.Request, k string) bool {
	v := r.URL.Query().Get(k)
	return v == "1" || v == "true"
}

func intQ(r *http.Request, k string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return n
	}
	return def
}

func writeOK(w http.ResponseWriter, out any) {
	// 결과 객체를 펼쳐서 {"ok":true, ...} 로 만든다
	b, err := json.Marshal(out)
	if err != nil {
		writeErr(w, err)
		return
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil || m == nil {
		m = map[string]json.RawMessage{}
	}
	m["ok"] = json.RawMessage("true")
	w.Header().Set("content-type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(m)
}

// localOnly 는 브라우저를 통한 공격을 막는다.
//   - Host 가 127.0.0.1/localhost/::1 이 아니면 거부 (DNS 리바인딩: 남의 도메인이 127.0.0.1 로 풀리는 경우)
//   - 쓰기 요청(GET/HEAD 외)에 다른 출처의 Origin 이 붙어 있으면 거부 (다른 웹사이트가 데몬에 메시지/작업을 몰래 보내는 것)
//
// CLI/훅/플러그인/브리지는 Origin 을 보내지 않으므로 영향이 없다.
func localOnly(next http.Handler) http.Handler {
	isLocal := func(host string) bool {
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLocal(r.Host) {
			writeErr(w, &core.Error{Status: 403, Code: "forbidden_host", Message: "로컬 주소로만 접근할 수 있음"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != "null" {
				u, err := url.Parse(o)
				if err != nil || !isLocal(u.Host) {
					writeErr(w, &core.Error{Status: 403, Code: "forbidden_origin", Message: "다른 출처에서 온 쓰기 요청은 받지 않음"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeErr(w http.ResponseWriter, err error) {
	var ce *core.Error
	if !errors.As(err, &ce) {
		log.Printf("internal error: %v", err)
		ce = &core.Error{Status: 500, Code: "internal", Message: err.Error()}
	}
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(ce.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": ce.Code, "message": ce.Message}})
}
