package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRedirectStderr(t *testing.T) {
	orig := os.Stderr
	path := filepath.Join(t.TempDir(), "state", "labmill.log")
	restore := redirectStderr(path)
	if os.Stderr == orig {
		t.Fatal("os.Stderr not redirected")
	}
	fmt.Fprint(os.Stderr, "Warning: from a library\n")
	restore()
	if os.Stderr != orig {
		t.Error("os.Stderr not restored")
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "Warning: from a library") {
		t.Errorf("log = %q, %v", b, err)
	}
	if fi, err := os.Stat(path); runtime.GOOS != "windows" && (err != nil || fi.Mode().Perm() != 0o600) {
		t.Errorf("log mode = %v, %v", fi.Mode(), err)
	}
	// No path or an unusable one: nothing changes.
	redirectStderr("")()
	redirectStderr(filepath.Join(path, "not-a-dir", "x.log"))()
	if os.Stderr != orig {
		t.Error("os.Stderr changed without a usable log file")
	}
}
