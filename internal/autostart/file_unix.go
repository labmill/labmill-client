//go:build !windows

package autostart

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// goos and homeDir are variables so tests can check both file layouts.
var (
	goos    = runtime.GOOS
	homeDir = os.UserHomeDir
)

// entryFile returns the registration file for e and its content when
// enabled or disabled.
func entryFile(e Entry) (path string, content func(off bool) []byte, err error) {
	home, err := homeDir()
	if err != nil {
		return "", nil, err
	}
	if goos == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", e.Label+".plist"),
			func(off bool) []byte { return launchAgentPlist(e, off) }, nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "autostart", e.Name+".desktop"),
		func(off bool) []byte { return desktopEntry(e, off) }, nil
}

func enable(e Entry) error { return write(e, false) }

func disable(e Entry) error {
	path, _, err := entryFile(e)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return write(e, true)
}

// enabled reports whether the registration file exists and isn't disabled:
// whatever command it runs counts (the user or an installer may have edited
// it). Linux: no Hidden=true line; macOS: no Disabled key.
func enabled(e Entry) (bool, error) {
	path, _, err := entryFile(e)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if goos == "darwin" {
		return !bytes.Contains(b, []byte("<key>Disabled</key>")), nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.EqualFold(strings.ReplaceAll(strings.TrimSpace(line), " ", ""), "Hidden=true") {
			return false, nil
		}
	}
	return true, nil
}

func write(e Entry, off bool) error {
	path, content, err := entryFile(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	if err := os.WriteFile(path, content(off), 0o644); err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	return nil
}
