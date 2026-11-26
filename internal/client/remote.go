package client

// 원격 연결: 이 PC 의 에이전트가 다른 PC 의 ATalk 데몬(원격 포트, HTTPS)에 참여한다.
//
//	atalk connect https://PC-A:7421 --code <비밀값>.<인증서 지문>
//
// 연결 정보(주소·토큰·인증서 지문)는 에이전트마다 <설정 폴더>/remote/<agent>.json 에 둔다.
// ForAgent 는 그 파일이 있으면 원격 클라이언트를, 없으면 로컬 데몬 클라이언트를 돌려주므로
// 훅·CLI 는 에이전트 id 만 알면 같은 코드로 원격 데몬과 통신한다.

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"atalk/internal/core"
	"atalk/internal/paths"
)

// RemoteConn 은 원격 연결 파일의 내용.
type RemoteConn struct {
	Agent       string    `json:"agent"`
	URL         string    `json:"url"`
	Token       string    `json:"token"`
	Fingerprint string    `json:"fingerprint"`            // 원격 데몬 인증서의 sha256 (base64url)
	HostName    string    `json:"host_name,omitempty"`    // 원격 PC 이름 (안내용)
	Host        core.Host `json:"host"`                   // 이 PC 정보 (원격 등록에 보냄)
	Workspace   *string   `json:"workspace,omitempty"`    // 이 PC 의 작업 이름
	WorkspaceID string    `json:"workspace_id,omitempty"` // 이 PC 의 작업 위치 ID
	ConnectedAt string    `json:"connected_at"`
}

func connFile(agent string) string {
	return filepath.Join(paths.RemoteConnDir(), strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(agent)+".json")
}

// LoadConn 은 에이전트의 원격 연결 정보. 없으면 nil.
func LoadConn(agent string) *RemoteConn {
	if agent == "" {
		return nil
	}
	b, err := os.ReadFile(connFile(agent))
	if err != nil {
		return nil
	}
	var rc RemoteConn
	if json.Unmarshal(b, &rc) != nil || rc.URL == "" || rc.Token == "" || rc.Fingerprint == "" {
		return nil
	}
	return &rc
}

// SaveConn 은 연결 정보를 저장한다 (토큰이 들어 있으므로 본인만 읽게).
func SaveConn(rc *RemoteConn) error {
	if err := os.MkdirAll(paths.RemoteConnDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(rc, "", "  ")
	return os.WriteFile(connFile(rc.Agent), b, 0o600)
}

// Conns 는 이 PC 에서 다른 PC 데몬에 연결된 에이전트들.
func Conns() []*RemoteConn {
	var out []*RemoteConn
	files, _ := filepath.Glob(filepath.Join(paths.RemoteConnDir(), "*.json"))
	for _, f := range files {
		if strings.HasSuffix(f, ".lobby.json") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rc RemoteConn
		if json.Unmarshal(b, &rc) == nil && rc.URL != "" && rc.Token != "" && rc.Fingerprint != "" {
			out = append(out, &rc)
		}
	}
	return out
}

// RemoveConn 은 연결 정보를 지운다.
func RemoveConn(agent string) error {
	err := os.Remove(connFile(agent))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ForAgent 는 에이전트가 원격 데몬에 연결돼 있으면 그 클라이언트, 아니면 로컬 데몬 클라이언트.
func ForAgent(agent string) *Client {
	if rc := LoadConn(agent); rc != nil {
		return NewRemote(rc)
	}
	return New("")
}

// NewRemote 는 원격 데몬 클라이언트. 인증서는 공인 기관 대신 연결할 때 받은 지문으로만 확인한다.
func NewRemote(rc *RemoteConn) *Client {
	c := newPinned(rc.URL, rc.Fingerprint)
	c.Token, c.Conn = rc.Token, rc
	return c
}

func newPinned(base, fingerprint string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // 자체 서명 인증서: 아래 VerifyConnection 에서 지문으로 확인한다
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("원격 데몬이 인증서를 보내지 않음")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			got := base64.RawURLEncoding.EncodeToString(sum[:])
			if subtle.ConstantTimeCompare([]byte(got), []byte(fingerprint)) != 1 {
				return fmt.Errorf("원격 데몬의 인증서 지문이 다름 (기대 %s, 받음 %s) — 다른 서버이거나 인증서가 바뀜. 참여 코드를 새로 받아 다시 연결하세요", short(fingerprint), short(got))
			}
			return nil
		},
	}
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Transport: tr}}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// RemoteCard 는 등록 정보를 원격용으로 바꾼다: 경로·PID 는 보내지 않고 이 PC 정보와 작업 위치 ID 를 보낸다.
func (rc *RemoteConn) RemoteCard(in core.RegisterInput) core.RegisterInput {
	h := rc.Host
	in.Host = &h
	if in.Cwd != nil && *in.Cwd != "" {
		in.WorkspaceID = core.WorkspaceID(h.ID, h.OS, *in.Cwd)
		if in.Workspace == nil && rc.Workspace == nil {
			w := core.WorkspaceLabel(*in.Cwd)
			in.Workspace = &w
		}
	} else if in.WorkspaceID == "" {
		in.WorkspaceID = rc.WorkspaceID
	}
	if in.Workspace == nil {
		in.Workspace = rc.Workspace
	}
	in.Cwd, in.Pid, in.Provisional = nil, nil, false
	return in
}

