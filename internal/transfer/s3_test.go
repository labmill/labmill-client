package transfer

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	rfshash "github.com/rclone/rclone/fs/hash"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/gofakes3"
	"github.com/rclone/gofakes3/s3mem"

	"labmill-client/internal/store"
)

func TestS3RemoteString(t *testing.T) {
	tests := []struct {
		r    S3Remote
		want string
	}{
		{S3Remote{Bucket: "b"}, "s3://b"},
		{S3Remote{Bucket: "b", Prefix: "p", Dir: "labmill/job"}, "s3://b/p/labmill/job"},
		{S3Remote{Bucket: "b", Dir: "d/"}, "s3://b/d"},
	}
	for _, tt := range tests {
		if got := tt.r.String(); got != tt.want {
			t.Errorf("%+v: got %q, want %q", tt.r, got, tt.want)
		}
	}
}

func TestS3SSEOptions(t *testing.T) {
	if got := (S3Remote{Bucket: "b"}).sseOptions(); len(got) != 0 {
		t.Errorf("no SSE: %v", got)
	}
	got := S3Remote{Bucket: "b", SSE: "AES256", KMSKeyID: "ignored"}.sseOptions()
	if got["server_side_encryption"] != "AES256" || len(got) != 1 {
		t.Errorf("AES256: %v", got)
	}
	got = S3Remote{Bucket: "b", SSE: "aws:kms", KMSKeyID: "alias/lab"}.sseOptions()
	if got["server_side_encryption"] != "aws:kms" || got["sse_kms_key_id"] != "alias/lab" {
		t.Errorf("kms: %v", got)
	}
}

