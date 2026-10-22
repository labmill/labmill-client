package knowledge

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"labmill-client/internal/site"
	"labmill-client/internal/skill"
	"labmill-client/skills"
)

// siteYAML is a fixture Site.yaml for domain d ("a.edu" -> skill site-a-edu).
func siteYAML(d string) string {
	return fmt.Sprintf(`# comment dropped
domain: %[1]s
name: Univ %[1]s
skill: %[2]s
docs_url: "https://docs.%[1]s"
forbidden_terms: [secretterm]
hosts:
  - {alias: x-login, hostname: login.%[1]s, role: login}
clusters: []
`, d, site.SkillName(d))
}

func siteFiles(d string) fstest.MapFS {
	n := site.SkillName(d)
	_ = n
	return fstest.MapFS{
		d + "/Site.yaml":                {Data: []byte(siteYAML(d))},
		d + "/SKILL.md":                 {Data: []byte("---\nname: " + n + "\ndescription: " + d + " specifics.\n---\nbody\n")},
		d + "/References/access.md":     {Data: []byte("access " + d + "\n")},
		d + "/iron/Cluster.yaml":        {Data: []byte("# c\nname: iron\ntype: slurm\nssh_host: x-login\ntransfer: sftp\nforbidden_terms: [ironsecret]\n")},
		d + "/iron/References/slurm.md": {Data: []byte("slurm " + d + "\n")},
		d + "/iron/QUESTIONS.md":        {Data: []byte("private\n")},
		d + "/iron/Evals/e.json":        {Data: []byte("{}")},
		d + "/zeta/Cluster.yaml":        {Data: []byte("name: zeta\ntype: slurm\nssh_host: x-login\ntransfer: sftp\n")},
		d + "/zeta/References/zeta.md":  {Data: []byte("zeta " + d + "\n")},
	}
}

