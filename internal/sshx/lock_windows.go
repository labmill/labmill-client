//go:build windows

package sshx

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

var errLockBusy = errors.New("lock held by another process")

// lockFile takes an exclusive, non-blocking lock on path (created) and
// returns a function that releases it. errLockBusy means another holder.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockBusy
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// checkDirOwner is a no-op on Windows (directory ACLs apply instead).
func checkDirOwner(os.FileInfo) error { return nil }
