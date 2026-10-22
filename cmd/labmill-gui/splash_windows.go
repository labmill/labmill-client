//go:build desktop && windows

package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"labmill-client/internal/gui"
	"labmill-client/internal/startup"
)

// The splash is plain Win32 (user32 and gdi32 through lazy DLL procs, like
// the other *_windows.go files), drawn by hand: a progress control's
// marquee style needs the version 6 common controls, which need a manifest
// the app doesn't have, and a self-drawn bar costs nothing extra.

var (
	gdi32                  = windows.NewLazySystemDLL("gdi32.dll")
	kernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procShowWindow         = user32.NewProc("ShowWindow")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procPostMessageW       = user32.NewProc("PostMessageW")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procSetTimer           = user32.NewProc("SetTimer")
	procKillTimer          = user32.NewProc("KillTimer")
	procBeginPaint         = user32.NewProc("BeginPaint")
	procEndPaint           = user32.NewProc("EndPaint")
	procFillRect           = user32.NewProc("FillRect")
	procDrawTextW          = user32.NewProc("DrawTextW")
	procDrawIconEx         = user32.NewProc("DrawIconEx")
	procDestroyIcon        = user32.NewProc("DestroyIcon")
	procLoadImageW         = user32.NewProc("LoadImageW")
	procLoadCursorW        = user32.NewProc("LoadCursorW")
	procSetCursor          = user32.NewProc("SetCursor")
	procInvalidateRect     = user32.NewProc("InvalidateRect")
	procGetClientRect      = user32.NewProc("GetClientRect")
	procSystemParamsInfoW  = user32.NewProc("SystemParametersInfoW")
	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	procCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procCreateFontW        = gdi32.NewProc("CreateFontW")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procSetBkMode          = gdi32.NewProc("SetBkMode")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procBitBlt             = gdi32.NewProc("BitBlt")
)

// Win32 constants used below.
const (
	wsPopup, wsBorder                    = 0x80000000, 0x00800000
	wsExToolWindow, wsExNoActivate       = 0x80, 0x08000000
	swShowNoActivate                     = 4
	wmDestroy, wmClose, wmPaint, wmTimer = 0x2, 0x10, 0xF, 0x113
	wmEraseBkgnd, wmSetCursor            = 0x14, 0x20
	idcAppStarting                       = 32650
	imageIcon                            = 1
	spiGetWorkArea                       = 0x30
	dtLeftSingleLineNoPrefix             = 0x0000 | 0x0020 | 0x0800 | 0x0004 // DT_LEFT | DT_SINGLELINE | DT_NOPREFIX | DT_VCENTER
	srcCopy                              = 0x00CC0020
	diNormal                             = 3
	timerBar, timerLife                  = 1, 2
	splashLife                           = 60000 // ms: the splash closes itself
	splashTick                           = 25    // ms per frame of the moving bar
	appIconResourceID                    = 3     // cmd/labmill-winres: the icon group Wails also uses
	splashText                           = "Starting labmill…"
	splashFont                           = "Segoe UI"
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      [2]int32
}

type paintStruct struct {
	Hdc       uintptr
	Erase     int32
	Paint     [4]int32
	Restore   int32
	IncUpdate int32
	Reserved  [32]byte
}

// splashPaint is the splash's drawing state. Only the splash thread uses it
// (the window procedure runs there).
var splashPaint struct {
	scale                 float64
	bg, track, fill, text uintptr // brushes (bg, track, fill) and the text colour
	font, icon            uintptr
	pos                   int // marquee position in pixels
}

var splashProcCB = syscall.NewCallback(splashWndProc)

// colorref packs r, g, b into a COLORREF.
func colorref(r, g, b uint8) uintptr { return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16 }

// appsUseLightTheme reads the Windows app theme; light when unreadable.
func appsUseLightTheme() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err != nil || v != 0
}

// startSplash shows the splash on its own thread and returns at once. The
// position, scale and theme are read here (a few milliseconds, and the
// monitor enumeration isn't safe to run twice at the same time); creating
// and running the window is the thread's job. closeSplash may be called
// from any thread, any number of times.
func startSplash(geom splashGeom) (closeSplash func()) {
	splashStarts.Add(1)
	scale := windowScale()
	var work [4]int32
	procSystemParamsInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&work)), 0)
	primary := primaryWork(work)
	x, y, w, h := splashRect(geom.Window, windowScreens(nil), primary, scale)
	light := appsUseLightTheme()

	var (
		hwnd   atomic.Uintptr
		closed atomic.Bool
		once   sync.Once
	)
	go func() {
		defer func() { recover() }() // a splash problem must never stop the app
		runtime.LockOSThread()       // the window and its message loop live on this thread only
		runSplash(x, y, w, h, scale, light, &hwnd, &closed)
	}()
	return func() {
		once.Do(func() {
			closed.Store(true)
			if h := hwnd.Load(); h != 0 {
				procPostMessageW.Call(h, wmClose, 0, 0)
			}
		})
	}
}

