//go:build !windows

package procs

import "errors"

// FocusWindow 는 아직 Windows 에서만 된다.
func FocusWindow(pid int) error { return errors.New("창 앞으로 가져오기는 Windows 에서만 지원") }

// FindWindow 는 아직 Windows 에서만 된다.
func FindWindow(pid int) (string, string, error) { return "", "", errors.New("Windows 에서만 지원") }
