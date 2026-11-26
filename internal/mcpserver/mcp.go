// Package mcpserver 는 ATalk MCP 서버(stdio)다. MCP 를 지원하는 어떤 에이전트 도구든 대화방에 참여할 수 있다.
// 내 agent id: ATALK_AGENT 환경 변수 → 없으면 atalk_attach / atalk_register 호출로 정해진다.
// 이 프로세스는 에이전트가 살아 있는 동안만 살아 있으므로 자기 PID 를 비정상 종료 감지용으로 등록한다.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atalk/internal/client"
	"atalk/internal/core"
)

type state struct {
	mu   sync.Mutex
	self string
	c    *client.Client
}

func (s *state) me() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.self == "" {
		return "", errors.New("내 agent id 가 정해지지 않음. 먼저 atalk_attach(agent_id) 또는 atalk_register 를 호출하세요")
	}
	return s.self, nil
}

func (s *state) set(id string) {
	s.mu.Lock()
	s.self = id
	s.mu.Unlock()
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func jsonText(v any) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return text(string(b))
}

// tool 은 핸들러 오류를 isError 결과로 바꿔 에이전트에게 보여 준다.
func tool[In any](srv *mcp.Server, name, desc string, fn func(In) (*mcp.CallToolResult, error)) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	simplifyTypes(schema)
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: desc, InputSchema: schema}, func(_ context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		res, err := fn(in)
		if err != nil {
			r := text("오류: " + err.Error())
			r.IsError = true
			return r, nil, nil
		}
		return res, nil, nil
	})
}

// simplifyTypes 는 nil 이 될 수 있는 Go 타입(슬라이스, 포인터)이 만드는 ["null","array"] 같은 합집합 타입을
// 단일 타입으로 바꾼다. 선택 항목은 required 에서 빠지는 것으로 충분하고, 합집합 타입을 못 다루는 클라이언트가 있다.
func simplifyTypes(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if len(s.Types) > 0 {
		var rest []string
		for _, t := range s.Types {
			if t != "null" {
				rest = append(rest, t)
			}
		}
		if len(rest) == 1 {
			s.Type, s.Types = rest[0], nil
		} else {
			s.Types = rest
		}
	}
	for _, p := range s.Properties {
		simplifyTypes(p)
	}
	simplifyTypes(s.Items)
}

type empty struct{}

// Run 은 stdio 로 MCP 서버를 실행한다.
func Run(ctx context.Context) error {
	st := &state{self: os.Getenv("ATALK_AGENT"), c: client.New("")}
	srv := mcp.NewServer(&mcp.Implementation{Name: "atalk", Version: "0.2.0"}, nil)
	register(srv, st)
	return srv.Run(ctx, &mcp.StdioTransport{})
}

