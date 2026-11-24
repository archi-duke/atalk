package core

// 원격 에이전트: 다른 PC 의 에이전트가 이 데몬의 원격 포트(HTTPS)로 접속해 네임카드를 등록하고 대화방에 참여한다.
//
//   - 신원: 처음 연결(페어링)할 때 에이전트 토큰을 발급한다. DB 에는 토큰의 해시만 둔다.
//   - 생존 확인: 다른 PC 의 PID 는 확인할 수 없으므로, 원격 에이전트의 요청을 하트비트로 쓴다.
//     wait 롱폴링·SSE 처럼 열려 있는 요청이 있거나 마지막 요청이 RemoteTTL 안이면 살아 있다.
//     방에 있는데 끊기면 자리 비움(heartbeat_lost), 다시 요청이 오면 복귀한다.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"time"
)

// RemoteTTL 동안 원격 에이전트의 요청이 없으면 끊긴 것으로 본다.
// Claude 훅의 대기(wait)는 2초마다 요청하고, 일하는 중에는 도구를 쓸 때마다 훅이 요청하므로 넉넉하게 잡는다.
const RemoteTTL = 3 * time.Minute

// AwayHeartbeatLost 는 원격 에이전트의 하트비트가 끊겨 자리 비움이 된 이유. 다시 요청이 오면 자동으로 복귀한다.
const AwayHeartbeatLost = "heartbeat_lost"

// RemoteToken 은 발급한 원격 토큰 (키 = 토큰의 sha256).
type RemoteToken struct {
	Agent     string `json:"agent"`
	CreatedAt string `json:"created_at"`
}

// RemoteConn 은 원격 연결 목록에 보이는 항목.
type RemoteConn struct {
	Agent     string `json:"agent"`
	Name      string `json:"name"`
	Host      *Host  `json:"host,omitempty"`
	CreatedAt string `json:"created_at"`
	LastSeen  string `json:"last_seen,omitempty"`
	Status    string `json:"status"`
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func newSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// remoteAlive 는 원격 에이전트가 살아 있는지 (열린 요청이 있거나 RemoteTTL 안에 요청이 있었음).
// 데몬을 다시 시작한 직후에는 시작 시각부터 센다.
func (c *Atalk) remoteAlive(id string) bool {
	if c.remoteActive[id] > 0 {
		return true
	}
	last, ok := c.remoteSeen[id] // 연결을 끊으면 0 시각이 들어가 바로 끊긴 것으로 보인다
	if !ok {
		last = c.started
	}
	return c.now().Sub(last) < RemoteTTL
}

// PairRemote 는 원격 PC 의 에이전트를 등록하고 토큰을 발급한다. 등록에는 다른 PC 의 host 가 있어야 한다.
// 같은 에이전트의 예전 토큰은 무효가 된다 (다시 연결).
func (c *Atalk) PairRemote(in RegisterInput) (res RegisterResult, token string, err error) {
	if in.Host == nil || in.Host.ID == "" || in.Host.ID == c.machineID {
		return res, "", bad("invalid_host", "원격 등록에는 다른 PC 의 host 가 필요함")
	}
	if in.SessionID == "" {
		return res, "", bad("invalid_session", "원격 등록에는 session_id 가 필요함")
	}
	in.Provisional = false
	id := MakeAgentID(in.Kind, in.SessionID)
	_ = c.guard(func() error { c.remoteSeen[id] = c.now(); return nil }) // 등록 응답에 바로 살아 있는 상태로 보이게
	if res, err = c.Register(in); err != nil {
		return res, "", err
	}
	err = c.guard(func() error {
		c.revokeRemote(res.Agent.ID)
		token = newSecret()
		c.remoteTokens[hashToken(token)] = RemoteToken{Agent: res.Agent.ID, CreatedAt: c.ts()}
		c.st.PutRemoteTokens(c.remoteTokens)
		return nil
	})
	return res, token, err
}

// RemoteAgent 는 토큰의 에이전트 id. 없거나 카드가 지워졌으면 "".
func (c *Atalk) RemoteAgent(token string) (agent string) {
	if token == "" {
		return ""
	}
	_ = c.guard(func() error {
		if t, ok := c.remoteTokens[hashToken(token)]; ok {
			if _, has := c.agents[t.Agent]; has {
				agent = t.Agent
			}
		}
		return nil
	})
	return
}

// TouchRemote 는 원격 에이전트의 요청을 기록한다 (하트비트). open 이 +1 이면 열린 요청 시작, -1 이면 끝.
// 하트비트가 끊겨 자리 비움이었으면 복귀시킨다.
func (c *Atalk) TouchRemote(agent string, open int) {
	_ = c.guard(func() error {
		card, ok := c.agents[agent]
		if !ok || c.isLocal(card) {
			return nil
		}
		wasAlive := c.remoteAlive(agent)
		c.remoteSeen[agent] = c.now()
		if n := c.remoteActive[agent] + open; n > 0 {
			c.remoteActive[agent] = n
		} else {
			delete(c.remoteActive, agent)
		}
		if m, ok := c.memberships[agent]; ok && m.State == StateAway && m.AwayReason != nil && *m.AwayReason == AwayHeartbeatLost {
			c.markBack(agent)
		}
		if !wasAlive {
			c.emit(Event{Type: "agent", Data: map[string]any{"agent": agent, "action": "update"}})
		}
		return nil
	})
}

// RevokeRemote 는 에이전트의 원격 토큰을 지운다 (연결 끊기). 방에 있으면 자리 비움으로 둔다.
func (c *Atalk) RevokeRemote(agent string) error {
	return c.guard(func() error {
		if _, err := c.requireAgent(agent); err != nil {
			return err
		}
		if !c.revokeRemote(agent) {
			return &Error{Status: 404, Code: "not_connected", Message: "원격 연결이 없음: " + agent}
		}
		c.markAway(agent, "remote_revoked")
		delete(c.remoteActive, agent)
		c.remoteSeen[agent] = time.Time{}
		c.emit(Event{Type: "agent", Data: map[string]any{"agent": agent, "action": "update"}})
		return nil
	})
}

func (c *Atalk) revokeRemote(agent string) bool {
	found := false
	for h, t := range c.remoteTokens {
		if t.Agent == agent {
			delete(c.remoteTokens, h)
			found = true
		}
	}
	if found {
		c.st.PutRemoteTokens(c.remoteTokens)
	}
	return found
}

// RemoteConns 는 토큰을 가진 원격 에이전트 목록.
func (c *Atalk) RemoteConns() (out []RemoteConn, err error) {
	err = c.guard(func() error {
		out = []RemoteConn{}
		for _, t := range c.remoteTokens {
			card, ok := c.agents[t.Agent]
			if !ok {
				continue
			}
			rc := RemoteConn{Agent: t.Agent, Name: card.Name, Host: card.Host, CreatedAt: t.CreatedAt, Status: c.statusOf(t.Agent)}
			if s, ok := c.remoteSeen[t.Agent]; ok && !s.IsZero() {
				rc.LastSeen = s.UTC().Format(time.RFC3339)
			}
			out = append(out, rc)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
		return nil
	})
	return
}
