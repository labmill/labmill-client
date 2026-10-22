package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/lib/encoder"

	"labmill-client/internal/store"
)

func TestCheckLocalName(t *testing.T) {
	cases := []struct {
		goos, name string
		bad        bool
	}{
		{"linux", "a:b*c?.txt", false},
		{"linux", "x\xff", false},
		{"windows", "ok/dir/file.txt", false},
		{"windows", "dir/a:b.txt", true},
		{"windows", "a*b", true},
		{"windows", "a?b", true},
		{"windows", `a"b`, true},
		{"windows", "a<b", true},
		{"windows", "a>b", true},
		{"windows", "a|b", true},
		{"windows", `a\b`, true},
		{"windows", "dir./file", true},
		{"windows", "dir/file ", true},
		{"windows", "dir/file.", true},
		{"darwin", "x\xff", true},
		{"windows", "x\xff", true},
		{"darwin", "dir/\xc3(", true},
		{"darwin", "a:b*c.txt", false},
		{"darwin", "héllo/日本.txt", false},
		{"linux", "\u5831\u544A\uFF0F2024.csv", false},
		{"linux", "a\u2400b", false},
		{"windows", ".", false},
		{"windows", "a/../b", false},
		{"darwin", "dir/a\uFF0Fb", false},
		{"darwin", "a\u2400b", false},
		{"windows", "a\uFF0Fb", false},
		{"windows", "a\u2400b", false},
		{"linux", "a\u2401b", true},
		{"linux", "a\u241Fb", true},
		{"linux", "a\u2421b", true},
		{"darwin", "a\u2401b", true},
		{"darwin", "a\u2421b", true},
		{"linux", "a\u201Bb", true},
		{"darwin", "a\u201Bb", true},
		{"windows", "a\u201Bb", true},
		{"linux", "a\x01b", false},
		{"windows", "a\x01b", true},
		{"linux", "a\uFF1Ab", false},
		{"windows", "a\uFF1Ab", true},
		{"windows", "dir/\uFF0Ax", true},
		{"windows", "name\u2420", true},
		{"windows", "name\uFF0E", true},
		{"windows", "na\u2420me", false},
		{"windows", "\u65E5\u672C\uFF0F", false},
		{"windows", "dir/\u65E5\u672C.txt", false},
	}
	for _, c := range cases {
		err := checkLocalName(c.goos, c.name)
		if (err != nil) != c.bad {
			t.Errorf("checkLocalName(%s, %q) = %v, bad=%v", c.goos, c.name, err, c.bad)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("%q", c.name)) {
			t.Errorf("error %q does not name the file", err)
		}
	}
}

// TestLocalMaskMatchesRclone catches drift between Labmill's per-GOOS masks and
// rclone's build-tagged encoder.OS for the GOOS the test runs on.
func TestLocalMaskMatchesRclone(t *testing.T) {
	if got := localMask(runtime.GOOS); got != encoder.OS {
		t.Errorf("localMask(%s) = %v, rclone encoder.OS = %v", runtime.GOOS, got, encoder.OS)
	}
}

func TestRecheckLocalReportsHashProgress(t *testing.T) {
	root := t.TempDir()
	data := strings.Repeat("z", 3<<20+5)
	if err := os.WriteFile(filepath.Join(root, "f.bin"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(data))
	e := store.ManifestEntry{Path: "f.bin", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	var calls int
	var last int64
	err := recheckLocal(context.Background(), root, e, func(n int64) {
		calls++
		if n < last {
			t.Errorf("progress went backwards: %d after %d", n, last)
		}
		last = n
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls < 2 || last != e.Size {
		t.Errorf("calls=%d last=%d, want several calls ending at %d", calls, last, e.Size)
	}
}

// The safety re-hash is reported in Checking, never in Bytes, so Bytes
// (data actually sent) can't run ahead and then fall back.
func TestPollerReportsCheckingSeparately(t *testing.T) {
	var reports []Progress
	pl := pollProgress(context.Background(), Progress{TotalBytes: 100, TotalFiles: 1}, func(p Progress) { reports = append(reports, p) })
	pl.setHashing(40)
	pl.emit(nil)
	pl.setHashing(0)
	pl.emit(nil)
	pl.finish(nil)
	var sawChecking bool
	var last int64
	for _, p := range reports {
		if p.Checking == 40 {
			sawChecking = true
			if p.Bytes != 0 {
				t.Errorf("Bytes = %d while only checking", p.Bytes)
			}
		}
		if p.Bytes < last {
			t.Errorf("Bytes went backwards: %d after %d", p.Bytes, last)
		}
		last = p.Bytes
	}
	if !sawChecking {
		t.Errorf("no report with Checking = 40: %+v", reports)
	}
}

type shutdownFake struct {
	rfs.Fs // nil: only Shutdown is called
	n      int
}

func (f *shutdownFake) Shutdown(context.Context) error { f.n++; return nil }

func TestShutdownFsCallsShutdowner(t *testing.T) {
	old := skipShutdown
	skipShutdown = false
	defer func() { skipShutdown = old }()
	f := &shutdownFake{}
	shutdownFs(f)
	if f.n != 1 {
		t.Errorf("Shutdown called %d times, want 1", f.n)
	}
	shutdownFs(nil) // must not panic
}
