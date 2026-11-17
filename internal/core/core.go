package core

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Error 는 HTTP 상태 코드와 오류 코드를 가진 도메인 오류.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func bad(code, msg string, a ...any) *Error       { return &Error{400, code, fmt.Sprintf(msg, a...)} }
func forbidden(code, msg string, a ...any) *Error { return &Error{403, code, fmt.Sprintf(msg, a...)} }
func notFound(code, msg string, a ...any) *Error  { return &Error{404, code, fmt.Sprintf(msg, a...)} }
func conflict(code, msg string, a ...any) *Error  { return &Error{409, code, fmt.Sprintf(msg, a...)} }

// ioFailure 는 파일 쓰기 실패를 공개 메서드 경계까지 전달하기 위한 panic 값.
type ioFailure struct{ err error }

func must(err error) {
	if err != nil {
		panic(ioFailure{err})
	}
}

type Options struct {
	Root      string
	AwayGrace time.Duration // away 가 이 시간을 넘으면 강제 퇴장 (기본 30분)
	IsAlive   func(pid int) bool
	Now       func() time.Time
}

// Atalk 은 데몬 안에서 단독으로 파일을 쓴다. 모든 공개 메서드는 mu 로 직렬화된다.
type Atalk struct {
	Root      string
	AwayGrace time.Duration
	isAlive   func(int) bool
	now       func() time.Time

	mu          sync.Mutex
	agents      map[string]*AgentCard
	memberships map[string]*Membership
	rooms       map[string]*Room
	lastSeq     map[string]int
	cursors     map[string]map[string]int

	subMu sync.Mutex
	subs  map[chan Event]struct{}
}

func New(opts Options) (*Atalk, error) {
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, err
	}
	c := &Atalk{
		Root: root, AwayGrace: opts.AwayGrace, isAlive: opts.IsAlive, now: opts.Now,
		agents: map[string]*AgentCard{}, memberships: map[string]*Membership{}, rooms: map[string]*Room{},
		lastSeq: map[string]int{}, cursors: map[string]map[string]int{}, subs: map[chan Event]struct{}{},
	}
	if c.AwayGrace == 0 {
		c.AwayGrace = 30 * time.Minute
	}
	if c.isAlive == nil {
		c.isAlive = DefaultIsAlive
	}
	if c.now == nil {
		c.now = time.Now
	}
	err = c.guard(func() error { c.load(); return nil })
	return c, err
}

// guard 는 잠금을 잡고 fn 을 실행하며, 파일 쓰기 실패 panic 을 오류로 바꾼다.
func (c *Atalk) guard(fn func() error) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			if f, ok := r.(ioFailure); ok {
				err = &Error{500, "io_error", f.err.Error()}
				return
			}
			panic(r)
		}
	}()
	return fn()
}

// ───────────────────────── 이벤트 구독 ─────────────────────────

// Subscribe 는 모든 이벤트를 받는 채널을 돌려준다. 다 쓰면 Unsubscribe 해야 한다.
func (c *Atalk) Subscribe() chan Event {
	ch := make(chan Event, 256)
	c.subMu.Lock()
	c.subs[ch] = struct{}{}
	c.subMu.Unlock()
	return ch
}

func (c *Atalk) Unsubscribe(ch chan Event) {
	c.subMu.Lock()
	delete(c.subs, ch)
	c.subMu.Unlock()
}

func (c *Atalk) emit(e Event) {
	if e.To == nil {
		e.To = []string{}
	}
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- e:
		default: // 느린 구독자는 버린다 (잠금 안에서 막히지 않게)
		}
	}
}

// ───────────────────────── 경로 / 시간 ─────────────────────────

func (c *Atalk) agentsDir() string         { return filepath.Join(c.Root, "agents") }
func (c *Atalk) roomsDir() string          { return filepath.Join(c.Root, "rooms") }
func (c *Atalk) membershipsFile() string   { return filepath.Join(c.Root, "memberships.json") }
func (c *Atalk) membershipLog() string     { return filepath.Join(c.Root, "memberships.log.jsonl") }
func (c *Atalk) cardFile(id string) string { return filepath.Join(c.agentsDir(), id+".json") }
func (c *Atalk) roomDir(r string) string   { return filepath.Join(c.roomsDir(), r) }
func (c *Atalk) roomFile(r string) string  { return filepath.Join(c.roomDir(r), "room.json") }
func (c *Atalk) messagesFile(r string) string {
	return filepath.Join(c.roomDir(r), "messages.jsonl")
}
func (c *Atalk) cursorsFile(r string) string { return filepath.Join(c.roomDir(r), "cursors.json") }
func (c *Atalk) logFile(r, agent string) string {
	return filepath.Join(c.roomDir(r), "logs", agent+".jsonl")
}

// ts 는 JavaScript toISOString() 과 같은 형식 (UTC, 밀리초).
func (c *Atalk) ts() string { return c.now().UTC().Format("2006-01-02T15:04:05.000Z") }

func ptr[T any](v T) *T { return &v }

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ───────────────────────── 로드 / 복구 ─────────────────────────

