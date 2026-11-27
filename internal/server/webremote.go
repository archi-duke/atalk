package server

// 원격 포트의 웹 화면: 바깥(휴대폰 등)에서 브라우저로 이 PC 의 대화 화면을 쓴다.
// 이 PC 에서 1회용 웹 코드(atalk remote web)를 받아 https://<주소>:<원격포트>/login 에 넣으면
// 로그인 쿠키(HttpOnly·Secure·SameSite=Strict)를 받고, 그 쿠키가 있는 요청은 로컬(127.0.0.1:7420) 화면과 똑같이 처리한다.
// 웹 로그인은 이 PC 주인의 권한이므로 코드는 이 PC 에서만 (로컬 관리 API) 발급한다.
// 에이전트 요청(Authorization: Bearer)은 지금까지처럼 remoteHandler 가 받는다.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"atalk/internal/core"
)

const (
	webCodeTTL    = 10 * time.Minute
	webSessionTTL = 30 * 24 * time.Hour
	webCookie     = "atalk_web"
)

func (s *Server) webSessionsFile() string { return filepath.Join(s.remoteDir, "web_sessions.json") }

// loadWebSessions: 로그인 쿠키(의 sha256) → 만료 시각. r.mu 를 잡고 부른다.
func (s *Server) loadWebSessions() {
	if s.remote.web != nil {
		return
	}
	s.remote.web = map[string]time.Time{}
	if b, err := os.ReadFile(s.webSessionsFile()); err == nil {
		_ = json.Unmarshal(b, &s.remote.web)
	}
}

func (s *Server) saveWebSessions() {
	now := time.Now()
	for k, t := range s.remote.web {
		if now.After(t) {
			delete(s.remote.web, k)
		}
	}
	if err := os.MkdirAll(s.remoteDir, 0o700); err != nil {
		return
	}
	b, _ := json.MarshalIndent(s.remote.web, "", "  ")
	_ = os.WriteFile(s.webSessionsFile(), b, 0o600)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// NewWebCode 는 웹 로그인 코드를 만든다 (한 번만, 10분).
func (s *Server) NewWebCode() (string, time.Time, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	exp := time.Now().Add(webCodeTTL)
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	if s.remote.webCodes == nil {
		s.remote.webCodes = map[string]time.Time{}
	}
	for k, t := range s.remote.webCodes {
		if time.Now().After(t) {
			delete(s.remote.webCodes, k)
		}
	}
	s.remote.webCodes[code] = exp
	return code, exp, nil
}

// webLoginWith 는 코드가 맞으면 지우고 새 로그인 토큰을 돌려준다. 틀린 시도는 참여 코드와 같은 제한을 받는다.
func (s *Server) webLoginWith(code string) (string, error) {
	code = strings.ToUpper(strings.Join(strings.Fields(code), ""))
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
		return "", &core.Error{Status: 429, Code: "too_many_attempts", Message: "코드를 너무 많이 틀림. 잠시 뒤 다시 시도하세요"}
	}
	for k, exp := range s.remote.webCodes {
		if subtle.ConstantTimeCompare([]byte(k), []byte(code)) == 1 {
			delete(s.remote.webCodes, k)
			if now.After(exp) {
				break
			}
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return "", err
			}
			tok := base64.RawURLEncoding.EncodeToString(b)
			s.loadWebSessions()
			s.remote.web[hashToken(tok)] = now.Add(webSessionTTL)
			s.saveWebSessions()
			return tok, nil
		}
	}
	s.remote.fails = append(s.remote.fails, now)
	return "", &core.Error{Status: 403, Code: "bad_code", Message: "코드가 틀렸거나 만료됨 (이 PC 에서 atalk remote web 으로 새로 받으세요)"}
}

func (s *Server) webSessionOK(tok string) bool {
	if tok == "" {
		return false
	}
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	s.loadWebSessions()
	exp, ok := s.remote.web[hashToken(tok)]
	return ok && time.Now().Before(exp)
}

func (s *Server) dropWebSession(tok string) {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	s.loadWebSessions()
	delete(s.remote.web, hashToken(tok))
	s.saveWebSessions()
}

// ClearWebSessions 는 모든 웹 로그인을 끊는다. 끊은 개수를 돌려준다.
func (s *Server) ClearWebSessions() int {
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	s.loadWebSessions()
	n := len(s.remote.web)
	s.remote.web = map[string]time.Time{}
	s.saveWebSessions()
	return n
}

