//go:build desktop && windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows       = user32.NewProc("EnumWindows")
	procGetWindowThreadPI = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible   = user32.NewProc("IsWindowVisible")
	procGetForeground     = user32.NewProc("GetForegroundWindow")
	procGetWindowRect     = user32.NewProc("GetWindowRect")
	procIsIconic          = user32.NewProc("IsIconic")
	procGetWindowLongW    = user32.NewProc("GetWindowLongW")
	procGetWindowTextW    = user32.NewProc("GetWindowTextW")
)

// windowProbe inspects the top-level windows this process owns and reports
// whether any is visible on screen, is the foreground window or has a
// taskbar button (visible, not a tool window). The self-test runs hidden, so
// the owner's desktop is never disturbed; this is how it proves that.
// found reports whether the app's main window (title "labmill") was among
// them, so a probe that looked nowhere can't pass the hidden check.
func windowProbe() (summary string, shown, found bool) {
	pid := uint32(windows.GetCurrentProcessId())
	fg, _, _ := procGetForeground.Call()
	var lines []string
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		var wpid uint32
		procGetWindowThreadPI.Call(h, uintptr(unsafe.Pointer(&wpid)))
		if wpid != pid {
			return 1
		}
		vis, _, _ := procIsWindowVisible.Call(h)
		iconic, _, _ := procIsIconic.Call(h)
		ex, _, _ := procGetWindowLongW.Call(h, uintptr(0xFFFFFFEC)) // GWL_EXSTYLE (-20)
		var rc [4]int32
		procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&rc)))
		var title [64]uint16
		procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&title[0])), uintptr(len(title)))
		isFg := h == fg
		if windows.UTF16ToString(title[:]) == mainTitle && !isSplashWindow(h) {
			found = true
		}
		tool := ex&0x80 != 0 // WS_EX_TOOLWINDOW
		if vis != 0 || isFg {
			shown = true
		}
		lines = append(lines, fmt.Sprintf("hwnd=%#x title=%q visible=%v foreground=%v minimized=%v toolwindow=%v rect=%v", h, windows.UTF16ToString(title[:]), vis != 0, isFg, iconic != 0, tool, rc))
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return fmt.Sprintf("%d top-level window(s) of this process: %v", len(lines), lines), shown, found
}
