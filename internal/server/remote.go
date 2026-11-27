package server

// 원격 포트: 다른 PC 의 에이전트가 이 데몬의 대화방에 참여하는 통로.
//
//   - 내부 포트(127.0.0.1)와 같은 핸들러를 쓰되, 원격 포트에는 에이전트용 API 만 올린다.
//     관리·설정·뷰어·전체 이벤트(/events)·내보내기·창 앞으로 같은 경로는 원격 포트에 없다.
//   - HTTPS(자체 서명 인증서). 클라이언트는 참여 코드에 들어 있는 인증서 지문으로 이 데몬만 믿는다.
//   - 처음 연결할 때 참여 코드(일회용, 10분)를 에이전트 토큰으로 바꾸고, 이후 요청은 모두 Bearer 토큰.
//     요청의 행위자(본문 agent, 경로 {id})는 토큰의 에이전트로 고정한다.
//   - 원격 에이전트의 요청은 하트비트로 기록한다 (core.TouchRemote).
//
//	로컬 관리 API (localOnly):
//	GET    /admin/remote                 상태 (켜짐, 포트, 인증서 지문, 이 PC 주소, 연결 목록)
//	PUT    /admin/remote {on, port}      원격 포트 켜기/끄기
//	POST   /admin/remote/code            참여 코드 발급 → {code, expires_at, urls}
//	DELETE /admin/remote/agents/{id}     원격 연결 끊기 (토큰 철회)

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"atalk/internal/core"
)

// DefaultRemotePort 는 원격 포트 기본값.
const DefaultRemotePort = 7421

// remoteListenHost 는 원격 포트가 듣는 주소 ("" = 모든 네트워크). 테스트는 127.0.0.1 로 바꿔
// Windows 방화벽 허용 창이 테스트할 때마다(테스트 실행 파일 경로가 매번 달라짐) 뜨지 않게 한다 (main_test.go).
var remoteListenHost = ""

const (
	pairCodeTTL  = 10 * time.Minute
	pairMaxFails = 10 // 10분 안에 이만큼 틀리면 잠시 페어링을 막는다
)

type remoteState struct {
	mu    sync.Mutex
	http  *http.Server
	port  int
	codes map[string]time.Time // 참여 코드 비밀값 → 만료 시각
	fails []time.Time
	// 웹 화면 로그인 (webremote.go)
	webCodes map[string]time.Time // 웹 로그인 코드 → 만료 시각
	web      map[string]time.Time // 로그인 쿠키의 sha256 → 만료 시각 (web_sessions.json)
}

type remoteSettings struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port,omitempty"`
	// NoLobby 이면 대기실(코드 없이 접속)을 막는다. 기본은 허용 (core/lobby.go)
	NoLobby bool `json:"no_lobby,omitempty"`
}

func (s *Server) remoteSettingsFile() string { return filepath.Join(s.remoteDir, "settings.json") }

