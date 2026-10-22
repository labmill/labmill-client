//go:build linux || darwin

package launch

import "golang.org/x/sys/unix"

// freeSpace reports the bytes free for the user on the disk holding dir.
func freeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
