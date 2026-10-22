package site

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"unicode"
)

// namingViolation reports why rel (slash path below sites/) breaks the
// naming rule, or "". Names are lowercase: domain folders and cluster
// folders (the folders directly below a domain whose first letter is not
// uppercase). Every other folder and file starts with an uppercase letter
// (References, Evals, Diagrams, Site.yaml, Cluster.yaml, Slurm.md, SKILL.md,
// README.md, ...). sites/embed.go is Go source and stays lowercase.
func namingViolation(rel string, isDir bool) string {
	parts := strings.Split(rel, "/")
	base := parts[len(parts)-1]
	switch {
	case rel == "embed.go":
		return ""
	case len(parts) == 1 && isDir:
		if base != strings.ToLower(base) {
			return "domain folders are lowercase"
		}
		return ""
	case len(parts) == 2 && isDir && !unicode.IsUpper(rune(base[0])):
		if base != strings.ToLower(base) {
			return "cluster folders are lowercase"
		}
		return ""
	}
	if !unicode.IsUpper(rune(base[0])) {
		return "must start with an uppercase letter"
	}
	return ""
}

// TestSitesNaming enforces the naming rule on every profile on disk (the
// private ones too, when unpacked), so a new profile follows it.
func TestSitesNaming(t *testing.T) {
	root := os.DirFS("../../sites")
	n := 0
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		n++
		if why := namingViolation(p, d.IsDir()); why != "" {
			t.Errorf("sites/%s: %s", p, why)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no files under sites/")
	}
}

func TestNamingViolation(t *testing.T) {
	for _, tc := range []struct {
		rel   string
		isDir bool
		bad   bool
	}{
		{"example.edu", true, false}, {"Example.org", true, true},
		{"example.edu/hpcone", true, false}, {"example.edu/References", true, false},
		{"example.edu/Hpcone", true, false}, // a capitalized folder is a non-name folder; the loader rejects unknown ones
		{"example.edu/Site.yaml", false, false}, {"example.edu/site.yaml", false, true},
		{"example.edu/References/Slurm.md", false, false}, {"example.edu/References/slurm.md", false, true},
		{"example.edu/hpcone/Cluster.yaml", false, false}, {"example.edu/hpcone/cluster.yaml", false, true},
		{"example.edu/SKILL.md", false, false}, {"example.edu/hpcone/references", true, true},
		{"embed.go", false, false}, {"README.md", false, false}, {"generic/_cluster-template", true, false},
		{"generic/_cluster-template/References/Cluster.md", false, false},
	} {
		if got := namingViolation(tc.rel, tc.isDir) != ""; got != tc.bad {
			t.Errorf("%s: violation = %v, want %v", tc.rel, got, tc.bad)
		}
	}
}
