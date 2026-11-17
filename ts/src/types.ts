// ATalk 데이터 모델. 파일에 저장되는 형태 그대로다.

/** agents/<id>.json — 에이전트가 쓰는 정적 정보 (누구인가) */
export interface AgentCard {
  id: string; // `${kind}_${session_id}`
  kind: string; // claude-code | codex | gemini-cli | opencode | custom ...
  session_id: string;
  name: string;
  description: string;
  capabilities: string[];
  cwd: string | null;
  pid: number | null; // 에이전트 프로세스 PID (비정상 종료 감지용)
  registered_at: string;
  updated_at: string;
}

export type MembershipState = "in_room" | "away";
export type Role = "owner" | "member";

/** memberships.json 의 값 — 데몬만 쓴다. 키는 agent id (세션 하나 = 방 하나) */
export interface Membership {
  room: string;
  role: Role;
  state: MembershipState;
  joined_at: string;
  last_seen: string;
  away_since: string | null;
  away_reason: string | null;
}
export type MembershipTable = Record<string, Membership>;

export interface MembershipEvent {
  ts: string;
  agent: string;
  room: string;
  event: "join" | "leave" | "move" | "away" | "back" | "expire" | "takeover" | "owner_transfer" | "close";
  detail?: string;
}

export interface Invite {
  agent: string;
  by: string;
  at: string;
  status: "pending" | "accepted" | "declined" | "cancelled";
}

export interface JoinRequest {
  agent: string;
  note: string;
  at: string;
  status: "pending" | "approved" | "rejected" | "cancelled";
}

/** rooms/<room>/room.json — 멤버 목록은 없다 (memberships.json 이 원본) */
export interface Room {
  room_id: string;
  title: string;
  owner: string | null;
  created_at: string;
  closed_at: string | null;
  invites: Invite[];
  requests: JoinRequest[];
  policy: { max_messages: number };
}

export const AGENT_MESSAGE_TYPES = ["chat", "task", "result", "status"] as const;
export type AgentMessageType = (typeof AGENT_MESSAGE_TYPES)[number];
export type MessageType = AgentMessageType | "system";

/** rooms/<room>/messages.jsonl 의 한 줄 */
export interface Message {
  seq: number;
  id: string;
  ts: string;
  room: string;
  from: string;
  type: MessageType;
  to: string[]; // 비어 있으면 방 전체
  reply_to: number | null;
  body: string;
}

export const LOG_STATES = ["received", "working", "done", "skipped", "failed"] as const;
export type LogState = (typeof LOG_STATES)[number];

/** rooms/<room>/logs/<agent>.jsonl 의 한 줄 */
export interface LogEntry {
  ts: string;
  agent: string;
  seq: number;
  state: LogState;
  note: string;
}

export type AgentStatus = "waiting" | "in_room" | "away" | "offline";

export interface AgentView extends AgentCard {
  status: AgentStatus;
  room: string | null;
  role: Role | null;
}

export interface RoomView extends Room {
  members: { agent: string; name: string; role: Role; state: MembershipState }[];
  last_seq: number;
}

export interface InboxMessage extends Message {
  for_me: boolean;
}

export interface Unread {
  room: string | null;
  cursor: number;
  messages: InboxMessage[];
}

export interface Pending {
  invites: (Invite & { room: string; title: string })[];
  requests: (JoinRequest & { room: string })[]; // 내가 방장인 방에 들어온 신청
}

export interface Recovery {
  room: string;
  role: Role;
  unread: InboxMessage[];
  unfinished: { seq: number; state: LogState; message: Message | null }[];
  recent?: Message[];
}

/** 데몬이 SSE 로 내보내는 이벤트 */
export interface AtalkEvent {
  type: "message" | "invite" | "request" | "membership" | "log" | "agent" | "room";
  to: string[]; // 받을 agent id 들 (뷰어는 전부 받음)
  data: unknown;
}