func (c *Atalk) load() {
	must(ensureDir(c.agentsDir()))
	must(ensureDir(c.roomsDir()))

	entries, err := os.ReadDir(c.agentsDir())
	must(err)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var card AgentCard
		if ok, err := readJSON(filepath.Join(c.agentsDir(), e.Name()), &card); ok && err == nil && card.ID != "" {
			if card.Capabilities == nil {
				card.Capabilities = []string{}
			}
			c.agents[card.ID] = &card
		}
	}

	dirs, err := os.ReadDir(c.roomsDir())
	must(err)
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		var room Room
		if ok, err := readJSON(c.roomFile(d.Name()), &room); !ok || err != nil {
			continue
		}
		normalizeRoom(&room)
		c.rooms[room.RoomID] = &room
		repairJSONLTail(c.messagesFile(room.RoomID))
		msgs, err := readJSONL[Message](c.messagesFile(room.RoomID))
		must(err)
		last := 0
		for _, m := range msgs {
			last = max(last, m.Seq)
		}
		c.lastSeq[room.RoomID] = last
		cur := map[string]int{}
		if _, err := readJSON(c.cursorsFile(room.RoomID), &cur); err != nil || cur == nil {
			cur = map[string]int{}
		}
		c.cursors[room.RoomID] = cur
	}

	if _, err := readJSON(c.membershipsFile(), &c.memberships); err != nil || c.memberships == nil {
		c.memberships = map[string]*Membership{}
	}
	c.reconcile()
}

func normalizeRoom(r *Room) {
	if r.Invites == nil {
		r.Invites = []Invite{}
	}
	if r.Requests == nil {
		r.Requests = []JoinRequest{}
	}
	if r.Policy.MaxMessages == 0 {
		r.Policy.MaxMessages = 1000
	}
}

// reconcile: 카드/방이 없는 연결 제거, 방장 없는 방 정리.
func (c *Atalk) reconcile() {
	dirty := false
	for agent, m := range c.memberships {
		room, ok := c.rooms[m.Room]
		if _, hasAgent := c.agents[agent]; !hasAgent || !ok || room.ClosedAt != nil {
			delete(c.memberships, agent)
			c.logMembership(MembershipEvent{TS: c.ts(), Agent: agent, Room: m.Room, Event: "leave", Detail: "reconcile: 카드 또는 방 없음"})
			dirty = true
		}
	}
	if dirty {
		c.saveMemberships()
	}
	for _, room := range c.sortedRooms() {
		if room.ClosedAt != nil {
			continue
		}
		var ownerM *Membership
		if room.Owner != nil {
			ownerM = c.memberships[*room.Owner]
		}
		if ownerM == nil || ownerM.Room != room.RoomID {
			c.fixOwner(room, "reconcile")
		}
	}
}

// ───────────────────────── 저장 ─────────────────────────

func (c *Atalk) saveCard(card *AgentCard)        { must(writeJSONAtomic(c.cardFile(card.ID), card)) }
func (c *Atalk) saveMemberships()                { must(writeJSONAtomic(c.membershipsFile(), c.memberships)) }
func (c *Atalk) saveRoom(room *Room)             { must(writeJSONAtomic(c.roomFile(room.RoomID), room)) }
func (c *Atalk) saveCursors(r string)            { must(writeJSONAtomic(c.cursorsFile(r), c.cursors[r])) }
func (c *Atalk) logMembership(e MembershipEvent) { must(appendJSONL(c.membershipLog(), e)) }

// ───────────────────────── 조회 헬퍼 ─────────────────────────

func (c *Atalk) requireAgent(id string) (*AgentCard, error) {
	a, ok := c.agents[id]
	if !ok {
		return nil, notFound("agent_not_found", "등록되지 않은 에이전트: %s", id)
	}
	return a, nil
}

func (c *Atalk) requireRoom(r string, open bool) (*Room, error) {
	room, ok := c.rooms[r]
	if !ok {
		return nil, notFound("room_not_found", "없는 대화방: %s", r)
	}
	if open && room.ClosedAt != nil {
		return nil, conflict("room_closed", "닫힌 대화방: %s", r)
	}
	return room, nil
}

func (c *Atalk) requireOwner(agent string, room *Room) error {
	if room.Owner == nil || *room.Owner != agent {
		owner := "-"
		if room.Owner != nil {
			owner = *room.Owner
		}
		return forbidden("not_owner", "방장만 할 수 있음 (방장: %s)", owner)
	}
	return nil
}

type memberEntry struct {
	agent string
	m     *Membership
}

func (c *Atalk) membersOf(r string) []memberEntry {
	var out []memberEntry
	for a, m := range c.memberships {
		if m.Room == r {
			out = append(out, memberEntry{a, m})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].m.JoinedAt != out[j].m.JoinedAt {
			return out[i].m.JoinedAt < out[j].m.JoinedAt
		}
		return out[i].agent < out[j].agent
	})
	return out
}

func (c *Atalk) memberIDs(r string, except string) []string {
	ids := []string{}
	for _, e := range c.membersOf(r) {
		if e.agent != except {
			ids = append(ids, e.agent)
		}
	}
	return ids
}

func (c *Atalk) sortedRooms() []*Room {
	out := make([]*Room, 0, len(c.rooms))
	for _, r := range c.rooms {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].RoomID < out[j].RoomID
	})
	return out
}

