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

// Atalk 은 데몬 안에서 단독으로 상태를 바꾼다. 상태는 메모리에 있고 바뀐 것은 Store(SQLite)에 저장한다.
// 모든 공개 메서드는 mu 로 직렬화되고, 메서드 하나가 트랜잭션 하나다.
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
	pastNames   map[string]string // 지운 카드의 이름 (지난 대화에서 이름을 보여 주기 위해)
	trashSwept  time.Time         // 휴지통을 마지막으로 정리한 시각 (한 시간에 한 번)
	cfg         Config            // 설정 (DB meta.config 의 사본)

	st      *sqliteStore // 저장 계층 (<root>/atalk.db)
	inGuard bool
	pending []Event // 트랜잭션이 확정된 뒤에 내보낼 이벤트

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
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	st, err := openSQLite(filepath.Join(root, "atalk.db"))
	if err != nil {
		return nil, err
	}
	c.st = st
	// 예전 파일 형식 데이터가 있으면 한 번 가져온다
	if hasLegacyFiles(root) {
		err = c.guard(func() error {
			snap, err := c.st.Load()
			if err != nil || snap.imported {
				return err
			}
			importLegacy(root, c.st, c.ts())
			return nil
		})
		if err != nil {
			st.Close()
			return nil, fmt.Errorf("예전 파일 데이터를 가져오지 못함: %w", err)
		}
		moveLegacy(root, c.now())
	}
	if err = c.guard(func() error { c.load(); return nil }); err != nil {
		st.Close()
		return nil, err
	}
	return c, nil
}

// Close 는 DB 를 닫는다.
func (c *Atalk) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Close()
}

// guard 는 잠금을 잡고 fn 을 트랜잭션 하나로 실행한다.
// 성공하면 확정하고 이벤트를 내보낸다. 오류나 저장 실패(panic)면 되돌리고, 메모리 상태도 DB 에서 다시 읽는다.
func (c *Atalk) guard(fn func() error) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inGuard, c.pending = true, nil
	defer func() {
		c.inGuard = false
		r := recover()
		if r != nil {
			f, ok := r.(ioFailure)
			if !ok {
				c.st.Rollback()
				panic(r)
			}
			err = &Error{500, "io_error", f.err.Error()}
		}
		if err == nil {
			if cerr := c.st.Commit(); cerr != nil {
				err = &Error{500, "io_error", cerr.Error()}
			}
		}
		if err != nil {
			c.st.Rollback()
			c.pending = nil
			if r != nil || isIOError(err) { // 메모리만 바뀌고 저장은 안 됐을 수 있으므로 DB 기준으로 되돌린다
				c.reload()
			}
			return
		}
		evs := c.pending
		c.pending = nil
		for _, e := range evs {
			c.send(e)
		}
	}()
	return fn()
}

func isIOError(err error) bool {
	e, ok := err.(*Error)
	return ok && e.Code == "io_error"
}

// reload 는 메모리 상태를 DB 에서 다시 읽는다 (트랜잭션 실패 후).
func (c *Atalk) reload() {
	defer func() { _ = recover(); c.st.Rollback() }()
	c.agents, c.memberships, c.rooms = map[string]*AgentCard{}, map[string]*Membership{}, map[string]*Room{}
	c.lastSeq, c.cursors = map[string]int{}, map[string]map[string]int{}
	c.load()
	_ = c.st.Commit()
}

// ───────────────────────── 이벤트 구독 ─────────────────────────

// Subscribe 는 모든 이벤트를 받는 채널을 돌려준다. 다 쓰면 Unsubscribe 해야 한다.
// Exclusive 는 다른 요청이 상태를 바꾸지 못하게 잠근 채로 fn 을 실행한다 (데이터 폴더 복사 등).
func (c *Atalk) Exclusive(fn func() error) error { return c.guard(fn) }

// BackupDB 는 DB 의 일관된 복사본을 path 에 만든다 (데이터 폴더 이동, 백업).
func (c *Atalk) BackupDB(path string) error {
	return c.guard(func() error { return c.st.BackupTo(path) })
}

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