func loadSite(t *testing.T, fsys fs.FS, d string) site.Site {
	t.Helper()
	s, err := site.Load(fsys, d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var genericFS = fstest.MapFS{
	"alpha/SKILL.md":          {Data: []byte("---\nname: alpha\ndescription: Alpha skill.\n---\nA\n")},
	"alpha/References/one.md": {Data: []byte("one\n")},
	"beta/SKILL.md":           {Data: []byte("---\nname: beta\ndescription: Beta skill.\n---\nB\n")},
}

func TestHash(t *testing.T) {
	base := map[string][]byte{"a/x": []byte("1"), "b/y": []byte("2"), "c": []byte("3")}
	h := Hash(base)
	if len(h) != 64 {
		t.Fatalf("hash %q", h)
	}
	for i := 0; i < 20; i++ { // map insertion and iteration order vary
		m := map[string][]byte{}
		for _, k := range []string{"c", "b/y", "a/x"} {
			m[k] = base[k]
		}
		if Hash(m) != h {
			t.Fatal("hash depends on order")
		}
	}
	tests := map[string]map[string][]byte{
		"one byte": {"a/x": []byte("1"), "b/y": []byte("2"), "c": []byte("4")},
		"path":     {"a/x": []byte("1"), "b/y": []byte("2"), "d": []byte("3")},
		"moved":    {"a/x": []byte("12"), "b/y": []byte(""), "c": []byte("3")},
		"missing":  {"a/x": []byte("1"), "b/y": []byte("2")},
		"extra":    {"a/x": []byte("1"), "b/y": []byte("2"), "c": []byte("3"), "e": nil},
	}
	for name, m := range tests {
		if Hash(m) == h {
			t.Errorf("%s: hash unchanged", name)
		}
	}
}

func buildA(t *testing.T) (*Bundle, site.Site) {
	t.Helper()
	s := loadSite(t, siteFiles("a.edu"), "a.edu")
	b, err := Build(genericFS, s)
	if err != nil {
		t.Fatal(err)
	}
	return b, s
}

func TestMarshalRoundTrip(t *testing.T) {
	b, _ := buildA(t)
	var one, two bytes.Buffer
	if err := b.Marshal(&one); err != nil {
		t.Fatal(err)
	}
	if err := b.Marshal(&two); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Error("Marshal is not deterministic")
	}
	got, err := Unmarshal(bytes.NewReader(one.Bytes()), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != b.Version || !reflect.DeepEqual(got.Files, b.Files) {
		t.Errorf("round trip differs")
	}
	// header fields
	zr, _ := gzip.NewReader(bytes.NewReader(one.Bytes()))
	if zr.Name != "" || !zr.ModTime.IsZero() && zr.ModTime.Unix() != 0 {
		t.Errorf("gzip header %q %v", zr.Name, zr.ModTime)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if h.Mode != 0o644 || h.ModTime.Unix() != 0 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" || h.Typeflag != tar.TypeReg {
			t.Errorf("%s header %+v", h.Name, h)
		}
	}
	if !sort.StringsAreSorted(names) || len(names) != len(b.Files) {
		t.Errorf("entries %v", names)
	}
}

// tarGz builds a gzip'd tar from headers (name, type, body).
type ent struct {
	name string
	typ  byte
	body string
}

func tarGz(t *testing.T, ents ...ent) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body))}
		if e.typ == tar.TypeSymlink {
			h.Linkname = "x"
			h.Size = 0
		}
		if e.typ == tar.TypeDir {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestUnmarshalRefuses(t *testing.T) {
	reg := byte(tar.TypeReg)
	tests := []struct {
		name string
		data []byte
		max  int64
	}{
		{"dotdot", tarGz(t, ent{"a/../../etc/x", reg, "x"}), 1 << 20},
		{"dotdot start", tarGz(t, ent{"../x", reg, "x"}), 1 << 20},
		{"absolute", tarGz(t, ent{"/etc/x", reg, "x"}), 1 << 20},
		{"backslash", tarGz(t, ent{`a\b`, reg, "x"}), 1 << 20},
		{"symlink", tarGz(t, ent{"a", tar.TypeSymlink, ""}), 1 << 20},
		{"dir", tarGz(t, ent{"a/", tar.TypeDir, ""}), 1 << 20},
		{"duplicate", tarGz(t, ent{"a", reg, "1"}, ent{"a", reg, "2"}), 1 << 20},
		{"oversize", tarGz(t, ent{"a", reg, strings.Repeat("x", 100)}), 99},
		{"oversize total", tarGz(t, ent{"a", reg, strings.Repeat("x", 60)}, ent{"b", reg, strings.Repeat("x", 60)}), 100},
		{"garbage", []byte("not gzip at all"), 1 << 20},
		{"gzip of garbage", func() []byte {
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			z.Write(bytes.Repeat([]byte("zz"), 600))
			z.Close()
			return b.Bytes()
		}(), 1 << 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Unmarshal(bytes.NewReader(tt.data), tt.max); err == nil {
				t.Error("accepted")
			}
		})
	}
	if _, err := Unmarshal(bytes.NewReader(tarGz(t, ent{"a/b", reg, "ok"})), 10); err != nil {
		t.Errorf("valid bundle refused: %v", err)
	}
}

func TestPrivacyTwoSites(t *testing.T) {
	fsys := siteFiles("a.edu")
	for p, f := range siteFiles("b.edu") {
		fsys[p] = f
	}
	sa, sb := loadSite(t, fsys, "a.edu"), loadSite(t, fsys, "b.edu")
	for _, tc := range []struct {
		s     site.Site
		other string
	}{{sa, "b.edu"}, {sb, "a.edu"}} {
		b, err := Build(genericFS, tc.s)
		if err != nil {
			t.Fatal(err)
		}
		want, err := tc.s.SkillsFS(genericFS)
		if err != nil {
			t.Fatal(err)
		}
		var wantPaths []string
		fs.WalkDir(want, ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				wantPaths = append(wantPaths, p)
			}
			return nil
		})
		var got []string
		for p, data := range b.Files {
			got = append(got, p)
			if strings.HasPrefix(p, site.SkillName(tc.other)+"/") || strings.Contains(string(data), tc.other) {
				t.Errorf("%s bundle holds %s: %s", tc.s.Domain, tc.other, p)
			}
		}
		sort.Strings(got)
		sort.Strings(wantPaths)
		if !reflect.DeepEqual(got, wantPaths) {
			t.Errorf("files %v, want %v", got, wantPaths)
		}
		idx, err := Index(b, tc.s)
		if err != nil || strings.Contains(idx, tc.other) {
			t.Errorf("index leaks %s (err %v)", tc.other, err)
		}
	}
}

