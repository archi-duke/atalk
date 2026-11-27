package server

// 원격 포트 웹 화면의 사내 SSO 로그인 (ADFS/MobilAve 계약 2.0.0 — GoJIRA 와 같은 방식).
// OIDC 가 아니라 사내 고유 흐름:
//
//	/auth/sso → <SSO>/Account/ADFSLogin?client=<base>/auth/sso/callback?state=…
//	→ <base>/auth/sso/callback?state=…&auth=1&code=… → 서버가 <SSO>/Account/exchange?code= 로 sessionToken
//	→ <SSO>/Account/introspect?token= 로 {loginid, active} → 허용 목록에 "sso:<loginid>" 가 있으면 로그인 쿠키.
//
// 클라이언트 등록·보안 비밀이 없는 계약이라, 로그인 위조(남의 code 를 심는 것)는 state 쿠키로 막고,
// code 교환·확인은 브라우저가 아니라 이 PC 의 데몬이 SSO 서버에 직접 한다. 설정은 remote-server/sso.json.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"atalk/internal/core"
)

var ssoHTTP = &http.Client{Timeout: 15 * time.Second}

const (
	ssoCookie   = "atalk_sso"
	ssoCallback = "/auth/sso/callback"
	ssoPrefix   = "sso:" // 허용 목록에서 SSO 사용자 (구글 이메일과 구분)
)

// SSOConfig 는 사내 SSO 로그인 설정.
type SSOConfig struct {
	URL  string `json:"url"`            // SSO 서버, 예: http://192.168.219.106:8443
	Base string `json:"base"`           // 이 화면의 바깥 주소, 예: https://archi-duke.ddns.net/atalk
	Name string `json:"name,omitempty"` // 로그인 버튼 이름 (기본 "사내 SSO")
}

// Label 은 로그인 버튼에 보일 이름.
func (c SSOConfig) Label() string {
	if c.Name == "" {
		return "사내 SSO"
	}
	return c.Name
}

func (s *Server) ssoFile() string { return filepath.Join(s.remoteDir, "sso.json") }

// ssoConfig 는 설정을 읽는다 (없으면 ok=false).
func (s *Server) ssoConfig() (SSOConfig, bool) {
	var c SSOConfig
	if s.remoteDir == "" {
		return c, false
	}
	b, err := os.ReadFile(s.ssoFile())
	if err != nil || json.Unmarshal(b, &c) != nil || c.URL == "" || c.Base == "" {
		return c, false
	}
	return c, true
}

