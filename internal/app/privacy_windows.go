//go:build windows

package app

import "syscall"

// hiddenAttr reports whether p carries the Windows hidden attribute.
func hiddenAttr(p string) bool {
	ptr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return false
	}
	a, err := syscall.GetFileAttributes(ptr)
	if err != nil {
		return false
	}
	return a&syscall.FILE_ATTRIBUTE_HIDDEN != 0
}
