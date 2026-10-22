package launch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"labmill-client/internal/broker"
	"labmill-client/internal/knowledge"
	"labmill-client/internal/site"
	"labmill-client/internal/skill"
	"labmill-client/internal/startup"
)

// The client side of the broker's cluster know-how
// (docs/dev/broker-knowledge.md): a background sync that downloads a newer
// verified bundle into the user cache, and the startup choice between the
// cached bundle and the embedded copy. A downloaded bundle is used from the
// next start, never mid-conversation.

// knowledgeMaxBytes bounds the unpacked size of a bundle.
const knowledgeMaxBytes = 32 << 20

var versionRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// knowledgeCacheDir is where bundles are cached
// (os.UserCacheDir()/labmill/knowledge); tests replace it.
var knowledgeCacheDir = func() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "labmill", "knowledge"), nil
}

// knowledgePollEvery is how often the sync waits for a token to appear.
var knowledgePollEvery = 2 * time.Second

// skillBase is the skill set Build starts from.
type skillBase struct {
	FS         fs.FS
	Version    string // the know-how version in use
	Cached     bool   // true when FS is the cached broker bundle
	Overridden bool   // the user's override folder is in use (set by Build)
}

// siteCacheDir is the cache folder of one site below root:
// root/<domain>, holding `current` and <version>/bundle.tgz.
func siteCacheDir(root, domain string) (string, error) {
	if domain == "" || domain == "." || domain == ".." || strings.ContainsAny(domain, `/\`) {
		return "", fmt.Errorf("invalid site %q for the know-how cache", domain)
	}
	return filepath.Join(root, domain), nil
}

// chooseSkillBase picks the cached, verified broker bundle named by the
// cache's `current` file when there is one, otherwise the embedded copy. The
// cache is ignored when it was fetched against another embedded version (a
// newer Labmill binary's skills win after an upgrade). Any problem with the
// cache falls back to the embedded copy and is returned as a log line, never
// as an error. dir is the site's cache folder; embedded is the embedded
// skills FS for st (site.SkillsFSAll); embeddedVersion its knowledge version
// (empty when it couldn't be computed; the cache is then not used).
//
// others are the other institutions' profiles (M45.1). The broker bundle
// carries only the home site's skill, and the knowledge version
// (knowledge.Build) is computed over the generic skills plus the home site
// skill only, so it is the version the broker and the cache know. The skills
// of the others always come from this binary: when the cached bundle is
// used, the embedded skill of each of them is added to it (the bundle wins
// for any name it has). The version therefore need not cover them: they
// change only with the binary, and a changed binary is read fresh at every
// start.
func chooseSkillBase(dir string, st site.Site, embedded fs.FS, embeddedVersion string, others ...site.Site) (skillBase, []string) {
	base := skillBase{FS: embedded, Version: embeddedVersion}
	b, err := loadCachedBundle(dir, st, embeddedVersion)
	if err != nil {
		return base, []string{"cached cluster know-how not used: " + err.Error()}
	}
	if b == nil {
		return base, nil
	}
	return skillBase{FS: overlaySkills(b.FS(), embedded, others), Version: b.Version, Cached: true}, nil
}

// overlaySkills returns bundle with the skill directories of others copied
// from embedded, for every skill the bundle lacks.
func overlaySkills(bundle, embedded fs.FS, others []site.Site) fs.FS {
	var missing []string
	for _, o := range others {
		if o.Skill == "" {
			continue
		}
		if _, err := fs.Stat(bundle, o.Skill); err != nil {
			missing = append(missing, o.Skill)
		}
	}
	if len(missing) == 0 {
		return bundle
	}
	out := fstest.MapFS{}
	copyTree := func(src fs.FS, root string) {
		_ = fs.WalkDir(src, root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if data, rerr := fs.ReadFile(src, p); rerr == nil {
				out[p] = &fstest.MapFile{Data: data, Mode: 0o444}
			}
			return nil
		})
	}
	copyTree(bundle, ".")
	for _, name := range missing {
		copyTree(embedded, name)
	}
	return out
}

// readCurrent reads the `current` pointer: line 1 is the cached version,
// line 2 the embedded version it was fetched against. An empty or missing
// file gives "".
func readCurrent(dir string) (version, embedded string, err error) {
	raw, err := os.ReadFile(filepath.Join(dir, "current"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return "", "", nil
	}
	version = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		embedded = strings.TrimSpace(lines[1])
	}
	return version, embedded, nil
}

// requireSiteSkill checks that b holds the site skill's SKILL.md (the
// generic profile has none) and that its reference files can be read: a
// SKILL.md that points at references/References files while the skill
// runtime finds none (a layout this client can't serve) is refused, so the
// client keeps its embedded copy instead of losing the references.
func requireSiteSkill(b *knowledge.Bundle, st site.Site) error {
	if st.Domain == site.GenericDomain {
		return nil
	}
	name := site.SkillName(st.Domain)
	p := name + "/SKILL.md"
	data, ok := b.Files[p]
	if !ok {
		return fmt.Errorf("the bundle lacks the site skill %s", p)
	}
	if !strings.Contains(strings.ToLower(string(data)), "references/") {
		return nil
	}
	mem := fstest.MapFS{}
	for path, content := range b.Files {
		if strings.HasPrefix(path, name+"/") {
			mem[path] = &fstest.MapFile{Data: content}
		}
	}
	set, err := skill.Load(mem, "bundle")
	if err != nil {
		return fmt.Errorf("the bundle's site skill %s can't be loaded: %w", name, err)
	}
	k, ok := set.Get(name)
	if !ok || len(k.References) == 0 {
		return fmt.Errorf("the bundle's site skill %s links to reference files that it doesn't serve", name)
	}
	return nil
}

// loadCachedBundle loads the bundle `current` names and re-verifies its
// hash and the site skill. It returns (nil, nil) when nothing is cached or
// the cache was fetched against another embedded version, and an error for a
// cache that is present but unusable.
func loadCachedBundle(dir string, st site.Site, embeddedVersion string) (*knowledge.Bundle, error) {
	version, fetchedAgainst, err := readCurrent(dir)
	if err != nil {
		return nil, err
	}
	if version == "" || embeddedVersion == "" || fetchedAgainst != embeddedVersion {
		return nil, nil
	}
	if !versionRe.MatchString(version) {
		return nil, fmt.Errorf("current names an invalid version %q", version)
	}
	raw, err := os.ReadFile(filepath.Join(dir, version, "bundle.tgz"))
	if err != nil {
		return nil, err
	}
	b, err := knowledge.Unmarshal(bytes.NewReader(raw), knowledgeMaxBytes)
	if err != nil {
		return nil, err
	}
	if b.Version != version {
		return nil, fmt.Errorf("the bundle in %s doesn't match its version", version[:12])
	}
	if err := requireSiteSkill(b, st); err != nil {
		return nil, err
	}
	b.Site = st.Domain
	return b, nil
}

// verifyBundle checks b against the manifest: version, site skill, and every
// file's size and sha256, with no extra or missing files.
func verifyBundle(m knowledge.Manifest, b *knowledge.Bundle, st site.Site) error {
	if b.Version != m.Version {
		return errors.New("the bundle's hash doesn't match the manifest version")
	}
	if len(b.Files) != len(m.Files) {
		return errors.New("the bundle's files don't match the manifest")
	}
	for _, f := range m.Files {
		data, ok := b.Files[f.Path]
		if !ok {
			return fmt.Errorf("the bundle lacks %s", f.Path)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
			return fmt.Errorf("%s doesn't match the manifest checksum", f.Path)
		}
	}
	return requireSiteSkill(b, st)
}

// writeAtomic writes data to path through a temp file in the same folder,
// synced, and a rename (0600). A failure removes only its own temp file.
func writeAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// storeBundle caches raw (the verified bundle.tgz) under dir/<version>/ and
// points `current` at it, recording the embedded version it was fetched
// against. Old versions stay in place.
func storeBundle(dir, version, embeddedVersion string, raw []byte) error {
	vdir := filepath.Join(dir, version)
	if err := os.MkdirAll(vdir, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(vdir, "bundle.tgz"), raw); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "current"), []byte(version+"\n"+embeddedVersion+"\n"))
}

// knowledgeFetcher is the part of broker.Client the sync uses.
type knowledgeFetcher interface {
	Manifest(ctx context.Context) (knowledge.Manifest, error)
	Bundle(ctx context.Context, version, ifNoneMatch string) ([]byte, error)
}

// syncKnowledge fetches the manifest and, when its version differs from the
// embedded bundle's and from the cached one (the verified `current`),
// downloads, verifies and caches the new bundle. dir is the site's cache
// folder. It reports whether a bundle was stored. An old broker (no
// endpoint) is not an error.
func syncKnowledge(ctx context.Context, f knowledgeFetcher, st site.Site, embeddedVersion, dir string) (bool, error) {
	m, err := f.Manifest(ctx)
	if errors.Is(err, broker.ErrKnowledgeUnavailable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !versionRe.MatchString(m.Version) {
		return false, fmt.Errorf("the manifest has an invalid version %q", m.Version)
	}
	if m.Site != st.Domain {
		return false, fmt.Errorf("the manifest is for site %q, not %q", m.Site, st.Domain)
	}
	cur, _, _ := readCurrent(dir)
	if cur == m.Version {
		if b, err := loadCachedBundle(dir, st, embeddedVersion); err == nil && b != nil {
			return false, nil
		}
	}
	if m.Version == embeddedVersion {
		// The broker is back at what is embedded: stop a stale cached
		// bundle from winning at the next start.
		if cur != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return false, err
			}
			return false, writeAtomic(filepath.Join(dir, "current"), nil)
		}
		return false, nil
	}
	// No If-None-Match: only versions this client doesn't have are asked
	// for, and the broker's ETag is that very version.
	raw, err := f.Bundle(ctx, m.Version, "")
	if errors.Is(err, broker.ErrKnowledgeUnavailable) || errors.Is(err, broker.ErrNotModified) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	b, err := knowledge.Unmarshal(bytes.NewReader(raw), knowledgeMaxBytes)
	if err != nil {
		return false, err
	}
	if err := verifyBundle(m, b, st); err != nil {
		return false, err
	}
	if err := storeBundle(dir, m.Version, embeddedVersion, raw); err != nil {
		return false, err
	}
	return true, nil
}

// startKnowledgeSync runs syncKnowledge in the background once a token is
// available without any dialog (ready reports that), polling until ctx ends.
// logf records the outcome; nothing is shown to the user.
func startKnowledgeSync(ctx context.Context, f knowledgeFetcher, st site.Site, embeddedVersion string, ready func() bool, logf func(string)) {
	go func() {
		root, err := knowledgeCacheDir()
		var dir string
		if err == nil {
			dir, err = siteCacheDir(root, st.Domain)
		}
		if err != nil {
			logf("cluster know-how sync skipped: " + err.Error())
			return
		}
		endWait := startup.Span("background: knowledge sync waits for a broker token")
		for !ready() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(knowledgePollEvery):
			}
		}
		endWait()
		endSync := startup.Span("background: knowledge sync (broker call)")
		got, err := syncKnowledge(ctx, f, st, embeddedVersion, dir)
		endSync()
		switch {
		case err != nil:
			if ctx.Err() == nil {
				logf("cluster know-how sync failed: " + err.Error())
			}
		case got:
			logf("downloaded newer cluster know-how; it is used from the next start")
		}
	}()
}

// knowledgeNotes shows the know-how notices, each at most once a session.
type knowledgeNotes struct {
	notice  func(string)
	inUse   string
	newer   sync.Once
	own     sync.Once
	overDir string
}

// current is broker.Config.OnSkillsCurrent.
func (n *knowledgeNotes) current(version string) {
	if n.inUse == "" || version == n.inUse {
		return
	}
	n.newer.Do(func() {
		n.notice("Updated cluster know-how is available; Labmill uses it from the next start.")
	})
}

// override shows the modified-skills notice (once).
func (n *knowledgeNotes) override() {
	n.own.Do(func() { n.notice("Using your own modified skills from " + n.overDir + ".") })
}

// overrideInUse reports whether dir is a folder with something in it.
func overrideInUse(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
