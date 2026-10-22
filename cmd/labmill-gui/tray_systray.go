//go:build desktop && (windows || linux)

package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2/pkg/menu"

	"labmill-client/internal/gui"
)

// The tray uses fyne.io/systray (Apache-2.0). It coexists with Wails v2
// because it needs nothing from Wails' main loop:
//   - Windows: systray.Run creates a hidden message window and runs its
//     message loop; both must stay on one OS thread, so Run gets its own
//     goroutine locked to a thread, next to Wails' UI thread.
//   - Linux: a pure-Go D-Bus StatusNotifierItem (no GTK, so no second GTK
//     main loop). It needs a StatusNotifierWatcher on the session bus (KDE,
//     most panels, GNOME only with the AppIndicator extension); without one
//     trayAvailable is false and closing the window quits as before.
//
// fyne.io/systray's RunWithExternalLoop is not used on Windows: it starts
// the message loop in an unlocked goroutine, which can't receive the
// messages of a window created on another thread.

// trayHooks are the actions of the tray menu.
type trayHooks struct {
	Open           func()              // "Open labmill" and a left click
	Settings       func()              // "Settings…": show the window on the Settings page
	Quit           func() bool         // "Quit labmill"; false when it couldn't act yet (the menu keeps running)
	StartAtLogin   func(on bool) error // the "Start in the tray when I log in" checkbox
	StartAtLoginOn bool                // its initial state
}

// tray is the system-tray icon. The zero value (no tray) is valid: Active
// is false and the methods do nothing.
type tray struct {
	active atomic.Bool
	mu     sync.Mutex
	status *systray.MenuItem
	login  *systray.MenuItem
	last   string
}

// startTray puts the icon in the tray if the desktop has one. It waits up
// to 5 s for the icon; call it off the UI thread.
func startTray(h trayHooks) *tray {
	t := &tray{}
	if !trayAvailable() {
		return t
	}
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread() // the tray window and its message loop share this thread (Windows)
		systray.Run(func() { t.build(h); close(ready) }, nil)
	}()
	select {
	case <-ready:
		t.active.Store(true)
	case <-time.After(5 * time.Second):
	}
	return t
}

// build creates the icon and menu (systray's onReady).
func (t *tray) build(h trayHooks) {
	systray.SetIcon(trayIcon())
	systray.SetTitle("labmill")
	systray.SetTooltip(gui.TrayStarting)
	open := systray.AddMenuItem("Open labmill", "Show the Labmill window")
	settingsItem := systray.AddMenuItem("Settings…", "Show Labmill's settings")
	status := systray.AddMenuItem(gui.TrayStarting, "")
	status.Disable()
	systray.AddSeparator()
	login := systray.AddMenuItemCheckbox("Start in the tray when I log in", "Start Labmill hidden in the tray when you log in to this computer", h.StartAtLoginOn)
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit labmill", "Quit Labmill; your cluster login ends after 10 minutes")
	// Left click (Windows) or activate (Linux) opens the window; the right
	// click shows the menu.
	systray.SetOnTapped(h.Open)
	t.mu.Lock()
	t.status = status
	t.login = login
	t.mu.Unlock()
	go func() {
		for {
			select {
			case <-open.ClickedCh:
				h.Open()
			case <-settingsItem.ClickedCh:
				if h.Settings != nil {
					h.Settings()
				}
			case <-login.ClickedCh:
				on := !login.Checked()
				if h.StartAtLogin(on) == nil {
					if on {
						login.Check()
					} else {
						login.Uncheck()
					}
				}
			case <-quit.ClickedCh:
				if h.Quit() {
					return
				}
			}
		}
	}()
}

// Active reports whether the icon is up (a click on it can bring the
// window back).
func (t *tray) Active() bool { return t != nil && t.active.Load() }

// SetStatus sets the disabled status line of the menu.
func (t *tray) SetStatus(s string) {
	if !t.Active() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status == nil || s == t.last {
		return
	}
	t.last = s
	t.status.SetTitle(s)
	systray.SetTooltip(gui.TrayTooltip(s))
}

// SetStartAtLogin sets the "Start in the tray when I log in" checkbox (after
// the Settings page changed it).
func (t *tray) SetStartAtLogin(on bool) {
	if !t.Active() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.login == nil {
		return
	}
	if on {
		t.login.Check()
	} else {
		t.login.Uncheck()
	}
}

// Stop removes the icon.
func (t *tray) Stop() {
	if t.Active() {
		t.active.Store(false)
		systray.Quit()
	}
}

// appMenu: no application menu outside macOS.
func appMenu(hide, settings func()) *menu.Menu { return nil }
