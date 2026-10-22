//go:build !windows && !darwin

package sshx

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable reports whether f has input within d.
func waitReadable(f *os.File, d time.Duration) (bool, error) {
	fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, int(d.Milliseconds()))
	if err == unix.EINTR {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
