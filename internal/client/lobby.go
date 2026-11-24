package client

// 원격 대기실 (에이전트 쪽): 참여 코드 없이 다른 PC 데몬에 접속해 네임카드만 올리고, 그 PC 에서 초대하면 자동으로 들어간다.
//
//	atalk connect https://PC-A:7421          ← --code 없이
//
// 대기 정보(주소·인증서 지문·비밀값)는 <설정 폴더>/remote/<agent>.lobby.json 에 둔다. 토큰이 없으므로 ForAgent 는
// 아직 이 PC 데몬을 쓴다. 이 PC 데몬이 3초마다 대기실을 확인하다가(WatchLobby) 초대가 오면 토큰을 받아
// <agent>.json 으로 바꾸고, 그때부터 훅·플러그인이 원격 데몬을 쓴다 (원격에서는 이미 그 방에 들어가 있음).

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"atalk/internal/core"
	"atalk/internal/paths"
)

// LobbyConn 은 대기실 정보 파일의 내용.
type LobbyConn struct {
	Agent       string             `json:"agent"`
	URL         string             `json:"url"`
	Fingerprint string             `json:"fingerprint"` // 처음 접속할 때 받은 인증서 지문 (이후 이 지문만 믿음)
	Secret      string             `json:"secret"`      // 토큰을 받을 자격 (호스트에는 해시만)
	Nonce       string             `json:"nonce"`       // 확인 숫자 계산용
	Confirm     string             `json:"confirm"`     // 확인 숫자 (호스트 화면의 숫자와 같아야 함)
	HostName    string             `json:"host_name,omitempty"`
	Card        core.RegisterInput `json:"card"` // 원격용 카드 (경로·PID 없음) — 호스트가 다시 시작하면 다시 올린다
	Host        core.Host          `json:"host"`
	Workspace   *string            `json:"workspace,omitempty"`
	WorkspaceID string             `json:"workspace_id,omitempty"`
	Since       string             `json:"since"`
}

func lobbyFile(agent string) string {
	return strings.TrimSuffix(connFile(agent), ".json") + ".lobby.json"
}

// LoadLobby 는 에이전트의 대기실 정보. 없으면 nil.
func LoadLobby(agent string) *LobbyConn {
	b, err := os.ReadFile(lobbyFile(agent))
	if err != nil {
		return nil
	}
	var lc LobbyConn
	if json.Unmarshal(b, &lc) != nil || lc.URL == "" || lc.Secret == "" {
		return nil
	}
	return &lc
}