// SetSSO 는 사내 SSO 설정을 저장한다 (URL 이 "" 이면 지움).
func (s *Server) SetSSO(c SSOConfig) error {
	if s.remoteDir == "" {
		return &core.Error{Status: 409, Code: "remote_unavailable", Message: "원격 기능을 쓸 수 없음"}
	}
	if c.URL == "" {
		if err := os.Remove(s.ssoFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	u, err := url.Parse(strings.TrimRight(c.URL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && !prefixRe.MatchString(u.Path)) || u.RawQuery != "" {
		return &core.Error{Status: 400, Code: "bad_url", Message: "SSO 주소는 http(s)://호스트[:포트][/경로] 형식이어야 함 (예: http://192.168.219.106:8443)"}
	}
	b, err := url.Parse(strings.TrimRight(c.Base, "/"))
	if err != nil || b.Scheme != "https" || b.Host == "" || (b.Path != "" && !prefixRe.MatchString(b.Path)) || b.RawQuery != "" {
		return &core.Error{Status: 400, Code: "bad_base", Message: "base 는 https://도메인[:포트][/경로] 형식이어야 함 (예: https://archi-duke.ddns.net/atalk)"}
	}
	c.URL, c.Base, c.Name = u.String(), b.String(), strings.TrimSpace(c.Name)
	if err := os.MkdirAll(s.remoteDir, 0o700); err != nil {
		return err
	}
	j, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(s.ssoFile(), j, 0o600)
}

// ssoStart: /auth/sso — SSO 로그인 화면으로. 설정한 주소가 아닌 곳으로 왔으면 그 주소로 먼저 옮긴다 (쿠키는 주소마다 따로).
func (s *Server) ssoStart(w http.ResponseWriter, r *http.Request) {
	c, ok := s.ssoConfig()
	if !ok {
		http.Redirect(w, r, webPrefix(r)+"/login", http.StatusSeeOther)
		return
	}
	base, _ := url.Parse(c.Base)
	if !strings.EqualFold(r.Host, base.Host) || webPrefix(r) != base.Path {
		http.Redirect(w, r, c.Base+"/auth/sso", http.StatusSeeOther)
		return
	}
	state := randToken(24)
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: state, Path: webCookiePath(r), MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	back := c.Base + ssoCallback + "?state=" + url.QueryEscape(state)
	http.Redirect(w, r, c.URL+"/Account/ADFSLogin?"+url.Values{"client": {back}}.Encode(), http.StatusSeeOther)
}

// ssoCallbackHandler: SSO 에서 돌아옴 → code 를 sessionToken 으로 바꾸고 loginid 확인 → 허용 목록이면 로그인.
func (s *Server) ssoCallbackHandler(w http.ResponseWriter, r *http.Request) {
	fail := func(msg string) { s.loginPage(w, r, msg) }
	c, ok := s.ssoConfig()
	if !ok {
		fail("사내 SSO 로그인이 설정되지 않았습니다.")
		return
	}
	ck, err := r.Cookie(ssoCookie)
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: webCookiePath(r), MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	q := r.URL.Query()
	if err != nil || ck.Value == "" || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(q.Get("state"))) != 1 {
		fail("로그인 요청이 만료됐거나 맞지 않습니다. 다시 시도하세요.")
		return
	}
	id, err := ssoExchange(c, q.Get("code"))
	if err != nil {
		fail(err.Error())
		return
	}
	if !slices.Contains(s.WebUsers(), ssoPrefix+id) {
		fail(id + " 은 등록되지 않은 사용자입니다. 이 PC 에서 atalk remote web users add " + ssoPrefix + id + " 로 등록하세요.")
		return
	}
	s.webLoggedIn(w, r, id)
}

// ssoExchange 는 code → sessionToken → loginid (소문자). 이 PC 데몬이 SSO 서버에 직접 묻는다.
func ssoExchange(c SSOConfig, code string) (string, error) {
	if code == "" {
		return "", errors.New("SSO 에서 받은 코드가 없습니다.")
	}
	var ex struct {
		SessionToken string `json:"sessionToken"`
	}
	if err := ssoGet(c.URL+"/Account/exchange?"+url.Values{"code": {code}}.Encode(), &ex); err != nil || ex.SessionToken == "" {
		return "", fmt.Errorf("SSO 코드를 바꾸지 못했습니다 (만료됐거나 이미 쓴 코드). 다시 시도하세요.%s", errSuffix(err))
	}
	var in struct {
		LoginID string `json:"loginid"`
		Active  any    `json:"active"` // 계약은 "true" (문자열), true 도 받음
	}
	if err := ssoGet(c.URL+"/Account/introspect?"+url.Values{"token": {ex.SessionToken}}.Encode(), &in); err != nil {
		return "", fmt.Errorf("SSO 에서 사용자를 확인하지 못했습니다.%s", errSuffix(err))
	}
	if a := fmt.Sprint(in.Active); a != "true" {
		return "", errors.New("SSO 세션이 유효하지 않습니다. 다시 로그인하세요.")
	}
	id := strings.ToLower(strings.TrimSpace(in.LoginID))
	if id == "" || !loginIDRe.MatchString(id) {
		return "", errors.New("SSO 에서 받은 사용자 아이디가 이상합니다.")
	}
	return id, nil
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

func ssoGet(u string, v any) error {
	resp, err := ssoHTTP.Get(u)
	if err != nil {
		return errors.New("SSO 서버에 연결하지 못함")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("SSO 응답 %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// webLoggedIn 은 확인된 사용자에게 로그인 쿠키를 주고 대화방 화면으로 보낸다 (구글·SSO 공통).
// 다른 사이트(구글·SSO)에서 넘어온 이동이라 바로 "/" 로 넘기면 SameSite=Strict 쿠키가 실리지 않는다 → 이 페이지에서 다시 이동.
func (s *Server) webLoggedIn(w http.ResponseWriter, r *http.Request, who string) {
	tok := randToken(32)
	s.remote.mu.Lock()
	s.loadWebSessions()
	s.remote.web[hashToken(tok)] = time.Now().Add(webSessionTTL)
	s.saveWebSessions()
	s.remote.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: webCookie, Value: tok, Path: webCookiePath(r), MaxAge: int(webSessionTTL / time.Second), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>ATalk</title>` +
		`<p style="font:16px system-ui;padding:24px">` + html.EscapeString(who) + ` 로 로그인했습니다. <a href="` + webPrefix(r) + `/">대화방으로</a></p><script>location.replace("` + webPrefix(r) + `/")</script>`))
}

// loginIDRe: SSO 로그인 아이디로 받는 형식 (예: duke.kimm).
var loginIDRe = regexp.MustCompile(`^[a-z0-9._@-]{1,128}$`)
