//go:build windows

package gui

import (
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// platformNotify shows a tray balloon (Shell_NotifyIcon with NIF_INFO) from
// a short-lived icon of its own: a hidden window, no new dependency. The
// icon is removed after a few seconds.
func platformNotify(title, text string) {
	runtime.LockOSThread() // the window and its messages stay on this thread
	defer runtime.UnlockOSThread()
	user32 := syscall.NewLazyDLL("user32.dll")
	shell32 := syscall.NewLazyDLL("shell32.dll")
	hwnd, _, _ := user32.NewProc("CreateWindowExW").Call(0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("STATIC"))), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	if hwnd == 0 {
		return
	}
	defer user32.NewProc("DestroyWindow").Call(hwnd)
	icon, _, _ := user32.NewProc("LoadIconW").Call(0, 32512) // IDI_APPLICATION
	var d notifyIconData
	d.CbSize = uint32(unsafe.Sizeof(d))
	d.HWnd, d.UID, d.HIcon = hwnd, 1, icon
	d.UFlags = 0x2 | 0x4 | 0x10 // NIF_ICON | NIF_TIP | NIF_INFO
	d.DwInfoFlags = 1           // NIIF_INFO
	copyUTF16(d.SzTip[:], "labmill")
	copyUTF16(d.SzInfoTitle[:], title)
	copyUTF16(d.SzInfo[:], text)
	notify := shell32.NewProc("Shell_NotifyIconW")
	if r, _, _ := notify.Call(0, uintptr(unsafe.Pointer(&d))); r == 0 { // NIM_ADD
		return
	}
	defer notify.Call(2, uintptr(unsafe.Pointer(&d))) // NIM_DELETE
	// Keep the window alive (and its messages answered) while the balloon shows.
	peek, dispatch := user32.NewProc("PeekMessageW"), user32.NewProc("DispatchMessageW")
	var msg [48]byte
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		for {
			if r, _, _ := peek.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1); r == 0 { // PM_REMOVE
				break
			}
			dispatch.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}
}

// notifyIconData is NOTIFYICONDATAW.
type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

// copyUTF16 copies s into dst, cut to fit and ending in a zero.
func copyUTF16(dst []uint16, s string) {
	u := syscall.StringToUTF16(s)
	if len(u) > len(dst) {
		u = append(u[:len(dst)-1], 0)
	}
	copy(dst, u)
}
