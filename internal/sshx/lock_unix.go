//go:build !windows

package sshx

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

var errLockBusy = errors.New("lock held by another process")

// lockFile takes an exclusive, non-blocking flock on path (created 0600) and
// returns a function that releases it. errLockBusy means another holder.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLockBusy
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// checkDirOwner refuses a directory owned by someone else.
func checkDirOwner(fi os.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("owned by uid %d, not by the current user", st.Uid)
	}
	return nil
}
