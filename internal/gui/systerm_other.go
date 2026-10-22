//go:build !windows

package gui

import "os/exec"

// setNewConsole does nothing outside Windows (only the Windows fallback uses it).
func setNewConsole(*exec.Cmd) {}
