package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/rc"
	"labmill-client/internal/store"
)

func manifestFor(t *testing.T, root string, paths ...string) (entries []store.ManifestEntry) {
	t.Helper()
	es, _, err := BuildManifest(context.Background(), root, paths, nil)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// Findings 1 and 2: rclone ignores RCLONE_* settings and runs no remote
// commands at all.
func TestRcloneIgnoresEnvironmentAndRunsNoCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	t.Setenv("RCLONE_DRY_RUN", "true")
	t.Setenv("RCLONE_EXCLUDE", "*.csv")
	t.Setenv("RCLONE_SFTP_SERVER_COMMAND", "evilcmd")
	t.Setenv("RCLONE_SFTP_SHELL_TYPE", "unix")
	t.Setenv("RCLONE_SFTP_DISABLE_HASHCHECK", "false")
	// rclone read its environment in its own init(), long before this test:
	// reproduce that by poisoning the process-wide globals directly.
	gci := rfs.GetConfig(context.Background())
	oldCI := *gci
	gci.DryRun, gci.IgnoreExisting, gci.IgnoreSize, gci.SizeOnly, gci.NoCheckDest, gci.CheckSum = true, true, true, true, true, true
	gf := filter.GetConfig(context.Background())
	oldF := *gf
	if err := gf.AddRule("- *.csv"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { *gci = oldCI; *gf = oldF })

	local := t.TempDir()
	write(t, filepath.Join(local, "a.csv"), "x,y\n1,2\n")
	write(t, filepath.Join(local, "b.txt"), "bbb")
	entries := manifestFor(t, local, ".")
	remoteRoot, r, srv := transferEnvSrv(t)
	if err := Upload(context.Background(), local, entries, r, nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	for _, f := range []string{"a.csv", "b.txt"} {
		if !exists(filepath.Join(remoteRoot, "proj", "run1", f)) {
			t.Errorf("%s was not uploaded", f)
		}
	}
	out := t.TempDir()
	written, err := Download(context.Background(), r, []string{"a.csv", "."}, out, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(written) != 2 || !exists(filepath.Join(out, "a.csv")) || !exists(filepath.Join(out, "b.txt")) {
		t.Errorf("written = %v", written)
	}
	// rclone itself runs nothing; the only command is Download's own
	// batched sha256sum check afterwards.
	for _, e := range srv.Execs() {
		if !strings.HasPrefix(e, "sha256sum -- ") {
			t.Errorf("rclone ran a command on the cluster: %q", e)
		}
	}
	// stats groups are released
	res, err := rc.Calls.Get("core/group-list").Fn(context.Background(), rc.Params{})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range res["groups"].([]string) {
		if strings.HasPrefix(g, "labmill-") {
			t.Errorf("stats group %q leaked", g)
		}
	}
}

// Finding 3: a destination that is a file on the cluster is an error.
func TestUploadDestinationIsFile(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	local := t.TempDir()
	write(t, filepath.Join(local, "a.txt"), "aaa")
	entries := manifestFor(t, local, ".")
	remoteRoot, r := transferEnv(t)
	write(t, filepath.Join(remoteRoot, "proj", "run1"), "i am a file")
	err := Upload(context.Background(), local, entries, r, nil)
	if err == nil || !strings.Contains(err.Error(), "is a file on the cluster") {
		t.Fatalf("err = %v", err)
	}
	var names []string
	filepath.Walk(remoteRoot, func(p string, _ os.FileInfo, _ error) error { names = append(names, p); return nil })
	if len(names) != 3 { // root, proj, proj/run1
		t.Errorf("remote tree changed: %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(remoteRoot, "proj", "run1")); string(b) != "i am a file" {
		t.Errorf("file overwritten: %q", b)
	}
}

// Finding 4: Download never replaces differing local files; identical files
// are skipped.
func TestDownloadDoesNotOverwrite(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	remoteRoot, r := transferEnv(t)
	base := filepath.Join(remoteRoot, "proj", "run1", "results")
	write(t, filepath.Join(base, "a.txt"), "remote-a")
	write(t, filepath.Join(base, "z.txt"), "remote-z")
	out := t.TempDir()
	write(t, filepath.Join(out, "results", "a.txt"), "LOCAL, different")
	_, err := Download(context.Background(), r, []string{"results"}, out, nil)
	if err == nil || !strings.Contains(err.Error(), "would be replaced") || !strings.Contains(err.Error(), "results/a.txt") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "z.txt") {
		t.Errorf("non-conflicting file listed: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "results", "a.txt")); string(b) != "LOCAL, different" {
		t.Errorf("local file replaced: %q", b)
	}
	if exists(filepath.Join(out, "results", "z.txt")) {
		t.Error("something was copied despite the conflict")
	}

	// A clean download, then again: identical files are skipped, not errors.
	out2 := t.TempDir()
	if _, err := Download(context.Background(), r, []string{"results"}, out2, nil); err != nil {
		t.Fatal(err)
	}
	written, err := Download(context.Background(), r, []string{"results"}, out2, nil)
	if err != nil || len(written) != 2 {
		t.Fatalf("second download: %v %v", written, err)
	}
	// Same size, different mtime counts as different.
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(filepath.Join(out2, "results", "a.txt"), old, old)
	if _, err := Download(context.Background(), r, []string{"results"}, out2, nil); err == nil || !strings.Contains(err.Error(), "results/a.txt") {
		t.Errorf("mtime difference not detected: %v", err)
	}
}

// Finding 5: Check fails fast, before anything is spawned.
func TestCheckFailsFast(t *testing.T) {
	sentinel := errors.New("not logged in to hpcx; log in in Labmill first")
	r := Remote{SSHCommand: []string{"/nonexistent/never-spawned"}, Dir: "x", Check: func(context.Context) error { return sentinel }}
	local := t.TempDir()
	write(t, filepath.Join(local, "a.txt"), "a")
	entries := manifestFor(t, local, ".")
	start := time.Now()
	if err := Upload(context.Background(), local, entries, r, nil); err != sentinel {
		t.Errorf("Upload err = %v", err)
	}
	if _, err := Download(context.Background(), r, []string{"a"}, t.TempDir(), nil); err != sentinel {
		t.Errorf("Download err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("took too long")
	}
}

// Finding 6: files re-checked against the manifest before sending.
func TestUploadDetectsChangedFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	local := t.TempDir()
	write(t, filepath.Join(local, "same.txt"), "approved")
	write(t, filepath.Join(local, "size.txt"), "approved")
	write(t, filepath.Join(local, "link.txt"), "approved")
	write(t, filepath.Join(local, "dir", "f.txt"), "approved")
	secret := filepath.Join(t.TempDir(), "secret")
	write(t, secret, "approved") // same bytes, but reached through a symlink
	entries := manifestFor(t, local, ".")
	remoteRoot, r := transferEnv(t)
	dst := filepath.Join(remoteRoot, "proj", "run1")

	cases := []struct {
		name   string
		mutate func()
		path   string
	}{
		{"content", func() { write(t, filepath.Join(local, "same.txt"), "APPROVED") }, "same.txt"},
		{"size", func() { write(t, filepath.Join(local, "size.txt"), "approved!!") }, "size.txt"},
		{"symlink", func() {
			os.Remove(filepath.Join(local, "link.txt"))
			if err := os.Symlink(secret, filepath.Join(local, "link.txt")); err != nil {
				t.Skip("no symlinks")
			}
		}, "link.txt"},
	}
	for _, c := range cases {
		c.mutate()
		only := []store.ManifestEntry{}
		for _, e := range entries {
			if e.Path == c.path {
				only = append(only, e)
			}
		}
		err := Upload(context.Background(), local, only, r, nil)
		if err == nil || !strings.Contains(err.Error(), "file changed since you approved the upload: "+c.path) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if exists(filepath.Join(dst, c.path)) {
			t.Errorf("%s: changed file was uploaded", c.name)
		}
	}
	// a symlinked parent directory
	os.RemoveAll(filepath.Join(local, "dir"))
	other := t.TempDir()
	write(t, filepath.Join(other, "f.txt"), "approved")
	if err := os.Symlink(other, filepath.Join(local, "dir")); err == nil {
		var only []store.ManifestEntry
		for _, e := range entries {
			if e.Path == "dir/f.txt" {
				only = append(only, e)
			}
		}
		if err := Upload(context.Background(), local, only, r, nil); err == nil || !strings.Contains(err.Error(), "changed since") {
			t.Errorf("symlinked parent: err = %v", err)
		}
	}
	// unchanged files still go
	if err := Upload(context.Background(), local, entries[:0], r, nil); err != nil {
		t.Errorf("empty upload: %v", err)
	}
}

// Finding 9: remote enumeration caps.
func TestDownloadCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	remoteRoot, r := transferEnv(t)
	base := filepath.Join(remoteRoot, "proj", "run1")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		write(t, filepath.Join(base, "many", n), n)
	}
	old := maxRemoteFiles
	maxRemoteFiles = 3
	t.Cleanup(func() { maxRemoteFiles = old })
	out := t.TempDir()
	if _, err := Download(context.Background(), r, []string{"many"}, out, nil); err == nil || !strings.Contains(err.Error(), "more than 3 remote files") {
		t.Errorf("file cap: err = %v", err)
	}
	maxRemoteFiles = old

	deep := filepath.Join(base, "deep")
	for i := 0; i < maxRemoteDepth+3; i++ {
		deep = filepath.Join(deep, "d")
	}
	write(t, filepath.Join(deep, "f"), "x")
	if _, err := Download(context.Background(), r, []string{"deep"}, out, nil); err == nil || !strings.Contains(err.Error(), "deeper than") {
		t.Errorf("depth cap: err = %v", err)
	}
	if exists(filepath.Join(out, "many")) || exists(filepath.Join(out, "deep")) {
		t.Error("files written despite a cap error")
	}
}

// Finding 3 (Download side): a missing path is an error, not a silent empty
// directory.
func TestDownloadMissingPath(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	remoteRoot, r := transferEnv(t)
	os.MkdirAll(filepath.Join(remoteRoot, "proj", "run1"), 0o755)
	if _, err := Download(context.Background(), r, []string{"nope"}, t.TempDir(), nil); err == nil {
		t.Error("Download of a missing path succeeded")
	}
}
