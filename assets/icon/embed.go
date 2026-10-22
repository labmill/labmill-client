// Package icon embeds the Labmill icon files the programs need at run time
// (the desktop app's window and tray icons) and at build time (the Windows
// exe resource, cmd/labmill-winres). See README.md for the full set.
package icon

import _ "embed"

// App256PNG is the app icon at 256x256 (Linux window icon).
//
//go:embed labmill-256.png
var App256PNG []byte

// AppICO is the multi-size app icon (16-256) for the Windows exe resource.
//
//go:embed labmill.ico
var AppICO []byte

// Tray icons: "light" is the white glyph for dark taskbars and panels,
// "dark" the dark glyph for light ones. Windows needs .ico; the Linux tray
// (StatusNotifierItem) takes PNG.
var (
	//go:embed tray-light.ico
	TrayLightICO []byte
	//go:embed tray-dark.ico
	TrayDarkICO []byte
	//go:embed tray-light-32.png
	TrayLight32PNG []byte
	//go:embed tray-dark-32.png
	TrayDark32PNG []byte
)
