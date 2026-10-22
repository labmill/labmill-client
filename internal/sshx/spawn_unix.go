//go:build !windows

package sshx

import (
	"errors"
	"os"
	"syscall"
)

func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// nothingListening reports whether a dial error means no helper is there
// (socket missing or connection refused), as opposed to e.g. EACCES.
func nothingListening(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)
}
