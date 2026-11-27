package server

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"atalk/internal/client"
)

// 원격 포트 웹 화면: 코드 없이는 로그인 화면, 코드로 로그인하면 로컬 화면 API 를 씀, 코드는 한 번만,
// 다른 출처 쓰기는 막음, 로그인한 브라우저는 새 코드를 못 받음, 로그아웃(모두 끊기) 뒤에는 다시 막힘.
func TestRemoteWebLogin(t *testing.T) {
	t.Setenv("ATALK_CONFIG_DIR", t.TempDir())
	srv, err := New(Options{Root: t.TempDir(), Addr: "127.0.0.1:0", MachineID: "host-pc", RemoteDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	port, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	lc := client.New(fmt.Sprintf("http://127.0.0.1:%d", port))
	if _, err := lc.NewWebCode(); err == nil {
		t.Fatal("원격 포트가 꺼져 있으면 코드 발급 불가")
	}
	rp, err := srv.StartRemote(0)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("https://127.0.0.1:%d", rp)
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := hc.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	login := func(code string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", base+"/login", strings.NewReader(url.Values{"code": {code}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", base)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 로그인 전: / 는 로그인 화면으로, 화면 API 는 막힘
	if code, body := get("/"); code != 200 || !strings.Contains(body, `action="/login"`) {
		t.Fatalf("로그인 전 / : %d %.80s", code, body)
	}
	if code, _ := get("/agents"); code != 401 && code != 404 {
		t.Fatalf("로그인 전 /agents 는 막혀야 함: %d", code)
	}
	if code, body := login("WRONGCODE"); code != 200 || !strings.Contains(body, "틀렸거나") {
		t.Fatalf("틀린 코드: %d %.200s", code, body)
	}

	// 브라우저가 출처를 null 로 보내도 (no-referrer) 같은 사이트 제출이면 로그인됨, 다른 사이트면 막음
	nullLogin := func(code, site string) string {
		t.Helper()
		req, _ := http.NewRequest("POST", base+"/login", strings.NewReader(url.Values{"code": {code}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "null")
		req.Header.Set("Sec-Fetch-Site", site)
		resp, err := (&http.Client{Transport: hc.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Status
	}
	nc, _ := lc.NewWebCode()
	if st := nullLogin(nc.Code, "cross-site"); !strings.HasPrefix(st, "200") {
		t.Fatalf("다른 사이트의 null 출처는 로그인 화면(오류)으로: %s", st)
	}
	nc, _ = lc.NewWebCode()
	if st := nullLogin(nc.Code, "same-origin"); !strings.HasPrefix(st, "303") {
		t.Fatalf("같은 사이트의 null 출처는 로그인돼야 함: %s", st)
	}
	lc.ClearWebLogins()

	pc, err := lc.NewWebCode()
	if err != nil {
		t.Fatal(err)
	}
	if code, body := login(strings.ToLower(pc.Code)); code != 200 || !strings.Contains(body, "ATALK") && !strings.Contains(body, "<html") {
		t.Fatalf("로그인 뒤 화면: %d", code)
	}
	if code, body := get("/agents"); code != 200 || !strings.Contains(body, `"agents"`) {
		t.Fatalf("로그인 뒤 /agents: %d %.120s", code, body)
	}
	// 코드는 한 번만 (쿠키 없는 새 브라우저로)
	hc2 := &http.Client{Transport: hc.Transport}
	req, _ := http.NewRequest("POST", base+"/login", strings.NewReader(url.Values{"code": {pc.Code}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := hc2.Do(req); err != nil {
		t.Fatal(err)
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), "틀렸거나") {
			t.Fatalf("같은 코드 두 번째 로그인은 실패해야 함: %.200s", b)
		}
	}

	// 다른 출처에서 온 쓰기는 막음
	req, _ = http.NewRequest("PUT", base+"/config", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example")
	if resp, err := hc.Do(req); err != nil {
		t.Fatal(err)
	} else if resp.Body.Close(); resp.StatusCode != 403 {
		t.Fatalf("다른 출처 쓰기: %d", resp.StatusCode)
	}
	// 로그인한 브라우저는 웹 코드를 새로 받지 못함
	req, _ = http.NewRequest("POST", base+"/admin/remote/web", strings.NewReader(`{}`))
	req.Header.Set("Origin", base)
	if resp, err := hc.Do(req); err != nil {
		t.Fatal(err)
	} else if resp.Body.Close(); resp.StatusCode != 403 {
		t.Fatalf("원격 웹에서 코드 발급: %d", resp.StatusCode)
	}

	// 데몬이 다시 떠도 로그인 유지 (파일에서 읽음)
	srv.remote.mu.Lock()
	srv.remote.web = nil
	srv.remote.mu.Unlock()
	if code, _ := get("/agents"); code != 200 {
		t.Fatalf("세션 파일에서 다시 읽기: %d", code)
	}

	if n, err := lc.ClearWebLogins(); err != nil || n != 1 {
		t.Fatalf("모두 끊기: %d %v", n, err)
	}
	if code, _ := get("/agents"); code == 200 {
		t.Fatal("끊은 뒤에는 막혀야 함")
	}
}