func register(srv *mcp.Server, st *state) {
	c := st.c
	pid := os.Getpid()

	tool(srv, "atalk_attach", "이미 등록된 내 네임카드(agent id)에 이 MCP 연결을 붙인다. 세션 시작 시 안내받은 id 를 넣는다.",
		func(in struct {
			AgentID string `json:"agent_id"`
		}) (*mcp.CallToolResult, error) {
			a, err := c.Agent(in.AgentID)
			if err != nil {
				return nil, err
			}
			st.set(a.ID)
			if _, err := c.Register(core.RegisterInput{Kind: a.Kind, SessionID: a.SessionID, Pid: &pid}); err != nil {
				return nil, err
			}
			return jsonText(map[string]any{"attached": a.ID, "status": a.Status, "room": a.Room}), nil
		})

	tool(srv, "atalk_register", "네임카드를 등록(또는 갱신)하고 이 연결의 내 id 로 삼는다. session_id 를 모르면 생략하면 새로 만든다.",
		func(in struct {
			Kind         string   `json:"kind" jsonschema:"에이전트 종류: claude-code | codex | gemini-cli | opencode | custom ..."`
			SessionID    string   `json:"session_id,omitempty"`
			Name         string   `json:"name,omitempty"`
			Description  string   `json:"description,omitempty" jsonschema:"어떤 작업을 하는 에이전트인지"`
			Capabilities []string `json:"capabilities,omitempty" jsonschema:"할 수 있는 일 목록"`
		}) (*mcp.CallToolResult, error) {
			cwd, _ := os.Getwd()
			reg := core.RegisterInput{Kind: in.Kind, SessionID: in.SessionID, Cwd: &cwd, Pid: &pid}
			if in.Name != "" {
				reg.Name = &in.Name
			}
			if in.Description != "" {
				reg.Description = &in.Description
			}
			if in.Capabilities != nil {
				reg.Capabilities = &in.Capabilities
			}
			r, err := c.Register(reg)
			if err != nil {
				return nil, err
			}
			st.set(r.Agent.ID)
			return jsonText(r), nil
		})

	tool(srv, "atalk_update_card", "내 네임카드의 이름/설명/능력을 수정한다.",
		func(in struct {
			Name         *string   `json:"name,omitempty"`
			Description  *string   `json:"description,omitempty"`
			Capabilities *[]string `json:"capabilities,omitempty"`
		}) (*mcp.CallToolResult, error) {
			me, err := st.me()
			if err != nil {
				return nil, err
			}
			a, err := c.UpdateCard(me, core.CardPatch{Name: in.Name, Description: in.Description, Capabilities: in.Capabilities})
			if err != nil {
				return nil, err
			}
			return jsonText(a), nil
		})

	tool(srv, "atalk_list_agents", "등록된 에이전트(네임카드) 목록. status 로 거를 수 있다 (waiting = 초대 가능).",
		func(in struct {
			Status string `json:"status,omitempty" jsonschema:"waiting | in_room | away | offline"`
			Kind   string `json:"kind,omitempty"`
		}) (*mcp.CallToolResult, error) {
			agents, err := c.Agents(in.Status, in.Kind)
			if err != nil {
				return nil, err
			}
			out := []map[string]any{}
			for _, a := range agents {
				out = append(out, map[string]any{"id": a.ID, "name": a.Name, "kind": a.Kind, "status": a.Status, "room": a.Room, "description": a.Description, "capabilities": a.Capabilities})
			}
			return jsonText(out), nil
		})

	tool(srv, "atalk_list_rooms", "열려 있는 대화방 목록", func(empty) (*mcp.CallToolResult, error) {
		rooms, err := c.Rooms(false)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, r := range rooms {
			ms := []string{}
			for _, m := range r.Members {
				ms = append(ms, m.Agent)
			}
			out = append(out, map[string]any{"room_id": r.RoomID, "title": r.Title, "owner": r.Owner, "members": ms, "last_seq": r.LastSeq})
		}
		return jsonText(out), nil
	})

	tool(srv, "atalk_create_room", "대화방을 만들고 방장이 된다. 다른 방에 있었다면 자동으로 나온다.",
		func(in struct {
			RoomID      string `json:"room_id" jsonschema:"문자/숫자/-/_ 1~64자"`
			Title       string `json:"title,omitempty"`
			Description string `json:"description,omitempty" jsonschema:"방의 목적 (에이전트들에게 전해짐)"`
		}) (*mcp.CallToolResult, error) {
			me, err := st.me()
			if err != nil {
				return nil, err
			}
			r, err := c.CreateRoomWith(me, in.RoomID, in.Title, in.Description)
			if err != nil {
				return nil, err
			}
			return jsonText(r), nil
		})

	tool(srv, "atalk_invite", "(방장) 에이전트를 내 방에 초대한다.",
		func(in struct {
			Target string `json:"target" jsonschema:"초대할 agent id"`
		}) (*mcp.CallToolResult, error) {
			me, room, err := meRoom(st)
			if err != nil {
				return nil, err
			}
			if err := c.Invite(me, room, in.Target); err != nil {
				return nil, err
			}
			return text(fmt.Sprintf("%s 를 %s 방에 초대함", in.Target, room)), nil
		})

	tool(srv, "atalk_respond_invite", "받은 초대를 수락/거절한다. 수락하면 현재 방에서 나와 그 방에 들어간다.",
		func(in struct {
			Room   string `json:"room"`
			Accept bool   `json:"accept"`
		}) (*mcp.CallToolResult, error) {
			me, err := st.me()
			if err != nil {
				return nil, err
			}
			if err := c.RespondInvite(me, in.Room, in.Accept); err != nil {
				return nil, err
			}
			if in.Accept {
				return text(in.Room + " 입장"), nil
			}
			return text(in.Room + " 초대 거절"), nil
		})

	tool(srv, "atalk_request_join", "대화방 방장에게 참여 신청을 보낸다.",
		func(in struct {
			Room string `json:"room"`
			Note string `json:"note,omitempty"`
		}) (*mcp.CallToolResult, error) {
			me, err := st.me()
			if err != nil {
				return nil, err
			}
			joined, err := c.RequestJoin(me, in.Room, in.Note)
			if err != nil {
				return nil, err
			}
			if joined {
				return text(in.Room + " 방에 들어감 (이전 자리로 복귀)"), nil
			}
			return text(in.Room + " 방에 참여 신청함"), nil
		})

	tool(srv, "atalk_respond_request", "(방장) 참여 신청을 승인/거절한다.",
		func(in struct {
			Target  string `json:"target"`
			Approve bool   `json:"approve"`
		}) (*mcp.CallToolResult, error) {
			me, room, err := meRoom(st)
			if err != nil {
				return nil, err
			}
			if err := c.RespondRequest(me, room, in.Target, in.Approve); err != nil {
				return nil, err
			}
			return text(fmt.Sprintf("%s 신청 %s", in.Target, map[bool]string{true: "승인", false: "거절"}[in.Approve])), nil
		})

	tool(srv, "atalk_pending", "나에게 온 초대와, 내가 방장인 방에 온 참여 신청", func(empty) (*mcp.CallToolResult, error) {
		me, err := st.me()
		if err != nil {
			return nil, err
		}
		p, err := c.Pending(me)
		if err != nil {
			return nil, err
		}
		return jsonText(p), nil
	})

	tool(srv, "atalk_post", "내 방에 메시지를 쓴다. 일을 맡길 땐 type=task 와 to 를, 결과는 type=result 와 reply_to 를 쓴다.",
		func(in struct {
			Body    string   `json:"body"`
			Type    string   `json:"type,omitempty" jsonschema:"chat | task | result | status"`
			To      []string `json:"to,omitempty" jsonschema:"받는 agent id 들. 비우면 방 전체"`
			ReplyTo *int     `json:"reply_to,omitempty" jsonschema:"답하는 메시지의 seq"`
		}) (*mcp.CallToolResult, error) {
			me, room, err := meRoom(st)
			if err != nil {
				return nil, err
			}
			m, err := c.Post(me, room, core.PostInput{Body: in.Body, Type: in.Type, To: in.To, ReplyTo: in.ReplyTo})
			if err != nil {
				return nil, err
			}
			return text(fmt.Sprintf("#%d 전송 (%s)", m.Seq, room)), nil
		})

	tool(srv, "atalk_unread", "내 방의 안 읽은 메시지. ack=false 가 아니면 읽음 처리한다.",
		func(in struct {
			Ack *bool `json:"ack,omitempty"`
		}) (*mcp.CallToolResult, error) {
			me, err := st.me()
			if err != nil {
				return nil, err
			}
			u, err := c.Unread(me, in.Ack == nil || *in.Ack)
			if err != nil {
				return nil, err
			}
			if len(u.Messages) == 0 {
				return text("(안 읽은 메시지 없음)"), nil
			}
			return text(client.FormatMessages(u.Messages, c.Names())), nil
		})

	tool(srv, "atalk_wait", "나에게 온 메시지(또는 초대/신청)가 올 때까지 기다린다. 할 일이 없을 때 이걸 반복 호출해 대기한다.",
		func(in struct {
			TimeoutSec int `json:"timeout_sec,omitempty" jsonschema:"1~600, 기본 120"`
		}) (*mcp.CallToolResult, error) {
			me, err := st.me()
			if err != nil {
				return nil, err
			}
			t := in.TimeoutSec
			if t <= 0 {
				t = 120
			}
			w, err := c.Wait(me, min(t, 600), true)
			if err != nil {
				return nil, err
			}
			parts := []string{"(" + w.Reason + ")"}
			if s := client.FormatMessages(w.Messages, c.Names()); s != "" {
				parts = append(parts, s)
			}
			var pend []string
			for _, i := range w.Pending.Invites {
				pend = append(pend, fmt.Sprintf("[초대] %s %q (보낸 이 %s) → atalk_respond_invite", i.Room, i.Title, i.By))
			}
			for _, q := range w.Pending.Requests {
				pend = append(pend, fmt.Sprintf("[참여 신청] %s → %s %s → atalk_respond_request", q.Agent, q.Room, q.Note))
			}
			if len(pend) > 0 {
				parts = append(parts, strings.Join(pend, "\n"))
			}
			return text(strings.Join(parts, "\n\n")), nil
		})

	tool(srv, "atalk_read", "내 방(또는 지정한 방)의 대화 기록",
		func(in struct {
			Room  string `json:"room,omitempty"`
			After int    `json:"after,omitempty"`
			Limit int    `json:"limit,omitempty"`
		}) (*mcp.CallToolResult, error) {
			room := in.Room
			if room == "" {
				var err error
				if _, room, err = meRoom(st); err != nil {
					return nil, err
				}
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 100
			}
			msgs, err := c.Messages(room, in.After, limit)
			if err != nil {
				return nil, err
			}
			if len(msgs) == 0 {
				return text("(메시지 없음)"), nil
			}
			return text(client.FormatMessages(client.AsInbox(msgs), c.Names())), nil
		})

	tool(srv, "atalk_log", "받은 메시지를 어떻게 처리하고 있는지 기록한다 (received → working → done/failed/skipped).",
		func(in struct {
			Seq   int    `json:"seq"`
			State string `json:"state" jsonschema:"received | working | done | skipped | failed"`
			Note  string `json:"note,omitempty"`
		}) (*mcp.CallToolResult, error) {
			me, room, err := meRoom(st)
			if err != nil {
				return nil, err
			}
			if err := c.Log(me, room, in.Seq, in.State, in.Note); err != nil {
				return nil, err
			}
			return text(fmt.Sprintf("기록: #%d %s", in.Seq, in.State)), nil
		})

	tool(srv, "atalk_leave", "지금 방에서 나온다 (대기 상태로 돌아감).", func(empty) (*mcp.CallToolResult, error) {
		me, err := st.me()
		if err != nil {
			return nil, err
		}
		if err := c.Leave(me, ""); err != nil {
			return nil, err
		}
		return text("퇴장함"), nil
	})

	tool(srv, "atalk_takeover", "재개 후 세션 ID 가 바뀌었을 때, 죽은 이전 세션의 방 자리를 이어받는다.",
		func(in struct {
			From string `json:"from" jsonschema:"이전 agent id"`
		}) (*mcp.CallToolResult, error) {
			me, err := st.me()
			if err != nil {
				return nil, err
			}
			rec, err := c.Takeover(me, in.From)
			if err != nil {
				return nil, err
			}
			seqs := []string{}
			for _, u := range rec.Unfinished {
				seqs = append(seqs, fmt.Sprintf("#%d", u.Seq))
			}
			if len(seqs) == 0 {
				seqs = []string{"없음"}
			}
			return text(fmt.Sprintf("이어받음: %s 방 (%s)\n미완료 작업: %s\n\n최근 대화:\n%s", rec.Room, rec.Role, strings.Join(seqs, ", "),
				client.FormatMessages(client.AsInbox(rec.Recent), c.Names()))), nil
		})
}

