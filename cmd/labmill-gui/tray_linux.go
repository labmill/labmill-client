//go:build desktop && linux

package main

import (
	"github.com/godbus/dbus/v5"

	"labmill-client/assets/icon"
)

// nativeHideOnClose: Linux closes go through OnBeforeClose (gui.Lifecycle).
const nativeHideOnClose = false

// trayAvailable reports whether a StatusNotifierWatcher (the tray host of
// KDE, most panels, GNOME with the AppIndicator extension) owns its name on
// the session bus. A private connection is used, so systray's shared one is
// never closed here.
func trayAvailable() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	var has bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&has); err != nil {
		return false
	}
	return has
}

// trayIcon: most Linux panels are dark, so the light glyph.
func trayIcon() []byte { return icon.TrayLight32PNG }