// emit 은 이벤트를 모아 두었다가 트랜잭션이 확정되면 내보낸다.
func (c *Atalk) emit(e Event) {
	if e.To == nil {
		e.To = []string{}
	}
	if c.inGuard {
		c.pending = append(c.pending, e)
		return
	}
	c.send(e)
}

func (c *Atalk) send(e Event) {
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
	snap, err := c.st.Load()
	must(err)
	c.pastNames = snap.names
	c.cfg = snap.config
	for _, a := range snap.agents {
		c.agents[a.ID] = a
	}
	for _, r := range snap.rooms {
		c.rooms[r.RoomID] = r
		c.lastSeq[r.RoomID] = snap.lastSeq[r.RoomID]
		cur := snap.cursors[r.RoomID]
		if cur == nil {
			cur = map[string]int{}
		}
		c.cursors[r.RoomID] = cur
	}
	c.memberships = snap.memberships
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

func (c *Atalk) saveCard(card *AgentCard)        { c.st.PutAgent(card) }
func (c *Atalk) saveMemberships()                { c.st.PutMemberships(c.memberships) }
func (c *Atalk) saveRoom(room *Room)             { c.st.PutRoom(room) }
func (c *Atalk) saveCursors(r string)            { c.st.PutCursors(r, c.cursors[r]) }
func (c *Atalk) logMembership(e MembershipEvent) { c.st.AppendMembershipEvent(e) }

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
	if n, ok := c.pastNames[id]; ok {
		return n
	}
	return id
}

// Names 는 지금 있는 카드와 지운 카드의 id → 이름 (지난 대화 표시용).
func (c *Atalk) Names() (out map[string]string, err error) {
	err = c.guard(func() error {
		out = make(map[string]string, len(c.pastNames)+len(c.agents))
		for id, n := range c.pastNames {
			out[id] = n
		}
		for id, a := range c.agents {
			out[id] = a.Name
		}
		return nil
	})
	return
}

// removeCard 는 카드를 지우고 이름은 names.json 에 남긴다.
func (c *Atalk) removeCard(id, reason string) {
	card, ok := c.agents[id]
	if !ok {
		return
	}
	c.pastNames[id] = card.Name
	c.st.PutName(id, card.Name)
	delete(c.agents, id)
	c.st.DeleteAgent(id)
	c.logMembership(MembershipEvent{TS: c.ts(), Agent: id, Room: "-", Event: "unregister", Detail: reason})
	c.emit(Event{Type: "agent", Data: map[string]any{"agent": id, "action": "unregister"}})
}

func (c *Atalk) statusOf(id string) string {
	if m, ok := c.memberships[id]; ok {
		return m.State
	}
	if card, ok := c.agents[id]; ok && card.RetiredBy != nil {
		return "offline"
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
		v.Members = append(v.Members, RoomMember{Agent: e.agent, Name: c.nameOf(e.agent), Role: e.m.Role, State: e.m.State, Cursor: c.cursors[room.RoomID][e.agent]})
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
			card.RetiredBy = nil
			card.OfflineSince = nil
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
				Name: defaultName(&AgentCard{Kind: in.Kind, SessionID: sid}), Capabilities: []string{},
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

// defaultName 은 등록할 때 이름을 주지 않았을 때 붙는 이름.
func defaultName(a *AgentCard) string { return fmt.Sprintf("%s-%s", a.Kind, truncate(a.SessionID, 8)) }

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
		if p.Human != nil {
			if h := strings.TrimSpace(*p.Human); h == "" {
				card.Human = nil
			} else if err := checkAlias(h); err != nil {
				return err
			} else {
				card.Human = &h
			}
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
		c.removeCard(id, "등록 해제")
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
		if title == "" {
			title = roomID
		}
		if maxMessages <= 0 {
			maxMessages = 1000
			if cfg := c.readConfig(); cfg.DefaultMaxMessages != nil {
				maxMessages = *cfg.DefaultMaxMessages
			}
		}
		room := &Room{
			RoomID: roomID, Title: title, Owner: ptr(agent), CreatedAt: c.ts(),
			Invites: []Invite{}, Requests: []JoinRequest{}, Policy: Policy{MaxMessages: maxMessages},
		}
		c.rooms[roomID] = room
		c.lastSeq[roomID] = 0
		c.cursors[roomID] = map[string]int{}
		c.saveRoom(room)
		c.saveCursors(roomID)
		c.joinInternal(agent, roomID, RoleOwner)
		c.emit(Event{Type: "room", Data: map[string]any{"room": roomID, "action": "create"}})
		v = c.roomView(room)
		return nil
	})
	return
}

