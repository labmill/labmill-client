//go:build windows

package sshx

import "syscall"

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

// nothingListening reports whether a dial error means no helper is there.
// Windows reports this with several WSA/file errors; when unsure, spawn.
func nothingListening(err error) bool { return true }
