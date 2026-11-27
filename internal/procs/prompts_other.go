//go:build !windows

package procs

// SystemPrompts 는 아직 Windows 에서만 찾는다.
func SystemPrompts() []Prompt { return nil }
