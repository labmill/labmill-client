//go:build desktop && !windows

package main

// webviewUserDataPath: only WebView2 (Windows) takes a user-data folder.
func webviewUserDataPath() string { return "" }
