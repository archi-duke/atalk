package server

// 설정 화면용 관리 API. localOnly 가드 뒤에 있으므로 이 PC 의 브라우저(같은 출처)와 CLI 만 쓸 수 있다.
//
//	GET  /admin/info                 데몬 정보 (데이터 폴더, PID, 포트, 자동 시작, 폴더 위치 파일)
//	PUT  /admin/autostart {on}       로그인 시 자동 시작
//	GET  /admin/dirs?path=           폴더 탐색 (설정 화면의 폴더 고르기)
//	POST /admin/move-root {path, mode: move|switch}
//	                                 move: 지금 데이터를 빈 폴더로 복사 / switch: 이미 ATalk 데이터가 있는 폴더로 전환
//	                                 → 위치 파일 저장 후 데몬이 새 위치로 다시 시작

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"atalk/internal/core"
	"atalk/internal/daemon"
)

func (s *Server) adminRoutes(h func(string, handler)) {
	h("GET /admin/info", func(w http.ResponseWriter, r *http.Request) (any, error) {
		port := 0
		if s.ln != nil {
			port = portOf(s.ln.Addr().String())
		}
		size, files := dirSize(s.Core.Root)
		return map[string]any{
			"root": s.Core.Root, "pid": os.Getpid(), "port": port,
			"root_pointer": daemon.RootPointerFile(), "env_root": os.Getenv("ATALK_ROOT"),
			"autostart": daemon.Autostart(), "data_bytes": size, "data_files": files,
			"os": runtime.GOOS,
		}, nil
	})
	h("PUT /admin/autostart", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			On bool `json:"on"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		if err := daemon.SetAutostart(b.On); err != nil {
			return nil, &core.Error{Status: 400, Code: "autostart", Message: err.Error()}
		}
		return map[string]any{"autostart": daemon.Autostart()}, nil
	})
	h("GET /admin/dirs", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return listDirs(r.URL.Query().Get("path"))
	})
	h("POST /admin/move-root", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		root, err := s.moveRoot(b.Path, b.Mode)
		if err != nil {
			return nil, err
		}
		go func() { // 응답을 보낸 뒤 다시 시작
			time.Sleep(300 * time.Millisecond)
			s.Restart <- struct{}{}
		}()
		return map[string]any{"root": root, "restarting": true}, nil
	})
}

func portOf(addr string) int {
	var p int
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		fmt.Sscanf(addr[i+1:], "%d", &p)
	}
	return p
}

func dirSize(root string) (size int64, files int) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
				files++
			}
		}
		return nil
	})
	return
}

// hasAtalkData 는 폴더에 ATalk 데이터(agents/ 또는 rooms/)가 있는지 본다.
func hasAtalkData(dir string) bool {
	for _, sub := range []string{"agents", "rooms"} {
		if fi, err := os.Stat(filepath.Join(dir, sub)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

func listDirs(path string) (any, error) {
	if path == "" {
		if runtime.GOOS == "windows" { // 드라이브 목록
			var drives []string
			for c := 'A'; c <= 'Z'; c++ {
				d := string(c) + `:\`
				if _, err := os.Stat(d); err == nil {
					drives = append(drives, d)
				}
			}
			return map[string]any{"path": "", "parent": nil, "dirs": drives, "has_data": false, "drives": true}, nil
		}
		path = "/"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, &core.Error{Status: 400, Code: "bad_path", Message: err.Error()}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, &core.Error{Status: 400, Code: "bad_path", Message: "폴더를 열 수 없음: " + abs}
	}
	dirs := []string{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), "$") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	var parent any
	if p := filepath.Dir(abs); p != abs {
		parent = p
	} else if runtime.GOOS == "windows" {
		parent = "" // 드라이브 목록으로
	}
	return map[string]any{"path": abs, "parent": parent, "dirs": dirs, "has_data": hasAtalkData(abs), "empty": len(entries) == 0}, nil
}

// moveRoot 는 데이터 폴더를 target 으로 옮기거나(move) 전환한다(switch). 원래 폴더는 지우지 않는다.
func (s *Server) moveRoot(target, mode string) (string, error) {
	bad := func(code, msg string, a ...any) error {
		return &core.Error{Status: 400, Code: code, Message: fmt.Sprintf(msg, a...)}
	}
	if env := os.Getenv("ATALK_ROOT"); env != "" {
		return "", bad("env_root", "ATALK_ROOT 환경 변수(%s)가 설정돼 있어 데이터 폴더를 바꿀 수 없음. 환경 변수를 지운 뒤 다시 하세요", env)
	}
	if strings.TrimSpace(target) == "" || !filepath.IsAbs(target) {
		return "", bad("bad_path", "전체 경로를 입력하세요 (예: D:\\atalk-data)")
	}
	target = filepath.Clean(target)
	cur := filepath.Clean(s.Core.Root)
	if strings.EqualFold(target, cur) {
		return "", bad("same_root", "지금 쓰는 폴더와 같음")
	}
	within := func(a, b string) bool { // a 가 b 안에 있음
		rel, err := filepath.Rel(b, a)
		return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
	}
	if within(target, cur) || within(cur, target) {
		return "", bad("nested_root", "지금 데이터 폴더의 안쪽이나 바깥쪽(상위) 폴더는 쓸 수 없음")
	}
	switch mode {
	case "switch":
		if !hasAtalkData(target) {
			return "", bad("no_data", "그 폴더에는 ATalk 데이터(agents/, rooms/)가 없음. 데이터를 옮기려면 '이동'을 쓰세요")
		}
		_ = os.Remove(filepath.Join(target, "MOVED.txt")) // 다시 쓰는 폴더이므로 "백업" 안내는 지운다
	case "move":
		if entries, err := os.ReadDir(target); err == nil && len(entries) > 0 {
			if hasAtalkData(target) {
				return "", bad("has_data", "그 폴더에는 이미 ATalk 데이터가 있음. 그 데이터를 쓰려면 '전환'을 쓰세요")
			}
			return "", bad("not_empty", "옮길 폴더가 비어 있지 않음. 빈 폴더나 새 폴더 이름을 고르세요")
		}
		// 복사하는 동안 다른 요청이 파일을 쓰지 못하게 잠근다
		err := s.Core.Exclusive(func() error { return copyTree(cur, target) })
		if err != nil {
			_ = os.RemoveAll(target)
			return "", bad("copy_failed", "복사 실패 (원래 데이터는 그대로): %v", err)
		}
		note := fmt.Sprintf("이 폴더의 ATalk 데이터는 %s 에 %s 으로 복사되었습니다.\r\n지금 데몬은 새 폴더를 씁니다. 이 폴더는 백업이며, 필요 없으면 지워도 됩니다.\r\n",
			time.Now().Format("2006-01-02 15:04"), target)
		_ = os.WriteFile(filepath.Join(cur, "MOVED.txt"), []byte(note), 0o644)
	default:
		return "", bad("bad_mode", "mode 는 move 또는 switch")
	}
	if err := daemon.SetRootPointer(target); err != nil {
		return "", bad("pointer", "위치 파일을 저장하지 못함: %v", err)
	}
	return target, nil
}

// copyTree 는 src 의 파일을 dst 로 복사한다 (daemon.lock 제외).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if rel == "daemon.lock" {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, in); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}
