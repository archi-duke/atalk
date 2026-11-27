package server

// 원격 포트 웹 화면의 구글 로그인: 구글 계정으로 로그인해서, 이 PC 에 등록된(허용 목록) 이메일이면 들어온다.
// 구글 OAuth 클라이언트(웹 애플리케이션)는 사용자가 만들고 ID·보안 비밀을 이 PC 에만 저장한다 (remote-server/google.json).
// 리디렉션 URI 는 <base>/auth/google/callback. 구글은 IP 주소를 허용하지 않으므로 base 는 도메인 (예: DDNS).
//
// 흐름: /auth/google → 구글 → /auth/google/callback?code&state → 토큰 엔드포인트에서 id_token 을 직접 받음
// (서버가 TLS 로 구글에서 바로 받은 토큰이라 서명 검증 대신 aud·iss·exp·nonce·email_verified 확인 — OIDC 3.1.3.7)
// → 허용 목록에 있으면 로그인 쿠키.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"atalk/internal/core"
)

var (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleHTTP     = &http.Client{Timeout: 15 * time.Second}
)

const oauthCookie = "atalk_oauth"

// GoogleConfig 는 구글 로그인 설정 (remote-server/google.json, 0600).
type GoogleConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Base         string `json:"base"` // 예: https://archi-duke.ddns.net:44310 (구글에 등록한 리디렉션 URI 의 앞부분)
}

func (s *Server) googleFile() string   { return filepath.Join(s.remoteDir, "google.json") }
func (s *Server) webUsersFile() string { return filepath.Join(s.remoteDir, "web_users.json") }

func (s *Server) googleConfig() (GoogleConfig, bool) {
	var g GoogleConfig
	if s.remoteDir == "" {
		return g, false
	}
	b, err := os.ReadFile(s.googleFile())
	if err != nil || json.Unmarshal(b, &g) != nil || g.ClientID == "" || g.ClientSecret == "" || g.Base == "" {
		return g, false
	}
	return g, true
}

