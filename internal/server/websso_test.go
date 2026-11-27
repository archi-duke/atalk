package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 사내 SSO (ADFS/MobilAve 계약): /auth/sso → SSO(가짜) → 콜백에서 code→sessionToken→loginid → 허용 목록(sso:아이디)이면 로그인.
// state 가 다르면 거절, 등록 안 된 아이디·비활성 세션은 거절, 설정은 바깥 브라우저에서 못 바꿈, 게이트웨이 경로(/atalk) 아래에서도 됨.
func TestSSOLogin(t *testing.T) {
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

	// 가짜 SSO: code "c-<user>" → token "t-<user>" → loginid <user>. active 는 "true", 사용자 "off" 는 비활성
	sso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/Account/exchange":
			if !strings.HasPrefix(q.Get("code"), "c-") {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sessionToken": "t-" + strings.TrimPrefix(q.Get("code"), "c-")})
		case "/Account/introspect":
			u := strings.TrimPrefix(q.Get("token"), "t-")
			active := "true"
			if u == "off" {
				active = "false"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"loginid": u, "active": active})
		default:
			w.WriteHeader(404)
		}
	}))
	defer sso.Close()

	if err := srv.SetSSO(SSOConfig{URL: "ftp://x", Base: base}); err == nil {
		t.Fatal("http(s) 가 아닌 SSO 주소는 거절")
	}
	if err := srv.SetSSO(SSOConfig{URL: sso.URL, Base: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.SetWebUser("sso:Duke.Kimm", true); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.SetWebUser("sso:", true); err == nil {
		t.Fatal("빈 SSO 아이디는 거절")
	}

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	newClient := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	do := func(hc *http.Client, u, prefix string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest("GET", u, nil)
		// 게이트웨이 흉내: 브라우저가 보는 주소는 <prefix>/… (쿠키 경로 /atalk 도 그 주소 기준), 데몬에는 앞부분을 뗀 경로로 감
		seen, _ := url.Parse(strings.Replace(u, base, base+prefix, 1))
		if prefix != "" {
			req.Header.Set("X-Forwarded-Prefix", prefix)
			for _, c := range hc.Jar.Cookies(seen) {
				req.AddCookie(c)
			}
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if prefix != "" {
			hc.Jar.SetCookies(seen, resp.Cookies())
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	if _, body := do(newClient(), base+"/login", ""); !strings.Contains(body, `href="/auth/sso"`) || !strings.Contains(body, "사내 SSO로 로그인") {
		t.Fatalf("로그인 화면에 SSO 버튼: %.400s", body)
	}

	// login: /auth/sso → SSO 로 가는 주소 확인 → (SSO 대신) 콜백을 code 와 함께 부름
	login := func(hc *http.Client, user, prefix, stateOverride string) string {
		t.Helper()
		resp, _ := do(hc, base+"/auth/sso", prefix)
		loc, _ := url.Parse(resp.Header.Get("Location"))
		back, _ := url.Parse(loc.Query().Get("client"))
		if loc.Path != "/Account/ADFSLogin" || back == nil || back.Path != prefix+ssoCallback {
			t.Fatalf("SSO 로 가는 주소: %s", loc)
		}
		state := back.Query().Get("state")
		if stateOverride != "" {
			state = stateOverride
		}
		cb := base + ssoCallback + "?state=" + url.QueryEscape(state) + "&auth=1&code=c-" + user
		_, body := do(hc, cb, prefix)
		return body
	}

	hc := newClient()
	if body := login(hc, "duke.kimm", "", ""); !strings.Contains(body, "location.replace") {
		t.Fatalf("SSO 로그인 성공 화면: %.300s", body)
	}
	if resp, _ := do(hc, base+"/agents", ""); resp.StatusCode != 200 {
		t.Fatalf("SSO 로그인 뒤 /agents: %d", resp.StatusCode)
	}
	if body := login(newClient(), "duke.kimm", "", "WRONG"); !strings.Contains(body, "맞지 않습니다") {
		t.Fatalf("state 불일치: %.300s", body)
	}
	hc2 := newClient()
	if body := login(hc2, "batman", "", ""); !strings.Contains(body, "등록되지 않은 사용자") {
		t.Fatalf("등록 안 된 아이디: %.300s", body)
	}
	if resp, _ := do(hc2, base+"/agents", ""); resp.StatusCode == 200 {
		t.Fatal("등록 안 된 아이디는 막혀야 함")
	}
	if body := login(newClient(), "off", "", ""); !strings.Contains(body, "유효하지 않습니다") {
		t.Fatalf("비활성 세션: %.300s", body)
	}
	// 구글 이메일 허용 항목과 섞이지 않음: 허용 목록의 이메일을 SSO 아이디로 쓰면 거절
	if _, err := srv.SetWebUser("me@example.com", true); err != nil {
		t.Fatal(err)
	}
	if body := login(newClient(), "me@example.com", "", ""); !strings.Contains(body, "등록되지 않은 사용자") {
		t.Fatalf("구글 이메일 항목으로 SSO 로그인은 안 됨: %.300s", body)
	}

	// 게이트웨이를 거친 접속은 기본으로 SSO 를 막음: 버튼 없음, 콜백을 직접 불러도 (아무 code 나 받는 목이어도) 로그인 안 됨
	if err := srv.SetSSO(SSOConfig{URL: sso.URL, Base: base + "/atalk"}); err != nil {
		t.Fatal(err)
	}
	if _, body := do(newClient(), base+"/login", "/atalk"); strings.Contains(body, "/auth/sso") {
		t.Fatal("게이트웨이 경유 로그인 화면에는 SSO 버튼이 없어야 함")
	}
	hcG := newClient()
	if resp, _ := do(hcG, base+"/auth/sso", "/atalk"); strings.Contains(resp.Header.Get("Location"), "ADFSLogin") {
		t.Fatal("게이트웨이 경유 /auth/sso 는 SSO 로 보내지 않아야 함")
	}
	if _, body := do(hcG, base+ssoCallback+"?state=x&code=c-duke.kimm", "/atalk"); !strings.Contains(body, "쓸 수 없습니다") {
		t.Fatalf("게이트웨이 경유 콜백: %.300s", body)
	}
	if resp, _ := do(hcG, base+"/agents", "/atalk"); resp.StatusCode == 200 {
		t.Fatal("게이트웨이 경유 SSO 로 들어오면 안 됨")
	}

	// --gateway 로 허용하면 게이트웨이 경로 아래 (/atalk) 에서도: 돌아올 주소에 /atalk 이 들어가고 로그인됨
	if err := srv.SetSSO(SSOConfig{URL: sso.URL, Base: base + "/atalk", Name: "MobilAve SSO", Gateway: true}); err != nil {
		t.Fatal(err)
	}
	hc3 := newClient()
	resp, _ := do(hc3, base+"/auth/sso", "/atalk")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if back := loc.Query().Get("client"); !strings.HasPrefix(back, base+"/atalk"+ssoCallback+"?state=") {
		t.Fatalf("돌아올 주소: %q", back)
	}
	if body := login(hc3, "duke.kimm", "/atalk", ""); !strings.Contains(body, `location.replace("/atalk/")`) {
		t.Fatalf("/atalk 아래 SSO 로그인: %.300s", body)
	}
	if _, body := do(newClient(), base+"/login", "/atalk"); !strings.Contains(body, "MobilAve SSO로 로그인") {
		t.Fatal("버튼 이름 설정")
	}

	// 바깥 브라우저(로그인한 쿠키)로는 SSO 설정을 못 바꿈
	req, _ := http.NewRequest("PUT", base+"/admin/remote/sso", strings.NewReader(`{"url":"http://evil","base":"`+base+`"}`))
	req.Header.Set("Origin", base)
	if resp, err := hc.Do(req); err != nil {
		t.Fatal(err)
	} else if resp.Body.Close(); resp.StatusCode != 403 {
		t.Fatalf("바깥에서 SSO 설정: %d", resp.StatusCode)
	}
	if c, _ := srv.ssoConfig(); c.URL != sso.URL {
		t.Fatalf("SSO 설정이 바뀜: %s", c.URL)
	}
}
