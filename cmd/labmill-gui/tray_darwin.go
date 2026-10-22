//go:build desktop && darwin

package main

import (
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
)

// macOS has no tray icon yet: fyne.io/systray's menu-bar item must be
// created on the main thread, which Wails v2's NSApp loop owns, and that
// can't be built or tested from the Linux build host. Instead the Mac does
// what Mac apps do: Wails' HideWindowOnClose hides the app on the red close
// button (no OnBeforeClose), File > Close Window (Cmd+W, appMenu) hides it
// the same way, a click on the Dock icon shows it again, and the app menu's
// Quit (Cmd+Q) goes through OnBeforeClose and quits.
const nativeHideOnClose = true

// appMenu is Wails' default Mac menu (app, Edit, Window; the Edit menu also
// carries Start Dictation, which voice needs) plus File > Settings… (Cmd+,)
// and File > Close Window.
func appMenu(hide, settings func()) *menu.Menu {
	file := menu.NewMenu()
	file.AddText("Settings…", keys.CmdOrCtrl(","), func(*menu.CallbackData) { settings() })
	file.AddText("Close Window", keys.CmdOrCtrl("w"), func(*menu.CallbackData) { hide() })
	return menu.NewMenuFromItems(menu.AppMenu(), menu.SubMenu("File", file), menu.EditMenu(), menu.WindowMenu())
}

type trayHooks struct {
	Open           func()
	Settings       func()
	Quit           func() bool
	StartAtLogin   func(on bool) error
	StartAtLoginOn bool
}

type tray struct{}

func trayAvailable() bool            { return false }
func startTray(trayHooks) *tray      { return &tray{} }
func (t *tray) Active() bool         { return false }
func (t *tray) SetStatus(string)     {}
func (t *tray) SetStartAtLogin(bool) {}
func (t *tray) Stop()                {}