func TestS3RootPath(t *testing.T) {
	good := map[string]S3Remote{
		"b":           {Bucket: "b"},
		"b/p/d/e":     {Bucket: "b", Prefix: "/p/", Dir: "d/e"},
		"b/labmill/j": {Bucket: "b", Dir: "labmill/j"},
	}
	for want, r := range good {
		if got, err := r.rootPath(); err != nil || got != want {
			t.Errorf("%+v: %q, %v; want %q", r, got, err, want)
		}
	}
	for _, r := range []S3Remote{{}, {Bucket: " "}, {Bucket: "a/b"}, {Bucket: "b", Dir: "../x"}, {Bucket: "b", Prefix: "p/../q"}, {Bucket: "b", Dir: "a//b"}} {
		if _, err := r.rootPath(); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

// fakeS3 is an in-process S3 (gofakes3, in memory) on 127.0.0.1. It records
// the access key id of every request, so tests can tell which credentials
// were used.
type fakeS3 struct {
	be  *s3mem.Backend
	url string
	mu  sync.Mutex
	ids map[string]bool
	// fakeETag makes object responses carry an ETag that looks like an MD5
	// but isn't one, as SSE-KMS buckets do.
	fakeETag bool
	// corruptBody flips a byte of every PUT body after the client set
	// Content-MD5, as a damaged transfer would.
	corruptBody bool
	plainHead   bool // with fakeETag: don't add the KMS header
	puts        int  // PUT object / UploadPart requests seen
	noMD5       int  // of those, ones without Content-Md5
}

// serveKMS buffers h's response to r and makes object responses look like
// those of an SSE-KMS bucket: an ETag that is not the MD5 and the
// x-amz-server-side-encryption header.
func serveKMS(h http.Handler, w http.ResponseWriter, r *http.Request, plainHead bool) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	if r.URL.Path != "/" && strings.Count(strings.Trim(r.URL.Path, "/"), "/") >= 1 {
		if w.Header().Get("Etag") != "" {
			w.Header().Set("Etag", `"0123456789abcdef0123456789abcdef"`)
		}
		if rec.Code < 300 && !plainHead {
			w.Header().Set("X-Amz-Server-Side-Encryption", "aws:kms")
		}
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}

func newFakeS3(t *testing.T, buckets ...string) *fakeS3 {
	t.Helper()
	f := &fakeS3{be: s3mem.New(), ids: map[string]bool{}}
	for _, b := range buckets {
		if err := f.be.CreateBucket(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	h := gofakes3.New(f.be).Server()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); strings.Contains(a, "Credential=") {
			id := strings.SplitN(strings.SplitN(a, "Credential=", 2)[1], "/", 2)[0]
			f.mu.Lock()
			f.ids[id] = true
			f.mu.Unlock()
		}
		if r.Method == http.MethodPut && strings.Count(strings.Trim(r.URL.Path, "/"), "/") >= 1 {
			f.mu.Lock()
			f.puts++
			if r.Header.Get("Content-Md5") == "" {
				f.noMD5++
			}
			f.mu.Unlock()
			if f.corruptBody {
				b, _ := io.ReadAll(r.Body)
				if len(b) > 0 {
					b[0] ^= 0xff
				}
				r.Body = io.NopCloser(strings.NewReader(string(b)))
			}
		}
		if f.fakeETag {
			serveKMS(h, w, r, f.plainHead)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// keys returns every key in bucket with its size.
func (f *fakeS3) keys(t *testing.T, bucket string) map[string]int64 {
	t.Helper()
	l, err := f.be.ListBucket(context.Background(), bucket, &gofakes3.Prefix{}, gofakes3.ListBucketPage{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, c := range l.Contents {
		out[c.Key] = c.Size
	}
	return out
}

func (f *fakeS3) put(t *testing.T, bucket, key, content string) {
	t.Helper()
	_, err := f.be.PutObject(context.Background(), bucket, key, nil, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fakeS3) get(t *testing.T, bucket, key string) string {
	t.Helper()
	o, err := f.be.GetObject(context.Background(), bucket, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Contents.Close()
	b, _ := io.ReadAll(o.Contents)
	return string(b)
}

// s3Creds isolates the AWS SDK from the developer's real ~/.aws and
// environment and supplies fake credentials: static ones in the environment,
// or (profile != "") a profile "lab" with other keys in a temp credentials
// file.
func s3Creds(t *testing.T, profile bool) {
	t.Helper()
	dir := t.TempDir()
	cfg, cred := filepath.Join(dir, "config"), filepath.Join(dir, "credentials")
	os.WriteFile(cfg, nil, 0o600)
	os.WriteFile(cred, []byte("[lab]\naws_access_key_id = LABKEY\naws_secret_access_key = labsecret\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", cred)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_REGION", "us-west-2")
	if profile {
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	} else {
		t.Setenv("AWS_ACCESS_KEY_ID", "ENVKEY")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "envsecret")
	}
}

func s3Tree(t *testing.T) (root string) {
	root = t.TempDir()
	write(t, filepath.Join(root, "data", "a.csv"), "x,y\n1,2\n")
	write(t, filepath.Join(root, "data", "deep", "b.bin"), strings.Repeat("b", 100000))
	write(t, filepath.Join(root, "data", "unselected.txt"), "must not be uploaded")
	return root
}

func TestUploadDownloadS3(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	// Neighbours outside the prefix must stay untouched and unread.
	f.put(t, "bkt", "other/keep.txt", "keep")
	f.put(t, "bkt", "pre/job1x/neighbor.txt", "near miss")

	root := s3Tree(t)
	entries, _, err := BuildManifest(context.Background(), root, []string{"data/a.csv", "data/deep"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dst := S3Remote{Bucket: "bkt", Prefix: "pre", Dir: "job1", Endpoint: f.url, Region: "us-west-2"}
	var seen []Progress
	if err := UploadS3(context.Background(), root, entries, dst, func(p Progress) { seen = append(seen, p) }); err != nil {
		t.Fatalf("UploadS3: %v", err)
	}
	got := f.keys(t, "bkt")
	want := map[string]int64{
		"pre/job1/data/a.csv":      8,
		"pre/job1/data/deep/b.bin": 100000,
		"other/keep.txt":           4,
		"pre/job1x/neighbor.txt":   9,
	}
	if len(got) != len(want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("key %s size %d, want %d", k, got[k], n)
		}
	}
	if f.get(t, "bkt", "pre/job1/data/a.csv") != "x,y\n1,2\n" {
		t.Error("content mismatch")
	}
	if len(seen) == 0 {
		t.Fatal("no progress")
	}
	var prev Progress
	for _, p := range seen {
		if p.Bytes < prev.Bytes || p.Files < prev.Files {
			t.Errorf("progress went backwards: %+v after %+v", p, prev)
		}
		prev = p
	}
	if last := seen[len(seen)-1]; last.Bytes != last.TotalBytes || last.Files != 2 || last.TotalFiles != 2 {
		t.Errorf("final progress = %+v", last)
	}
	if !f.ids["ENVKEY"] || len(f.ids) != 1 {
		t.Errorf("access keys used = %v", f.ids)
	}

	// Results come back: a file and a directory; names inside the prefix only.
	f.put(t, "bkt", "pre/job1/results/model.pt", "weights")
	f.put(t, "bkt", "pre/job1/results/sub/log.txt", "log")
	out := t.TempDir()
	seen = nil
	written, err := DownloadS3(context.Background(), dst, []string{"results", "data/a.csv"}, out, func(p Progress) { seen = append(seen, p) })
	if err != nil {
		t.Fatalf("DownloadS3: %v", err)
	}
	if len(written) != 3 {
		t.Errorf("written = %v", written)
	}
	for rel, content := range map[string]string{"results/model.pt": "weights", "results/sub/log.txt": "log", "data/a.csv": "x,y\n1,2\n"} {
		b, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(rel)))
		if err != nil || string(b) != content {
			t.Errorf("%s = %q, %v", rel, b, err)
		}
	}
	prev = Progress{}
	for _, p := range seen {
		if p.Bytes < prev.Bytes || p.Files < prev.Files {
			t.Errorf("download progress went backwards: %+v after %+v", p, prev)
		}
		prev = p
	}
	if last := seen[len(seen)-1]; last.Files != 3 || last.Bytes != last.TotalBytes {
		t.Errorf("final download progress = %+v", last)
	}
	// Existing identical files are skipped, a changed one refuses everything.
	if _, err := DownloadS3(context.Background(), dst, []string{"results"}, out, nil); err != nil {
		t.Errorf("re-download of identical files: %v", err)
	}
	os.WriteFile(filepath.Join(out, "results", "model.pt"), []byte("different"), 0o644)
	if _, err := DownloadS3(context.Background(), dst, []string{"results"}, out, nil); err == nil || !strings.Contains(err.Error(), "would be replaced") {
		t.Errorf("overwrite not refused: %v", err)
	}
	// Nothing outside the prefix is reachable.
	for _, p := range []string{"../job1x/neighbor.txt", "../../other/keep.txt", "/other/keep.txt"} {
		if _, err := DownloadS3(context.Background(), dst, []string{p}, t.TempDir(), nil); err == nil {
			t.Errorf("download of %q succeeded", p)
		}
	}
	// A directory name that is only a string prefix of a neighbour finds nothing.
	if w, err := DownloadS3(context.Background(), dst, []string{"nope"}, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "nothing found at that path") {
		t.Errorf("missing path: written %v, err %v", w, err)
	}
	// One missing path among found ones fails the whole request.
	if _, err := DownloadS3(context.Background(), dst, []string{"results", "nope"}, t.TempDir(), nil); err == nil {
		t.Error("partial request with a missing path succeeded")
	}
}

func TestUploadS3RefusesChangedFile(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	root := s3Tree(t)
	entries, _, err := BuildManifest(context.Background(), root, []string{"data/a.csv"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "data", "a.csv"), "x,y\n9,9\n") // same size, other content
	err = UploadS3(context.Background(), root, entries, S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url, Region: "us-west-2"}, nil)
	if err == nil || !strings.Contains(err.Error(), "file changed since you approved the upload") {
		t.Fatalf("err = %v", err)
	}
	if k := f.keys(t, "bkt"); len(k) != 0 {
		t.Errorf("objects written for a changed file: %v", k)
	}
}

func TestUploadS3OnlyManifestAndCleanPaths(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	root := s3Tree(t)
	bad := []string{"../x", "/abs"}
	for _, p := range bad {
		err := UploadS3(context.Background(), root, entriesFor(p), S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url}, nil)
		if err == nil {
			t.Errorf("manifest path %q accepted", p)
		}
	}
	if k := f.keys(t, "bkt"); len(k) != 0 {
		t.Errorf("objects = %v", k)
	}
}

func TestS3Profile(t *testing.T) {
	s3Creds(t, true)
	f := newFakeS3(t, "bkt")
	root := s3Tree(t)
	entries, _, _ := BuildManifest(context.Background(), root, []string{"data/a.csv"}, nil)
	dst := S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url, Region: "us-west-2", Profile: "lab"}
	if err := UploadS3(context.Background(), root, entries, dst, nil); err != nil {
		t.Fatal(err)
	}
	if !f.ids["LABKEY"] || len(f.ids) != 1 {
		t.Errorf("access keys used = %v, want only the profile's", f.ids)
	}
	// An unknown profile is a plain-language sign-in problem.
	dst.Profile = "nope"
	err := UploadS3(context.Background(), root, entries, dst, nil)
	if err == nil || !strings.Contains(err.Error(), "I couldn't sign in to AWS") {
		t.Errorf("unknown profile err = %v", err)
	}
}

func TestS3MissingBucketAndEmptyBucket(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t)
	root := s3Tree(t)
	entries, _, _ := BuildManifest(context.Background(), root, []string{"data/a.csv"}, nil)
	err := UploadS3(context.Background(), root, entries, S3Remote{Bucket: "missing", Dir: "j", Endpoint: f.url}, nil)
	if err == nil || !strings.Contains(err.Error(), `The S3 bucket "missing" doesn't exist`) {
		t.Errorf("upload err = %v", err)
	}
	if ok, _ := f.be.BucketExists(context.Background(), "missing"); ok {
		t.Error("bucket was created")
	}
	_, err = DownloadS3(context.Background(), S3Remote{Bucket: "missing", Dir: "j", Endpoint: f.url}, []string{"x"}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), `The S3 bucket "missing" doesn't exist`) {
		t.Errorf("download err = %v", err)
	}
	if err := UploadS3(context.Background(), root, entries, S3Remote{Dir: "j", Endpoint: f.url}, nil); err == nil {
		t.Error("empty bucket accepted")
	}
	if _, err := DownloadS3(context.Background(), S3Remote{Endpoint: f.url}, []string{"x"}, t.TempDir(), nil); err == nil {
		t.Error("empty bucket accepted for download")
	}
}

func TestS3ContextCancel(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	root := s3Tree(t)
	entries, _, _ := BuildManifest(context.Background(), root, []string{"data"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		done <- UploadS3(ctx, root, entries, S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url}, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("cancelled upload succeeded")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled upload did not return")
	}
	if k := f.keys(t, "bkt"); len(k) != 0 {
		t.Errorf("objects after cancel: %v", k)
	}
}

func entriesFor(p string) []store.ManifestEntry {
	return []store.ManifestEntry{{Path: p, Size: 1, SHA256: "00"}}
}

// The stored object carries an MD5 that matches the local file, so rclone's
// post-copy hash comparison is active (not size-only).
func TestS3ObjectHasMD5(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	root := s3Tree(t)
	entries, _, _ := BuildManifest(context.Background(), root, []string{"data/deep"}, nil)
	dst := S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url}
	if err := UploadS3(context.Background(), root, entries, dst, nil); err != nil {
		t.Fatal(err)
	}
	ctx, done, err := rcloneContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	fs3, err := newS3Fs(ctx, dst, "bkt/j")
	if err != nil {
		t.Fatal(err)
	}
	o, err := fs3.NewObject(ctx, "data/deep/b.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := o.Hash(ctx, rfshash.MD5)
	sum := md5.Sum([]byte(strings.Repeat("b", 100000)))
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Errorf("MD5 = %q, %v", got, err)
	}
}

// With no region and no endpoint, the region comes from the AWS profile.
func TestS3RegionFromProfile(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	write(t, cfg, "[profile x]\nregion = eu-central-1\n")
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	ctx := context.Background()
	if got := (S3Remote{Bucket: "b", Profile: "x"}).resolveRegion(ctx); got != "eu-central-1" {
		t.Errorf("region = %q", got)
	}
	if got := (S3Remote{Bucket: "b", Profile: "x", Region: "us-west-2"}).resolveRegion(ctx); got != "us-west-2" {
		t.Errorf("explicit region = %q", got)
	}
	if got := (S3Remote{Bucket: "b", Profile: "x", Endpoint: "http://h"}).resolveRegion(ctx); got != "" {
		t.Errorf("region with endpoint = %q", got)
	}
}

// A bucket whose default encryption is SSE-KMS returns ETags that are not
// MD5s; Labmill isn't told, and uploads and downloads must still work. Every
// upload request keeps its Content-MD5, so S3 still checks the bytes.
func TestS3NonMD5ETag(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	f.fakeETag = true
	root := s3Tree(t)
	entries, _, err := BuildManifest(context.Background(), root, []string{"data/deep"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dst := S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url, Region: "us-west-2"}
	if err := UploadS3(context.Background(), root, entries, dst, nil); err != nil {
		t.Fatalf("UploadS3 with non-MD5 ETag: %v", err)
	}
	if f.puts == 0 || f.noMD5 != 0 {
		t.Errorf("%d PUTs, %d without Content-Md5", f.puts, f.noMD5)
	}
	if f.get(t, "bkt", "j/data/deep/b.bin") != strings.Repeat("b", 100000) {
		t.Error("content mismatch")
	}
	out := t.TempDir()
	if _, err := DownloadS3(context.Background(), dst, []string{"data/deep"}, out, nil); err != nil {
		t.Fatalf("DownloadS3 with non-MD5 ETag: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "data", "deep", "b.bin")); err != nil || len(b) != 100000 {
		t.Errorf("downloaded file: %d bytes, %v", len(b), err)
	}
}

// With bogus ETags the retry must not hide a body damaged in transit: S3
// (here the fake) rejects it through Content-MD5.
func TestS3NonMD5ETagCorruptBodyFails(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	f.fakeETag, f.corruptBody = true, true
	root := s3Tree(t)
	entries, _, _ := BuildManifest(context.Background(), root, []string{"data/deep"}, nil)
	dst := S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url, Region: "us-west-2"}
	if err := UploadS3(context.Background(), root, entries, dst, nil); err == nil {
		t.Fatal("upload of a corrupted body succeeded")
	}
}

// A download whose MD5 differs on an object that is not encrypted with KMS
// is corruption and must be reported, not retried away.
func TestS3DownloadMismatchWithoutKMSFails(t *testing.T) {
	s3Creds(t, false)
	f := newFakeS3(t, "bkt")
	f.put(t, "bkt", "j/x.bin", strings.Repeat("x", 5000))
	f.fakeETag = true
	f.plainHead = true // bogus ETag, but HEAD doesn't show KMS
	dst := S3Remote{Bucket: "bkt", Dir: "j", Endpoint: f.url, Region: "us-west-2"}
	_, err := DownloadS3(context.Background(), dst, []string{"x.bin"}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "md5 hashes differ") {
		t.Fatalf("err = %v, want an md5 mismatch", err)
	}
}