// PairInput 은 연결할 때 보내는 정보.
type PairInput struct {
	URL  string
	Code string // <비밀값>.<인증서 지문>
	Card core.RegisterInput
	Host core.Host // 이 PC
	Cwd  string    // 이 에이전트의 작업 폴더 (보내지 않고 작업 위치 ID 만 계산)
}

// Pair 는 원격 데몬에 참여 코드로 연결해 네임카드를 등록하고 토큰을 받아 저장한다.
func Pair(p PairInput) (*RemoteConn, core.AgentView, error) {
	_, fp, ok := strings.Cut(strings.TrimSpace(p.Code), ".")
	if !ok || fp == "" {
		return nil, core.AgentView{}, &Error{0, "bad_code", "참여 코드 형식이 아님 (<코드>.<지문>, 호스트 PC 에서 atalk remote code 로 받음)"}
	}
	u := strings.TrimRight(strings.TrimSpace(p.URL), "/")
	if !strings.HasPrefix(u, "https://") {
		if strings.Contains(u, "://") {
			return nil, core.AgentView{}, &Error{0, "bad_url", "원격 주소는 https:// 여야 함"}
		}
		u = "https://" + u
	}
	rc := &RemoteConn{URL: u, Fingerprint: fp, Host: p.Host, ConnectedAt: time.Now().UTC().Format(time.RFC3339)}
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
	c := newPinned(u, fp)
	var out struct {
		Agent   core.AgentView `json:"agent"`
		Token   string         `json:"token"`
		Machine struct {
			Name string `json:"name"`
		} `json:"machine"`
	}
	if err := c.call("POST", "/remote/pair", map[string]any{"code": p.Code, "card": card}, &out, 0); err != nil {
		return nil, core.AgentView{}, err
	}
	rc.Agent, rc.Token, rc.HostName = out.Agent.ID, out.Token, out.Machine.Name
	if err := SaveConn(rc); err != nil {
		return nil, core.AgentView{}, err
	}
	return rc, out.Agent, nil
}

// ── 호스트 PC 의 원격 포트 관리 (로컬 데몬 관리 API) ──

type RemoteStatus struct {
	Enabled     bool              `json:"enabled"`
	Port        int               `json:"port"`
	SavedPort   int               `json:"saved_port"`
	Fingerprint string            `json:"fingerprint"`
	Addresses   []string          `json:"addresses"`
	Connections []core.RemoteConn `json:"connections"`
	LobbyOn     bool              `json:"lobby_enabled"`
	Lobby       []core.AgentView  `json:"lobby"`
}

func (c *Client) RemoteStatus() (st RemoteStatus, err error) {
	err = c.call("GET", "/admin/remote", nil, &st, 0)
	return
}

func (c *Client) SetRemote(on bool, port int) (st RemoteStatus, err error) {
	err = c.call("PUT", "/admin/remote", map[string]any{"on": on, "port": port}, &st, 0)
	return
}

type PairCode struct {
	Code      string   `json:"code"`
	ExpiresAt string   `json:"expires_at"`
	URLs      []string `json:"urls"`
}

func (c *Client) NewPairCode() (pc PairCode, err error) {
	err = c.call("POST", "/admin/remote/code", map[string]any{}, &pc, 0)
	return
}

func (c *Client) RevokeRemote(agent string) error {
	return c.call("DELETE", "/admin/remote/agents/"+esc(agent), nil, nil, 0)
}

// DisconnectRemote 는 원격 데몬에 내 토큰을 철회하게 한다 (연결 끊기).
func (c *Client) DisconnectRemote() error {
	return c.call("POST", "/remote/disconnect", map[string]any{}, nil, 0)
}

// SpawnRemote 는 원격에 연결된 에이전트(이 클라이언트의 토큰)가 이 PC 에서 띄운 세션(card)을 원격 데몬에 등록하고
// 그 세션의 연결 정보를 저장한다. 원격 데몬은 그 세션을 띄운 에이전트의 방에 바로 넣는다.
func (c *Client) SpawnRemote(card core.RegisterInput, cwd string) (*RemoteConn, core.AgentView, error) {
	if c.Conn == nil {
		return nil, core.AgentView{}, &Error{0, "not_remote", "원격 연결이 아님"}
	}
	rc := &RemoteConn{URL: c.Conn.URL, Fingerprint: c.Conn.Fingerprint, HostName: c.Conn.HostName, Host: c.Conn.Host, ConnectedAt: time.Now().UTC().Format(time.RFC3339)}
	if cwd != "" {
		rc.WorkspaceID = core.WorkspaceID(rc.Host.ID, rc.Host.OS, cwd)
		w := core.WorkspaceLabel(cwd)
		rc.Workspace = &w
	}
	if card.Workspace != nil {
		rc.Workspace = card.Workspace
	}
	card.Cwd = &cwd
	card = rc.RemoteCard(card)
	var out struct {
		Agent core.AgentView `json:"agent"`
		Token string         `json:"token"`
	}
	if err := c.call("POST", "/remote/spawn", map[string]any{"card": card}, &out, 0); err != nil {
		return nil, core.AgentView{}, err
	}
	rc.Agent, rc.Token = out.Agent.ID, out.Token
	if err := SaveConn(rc); err != nil {
		return nil, core.AgentView{}, err
	}
	return rc, out.Agent, nil
}
