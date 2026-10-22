package main

import (
	"labmill-client/internal/launch"
	"labmill-client/internal/tui"
)

// The TUI's bridge is its front end's launch.UI, and launch's Session is
// what the TUI model sends chat turns to.
var (
	_ launch.UI   = (*tui.Bridge)(nil)
	_ tui.Session = (*launch.Session)(nil)
)