// RenameRoom 은 방 제목을 바꾼다 (방 ID 는 폴더 이름이라 바꾸지 않음). by 는 바꾼 에이전트, 사람이 화면/CLI 에서 바꾸면 "".
func (c *Atalk) RenameRoom(r, title, by string) (v RoomView, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, false)
		if err != nil {
			return err
		}
		title = strings.TrimSpace(title)
		if title == "" || len([]rune(title)) > 100 || strings.ContainsAny(title, "\r\n") {
			return bad("invalid_title", "제목은 1~100자, 줄바꿈 불가")
		}
		if old := room.Title; old != title {
			room.Title = title
			c.saveRoom(room)
			who := ""
			if by != "" {
				who = " (" + c.nameOf(by) + ")"
			}
			c.systemMessage(r, fmt.Sprintf("제목 변경: %s → %s%s", old, title, who), false)
			c.emit(Event{Type: "room", Data: map[string]any{"room": r, "action": "rename", "title": title}})
		}
		v = c.roomView(room)
		return nil
	})
	return
}

// SetRoomLimit 는 방의 메시지 한도를 바꾼다.
func (c *Atalk) SetRoomLimit(r string, n int) (v RoomView, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, false)
		if err != nil {
			return err
		}
		if n < 10 || n > 1000000 {
			return bad("invalid_limit", "메시지 한도는 10 ~ 1000000")
		}
		if room.Policy.MaxMessages != n {
			room.Policy.MaxMessages = n
			c.saveRoom(room)
			c.systemMessage(r, fmt.Sprintf("메시지 한도 변경: %d", n), false)
			c.emit(Event{Type: "room", Data: map[string]any{"room": r, "action": "limit", "max_messages": n}})
		}
		v = c.roomView(room)
		return nil
	})
	return
}

// TrashInfo 는 휴지통(삭제한 방)의 개수와 크기.
func (c *Atalk) TrashInfo() (count int, size int64) {
	_ = c.guard(func() error { count, size = c.st.TrashInfo(); return nil })
	return
}

// EmptyTrash 는 휴지통을 비운다. olderThan > 0 이면 그보다 오래된 것만.
func (c *Atalk) EmptyTrash(olderThan time.Duration) (removed int, err error) {
	err = c.guard(func() error {
		before := ""
		if olderThan > 0 {
			before = c.now().Add(-olderThan).UTC().Format("2006-01-02T15:04:05.000Z")
		}
		removed = c.st.EmptyTrash(before)
		return nil
	})
	return
}

