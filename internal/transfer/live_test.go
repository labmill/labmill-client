package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"labmill-client/internal/livetest"
)

// liveDir is a fixed folder the owner designated for live checks. Each run
// overwrites it; the test never deletes anything remotely.
const liveDir = "/data/mygroup/labmill-live/transfer"

// TestLiveTransfer uploads a small tree to the real cluster over the
// owner's ControlMaster and downloads it back.
func TestLiveTransfer(t *testing.T) {
	alias := livetest.Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	src := t.TempDir()
	rng := rand.New(rand.NewSource(42))
	files := map[string]int{"a.bin": 60000, "sub/b.txt": 30000, "données.csv": 10000}
	var paths []string
	for rel, n := range files {
		data := make([]byte, n)
		rng.Read(data)
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, rel)
	}
	entries, skipped, err := BuildManifest(ctx, src, paths, nil)
	if err != nil || len(skipped) != 0 || len(entries) != 3 {
		t.Fatalf("manifest: %v skipped=%v n=%d", err, skipped, len(entries))
	}
	remote := Remote{SSHCommand: []string{liveSFTPScript(t, alias)}, Dir: liveDir}

	var last Progress
	var calls int
	err = Upload(ctx, src, entries, remote, func(p Progress) {
		calls++
		if p.Bytes < last.Bytes || p.Files < last.Files {
			t.Errorf("progress went backwards: %+v after %+v", p, last)
		}
		last = p
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if calls == 0 || last.Bytes != last.TotalBytes || last.Files != 3 {
		t.Errorf("final progress %+v after %d calls", last, calls)
	}

	dst := t.TempDir()
	var dlast Progress
	written, err := Download(ctx, remote, paths, dst, func(p Progress) {
		if p.Bytes < dlast.Bytes {
			t.Errorf("download progress went backwards: %+v after %+v", p, dlast)
		}
		dlast = p
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	t.Logf("uploaded %d files (%d bytes, %d progress calls), downloaded %d", len(entries), last.TotalBytes, calls, len(written))
	for _, e := range entries {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(e.Path)))
		if err != nil {
			t.Fatalf("%s: %v", e.Path, err)
		}
		want, _ := os.ReadFile(filepath.Join(src, filepath.FromSlash(e.Path)))
		sum := sha256.Sum256(got)
		if !bytes.Equal(got, want) || int64(len(got)) != e.Size || hex.EncodeToString(sum[:]) != e.SHA256 {
			t.Errorf("%s differs after round trip", e.Path)
		}
	}
}

// liveSFTPScript writes a stand-in for `ssh <alias> -s sftp` that runs
// sftp-server as a command over the owner's master: a login host
// accepts the sftp subsystem request but closes it without a byte (Labmill's
// ssh-exec falls back to the command; see sshx.Exec). It ignores the "-s
// sftp" rclone appends.
func liveSFTPScript(t *testing.T, alias string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sftp-over-ssh.sh")
	script := "#!/bin/sh\nexec ssh -o BatchMode=yes " + alias + " /usr/libexec/openssh/sftp-server\n"
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}
