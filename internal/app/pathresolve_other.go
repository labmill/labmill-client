//go:build !windows

package app

import "path/filepath"

// resolveLinks resolves symlinks (see evalLinks).
func resolveLinks(p string) (string, error) { return filepath.EvalSymlinks(p) }
