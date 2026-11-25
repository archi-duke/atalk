// Package client 는 ATalk 데몬 HTTP 클라이언트다 (CLI, MCP 서버, 훅, 브리지가 공통으로 쓴다).
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"atalk/internal/core"
	"atalk/internal/paths"
)

// DefaultURL 은 데몬 주소: ATALK_URL → 127.0.0.1:<paths.Port()>.
func DefaultURL() string { return paths.URL() }

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

type Client struct {
	Base string
	HTTP *http.Client
	// Token 이 있으면 원격 데몬에 연결한 클라이언트 (Authorization: Bearer). remote.go
	Token string
	// Conn 은 원격 연결 정보 (원격일 때만)
	Conn *RemoteConn
}

// Remote 는 다른 PC 의 데몬에 연결한 클라이언트인지.
func (c *Client) Remote() bool { return c.Conn != nil }

func (c *Client) auth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func New(base string) *Client {
	if base == "" {
		base = DefaultURL()
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{}}
}

func (c *Client) call(method, path string, body any, out any, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	c.auth(req)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{0, "unreachable", fmt.Sprintf("ATalk 데몬에 연결할 수 없음 (%s): %v", c.Base, err)}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	var env struct {
		OK    *bool `json:"ok"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env)
	if res.StatusCode >= 400 || (env.OK != nil && !*env.OK) {
		if env.Error != nil {
			return &Error{res.StatusCode, env.Error.Code, env.Error.Message}
		}
		return &Error{res.StatusCode, "http_error", fmt.Sprintf("HTTP %d", res.StatusCode)}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func esc(s string) string { return url.PathEscape(s) }

// ── 디스커버리 ──

func (c *Client) Health() error {
	return c.call("GET", "/health", nil, nil, 2*time.Second)
}

// HealthInfo 는 데몬의 데이터 폴더와 PID 를 돌려준다.
func (c *Client) HealthInfo() (h struct {
	Root string `json:"root"`
	Pid  int    `json:"pid"`
}, err error) {
	err = c.call("GET", "/health", nil, &h, 2*time.Second)
	return
}

// Export 는 방 대화를 메신저 형식 HTML 로 받는다.
func (c *Client) Export(room string) ([]byte, error) {
	res, err := c.HTTP.Get(c.Base + "/rooms/" + esc(room) + "/export")
	if err != nil {
		return nil, &Error{0, "unreachable", err.Error()}
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		var env struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(b, &env)
		return nil, &Error{res.StatusCode, env.Error.Code, env.Error.Message}
	}
	return b, nil
}

// ResumePrevious 는 같은 폴더의 이전 세션이 있던 방 자리를 이어받는다. 없으면 code no_previous 오류.
func (c *Client) ResumePrevious(id string) (*core.Recovery, string, error) {
	var out struct {
		Recovery *core.Recovery `json:"recovery"`
		From     string         `json:"from"`
	}
	err := c.call("POST", "/agents/"+esc(id)+"/resume", map[string]any{}, &out, 0)
	return out.Recovery, out.From, err
}

func (c *Client) Config() (cfg core.Config, err error) {
	var out struct {
		Config core.Config `json:"config"`
	}
	err = c.call("GET", "/config", nil, &out, 0)
	return out.Config, err
}

func (c *Client) SetConfig(cfg core.Config) (core.Config, error) {
	var out struct {
		Config core.Config `json:"config"`
	}
	err := c.call("PUT", "/config", cfg, &out, 0)
	return out.Config, err
}

func (c *Client) Register(in core.RegisterInput) (r core.RegisterResult, err error) {
	if c.Conn != nil { // 원격: 경로·PID 대신 이 PC 정보와 작업 위치 ID 만 보낸다
		in = c.Conn.RemoteCard(in)
	}
	err = c.call("POST", "/agents", in, &r, 0)
	return
}

func (c *Client) Agents(status, kind string) ([]core.AgentView, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	path := "/agents"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var r struct{ Agents []core.AgentView }
	err := c.call("GET", path, nil, &r, 0)
	return r.Agents, err
}

func (c *Client) Agent(id string) (core.AgentView, error) {
	var r struct{ Agent core.AgentView }
	err := c.call("GET", "/agents/"+esc(id), nil, &r, 0)
	return r.Agent, err
}

func (c *Client) UpdateCard(id string, p core.CardPatch) (core.AgentView, error) {
	var r struct{ Agent core.AgentView }
	err := c.call("PATCH", "/agents/"+esc(id), p, &r, 0)
	return r.Agent, err
}

func (c *Client) Unregister(id string) error {
	return c.call("DELETE", "/agents/"+esc(id), nil, nil, 0)
}

// EchoPrompt 는 사람이 에이전트 창에 입력한 말을 그 에이전트의 방에 기록만 한다 (알림 없음). 방에 없으면 아무것도 안 함.
func (c *Client) EchoPrompt(id, text string) error {
	return c.call("POST", "/agents/"+esc(id)+"/echo", map[string]any{"text": text}, nil, 0)
}

// EchoReply 는 응답을 받는 중인 CLI 입력에 대한 에이전트의 응답 문장들을 기록한다 (새 문장만, 알림 없음).
// final 이면 그 턴을 닫는다. 응답을 받는 중인 입력이 없으면 아무것도 안 함.
// prompt 는 이번 턴을 시작한 사람 입력 (모르면 ""). 응답을 기다리는 CLI 입력과 다르면 데몬이 무시한다.
func (c *Client) EchoReply(id string, texts []string, final bool, prompt string) error {
	return c.call("POST", "/agents/"+esc(id)+"/echo", map[string]any{"texts": texts, "reply": true, "final": final, "prompt": prompt}, nil, 0)
}

// Echo 는 CLI 입력·응답 기록 하나를 보낸다 (훅의 outbox 가 쓴다). eid 가 같은 기록은 데몬이 한 번만 남긴다.
func (c *Client) Echo(id string, e EchoEvent) error {
	return c.call("POST", "/agents/"+esc(id)+"/echo", e, nil, 0)
}

// EchoEvent 는 /agents/:id/echo 본문.
type EchoEvent struct {
	EID    string   `json:"eid,omitempty"`
	Text   string   `json:"text,omitempty"`   // CLI 입력
	Reply  bool     `json:"reply,omitempty"`  // 응답
	Texts  []string `json:"texts,omitempty"`  // 응답: 이번 턴에 지금까지 쓴 문장들
	Final  *bool    `json:"final,omitempty"`  // 응답: 턴이 끝남 (없으면 true)
	Prompt string   `json:"prompt,omitempty"` // 응답: 이번 턴을 시작한 사람 입력
	Wake   []int    `json:"wake,omitempty"`   // 응답: 이번 턴을 시작한 ATalk 알림의 메시지 번호들 (사람 입력 없이 알림으로 깨어난 턴)
}

// Activity 는 에이전트가 일을 시작(busy=true)·끝냈음(false)을 알린다 (작업 중 표시).
func (c *Client) Activity(id string, busy bool) error {
	return c.call("POST", "/agents/"+esc(id)+"/activity", map[string]any{"busy": busy}, nil, 0)
}

// Activate 는 임시 세션을 확정한다 (사람이 처음 입력함).
func (c *Client) Activate(id string) error {
	return c.call("POST", "/agents/"+esc(id)+"/activate", map[string]any{}, nil, 0)
}

func (c *Client) Away(id, reason string) error {
	return c.call("POST", "/agents/"+esc(id)+"/away", map[string]string{"reason": reason}, nil, 0)
}

func (c *Client) Leave(id, reason string) error {
	return c.call("POST", "/agents/"+esc(id)+"/leave", map[string]string{"reason": reason}, nil, 0)
}

func (c *Client) Takeover(id, from string) (*core.Recovery, error) {
	var r struct{ Recovery *core.Recovery }
	err := c.call("POST", "/agents/"+esc(id)+"/takeover", map[string]string{"from": from}, &r, 0)
	return r.Recovery, err
}

func (c *Client) Unread(id string, ack bool) (u core.Unread, err error) {
	path := "/agents/" + esc(id) + "/unread"
	if ack {
		path += "?ack=1"
	}
	err = c.call("GET", path, nil, &u, 0)
	return
}

func (c *Client) Ack(id string, seq int) error {
	return c.call("POST", "/agents/"+esc(id)+"/ack", map[string]int{"seq": seq}, nil, 0)
}

func (c *Client) Pending(id string) (p core.Pending, err error) {
	err = c.call("GET", "/agents/"+esc(id)+"/pending", nil, &p, 0)
	return
}

type WaitResult struct {
	Reason   string              `json:"reason"`
	Room     *string             `json:"room"`
	Cursor   int                 `json:"cursor"`
	Messages []core.InboxMessage `json:"messages"`
	Pending  core.Pending        `json:"pending"`
}

func (c *Client) Wait(id string, timeoutSec int, ack bool) (r WaitResult, err error) {
	path := fmt.Sprintf("/agents/%s/wait?timeout=%d", esc(id), timeoutSec)
	if ack {
		path += "&ack=1"
	}
	err = c.call("GET", path, nil, &r, time.Duration(timeoutSec+10)*time.Second)
	return
}

// ── 대화방 ──

func (c *Client) Rooms(includeClosed bool) ([]core.RoomView, error) {
	path := "/rooms"
	if includeClosed {
		path += "?include_closed=1"
	}
	var r struct{ Rooms []core.RoomView }
	err := c.call("GET", path, nil, &r, 0)
	return r.Rooms, err
}

func (c *Client) Room(room string) (core.RoomView, error) {
	var r struct{ Room core.RoomView }
	err := c.call("GET", "/rooms/"+esc(room), nil, &r, 0)
	return r.Room, err
}

// CreateRoomWith 는 설명(목적)과 함께 방을 만든다. agent 가 "" 이면 사람이 방장 없이 만든 방.
func (c *Client) CreateRoomWith(agent, roomID, title, description string) (core.RoomView, error) {
	var out struct {
		Room core.RoomView `json:"room"`
	}
	err := c.call("POST", "/rooms", map[string]string{"agent": agent, "room_id": roomID, "title": title, "description": description}, &out, 0)
	return out.Room, err
}

// SetRoomDescription 은 방 설명을 바꾼다.
func (c *Client) SetRoomDescription(agent, room, desc string) (core.RoomView, error) {
	var out struct {
		Room core.RoomView `json:"room"`
	}
	err := c.call("PATCH", "/rooms/"+esc(room), map[string]any{"agent": agent, "description": desc}, &out, 0)
	return out.Room, err
}

func (c *Client) CreateRoom(agent, roomID, title string) (core.RoomView, error) {
	var r struct{ Room core.RoomView }
	err := c.call("POST", "/rooms", map[string]string{"agent": agent, "room_id": roomID, "title": title}, &r, 0)
	return r.Room, err
}

func (c *Client) RenameRoom(agent, room, title string) (v core.RoomView, err error) {
	var out struct {
		Room core.RoomView `json:"room"`
	}
	err = c.call("PATCH", "/rooms/"+esc(room), map[string]string{"agent": agent, "title": title}, &out, 0)
	return out.Room, err
}

// DeleteRoom 은 방을 지우고 대화 기록을 옮긴 trash 폴더 경로를 돌려준다.
func (c *Client) DeleteRoom(room string) (string, error) {
	var out struct {
		Trash string `json:"trash"`
	}
	err := c.call("DELETE", "/rooms/"+esc(room), nil, &out, 0)
	return out.Trash, err
}

func (c *Client) Invite(agent, room, target string) error {
	return c.call("POST", "/rooms/"+esc(room)+"/invites", map[string]string{"agent": agent, "target": target}, nil, 0)
}

func (c *Client) RespondInvite(agent, room string, accept bool) error {
	return c.call("POST", "/rooms/"+esc(room)+"/invites/respond", map[string]any{"agent": agent, "accept": accept}, nil, 0)
}

// RequestJoin 은 참여 신청을 보낸다. joined 가 true 면 신청 대신 바로 들어감 (내 자리·이전 세션의 자리).
func (c *Client) RequestJoin(agent, room, note string) (joined bool, err error) {
	var out struct {
		Joined bool `json:"joined"`
	}
	err = c.call("POST", "/rooms/"+esc(room)+"/requests", map[string]string{"agent": agent, "note": note}, &out, 0)
	return out.Joined, err
}

func (c *Client) RespondRequest(agent, room, target string, approve bool) error {
	return c.call("POST", "/rooms/"+esc(room)+"/requests/respond", map[string]any{"agent": agent, "target": target, "approve": approve}, nil, 0)
}

func (c *Client) Messages(room string, after, limit int) ([]core.Message, error) {
	var r struct{ Messages []core.Message }
	err := c.call("GET", fmt.Sprintf("/rooms/%s/messages?after=%d&limit=%d", esc(room), after, limit), nil, &r, 0)
	return r.Messages, err
}

func (c *Client) Post(agent, room string, in core.PostInput) (core.Message, error) {
	body := struct {
		Agent string `json:"agent"`
		core.PostInput
	}{agent, in}
	var r struct{ Message core.Message }
	err := c.call("POST", "/rooms/"+esc(room)+"/messages", body, &r, 0)
	return r.Message, err
}

func (c *Client) Log(agent, room string, seq int, state, note string) error {
	return c.call("POST", "/rooms/"+esc(room)+"/logs", map[string]any{"agent": agent, "seq": seq, "state": state, "note": note}, nil, 0)
}

func (c *Client) Logs(room, agent string) ([]core.LogEntry, error) {
	path := "/rooms/" + esc(room) + "/logs"
	if agent != "" {
		path += "?agent=" + url.QueryEscape(agent)
	}
	var r struct{ Logs []core.LogEntry }
	err := c.call("GET", path, nil, &r, 0)
	return r.Logs, err
}

// MyRoom 은 내가 들어가 있는 방 ID 를 돌려준다.
func (c *Client) MyRoom(id string) (string, error) {
	a, err := c.Agent(id)
	if err != nil {
		return "", err
	}
	if a.Room == nil {
		return "", &Error{409, "not_in_room", "들어가 있는 방이 없음"}
	}
	return *a.Room, nil
}

// Names 는 agent id → 이름 표 (실패하면 빈 표).
func (c *Client) Names() map[string]string {
	out := map[string]string{}
	var res struct {
		Names map[string]string `json:"names"`
	}
	if err := c.call("GET", "/names", nil, &res, 0); err == nil && res.Names != nil {
		return res.Names // 지운 카드의 이름까지
	}
	if agents, err := c.Agents("", ""); err == nil {
		for _, a := range agents {
			out[a.ID] = a.Name
		}
	}
	return out
}

// SSE 는 GET path 의 Server-Sent Events 를 읽어 onEvent 를 부른다. 연결이 끊기면 오류를 돌려준다.
func (c *Client) SSE(ctx context.Context, path string, onEvent func(event string, data []byte)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("SSE %s: HTTP %d", path, res.StatusCode)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	event, data := "message", []byte(nil)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data != nil {
				onEvent(event, data)
			}
			event, data = "message", nil
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " ")...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// FormatMessages 는 에이전트에게 보여 줄 메시지 텍스트를 만든다.
// Author 는 메시지 작성자 표시: 사람이 직접 쓴 메시지는 "<사람>@<에이전트> (사람)", 아니면 에이전트 이름.
func Author(m core.Message, agentName string) string {
	switch {
	case m.Human != "" && m.Echo: // 에이전트 창(CLI)에 입력한 말의 기록 — 이미 그 에이전트가 받은 입력
		return m.Human + "@" + agentName + " (사람, CLI 입력 기록)"
	case m.Human != "":
		return m.Human + "@" + agentName + " (사람)"
	case m.Echo: // 그 입력에 대한 에이전트의 응답 기록
		return agentName + " (CLI 응답 기록)"
	}
	return agentName
}

func FormatMessages(msgs []core.InboxMessage, names map[string]string) string {
	n := func(id string) string {
		if v, ok := names[id]; ok {
			return v
		}
		return id
	}
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if m.Type == "system" {
			parts = append(parts, fmt.Sprintf("#%d [system] %s", m.Seq, m.Body))
			continue
		}
		to, re, mark := "", "", ""
		if len(m.To) > 0 {
			ns := make([]string, len(m.To))
			for i, t := range m.To {
				ns[i] = n(t)
			}
			to = " → " + strings.Join(ns, ", ")
		}
		if m.ReplyTo != nil {
			re = fmt.Sprintf(" (re #%d)", *m.ReplyTo)
		}
		if !m.ForMe {
			mark = " (참고용: 나에게 온 것 아님)"
		}
		parts = append(parts, fmt.Sprintf("#%d [%s] %s%s%s%s\n%s", m.Seq, m.Type, Author(m.Message, n(m.From)), to, re, mark, m.Body))
	}
	return strings.Join(parts, "\n\n")
}

// AsInbox 는 일반 메시지 목록을 for_me=true 인 InboxMessage 로 바꾼다 (기록 보기용).
func AsInbox(msgs []core.Message) []core.InboxMessage {
	out := make([]core.InboxMessage, len(msgs))
	for i, m := range msgs {
		out[i] = core.InboxMessage{Message: m, ForMe: true}
	}
	return out
}

// Purpose 는 방 목적 한 줄 ("[방 목적] …"). 설명이 없거나 못 읽으면 "".
func (c *Client) Purpose(room string) string {
	if room == "" {
		return ""
	}
	if r, err := c.Room(room); err == nil && r.Description != "" {
		return "[" + room + " 방 목적] " + r.Description
	}
	return ""
}

// HumanPrefix 는 에이전트 입력창에서 "사람이 대화방에 직접 보내는 말" 을 표시하는 접두어.
const HumanPrefix = ">>"

// ShowRule 은 알림으로 받은 메시지를 사람 화면에 보이게 하는 규칙.
// Claude Code 는 훅이 넣어 준 알림 내용을 모델에게만 주고 화면에는 접힌 "Stop hook feedback" 한 줄만 보여 준다.
// 그래서 사람이 무엇을 받았는지 알 수 있게 모델이 응답 첫머리에 받은 메시지를 옮겨 적는다.
// (알림으로 시작한 턴은 CLI 응답으로 기록되지 않으므로 대화방에 중복되지 않는다)
const ShowRule = "작업(명령 실행·도구 사용)을 시작하기 전에, 응답의 첫 출력으로 받은 메시지를 한 건씩 그대로 옮겨 적으세요 (작업을 마친 뒤 결과와 함께 적지 말 것) — 형식: \"📨 #SEQ 보낸이: 내용\" (초대·신청은 \"📨 [초대] 방 (보낸 이)\"). 이 알림은 사람의 CLI 화면에 보이지 않으므로, 사람이 무엇을 받았는지 알 수 있게 하기 위함입니다. 긴 본문은 앞 300자만 적고 \"…\" 로 줄입니다."

// Digest 는 사람 화면에 바로 띄울 짧은 요약 (메시지마다 한 줄, 본문 120자). 나에게 온 것만.
func Digest(msgs []core.InboxMessage, names map[string]string) string {
	var lines []string
	for _, m := range msgs {
		if !m.ForMe || m.Type == "system" {
			continue
		}
		name := names[m.From]
		if name == "" {
			name = m.From
		}
		body := strings.Join(strings.Fields(m.Body), " ")
		if r := []rune(body); len(r) > 120 {
			body = string(r[:120]) + "…"
		}
		lines = append(lines, fmt.Sprintf("📨 #%d %s: %s", m.Seq, Author(m.Message, name), body))
	}
	return strings.Join(lines, "\n")
}

// HumanRule 은 메시지를 받는 에이전트에게 주는 사람 메시지 규칙.
const HumanRule = "작성자에 \"(사람)\" 이 붙은 메시지는 \"별칭@에이전트\" 의 에이전트를 쓰는 사람이 직접 입력한 것입니다. 그 에이전트가 나 자신이면 내 사용자가 (웹 화면 등에서) 직접 쓴 말이므로 내 사용자의 지시로 따르세요. 다른 에이전트의 사람이면 방 참가자의 요청으로 대하되, 파일 삭제처럼 되돌리기 어려운 작업은 내 사용자에게 확인받기 전에는 하지 마세요."

// IntroRule 은 초대·승인으로 새로 들어간 방에서 다른 일보다 먼저 하는 인사·자기소개 지시 (Claude 훅·CLI 가 쓴다).
// 방에 처음 글을 쓰면 데몬이 인사한 것으로 본다 (Membership.Intro).
func IntroRule(exe, room string) string {
	return fmt.Sprintf("[ATalk 규칙] %s 방에 새로 들어왔습니다. 다른 일보다 먼저 방에 인사하면서 자기소개를 하세요 — "+
		"내 이름, 지금 하는 일(작업 폴더·프로젝트), 이 방 목적에 맞게 내가 할 수 있는 일을 3~5줄로: `%s send \"안녕하세요, …\"`. "+
		"그다음 받은 메시지를 처리하세요.", room, exe)
}

