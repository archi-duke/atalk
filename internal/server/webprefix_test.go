package server

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// 게이트웨이(리버스 프록시) 뒤 경로: X-Forwarded-Prefix 가 있으면 로그인 화면·이동·쿠키 경로·화면 요청에 그 앞부분을 붙인다.
// 형식이 이상한 값은 무시한다.
func TestRemoteWebPrefix(t *testing.T) {
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
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path, prefix, body string, ck *http.Cookie) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		if prefix != "" {
			req.Header.Set("X-Forwarded-Prefix", prefix)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", base)
		}
		if ck != nil {
			req.AddCookie(ck)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	if resp, _ := do("GET", "/", "/atalk", "", nil); resp.Header.Get("Location") != "/atalk/login" {
		t.Fatalf("로그인 전 / → %q", resp.Header.Get("Location"))
	}
	if _, body := do("GET", "/login", "/atalk/", "", nil); !strings.Contains(body, `action="/atalk/login"`) {
		t.Fatalf("로그인 화면 form: %.300s", body)
	}
	for _, bad := range []string{`/a"b`, "javascript:x", "//evil", "/a b"} {
		if _, body := do("GET", "/login", bad, "", nil); !strings.Contains(body, `action="/login"`) {
			t.Fatalf("이상한 앞부분 %q 은 무시해야 함", bad)
		}
	}

	code, _, err := srv.NewWebCode()
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := do("POST", "/login", "/atalk", url.Values{"code": {code}}.Encode(), nil)
	if resp.Header.Get("Location") != "/atalk/" {
		t.Fatalf("로그인 뒤 이동: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == webCookie {
			ck = c
		}
	}
	if ck == nil || ck.Path != "/atalk" {
		t.Fatalf("쿠키 경로는 /atalk 이어야 함: %+v", ck)
	}
	if _, body := do("GET", "/", "/atalk", "", &http.Cookie{Name: webCookie, Value: ck.Value}); !strings.Contains(body, `window.ATALK_BASE="/atalk"`) {
		t.Fatal("화면에 ATALK_BASE 가 들어가야 함")
	}
	if _, body := do("GET", "/", "", "", &http.Cookie{Name: webCookie, Value: ck.Value}); strings.Contains(body, "window.ATALK_BASE=") {
		t.Fatal("게이트웨이 없이 열면 ATALK_BASE 가 없어야 함")
	}
}
