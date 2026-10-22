//go:build darwin

package app

import "syscall"

// ufHidden is the BSD UF_HIDDEN file flag (Finder's "hidden" bit).
const ufHidden = 0x8000

// hiddenAttr reports whether p carries the macOS hidden flag; a leading "."
// is checked separately.
func hiddenAttr(p string) bool {
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return false
	}
	return st.Flags&ufHidden != 0
}