// SetGoogle 은 구글 로그인 설정을 저장한다 (ClientID 가 "" 이면 지움).
func (s *Server) SetGoogle(g GoogleConfig) error {
	if s.remoteDir == "" {
		return &core.Error{Status: 409, Code: "remote_unavailable", Message: "원격 기능을 쓸 수 없음"}
	}
	if g.ClientID == "" {
		if err := os.Remove(s.googleFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	u, err := url.Parse(strings.TrimRight(g.Base, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" {
		return &core.Error{Status: 400, Code: "bad_base", Message: "base 는 https://도메인:포트 형식이어야 함 (예: https://archi-duke.ddns.net:44310)"}
	}
	if g.ClientSecret == "" {
		return &core.Error{Status: 400, Code: "no_secret", Message: "보안 비밀(client secret)이 필요함"}
	}
	g.Base = u.String()
	if err := os.MkdirAll(s.remoteDir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(g, "", "  ")
	return os.WriteFile(s.googleFile(), b, 0o600)
}

// WebUsers 는 웹 화면에 들어올 수 있는 구글 계정 이메일 (소문자).
func (s *Server) WebUsers() []string {
	var u []string
	if b, err := os.ReadFile(s.webUsersFile()); err == nil {
		_ = json.Unmarshal(b, &u)
	}
	if u == nil {
		u = []string{}
	}
	return u
}

// SetWebUser 는 허용 목록에 넣거나(on) 뺀다.
func (s *Server) SetWebUser(email string, on bool) ([]string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return nil, &core.Error{Status: 400, Code: "bad_email", Message: "이메일 주소가 필요함"}
	}
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	u := s.WebUsers()
	i := slices.Index(u, email)
	switch {
	case on && i < 0:
		u = append(u, email)
	case !on && i >= 0:
		u = slices.Delete(u, i, i+1)
	default:
		return u, nil
	}
	if err := os.MkdirAll(s.remoteDir, 0o700); err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(u, "", "  ")
	return u, os.WriteFile(s.webUsersFile(), b, 0o600)
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// googleStart: /auth/google — 구글 로그인 화면으로. 설정한 도메인이 아닌 주소로 왔으면 그 도메인으로 먼저 옮긴다
// (로그인 쿠키는 주소마다 따로이고, 구글은 등록한 리디렉션 URI 로만 돌려보내므로).
func (s *Server) googleStart(w http.ResponseWriter, r *http.Request) {
	g, ok := s.googleConfig()
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	base, _ := url.Parse(g.Base)
	if !strings.EqualFold(r.Host, base.Host) {
		http.Redirect(w, r, g.Base+"/auth/google", http.StatusSeeOther)
		return
	}
	state, nonce := randToken(24), randToken(24)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: state + "." + nonce, Path: "/auth/google", MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	q := url.Values{
		"client_id":     {g.ClientID},
		"redirect_uri":  {g.Base + "/auth/google/callback"},
		"response_type": {"code"},
		"scope":         {"openid email"},
		"state":         {state},
		"nonce":         {nonce},
		"prompt":        {"select_account"},
	}
	http.Redirect(w, r, googleAuthURL+"?"+q.Encode(), http.StatusSeeOther)
}

// googleCallback: 구글에서 돌아옴 → 코드로 id_token 을 받아 이메일 확인 → 허용 목록에 있으면 로그인.
func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) { s.loginPage(w, r, msg) }
	g, ok := s.googleConfig()
	if !ok {
		fail("구글 로그인이 설정되지 않았습니다.")
		return
	}
	ck, err := r.Cookie(oauthCookie)
	http.SetCookie(w, &http.Cookie{Name: oauthCookie, Value: "", Path: "/auth/google", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	state, nonce, _ := strings.Cut(func() string {
		if err != nil {
			return ""
		}
		return ck.Value
	}(), ".")
	if e := r.URL.Query().Get("error"); e != "" {
		fail("구글 로그인을 취소했거나 실패했습니다 (" + e + ").")
		return
	}
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(r.URL.Query().Get("state"))) != 1 {
		fail("로그인 요청이 만료됐거나 맞지 않습니다. 다시 시도하세요.")
		return
	}
	email, err := googleExchange(g, r.URL.Query().Get("code"), nonce)
	if err != nil {
		fail(err.Error())
		return
	}
	if !slices.Contains(s.WebUsers(), email) {
		fail(email + " 은 등록되지 않은 계정입니다. 이 PC 에서 atalk remote web users add " + email + " 로 등록하세요.")
		return
	}
	tok := randToken(32)
	s.remote.mu.Lock()
	s.loadWebSessions()
	s.remote.web[hashToken(tok)] = time.Now().Add(webSessionTTL)
	s.saveWebSessions()
	s.remote.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: webCookie, Value: tok, Path: "/", MaxAge: int(webSessionTTL / time.Second), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	// 구글에서 넘어온(다른 사이트에서 시작한) 이동이라 바로 "/" 로 넘기면 SameSite=Strict 쿠키가 실리지 않는다 → 이 페이지에서 다시 이동
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>ATalk</title>` +
		`<p style="font:16px system-ui;padding:24px">` + html.EscapeString(email) + ` 로 로그인했습니다. <a href="/">대화방으로</a></p><script>location.replace("/")</script>`))
}

// googleExchange 는 인가 코드를 id_token 으로 바꾸고 확인된 이메일(소문자)을 돌려준다.
func googleExchange(g GoogleConfig, code, nonce string) (string, error) {
	if code == "" {
		return "", errors.New("구글에서 받은 코드가 없습니다.")
	}
	resp, err := googleHTTP.PostForm(googleTokenURL, url.Values{
		"code":          {code},
		"client_id":     {g.ClientID},
		"client_secret": {g.ClientSecret},
		"redirect_uri":  {g.Base + "/auth/google/callback"},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return "", errors.New("구글에 연결하지 못했습니다: " + err.Error())
	}
	defer resp.Body.Close()
	var tr struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil || tr.IDToken == "" {
		return "", errors.New("구글 토큰을 받지 못했습니다: " + strings.TrimSpace(tr.Error+" "+tr.Desc))
	}
	parts := strings.Split(tr.IDToken, ".")
	if len(parts) != 3 {
		return "", errors.New("구글 토큰 형식이 이상합니다.")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("구글 토큰을 읽지 못했습니다.")
	}
	var c struct {
		Iss      string `json:"iss"`
		Aud      string `json:"aud"`
		Exp      int64  `json:"exp"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Nonce    string `json:"nonce"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", errors.New("구글 토큰을 읽지 못했습니다.")
	}
	switch {
	case c.Iss != "https://accounts.google.com" && c.Iss != "accounts.google.com":
		return "", errors.New("구글이 발급한 토큰이 아닙니다.")
	case c.Aud != g.ClientID:
		return "", errors.New("이 앱용 토큰이 아닙니다.")
	case time.Now().Unix() > c.Exp:
		return "", errors.New("구글 토큰이 만료됐습니다. 다시 시도하세요.")
	case nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(nonce)) != 1:
		return "", errors.New("로그인 요청이 맞지 않습니다. 다시 시도하세요.")
	case c.Email == "" || !c.Verified:
		return "", errors.New("구글에서 확인된 이메일이 없습니다.")
	}
	return strings.ToLower(c.Email), nil
}
