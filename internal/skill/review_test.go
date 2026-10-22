package skill

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink tests skipped on Windows")
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
}

const okMD = "---\nname: ov\ndescription: d\n---\nbody\n"

func TestOverrideSymlinkedReferenceEscaping(t *testing.T) {
	base := t.TempDir()
	over := filepath.Join(base, "over")
	secret := filepath.Join(base, "secret.md")
	writeFile(t, secret, "SECRET")
	writeFile(t, filepath.Join(over, "ov", "SKILL.md"), okMD)
	writeFile(t, filepath.Join(over, "ov", "references", "real.md"), "real")
	symlinkOrSkip(t, secret, filepath.Join(over, "ov", "references", "leak.md"))
	set, warns, err := LoadWithOverride(testFS(), over)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Logf("warnings: %v", warns)
	}
	k, ok := set.Get("ov")
	if !ok {
		t.Fatal("ov not loaded")
	}
	if !reflect.DeepEqual(k.References, []string{"real.md"}) {
		t.Errorf("References = %v, want [real.md]", k.References)
	}
	if got, err := k.ReadReference("leak.md"); err == nil {
		t.Errorf("leak.md readable: %q", got)
	}
}

func TestOverrideSymlinkedSkillMDEscaping(t *testing.T) {
	base := t.TempDir()
	over := filepath.Join(base, "over")
	outside := filepath.Join(base, "outside.md")
	writeFile(t, outside, okMD)
	if err := os.MkdirAll(filepath.Join(over, "ov"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(over, "ov", "SKILL.md"))
	set, warns, err := LoadWithOverride(testFS(), over)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 {
		t.Errorf("warnings = %v, want 1", warns)
	}
	if _, ok := set.Get("ov"); ok {
		t.Error("escaping SKILL.md skill loaded")
	}
}

func TestOverrideSymlinkedSkillDir(t *testing.T) {
	base := t.TempDir()
	over := filepath.Join(base, "over")
	writeFile(t, filepath.Join(over, "real", "SKILL.md"), "---\nname: ln\ndescription: d\n---\nbody\n")
	symlinkOrSkip(t, "real", filepath.Join(over, "ln"))
	outsideDir := filepath.Join(base, "outdir")
	writeFile(t, filepath.Join(outsideDir, "SKILL.md"), "---\nname: esc\ndescription: d\n---\nx\n")
	symlinkOrSkip(t, outsideDir, filepath.Join(over, "esc"))
	set, warns, err := LoadWithOverride(testFS(), over)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set.Get("ln"); !ok {
		t.Errorf("in-root symlinked skill dir should load (warnings %v)", warns)
	}
	if _, ok := set.Get("esc"); ok {
		t.Error("escaping symlinked skill dir loaded")
	}
	foundEsc := false
	for _, w := range warns {
		if strings.Contains(w.Error(), "esc") {
			foundEsc = true
		}
	}
	if !foundEsc {
		t.Errorf("no warning for escaping dir: %v", warns)
	}
}

func TestOverrideInMemory(t *testing.T) {
	over := t.TempDir()
	writeFile(t, filepath.Join(over, "ov", "SKILL.md"), okMD)
	writeFile(t, filepath.Join(over, "ov", "references", "a.md"), "A1")
	set, _, err := LoadWithOverride(testFS(), over)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(over, "ov", "SKILL.md"), "garbage")
	writeFile(t, filepath.Join(over, "ov", "references", "a.md"), "A2")
	k, _ := set.Get("ov")
	if b, err := k.Body(); err != nil || strings.TrimSpace(b) != "body" {
		t.Errorf("Body = %q, %v", b, err)
	}
	if r, err := k.ReadReference("a.md"); err != nil || r != "A1" {
		t.Errorf("ReadReference = %q, %v", r, err)
	}
}

