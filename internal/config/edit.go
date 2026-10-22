package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"
)

// Editing clusters.yaml in place (the desktop app's Settings page, M29).
// The edits work on the YAML node tree, so comments, key order and the keys
// that don't change survive; only the touched values are rewritten. Every
// result is checked with Parse before it is returned.

// clusterKeys is the order in which new keys are placed in a cluster's
// mapping (existing keys keep their place).
var clusterKeys = []string{"name", "type", "ssh_host", "files_host", "transfer", "timezone", "site", "s3"}

// s3Keys is the same order for the s3 block.
var s3Keys = []string{"bucket", "prefix", "region", "profile", "endpoint", "sse", "kms_key_id"}

// Backup copies the file at path to "<path>.labmill-backup-<YYYYMMDD-HHMMSS>"
// (with "-1", "-2", … appended when that name exists, so backups never
// overwrite each other; created with O_EXCL, mode 0600) and returns the
// backup's path. A missing file needs no backup: it returns "" and no error.
// A symlinked path is backed up from its target.
func Backup(path string, now time.Time) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("back up %s: %w", path, err)
	}
	base := path + ".labmill-backup-" + now.Format("20060102-150405")
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("back up %s: %w", path, err)
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return "", fmt.Errorf("back up %s: %w", path, werr)
		}
		return name, nil
	}
	return "", fmt.Errorf("back up %s: too many backups with the same name", path)
}

// WriteFile writes data (an edited clusters.yaml) to path atomically, like
// Save: temp file in the same folder, mode 0600, then rename; parent folders
// are created with mode 0700, and a symlinked path is written through to its
// target. It does not validate data.
func WriteFile(path string, data []byte) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve symlink %s: %w", path, err)
		}
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".clusters-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName) // our own temp file
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fail(err)
	}
	return nil
}

// SetSiteYAML returns data (a clusters.yaml document) with the top-level
// `site:` set to site, or removed when site is "". Everything else (comments,
// key order, the clusters) stays as it is. The result must pass Parse.
func SetSiteYAML(data []byte, site string) ([]byte, error) {
	doc, root, err := loadDoc(data)
	if err != nil {
		return nil, err
	}
	var val *yaml.Node
	if site != "" {
		val = strNode(site)
	}
	setKey(root, "site", val, []string{"site", "clusters"})
	return finish(doc)
}

// PutClusterYAML returns data with the cluster named oldName set to c, or c
// appended as a new cluster when oldName is "". In an existing cluster only
// the keys whose value changes are touched: a changed value is replaced in
// place (its comments stay), an emptied one is removed, a new one is inserted
// after the keys that precede it in the usual order (name, type, ssh_host,
// files_host, transfer, timezone, site, s3). The s3 block is
// edited the same way, or removed when c.S3 is nil. It fails when oldName is
// not configured, and when the result does not pass Parse (the caller
// validates c first, so that is a last line of defence).
func PutClusterYAML(data []byte, oldName string, c Cluster) ([]byte, error) {
	doc, root, err := loadDoc(data)
	if err != nil {
		return nil, err
	}
	seq := clustersSeq(root)
	var target *yaml.Node
	if oldName != "" {
		_, target = findCluster(seq, oldName)
		if target == nil {
			return nil, fmt.Errorf("cluster %s is no longer configured", oldName)
		}
	} else {
		target = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		seq.Content = append(seq.Content, target)
	}
	for _, kv := range []struct{ k, v string }{
		{"name", c.Name}, {"type", string(c.Type)}, {"ssh_host", c.SSHHost}, {"files_host", c.FilesHost},
		{"transfer", string(c.Transfer)}, {"timezone", c.Timezone}, {"site", c.Site},
	} {
		setScalar(target, kv.k, kv.v, clusterKeys)
	}
	if c.S3 == nil {
		setKey(target, "s3", nil, clusterKeys)
	} else {
		s3 := mapValue(target, "s3")
		if s3 == nil || s3.Kind != yaml.MappingNode {
			s3 = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setKey(target, "s3", s3, clusterKeys)
		}
		for _, kv := range []struct{ k, v string }{
			{"bucket", c.S3.Bucket}, {"prefix", c.S3.Prefix}, {"region", c.S3.Region}, {"profile", c.S3.Profile},
			{"endpoint", c.S3.Endpoint}, {"sse", c.S3.SSE}, {"kms_key_id", c.S3.KMSKeyID},
		} {
			setScalar(s3, kv.k, kv.v, s3Keys)
		}
	}
	return finish(doc)
}