func (c *Atalk) nameOf(id string) string {
	if a, ok := c.agents[id]; ok {
		return a.Name
	}
	return id
}

func (c *Atalk) statusOf(id string) string {
	if m, ok := c.memberships[id]; ok {
		return m.State
	}
	if card, ok := c.agents[id]; ok && card.Pid != nil && !c.isAlive(*card.Pid) {
		return "offline"
	}
	return "waiting"
}

func (c *Atalk) view(card *AgentCard) AgentView {
	v := AgentView{AgentCard: *card, Status: c.statusOf(card.ID)}
	v.Capabilities = slices.Clone(card.Capabilities)
	if m, ok := c.memberships[card.ID]; ok {
		v.Room, v.Role = ptr(m.Room), ptr(m.Role)
	}
	return v
}

func (c *Atalk) roomView(room *Room) RoomView {
	v := RoomView{Room: *room, Members: []RoomMember{}, LastSeq: c.lastSeq[room.RoomID]}
	v.Invites = slices.Clone(room.Invites)
	v.Requests = slices.Clone(room.Requests)
	for _, e := range c.membersOf(room.RoomID) {
		v.Members = append(v.Members, RoomMember{Agent: e.agent, Name: c.nameOf(e.agent), Role: e.m.Role, State: e.m.State})
	}
	return v
}

// ───────────────────────── 에이전트 (디스커버리) ─────────────────────────

func (c *Atalk) Register(in RegisterInput) (res RegisterResult, err error) {
	err = c.guard(func() error {
		if !IsValidKind(in.Kind) {
			return bad("invalid_kind", "kind 는 소문자/숫자/- 로 된 1~32자여야 함 (예: claude-code)")
		}
		sid := in.SessionID
		if sid == "" {
			sid = newID()
		}
		id := MakeAgentID(in.Kind, sid)
		now := c.ts()
		existing, had := c.agents[id]

		var card AgentCard
		if had {
			card = *existing
			card.UpdatedAt = now
			if in.Name != nil {
				card.Name = *in.Name
			}
			if in.Description != nil {
				card.Description = *in.Description
			}
			if in.Capabilities != nil {
				card.Capabilities = slices.Clone(*in.Capabilities)
			}
			if in.Cwd != nil {
				card.Cwd = in.Cwd
			}
			// 살아 있는 PID 는 유지한다 (훅이 찾은 정확한 PID 를 MCP 등이 덮어쓰지 않게)
			if in.Pid != nil && *in.Pid > 0 && (card.Pid == nil || !c.isAlive(*card.Pid)) {
				card.Pid = in.Pid
			}
		} else {
			card = AgentCard{
				ID: id, Kind: in.Kind, SessionID: sid,
				Name: fmt.Sprintf("%s-%s", in.Kind, truncate(sid, 8)), Capabilities: []string{},
				Cwd: in.Cwd, RegisteredAt: now, UpdatedAt: now,
			}
			if in.Name != nil && *in.Name != "" {
				card.Name = *in.Name
			}
			if in.Description != nil {
				card.Description = *in.Description
			}
			if in.Capabilities != nil {
				card.Capabilities = slices.Clone(*in.Capabilities)
			}
			if in.Pid != nil && *in.Pid > 0 {
				card.Pid = in.Pid
			}
		}
		if card.Capabilities == nil {
			card.Capabilities = []string{}
		}
		c.agents[id] = &card
		c.saveCard(&card)

		if m, ok := c.memberships[id]; ok {
			if m.State == StateAway {
				c.markBack(id)
			}
			res.Recovery = c.buildRecovery(id, 0)
		}
		action := "register"
		if had {
			action = "update"
		}
		c.emit(Event{Type: "agent", Data: map[string]any{"agent": id, "action": action}})
		res.Agent, res.Created = c.view(&card), !had
		return nil
	})
	return
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func (c *Atalk) UpdateCard(id string, p CardPatch) (v AgentView, err error) {
	err = c.guard(func() error {
		card, err := c.requireAgent(id)
		if err != nil {
			return err
		}
		if p.Name != nil {
			card.Name = *p.Name
		}
		if p.Description != nil {
			card.Description = *p.Description
		}
		if p.Capabilities != nil {
			card.Capabilities = slices.Clone(*p.Capabilities)
		}
		card.UpdatedAt = c.ts()
		c.saveCard(card)
		c.emit(Event{Type: "agent", Data: map[string]any{"agent": id, "action": "update"}})
		v = c.view(card)
		return nil
	})
	return
}

func (c *Atalk) Unregister(id string) error {
	return c.guard(func() error {
		if _, err := c.requireAgent(id); err != nil {
			return err
		}
		if _, ok := c.memberships[id]; ok {
			c.leaveInternal(id, "leave", "등록 해제")
		}
		delete(c.agents, id)
		_ = os.Remove(c.cardFile(id))
		c.emit(Event{Type: "agent", Data: map[string]any{"agent": id, "action": "unregister"}})
		return nil
	})
}

func (c *Atalk) GetAgent(id string) (v AgentView, err error) {
	err = c.guard(func() error {
		card, err := c.requireAgent(id)
		if err != nil {
			return err
		}
		v = c.view(card)
		return nil
	})
	return
}

func (c *Atalk) ListAgents(status, kind string) (out []AgentView, err error) {
	err = c.guard(func() error {
		out = []AgentView{}
		for _, card := range c.agents {
			v := c.view(card)
			if (status == "" || v.Status == status) && (kind == "" || v.Kind == kind) {
				out = append(out, v)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].RegisteredAt != out[j].RegisteredAt {
				return out[i].RegisteredAt < out[j].RegisteredAt
			}
			return out[i].ID < out[j].ID
		})
		return nil
	})
	return
}

