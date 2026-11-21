package transfer

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"labmill-client/internal/sshx/sshtest"
)

// A file that is already on the cluster with the same size and SHA-256 is not sent, even when its
// modification time differs; a same-size file with other content is replaced.
func TestUploadSkipsFilesAlreadyOnTheCluster(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	local := t.TempDir()
	write(t, filepath.Join(local, "d", "present.txt"), "same content")
	write(t, filepath.Join(local, "d", "other.txt"), "aaaa")
	write(t, filepath.Join(local, "d", "new.txt"), "brand new")
	entries, _, err := BuildManifest(context.Background(), local, []string{"d"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSha256sum{lie: map[string]bool{}, release: make(chan struct{})}
	t.Cleanup(func() { close(f.release) })
	remoteRoot, r, _ := transferEnvCmds(t, map[string]sshtest.Handler{"sha256sum": f.handler})
	f.root = remoteRoot
	base := filepath.Join(remoteRoot, "proj", "run1", "d")
	write(t, filepath.Join(base, "present.txt"), "same content")
	write(t, filepath.Join(base, "other.txt"), "bbbb")
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(base, "present.txt"), old, old)
	var present []string
	var replaced []string
	r.OnReplace = func(rel string, _ int64) { replaced = append(replaced, rel) }
	r.OnPresent = func(rels []string) { present = rels }
	var last Progress
	if err := Upload(context.Background(), local, entries, r, func(p Progress) { last = p }); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !reflect.DeepEqual(present, []string{"d/present.txt"}) {
		t.Errorf("present = %v", present)
	}
	want := []string{"proj/run1/d/other.txt", "proj/run1/d/present.txt"}
	if got := f.args(); !reflect.DeepEqual(got, want) {
		t.Errorf("sha256sum asked about %q, want only the same-size files %q", got, want)
	}
	if !reflect.DeepEqual(replaced, []string{"d/other.txt"}) {
		t.Errorf("replaced = %v", replaced)
	}
	if fi, _ := os.Stat(filepath.Join(base, "present.txt")); !fi.ModTime().Equal(old) {
		t.Errorf("present.txt was sent again (mtime %v)", fi.ModTime())
	}
	if b, _ := os.ReadFile(filepath.Join(base, "other.txt")); string(b) != "aaaa" {
		t.Errorf("other.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "new.txt")); string(b) != "brand new" {
		t.Errorf("new.txt = %q", b)
	}
	if last.Files != 3 || last.Bytes != last.TotalBytes {
		t.Errorf("final progress %+v", last)
	}
}

// Without a working sha256sum nothing is skipped by checksum; the upload still works.
func TestUploadWithoutSha256sumSkipsNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	local := t.TempDir()
	write(t, filepath.Join(local, "d", "a.txt"), "same content")
	entries, _, _ := BuildManifest(context.Background(), local, []string{"d"}, nil)
	remoteRoot, r := transferEnv(t)
	write(t, filepath.Join(remoteRoot, "proj", "run1", "d", "a.txt"), "same content")
	r.OnPresent = func(rels []string) {
		if len(rels) > 0 {
			t.Error("OnPresent without a checksum")
		}
	}
	if err := Upload(context.Background(), local, entries, r, nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
}

// A retry with the same Sent map does not hash again what the first attempt sent;
// a file changed on the cluster in between is hashed and replaced.
func TestRetryDoesNotRehashSentFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	local := t.TempDir()
	write(t, filepath.Join(local, "d", "a.txt"), "aaaa")
	write(t, filepath.Join(local, "d", "b.txt"), "bbbb")
	entries, _, err := BuildManifest(context.Background(), local, []string{"d"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSha256sum{lie: map[string]bool{}, release: make(chan struct{})}
	t.Cleanup(func() { close(f.release) })
	remoteRoot, r, _ := transferEnvCmds(t, map[string]sshtest.Handler{"sha256sum": f.handler})
	f.root = remoteRoot
	r.Sent = SentFiles{}
	if err := Upload(context.Background(), local, entries, r, nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(r.Sent) != 2 {
		t.Fatalf("sent = %v", r.Sent)
	}
	base := filepath.Join(remoteRoot, "proj", "run1", "d")
	cur := time.Now().Add(time.Hour).Truncate(time.Second)
	write(t, filepath.Join(base, "b.txt"), "xxxx") // same size, other content, new mtime
	os.Chtimes(filepath.Join(base, "b.txt"), cur, cur)
	var present []string
	r.OnPresent = func(rels []string) { present = rels }
	if err := Upload(context.Background(), local, entries, r, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !reflect.DeepEqual(present, []string{"d/a.txt"}) {
		t.Errorf("present = %v", present)
	}
	if got := f.args(); !reflect.DeepEqual(got, []string{"proj/run1/d/b.txt"}) {
		t.Errorf("sha256sum asked about %q, want only the changed file", got)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "b.txt")); string(b) != "bbbb" {
		t.Errorf("b.txt = %q", b)
	}
}
