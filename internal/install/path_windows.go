package install

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// 사용자 PATH (HKCU\Environment\Path). setx 는 1024자에서 잘리므로 레지스트리를 직접 고친다.
const envKey = `Environment`

func readUserPath() (string, uint32, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE)
	if err != nil {
		return "", registry.EXPAND_SZ, err
	}
	defer k.Close()
	v, typ, err := k.GetStringValue("Path")
	if err == registry.ErrNotExist {
		return "", registry.EXPAND_SZ, nil
	}
	return v, typ, err
}

func writeUserPath(v string, typ uint32) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if typ == registry.SZ {
		err = k.SetStringValue("Path", v)
	} else {
		err = k.SetExpandStringValue("Path", v)
	}
	if err == nil {
		broadcastEnvChange()
	}
	return err
}

func samePathEntry(a, b string) bool {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if exp, err := registry.ExpandString(s); err == nil {
			s = exp
		}
		return strings.ToLower(filepath.Clean(s))
	}
	return clean(a) == clean(b)
}

// PathDir 는 PATH 에 넣을 폴더 (atalk 실행 파일이 있는 곳).
func PathDir() string { return filepath.Dir(filepath.FromSlash(Exe())) }

// InUserPath 는 사용자 PATH 에 dir 이 있는지 본다.
func InUserPath(dir string) bool {
	v, _, err := readUserPath()
	if err != nil {
		return false
	}
	for _, e := range strings.Split(v, ";") {
		if samePathEntry(e, dir) {
			return true
		}
	}
	return false
}

// AddToUserPath 는 사용자 PATH 끝에 dir 을 추가한다. 이미 있으면 false.
func AddToUserPath(dir string) (bool, error) {
	v, typ, err := readUserPath()
	if err != nil {
		return false, err
	}
	for _, e := range strings.Split(v, ";") {
		if samePathEntry(e, dir) {
			return false, nil
		}
	}
	v = strings.TrimRight(v, ";")
	if v != "" {
		v += ";"
	}
	return true, writeUserPath(v+dir, typ)
}

// RemoveFromUserPath 는 사용자 PATH 에서 dir 을 뺀다. 없으면 false.
func RemoveFromUserPath(dir string) (bool, error) {
	v, typ, err := readUserPath()
	if err != nil {
		return false, err
	}
	var kept []string
	removed := false
	for _, e := range strings.Split(v, ";") {
		if samePathEntry(e, dir) {
			removed = true
			continue
		}
		if e != "" {
			kept = append(kept, e)
		}
	}
	if !removed {
		return false, nil
	}
	return true, writeUserPath(strings.Join(kept, ";"), typ)
}

// broadcastEnvChange 는 탐색기 등에 환경 변수가 바뀌었다고 알린다 (새로 여는 터미널부터 적용).
func broadcastEnvChange() {
	user32 := windows.NewLazySystemDLL("user32.dll")
	send := user32.NewProc("SendMessageTimeoutW")
	env, _ := windows.UTF16PtrFromString("Environment")
	const hwndBroadcast, wmSettingChange, smtoAbortIfHung = 0xffff, 0x001A, 0x0002
	var result uintptr
	_, _, _ = send.Call(hwndBroadcast, wmSettingChange, 0, uintptr(unsafe.Pointer(env)), smtoAbortIfHung, 5000, uintptr(unsafe.Pointer(&result)))
}