// DeleteRoom 은 방을 지운다. 멤버는 모두 방에서 나오고(대기 상태), 대화 기록은 휴지통으로 간다.
// 휴지통은 설정의 보관 기간(기본 30일)이 지나면 비워진다.
func (c *Atalk) DeleteRoom(r string) (trash string, err error) {
	err = c.guard(func() error {
		room, err := c.requireRoom(r, false)
		if err != nil {
			return err
		}
		now := c.ts()
		var members []string
		for _, e := range c.membersOf(r) {
			members = append(members, e.agent)
			delete(c.memberships, e.agent)
			c.logMembership(MembershipEvent{TS: now, Agent: e.agent, Room: r, Event: "room_deleted", Detail: "방 삭제"})
		}
		c.saveMemberships()
		id := c.st.TrashRoom(room, now)
		trash = fmt.Sprintf("휴지통 #%d", id)
		delete(c.rooms, r)
		delete(c.lastSeq, r)
		delete(c.cursors, r)
		c.logMembership(MembershipEvent{TS: now, Agent: "-", Room: r, Event: "delete", Detail: trash})
		for _, a := range members {
			c.emit(Event{Type: "membership", To: []string{a}, Data: map[string]any{"agent": a, "room": r, "action": "leave", "reason": "방 삭제"}})
		}
		c.emit(Event{Type: "room", Data: map[string]any{"room": r, "action": "delete"}})
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
	logs := c.st.Logs(m.Room, agent)
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
		bySeq := c.st.MessagesBySeq(m.Room, unfinished)
		for _, seq := range unfinished {
			u := Unfinished{Seq: seq, State: last[seq]}
			if msg, ok := bySeq[seq]; ok {
				u.Message = &msg
			}
			rec.Unfinished = append(rec.Unfinished, u)
		}
		if recentCount > 0 {
			rec.Recent = c.st.Messages(m.Room, max(0, c.lastSeq[m.Room]-recentCount), 0)
		}
	}
	return rec
}

// Takeover: 새 세션이 죽은 이전 세션의 자리를 이어받는다 (재개 시 세션 ID 가 바뀐 경우).
// 조건: 같은 kind, 같은 cwd(둘 다 있을 때), 이전 세션이 away 이거나 프로세스가 죽었음, 새 세션은 방이 없음.
func (c *Atalk) Takeover(newID, oldID string) (rec *Recovery, err error) {
	err = c.guard(func() error {
		var err error
		rec, err = c.takeoverLocked(newID, oldID)
		return err
	})
	return
}

// ResumePrevious 는 새로 시작한 세션이 같은 종류·같은 작업 폴더의 이전 세션이 들어가 있던 방 자리를 자동으로 이어받게 한다.
// 이전 세션 후보: 방에 있고, 자리 비움이거나 프로세스가 죽었거나, 같은 프로세스(PID)의 이전 세션(Claude /clear, opencode 새 세션).
// 후보가 여럿이면 가장 최근에 활동한 세션. 이어받을 것이 없으면 no_previous.
func (c *Atalk) ResumePrevious(newID string) (rec *Recovery, from string, err error) {
	err = c.guard(func() error {
		nu, err := c.requireAgent(newID)
		if err != nil {
			return err
		}
		if _, ok := c.memberships[newID]; ok {
			return conflict("already_in_room", "이미 방에 있음")
		}
		if cfg := c.readConfig(); cfg.AutoResume != nil && !*cfg.AutoResume {
			return notFound("no_previous", "자동 이어받기가 꺼져 있음 (설정)")
		}
		if nu.Cwd == nil {
			return notFound("no_previous", "작업 폴더 정보가 없어 이전 세션을 찾을 수 없음")
		}
		type cand struct {
			id   string
			seen string
		}
		var cands []cand
		for id, m := range c.memberships {
			old, ok := c.agents[id]
			if !ok || id == newID || old.Kind != nu.Kind || old.Cwd == nil || !samePath(*old.Cwd, *nu.Cwd) {
				continue
			}
			samePid := nu.Pid != nil && old.Pid != nil && *nu.Pid == *old.Pid
			if m.State == StateAway || (old.Pid != nil && !c.isAlive(*old.Pid)) || samePid {
				cands = append(cands, cand{id, m.LastSeen})
			}
		}
		if len(cands) == 0 {
			return notFound("no_previous", "이어받을 이전 세션 없음")
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].seen > cands[j].seen })
		from = cands[0].id
		rec, err = c.takeoverLocked(newID, from)
		return err
	})
	return
}

