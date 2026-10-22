// Package knowledge builds the bundle of cluster know-how that the client and
// the broker share: the generic skills plus one site's skill, with a content
// hash as its version, a deterministic gzip'd tar form for transport, and the
// byte-stable Markdown index the broker adds to the model's prompt. Both
// binaries build the same bundle from the same inputs, so they agree byte for
// byte. The design is in docs/dev/broker-knowledge.md.
package knowledge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"testing/fstest"
	"time"

	"go.yaml.in/yaml/v3"

	"labmill-client/internal/site"
	"labmill-client/internal/skill"
)

// Bundle is one site's complete knowledge: the generic skills plus that
// site's skill, exactly the tree Site.SkillsFS builds.
type Bundle struct {
	// Site is the site domain; site.GenericDomain for the generic profile.
	Site string
	// Version is Hash(Files).
	Version string
	// Files maps slash paths as in Site.SkillsFS, e.g. "slurm-user/SKILL.md"
	// or "site-example-edu/bigcluster/References/Slurm.md", to contents.
	Files map[string][]byte
}

// Manifest describes a bundle without its contents.
type Manifest struct {
	Site    string `json:"site"`
	Version string `json:"version"`
	// Index says the broker adds the index block to messages.
	Index bool `json:"index"`
	// Files are sorted by Path.
	Files []FileInfo `json:"files"`
}

// FileInfo is one file of a Manifest.
type FileInfo struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // hex
}

// Build walks s.SkillsFS(generic), which holds the generic skills and only
// that one site's skill, and returns the bundle. Nothing else can enter it.
func Build(generic fs.FS, s site.Site) (*Bundle, error) {
	fsys, err := s.SkillsFS(generic)
	if err != nil {
		return nil, fmt.Errorf("build knowledge bundle: %w", err)
	}
	files := map[string][]byte{}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return fmt.Errorf("%s: contains a NUL byte", p)
		}
		files[p] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("build knowledge bundle: %w", err)
	}
	return &Bundle{Site: s.Domain, Version: Hash(files), Files: files}, nil
}

