package skill

import (
	"strings"
	"testing"
	"testing/fstest"
)

// A site skill keeps its reference files in "References/" with capitalized
// names; the generic skills keep "references/". Both load, and a reference
// can be read with any case.
func TestSiteReferencesFolder(t *testing.T) {
	fsys := fstest.MapFS{
		"site/SKILL.md":                    skillMD("site", "d", "body"),
		"site/References/Access.md":        {Data: []byte("top")},
		"site/bigiron/References/Slurm.md": {Data: []byte("ww")},
		"site/bigiron/Cluster.yaml":        {Data: []byte("name: ww")},
		"site/Diagrams/Architecture.svg":   {Data: []byte("<svg/>")},
		"generic/SKILL.md":                 skillMD("generic", "d", "body"),
		"generic/references/plain.md":      {Data: []byte("plain")},
	}
	set, err := Load(fsys, "test")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := set.Get("site")
	if got := strings.Join(k.References, ","); got != "Access.md,bigiron/Slurm.md" {
		t.Fatalf("references = %s", got)
	}
	for name, want := range map[string]string{
		"Access.md": "top", "access.md": "top", "References/Access.md": "top", "references/access.md": "top",
		"bigiron/Slurm.md": "ww", "bigiron/References/Slurm.md": "ww", "../bigiron/References/Slurm.md": "ww", "bigiron/slurm.md": "ww",
	} {
		if got, err := k.ReadReference(name); err != nil || got != want {
			t.Errorf("ReadReference(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	g, _ := set.Get("generic")
	if got, err := g.ReadReference("plain.md"); err != nil || got != "plain" {
		t.Errorf("generic plain.md = %q, %v", got, err)
	}
}