func TestManifest(t *testing.T) {
	b, _ := buildA(t)
	m := b.Manifest()
	if m.Site != "a.edu" || m.Version != b.Version || !m.Index || len(m.Files) != len(b.Files) {
		t.Errorf("manifest %+v", m)
	}
	for i, f := range m.Files {
		if i > 0 && m.Files[i-1].Path >= f.Path || f.Size != int64(len(b.Files[f.Path])) || len(f.SHA256) != 64 {
			t.Errorf("file info %+v", f)
		}
	}
}

func TestIndex(t *testing.T) {
	b, s := buildA(t)
	one, err := Index(b, s)
	if err != nil {
		t.Fatal(err)
	}
	b2, s2 := buildA(t)
	two, err := Index(b2, s2)
	if err != nil || one != two {
		t.Errorf("index not byte-stable (err %v)", err)
	}
	for _, want := range []string{
		"# Cluster know-how index (version " + b.Version[:12] + ")",
		"Institution: Univ a.edu (a.edu). Documentation: https://docs.a.edu.",
		"The site skill is site-a-edu.",
		"- alpha: Alpha skill.", "- beta: Beta skill.", "- site-a-edu: a.edu specifics.",
		"## Site profile (Site.yaml)", "login.a.edu",
		"## Cluster iron (Cluster.yaml)", "## Cluster zeta (Cluster.yaml)",
	} {
		if !strings.Contains(one, want) {
			t.Errorf("index lacks %q:\n%s", want, one)
		}
	}
	for _, bad := range []string{"device_policy", "require_disk_encryption", "forbidden_terms", "secretterm", "ironsecret", "comment dropped"} {
		if strings.Contains(one, bad) {
			t.Errorf("index has %q", bad)
		}
	}
	if strings.Index(one, "- alpha") > strings.Index(one, "- beta") || strings.Index(one, "Cluster iron") > strings.Index(one, "Cluster zeta") {
		t.Error("not sorted")
	}
}

func TestRealEmbedded(t *testing.T) {
	s := site.Default()
	b, err := Build(skills.FS, s)
	if err != nil {
		t.Fatal(err)
	}
	set, err := skill.Load(b.FS(), "bundle")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"slurm-user", "research-computing", s.Skill} {
		if _, ok := set.Get(n); !ok {
			t.Errorf("skill %q missing from the bundle", n)
		}
	}
	set2, warns, err := skill.LoadWithOverride(b.FS(), t.TempDir())
	if err != nil || len(warns) > 0 || len(set2.List()) != len(set.List()) {
		t.Errorf("LoadWithOverride: %v %v", warns, err)
	}
	idx, err := Index(b, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range set.List() {
		if !strings.Contains(idx, "- "+k.Name+": ") {
			t.Errorf("index lacks skill %s", k.Name)
		}
	}
	if strings.Contains(idx, "device_policy") || strings.Contains(idx, "forbidden_terms") {
		t.Error("index has excluded keys")
	}
	var buf bytes.Buffer
	if err := b.Marshal(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(&buf, 8<<20)
	if err != nil || got.Version != b.Version {
		t.Errorf("round trip: %v", err)
	}
	g := site.Generic()
	if gb, err := Build(skills.FS, g); err != nil || gb.Site != site.GenericDomain {
		t.Errorf("generic bundle: %+v %v", gb, err)
	}
}
