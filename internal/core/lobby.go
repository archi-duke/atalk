package core

// 원격 대기실: 다른 PC 의 에이전트가 참여 코드 없이 접속해 네임카드만 올려 두고, 방장이 초대하면 그때 토큰을 받아 들어온다.
//
//   - 대기실에서는 아무 방도 보거나 쓸 수 없다 (토큰이 없으므로 원격 포트의 에이전트 API 를 쓸 수 없음).
//   - 사칭·중간자 방지: 에이전트 쪽 화면과 이 PC 의 웹(대기실 카드)에 같은 확인 숫자를 보여 준다.
//     숫자 = sha256(이 데몬 인증서 지문 | 에이전트가 만든 nonce) 에서 6자리. 사람이 맞춰 본 뒤 초대한다.
//   - 토큰은 대기실에 들어올 때 에이전트가 보낸 비밀값(의 해시)을 가진 요청에만 준다.
//   - 범람 방지: 대기실 최대 LobbyMax 명, IP 당 LobbyPerIP 명. 초대 없이 LobbyTTL 이 지나거나 확인이 끊기면 지운다.
//   - 대기실 정보는 메모리에만 둔다. 데몬이 다시 시작하면 에이전트 쪽이 같은 비밀값으로 다시 들어온다.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"time"
)

const (
	LobbyMax   = 20
	LobbyPerIP = 3
	LobbyTTL   = time.Hour
)

// LobbyEntry 는 대기실에 있는 원격 에이전트.
type LobbyEntry struct {
	SecretHash string
	Confirm    string // 확인 숫자 6자리
	IP         string
	Since      time.Time
}

// LobbyInfo 는 이 PC 의 화면(웹·CLI)에 보여 주는 대기실 정보. 원격으로는 보내지 않는다.
type LobbyInfo struct {
	Confirm string `json:"confirm"`
	IP      string `json:"ip,omitempty"`
	Since   string `json:"since"`
}

// LobbyConfirm 은 확인 숫자. 호스트는 자기 인증서 지문으로, 에이전트는 처음 접속할 때 받은 지문으로 계산한다.
// 중간에 다른 서버가 끼면 지문이 달라 숫자가 맞지 않는다.
func LobbyConfirm(fingerprint, nonce string) string {
	sum := sha256.Sum256([]byte(fingerprint + "|" + nonce))
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(sum[:8])%1_000_000)
}

// EnterLobby 는 원격 에이전트를 대기실에 넣는다 (네임카드 등록, 토큰 없음).
// 같은 비밀값으로 다시 들어오면(데몬 재시작·재시도) 그대로 이어진다.
func (c *Atalk) EnterLobby(in RegisterInput, secret, confirm, ip string) (v AgentView, err error) {
	if in.Host == nil || in.Host.ID == "" || in.Host.ID == c.machineID {
		return v, bad("invalid_host", "대기실에는 다른 PC 의 에이전트만 들어올 수 있음")
	}
	if in.SessionID == "" || len(secret) < 16 || confirm == "" {
		return v, bad("invalid_lobby", "대기실 등록에는 session_id·비밀값·확인 숫자가 필요함")
	}
	id := MakeAgentID(in.Kind, in.SessionID)
	err = c.guard(func() error {
		c.sweepLobbyLocked()
		if card, ok := c.agents[id]; ok {
			if c.isLocal(card) {
				return &Error{409, "agent_exists", "이 PC 의 에이전트와 id 가 같음"}
			}
			if c.hasRemoteToken(id) {
				return &Error{409, "already_connected", "이미 연결된 에이전트 — 같은 에이전트는 atalk disconnect 뒤에 다시 들어오세요"}
			}
		}
		if e, ok := c.lobby[id]; ok {
			if subtle.ConstantTimeCompare([]byte(e.SecretHash), []byte(hashToken(secret))) != 1 {
				return &Error{409, "lobby_taken", "같은 에이전트가 이미 대기실에 있음"}
			}
		} else {
			if len(c.lobby) >= LobbyMax {
				return &Error{429, "lobby_full", "대기실이 가득 참. 잠시 뒤 다시 시도하세요"}
			}
			n := 0
			for _, e := range c.lobby {
				if e.IP == ip {
					n++
				}
			}
			if n >= LobbyPerIP {
				return &Error{429, "lobby_full", "이 주소에서 대기실에 들어온 에이전트가 너무 많음"}
			}
			c.lobby[id] = &LobbyEntry{SecretHash: hashToken(secret), Confirm: confirm, IP: ip, Since: c.now()}
		}
		c.remoteSeen[id] = c.now()
		return nil
	})
	if err != nil {
		return v, err
	}
	in.Provisional = false
	in.Cwd, in.Pid = nil, nil
	res, err := c.Register(in)
	if err != nil {
		_ = c.guard(func() error { delete(c.lobby, id); return nil })
		return v, err
	}
	return res.Agent, nil
}

