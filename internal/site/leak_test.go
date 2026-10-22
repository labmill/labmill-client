package site

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"labmill-client/internal/config"
)

// privateTerms returns the terms that identify s and must not appear in the
// public generic skills: the domain, the institution's name, every host's
// hostname and its parent domain, every alias, every cluster name that isn't
// just a backend type (Site.yaml's and the Cluster.yaml templates), every
// cluster folder name, every URL host, and the profile's forbidden_terms
// (Site.yaml's and every Cluster.yaml's). Lowercased, deduplicated and
// sorted.
func privateTerms(s Site) []string {
	set := map[string]bool{}
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if len(t) >= 3 {
			set[t] = true
		}
	}
	addURL := func(raw string) {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			add(u.Hostname())
		}
	}
	add(s.Domain)
	add(s.Name)
	for _, h := range s.Hosts {
		add(h.Alias)
		add(h.HostName)
		if i := strings.IndexByte(h.HostName, '.'); i > 0 && strings.Contains(h.HostName[i+1:], ".") {
			add(h.HostName[i+1:])
		}
	}
	for _, c := range s.Clusters {
		if c.Name != string(config.TypeSlurm) {
			add(c.Name)
		}
	}
	for _, f := range s.Folders {
		add(f.Dir)
	}
	for _, u := range []string{s.DocsURL, s.BrokerURL, s.TwoFactorURL} {
		addURL(u)
	}
	for _, t := range s.ForbiddenTerms {
		add(t)
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func isAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

// termRE matches term case-insensitively, as a whole word on each side whose
// edge character is a letter or digit ("7.1." also matches "7.1.2", but
// "onex" doesn't match "bonexa").
func termRE(term string) *regexp.Regexp {
	re := regexp.QuoteMeta(term)
	if isAlnum(term[0]) {
		re = `(?:^|[^a-z0-9])` + re
	}
	if isAlnum(term[len(term)-1]) {
		re += `(?:$|[^a-z0-9])`
	}
	return regexp.MustCompile(`(?i)` + re)
}

// leakTerm is one term of a private profile, with its matcher.
type leakTerm struct {
	site, text string
	re         *regexp.Regexp
}

// leakKind says how a repo file is checked.
type leakKind int

const (
	leakNone leakKind = iota // not covered (exempt, or not public)
	leakText                 // every line
	leakGo                   // string literals only
)

// classifyLeak decides how the repo-relative, slash-separated path p is
// checked. Covered: all of skills/ (the upstream license text excepted, which
// MIT requires unaltered), sites/README.md, sites/embed.go and sites/generic,
// the string literals of non-test Go files under cmd/ and internal/, the
// embedded image catalog, the GUI frontend, README.md, docs/index.md and
// docs/user. Exempt: _test.go files (fixtures may name real-looking sites),
// docs/dev (developer history and design) and CLAUDE.md.
func classifyLeak(p string) leakKind {
	switch {
	case strings.HasSuffix(p, "_test.go"):
		return leakNone
	case p == "skills/LICENSE.upstream":
		return leakNone
	case strings.HasPrefix(p, "skills/"):
		return leakText
	case p == "sites/README.md", strings.HasPrefix(p, "sites/generic/"):
		return leakText
	case p == "sites/embed.go":
		return leakGo
	case strings.HasSuffix(p, ".go") && (strings.HasPrefix(p, "cmd/") || strings.HasPrefix(p, "internal/")):
		return leakGo
	case p == "internal/rules/images.yaml", strings.HasPrefix(p, "cmd/labmill-gui/frontend/"):
		return leakText
	case p == "README.md", p == "docs/index.md", strings.HasPrefix(p, "docs/user/"):
		return leakText
	}
	return leakNone
}

// scanLeaks checks the files (repo-relative path to content) against terms
// and returns one message per hit. Go files are checked in their string
// literals, or with comments set in their comments instead.
func scanLeaks(terms []leakTerm, files map[string]string, comments bool) []string {
	var out []string
	lines := func(path string, first int, text string) {
		for i, line := range strings.Split(text, "\n") {
			lower := strings.ToLower(line)
			for _, tm := range terms {
				if strings.Contains(lower, tm.text) && tm.re.MatchString(line) {
					out = append(out, fmt.Sprintf("%s:%d: %q (from the %s profile) belongs in that site's skill", path, first+i, tm.text, tm.site))
				}
			}
		}
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		switch classifyLeak(p) {
		case leakText:
			if !comments {
				lines(p, 1, files[p])
			}
		case leakGo:
			mode := parser.Mode(0)
			if comments {
				mode = parser.ParseComments
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, files[p], mode)
			if err != nil {
				out = append(out, fmt.Sprintf("%s: %v", p, err))
				continue
			}
			if comments {
				for _, g := range f.Comments {
					lines(p, fset.Position(g.Pos()).Line, g.Text())
				}
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if l, ok := n.(*ast.BasicLit); ok && l.Kind == token.STRING {
					v, err := strconv.Unquote(l.Value)
					if err != nil {
						v = l.Value
					}
					lines(p, fset.Position(l.Pos()).Line, v)
				}
				return true
			})
		}
	}
	return out
}

