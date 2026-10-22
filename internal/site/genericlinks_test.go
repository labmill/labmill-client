package site

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	genericSkillNames = []string{"slurm-user", "research-computing"}
	refTokenRE        = regexp.MustCompile("`((?:[A-Za-z0-9_.-]+/)*[Rr]eferences/[A-Za-z0-9_./-]+\\.md)(?:#[^`]*)?`")
	skillsPathRE      = regexp.MustCompile("(?:^|[^A-Za-z0-9_/.-])(skills/(?:slurm-user|research-computing)(?:/[A-Za-z0-9_.-]+)*)")
)

// exactExists reports whether rel exists under root with exactly this case.
func exactExists(root fs.FS, rel string) bool {
	cur := "."
	for _, part := range strings.Split(path.Clean(rel), "/") {
		entries, err := fs.ReadDir(root, cur)
		if err != nil {
			return false
		}
		found := false
		for _, e := range entries {
			if e.Name() == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		cur = path.Join(cur, part)
	}
	return true
}

// genericLinkProblems checks every mention in a site file (sitesFS) of a
// generic skill's file (skillsFS; rootFS holds both as sites/ and skills/):
// "skills/<name>/..." paths, and a `references/x.md` token that follows a
// skill name ("slurm-user → `references/slurm.md`") must exist in skills/
// with exactly that case. The generic skills keep lowercase names while site
// files are capitalized, so a blanket rename can capitalize a link by
// mistake. A token that sits right behind a skill name (a short gap without
// a backtick: "skill → ", "**skill** ") is always a generic link; a token
// further away that names an existing site file is taken for that file.
func genericLinkProblems(sitesFS, skillsFS, rootFS fs.FS) (problems []string, checked int, err error) {
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	err = fs.WalkDir(sitesFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") || p == "README.md" || strings.HasSuffix(p, "/SOURCE.md") { // SOURCE.md names upstream files that were renamed
			return err
		}
		data, err := fs.ReadFile(sitesFS, p)
		if err != nil {
			return err
		}
		text := string(data)
		for _, m := range skillsPathRE.FindAllStringSubmatch(text, -1) {
			tok := strings.TrimRight(m[1], ".")
			if !exactExists(rootFS, tok) {
				bad("sites/%s: %s does not exist (case matters)", p, tok)
			}
			checked++
		}
		// Directories a bare token may resolve to inside the site tree.
		parts := strings.Split(p, "/")
		bases := []string{path.Dir(p)}
		if len(parts) >= 2 {
			bases = append(bases, parts[0])
			if entries, err := fs.ReadDir(sitesFS, parts[0]); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						bases = append(bases, path.Join(parts[0], e.Name()))
					}
				}
			}
		}
		for _, loc := range refTokenRE.FindAllStringSubmatchIndex(text, -1) {
			tok := text[loc[2]:loc[3]]
			// The nearest skill name before the token, within 150 characters.
			start := max(0, loc[0]-150)
			ctx := text[start:loc[0]]
			before := text[:loc[0]]
			line := before[strings.LastIndex(before, "\n\n")+1:] // the paragraph: a Sources: note wraps over lines
			if strings.Contains(line, "Sources:") {
				continue // provenance lines name upstream files that were renamed
			}
			skill, at := "", -1
			for _, n := range genericSkillNames {
				if i := strings.LastIndex(ctx, n); i > at {
					skill, at = n, i
				}
			}
			direct := false
			if skill != "" {
				gap := ctx[at+len(skill):]
				direct = len(gap) <= 20 && !strings.Contains(gap, "`")
			}
			for _, n := range genericSkillNames {
				if rest, ok := strings.CutPrefix(tok, n+"/"); ok {
					skill, tok, direct = n, rest, true
				}
			}
			if skill == "" || strings.Contains(tok, "..") || strings.HasPrefix(tok, "sites/") {
				continue
			}
			if !direct {
				inSite := false
				for _, b := range bases {
					if exactExists(sitesFS, path.Join(b, tok)) {
						inSite = true
					}
				}
				if inSite {
					continue
				}
			}
			checked++
			if !exactExists(skillsFS, path.Join(skill, tok)) {
				bad("sites/%s: `%s` after %q is not a file of skills/%s (case matters)", p, tok, skill, skill)
			}
		}
		return nil
	})
	return problems, checked, err
}

func TestSiteFilesLinkGenericSkillsExactly(t *testing.T) {
	root := os.DirFS("../..")
	sitesFS, err := fs.Sub(root, "sites")
	if err != nil {
		t.Fatal(err)
	}
	skillsFS, err := fs.Sub(root, "skills")
	if err != nil {
		t.Fatal(err)
	}
	problems, checked, err := genericLinkProblems(sitesFS, skillsFS, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	if checked == 0 {
		t.Log("no generic-skill mentions found (public profile only)")
	}
}

// A capitalized link to a generic skill's file is caught even when a site
// file of the same name exists next to it (Slurm.md, Access.md, ...).
func TestGenericLinkCheckCatchesCollidingNames(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("skills/slurm-user/references/slurm.md", "x")
	write("sites/example.edu/References/Slurm.md", "x")
	write("sites/example.edu/References/Notes.md", "Cluster details: see slurm-user → `references/slurm.md` (ok).\n"+
		"Site file: `References/Slurm.md` (ok, far from any skill name).\n")
	check := func() []string {
		root := os.DirFS(dir)
		sitesFS, _ := fs.Sub(root, "sites")
		skillsFS, _ := fs.Sub(root, "skills")
		problems, _, err := genericLinkProblems(sitesFS, skillsFS, root)
		if err != nil {
			t.Fatal(err)
		}
		return problems
	}
	if p := check(); len(p) != 0 {
		t.Fatalf("clean fixture reported %v", p)
	}
	for _, bad := range []string{
		"see slurm-user → `References/Slurm.md` here\n",
		"see **slurm-user** `References/Slurm.md` here\n",
		"slurm-user: `References/Slurm.md`\n",
	} {
		write("sites/example.edu/References/Notes.md", bad)
		if p := check(); len(p) != 1 {
			t.Errorf("%q: problems = %v, want 1", bad, p)
		}
	}
}
