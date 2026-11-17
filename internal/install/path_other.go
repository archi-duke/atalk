//go:build !windows

package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func PathDir() string { return filepath.Dir(Exe()) }

func InUserPath(dir string) bool {
	for _, e := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(e) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

var errManual = errors.New("PATH 등록은 Windows 만 자동 지원함 — 셸 설정(~/.bashrc, ~/.zshrc)에 export PATH=\"$PATH:" + strings.ReplaceAll(PathDir(), `"`, `\"`) + "\" 를 추가하세요")

func AddToUserPath(dir string) (bool, error) {
	if InUserPath(dir) {
		return false, nil
	}
	return false, errManual
}

func RemoveFromUserPath(dir string) (bool, error) { return false, nil }
