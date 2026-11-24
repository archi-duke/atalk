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
	Human        *string  `json:"human,omitempty"`         // 이 에이전트를 쓰는 사람의 별칭 (없으면 config.json 의 user)
	RetiredBy    *string  `json:"retired_by,omitempty"`    // 다른 세션이 이 세션의 자리를 이어받았으면 그 세션 id (상태는 offline)
	OfflineSince *string  `json:"offline_since,omitempty"` // 데몬이 처음 offline 으로 본 시각. 오래되면 카드 자동 삭제
	// Host 는 에이전트가 실행 중인 PC. 없으면(예전 카드) 이 PC.
	Host *Host `json:"host,omitempty"`
	// Workspace 는 작업 이름 (기본: 작업 폴더 또는 git 저장소 폴더 이름). "" 이면 숨김. 전체 경로 대신 화면에 보인다.
	Workspace *string `json:"workspace,omitempty"`
	// Provisional 은 아직 사람이 쓰지 않은 새 세션 (훅·플러그인이 시작할 때 등록). 이전 세션의 이름표를 미리 보여 주기만 하고
	// 이전 카드를 넘겨받지 않는다. 사람이 처음 입력하거나 방에 들어가면 확정(activate)되어 그때 넘겨받는다.
	// 이전 세션이 다시 돌아오면 확정되지 않은 임시 카드는 지워진다.
	Provisional bool `json:"provisional,omitempty"`
	// InheritFrom 은 이름표를 이어받은(받을) 이전 세션 id.
	InheritFrom string `json:"inherit_from,omitempty"`
	// LastRoom 은 자리 비움 유예가 지나 자동 퇴장된 방. 이 세션이 다시 돌아오면 그 방으로 들어간다 (직접 나간 방은 아님).
	LastRoom string `json:"last_room,omitempty"`
	// WorkspaceID 는 해시(PC ID + 정규화한 작업 폴더 경로). 경로를 드러내지 않고 "같은 PC 의 같은 폴더" 인지 비교하는 데 쓴다.
	WorkspaceID string `json:"workspace_id,omitempty"`
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
	// EchoQueue 는 응답을 기록 중인 CLI 입력 기록의 seq 들 (오래된 것부터). 그 턴이 끝나면 빠진다.
	// 앞선 턴이 진행 중일 때 다음 입력이 대기열에 들어와도 앞선 입력의 응답이 계속 기록되도록 큐로 둔다.
	// 멤버십과 함께 저장되어 데몬을 다시 시작해도 유지된다.
	EchoQueue []int `json:"echo_queue,omitempty"`
	// EchoOpen 은 예전 형식(한 칸). 읽을 때 EchoQueue 로 옮긴다.
	EchoOpen int `json:"echo_open,omitempty"`
	// Intro 는 초대 수락·신청 승인으로 새로 들어와 아직 방에 인사·자기소개를 하지 않았음.
	// 이 에이전트가 방에 처음 글을 쓰면 꺼진다. 훅·플러그인이 보고 "먼저 인사하고 자기소개" 를 지시한다.
	Intro bool `json:"intro,omitempty"`
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
	RoomID string `json:"room_id"`
	Title  string `json:"title"`
	// Description 은 방의 목적·설명. 에이전트에게 초대·메시지와 함께 전해진다.
	Description string        `json:"description,omitempty"`
	Owner       *string       `json:"owner"`
	CreatedAt   string        `json:"created_at"`
	ClosedAt    *string       `json:"closed_at"`
	Invites     []Invite      `json:"invites"`
	Requests    []JoinRequest `json:"requests"`
	Policy      Policy        `json:"policy"`
	// Persistent 인 방(사람이 화면에서 방장 없이 만든 방)은 멤버가 모두 나가도 닫히지 않고 방장 없이 남는다.
	Persistent bool `json:"persistent,omitempty"`
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
	// Echo 는 사람이 에이전트 창(CLI)에 직접 입력한 말을 대화 기록으로만 남긴 것. 어떤 에이전트에게도 전달·알림하지 않는다.
	Echo bool `json:"echo,omitempty"`
}