// ───────────────────────── 대화방 ─────────────────────────

func (c *Atalk) CreateRoom(agent, roomID, title string, maxMessages int) (v RoomView, err error) {
	err = c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		if !IsValidRoomID(roomID) {
			return bad("invalid_room_id", "방 ID 는 문자/숫자/-/_ 로 된 1~64자여야 함")
		}
		if _, ok := c.rooms[roomID]; ok {
			return conflict("room_exists", "이미 있는 방: %s", roomID)
		}
		if _, err := os.Stat(c.roomDir(roomID)); err == nil {
			return conflict("room_exists", "이미 있는 방: %s", roomID)
		}
		if title == "" {
			title = roomID
		}
		if maxMessages <= 0 {
			maxMessages = 1000
		}
		room := &Room{
			RoomID: roomID, Title: title, Owner: ptr(agent), CreatedAt: c.ts(),
			Invites: []Invite{}, Requests: []JoinRequest{}, Policy: Policy{MaxMessages: maxMessages},
		}
		must(ensureDir(filepath.Join(c.roomDir(roomID), "logs")))
		c.rooms[roomID] = room
		c.lastSeq[roomID] = 0
		c.cursors[roomID] = map[string]int{}
		c.saveRoom(room)
		c.saveCursors(roomID)
		must(os.WriteFile(c.messagesFile(roomID), nil, 0o644))
		c.joinInternal(agent, roomID, RoleOwner)
		c.emit(Event{Type: "room", Data: map[string]any{"room": roomID, "action": "create"}})
		v = c.roomView(room)
		return nil
	})
	return
}

func (c *Atalk) GetRoom(r string) (v RoomView, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, false)
		if err != nil {
			return err
		}
		v = c.roomView(room)
		return nil
	})
	return
}

func (c *Atalk) ListRooms(includeClosed bool) (out []RoomView, err error) {
	err = c.guard(func() error {
		out = []RoomView{}
		for _, r := range c.sortedRooms() {
			if includeClosed || r.ClosedAt == nil {
				out = append(out, c.roomView(r))
			}
		}
		return nil
	})
	return
}

func (c *Atalk) Invite(owner, r, target string) (v RoomView, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, true)
		if err != nil {
			return err
		}
		if err := c.requireOwner(owner, room); err != nil {
			return err
		}
		if _, err := c.requireAgent(target); err != nil {
			return err
		}
		if m, ok := c.memberships[target]; ok && m.Room == r {
			return conflict("already_member", "%s 는 이미 이 방 멤버", target)
		}
		for _, i := range room.Invites {
			if i.Agent == target && i.Status == "pending" {
				return conflict("already_invited", "%s 는 이미 초대됨", target)
			}
		}
		room.Invites = append(room.Invites, Invite{Agent: target, By: owner, At: c.ts(), Status: "pending"})
		c.saveRoom(room)
		c.emit(Event{Type: "invite", To: []string{target}, Data: map[string]any{"room": r, "title": room.Title, "by": owner}})
		v = c.roomView(room)
		return nil
	})
	return
}

func (c *Atalk) RespondInvite(agent, r string, accept bool) (v RoomView, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, true)
		if err != nil {
			return err
		}
		idx := slices.IndexFunc(room.Invites, func(i Invite) bool { return i.Agent == agent && i.Status == "pending" })
		if idx < 0 {
			return notFound("no_invite", "%s 방에서 받은 초대가 없음", r)
		}
		if accept {
			room.Invites[idx].Status = "accepted"
		} else {
			room.Invites[idx].Status = "declined"
		}
		c.saveRoom(room)
		if accept {
			c.joinInternal(agent, r, RoleMember)
		} else if room.Owner != nil {
			c.emit(Event{Type: "invite", To: []string{*room.Owner}, Data: map[string]any{"room": r, "agent": agent, "declined": true}})
		}
		v = c.roomView(room)
		return nil
	})
	return
}

func (c *Atalk) RequestJoin(agent, r, note string) (v RoomView, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, true)
		if err != nil {
			return err
		}
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		if m, ok := c.memberships[agent]; ok && m.Room == r {
			return conflict("already_member", "이미 이 방 멤버")
		}
		for _, q := range room.Requests {
			if q.Agent == agent && q.Status == "pending" {
				return conflict("already_requested", "이미 참여 신청함")
			}
		}
		room.Requests = append(room.Requests, JoinRequest{Agent: agent, Note: note, At: c.ts(), Status: "pending"})
		c.saveRoom(room)
		if room.Owner != nil {
			c.emit(Event{Type: "request", To: []string{*room.Owner}, Data: map[string]any{"room": r, "agent": agent, "note": note}})
		}
		v = c.roomView(room)
		return nil
	})
	return
}