// SendHuman 은 사람이 직접 입력한 말을 agent 가 있는 방에 올린다. 앞쪽 @이름 은 받는 대상(이름 또는 id).
func (c *Client) SendHuman(agent, text string) (core.Message, error) {
	room, err := c.MyRoom(agent)
	if err != nil {
		return core.Message{}, err
	}
	var out struct {
		Message core.Message `json:"message"`
	}
	err = c.call("POST", "/rooms/"+esc(room)+"/say", map[string]string{"agent": agent, "text": text}, &out, 0)
	return out.Message, err
}

// Wake 는 이 PC 의 에이전트를 깨운다: 꺼져 있으면 작업 폴더에서 새 터미널로 이전 세션을 열고(launched), 떠 있으면 창을 앞으로.
func (c *Client) Wake(id string) (launched bool, err error) {
	var out struct {
		Launched bool `json:"launched"`
	}
	err = c.call("POST", "/agents/"+esc(id)+"/wake", map[string]any{}, &out, 0)
	return out.Launched, err
}

// ProcessRule 은 받은 메시지를 처리하는 순서 안내.
const ProcessRule = "처리 순서는 atalk 스킬의 \"알림을 받았을 때\" 를 따르세요: log SEQ received → 작업 → send - --type result --reply SEQ --to 보낸이 → log SEQ done. result/status 에는 답장하지 마세요. 답할 필요가 없으면 log SEQ skipped."

// Notice 는 에이전트에게 주는 새 메시지 알림 글 (Claude Stop 훅의 reason, Codex 대기열 메시지).
// show 가 true 면 받은 메시지를 응답에 옮겨 적게 한다 (Claude CLI 는 훅 알림을 사람 화면에 보여 주지 않음).
func (c *Client) Notice(room string, msgs []core.InboxMessage, extra []string, show bool) string {
	parts := []string{fmt.Sprintf("[ATalk] %s 방에서 새 메시지가 왔습니다. 처리하세요.", room)}
	if p := c.Purpose(room); p != "" {
		parts = append(parts, p)
	}
	if len(msgs) > 0 {
		parts = append(parts, FormatMessages(msgs, c.Names()))
	}
	if len(extra) > 0 {
		parts = append(parts, strings.Join(extra, "\n"))
	}
	parts = append(parts, HumanRule)
	if show {
		parts = append(parts, ShowRule)
	}
	parts = append(parts, ProcessRule)
	return strings.Join(parts, "\n\n")
}