// RemoveClusterYAML returns data without the cluster named name (comments
// attached to that entry go with it; everything else stays). It fails when
// name is not configured or the result does not pass Parse.
func RemoveClusterYAML(data []byte, name string) ([]byte, error) {
	doc, root, err := loadDoc(data)
	if err != nil {
		return nil, err
	}
	seq := clustersSeq(root)
	i, _ := findCluster(seq, name)
	if i < 0 {
		return nil, fmt.Errorf("cluster %s is no longer configured", name)
	}
	seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
	return finish(doc)
}

// loadDoc parses data into a document whose root is a mapping; an empty
// document becomes an empty mapping.
func loadDoc(data []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse clusters.yaml: %w", err)
	}
	if doc.Kind == 0 {
		root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errors.New("clusters.yaml has an unexpected layout")
	}
	return &doc, doc.Content[0], nil
}

// clustersSeq returns the `clusters:` sequence of root, creating it (or
// replacing a null value) when needed.
func clustersSeq(root *yaml.Node) *yaml.Node {
	v := mapValue(root, "clusters")
	if v != nil && v.Kind == yaml.SequenceNode {
		return v
	}
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	setKey(root, "clusters", seq, []string{"site", "clusters"})
	return seq
}

// findCluster returns the index and mapping of the cluster named name in
// seq, or -1 and nil.
func findCluster(seq *yaml.Node, name string) (int, *yaml.Node) {
	for i, c := range seq.Content {
		if c.Kind != yaml.MappingNode {
			continue
		}
		if v := mapValue(c, "name"); v != nil && v.Kind == yaml.ScalarNode && v.Value == name {
			return i, c
		}
	}
	return -1, nil
}

// mapValue returns the value node of key in mapping m, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func strNode(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

// setScalar sets key to the string v in m ("" removes it). An unchanged value
// is left alone; a changed scalar keeps its node, so its comments stay.
func setScalar(m *yaml.Node, key, v string, order []string) {
	if v == "" {
		setKey(m, key, nil, order)
		return
	}
	if old := mapValue(m, key); old != nil && old.Kind == yaml.ScalarNode {
		if old.Value != v || old.Tag != "!!str" {
			old.Value, old.Tag = v, "!!str"
			if old.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
				old.Style = 0
			}
		}
		return
	}
	setKey(m, key, strNode(v), order)
}

// setKey sets key to val in mapping m: replacing the value of an existing
// key, removing the key when val is nil, or inserting it after the last
// existing key that comes before it in order (at the end when none does).
func setKey(m *yaml.Node, key string, val *yaml.Node, order []string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != key {
			continue
		}
		if val == nil {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
		} else {
			m.Content[i+1] = val
		}
		return
	}
	if val == nil {
		return
	}
	rank := map[string]int{}
	for i, k := range order {
		rank[k] = i
	}
	want, known := rank[key]
	pos := len(m.Content)
	if known {
		pos = 0
		for i := 0; i+1 < len(m.Content); i += 2 {
			if r, ok := rank[m.Content[i].Value]; ok && r < want {
				pos = i + 2
			}
		}
	}
	kv := []*yaml.Node{strNode(key), val}
	m.Content = append(m.Content[:pos], append(kv, m.Content[pos:]...)...)
}

// finish encodes doc (indent 2) and checks the result with Parse.
func finish(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode clusters.yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode clusters.yaml: %w", err)
	}
	if _, err := Parse(buf.Bytes()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