func (c *Atalk) RespondRequest(owner, r, target string, approve bool) (v RoomView, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, true)
		if err != nil {
			return err
		}
		if err := c.requireOwner(owner, room); err != nil {
			return err
		}
		idx := slices.IndexFunc(room.Requests, func(q JoinRequest) bool { return q.Agent == target && q.Status == "pending" })
		if idx < 0 {
			return notFound("no_request", "%s 의 참여 신청이 없음", target)
		}
		if approve {
			room.Requests[idx].Status = "approved"
		} else {
			room.Requests[idx].Status = "rejected"
		}
		c.saveRoom(room)
		if approve {
			c.joinInternal(target, r, RoleMember)
		} else {
			c.emit(Event{Type: "request", To: []string{target}, Data: map[string]any{"room": r, "rejected": true}})
		}
		v = c.roomView(room)
		return nil
	})
	return
}

func (c *Atalk) Leave(agent, reason string) error {
	return c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		if _, ok := c.memberships[agent]; !ok {
			return conflict("not_in_room", "들어가 있는 방이 없음")
		}
		if reason == "" {
			reason = "퇴장"
		}
		c.leaveInternal(agent, "leave", reason)
		return nil
	})
}

func (c *Atalk) Pending(agent string) (p Pending, err error) {
	err = c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		p = c.pendingLocked(agent)
		return nil
	})
	return
}

func (c *Atalk) pendingLocked(agent string) Pending {
	p := Pending{Invites: []PendingInvite{}, Requests: []PendingRequest{}}
	for _, room := range c.sortedRooms() {
		if room.ClosedAt != nil {
			continue
		}
		for _, i := range room.Invites {
			if i.Agent == agent && i.Status == "pending" {
				p.Invites = append(p.Invites, PendingInvite{Invite: i, Room: room.RoomID, Title: room.Title})
			}
		}
		if room.Owner != nil && *room.Owner == agent {
			for _, q := range room.Requests {
				if q.Status == "pending" {
					p.Requests = append(p.Requests, PendingRequest{JoinRequest: q, Room: room.RoomID})
				}
			}
		}
	}
	return p
}

// ───────────────────────── 입장 / 퇴장 (내부) ─────────────────────────

// joinInternal: 세션 하나 = 방 하나. 다른 방에 있으면 먼저 그 방에서 나온다.
func (c *Atalk) joinInternal(agent, r, role string) {
	if cur, ok := c.memberships[agent]; ok {
		if cur.Room == r {
			if cur.State == StateAway {
				c.markBack(agent)
			}
			return
		}
		c.leaveInternal(agent, "move", r+" 방으로 이동")
	}
	now := c.ts()
	c.memberships[agent] = &Membership{Room: r, Role: role, State: StateInRoom, JoinedAt: now, LastSeen: now}
	c.saveMemberships()

	c.cursors[r][agent] = c.lastSeq[r] // 입장 이후 메시지부터 읽음
	c.saveCursors(r)

	room := c.rooms[r]
	touched := false
	for i := range room.Invites {
		if room.Invites[i].Agent == agent && room.Invites[i].Status == "pending" {
			room.Invites[i].Status, touched = "accepted", true
		}
	}
	for i := range room.Requests {
		if room.Requests[i].Agent == agent && room.Requests[i].Status == "pending" {
			room.Requests[i].Status, touched = "approved", true
		}
	}
	if touched {
		c.saveRoom(room)
	}
	c.logMembership(MembershipEvent{TS: now, Agent: agent, Room: r, Event: "join", Detail: role})
	suffix := ""
	if role == RoleOwner {
		suffix = " (방장)"
	}
	c.systemMessage(r, c.nameOf(agent)+" 입장"+suffix, false)
	c.emit(Event{Type: "membership", To: []string{agent}, Data: map[string]any{"agent": agent, "room": r, "action": "join", "role": role}})
}

func (c *Atalk) leaveInternal(agent, event, reason string) {
	m, ok := c.memberships[agent]
	if !ok {
		return
	}
	delete(c.memberships, agent)
	c.saveMemberships()
	if cur, ok := c.cursors[m.Room]; ok {
		delete(cur, agent)
		c.saveCursors(m.Room)
	}
	c.logMembership(MembershipEvent{TS: c.ts(), Agent: agent, Room: m.Room, Event: event, Detail: reason})
	c.systemMessage(m.Room, fmt.Sprintf("%s 퇴장 (%s)", c.nameOf(agent), reason), false)
	c.emit(Event{Type: "membership", To: []string{agent}, Data: map[string]any{"agent": agent, "room": m.Room, "action": event, "reason": reason}})
	if room, ok := c.rooms[m.Room]; ok && room.Owner != nil && *room.Owner == agent {
		c.fixOwner(room, "방장 퇴장")
	}
}