// sameOrigin: 쓰기 요청이 이 화면에서 온 것인지 (다른 사이트가 쿠키를 실어 보내는 것을 막음).
func sameOrigin(r *http.Request) bool {
	if sf := r.Header.Get("Sec-Fetch-Site"); sf != "" && sf != "same-origin" && sf != "none" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	if o == "null" { // 출처를 숨기는 브라우저 설정·페이지 (Referrer-Policy no-referrer 등): Sec-Fetch-Site 로만 판단
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// webOrAgent 는 원격 포트의 맨 앞 라우터: 에이전트 토큰 요청은 api 로, 로그인한 브라우저는 로컬 화면 라우터로.
func (s *Server) webOrAgent(api http.Handler) http.Handler {
	inner := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			api.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/login":
			s.webLogin(w, r)
			return
		case "/auth/google":
			s.googleStart(w, r)
			return
		case "/auth/google/callback":
			s.googleCallback(w, r)
			return
		case "/logout":
			if r.Method == http.MethodPost && sameOrigin(r) {
				if ck, err := r.Cookie(webCookie); err == nil {
					s.dropWebSession(ck.Value)
				}
				http.SetCookie(w, &http.Cookie{Name: webCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
		}
		if ck, err := r.Cookie(webCookie); err == nil && s.webSessionOK(ck.Value) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
				writeErr(w, &core.Error{Status: 403, Code: "forbidden_origin", Message: "다른 출처에서 온 쓰기 요청은 받지 않음"})
				return
			}
			inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), remoteWebKey{}, true)))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		api.ServeHTTP(w, r)
	})
}

func (s *Server) webLogin(w http.ResponseWriter, r *http.Request) {
	msg := ""
	if r.Method == http.MethodPost {
		if !sameOrigin(r) {
			msg = "다른 출처에서 온 요청은 받지 않습니다."
		} else if tok, err := s.webLoginWith(r.PostFormValue("code")); err != nil {
			msg = err.Error()
			if e, ok := err.(*core.Error); ok {
				msg = e.Message
			}
		} else {
			http.SetCookie(w, &http.Cookie{Name: webCookie, Value: tok, Path: "/", MaxAge: int(webSessionTTL / time.Second), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	} else if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, &core.Error{Status: 405, Code: "method_not_allowed", Message: "GET 또는 POST"})
		return
	}
	s.loginPage(w, r, msg)
}

// loginPage 는 로그인 화면 (구글 로그인이 설정돼 있으면 구글 버튼이 먼저, 코드 로그인은 접어 둠).
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request, msg string) {
	code := r.URL.Query().Get("code")
	googleBtn, codeOpen := "", " open"
	if _, ok := s.googleConfig(); ok {
		googleBtn = `<a class="gbtn" href="/auth/google">Google 계정으로 로그인</a><p class="hint">이 PC 에 등록된 구글 계정만 들어올 수 있습니다.</p>`
		if code == "" {
			codeOpen = ""
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	page := strings.NewReplacer("{{MSG}}", html.EscapeString(msg), "{{CODE}}", html.EscapeString(code), "{{PC}}", html.EscapeString(s.Core.Machine().Name), "{{GOOGLE}}", googleBtn, "{{OPEN}}", codeOpen).Replace(webLoginPage)
	_, _ = w.Write([]byte(page))
}

const webLoginPage = `<!doctype html>
<html lang="ko"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>ATalk 로그인</title>
<style>
:root{--bg:#f6f7f9;--fg:#1c1f24;--card:#fff;--line:#d9dde3;--acc:#2f6fed;--err:#c62828}
@media (prefers-color-scheme:dark){:root{--bg:#15171a;--fg:#e8eaed;--card:#1f2226;--line:#33373d;--acc:#6c9cff;--err:#ff8a80}}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif;display:flex;min-height:100vh;align-items:center;justify-content:center}
form{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:24px;width:min(360px,calc(100vw - 32px));box-sizing:border-box}
h1{font-size:20px;margin:0 0 4px}p{margin:0 0 16px;opacity:.8;font-size:14px}
input{width:100%;box-sizing:border-box;padding:10px;font:inherit;letter-spacing:2px;text-transform:uppercase;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit}
button{margin-top:12px;width:100%;padding:10px;font:inherit;border:0;border-radius:8px;background:var(--acc);color:#fff;cursor:pointer}
.err{color:var(--err);font-size:14px;margin-top:10px}
.gbtn{display:block;text-align:center;padding:10px;border-radius:8px;background:var(--acc);color:#fff;text-decoration:none;font-weight:600}
.hint{margin:8px 0 12px;font-size:13px}
details{margin-top:8px}summary{cursor:pointer;font-size:14px;opacity:.8}
</style></head><body>
<form method="post" action="/login">
<h1>ATalk — {{PC}}</h1>
{{GOOGLE}}
<details{{OPEN}}><summary>1회용 코드로 로그인</summary>
<p>이 PC 에서 <code>atalk remote web</code> 으로 받은 코드를 넣으세요 (한 번만, 10분).</p>
<input name="code" value="{{CODE}}" autocomplete="one-time-code">
<button type="submit">로그인</button>
</details>
<div class="err">{{MSG}}</div>
</form></body></html>`

type remoteWebKey struct{}

// isRemoteWeb: 원격 포트로 로그인한 브라우저의 요청인지.
func isRemoteWeb(r *http.Request) bool {
	v, _ := r.Context().Value(remoteWebKey{}).(bool)
	return v
}
