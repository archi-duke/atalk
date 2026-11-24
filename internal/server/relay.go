package server

// 중계(relay): 이 PC 의 플러그인(opencode 등)이 에이전트 대신 데몬을 부를 때 쓰는 통로.
//
//	ANY /relay/{agent}/{경로...}
//
// 그 에이전트가 다른 PC 데몬에 연결돼 있으면(atalk connect → <설정 폴더>/remote/<agent>.json) 이 데몬이
// 인증서 지문 확인·토큰을 붙여 원격 데몬으로 전달하고, 아니면 이 데몬에서 그대로 처리한다.
// 그래서 플러그인은 HTTPS·토큰을 몰라도 되고, 에이전트가 연결하거나 끊으면 다음 요청부터 바로 따라간다.
// 내부 포트(localOnly) 에만 있다.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"atalk/internal/client"
	"atalk/internal/core"
)

type relayClients struct {
	mu sync.Mutex
	m  map[string]*client.Client // URL+토큰 → 원격 클라이언트 (TLS 연결 재사용)
}

func (rc *relayClients) get(conn *client.RemoteConn) *client.Client {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	k := conn.URL + "\n" + conn.Token + "\n" + conn.Fingerprint
	if c, ok := rc.m[k]; ok {
		return c
	}
	if rc.m == nil {
		rc.m = map[string]*client.Client{}
	}
	c := client.NewRemote(conn)
	c.HTTP.Timeout = 0 // wait 롱폴링·SSE 는 요청 쪽 context 로 끝난다
	rc.m[k] = c
	return c
}

func (s *Server) relay(local http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent, rest := r.PathValue("agent"), "/"+r.PathValue("rest")
		conn := client.LoadConn(agent)
		if conn == nil { // 이 PC 데몬에서 처리
			r2 := r.Clone(r.Context())
			r2.URL.Path, r2.URL.RawPath = rest, ""
			local.ServeHTTP(w, r2)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1_000_001))
		if err != nil || len(body) > 1_000_000 {
			writeErr(w, &core.Error{Status: 413, Code: "too_large", Message: "요청 본문이 너무 큼 (최대 1MB)"})
			return
		}
		if r.Method == http.MethodPost && rest == "/agents" && len(body) > 0 { // 등록: 경로·PID 대신 이 PC 정보·작업 위치 ID
			var in core.RegisterInput
			if json.Unmarshal(body, &in) == nil {
				body, _ = json.Marshal(conn.RemoteCard(in))
			}
		}
		rc := s.relayClients.get(conn)
		u := rc.Base + rest
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, u, bytes.NewReader(body))
		if err != nil {
			writeErr(w, err)
			return
		}
		if len(body) > 0 {
			req.Header.Set("content-type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+conn.Token)
		res, err := rc.HTTP.Do(req)
		if err != nil {
			msg := err.Error()
			if i := strings.Index(msg, "원격 데몬의 인증서"); i >= 0 {
				msg = msg[i:]
			}
			writeErr(w, &core.Error{Status: 502, Code: "remote_unreachable", Message: "원격 데몬(" + conn.URL + ")에 연결할 수 없음: " + msg})
			return
		}
		defer res.Body.Close()
		for _, h := range []string{"content-type", "cache-control"} {
			if v := res.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 32*1024)
		for { // SSE 도 바로바로 흘려보낸다
			n, err := res.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				if fl != nil {
					fl.Flush()
				}
			}
			if err != nil {
				return
			}
		}
	}
}