// fixOwner: 가장 먼저 들어온 멤버(in_room 우선)에게 방장을 넘기고, 아무도 없으면 방을 닫는다.
func (c *Atalk) fixOwner(room *Room, why string) {
	members := c.membersOf(room.RoomID)
	var next *memberEntry
	for i := range members {
		if members[i].m.State == StateInRoom {
			next = &members[i]
			break
		}
	}
	if next == nil && len(members) > 0 {
		next = &members[0]
	}
	if next != nil {
		room.Owner = ptr(next.agent)
		for _, e := range members {
			if e.agent == next.agent {
				e.m.Role = RoleOwner
			} else {
				e.m.Role = RoleMember
			}
		}
		c.saveMemberships()
		c.saveRoom(room)
		c.logMembership(MembershipEvent{TS: c.ts(), Agent: next.agent, Room: room.RoomID, Event: "owner_transfer", Detail: why})
		c.systemMessage(room.RoomID, fmt.Sprintf("방장 변경 → %s (%s)", c.nameOf(next.agent), why), false)
		c.emit(Event{Type: "membership", To: []string{next.agent}, Data: map[string]any{"agent": next.agent, "room": room.RoomID, "action": "owner"}})
		return
	}
	room.Owner = nil
	room.ClosedAt = ptr(c.ts())
	for i := range room.Invites {
		if room.Invites[i].Status == "pending" {
			room.Invites[i].Status = "cancelled"
		}
	}
	for i := range room.Requests {
		if room.Requests[i].Status == "pending" {
			room.Requests[i].Status = "cancelled"
		}
	}
	c.saveRoom(room)
	c.logMembership(MembershipEvent{TS: c.ts(), Agent: "-", Room: room.RoomID, Event: "close", Detail: why})
	c.systemMessage(room.RoomID, "대화방 닫힘 (남은 멤버 없음)", true)
	c.emit(Event{Type: "room", Data: map[string]any{"room": room.RoomID, "action": "close"}})
}

// ───────────────────────── away / 복귀 / 이어받기 ─────────────────────────

func (c *Atalk) MarkAway(agent, reason string) error {
	return c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		if reason == "" {
			reason = "away"
		}
		c.markAway(agent, reason)
		return nil
	})
}

func (c *Atalk) markAway(agent, reason string) {
	m, ok := c.memberships[agent]
	if !ok || m.State == StateAway {
		return
	}
	m.State, m.AwaySince, m.AwayReason = StateAway, ptr(c.ts()), ptr(reason)
	c.saveMemberships()
	c.logMembership(MembershipEvent{TS: c.ts(), Agent: agent, Room: m.Room, Event: "away", Detail: reason})
	c.systemMessage(m.Room, fmt.Sprintf("%s 자리 비움 (%s)", c.nameOf(agent), reason), false)
}

func (c *Atalk) markBack(agent string) {
	m, ok := c.memberships[agent]
	if !ok || m.State != StateAway {
		return
	}
	m.State, m.AwaySince, m.AwayReason, m.LastSeen = StateInRoom, nil, nil, c.ts()
	c.saveMemberships()
	c.logMembership(MembershipEvent{TS: c.ts(), Agent: agent, Room: m.Room, Event: "back"})
	c.systemMessage(m.Room, c.nameOf(agent)+" 복귀", false)
}

func (c *Atalk) buildRecovery(agent string, recentCount int) *Recovery {
	m, ok := c.memberships[agent]
	if !ok {
		return nil
	}
	rec := &Recovery{Room: m.Room, Role: m.Role, Unread: c.unreadLocked(agent, false).Messages, Unfinished: []Unfinished{}}
	logs, err := readJSONL[LogEntry](c.logFile(m.Room, agent))
	must(err)
	last := map[int]string{}
	var order []int
	for _, e := range logs {
		if _, seen := last[e.Seq]; !seen {
			order = append(order, e.Seq)
		}
		last[e.Seq] = e.State
	}
	var unfinished []int
	for _, seq := range order {
		if s := last[seq]; s == "received" || s == "working" {
			unfinished = append(unfinished, seq)
		}
	}
	if len(unfinished) > 0 || recentCount > 0 {
		all, err := readJSONL[Message](c.messagesFile(m.Room))
		must(err)
		bySeq := map[int]Message{}
		for _, x := range all {
			bySeq[x.Seq] = x
		}
		for _, seq := range unfinished {
			u := Unfinished{Seq: seq, State: last[seq]}
			if msg, ok := bySeq[seq]; ok {
				u.Message = &msg
			}
			rec.Unfinished = append(rec.Unfinished, u)
		}
		if recentCount > 0 {
			rec.Recent = all[max(0, len(all)-recentCount):]
		}
	}
	return rec
}

