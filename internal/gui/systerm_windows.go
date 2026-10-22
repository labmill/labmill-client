//go:build windows

package gui

import (
	"os/exec"
	"syscall"
)

// createNewConsole is CREATE_NEW_CONSOLE: the program gets a console window
// of its own.
const createNewConsole = 0x00000010

// setNewConsole makes cmd start in a new console window.
func setNewConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewConsole}
}
