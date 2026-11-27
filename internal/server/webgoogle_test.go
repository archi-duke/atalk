package server

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 구글 로그인: /auth/google → 구글(가짜) → 콜백에서 id_token 확인 → 허용 목록이면 로그인, 아니면 거절.
// state 가 다르면 거절, 설정한 도메인이 아닌 주소로 오면 그 도메인으로 옮김, 설정·허용 목록은 바깥 브라우저에서 못 바꿈.
func TestGoogleLogin(t *testing.T) {
	t.Setenv("ATALK_CONFIG_DIR", t.TempDir())
	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0", MachineID: "host-pc", RemoteDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	rp, err := srv.StartRemote(0)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("https://127.0.0.1:%d", rp)

	// 가짜 구글 토큰 엔드포인트: 받은 코드에 따라 이메일을 정한다
	var nonce string
	email := "Me@Example.com"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("client_secret") != "sec" || r.PostForm.Get("redirect_uri") != base+"/auth/google/callback" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		claims, _ := json.Marshal(map[string]any{"iss": "https://accounts.google.com", "aud": "cid", "exp": time.Now().Add(time.Hour).Unix(),
			"email": email, "email_verified": true, "nonce": nonce})
		tok := "x." + base64.RawURLEncoding.EncodeToString(claims) + ".y"
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": tok})
	}))
	defer fake.Close()
	oldAuth, oldTok := googleAuthURL, googleTokenURL
	googleAuthURL, googleTokenURL = "https://google.invalid/auth", fake.URL
	defer func() { googleAuthURL, googleTokenURL = oldAuth, oldTok }()

	if err := srv.SetGoogle(GoogleConfig{ClientID: "cid", ClientSecret: "sec", Base: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.SetWebUser("me@example.com", true); err != nil {
		t.Fatal(err)
	}

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	do := func(hc *http.Client, u string) (*http.Response, string) {
		t.Helper()
		resp, err := hc.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	// 로그인 화면에 구글 버튼
	if _, body := do(newClient(), base+"/login"); !strings.Contains(body, `href="/auth/google"`) {
		t.Fatal("구글 버튼이 있어야 함")
	}
	// 설정한 주소가 아닌 곳(localhost)으로 오면 그 주소로 옮김
	if resp, _ := do(newClient(), strings.Replace(base, "127.0.0.1", "localhost", 1)+"/auth/google"); resp.Header.Get("Location") != base+"/auth/google" {
		t.Fatalf("다른 주소 → 설정 주소로: %q", resp.Header.Get("Location"))
	}

	login := func(hc *http.Client, stateOverride string) string {
		t.Helper()
		resp, _ := do(hc, base+"/auth/google")
		loc, _ := url.Parse(resp.Header.Get("Location"))
		q := loc.Query()
		if !strings.HasPrefix(loc.String(), googleAuthURL) || q.Get("client_id") != "cid" || q.Get("redirect_uri") != base+"/auth/google/callback" {
			t.Fatalf("구글로 보내는 주소: %s", loc)
		}
		nonce = q.Get("nonce")
		state := q.Get("state")
		if stateOverride != "" {
			state = stateOverride
		}
		_, body := do(hc, base+"/auth/google/callback?code=abc&state="+url.QueryEscape(state))
		return body
	}

	hc := newClient()
	if body := login(hc, ""); !strings.Contains(body, "location.replace") {
		t.Fatalf("로그인 성공 화면: %.200s", body)
	}
	if resp, _ := do(hc, base+"/agents"); resp.StatusCode != 200 {
		t.Fatalf("로그인 뒤 /agents: %d", resp.StatusCode)
	}

	// state 가 다르면 거절
	if body := login(newClient(), "WRONG"); !strings.Contains(body, "맞지 않습니다") {
		t.Fatalf("state 불일치: %.300s", body)
	}
	// 허용 목록에 없는 계정은 거절
	email = "other@example.com"
	hc2 := newClient()
	if body := login(hc2, ""); !strings.Contains(body, "등록되지 않은 계정") {
		t.Fatalf("등록 안 된 계정: %.300s", body)
	}
	if resp, _ := do(hc2, base+"/agents"); resp.StatusCode == 200 {
		t.Fatal("등록 안 된 계정은 막혀야 함")
	}

	// 바깥 브라우저(로그인한 쿠키)로는 허용 목록·설정을 못 바꿈
	req, _ := http.NewRequest("POST", base+"/admin/remote/web/users", strings.NewReader(`{"email":"evil@example.com"}`))
	req.Header.Set("Origin", base)
	if resp, err := hc.Do(req); err != nil {
		t.Fatal(err)
	} else if resp.Body.Close(); resp.StatusCode != 403 {
		t.Fatalf("바깥에서 허용 목록 추가: %d", resp.StatusCode)
	}
	if u := srv.WebUsers(); len(u) != 1 || u[0] != "me@example.com" {
		t.Fatalf("허용 목록: %v", u)
	}
}