// Takeover: 새 세션이 죽은 이전 세션의 자리를 이어받는다 (재개 시 세션 ID 가 바뀐 경우).
// 조건: 같은 kind, 같은 cwd(둘 다 있을 때), 이전 세션이 away 이거나 프로세스가 죽었음, 새 세션은 방이 없음.
func (c *Atalk) Takeover(newID, oldID string) (rec *Recovery, err error) {
	err = c.guard(func() error {
		nu, err := c.requireAgent(newID)
		if err != nil {
			return err
		}
		old, err := c.requireAgent(oldID)
		if err != nil {
			return err
		}
		if newID == oldID {
			return bad("same_agent", "자기 자신은 이어받을 수 없음")
		}
		if nu.Kind != old.Kind {
			return forbidden("kind_mismatch", "같은 종류의 에이전트만 이어받을 수 있음")
		}
		if nu.Cwd != nil && old.Cwd != nil && !samePath(*nu.Cwd, *old.Cwd) {
			return forbidden("cwd_mismatch", "같은 작업 폴더의 세션만 이어받을 수 있음")
		}
		m, ok := c.memberships[oldID]
		if !ok {
			return conflict("not_in_room", "%s 는 들어가 있는 방이 없음", oldID)
		}
		oldDead := m.State == StateAway || (old.Pid != nil && !c.isAlive(*old.Pid))
		if !oldDead {
			return conflict("still_alive", "%s 가 아직 살아 있음", oldID)
		}
		if _, ok := c.memberships[newID]; ok {
			return conflict("already_in_room", "새 세션이 이미 다른 방에 있음. 먼저 나와야 함")
		}
		now := c.ts()
		delete(c.memberships, oldID)
		nm := *m
		nm.State, nm.AwaySince, nm.AwayReason, nm.LastSeen = StateInRoom, nil, nil, now
		c.memberships[newID] = &nm
		c.saveMemberships()

		cur := c.cursors[m.Room]
		if v, ok := cur[oldID]; ok {
			cur[newID] = v
		} else {
			cur[newID] = c.lastSeq[m.Room]
		}
		delete(cur, oldID)
		c.saveCursors(m.Room)

		room := c.rooms[m.Room]
		if room.Owner != nil && *room.Owner == oldID {
			room.Owner = ptr(newID)
			c.saveRoom(room)
		}
		// 이전 세션의 처리 기록을 이어받아야 미완료 작업이 보인다
		oldLogs, err := readJSONL[LogEntry](c.logFile(m.Room, oldID))
		must(err)
		for _, e := range oldLogs {
			e.Agent = newID
			must(appendJSONL(c.logFile(m.Room, newID), e))
		}
		c.logMembership(MembershipEvent{TS: now, Agent: newID, Room: m.Room, Event: "takeover", Detail: "from " + oldID})
		c.systemMessage(m.Room, fmt.Sprintf("%s 가 %s 세션을 이어받음", c.nameOf(newID), c.nameOf(oldID)), false)
		c.emit(Event{Type: "membership", To: []string{newID}, Data: map[string]any{"agent": newID, "room": m.Room, "action": "takeover", "from": oldID}})
		rec = c.buildRecovery(newID, 30)
		return nil
	})
	return
}

func samePath(a, b string) bool {
	pa, _ := filepath.Abs(a)
	pb, _ := filepath.Abs(b)
	return strings.EqualFold(filepath.Clean(pa), filepath.Clean(pb))
}

// Sweep: in_room 인데 프로세스가 죽었으면 away, away 가 유예 시간을 넘으면 강제 퇴장.
func (c *Atalk) Sweep() error {
	return c.guard(func() error {
		now := c.now()
		agents := make([]string, 0, len(c.memberships))
		for a := range c.memberships {
			agents = append(agents, a)
		}
		sort.Strings(agents)
		for _, agent := range agents {
			m, ok := c.memberships[agent]
			if !ok {
				continue
			}
			card, ok := c.agents[agent]
			if !ok {
				c.leaveInternal(agent, "expire", "카드 없음")
				continue
			}
			if m.State == StateInRoom && card.Pid != nil && !c.isAlive(*card.Pid) {
				c.markAway(agent, "process_exit")
			} else if m.State == StateAway && m.AwaySince != nil {
				if t, err := time.Parse(time.RFC3339Nano, *m.AwaySince); err == nil && now.Sub(t) > c.AwayGrace {
					c.leaveInternal(agent, "expire", "자리 비움 유예 시간 초과")
				}
			}
		}
		return nil
	})
}

// ───────────────────────── 메시지 ─────────────────────────

func (c *Atalk) appendMessage(r string, msg Message) Message {
	msg.Seq = c.lastSeq[r] + 1
	msg.ID, msg.TS, msg.Room = newID(), c.ts(), r
	if msg.To == nil {
		msg.To = []string{}
	}
	must(appendJSONL(c.messagesFile(r), msg))
	c.lastSeq[r] = msg.Seq
	c.emit(Event{Type: "message", To: c.memberIDs(r, msg.From), Data: msg})
	return msg
}

func (c *Atalk) systemMessage(r, body string, force bool) {
	room, ok := c.rooms[r]
	if !ok || (room.ClosedAt != nil && !force) {
		return
	}
	c.appendMessage(r, Message{From: "system", Type: "system", Body: body})
}

func (c *Atalk) Post(agent, r string, in PostInput) (msg Message, err error) {
	err = c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		room, err := c.requireRoom(r, true)
		if err != nil {
			return err
		}
		m, ok := c.memberships[agent]
		if !ok || m.Room != r {
			return forbidden("not_member", "%s 방 멤버가 아님", r)
		}
		typ := in.Type
		if typ == "" {
			typ = "chat"
		}
		if !slices.Contains(AgentMessageTypes, typ) {
			return bad("invalid_type", "type 은 %s 중 하나", strings.Join(AgentMessageTypes, "|"))
		}
		if strings.TrimSpace(in.Body) == "" {
			return bad("empty_body", "메시지 본문이 비어 있음")
		}
		for _, t := range in.To {
			if tm, ok := c.memberships[t]; !ok || tm.Room != r {
				return bad("invalid_to", "받는 대상 %s 는 이 방 멤버가 아님", t)
			}
		}
		if c.lastSeq[r] >= room.Policy.MaxMessages {
			return &Error{429, "room_limit", fmt.Sprintf("방 메시지 한도(%d) 초과. 방장이 한도를 올리거나 새 방을 만들어야 함", room.Policy.MaxMessages)}
		}
		m.LastSeen = c.ts()
		if m.State == StateAway {
			c.markBack(agent)
		} else {
			c.saveMemberships()
		}
		msg = c.appendMessage(r, Message{From: agent, Type: typ, To: slices.Clone(in.To), ReplyTo: in.ReplyTo, Body: in.Body})
		return nil
	})
	return
}

