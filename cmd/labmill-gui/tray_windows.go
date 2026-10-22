//go:build desktop && windows

package main

import (
	"golang.org/x/sys/windows/registry"

	"labmill-client/assets/icon"
)

// nativeHideOnClose: Windows closes go through OnBeforeClose (gui.Lifecycle).
const nativeHideOnClose = false

// trayAvailable: the Windows notification area is always there.
func trayAvailable() bool { return true }

// trayIcon picks the glyph for the taskbar's theme: the dark glyph on a
// light taskbar (SystemUsesLightTheme = 1), else the light one (also when
// the setting can't be read). It is read once at start.
func trayIcon() []byte {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if v, _, err := k.GetIntegerValue("SystemUsesLightTheme"); err == nil && v == 1 {
			return icon.TrayDarkICO
		}
	}
	return icon.TrayLightICO
}
