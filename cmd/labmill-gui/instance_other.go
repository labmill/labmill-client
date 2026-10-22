//go:build desktop && !windows

package main

// otherInstanceRunning: only Windows needs it (Wails' single-instance check
// exits the second process after the early tray would have started there).
func otherInstanceRunning(string) bool { return false }