func saveLobby(lc *LobbyConn) error {
	if err := os.MkdirAll(paths.RemoteConnDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(lc, "", "  ")
	return os.WriteFile(lobbyFile(lc.Agent), b, 0o600)
}

// RemoveLobbyFile 은 대기실 정보를 지운다.
func RemoveLobbyFile(agent string) error {
	err := os.Remove(lobbyFile(agent))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Lobbies 는 이 PC 에서 대기실에 있는 에이전트들.
func Lobbies() []*LobbyConn {
	var out []*LobbyConn
	files, _ := filepath.Glob(filepath.Join(paths.RemoteConnDir(), "*.lobby.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var lc LobbyConn
		if json.Unmarshal(b, &lc) == nil && lc.URL != "" && lc.Secret != "" {
			out = append(out, &lc)
		}
	}
	return out
}

// probeFingerprint 는 원격 데몬의 인증서 지문을 읽는다 (처음 접속: 이 지문을 기억하고 확인 숫자로 사람이 대조한다).
func probeFingerprint(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "7421")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", host, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		return "", &Error{0, "unreachable", "원격 데몬에 연결할 수 없음: " + err.Error()}
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("원격 데몬이 인증서를 보내지 않음")
	}
	sum := sha256.Sum256(certs[0].Raw)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// EnterLobby 는 참여 코드 없이 원격 데몬의 대기실에 들어간다.
func EnterLobby(p PairInput) (*LobbyConn, core.AgentView, error) {
	u, err := normURL(p.URL)
	if err != nil {
		return nil, core.AgentView{}, err
	}
	fp, err := probeFingerprint(u)
	if err != nil {
		return nil, core.AgentView{}, err
	}
	rc := &RemoteConn{Host: p.Host}
	if p.Cwd != "" {
		rc.WorkspaceID = core.WorkspaceID(p.Host.ID, p.Host.OS, p.Cwd)
	}
	rc.Workspace = p.Card.Workspace
	if rc.Workspace == nil && p.Cwd != "" {
		w := core.WorkspaceLabel(p.Cwd)
		rc.Workspace = &w
	}
	card := p.Card
	card.Cwd = &p.Cwd
	card = rc.RemoteCard(card)
	lc := &LobbyConn{URL: u, Fingerprint: fp, Secret: randomString(), Nonce: randomString(), Card: card, Host: p.Host,
		Workspace: rc.Workspace, WorkspaceID: rc.WorkspaceID, Since: time.Now().UTC().Format(time.RFC3339)}
	lc.Confirm = core.LobbyConfirm(fp, lc.Nonce)
	v, err := lc.enter()
	if err != nil {
		return nil, core.AgentView{}, err
	}
	return lc, v, nil
}

func (lc *LobbyConn) enter() (core.AgentView, error) {
	var out struct {
		Agent   core.AgentView `json:"agent"`
		Machine struct {
			Name string `json:"name"`
		} `json:"machine"`
	}
	c := newPinned(lc.URL, lc.Fingerprint)
	if err := c.call("POST", "/remote/lobby", map[string]any{"card": lc.Card, "secret": lc.Secret, "nonce": lc.Nonce}, &out, 0); err != nil {
		return core.AgentView{}, err
	}
	lc.Agent, lc.HostName = out.Agent.ID, out.Machine.Name
	return out.Agent, saveLobby(lc)
}

// Poll 은 대기실을 한 번 확인한다. 초대가 와서 토큰을 받았으면 원격 연결 파일로 바꾸고 들어간 방을 돌려준다.
// 호스트가 다시 시작해 대기실에서 빠졌으면 같은 비밀값으로 다시 들어간다. 만료되면 대기 정보를 지운다.
func (lc *LobbyConn) Poll() (room string, err error) {
	c := newPinned(lc.URL, lc.Fingerprint)
	var out struct {
		Token string `json:"token"`
		Room  string `json:"room"`
	}
	err = c.call("POST", "/remote/lobby/poll", map[string]any{"agent": lc.Agent, "secret": lc.Secret}, &out, 0)
	var ce *Error
	if errors.As(err, &ce) && ce.Code == "not_in_lobby" {
		since, _ := time.Parse(time.RFC3339, lc.Since)
		if time.Since(since) > core.LobbyTTL {
			_ = RemoveLobbyFile(lc.Agent)
			return "", err
		}
		_, err = lc.enter() // 호스트 데몬이 다시 시작됨 → 같은 비밀값으로 다시 (확인 숫자도 같음)
		return "", err
	}
	if err != nil || out.Token == "" {
		return "", err
	}
	rc := &RemoteConn{Agent: lc.Agent, URL: lc.URL, Token: out.Token, Fingerprint: lc.Fingerprint, HostName: lc.HostName, Host: lc.Host,
		Workspace: lc.Workspace, WorkspaceID: lc.WorkspaceID, ConnectedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := SaveConn(rc); err != nil {
		return "", err
	}
	_ = RemoveLobbyFile(lc.Agent)
	return out.Room, nil
}

// Leave 는 대기실에서 나온다 (원격의 네임카드도 지워짐).
func (lc *LobbyConn) Leave() error {
	err := newPinned(lc.URL, lc.Fingerprint).call("POST", "/remote/lobby/leave", map[string]any{"agent": lc.Agent, "secret": lc.Secret}, nil, 0)
	if rerr := RemoveLobbyFile(lc.Agent); rerr != nil {
		return rerr
	}
	return err
}

func normURL(raw string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.HasPrefix(u, "https://") {
		if strings.Contains(u, "://") {
			return "", &Error{0, "bad_url", "원격 주소는 https:// 여야 함"}
		}
		u = "https://" + u
	}
	return u, nil
}

// SetLobby 는 (호스트 PC) 대기실 받기를 켜거나 끈다.
func (c *Client) SetLobby(on bool) (st RemoteStatus, err error) {
	err = c.call("PUT", "/admin/remote/lobby", map[string]any{"on": on}, &st, 0)
	return
}

// KickLobby 는 (호스트 PC) 대기실 카드를 내보낸다.
func (c *Client) KickLobby(agent string) error {
	return c.call("DELETE", "/admin/remote/lobby/"+esc(agent), nil, nil, 0)
}
