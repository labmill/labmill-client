package knowledge

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"labmill-client/internal/site"
	"labmill-client/skills"
)

func TestNULRefused(t *testing.T) {
	g := fstest.MapFS{"alpha/SKILL.md": {Data: []byte("---\nname: alpha\ndescription: A.\n---\nx\x00y\n")}}
	if _, err := Build(g, site.Generic()); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("Build with NUL: %v", err)
	}
	data := tarGz(t, ent{"a", tar.TypeReg, "x\x00y"})
	if _, err := Unmarshal(bytes.NewReader(data), 1<<20); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Errorf("Unmarshal with NUL: %v", err)
	}
}

func TestUnmarshalCapAtBlockBoundary(t *testing.T) {
	// maxBytes 0 leaves a 1 MiB stream cap; empty files cost 512 bytes each,
	// so 2048 headers end exactly at the cap and the trailer pushes past it.
	for _, n := range []int{2048, 2049, 2100} {
		var ents []ent
		for i := 0; i < n; i++ {
			ents = append(ents, ent{fmt.Sprintf("f%d", i), tar.TypeReg, ""})
		}
		_, err := Unmarshal(bytes.NewReader(tarGz(t, ents...)), 0)
		if err == nil || !strings.Contains(err.Error(), "more than") {
			t.Errorf("%d entries: err %v", n, err)
		}
	}
	var ents []ent
	for i := 0; i < 100; i++ {
		ents = append(ents, ent{fmt.Sprintf("f%d", i), tar.TypeReg, ""})
	}
	if _, err := Unmarshal(bytes.NewReader(tarGz(t, ents...)), 0); err != nil {
		t.Errorf("small bundle: %v", err)
	}
}

func TestIndexOmissions(t *testing.T) {
	out, err := dropKeys([]byte("name: x\nbroker_url: https://b\nbroker:\n  issuers: [https://i]\ndevice_policy: {a: 1}\nkeep: 1\n"),
		"device_policy", "forbidden_terms", "broker_url", "broker")
	if err != nil || out != "name: x\nkeep: 1" {
		t.Errorf("dropKeys = %q, %v", out, err)
	}
	// Index drops them from a real profile parse and omits an empty DocsURL.
	fsys := siteFiles("a.edu")
	fsys["a.edu/Site.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(siteYAML("a.edu"), `docs_url: "https://docs.a.edu"`, `docs_url: ""`, 1) + "broker_url: https://b.a.edu\nbroker: {issuers: [\"https://idp.a.edu/realms/a\"]}\n")}
	s := loadSite(t, fsys, "a.edu")
	b, err := Build(genericFS, s)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Index(b, s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(idx, "Documentation") || strings.Contains(idx, "broker_url") || strings.Contains(idx, "b.a.edu") || strings.Contains(idx, "issuers") || strings.Contains(idx, "idp.a.edu") {
		t.Errorf("index:\n%s", idx)
	}
	if !strings.Contains(idx, "Institution: Univ a.edu (a.edu).\n") {
		t.Errorf("institution line:\n%s", idx)
	}
}

// A Windows checkout must not turn LF into CRLF (see the repo's attributes
// file), or the bundle Version would differ from the one built on Linux.
func TestEmbeddedNoCR(t *testing.T) {
	err := fs.WalkDir(skills.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(skills.FS, p)
		if err == nil && bytes.IndexByte(data, '\r') >= 0 {
			t.Errorf("%s contains a CR", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
