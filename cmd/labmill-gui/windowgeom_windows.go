//go:build desktop && windows

package main

import (
	"context"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"labmill-client/internal/gui"
)

var (
	procEnumDisplayMonitors = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procGetDpiForSystem     = user32.NewProc("GetDpiForSystem")
)

// monitorInfo is MONITORINFO: the monitor and work rectangles are
// left, top, right, bottom in desktop pixels.
type monitorInfo struct {
	Size    uint32
	Monitor [4]int32
	Work    [4]int32
	Flags   uint32
}

// monitorWork collects the work areas during EnumDisplayMonitors. Windows
// calls the callback synchronously, and only windowScreens (one caller at a
// time) uses it. The callback is created once: syscall.NewCallback's slots
// are never freed.
var (
	monitorWork []gui.Screen
	monitorCB   = syscall.NewCallback(func(hMon, _, _, _ uintptr) uintptr {
		mi := monitorInfo{Size: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); r != 0 {
			w := mi.Work
			monitorWork = append(monitorWork, gui.Screen{X: int(w[0]), Y: int(w[1]), Width: int(w[2] - w[0]), Height: int(w[3] - w[1])})
		}
		return 1
	})
)

// windowScreens is the work area of every monitor in physical desktop
// pixels (monitors left of the primary one have negative X). Empty if the
// OS call failed; the caller then opens centered.
func windowScreens(context.Context) []gui.Screen {
	monitorWork = nil
	procEnumDisplayMonitors.Call(0, 0, monitorCB, 0)
	out := monitorWork
	monitorWork = nil
	return out
}

// windowScale is physical pixels per logical pixel: Wails sizes are
// logical (96 DPI), positions and monitor work areas are physical.
func windowScale() float64 {
	if procGetDpiForSystem.Find() != nil {
		return 1 // before Windows 10 1607
	}
	if dpi, _, _ := procGetDpiForSystem.Call(); dpi >= 96 {
		return float64(dpi) / 96
	}
	return 1
}

// mainHwnd is the app's main window (title "labmill", this process), set by
// mainHwndCB; Windows calls the callback synchronously.
var (
	mainHwnd   uintptr
	mainHwndCB = syscall.NewCallback(func(h, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadPI.Call(h, uintptr(unsafe.Pointer(&pid)))
		if pid != windows.GetCurrentProcessId() {
			return 1
		}
		var title [16]uint16
		procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&title[0])), uintptr(len(title)))
		if windows.UTF16ToString(title[:]) == mainTitle && !isSplashWindow(h) {
			mainHwnd = h
			return 0
		}
		return 1
	})
)

// windowSetPos moves the window to desktop position (x, y) with SetWindowPos
// (no size change, no activation, so a window hidden in the tray stays
// hidden). Wails' own WindowSetPosition adds the work-area corner of the
// monitor the window is on, which doesn't match desktop coordinates.
func windowSetPos(_ context.Context, x, y int) {
	mainHwnd = 0
	procEnumWindows.Call(mainHwndCB, 0)
	if mainHwnd == 0 {
		return
	}
	const swpNoSize, swpNoZOrder, swpNoActivate = 0x1, 0x4, 0x10
	procSetWindowPos.Call(mainHwnd, 0, uintptr(int32(x)), uintptr(int32(y)), 0, 0, swpNoSize|swpNoZOrder|swpNoActivate)
}