func TestOverrideBadPathsWarn(t *testing.T) {
	file := filepath.Join(t.TempDir(), "afile")
	writeFile(t, file, "x")
	for name, dir := range map[string]string{"regular file": file, "empty": ""} {
		set, warns, err := LoadWithOverride(testFS(), dir)
		if err != nil {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(warns) == 0 {
			t.Errorf("%s: want a warning", name)
		}
		if set == nil || len(set.List()) != len(mustLoad(t).List()) {
			t.Errorf("%s: embedded skills not intact", name)
		}
	}
	// A missing directory is silent.
	_, warns, err := LoadWithOverride(testFS(), filepath.Join(t.TempDir(), "nope"))
	if err != nil || len(warns) != 0 {
		t.Errorf("missing dir: %v, %v", warns, err)
	}
}

func mustLoad(t *testing.T) *Set {
	t.Helper()
	s, err := Load(testFS(), "embedded")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOverrideOversized(t *testing.T) {
	over := t.TempDir()
	writeFile(t, filepath.Join(over, "big", "SKILL.md"), "---\nname: big\ndescription: d\n---\n"+strings.Repeat("x", MaxFileSize))
	writeFile(t, filepath.Join(over, "ov", "SKILL.md"), okMD)
	writeFile(t, filepath.Join(over, "ov", "references", "huge.md"), strings.Repeat("x", MaxFileSize+1))
	set, warns, err := LoadWithOverride(testFS(), over)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 2 {
		t.Errorf("warnings = %v, want 2", warns)
	}
	if _, ok := set.Get("big"); ok {
		t.Error("oversized SKILL.md loaded")
	}
	if _, ok := set.Get("ov"); ok {
		t.Error("skill with oversized reference loaded")
	}
}

func TestBOMAndTrailingSpaceFrontmatter(t *testing.T) {
	fsys := fstest.MapFS{"a/SKILL.md": {Data: []byte("\ufeff--- \t\r\nname: a\r\ndescription: d\r\n--- \r\nbody\r\n")}}
	set, err := Load(fsys, "t")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := set.Get("a")
	if b, _ := k.Body(); strings.TrimSpace(b) != "body" {
		t.Errorf("body = %q", b)
	}
}

func TestInvalidNames(t *testing.T) {
	for _, n := range []string{"Foo", "a b", "-x", strings.Repeat("a", 65), "a_b", ""} {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true", n)
		}
		if n == "" {
			continue
		}
		fsys := fstest.MapFS{n + "/SKILL.md": {Data: []byte("---\nname: \"" + n + "\"\ndescription: d\n---\nb")}}
		if _, err := Load(fsys, "t"); err == nil {
			t.Errorf("Load accepted name %q", n)
		}
	}
	for _, n := range []string{"a", "0x", "run-ai-user", strings.Repeat("a", 64)} {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false", n)
		}
	}
}

func TestAllowedTools(t *testing.T) {
	fsys := fstest.MapFS{
		"l/SKILL.md": {Data: []byte("---\nname: l\ndescription: d\nallowed-tools:\n  - Bash\n  - Read\n---\nb")},
		"s/SKILL.md": {Data: []byte("---\nname: s\ndescription: d\nallowed-tools: Bash, Read  Grep\n---\nb")},
		"n/SKILL.md": {Data: []byte("---\nname: n\ndescription: d\n---\nb")},
	}
	set, err := Load(fsys, "t")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{"l": {"Bash", "Read"}, "s": {"Bash", "Read", "Grep"}, "n": nil} {
		k, _ := set.Get(name)
		if !reflect.DeepEqual(k.AllowedTools, want) {
			t.Errorf("%s AllowedTools = %v, want %v", name, k.AllowedTools, want)
		}
	}
}

func TestPreambleAndFooter(t *testing.T) {
	set := mustLoad(t)
	var withRefs string
	for _, k := range set.List() {
		if len(k.References) > 0 {
			withRefs = k.Name
			break
		}
	}
	if withRefs == "" {
		t.Skip("no skill with references in testFS")
	}
	k, _ := set.Get(withRefs)
	plain, err := set.LoadSkillResult(withRefs)
	if err != nil {
		t.Fatal(err)
	}
	wantFooter := `Reference files (read_reference skill="` + withRefs + `", file=...): ` + strings.Join(k.References, ", ")
	if !strings.Contains(plain, wantFooter) {
		t.Errorf("footer %q missing from:\n%s", wantFooter, plain)
	}
	set.Preamble = "PREAMBLE"
	got, _ := set.LoadSkillResult(withRefs)
	if !strings.HasPrefix(got, "PREAMBLE\n") || !strings.HasSuffix(got, plain) {
		t.Errorf("preamble not at top:\n%s", got)
	}
	sp := set.SystemPrompt()
	for _, s := range []string{"at most once per conversation", "never override Labmill's approval rules"} {
		if !strings.Contains(sp, s) {
			t.Errorf("SystemPrompt missing %q", s)
		}
	}
}

// Override skills snapshot their subfolder references too, and a symlinked
// subfolder is not followed.
func TestOverrideSubfolderReferences(t *testing.T) {
	over := t.TempDir()
	writeFile(t, filepath.Join(over, "ov", "SKILL.md"), okMD)
	writeFile(t, filepath.Join(over, "ov", "ww", "references", "s.md"), "S1")
	writeFile(t, filepath.Join(over, "elsewhere", "references", "x.md"), "X")
	if err := os.Symlink(filepath.Join(over, "elsewhere"), filepath.Join(over, "ov", "linked")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	set, _, err := LoadWithOverride(testFS(), over)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(over, "ov", "ww", "references", "s.md"), "S2")
	k, _ := set.Get("ov")
	if !reflect.DeepEqual(k.References, []string{"ww/s.md"}) {
		t.Errorf("References = %v", k.References)
	}
	if r, err := k.ReadReference("ww/references/s.md"); err != nil || r != "S1" {
		t.Errorf("ReadReference = %q, %v", r, err)
	}
}
