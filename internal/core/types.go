// Package core 는 ATalk 의 데이터 모델과 도메인 로직이다.
// 파일 포맷과 JSON 필드는 docs/DESIGN.md 와 TypeScript 구현(ts/)과 동일하다.
package core

// AgentCard 는 agents/<id>.json — 에이전트가 누구인지 (정적 정보).
type AgentCard struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	SessionID    string   `json:"session_id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Cwd          *string  `json:"cwd"`
	Pid          *int     `json:"pid"`
	Human        *string  `json:"human,omitempty"` // 이 에이전트를 쓰는 사람의 별칭 (없으면 config.json 의 user)
	RegisteredAt string   `json:"registered_at"`
	UpdatedAt    string   `json:"updated_at"`
}

const (
	StateInRoom = "in_room"
	StateAway   = "away"
	RoleOwner   = "owner"
	RoleMember  = "member"
)

// Membership 은 memberships.json 의 값 (키 = agent id). 데몬만 쓴다.
type Membership struct {
	Room       string  `json:"room"`
	Role       string  `json:"role"`
	State      string  `json:"state"`
	JoinedAt   string  `json:"joined_at"`
	LastSeen   string  `json:"last_seen"`
	AwaySince  *string `json:"away_since"`
	AwayReason *string `json:"away_reason"`
}

type MembershipEvent struct {
	TS     string `json:"ts"`
	Agent  string `json:"agent"`
	Room   string `json:"room"`
	Event  string `json:"event"`
	Detail string `json:"detail,omitempty"`
}

type Invite struct {
	Agent  string `json:"agent"`
	By     string `json:"by"`
	At     string `json:"at"`
	Status string `json:"status"` // pending | accepted | declined | cancelled
}

type JoinRequest struct {
	Agent  string `json:"agent"`
	Note   string `json:"note"`
	At     string `json:"at"`
	Status string `json:"status"` // pending | approved | rejected | cancelled
}

type Policy struct {
	MaxMessages int `json:"max_messages"`
}

// Room 은 rooms/<room>/room.json. 멤버 목록은 없다 (memberships.json 이 원본).
type Room struct {
	RoomID    string        `json:"room_id"`
	Title     string        `json:"title"`
	Owner     *string       `json:"owner"`
	CreatedAt string        `json:"created_at"`
	ClosedAt  *string       `json:"closed_at"`
	Invites   []Invite      `json:"invites"`
	Requests  []JoinRequest `json:"requests"`
	Policy    Policy        `json:"policy"`
}

var AgentMessageTypes = []string{"chat", "task", "result", "status"}

// Message 는 rooms/<room>/messages.jsonl 의 한 줄.
type Message struct {
	Seq     int      `json:"seq"`
	ID      string   `json:"id"`
	TS      string   `json:"ts"`
	Room    string   `json:"room"`
	From    string   `json:"from"`
	Type    string   `json:"type"`
	To      []string `json:"to"`
	ReplyTo *int     `json:"reply_to"`
	Body    string   `json:"body"`
	// Human 은 사람이 직접 입력한 메시지일 때 그 사람의 별칭. 표시는 "<human>@<에이전트 이름>".
	// 에이전트(모델)가 쓴 메시지는 비어 있다.
	Human string `json:"human,omitempty"`
}

// Config 는 <root>/config.json. 이 PC 의 공통 설정.
type Config struct {
	User string `json:"user"` // 사용자 별칭 기본값
}

var LogStates = []string{"received", "working", "done", "skipped", "failed"}

// LogEntry 는 rooms/<room>/logs/<agent>.jsonl 의 한 줄.
type LogEntry struct {
	TS    string `json:"ts"`
	Agent string `json:"agent"`
	Seq   int    `json:"seq"`
	State string `json:"state"`
	Note  string `json:"note"`
}

type AgentView struct {
	AgentCard
	Status string  `json:"status"` // waiting | in_room | away | offline
	Room   *string `json:"room"`
	Role   *string `json:"role"`
}

type RoomMember struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	State string `json:"state"`
	// Cursor 는 이 멤버가 마지막으로 읽은 seq (메신저 화면의 "안 읽은 사람 수" 계산용)
	Cursor int `json:"cursor"`
}

type RoomView struct {
	Room
	Members []RoomMember `json:"members"`
	LastSeq int          `json:"last_seq"`
}

type InboxMessage struct {
	Message
	ForMe bool `json:"for_me"`
}

type Unread struct {
	Room     *string        `json:"room"`
	Cursor   int            `json:"cursor"`
	Messages []InboxMessage `json:"messages"`
}

type PendingInvite struct {
	Invite
	Room  string `json:"room"`
	Title string `json:"title"`
}

type PendingRequest struct {
	JoinRequest
	Room string `json:"room"`
}

type Pending struct {
	Invites  []PendingInvite  `json:"invites"`
	Requests []PendingRequest `json:"requests"`
}

type Unfinished struct {
	Seq     int      `json:"seq"`
	State   string   `json:"state"`
	Message *Message `json:"message"`
}

type Recovery struct {
	Room       string         `json:"room"`
	Role       string         `json:"role"`
	Unread     []InboxMessage `json:"unread"`
	Unfinished []Unfinished   `json:"unfinished"`
	Recent     []Message      `json:"recent,omitempty"`
}

// Event 는 데몬이 SSE 로 내보내는 이벤트.
type Event struct {
	Type string   // message | invite | request | membership | log | agent | room
	To   []string // 받을 agent id 들 (뷰어는 전부 받음)
	Data any
}

type RegisterInput struct {
	Kind         string    `json:"kind"`
	SessionID    string    `json:"session_id,omitempty"`
	Name         *string   `json:"name,omitempty"`
	Description  *string   `json:"description,omitempty"`
	Capabilities *[]string `json:"capabilities,omitempty"`
	Cwd          *string   `json:"cwd,omitempty"`
	Pid          *int      `json:"pid,omitempty"`
}

type RegisterResult struct {
	Agent    AgentView `json:"agent"`
	Created  bool      `json:"created"`
	Recovery *Recovery `json:"recovery"`
}

type CardPatch struct {
	Name         *string   `json:"name,omitempty"`
	Description  *string   `json:"description,omitempty"`
	Capabilities *[]string `json:"capabilities,omitempty"`
	Human        *string   `json:"human,omitempty"` // 빈 문자열이면 지움 (config.json 의 user 를 따름)
}

type PostInput struct {
	Type    string   `json:"type,omitempty"`
	To      []string `json:"to,omitempty"`
	ReplyTo *int     `json:"reply_to,omitempty"`
	Body    string   `json:"body"`
	// Human 이 true 면 사람이 직접 입력한 메시지. 데몬이 별칭(카드 human → config user → "사용자")을 붙인다.
	Human bool `json:"human,omitempty"`
}