// LobbyPoll 은 대기실의 에이전트가 초대를 기다리며 부른다 (하트비트 겸용).
// 초대가 와 있으면 토큰을 발급하고 그 방에 들어가게 한 뒤 토큰과 방을 돌려준다. 아직이면 token "".
func (c *Atalk) LobbyPoll(agent, secret string) (token, room string, err error) {
	err = c.guard(func() error {
		c.sweepLobbyLocked()
		e, ok := c.lobby[agent]
		if !ok || subtle.ConstantTimeCompare([]byte(e.SecretHash), []byte(hashToken(secret))) != 1 {
			return notFound("not_in_lobby", "대기실에 없음 (만료됐거나 호스트 데몬이 다시 시작됨)")
		}
		if _, ok := c.agents[agent]; !ok {
			delete(c.lobby, agent)
			return notFound("not_in_lobby", "대기실 네임카드가 지워짐")
		}
		c.remoteSeen[agent] = c.now()
		inv := c.pendingLocked(agent).Invites
		if len(inv) == 0 {
			return nil
		}
		last := inv[len(inv)-1] // 가장 최근 초대
		r := c.rooms[last.Room]
		idx := -1
		for i, x := range r.Invites {
			if x.Agent == agent && x.Status == "pending" {
				idx = i
			}
		}
		if idx < 0 {
			return nil
		}
		r.Invites[idx].Status = "accepted"
		c.saveRoom(r)
		delete(c.lobby, agent)
		c.revokeRemote(agent)
		token = newSecret()
		c.remoteTokens[hashToken(token)] = RemoteToken{Agent: agent, CreatedAt: c.ts()}
		c.st.PutRemoteTokens(c.remoteTokens)
		c.joinInternal(agent, last.Room, RoleMember, true)
		room = last.Room
		c.emit(Event{Type: "agent", Data: map[string]any{"agent": agent, "action": "update"}})
		return nil
	})
	return
}

// LeaveLobby 는 대기실에서 나간다 (에이전트가 그만둠). 네임카드도 지운다.
func (c *Atalk) LeaveLobby(agent, secret string) error {
	return c.guard(func() error {
		e, ok := c.lobby[agent]
		if !ok || subtle.ConstantTimeCompare([]byte(e.SecretHash), []byte(hashToken(secret))) != 1 {
			return notFound("not_in_lobby", "대기실에 없음")
		}
		c.dropLobbyLocked(agent, "lobby_leave")
		return nil
	})
}

// RemoveLobby 는 이 PC 의 사람이 대기실 카드를 내보낸다.
func (c *Atalk) RemoveLobby(agent string) error {
	return c.guard(func() error {
		if _, ok := c.lobby[agent]; !ok {
			return notFound("not_in_lobby", "대기실에 없음: %s", agent)
		}
		c.dropLobbyLocked(agent, "lobby_removed")
		return nil
	})
}

func (c *Atalk) dropLobbyLocked(agent, reason string) {
	delete(c.lobby, agent)
	if _, in := c.memberships[agent]; !in && !c.hasRemoteToken(agent) {
		c.removeCard(agent, reason)
	}
}

// sweepLobbyLocked 는 초대 없이 LobbyTTL 이 지났거나 확인(poll)이 RemoteTTL 동안 끊긴 대기실 에이전트를 지운다.
func (c *Atalk) sweepLobbyLocked() {
	now := c.now()
	for id, e := range c.lobby {
		if now.Sub(e.Since) > LobbyTTL || !c.remoteAlive(id) {
			c.dropLobbyLocked(id, "lobby_expired")
		}
	}
}

func (c *Atalk) hasRemoteToken(agent string) bool {
	for _, t := range c.remoteTokens {
		if t.Agent == agent {
			return true
		}
	}
	return false
}

// lobbyInfo 는 대기실에 있으면 화면용 정보.
func (c *Atalk) lobbyInfo(id string) *LobbyInfo {
	e, ok := c.lobby[id]
	if !ok {
		return nil
	}
	return &LobbyInfo{Confirm: e.Confirm, IP: e.IP, Since: e.Since.UTC().Format(time.RFC3339)}
}