func (c *Atalk) ReadMessages(r string, after, limit int) (out []Message, err error) {
	err = c.guard(func() error {
		if _, err := c.requireRoom(r, false); err != nil {
			return err
		}
		all, err := readJSONL[Message](c.messagesFile(r))
		must(err)
		limit = max(1, min(limit, 1000))
		out = []Message{}
		for _, m := range all {
			if m.Seq > after && len(out) < limit {
				out = append(out, m)
			}
		}
		return nil
	})
	return
}

// Unread: 내 방의 cursor 이후 메시지 (내가 쓴 것 제외). for_me = 나에게 처리 책임이 있는 메시지.
func (c *Atalk) Unread(agent string, ack bool) (u Unread, err error) {
	err = c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		u = c.unreadLocked(agent, ack)
		return nil
	})
	return
}

func (c *Atalk) unreadLocked(agent string, ack bool) Unread {
	m, ok := c.memberships[agent]
	if !ok {
		return Unread{Messages: []InboxMessage{}}
	}
	cur := c.cursors[m.Room]
	cursor := cur[agent]
	all, err := readJSONL[Message](c.messagesFile(m.Room))
	must(err)
	msgs := []InboxMessage{}
	for _, x := range all {
		if x.Seq > cursor && x.From != agent {
			forMe := x.Type != "system" && (len(x.To) == 0 || slices.Contains(x.To, agent))
			msgs = append(msgs, InboxMessage{Message: x, ForMe: forMe})
		}
	}
	newCursor := cursor
	if ack {
		newCursor = c.lastSeq[m.Room]
		if newCursor != cursor {
			cur[agent] = newCursor
			c.saveCursors(m.Room)
		}
	}
	m.LastSeen = c.ts()
	return Unread{Room: ptr(m.Room), Cursor: newCursor, Messages: msgs}
}

func (c *Atalk) Ack(agent string, seq int) (cursor int, err error) {
	err = c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		m, ok := c.memberships[agent]
		if !ok {
			return conflict("not_in_room", "들어가 있는 방이 없음")
		}
		cur := c.cursors[m.Room]
		target := min(seq, c.lastSeq[m.Room])
		if target > cur[agent] {
			cur[agent] = target
			c.saveCursors(m.Room)
		}
		cursor = cur[agent]
		return nil
	})
	return
}

// ───────────────────────── 처리 기록 ─────────────────────────

func (c *Atalk) Log(agent, r string, seq int, state, note string) (e LogEntry, err error) {
	err = c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		if _, err := c.requireRoom(r, false); err != nil {
			return err
		}
		if m, ok := c.memberships[agent]; !ok || m.Room != r {
			return forbidden("not_member", "%s 방 멤버가 아님", r)
		}
		if seq < 1 || seq > c.lastSeq[r] {
			return bad("invalid_seq", "없는 메시지 번호: %d", seq)
		}
		if !slices.Contains(LogStates, state) {
			return bad("invalid_state", "state 는 %s 중 하나", strings.Join(LogStates, "|"))
		}
		e = LogEntry{TS: c.ts(), Agent: agent, Seq: seq, State: state, Note: note}
		must(appendJSONL(c.logFile(r, agent), e))
		c.emit(Event{Type: "log", Data: map[string]any{"room": r, "ts": e.TS, "agent": agent, "seq": seq, "state": state, "note": note}})
		return nil
	})
	return
}

func (c *Atalk) ReadLogs(r, agent string) (out []LogEntry, err error) {
	err = c.guard(func() error {
		if _, err := c.requireRoom(r, false); err != nil {
			return err
		}
		out = []LogEntry{}
		if agent != "" {
			if !IsValidAgentID(agent) {
				return bad("invalid_agent", "잘못된 agent id")
			}
			l, err := readJSONL[LogEntry](c.logFile(r, agent))
			must(err)
			out = append(out, l...)
			return nil
		}
		dir := filepath.Join(c.roomDir(r), "logs")
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				l, err := readJSONL[LogEntry](filepath.Join(dir, e.Name()))
				must(err)
				out = append(out, l...)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
		return nil
	})
	return
}

// HasWork: 지금 바로 처리할 메시지(for_me)가 있는가.
// 대기 중인 초대/신청은 계속 남아 있으므로 보지 않는다 (새 이벤트가 오면 wait 이 깨어남).
func (c *Atalk) HasWork(agent string) bool {
	has := false
	_ = c.guard(func() error {
		if _, ok := c.agents[agent]; !ok {
			return nil
		}
		for _, m := range c.unreadLocked(agent, false).Messages {
			if m.ForMe {
				has = true
				break
			}
		}
		return nil
	})
	return has
}
