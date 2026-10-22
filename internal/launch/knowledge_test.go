package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"labmill-client/internal/broker"
	"labmill-client/internal/knowledge"
	"labmill-client/internal/skill"
)

func genericWith(body string) fstest.MapFS {
	return fstest.MapFS{
		"alpha/SKILL.md": {Data: []byte("---\nname: alpha\ndescription: Alpha skill.\n---\n" + body + "\n")},
	}
}

func buildBundle(t *testing.T, body string) (*knowledge.Bundle, []byte) {
	t.Helper()
	st := fixtureSite(t)
	b, err := knowledge.Build(genericWith(body), st)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := b.Marshal(&buf); err != nil {
		t.Fatal(err)
	}
	return b, buf.Bytes()
}

// fakeKnowledge serves the knowledge endpoints for one bundle.
type fakeKnowledge struct {
	manifest knowledge.Manifest
	raw      []byte
	status   int // when not 0, every request gets it
	current  string
	headers  []http.Header
	bundles  atomic.Int32
	mu       sync.Mutex
}

func (f *fakeKnowledge) serve(t *testing.T) *broker.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		if f.current != "" {
			w.Header().Set(broker.SkillsCurrentHeader, f.current)
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		switch r.URL.Path {
		case "/v1/knowledge/manifest":
			json.NewEncoder(w).Encode(f.manifest)
		case "/v1/knowledge/bundle":
			// Like the broker: the ETag is the version, and a matching
			// If-None-Match gets a 304.
			etag := `"` + r.URL.Query().Get("version") + `"`
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			f.bundles.Add(1)
			w.Write(f.raw)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := broker.New(broker.Config{BaseURL: srv.URL, Site: "example.edu", SkillsVersion: "embedded",
		Token: func(context.Context) (string, error) { return "tok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSyncDownloadsVerifiesAndNextStartUsesIt(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old body")
	newer, raw := buildBundle(t, "new body")
	f := &fakeKnowledge{manifest: newer.Manifest(), raw: raw}
	c := f.serve(t)
	dir := filepath.Join(t.TempDir(), "knowledge")

	got, err := syncKnowledge(context.Background(), c, st, emb.Version, dir)
	if err != nil || !got {
		t.Fatalf("sync = %v, %v", got, err)
	}
	// Stored with private permissions, current points at it.
	if runtime.GOOS != "windows" {
		for _, p := range []string{dir, filepath.Join(dir, newer.Version)} {
			if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o700 {
				t.Errorf("%s mode %v", p, fi.Mode().Perm())
			}
		}
		for _, p := range []string{filepath.Join(dir, newer.Version, "bundle.tgz"), filepath.Join(dir, "current")} {
			if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
				t.Errorf("%s mode %v", p, fi.Mode().Perm())
			}
		}
	}
	// The next start uses it.
	base, logs := chooseSkillBase(dir, st, genericWith("old body"), emb.Version)
	if !base.Cached || base.Version != newer.Version || len(logs) != 0 {
		t.Fatalf("base = %+v logs %v", base, logs)
	}
	set, _, err := skill.LoadWithOverride(base.FS, filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := set.Get("alpha")
	if !ok {
		t.Fatal("alpha missing")
	}
	if txt, _ := set.LoadSkillResult(sk.Name); !strings.Contains(txt, "new body") {
		t.Errorf("skill text %q", txt)
	}
	// A second sync finds the cached version: no new download.
	if got, err := syncKnowledge(context.Background(), c, st, emb.Version, dir); err != nil || got || f.bundles.Load() != 1 {
		t.Errorf("second sync = %v, %v, downloads %d", got, err, f.bundles.Load())
	}
	// The headers carried the site and the version in use.
	if h := f.headers[0]; h.Get(broker.SiteHeader) != "example.edu" || h.Get(broker.SkillsHeader) != "embedded" {
		t.Errorf("headers %v", h)
	}
}

func TestSyncNoDownloadWhenVersionMatchesEmbedded(t *testing.T) {
	st := fixtureSite(t)
	emb, raw := buildBundle(t, "same")
	f := &fakeKnowledge{manifest: emb.Manifest(), raw: raw}
	c := f.serve(t)
	dir := filepath.Join(t.TempDir(), "knowledge")
	if got, err := syncKnowledge(context.Background(), c, st, emb.Version, dir); err != nil || got || f.bundles.Load() != 0 {
		t.Fatalf("sync = %v, %v, downloads %d", got, err, f.bundles.Load())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("cache folder created without a download")
	}
}

func TestSyncMatchingEmbeddedClearsStaleCurrent(t *testing.T) {
	st := fixtureSite(t)
	emb, raw := buildBundle(t, "embedded")
	old, oldRaw := buildBundle(t, "cached")
	dir := filepath.Join(t.TempDir(), "knowledge")
	if err := storeBundle(dir, old.Version, emb.Version, oldRaw); err != nil {
		t.Fatal(err)
	}
	f := &fakeKnowledge{manifest: emb.Manifest(), raw: raw}
	if _, err := syncKnowledge(context.Background(), f.serve(t), st, emb.Version, dir); err != nil {
		t.Fatal(err)
	}
	base, _ := chooseSkillBase(dir, st, genericWith("embedded"), emb.Version)
	if base.Cached {
		t.Error("a stale cached bundle still wins over the embedded copy")
	}
	if _, err := os.Stat(filepath.Join(dir, old.Version, "bundle.tgz")); err != nil {
		t.Error("old version was removed")
	}
}

func TestSyncRefusesCorruptedBundle(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old")
	newer, raw := buildBundle(t, "new")
	for name, bad := range map[string][]byte{
		"garbage":   []byte("not a gzip"),
		"truncated": raw[:len(raw)/2],
	} {
		f := &fakeKnowledge{manifest: newer.Manifest(), raw: bad}
		dir := filepath.Join(t.TempDir(), "knowledge")
		if got, err := syncKnowledge(context.Background(), f.serve(t), st, emb.Version, dir); err == nil || got {
			t.Errorf("%s: sync = %v, %v", name, got, err)
		}
		base, _ := chooseSkillBase(dir, st, genericWith("old"), emb.Version)
		if base.Cached || base.Version != emb.Version {
			t.Errorf("%s: base = %+v", name, base)
		}
	}
}

func TestSyncRefusesWrongFileChecksum(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old")
	newer, raw := buildBundle(t, "new")
	m := newer.Manifest()
	m.Files[0].SHA256 = strings.Repeat("0", 64)
	f := &fakeKnowledge{manifest: m, raw: raw}
	dir := filepath.Join(t.TempDir(), "knowledge")
	if got, err := syncKnowledge(context.Background(), f.serve(t), st, emb.Version, dir); err == nil || got {
		t.Fatalf("sync = %v, %v", got, err)
	}
	// A bundle whose content differs from the version in the manifest.
	other, otherRaw := buildBundle(t, "other")
	f = &fakeKnowledge{manifest: newer.Manifest(), raw: otherRaw}
	if got, err := syncKnowledge(context.Background(), f.serve(t), st, emb.Version, dir); err == nil || got || other.Version == newer.Version {
		t.Fatalf("mismatched bundle: %v, %v", got, err)
	}
	// A manifest for another site, or with a path-like version.
	m = newer.Manifest()
	m.Site = "other.edu"
	f = &fakeKnowledge{manifest: m, raw: raw}
	if _, err := syncKnowledge(context.Background(), f.serve(t), st, emb.Version, dir); err == nil {
		t.Error("manifest of another site accepted")
	}
	m = newer.Manifest()
	m.Version = "../../etc"
	f = &fakeKnowledge{manifest: m, raw: raw}
	if _, err := syncKnowledge(context.Background(), f.serve(t), st, emb.Version, dir); err == nil {
		t.Error("path-like version accepted")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("cache written for a refused bundle")
	}
}

func TestSyncOldBrokerIsNotAnError(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old")
	f := &fakeKnowledge{status: http.StatusNotFound}
	dir := filepath.Join(t.TempDir(), "knowledge")
	if got, err := syncKnowledge(context.Background(), f.serve(t), st, emb.Version, dir); err != nil || got {
		t.Fatalf("sync = %v, %v", got, err)
	}
}

func TestCachedBundleTamperedFallsBackWithLogLine(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old")
	newer, raw := buildBundle(t, "new")
	dir := filepath.Join(t.TempDir(), "knowledge")
	if err := storeBundle(dir, newer.Version, emb.Version, raw); err != nil {
		t.Fatal(err)
	}
	_, other := buildBundle(t, "tampered")
	if err := os.WriteFile(filepath.Join(dir, newer.Version, "bundle.tgz"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	base, logs := chooseSkillBase(dir, st, genericWith("old"), emb.Version)
	if base.Cached || base.Version != emb.Version || len(logs) != 1 {
		t.Fatalf("base = %+v logs %v", base, logs)
	}
	// A cache fetched against another embedded version (the Labmill binary
	// was upgraded since) is ignored quietly.
	if err := storeBundle(dir, newer.Version, "older-binary", raw); err != nil {
		t.Fatal(err)
	}
	if base, logs := chooseSkillBase(dir, st, genericWith("old"), emb.Version); base.Cached || len(logs) != 0 {
		t.Errorf("other site's cache: %+v %v", base, logs)
	}
}

func TestOverrideWinsOverCachedBundle(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old")
	newer, raw := buildBundle(t, "from the broker")
	dir := filepath.Join(t.TempDir(), "knowledge")
	if err := storeBundle(dir, newer.Version, emb.Version, raw); err != nil {
		t.Fatal(err)
	}
	over := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(over, "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(over, "alpha", "SKILL.md"), []byte("---\nname: alpha\ndescription: Mine.\n---\nmy own text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, _ := chooseSkillBase(dir, st, genericWith("old"), emb.Version)
	if !base.Cached {
		t.Fatal("cached bundle not chosen")
	}
	set, _, err := skill.LoadWithOverride(base.FS, over)
	if err != nil {
		t.Fatal(err)
	}
	if txt, _ := set.LoadSkillResult("alpha"); !strings.Contains(txt, "my own text") || strings.Contains(txt, "from the broker") {
		t.Errorf("override lost: %q", txt)
	}
	if !overrideInUse(over) || overrideInUse(filepath.Join(over, "nope")) {
		t.Error("overrideInUse wrong")
	}
}

func TestKnowledgeNotesOncePerSession(t *testing.T) {
	var lines []string
	n := &knowledgeNotes{notice: func(s string) { lines = append(lines, s) }, inUse: "v1", overDir: "/home/me/.config/labmill/skills"}
	n.current("v1") // same: silent
	n.current("v2")
	n.current("v3")
	n.override()
	n.override()
	if len(lines) != 2 || !strings.Contains(lines[0], "Updated cluster know-how is available") || lines[1] != "Using your own modified skills from /home/me/.config/labmill/skills." {
		t.Errorf("notices = %q", lines)
	}
}

func TestStartKnowledgeSyncWaitsForToken(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old")
	newer, raw := buildBundle(t, "new")
	f := &fakeKnowledge{manifest: newer.Manifest(), raw: raw}
	c := f.serve(t)
	dir := filepath.Join(t.TempDir(), "knowledge")
	orig, origPoll := knowledgeCacheDir, knowledgePollEvery
	knowledgeCacheDir = func() (string, error) { return dir, nil }
	knowledgePollEvery = time.Millisecond
	t.Cleanup(func() { knowledgeCacheDir, knowledgePollEvery = orig, origPoll })

	var ready atomic.Bool
	logged := make(chan string, 4)
	startKnowledgeSync(context.Background(), c, st, emb.Version, ready.Load, func(s string) { logged <- s })
	select {
	case s := <-logged:
		t.Fatalf("synced before a token was there: %s", s)
	case <-time.After(30 * time.Millisecond):
	}
	ready.Store(true)
	select {
	case s := <-logged:
		if !strings.Contains(s, "downloaded") {
			t.Errorf("log = %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no sync after the token appeared")
	}
	if base, _ := chooseSkillBase(filepath.Join(dir, st.Domain), st, genericWith("old"), emb.Version); !base.Cached {
		t.Error("downloaded bundle not usable")
	}
}

// Odd `current` contents (hand-edited, legacy, CRLF, hostile) must never use
// the cache, never panic and never escape the cache folder.
func TestOddCurrentFileFallsBack(t *testing.T) {
	st := fixtureSite(t)
	emb, _ := buildBundle(t, "old")
	cases := []struct {
		name, current string
		wantLog       bool
	}{
		{"legacy one line", "v\n", false},
		{"empty", "", false},
		{"garbage", "garbage", false},
		{"crlf", "v\r\n" + emb.Version + "\r\n", true},
		{"traversal", "../x\n" + emb.Version + "\n", true},
		{"traversal, other embedded", "../x\nE\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "current"), []byte(c.current), 0o600); err != nil {
				t.Fatal(err)
			}
			b, err := loadCachedBundle(dir, st, emb.Version)
			if b != nil {
				t.Fatalf("bundle used: %+v", b)
			}
			if (err != nil) != c.wantLog {
				t.Errorf("err = %v, want error %v", err, c.wantLog)
			}
			base, logs := chooseSkillBase(dir, st, genericWith("old"), emb.Version)
			if base.Cached || base.Version != emb.Version {
				t.Errorf("base = %+v", base)
			}
			if (len(logs) != 0) != c.wantLog {
				t.Errorf("logs = %v, want log line %v", logs, c.wantLog)
			}
		})
	}
}
