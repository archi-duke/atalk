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
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"atalk/internal/core"
	"atalk/internal/daemon"
	"atalk/internal/install"
	"atalk/internal/paths"
)

func (s *Server) adminRoutes(h func(string, handler)) {
	h("GET /admin/info", func(w http.ResponseWriter, r *http.Request) (any, error) {
		port := 0
		if s.ln != nil {
			port = portOf(s.ln.Addr().String())
		}
		size, files := dirSize(s.Core.Root)
		tc, tb := s.Core.TrashInfo()
		m := s.Core.Machine()
		return map[string]any{
			"root": s.Core.Root, "pid": os.Getpid(), "port": port,
			"root_pointer": daemon.RootPointerFile(), "env_root": os.Getenv("ATALK_ROOT"), "env_port": os.Getenv("ATALK_PORT"),
			"config_dir": paths.ConfigDir(), "autostart": daemon.Autostart(), "data_bytes": size, "data_files": files,
			"trash_count": tc, "trash_bytes": tb, "os": runtime.GOOS,
			"machine": map[string]any{"name": m.Name, "os": m.OS, "default_name": core.DefaultMachineName(m.ID)},
		}, nil
	})
	h("POST /admin/trash/empty", func(w http.ResponseWriter, r *http.Request) (any, error) {
		n, err := s.Core.EmptyTrash(0)
		return map[string]any{"removed": n}, err
	})
	h("GET /admin/backup", func(w http.ResponseWriter, r *http.Request) (any, error) {
		name := "atalk-backup-" + time.Now().Format("20060102-1504") + ".zip"
		w.Header().Set("content-type", "application/zip")
		w.Header().Set("content-disposition", `attachment; filename="`+name+`"`)
		tmp, err := os.MkdirTemp("", "atalk-backup-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		dbCopy := filepath.Join(tmp, "atalk.db")
		if err := s.Core.BackupDB(dbCopy); err != nil {
			return nil, &core.Error{Status: 500, Code: "backup_failed", Message: err.Error()}
		}
		if err := zipTree(s.Core.Root, dbCopy, w); err != nil {
			log.Printf("backup: %v", err)
		}
		return skip{}, nil
	})
	h("PUT /admin/port", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Port int `json:"port"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		if os.Getenv("ATALK_PORT") != "" || os.Getenv("ATALK_URL") != "" {
			return nil, &core.Error{Status: 400, Code: "env_port", Message: "ATALK_PORT/ATALK_URL 환경 변수가 있어 포트를 바꿀 수 없음"}
		}
		if b.Port < 1024 || b.Port > 65535 {
			return nil, &core.Error{Status: 400, Code: "bad_port", Message: "포트는 1024 ~ 65535"}
		}
		if s.ln != nil && portOf(s.ln.Addr().String()) == b.Port {
			return nil, &core.Error{Status: 400, Code: "same_port", Message: "지금 쓰는 포트와 같음"}
		}
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", b.Port)); err != nil {
			return nil, &core.Error{Status: 400, Code: "port_busy", Message: fmt.Sprintf("포트 %d 를 다른 프로그램이 쓰고 있음", b.Port)}
		} else {
			ln.Close()
		}
		if err := paths.SetPort(b.Port); err != nil {
			return nil, err
		}
		s.RestartPort = b.Port
		go func() {
			time.Sleep(300 * time.Millisecond)
			s.Restart <- struct{}{}
		}()
		return map[string]any{"port": b.Port, "url": fmt.Sprintf("http://127.0.0.1:%d/", b.Port), "restarting": true}, nil
	})
	h("GET /admin/integrations", func(w http.ResponseWriter, r *http.Request) (any, error) {
		return map[string]any{"status": install.Check(install.DefaultPaths())}, nil
	})
	h("POST /admin/integrations/install", func(w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Target string `json:"target"`
		}
		if err := readBody(r, &b); err != nil {
			return nil, err
		}
		p := install.DefaultPaths()
		var done []string
		var err error
		switch b.Target {
		case "claude":
			done, err = install.Claude(p)
		case "opencode":
			done, err = install.Opencode(p)
		case "path":
			done, err = install.Path()
		default:
			return nil, &core.Error{Status: 400, Code: "bad_target", Message: "target 은 claude | opencode | path"}
		}
		if err != nil {
			return nil, &core.Error{Status: 400, Code: "install_failed", Message: err.Error()}
		}
		return map[string]any{"done": done, "status": install.Check(p)}, nil
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

// hasAtalkData 는 폴더에 ATalk 데이터(atalk.db, 또는 예전 형식의 agents/ rooms/)가 있는지 본다.
func hasAtalkData(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "atalk.db")); err == nil {
		return true
	}
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
		// DB 는 일관된 복사본으로, 나머지 파일(legacy 백업 등)은 그대로 복사
		err := copyTree(cur, target)
		if err == nil {
			err = s.Core.BackupDB(filepath.Join(target, "atalk.db"))
		}
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

// skipCopy 는 복사·백업에서 뺄 파일 (DB 는 따로 일관된 복사본을 만든다).
func skipCopy(rel string) bool {
	switch rel {
	case "daemon.lock", "daemon.log", "atalk.db", "atalk.db-wal", "atalk.db-shm":
		return true
	}
	return false
}

// zipTree 는 데이터 폴더를 zip 으로 쓴다. DB 는 dbCopy(일관된 복사본)를 atalk.db 로 넣는다.
func zipTree(root, dbCopy string, w io.Writer) error {
	zw := zip.NewWriter(w)
	add := func(p, name string) error {
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		fi, _ := in.Stat()
		hdr, _ := zip.FileInfoHeader(fi)
		hdr.Name, hdr.Method = name, zip.Deflate
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return err
	}
	if err := add(dbCopy, "atalk.db"); err != nil {
		return err
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if skipCopy(filepath.ToSlash(rel)) {
			return nil
		}
		return add(p, filepath.ToSlash(rel))
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

// copyTree 는 src 의 파일을 dst 로 복사한다 (DB·잠금·로그 제외, DB 는 BackupDB 로).
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
		if skipCopy(filepath.ToSlash(rel)) {
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
