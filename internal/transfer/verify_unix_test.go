//go:build !windows

package transfer

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"labmill-client/internal/backend"
)

// The quoted sha256sum command must reach a POSIX shell as exactly the
// intended arguments, whatever the file names hold. printf stands in for
// sha256sum so the test needs no ssh server.
func TestSumArgvSurvivesShell(t *testing.T) {
	t.Chdir(t.TempDir())
	names := append([]string{"a;touch pwned", "x`touch pwned`", "~root", "*", "tab\there", "new\nline", "-n", "é ü"}, oddNames...)
	src := Remote{Dir: "my dir"}
	argv := sumArgv(src, names)
	if argv[0] != "sha256sum" || argv[1] != "--" {
		t.Fatalf("argv = %q", argv[:2])
	}
	cmd := backend.Quote(append([]string{"printf", "%s\\0"}, argv[2:]...))
	out, err := exec.Command("sh", "-c", cmd).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(got) != len(names) {
		t.Fatalf("got %d args, want %d: %q", len(got), len(names), got)
	}
	for i, n := range names {
		if want := "my dir/" + n; got[i] != want {
			t.Errorf("arg %d = %q, want %q", i, got[i], want)
		}
	}
	if _, err := os.Stat("pwned"); err == nil {
		t.Error("a file name was executed by the shell")
	}
}