func sortedPaths(files map[string][]byte) []string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// Hash returns the hex SHA-256 over the files sorted by path, each fed as the
// path, a NUL byte, the content and a NUL byte. Map order never changes it;
// one changed byte or path does.
func Hash(files map[string][]byte) string {
	h := sha256.New()
	for _, p := range sortedPaths(files) {
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(files[p])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FS returns a read-only view of the files, for skill.LoadWithOverride.
func (b *Bundle) FS() fs.FS {
	m := fstest.MapFS{}
	for p, data := range b.Files {
		m[p] = &fstest.MapFile{Data: data, Mode: 0o444}
	}
	return m
}

// Manifest returns the bundle's manifest (Index is true).
func (b *Bundle) Manifest() Manifest {
	m := Manifest{Site: b.Site, Version: b.Version, Index: true, Files: []FileInfo{}}
	for _, p := range sortedPaths(b.Files) {
		sum := sha256.Sum256(b.Files[p])
		m.Files = append(m.Files, FileInfo{Path: p, Size: int64(len(b.Files[p])), SHA256: hex.EncodeToString(sum[:])})
	}
	return m
}

// Marshal writes the bundle as a deterministic gzip'd tar: entries sorted by
// path, regular files only, mode 0644, mtime 0 (the Unix epoch), uid/gid 0,
// empty user and group names, and a gzip header without name or time at a
// fixed compression level. The same bundle always gives the same bytes. The
// bundle's site and version are not stored; Unmarshal recomputes the version.
func (b *Bundle) Marshal(w io.Writer) error {
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}
	tw := tar.NewWriter(zw)
	for _, p := range sortedPaths(b.Files) {
		data := b.Files[p]
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     p,
			Size:     int64(len(data)),
			Mode:     0o644,
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("marshal bundle: %s: %w", p, err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("marshal bundle: %s: %w", p, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("marshal bundle: %w", err)
	}
	return nil
}

// Unmarshal reads a bundle written by Marshal. It refuses paths that are
// absolute, hold ".." or backslashes, entries that aren't regular files,
// duplicate paths, and a total of file bytes over maxBytes. It recomputes
// Version; the caller compares it with the version it expects. Site is left
// empty: the manifest carries it.
func Unmarshal(r io.Reader, maxBytes int64) (*Bundle, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("read bundle: %w", err)
	}
	defer zr.Close()
	// Headers and padding count too, so bound the stream itself loosely.
	capped := &capReader{r: zr, left: 2*maxBytes + 1<<20}
	tr := tar.NewReader(capped)
	files := map[string][]byte{}
	var total int64
	for {
		hdr, err := tr.Next()
		if capped.hit {
			return nil, fmt.Errorf("read bundle: more than %d bytes", maxBytes)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle: %w", err)
		}
		name := hdr.Name
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("read bundle: %q is not a regular file", name)
		}
		if strings.Contains(name, `\`) || strings.HasPrefix(name, "/") || name == "." || !fs.ValidPath(name) {
			return nil, fmt.Errorf("read bundle: unsafe path %q", name)
		}
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("read bundle: duplicate path %q", name)
		}
		total += hdr.Size
		if hdr.Size < 0 || total > maxBytes {
			return nil, fmt.Errorf("read bundle: more than %d bytes", maxBytes)
		}
		data, err := io.ReadAll(tr)
		if capped.hit {
			return nil, fmt.Errorf("read bundle: more than %d bytes", maxBytes)
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle: %q: %w", name, err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil, fmt.Errorf("read bundle: %q contains a NUL byte", name)
		}
		files[name] = data
	}
	// Drain what follows the tar end marker so gzip checks its CRC.
	if _, err := io.Copy(io.Discard, capped); err != nil && !capped.hit {
		return nil, fmt.Errorf("read bundle: %w", err)
	}
	if capped.hit {
		return nil, fmt.Errorf("read bundle: more than %d bytes", maxBytes)
	}
	return &Bundle{Version: Hash(files), Files: files}, nil
}

// capReader reads at most left bytes from r and records whether more were
// available, so Unmarshal can fail explicitly instead of seeing a plain EOF.
type capReader struct {
	r    io.Reader
	left int64
	hit  bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// Probe one byte: more data means the cap was hit.
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			c.hit = true
			return 0, errors.New("bundle exceeds the size cap")
		}
		return 0, err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// dropKeys removes the named top-level keys from a YAML document and
// re-marshals it (comments dropped, key order kept). A document with nothing
// left gives "".
func dropKeys(raw []byte, drop ...string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", nil
	}
	m := doc.Content[0]
	var kept []*yaml.Node
	for i := 0; i+1 < len(m.Content); i += 2 {
		skip := false
		for _, d := range drop {
			if m.Content[i].Value == d {
				skip = true
			}
		}
		if !skip {
			kept = append(kept, m.Content[i], m.Content[i+1])
		}
	}
	if len(kept) == 0 {
		return "", nil
	}
	clean := &yaml.Node{Kind: yaml.MappingNode, Tag: m.Tag, Content: kept}
	stripComments(clean)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(clean); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// stripComments clears every comment in n's tree.
func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}

// Index returns the Markdown index block the broker puts in the prompt: the
// version, the institution, every skill with its description, then Site.yaml
// (without device_policy, forbidden_terms, broker_url and broker) and each Cluster.yaml (without
// forbidden_terms), folders sorted. Equal input gives identical bytes, so the
// prompt cache can hold it.
func Index(b *Bundle, s site.Site) (string, error) {
	set, err := skill.Load(b.FS(), "bundle")
	if err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	var sb strings.Builder
	ver := b.Version
	if len(ver) > 12 {
		ver = ver[:12]
	}
	fmt.Fprintf(&sb, "# Cluster know-how index (version %s)\n\n", ver)
	fmt.Fprintf(&sb, "Institution: %s (%s).", s.Name, s.Domain)
	if s.DocsURL != "" {
		fmt.Fprintf(&sb, " Documentation: %s.", s.DocsURL)
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "Full text: load_skill / read_reference. The site skill is %s.\n\n", s.Skill)
	sb.WriteString("## Skills\n")
	skills := set.List()
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	for _, k := range skills {
		fmt.Fprintf(&sb, "- %s: %s\n", k.Name, k.Description)
	}
	if raw := s.RawYAML(); len(raw) > 0 {
		out, err := dropKeys(raw, "device_policy", "forbidden_terms", "broker_url", "broker")
		if err != nil {
			return "", fmt.Errorf("index: Site.yaml: %w", err)
		}
		if out != "" {
			fmt.Fprintf(&sb, "\n## Site profile (Site.yaml)\n%s\n", out)
		}
	}
	folders := append([]site.ClusterFolder(nil), s.Folders...)
	sort.Slice(folders, func(i, j int) bool { return folders[i].Dir < folders[j].Dir })
	for _, f := range folders {
		raw := f.RawYAML()
		if len(raw) == 0 {
			continue
		}
		out, err := dropKeys(raw, "forbidden_terms")
		if err != nil {
			return "", fmt.Errorf("index: %s/Cluster.yaml: %w", f.Dir, err)
		}
		if out != "" {
			fmt.Fprintf(&sb, "\n## Cluster %s (Cluster.yaml)\n%s\n", f.Dir, out)
		}
	}
	return sb.String(), nil
}