func (c *Atalk) takeoverLocked(newID, oldID string) (rec *Recovery, err error) {
	{
		nu, err := c.requireAgent(newID)
		if err != nil {
			return nil, err
		}
		old, err := c.requireAgent(oldID)
		if err != nil {
			return nil, err
		}
		if newID == oldID {
			return nil, bad("same_agent", "자기 자신은 이어받을 수 없음")
		}
		if nu.Kind != old.Kind {
			return nil, forbidden("kind_mismatch", "같은 종류의 에이전트만 이어받을 수 있음")
		}
		if nu.Cwd != nil && old.Cwd != nil && !samePath(*nu.Cwd, *old.Cwd) {
			return nil, forbidden("cwd_mismatch", "같은 작업 폴더의 세션만 이어받을 수 있음")
		}
		m, ok := c.memberships[oldID]
		if !ok {
			return nil, conflict("not_in_room", "%s 는 들어가 있는 방이 없음", oldID)
		}
		// 같은 프로세스의 이전 세션(Claude /clear, opencode 새 세션)은 살아 있어도 더는 쓰이지 않으므로 이어받을 수 있다
		samePid := nu.Pid != nil && old.Pid != nil && *nu.Pid == *old.Pid
		oldDead := m.State == StateAway || (old.Pid != nil && !c.isAlive(*old.Pid)) || samePid
		if !oldDead {
			return nil, conflict("still_alive", "%s 가 아직 살아 있음", oldID)
		}
		if _, ok := c.memberships[newID]; ok {
			return nil, conflict("already_in_room", "새 세션이 이미 다른 방에 있음. 먼저 나와야 함")
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
		c.st.CopyLogs(m.Room, oldID, newID)
		// 사람이 정해 둔 이름/설명/능력/별칭은 새 세션이 이어받는다 (새 세션이 기본값일 때만)
		if old.Name != defaultName(old) && (nu.Name == defaultName(nu) || nu.Name == "") {
			nu.Name = old.Name
		}
		if nu.Description == "" {
			nu.Description = old.Description
		}
		if len(nu.Capabilities) == 0 && len(old.Capabilities) > 0 {
			nu.Capabilities = slices.Clone(old.Capabilities)
		}
		if nu.Human == nil {
			nu.Human = old.Human
		}
		nu.UpdatedAt = now
		c.saveCard(nu)
		// 이전 세션 카드는 남겨 두되(지난 메시지의 이름 표시용) 끝난 세션으로 표시
		old.RetiredBy = ptr(newID)
		old.UpdatedAt = now
		c.saveCard(old)
		c.logMembership(MembershipEvent{TS: now, Agent: newID, Room: m.Room, Event: "takeover", Detail: "from " + oldID})
		c.systemMessage(m.Room, fmt.Sprintf("%s 가 %s 세션을 이어받음", c.nameOf(newID), c.nameOf(oldID)), false)
		c.emit(Event{Type: "membership", To: []string{newID}, Data: map[string]any{"agent": newID, "room": m.Room, "action": "takeover", "from": oldID}})
		return c.buildRecovery(newID, 30), nil
	}
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
			} else if grace := c.awayGrace(); m.State == StateAway && m.AwaySince != nil && grace > 0 {
				if t, err := time.Parse(time.RFC3339Nano, *m.AwaySince); err == nil && now.Sub(t) > grace {
					c.leaveInternal(agent, "expire", "자리 비움 유예 시간 초과")
				}
			}
		}
		c.sweepOffline(now)
		if now.Sub(c.trashSwept) > time.Hour {
			c.trashSwept = now
			days := 30
			if cfg := c.readConfig(); cfg.TrashRetentionDays != nil {
				days = *cfg.TrashRetentionDays
			}
			if days > 0 {
				c.st.EmptyTrash(now.Add(-time.Duration(days) * 24 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"))
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
	c.st.AppendMessage(msg)
	c.lastSeq[r] = msg.Seq
	to := c.memberIDs(r, msg.From)
	if msg.Human != "" && slices.Contains(msg.To, msg.From) {
		to = append(to, msg.From)
	}
	c.emit(Event{Type: "message", To: to, Data: msg})
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
		out := Message{From: agent, Type: typ, To: slices.Clone(in.To), ReplyTo: in.ReplyTo, Body: in.Body}
		if in.Human {
			out.Human = c.humanOf(agent)
		}
		msg = c.appendMessage(r, out)
		return nil
	})
	return
}

// SplitMentions 는 본문 앞쪽의 "@이름" 토큰들을 떼어 낸다. "@오프1 @오프2 안녕" → [오프1 오프2], "안녕".
func SplitMentions(text string) (mentions []string, body string) {
	rest := strings.TrimSpace(text)
	for strings.HasPrefix(rest, "@") {
		end := strings.IndexAny(rest, " \t\r\n")
		if end < 0 {
			end = len(rest)
		}
		for _, m := range strings.Split(rest[1:end], ",") {
			if m = strings.TrimSpace(strings.TrimPrefix(m, "@")); m != "" {
				mentions = append(mentions, m)
			}
		}
		rest = strings.TrimSpace(rest[end:])
	}
	return mentions, rest
}

// IsAllMention 은 방 전체를 뜻하는 @이름 인지 본다.
func IsAllMention(m string) bool {
	switch strings.ToLower(m) {
	case "모두", "전체", "all", "everyone":
		return true
	}
	return false
}

// Say 는 사람이 agent 를 통해 room 에 직접 쓴 말을 올린다. 앞쪽 "@이름"(이름 또는 id)은 받는 대상.
func (c *Atalk) Say(agent, room, text string) (Message, error) {
	mentions, body := SplitMentions(text)
	if body == "" {
		return Message{}, bad("empty_body", "보낼 내용이 없음")
	}
	// @모두 / @all → 방 전체 (다른 대상과 같이 쓰면 방 전체가 이긴다)
	for _, m := range mentions {
		if IsAllMention(m) {
			mentions = nil
			break
		}
	}
	var to []string
	if len(mentions) > 0 {
		err := c.guard(func() error {
			var members []string
			for _, e := range c.membersOf(room) {
				members = append(members, e.agent)
			}
			for _, want := range mentions {
				found := ""
				for _, id := range members {
					if n := c.nameOf(id); id == want || n == want || strings.EqualFold(n, want) {
						found = id
						break
					}
				}
				if found == "" {
					var list []string
					for _, id := range members {
						if id != agent {
							list = append(list, fmt.Sprintf("%s(%s)", c.nameOf(id), id))
						}
					}
					return bad("unknown_mention", "%s 방에 @%s 가 없음. 멤버: %s", room, want, strings.Join(list, ", "))
				}
				to = append(to, found)
			}
			return nil
		})
		if err != nil {
			return Message{}, err
		}
	}
	return c.Post(agent, room, PostInput{Type: "chat", To: to, Body: body, Human: true})
}

// humanOf 는 에이전트를 쓰는 사람의 별칭: 카드 human → config.json user → "사용자".
func (c *Atalk) humanOf(agent string) string {
	if card, ok := c.agents[agent]; ok && card.Human != nil && *card.Human != "" {
		return *card.Human
	}
	if cfg := c.readConfig(); cfg.User != "" {
		return cfg.User
	}
	return "사용자"
}

// sweepOffline 은 offline 이 된 시각을 카드에 적고, offline 이 오래된(기본 24시간) 카드는 지운다.
// 방에 있는(in_room/away) 카드는 지우지 않는다. pid 를 모르는 카드(CLI 로 등록한 사람 등)는 offline 이 되지 않으므로 지워지지 않는다.
func (c *Atalk) sweepOffline(now time.Time) {
	ttl := 24 * time.Hour
	if cfg := c.readConfig(); cfg.OfflineTTLHours != nil {
		ttl = time.Duration(*cfg.OfflineTTLHours) * time.Hour
	}
	ids := make([]string, 0, len(c.agents))
	for id := range c.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		card := c.agents[id]
		if _, inRoom := c.memberships[id]; inRoom {
			continue
		}
		off := c.statusOf(id) == "offline"
		switch {
		case off && card.OfflineSince == nil:
			card.OfflineSince = ptr(c.ts())
			c.saveCard(card)
		case !off && card.OfflineSince != nil:
			card.OfflineSince = nil
			c.saveCard(card)
		case off && ttl > 0:
			if t, err := time.Parse(time.RFC3339Nano, *card.OfflineSince); err == nil && now.Sub(t) > ttl {
				c.removeCard(id, "offline 이 오래되어 자동 삭제")
			}
		}
	}
}