// readRepoFiles loads every file under root that classifyLeak covers.
func readRepoFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, r := range []string{"skills", "sites", "cmd", "internal", "docs/user", "README.md", "docs/index.md"} {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(r)), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if classifyLeak(rel) == leakNone {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
				return nil // binary
			}
			files[rel] = string(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func repoLeakTerms() []leakTerm {
	var terms []leakTerm
	for _, s := range List() {
		for _, x := range privateTerms(s) {
			terms = append(terms, leakTerm{s.Domain, x, termRE(x)})
		}
	}
	return terms
}

// The public files (the generic skills, the generic site profile, runtime Go
// strings, embedded data, the README and the user docs) carry no fact of any
// site whose private profile is in this build. The terms come from the
// profiles themselves, so this test file names no site; without the private
// profiles (a clone without the sites key) it checks nothing.
func TestGenericSkillsHaveNoSiteFacts(t *testing.T) {
	terms := repoLeakTerms()
	if len(terms) == 0 {
		t.Log("no private site profiles in this build; nothing to check")
		return
	}
	files := readRepoFiles(t, filepath.Join("..", ".."))
	for _, k := range []struct {
		name  string
		match func(p string) bool
	}{
		{"skills/", func(p string) bool { return strings.HasPrefix(p, "skills/") }},
		{"sites/README.md", func(p string) bool { return p == "sites/README.md" }},
		{"sites/embed.go", func(p string) bool { return p == "sites/embed.go" }},
		{"sites/generic/", func(p string) bool { return strings.HasPrefix(p, "sites/generic/") }},
		{"images.yaml", func(p string) bool { return p == "internal/rules/images.yaml" }},
		{"frontend", func(p string) bool { return strings.HasPrefix(p, "cmd/labmill-gui/frontend/") }},
		{"README.md", func(p string) bool { return p == "README.md" }},
		{"docs/index.md", func(p string) bool { return p == "docs/index.md" }},
		{"docs/user/", func(p string) bool { return strings.HasPrefix(p, "docs/user/") }},
		{"Go in cmd/", func(p string) bool { return strings.HasPrefix(p, "cmd/") && strings.HasSuffix(p, ".go") }},
		{"Go in internal/", func(p string) bool { return strings.HasPrefix(p, "internal/") && strings.HasSuffix(p, ".go") }},
	} {
		var n int
		for p := range files {
			if k.match(p) {
				n++
			}
		}
		if n == 0 {
			t.Errorf("no file scanned for %s; is the repo layout as expected?", k.name)
		}
	}
	for _, m := range scanLeaks(terms, files, false) {
		t.Error(m)
	}
}

// Comments in runtime Go files are public source too.
func TestGoCommentsHaveNoSiteFacts(t *testing.T) {
	terms := repoLeakTerms()
	if len(terms) == 0 {
		t.Log("no private site profiles in this build; nothing to check")
		return
	}
	for _, m := range scanLeaks(terms, readRepoFiles(t, filepath.Join("..", "..")), true) {
		t.Error(m)
	}
}

// The scanner itself: a planted term fails in every covered kind of file and
// passes in the exempt ones. Runs without any private profile.
func TestLeakScannerCoverage(t *testing.T) {
	terms := []leakTerm{{"example.edu", "zqplant", termRE("zqplant")}}
	goSrc := func(body string) string { return "package x\n\n" + body + "\n" }
	for _, c := range []struct {
		name, path, content string
		comments, want      bool
	}{
		{"skill file", "skills/other-user/references/a.md", "see zqplant here", false, true},
		{"skill script", "skills/other-user/tools/x.sh", "echo zqplant", false, true},
		{"skill SOURCE", "skills/SOURCE.md", "from ZQPlant", false, true},
		{"skill license", "skills/LICENSE.upstream", "zqplant", false, false},
		{"sites README", "sites/README.md", "zqplant", false, true},
		{"sites embed.go string", "sites/embed.go", goSrc(`var x = "zqplant"`), false, true},
		{"sites generic", "sites/generic/References/A.md", "zqplant", false, true},
		{"private profile", "sites/example.edu/SKILL.md", "zqplant", false, false},
		{"go string", "internal/app/a.go", goSrc(`var s = "use zqplant"`), false, true},
		{"go raw string", "cmd/x/main.go", goSrc("var s = `a\nzqplant`"), false, true},
		{"go comment is not a string", "internal/app/a.go", goSrc("// zqplant\nvar s = 1"), false, false},
		{"go comment in comment mode", "internal/app/a.go", goSrc("// zqplant\nvar s = 1"), true, true},
		{"go string in comment mode", "internal/app/a.go", goSrc(`var s = "zqplant"`), true, false},
		{"go test file", "internal/app/a_test.go", goSrc(`var s = "zqplant"`), false, false},
		{"image catalog", "internal/rules/images.yaml", "- zqplant", false, true},
		{"frontend js", "cmd/labmill-gui/frontend/app.js", "x('zqplant')", false, true},
		{"frontend html", "cmd/labmill-gui/frontend/index.html", "<p>zqplant</p>", false, true},
		{"frontend css", "cmd/labmill-gui/frontend/app.css", "/* zqplant */", false, true},
		{"README", "README.md", "zqplant", false, true},
		{"docs index", "docs/index.md", "zqplant", false, true},
		{"user doc", "docs/user/nested/a.md", "zqplant", false, true},
		{"dev doc", "docs/dev/done.md", "zqplant", false, false},
		{"CLAUDE.md", "CLAUDE.md", "zqplant", false, false},
		{"whole word only", "README.md", "zqplanted", false, false},
	} {
		got := scanLeaks(terms, map[string]string{c.path: c.content}, c.comments)
		if (len(got) > 0) != c.want {
			t.Errorf("%s: findings = %v, want hit = %v", c.name, got, c.want)
		}
	}
	// A Go file that doesn't parse is reported, not skipped.
	if got := scanLeaks(terms, map[string]string{"internal/app/a.go": "package"}, false); len(got) != 1 {
		t.Errorf("unparsable file: %v", got)
	}
	// Line numbers point at the literal.
	got := scanLeaks(terms, map[string]string{"internal/app/a.go": goSrc("var a = 1\nvar s = \"zqplant\"")}, false)
	if len(got) != 1 || !strings.HasPrefix(got[0], "internal/app/a.go:4:") {
		t.Errorf("line: %v", got)
	}
}

func TestPrivateTermsFixture(t *testing.T) {
	s, err := Load(fstest.MapFS{
		"example.edu/Site.yaml":           {Data: []byte(fixtureYAML + "forbidden_terms: [proj-42, \"7.1.\"]\n")},
		"example.edu/SKILL.md":            {Data: []byte("x")},
		"example.edu/gpupod/Cluster.yaml": {Data: []byte(gpupodYAML + "forbidden_terms: [podpuller]\n")},
		"example.edu/oldbox/QUESTIONS.md": {Data: []byte("q")},
	}, "example.edu")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(privateTerms(s), " ")
	for _, want := range []string{"example.edu", "example university", "ex-login", "login.hpc.example.edu", "hpc.example.edu", "ex-files", "bigiron", "proj-42", "7.1.",
		"gpupod", "gpubox", "podpuller", "oldbox"} {
		if !strings.Contains(" "+got+" ", " "+want+" ") {
			t.Errorf("terms %q lack %q", got, want)
		}
	}
	if strings.Contains(" "+got+" ", " slurm ") {
		t.Errorf("the backend name slurm is a term: %q", got)
	}
	for text, want := range map[string]bool{
		"ssh ex-login":          true,
		"see BIGIRON docs":      true,
		"driver 7.1.2":          true,
		"version 17.1.":         false,
		"bigironclad":           false,
		"https://example.edu/x": true,
		"use sbatch job.sh":     false,
	} {
		var hit bool
		for _, x := range privateTerms(s) {
			hit = hit || termRE(x).MatchString(text)
		}
		if hit != want {
			t.Errorf("%q matched = %v, want %v", text, hit, want)
		}
	}
}
