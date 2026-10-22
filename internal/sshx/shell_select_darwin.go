//go:build darwin

package sshx

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable reports whether f has input within d. It uses select(2):
// poll(2) is unreliable on terminals on macOS.
func waitReadable(f *os.File, d time.Duration) (bool, error) {
	fd := int(f.Fd())
	if fd < 0 || fd >= 1024 {
		return false, errors.New("terminal descriptor out of range")
	}
	var set unix.FdSet
	set.Bits[fd/32] |= int32(1) << (uint(fd) % 32)
	tv := unix.NsecToTimeval(d.Nanoseconds())
	n, err := unix.Select(fd+1, &set, nil, nil, &tv)
	if err == unix.EINTR {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
