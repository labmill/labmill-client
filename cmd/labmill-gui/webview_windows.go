//go:build desktop && windows

package main

import (
	"os"
	"path/filepath"
)

// webviewUserDataPath is the one WebView2 user-data folder Labmill uses, so the
// browser profile (and its warm caches) is reused by every start and does not
// depend on the executable's file name (Wails' default folder is named after
// it). "" lets Wails choose.
func webviewUserDataPath() string {
	dir, err := os.UserCacheDir() // %LocalAppData%
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "labmill", "WebView2")
}
