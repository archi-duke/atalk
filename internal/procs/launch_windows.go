//go:build windows

package procs

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

// psScript 는 argv 를 실행하는 PowerShell 스크립트. 인자는 작은따옴표로 감싸 공백·특수 문자가 있어도 그대로 전달하고,
// 정상 종료하면 창이 닫히며 오류로 끝나면 내용을 볼 수 있게 Enter 를 기다린다 (꺼진 세션의 빈 창이 남지 않게).
func psScript(argv []string, env map[string]string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
	}
	pre := ""
	for k, v := range env { // wt 는 데몬의 환경 변수를 넘겨주지 않으므로 스크립트 안에서 정한다
		pre += "$env:" + k + " = '" + strings.ReplaceAll(v, "'", "''") + "'\n"
	}
	return pre + "& " + strings.Join(q, " ") + "\nif ($LASTEXITCODE) { Read-Host \"종료 코드 $LASTEXITCODE - Enter 를 누르면 창을 닫습니다\" }"
}

// encodePS 는 -EncodedCommand 값 (UTF-16LE base64). wt·cmd 를 거쳐도 따옴표가 풀리거나 ; 로 나뉘지 않는다.
func encodePS(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[2*i], b[2*i+1] = byte(v), byte(v>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// LaunchTerminal 은 cwd 에서 새 터미널 창을 열어 argv 를 실행한다.
// Windows Terminal(wt)이 있으면 새 창으로, 없으면 새 콘솔 창의 PowerShell 로.
func LaunchTerminal(cwd string, argv []string) error { return LaunchTerminalEnv(cwd, argv, nil) }

// LaunchTerminalEnv 는 LaunchTerminal 에 더해 새 창의 프로세스에 환경 변수를 준다 (atalk spawn 의 연결 표식).
func LaunchTerminalEnv(cwd string, argv []string, env map[string]string) error {
	ps := []string{"powershell.exe", "-NoLogo", "-EncodedCommand", encodePS(psScript(argv, env))}
	var cmd *exec.Cmd
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		cmd = exec.Command(wt, append([]string{"-w", "new", "-d", cwd}, ps...)...)
	} else {
		cmd = exec.Command("cmd.exe", "/c", "start", "ATalk", "/D", cwd, ps[0], strings.Join(ps[1:], " "))
	}
	cmd.Dir = cwd
	// 데몬은 창 없이 돌므로, 새 창이 콘솔을 따로 갖게 한다
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010 /* CREATE_NEW_CONSOLE */}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
