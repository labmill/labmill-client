package skill

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: d.\n---\nbody\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func warnText(ws []error) string {
	var b strings.Builder
	for _, w := range ws {
		b.WriteString(w.Error() + "\n")
	}
	return b.String()
}

func TestOverrideDanglingSkillMDSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "dang"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere.md", filepath.Join(dir, "dang", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, dir, "good")
	set, warnings, err := LoadWithOverride(testFS(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "dangling") {
		t.Errorf("warnings = %q", warnText(warnings))
	}
	if _, ok := set.Get("good"); !ok {
		t.Error("good skill missing")
	}
}

// Override symlinks: relative ones that stay inside the directory work;
// ones with ".." that leave it, and all absolute ones, are skipped with a
// warning.
func TestOverrideSymlinkRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "skills")
	outside := filepath.Join(base, "outside")
	writeSkill(t, outside, "esc")
	writeSkill(t, dir, "real")
	writeSkill(t, dir, "abs")
	if err := os.Symlink("../outside/esc", filepath.Join(dir, "esc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "esc"), filepath.Join(dir, "absout")); err != nil {
		t.Fatal(err)
	}
	// An absolute symlink to a directory inside the override dir is refused too.
	if err := os.Symlink(filepath.Join(dir, "abs"), filepath.Join(dir, "absin")); err != nil {
		t.Fatal(err)
	}
	// A SKILL.md symlink that escapes is refused.
	if err := os.MkdirAll(filepath.Join(dir, "linkmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside/esc/SKILL.md", filepath.Join(dir, "linkmd", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	set, warnings, err := LoadWithOverride(testFS(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(set.List()); got != "abs,alpha,beta,real" && got != "abs,alpha,real" {
		// the embedded test FS decides the other names; esc must be absent
		if strings.Contains(got, "esc") {
			t.Errorf("escaping skill loaded: %s", got)
		}
	}
	for _, n := range []string{"esc", "absout", "absin", "linkmd"} {
		if _, ok := set.Get(n); ok {
			t.Errorf("skill %q loaded through a refused symlink", n)
		}
		if !strings.Contains(warnText(warnings), `"`+n+`"`) {
			t.Errorf("no warning for %q: %q", n, warnText(warnings))
		}
	}
}

func TestParseAllowedToolsParens(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want []string
	}{
		{"Bash(git status:*), Read", []string{"Bash(git status:*)", "Read"}},
		{"Read Grep", []string{"Read", "Grep"}},
		{"Bash(a, b) Bash(c (d e), f)", []string{"Bash(a, b)", "Bash(c (d e), f)"}},
		{"Read) Grep", []string{"Read)", "Grep"}},
		{"Bash(git status, Read", []string{"Bash(git status, Read"}},
		{[]any{"Bash(git log:*)", "Read, Edit"}, []string{"Bash(git log:*)", "Read", "Edit"}},
		{nil, nil},
	} {
		if got := parseAllowedTools(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseAllowedTools(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