func (s *Server) readRemoteSettings() remoteSettings {
	var st remoteSettings
	if b, err := os.ReadFile(s.remoteSettingsFile()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Port <= 0 || st.Port > 65535 {
		st.Port = DefaultRemotePort
	}
	return st
}

func (s *Server) writeRemoteSettings(st remoteSettings) error {
	if err := os.MkdirAll(s.remoteDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(s.remoteSettingsFile(), b, 0o600)
}

// remoteCert 는 원격 포트의 인증서. 없으면 만든다 (ECDSA P-256 자체 서명). 지문 = base64url(sha256(DER)).
func (s *Server) remoteCert() (tls.Certificate, string, error) {
	certFile, keyFile := filepath.Join(s.remoteDir, "cert.pem"), filepath.Join(s.remoteDir, "key.pem")
	if cert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		return cert, Fingerprint(cert.Certificate[0]), nil
	}
	if err := os.MkdirAll(s.remoteDir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "ATalk " + s.Core.Machine().Name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	return cert, Fingerprint(der), err
}

// Fingerprint 는 인증서(DER)의 지문.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// StartRemote 는 원격 포트를 연다 (port 0 이면 임의 포트). 실제 포트를 돌려준다.
func (s *Server) StartRemote(port int) (int, error) {
	if s.remoteDir == "" {
		return 0, errors.New("원격 기능을 쓸 수 없음 (설정 폴더 없음)")
	}
	s.StopRemote()
	cert, _, err := s.remoteCert()
	if err != nil {
		return 0, fmt.Errorf("인증서를 만들 수 없음: %w", err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(remoteListenHost, strconv.Itoa(port)))
	if err != nil {
		return 0, err
	}
	srv := &http.Server{
		Handler:           s.webOrAgent(s.remoteHandler()),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // 인증서를 믿지 않는 클라이언트의 TLS 오류로 로그가 넘치지 않게
	}
	tl := tls.NewListener(ln, srv.TLSConfig)
	s.remote.mu.Lock()
	s.remote.http, s.remote.port = srv, ln.Addr().(*net.TCPAddr).Port
	p := s.remote.port
	s.remote.mu.Unlock()
	go func() {
		if err := srv.Serve(tl); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("원격 포트: %v", err)
		}
	}()
	return p, nil
}

// StopRemote 는 원격 포트를 닫는다.
func (s *Server) StopRemote() {
	s.remote.mu.Lock()
	srv := s.remote.http
	s.remote.http, s.remote.port = nil, 0
	s.remote.mu.Unlock()
	if srv != nil {
		_ = srv.Close() // 열린 wait·SSE 도 바로 끊는다
	}
}

// RemotePort 는 열린 원격 포트 (닫혀 있으면 0).
func (s *Server) RemotePort() int {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	return s.remote.port
}

// startRemoteFromSettings 는 데몬이 시작할 때 설정에 켜져 있으면 원격 포트를 연다.
func (s *Server) startRemoteFromSettings() {
	if s.remoteDir == "" {
		return
	}
	if st := s.readRemoteSettings(); st.Enabled {
		if p, err := s.StartRemote(st.Port); err != nil {
			log.Printf("원격 포트 %d 를 열 수 없음: %v", st.Port, err)
		} else {
			log.Printf("원격 포트 실행 중: https://0.0.0.0:%d", p)
		}
	}
}

// NewPairCode 는 참여 코드를 만든다: <비밀값>.<인증서 지문>. 한 번만 쓸 수 있고 10분 뒤 만료된다.
func (s *Server) NewPairCode() (code string, expires time.Time, err error) {
	_, fp, err := s.remoteCert()
	if err != nil {
		return "", time.Time{}, err
	}
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b) // 16자, 대문자·숫자
	expires = time.Now().Add(pairCodeTTL)
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	if s.remote.codes == nil {
		s.remote.codes = map[string]time.Time{}
	}
	for k, t := range s.remote.codes {
		if time.Now().After(t) {
			delete(s.remote.codes, k)
		}
	}
	s.remote.codes[secret] = expires
	return secret + "." + fp, expires, nil
}

// usePairCode 는 참여 코드의 비밀값이 맞으면 지우고 true. 틀린 시도가 많으면 잠시 막는다.
func (s *Server) usePairCode(secret string) error {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	now := time.Now()
	recent := s.remote.fails[:0]
	for _, t := range s.remote.fails {
		if now.Sub(t) < pairCodeTTL {
			recent = append(recent, t)
		}
	}
	s.remote.fails = recent
	if len(recent) >= pairMaxFails {
		return &core.Error{Status: 429, Code: "too_many_attempts", Message: "참여 코드를 너무 많이 틀림. 잠시 뒤 다시 시도하세요"}
	}
	for k, exp := range s.remote.codes {
		if subtle.ConstantTimeCompare([]byte(k), []byte(strings.ToUpper(secret))) == 1 {
			delete(s.remote.codes, k)
			if now.After(exp) {
				break
			}
			return nil
		}
	}
	s.remote.fails = append(s.remote.fails, now)
	return &core.Error{Status: 403, Code: "bad_code", Message: "참여 코드가 틀렸거나 만료됨 (호스트 PC 에서 atalk remote code 로 새로 받으세요)"}
}

// LocalAddrs 는 이 PC 의 외부 IPv4 주소 (참여 안내에 보여 줌).
func LocalAddrs() []string {
	out := []string{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() && ipn.IP.To4() != nil {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// ───────────────────────── 원격 라우팅 ─────────────────────────

type remoteAgentKey struct{}

func remoteAgentOf(r *http.Request) string {
	v, _ := r.Context().Value(remoteAgentKey{}).(string)
	return v
}

// remoteHandler 는 원격 포트의 라우터. 허용한 경로만 토큰을 확인한 뒤 내부 라우터로 넘긴다.
func (s *Server) remoteHandler() http.Handler {
	inner := s.routes()
	c := s.Core
	mux := http.NewServeMux()
	type rule struct {
		self  bool // 경로의 {id} 가 토큰의 에이전트여야 함
		actor bool // 본문의 agent 를 토큰의 에이전트로 고정
		open  bool // 오래 열려 있는 요청 (wait, SSE) → 열려 있는 동안 살아 있음
		mine  bool // 내가 들어가 있는 방만 ({r})
	}
	authed := func(pattern string, ru rule, next http.Handler) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			agent := ""
			if ok {
				agent = c.RemoteAgent(strings.TrimSpace(tok))
			}
			if agent == "" {
				writeErr(w, &core.Error{Status: 401, Code: "unauthorized", Message: "원격 토큰이 없거나 철회됨 (atalk connect 로 다시 연결하세요)"})
				return
			}
			if ru.self && r.PathValue("id") != agent {
				writeErr(w, &core.Error{Status: 403, Code: "forbidden_agent", Message: "원격 연결은 자기 에이전트만 다룰 수 있음"})
				return
			}
			if ru.mine && c.RoomOf(agent) != r.PathValue("r") {
				writeErr(w, &core.Error{Status: 403, Code: "not_member", Message: "원격 에이전트는 들어가 있는 방의 대화만 볼 수 있음"})
				return
			}
			if ru.actor {
				if err := setActor(r, agent); err != nil {
					writeErr(w, err)
					return
				}
			}
			if ru.open {
				c.TouchRemote(agent, +1)
				defer c.TouchRemote(agent, -1)
			} else {
				c.TouchRemote(agent, 0)
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteAgentKey{}, agent)))
		})
	}
	pass := func(pattern string, ru rule) { authed(pattern, ru, inner) }
	fn := func(pattern string, ru rule, f handler) {
		authed(pattern, ru, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			out, err := f(w, r)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeOK(w, out)
		}))
	}

	// 인증 없이: 접속 확인, 페어링
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		m := c.Machine()
		writeOK(w, map[string]any{"remote": true, "machine": map[string]string{"name": m.Name, "os": m.OS}})
	})
	mux.HandleFunc("POST /remote/pair", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Code string             `json:"code"`
			Card core.RegisterInput `json:"card"`
		}
		if err := readBody(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		secret, _, _ := strings.Cut(strings.TrimSpace(b.Code), ".")
		if err := s.usePairCode(secret); err != nil {
			writeErr(w, err)
			return
		}
		b.Card.Cwd, b.Card.Pid = nil, nil
		res, tok, err := c.PairRemote(b.Card)
		if err != nil {
			writeErr(w, err)
			return
		}
		m := c.Machine()
		if res.Agent.Host != nil {
			forRemote(&res.Agent, res.Agent.Host.ID)
		}
		writeOK(w, map[string]any{"agent": res.Agent, "token": tok, "inherited_from": res.InheritedFrom, "previous_room": res.PreviousRoom,
			"machine": map[string]string{"name": m.Name, "os": m.OS}})
	})

	// 대기실 (인증 없이, 토큰 대신 에이전트가 만든 비밀값): 코드 없이 접속해 네임카드만 올리고 초대를 기다린다
	lobbyOn := func(w http.ResponseWriter) bool {
		if s.readRemoteSettings().NoLobby {
			writeErr(w, &core.Error{Status: 403, Code: "lobby_off", Message: "이 PC 는 대기실을 받지 않음 — 호스트 PC 에서 참여 코드(atalk remote code)를 받아 --code 로 연결하세요"})
			return false
		}
		return true
	}
	mux.HandleFunc("POST /remote/lobby", func(w http.ResponseWriter, r *http.Request) {
		if !lobbyOn(w) {
			return
		}
		var b struct {
			Card   core.RegisterInput `json:"card"`
			Secret string             `json:"secret"`
			Nonce  string             `json:"nonce"`
		}
		if err := readBody(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		_, fp, err := s.remoteCert()
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(b.Nonce) < 16 {
			writeErr(w, &core.Error{Status: 400, Code: "invalid_lobby", Message: "nonce 가 필요함"})
			return
		}
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		v, err := c.EnterLobby(b.Card, b.Secret, core.LobbyConfirm(fp, b.Nonce), ip)
		if err != nil {
			writeErr(w, err)
			return
		}
		forRemote(&v, "")
		m := c.Machine()
		writeOK(w, map[string]any{"agent": v, "machine": map[string]string{"name": m.Name, "os": m.OS}})
	})
	mux.HandleFunc("POST /remote/lobby/poll", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Agent  string `json:"agent"`
			Secret string `json:"secret"`
		}
		if err := readBody(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		tok, room, err := c.LobbyPoll(b.Agent, b.Secret)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, map[string]any{"token": tok, "room": room})
	})
	mux.HandleFunc("POST /remote/lobby/leave", func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Agent  string `json:"agent"`
			Secret string `json:"secret"`
		}
		if err := readBody(r, &b); err != nil {
			writeErr(w, err)
			return
		}
		if err := c.LeaveLobby(b.Agent, b.Secret); err != nil {
			writeErr(w, err)
			return
		}
		writeOK(w, map[string]any{})
	})

	// 디스커버리: 다른 에이전트의 경로·PID 는 빼고 보낸다
	fn("GET /agents", rule{}, func(w http.ResponseWriter, r *http.Request) (any, error) {
		a, err := c.ListAgents(r.URL.Query().Get("status"), r.URL.Query().Get("kind"))
		h := s.viewerHost(r)
		for i := range a {
			forRemote(&a[i], h)
		}
		return map[string]any{"agents": a}, err
	})
	fn("GET /agents/{id}", rule{}, func(w http.ResponseWriter, r *http.Request) (any, error) {
		a, err := c.GetAgent(r.PathValue("id"))
		forRemote(&a, s.viewerHost(r))
		return map[string]any{"agent": a}, err
	})
	// 다시 등록 (세션 시작·복귀): 내 카드만, 다른 PC 로
	fn("POST /agents", rule{}, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var in core.RegisterInput
		if err := readBody(r, &in); err != nil {
			return nil, err
		}
		me := remoteAgentOf(r)
		if in.SessionID == "" || core.MakeAgentID(in.Kind, in.SessionID) != me {
			return nil, &core.Error{Status: 403, Code: "forbidden_agent", Message: "원격 연결은 자기 에이전트만 등록할 수 있음"}
		}
		cur, err := c.GetAgent(me)
		if err != nil {
			return nil, err
		}
		if in.Host == nil || in.Host.ID == "" || in.Host.ID == c.Machine().ID {
			in.Host = cur.Host
		}
		in.Cwd, in.Pid, in.Provisional = nil, nil, false
		res, err := c.Register(in)
		forRemote(&res.Agent, s.viewerHost(r))
		return res, err
	})
	// 원격 에이전트가 자기 PC 에서 띄운 협업 세션(atalk spawn)을 이 데몬에 등록 → 새 세션의 토큰. 띄운 에이전트의 방에 바로 들어감
	fn("POST /remote/spawn", rule{}, func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Card core.RegisterInput `json:"card"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		res, tok, err := c.SpawnRemote(remoteAgentOf(r), b.Card)
		if err != nil {
			return nil, err
		}
		forRemote(&res.Agent, s.viewerHost(r))
		return map[string]any{"agent": res.Agent, "token": tok}, nil
	})
	fn("POST /remote/disconnect", rule{}, func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, c.RevokeRemote(remoteAgentOf(r))
	})
	pass("PATCH /agents/{id}", rule{self: true})
	for _, p := range []string{"echo", "activate", "activity", "away", "leave", "ack", "delivered", "need-input", "ask"} {
		pass("POST /agents/{id}/"+p, rule{self: true})
	}
	pass("POST /approval", rule{actor: true, open: true}) // 허가 창·질문을 호스트 화면 카드에서 답하게 (답할 때까지 기다림)
	pass("GET /agents/{id}/unread", rule{self: true})
	pass("GET /agents/{id}/pending", rule{self: true})
	pass("GET /agents/{id}/wait", rule{self: true, open: true})
	pass("GET /agents/{id}/stream", rule{self: true, open: true})
	pass("GET /names", rule{})

	// 대화방
	pass("GET /rooms", rule{})
	pass("GET /rooms/{r}", rule{})
	pass("POST /rooms", rule{actor: true})
	pass("POST /rooms/{r}/invites", rule{actor: true})
	pass("POST /rooms/{r}/invites/respond", rule{actor: true})
	pass("POST /rooms/{r}/requests", rule{actor: true})
	pass("POST /rooms/{r}/requests/respond", rule{actor: true})
	pass("POST /rooms/{r}/say", rule{actor: true, mine: true})
	pass("GET /rooms/{r}/messages", rule{mine: true})
	pass("POST /rooms/{r}/messages", rule{actor: true, mine: true})
	pass("GET /rooms/{r}/logs", rule{mine: true})
	pass("POST /rooms/{r}/logs", rule{actor: true, mine: true})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, &core.Error{Status: 404, Code: "no_route", Message: "원격 포트에서 쓸 수 없는 경로: " + r.Method + " " + r.URL.Path})
	})
	return mux
}

// viewerHost 는 원격 요청을 보낸 에이전트의 PC ID.
func (s *Server) viewerHost(r *http.Request) string {
	if v, err := s.Core.GetAgent(remoteAgentOf(r)); err == nil && v.Host != nil {
		return v.Host.ID
	}
	return ""
}

// forRemote 는 원격에 보내는 카드를 고친다: 경로·PID 는 빼고, "이 PC"(local) 는 호스트 데몬이 아니라
// 요청한 에이전트의 PC 기준으로 다시 정한다 (원격에서 호스트 PC 의 에이전트가 "이 PC" 로 보이지 않게).
func forRemote(a *core.AgentView, viewerHost string) {
	a.Cwd, a.Pid, a.Lobby = nil, nil, nil
	a.Local = viewerHost != "" && a.Host != nil && a.Host.ID == viewerHost
}

// setActor 는 요청 본문(JSON 객체)의 agent 를 토큰의 에이전트로 바꾼다.
// 본문에 agent 가 없으면 "사람(관리자)" 으로 처리되는 경로가 있으므로, 원격 요청은 항상 에이전트로 만든다.
func setActor(r *http.Request, agent string) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1_000_001))
	if err != nil {
		return err
	}
	if len(b) > 1_000_000 {
		return &core.Error{Status: 413, Code: "too_large", Message: "요청 본문이 너무 큼 (최대 1MB)"}
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &m); err != nil || m == nil {
			return &core.Error{Status: 400, Code: "bad_json", Message: "JSON 객체 본문이 필요함"}
		}
	}
	if a, ok := m["agent"].(string); ok && a != "" && a != agent {
		return &core.Error{Status: 403, Code: "forbidden_agent", Message: "원격 연결은 자기 에이전트로만 요청할 수 있음"}
	}
	m["agent"] = agent
	nb, _ := json.Marshal(m)
	r.Body = io.NopCloser(bytes.NewReader(nb))
	r.ContentLength = int64(len(nb))
	return nil
}

// ───────────────────────── 로컬 관리 API ─────────────────────────

func (s *Server) remoteAdminRoutes(h func(string, handler)) {
	c := s.Core
	status := func() (any, error) {
		st := remoteSettings{}
		fp := ""
		if s.remoteDir != "" {
			st = s.readRemoteSettings()
			if _, err := os.Stat(filepath.Join(s.remoteDir, "cert.pem")); err == nil {
				_, fp, _ = s.remoteCert()
			}
		}
		conns, err := c.RemoteConns()
		lobby := []core.AgentView{}
		if all, e := c.ListAgents("", ""); e == nil {
			for _, a := range all {
				if a.Lobby != nil {
					lobby = append(lobby, a)
				}
			}
		}
		return map[string]any{"enabled": s.RemotePort() > 0, "port": s.RemotePort(), "saved_port": st.Port, "fingerprint": fp,
			"addresses": LocalAddrs(), "connections": conns, "lobby_enabled": !st.NoLobby, "lobby": lobby}, err
	}
	h("PUT /admin/remote/lobby", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			On bool `json:"on"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		if s.remoteDir == "" {
			return nil, &core.Error{Status: 409, Code: "remote_unavailable", Message: "원격 기능을 쓸 수 없음"}
		}
		st := s.readRemoteSettings()
		st.NoLobby = !b.On
		if err := s.writeRemoteSettings(st); err != nil {
			return nil, err
		}
		return status()
	})
	h("DELETE /admin/remote/lobby/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, c.RemoveLobby(r.PathValue("id"))
	})
	h("GET /admin/remote", func(w http.ResponseWriter, r *http.Request) (any, error) { return status() })
	h("PUT /admin/remote", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			On   bool `json:"on"`
			Port int  `json:"port"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		if s.remoteDir == "" {
			return nil, &core.Error{Status: 409, Code: "remote_unavailable", Message: "원격 기능을 쓸 수 없음"}
		}
		st := s.readRemoteSettings()
		if b.Port != 0 {
			if b.Port < 1 || b.Port > 65535 {
				return nil, &core.Error{Status: 400, Code: "invalid_port", Message: "포트는 1~65535"}
			}
			st.Port = b.Port
		}
		if b.On {
			if _, err := s.StartRemote(st.Port); err != nil {
				return nil, &core.Error{Status: 409, Code: "listen_failed", Message: fmt.Sprintf("포트 %d 를 열 수 없음: %v", st.Port, err)}
			}
		} else {
			s.StopRemote()
		}
		st.Enabled = b.On
		if err := s.writeRemoteSettings(st); err != nil {
			return nil, err
		}
		return status()
	})
	h("POST /admin/remote/code", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if s.RemotePort() == 0 {
			return nil, &core.Error{Status: 409, Code: "remote_off", Message: "원격 포트가 꺼져 있음 (atalk remote on)"}
		}
		code, exp, err := s.NewPairCode()
		if err != nil {
			return nil, err
		}
		urls := []string{}
		for _, a := range LocalAddrs() {
			urls = append(urls, fmt.Sprintf("https://%s:%d", a, s.RemotePort()))
		}
		return map[string]any{"code": code, "expires_at": exp.UTC().Format(time.RFC3339), "urls": urls}, nil
	})
	h("DELETE /admin/remote/agents/{id}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{}, c.RevokeRemote(r.PathValue("id"))
	})
	// 웹 화면 로그인 코드 (webremote.go). 원격 포트로 로그인한 브라우저에서는 못 받는다 (이 PC 에서만).
	h("POST /admin/remote/web", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if isRemoteWeb(r) {
			return nil, &core.Error{Status: 403, Code: "local_only", Message: "웹 로그인 코드는 이 PC 에서만 받을 수 있음"}
		}
		if s.RemotePort() == 0 {
			return nil, &core.Error{Status: 409, Code: "remote_off", Message: "원격 포트가 꺼져 있음 (atalk remote on)"}
		}
		code, exp, err := s.NewWebCode()
		if err != nil {
			return nil, err
		}
		urls := []string{}
		for _, a := range LocalAddrs() {
			urls = append(urls, fmt.Sprintf("https://%s:%d/login", a, s.RemotePort()))
		}
		return map[string]any{"code": code, "expires_at": exp.UTC().Format(time.RFC3339), "urls": urls}, nil
	})
	// 구글 로그인 (webgoogle.go): 설정과 허용 목록은 이 PC 에서만 바꾼다 (바깥 브라우저가 자기 계정을 넣지 못하게)
	localOnlyErr := &core.Error{Status: 403, Code: "local_only", Message: "이 PC 에서만 바꿀 수 있음"}
	h("GET /admin/remote/google", func(w http.ResponseWriter, r *http.Request) (any, error) {
		g, ok := s.googleConfig()
		return map[string]any{"configured": ok, "client_id": g.ClientID, "base": g.Base, "users": s.WebUsers()}, nil
	})
	h("PUT /admin/remote/google", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if isRemoteWeb(r) {
			return nil, localOnlyErr
		}
		var g GoogleConfig
		if err := readBody(r, &g); err != nil {
			return nil, err
		}
		if err := s.SetGoogle(g); err != nil {
			return nil, err
		}
		g, ok := s.googleConfig()
		return map[string]any{"configured": ok, "client_id": g.ClientID, "base": g.Base, "users": s.WebUsers()}, nil
	})
	h("POST /admin/remote/web/users", func(w http.ResponseWriter, r *http.Request) (any, error) {
		if isRemoteWeb(r) {
			return nil, localOnlyErr
		}
		var b struct {
			Email string `json:"email"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		u, err := s.SetWebUser(b.Email, true)
		return map[string]any{"users": u}, err
	})
	h("DELETE /admin/remote/web/users/{email}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		u, err := s.SetWebUser(r.PathValue("email"), false)
		return map[string]any{"users": u}, err
	})
	h("DELETE /admin/remote/web", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"logged_out": s.ClearWebSessions()}, nil
	})
}