func meRoom(st *state) (string, string, error) {
	me, err := st.me()
	if err != nil {
		return "", "", err
	}
	room, err := st.c.MyRoom(me)
	return me, room, err
}

// RunPermission 은 헤드리스 Claude Code 턴(claude -p --permission-prompt-tool mcp__atalkperm__permission)의 허가 도구만 있는 MCP 서버.
// 허가가 필요한 도구를 쓰려 하면 ATalk 대화방에 확인 카드를 올리고, 사람이 웹에서 [허용]/[거부]할 때까지 기다린다.
func RunPermission(ctx context.Context) error {
	c := client.New("")
	agent, spawn := os.Getenv("ATALK_AGENT"), os.Getenv("ATALK_SPAWN")
	srv := mcp.NewServer(&mcp.Implementation{Name: "atalkperm", Version: "0.1.0"}, nil)
	tool(srv, "permission", "ATalk: 사람에게 도구 실행 허가를 묻는다 (대화방 확인 카드).",
		func(in struct {
			ToolName  string         `json:"tool_name"`
			Input     map[string]any `json:"input"`
			ToolUseID string         `json:"tool_use_id,omitempty"`
		}) (*mcp.CallToolResult, error) {
			detail := ""
			for _, k := range []string{"command", "file_path", "path", "url", "pattern"} {
				if v, ok := in.Input[k].(string); ok && v != "" {
					detail = v
					break
				}
			}
			text := in.ToolName + " 실행 허가"
			if detail != "" {
				text += ": " + detail
			}
			allow, msg, err := c.Approval(agent, spawn, text)
			if err != nil {
				allow, msg = false, "ATalk 에 허가를 묻지 못함: "+err.Error()
			}
			if allow {
				return jsonText(map[string]any{"behavior": "allow", "updatedInput": in.Input}), nil
			}
			return jsonText(map[string]any{"behavior": "deny", "message": msg}), nil
		})
	return srv.Run(ctx, &mcp.StdioTransport{})
}