// Config 는 <root>/config.json. 이 PC 의 공통 설정.
type Config struct {
	User string `json:"user"` // 사용자 별칭 기본값
	// AwayGraceMin 은 자리 비움(away) 후 자동 퇴장까지의 분. 0 이면 자동 퇴장하지 않음. 없으면 데몬 옵션(기본 30분).
	AwayGraceMin *int `json:"away_grace_min,omitempty"`
	// OfflineTTLHours 는 offline 카드를 자동 삭제할 때까지의 시간. 0 이면 지우지 않음. 없으면 24.
	OfflineTTLHours *int `json:"offline_ttl_hours,omitempty"`
	// DefaultMaxMessages 는 새 방의 메시지 한도. 없으면 1000.
	DefaultMaxMessages *int `json:"default_max_messages,omitempty"`
	// TrashRetentionDays 는 삭제한 방(trash/)을 보관하는 날 수. 0 이면 자동으로 비우지 않음. 없으면 30.
	TrashRetentionDays *int `json:"trash_retention_days,omitempty"`
	// AutoResume 이 false 면 새 세션이 이전 세션 자리를 자동으로 이어받지 않음. 없으면 켜짐.
	AutoResume *bool `json:"auto_resume,omitempty"`
	// AutoAcceptInvites 가 false 면 opencode 플러그인이 초대를 자동 수락하지 않음. 없으면 켜짐.
	AutoAcceptInvites *bool `json:"auto_accept_invites,omitempty"`
	// EchoPrompts 가 false 면 에이전트 창(CLI)에 입력한 말을 대화방에 기록하지 않음. 없으면 켜짐.
	EchoPrompts *bool `json:"echo_prompts,omitempty"`
	// EchoFinalOnly 가 true 면 CLI 응답 중 작업 중간에 쓴 문장은 빼고 마지막 답만 기록한다. 없으면 꺼짐 (문장마다 실시간 기록).
	EchoFinalOnly bool `json:"echo_final_only,omitempty"`
	// MachineName 은 이 PC 의 이름 (에이전트 목록에 보임). 없으면 "PC-" + PC ID 앞 4자리. 호스트 이름은 쓰지 않는다.
	MachineName string `json:"machine_name,omitempty"`
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

// Host 는 PC 정보. ID 는 PC 마다 처음 실행할 때 만든 무작위 값(하드웨어 정보나 호스트 이름이 아님),
// Name 은 사람이 붙인 별칭(기본 "PC-XXXX"), OS 는 windows | mac | linux.
type Host struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	OS   string `json:"os,omitempty"`
}

// AgentView 는 API 로 내보내는 카드. 다른 PC 의 카드는 cwd·pid 를 빼고 보낸다 (그 PC 밖에서는 의미도 없고 노출만 된다).
type AgentView struct {
	AgentCard
	Busy   bool   `json:"busy,omitempty"` // 지금 일하는 중 (CLI 에서 받은 일 포함, 훅·플러그인이 알림)
	Local  bool   `json:"local"`  // 이 데몬과 같은 PC 에서 실행 중
	Status string  `json:"status"` // waiting | in_room | away | offline
	Room   *string `json:"room"`
	Role   *string `json:"role"`
	Intro  bool    `json:"intro,omitempty"` // 새로 들어와 아직 인사·자기소개 전 (Membership.Intro)
	// Lobby 는 원격 대기실에 있으면 확인 숫자·주소 (이 PC 화면용, 원격으로는 보내지 않음). lobby.go
	Lobby *LobbyInfo `json:"lobby,omitempty"`
}

type RoomMember struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	State string `json:"state"`
	// Cursor 는 이 멤버가 마지막으로 읽은 seq (메신저 화면의 "안 읽은 사람 수" 계산용)
	Cursor int `json:"cursor"`
	Busy   bool `json:"busy,omitempty"` // 지금 일하는 중 (CLI 에서 받은 일 포함)
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
	// Host 가 없으면 이 데몬의 PC 로 본다 (로컬 등록). 다른 PC 에서 등록할 때만 보낸다.
	Host *Host `json:"host,omitempty"`
	// Workspace 는 작업 이름. 없으면 데몬이 작업 폴더 이름으로 정한다.
	Workspace *string `json:"workspace,omitempty"`
	// WorkspaceID 는 다른 PC 에서 등록할 때 클라이언트가 계산해 보내는 작업 위치 ID (경로는 보내지 않음).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// Provisional 이면 새로 만든 카드를 임시로 둔다 (훅·플러그인이 세션 시작 때). 이미 있는 카드에는 영향 없음.
	Provisional bool `json:"provisional,omitempty"`
}

type RegisterResult struct {
	Agent    AgentView `json:"agent"`
	Created  bool      `json:"created"`
	Recovery *Recovery `json:"recovery"` // 같은 세션이 다시 등록했고 방에 있으면 (예: 대화 압축)
	// 새 세션이 같은 폴더의 이전 세션 이름표(이름·설명·능력)를 이어받았으면 그 세션 id.
	// 방 자리는 이어받지 않는다: PreviousRoom 에 있던 방은 입장 명령(request)을 받으면 승인 없이 원래 자리로 들어간다.
	InheritedFrom string `json:"inherited_from,omitempty"`
	PreviousRoom  string `json:"previous_room,omitempty"`
}

type CardPatch struct {
	Name         *string   `json:"name,omitempty"`
	Description  *string   `json:"description,omitempty"`
	Capabilities *[]string `json:"capabilities,omitempty"`
	Human        *string   `json:"human,omitempty"` // 빈 문자열이면 지움 (config.json 의 user 를 따름)
	Workspace    *string   `json:"workspace,omitempty"` // 작업 이름. 빈 문자열이면 숨김
}

type PostInput struct {
	Type    string   `json:"type,omitempty"`
	To      []string `json:"to,omitempty"`
	ReplyTo *int     `json:"reply_to,omitempty"`
	Body    string   `json:"body"`
	// Human 이 true 면 사람이 직접 입력한 메시지. 데몬이 별칭(카드 human → config user → "사용자")을 붙인다.
	Human bool `json:"human,omitempty"`
}