// runSplash creates the window, runs its message loop and cleans up. closed
// is checked after hwnd is stored, and closeSplash sets it before it reads
// hwnd, so a close racing the creation is never lost.
func runSplash(x, y, w, h int, scale float64, light bool, hwnd *atomic.Uintptr, closed *atomic.Bool) {
	inst, _, _ := procGetModuleHandleW.Call(0)
	cursor, _, _ := procLoadCursorW.Call(0, idcAppStarting)
	className, _ := windows.UTF16PtrFromString(splashClass)
	wc := wndClassEx{Size: uint32(unsafe.Sizeof(wndClassEx{})), WndProc: splashProcCB, Instance: inst, Cursor: cursor, ClassName: className}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	p := &splashPaint
	p.scale = scale
	bg, track, text := [3]uint8{243, 243, 243}, [3]uint8{218, 218, 218}, colorref(32, 32, 32)
	if !light {
		bg, track, text = [3]uint8{32, 32, 32}, [3]uint8{66, 66, 66}, colorref(255, 255, 255)
	}
	p.bg, _, _ = procCreateSolidBrush.Call(colorref(bg[0], bg[1], bg[2]))
	p.track, _, _ = procCreateSolidBrush.Call(colorref(track[0], track[1], track[2]))
	p.fill, _, _ = procCreateSolidBrush.Call(colorref(0, 120, 212)) // the Windows accent blue
	p.text = text
	face, _ := windows.UTF16PtrFromString(splashFont)
	f, _, _ := procCreateFontW.Call(uintptr(-int(scale*17+0.5)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(face))) // -17 logical px, CLEARTYPE_QUALITY (5)
	p.font = f
	isz := uintptr(scale*48 + 0.5)
	p.icon, _, _ = procLoadImageW.Call(inst, appIconResourceID, imageIcon, isz, isz, 0)
	defer func() {
		for _, o := range []uintptr{p.bg, p.track, p.fill, p.font} {
			procDeleteObject.Call(o)
		}
		if p.icon != 0 {
			procDestroyIcon.Call(p.icon)
		}
	}()

	title, _ := windows.UTF16PtrFromString(splashTitle)
	h0, _, _ := procCreateWindowExW.Call(wsExToolWindow|wsExNoActivate, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		wsPopup|wsBorder, uintptr(int32(x)), uintptr(int32(y)), uintptr(w), uintptr(h), 0, 0, inst, 0)
	if h0 == 0 {
		return
	}
	hwnd.Store(h0)
	defer hwnd.Store(0) // closeSplash must not post to a destroyed window
	if closed.Load() {
		procDestroyWindow.Call(h0)
		return
	}
	procSetTimer.Call(h0, timerBar, splashTick, 0)
	procSetTimer.Call(h0, timerLife, splashLife, 0)
	procShowWindow.Call(h0, swShowNoActivate) // shown without taking the focus
	startup.Mark("gui: splash shown")
	defer startup.Mark("gui: splash closed")
	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// primaryWork converts a RECT (left, top, right, bottom) to a gui.Screen
// through the same type the monitor enumeration returns.
func primaryWork(r [4]int32) gui.Screen {
	return gui.Screen{X: int(r[0]), Y: int(r[1]), Width: int(r[2] - r[0]), Height: int(r[3] - r[1])}
}

func splashWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmEraseBkgnd:
		return 1 // paint draws everything
	case wmSetCursor:
		c, _, _ := procLoadCursorW.Call(0, idcAppStarting)
		procSetCursor.Call(c)
		return 1
	case wmTimer:
		if wparam == timerLife {
			procDestroyWindow.Call(hwnd)
			return 0
		}
		splashPaint.pos += int(splashPaint.scale*5 + 0.5)
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmPaint:
		splashDraw(hwnd)
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procKillTimer.Call(hwnd, timerBar)
		procKillTimer.Call(hwnd, timerLife)
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// splashDraw paints the whole client area into a memory bitmap and copies
// it over, so the moving bar doesn't flicker.
func splashDraw(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	var rc [4]int32
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	cw, ch := int(rc[2]), int(rc[3])
	p := &splashPaint
	mem, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatBitmap.Call(hdc, uintptr(cw), uintptr(ch))
	old, _, _ := procSelectObject.Call(mem, bmp)
	defer func() {
		procSelectObject.Call(mem, old)
		procDeleteObject.Call(bmp)
		procDeleteDC.Call(mem)
	}()
	s := func(v float64) int { return int(v*p.scale + 0.5) }
	fill := func(brush uintptr, l, t, r, b int) {
		R := [4]int32{int32(l), int32(t), int32(r), int32(b)}
		procFillRect.Call(mem, uintptr(unsafe.Pointer(&R)), brush)
	}
	fill(p.bg, 0, 0, cw, ch)
	if p.icon != 0 {
		procDrawIconEx.Call(mem, uintptr(s(20)), uintptr(s(24)), p.icon, uintptr(s(48)), uintptr(s(48)), 0, 0, diNormal)
	}
	// Text, right of the icon, in the upper half.
	procSetBkMode.Call(mem, 1) // TRANSPARENT
	procSetTextColor.Call(mem, p.text)
	procSelectObject.Call(mem, p.font)
	tr := [4]int32{int32(s(84)), int32(s(20)), int32(cw - s(16)), int32(s(56))}
	txt, _ := windows.UTF16PtrFromString(splashText)
	procDrawTextW.Call(mem, uintptr(unsafe.Pointer(txt)), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), dtLeftSingleLineNoPrefix)
	// The moving bar: a track with a segment that wraps around.
	bl, br, bt, bb := s(84), cw-s(20), s(66), s(72)
	fill(p.track, bl, bt, br, bb)
	if tw := br - bl; tw > 0 {
		seg := tw * 3 / 10
		a := p.pos % tw
		fill(p.fill, bl+a, bt, bl+min(a+seg, tw), bb)
		if a+seg > tw {
			fill(p.fill, bl, bt, bl+a+seg-tw, bb)
		}
	}
	procBitBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), mem, 0, 0, srcCopy)
}

var procGetClassNameW = user32.NewProc("GetClassNameW")

// isSplashWindow reports whether h is the start splash, which must never be
// taken for the main window (position restore, self-test probe).
func isSplashWindow(h uintptr) bool {
	var cls [32]uint16
	n, _, _ := procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&cls[0])), uintptr(len(cls)))
	return n != 0 && windows.UTF16ToString(cls[:]) == splashClass
}
