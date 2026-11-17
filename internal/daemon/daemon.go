// Package daemon 은 ATalk 데몬을 자동으로 띄우고 멈추고, 로그인 시 자동 시작을 등록한다.
//
// 데몬은 데이터 폴더의 daemon.lock 을 잡은 프로세스 하나만 실행된다. 훅/CLI/플러그인이 동시에
// 띄우려 해도 두 번째 프로세스는 잠금을 못 잡고 바로 끝난다.
package daemon

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"atalk/internal/client"
)

// DefaultRoot 는 데이터 폴더 (ATALK_ROOT, 기본 ~/.atalk).
func DefaultRoot() string {
	if r := os.Getenv("ATALK_ROOT"); r != "" {
		return r
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".atalk")
}

// Lock 은 데이터 폴더의 daemon.lock 을 독점으로 잡는다. 이미 다른 데몬이 잡고 있으면 오류.
func Lock(root string) (release func(), err error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	release, err = TryLock(filepath.Join(root, "daemon.lock"))
	if err != nil {
		return nil, fmt.Errorf("이미 다른 데몬이 이 데이터 폴더를 쓰고 있음 (%s)", root)
	}
	return release, nil
}

// TryLock 은 파일 잠금을 기다리지 않고 잡는다. 프로세스가 죽으면 OS 가 잠금을 푼다.
func TryLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { _ = unlockFile(f); f.Close() }, nil
}

// local 은 데몬 주소가 이 PC(127.0.0.1/localhost)인지와 포트를 돌려준다.
func local(base string) (port string, ok bool) {
	u, err := url.Parse(base)
	if err != nil {
		return "", false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return "", false
	}
	port = u.Port()
	if port == "" {
		port = "80"
	}
	return port, true
}

// Ensure 는 데몬이 응답하지 않으면 백그라운드로 띄우고 응답할 때까지 기다린다.
// 데몬 주소가 원격이면 띄우지 않는다.
func Ensure(c *client.Client) error {
	if c.Health() == nil {
		return nil
	}
	port, ok := local(c.Base)
	if !ok {
		return fmt.Errorf("ATalk 데몬(%s)에 연결할 수 없음", c.Base)
	}
	if err := Start(port); err != nil {
		return err
	}
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(150 * time.Millisecond) {
		if c.Health() == nil {
			return nil
		}
	}
	return fmt.Errorf("데몬을 띄웠지만 응답이 없음 (로그: %s)", filepath.Join(DefaultRoot(), "daemon.log"))
}

// Start 는 `atalk server` 를 창 없이 분리된 프로세스로 띄운다. 출력은 <root>/daemon.log.
func Start(port string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	root := DefaultRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(root, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	args := []string{"server", "--root", root}
	if port != "" {
		args = append(args, "--port", port)
	}
	// 훅/도구 실행 환경이 Job 으로 묶여 있으면 부모가 끝날 때 같이 죽을 수 있어서 먼저 Job 에서 분리를 시도한다.
	for _, breakaway := range []bool{true, false} {
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = logf, logf
		detach(cmd, breakaway)
		if err = cmd.Start(); err == nil {
			return cmd.Process.Release()
		}
	}
	return err
}

// Stop 은 실행 중인 데몬을 끝낸다.
func Stop(c *client.Client) error {
	h, err := c.HealthInfo()
	if err != nil {
		return errors.New("실행 중인 데몬 없음")
	}
	p, err := os.FindProcess(h.Pid)
	if err != nil {
		return err
	}
	if err := p.Kill(); err != nil {
		return err
	}
	for i := 0; i < 40 && c.Health() == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}
