//go:build desktop && !windows

package main

// windowProbe is Windows-only (the Windows self-test proves it ran hidden).
func windowProbe() (string, bool, bool) { return "", false, false }
