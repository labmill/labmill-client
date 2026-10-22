//go:build !windows && !darwin

package app

// hiddenAttr is Windows-only; elsewhere hidden means a leading ".".
func hiddenAttr(string) bool { return false }