// awayGrace 는 자리 비움 후 자동 퇴장까지의 시간. config.json 의 away_grace_min 이 데몬 옵션보다 우선. 0 이면 자동 퇴장 안 함.
func (c *Atalk) awayGrace() time.Duration {
	if cfg := c.readConfig(); cfg.AwayGraceMin != nil {
		return time.Duration(*cfg.AwayGraceMin) * time.Minute
	}
	return c.AwayGrace
}

func (c *Atalk) readConfig() Config { return c.cfg }

// checkAlias: 표시에 "@" 를 구분자로 쓰므로 별칭에는 @ 와 줄바꿈을 허용하지 않는다.
func checkAlias(s string) error {
	if s == "" || len([]rune(s)) > 32 || strings.ContainsAny(s, "@\r\n") {
		return bad("invalid_alias", "별칭은 1~32자, @ 와 줄바꿈 불가")
	}
	return nil
}

func (c *Atalk) GetConfig() (cfg Config, err error) {
	err = c.guard(func() error { cfg = c.readConfig(); return nil })
	return
}

// SetConfig 는 설정 전체를 바꾼다 (사용자 별칭, 자리 비움 유예 시간).
func (c *Atalk) SetConfig(in Config) (cfg Config, err error) {
	err = c.guard(func() error {
		in.User = strings.TrimSpace(in.User)
		if in.User != "" {
			if err := checkAlias(in.User); err != nil {
				return err
			}
		}
		if in.AwayGraceMin != nil && (*in.AwayGraceMin < 0 || *in.AwayGraceMin > 60*24*365) {
			return bad("invalid_grace", "away_grace_min 은 0(자동 퇴장 안 함) ~ 525600 분")
		}
		if in.OfflineTTLHours != nil && (*in.OfflineTTLHours < 0 || *in.OfflineTTLHours > 24*3650) {
			return bad("invalid_ttl", "offline_ttl_hours 는 0(지우지 않음) ~ 87600 시간")
		}
		if in.DefaultMaxMessages != nil && (*in.DefaultMaxMessages < 10 || *in.DefaultMaxMessages > 1000000) {
			return bad("invalid_limit", "default_max_messages 는 10 ~ 1000000")
		}
		if in.TrashRetentionDays != nil && (*in.TrashRetentionDays < 0 || *in.TrashRetentionDays > 3650) {
			return bad("invalid_trash", "trash_retention_days 는 0(비우지 않음) ~ 3650 일")
		}
		cfg = in
		c.st.PutConfig(cfg)
		c.cfg = cfg
		c.emit(Event{Type: "config", Data: cfg})
		return nil
	})
	return
}

func (c *Atalk) ReadMessages(r string, after, limit int) (out []Message, err error) {
	err = c.guard(func() error {
		if _, err := c.requireRoom(r, false); err != nil {
			return err
		}
		out = c.st.Messages(r, after, max(1, min(limit, 1000)))
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
	msgs := []InboxMessage{}
	for _, x := range c.st.Messages(m.Room, cursor, 0) {
		// 보통 내가 보낸 메시지는 받지 않지만, 내 사람이 나를 지정해 쓴 말(대장@나 → 나)은 받는다
		toSelf := x.From == agent && x.Human != "" && slices.Contains(x.To, agent)
		if x.Seq > cursor && (x.From != agent || toSelf) {
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
		c.st.AppendLog(r, e)
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
			out = c.st.Logs(r, agent)
			return nil
		}
		out = c.st.Logs(r, "")
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
