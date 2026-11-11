package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDryRun(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"-dir", "../../tests/cases", "-dry-run", "-cases", "fail-*"}, &out, &errb); code != 0 {
		t.Fatalf("code %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "fail-exit-1") || strings.Contains(out.String(), "py-numpy-sum") || !strings.Contains(out.String(), "8 cases") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestInvalidAndNoDryRun(t *testing.T) {
	bad := t.TempDir()
	os.Mkdir(filepath.Join(bad, "x"), 0o755)
	os.WriteFile(filepath.Join(bad, "x", "case.yaml"), []byte("id: y\n"), 0o644)
	var out, errb bytes.Buffer
	if code := run([]string{"-dir", bad, "-dry-run"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "invalid case") {
		t.Errorf("invalid: code %d, %s", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"-dir", "../../tests/cases"}, &out, &errb); code != 2 {
		t.Errorf("no -dry-run: code %d", code)
	}
}
