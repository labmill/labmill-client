//go:build windows

package app

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// resolveLinks resolves symlinks with filepath.EvalSymlinks, then asks
// Windows for the final path of the result, which also resolves junctions
// (see evalLinks). It fails closed: if the path exists but its final path
// can't be had (handle refused, name too odd), the error is returned, so the
// callers refuse the path instead of trusting an unresolved junction
// (resolveLoose treats an error on an existing path as a refusal, not as
// "doesn't exist"). Hard links to files are not detected here or elsewhere
// (a file has no single real path).
func resolveLinks(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	final, err := finalPath(real)
	if err != nil {
		return "", err
	}
	return filepath.Clean(final), nil
}

// finalPath returns the path GetFinalPathNameByHandle reports for p, without
// the \\?\ prefix. FILE_FLAG_BACKUP_SEMANTICS lets it open directories; the
// open follows junctions and symlinks.
func finalPath(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) > len(buf) {
			buf = make([]uint16, n)
			continue
		}
		return stripExtendedPrefix(windows.UTF16ToString(buf[:n])), nil
	}
}
