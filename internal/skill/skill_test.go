package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"labmill-client/skills"
)

func skillMD(name, desc, body string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: >-\n  " + desc + "\n---\n\n" + body)}
}

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"alpha/SKILL.md":            skillMD("alpha", "Use for alpha things,\n  across two lines.", "# Alpha\nDo alpha.\n"),
		"alpha/references/b.md":     {Data: []byte("bee")},
		"alpha/references/a.md":     {Data: []byte("ay")},
		"alpha/references/x.txt":    {Data: []byte("not md")},
		"alpha/references/sub/c.md": {Data: []byte("nested")},
		"beta/SKILL.md":             skillMD("beta", "Use for beta.", "Beta body.\n"),
		"notaskill/README.md":       {Data: []byte("ignored")},
	}
}

func TestLoad(t *testing.T) {
	set, err := Load(testFS(), "embedded")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	list := set.List()
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("List = %v", names(list))
	}
	a, ok := set.Get("alpha")
	if !ok {
		t.Fatal("alpha missing")
	}
	if a.Description != "Use for alpha things, across two lines." {
		t.Errorf("description = %q", a.Description)
	}
	if strings.Join(a.References, ",") != "a.md,b.md" {
		t.Errorf("references = %v", a.References)
	}
	if a.Source != "embedded" {
		t.Errorf("source = %q", a.Source)
	}
	body, err := a.Body()
	if err != nil || body != "# Alpha\nDo alpha.\n" {
		t.Errorf("body = %q, %v", body, err)
	}
	if _, ok := set.Get("notaskill"); ok {
		t.Error("directory without SKILL.md loaded")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]fstest.MapFS{
		"no frontmatter": {"a/SKILL.md": {Data: []byte("# just markdown\n")}},
		"no name":        {"a/SKILL.md": {Data: []byte("---\ndescription: d\n---\nbody")}},
		"no description": {"a/SKILL.md": {Data: []byte("---\nname: a\n---\nbody")}},
		"name mismatch":  {"a/SKILL.md": skillMD("b", "d", "body")},
		"unterminated":   {"a/SKILL.md": {Data: []byte("---\nname: a\ndescription: d\nbody")}},
	}
	for name, fsys := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fsys, "embedded"); err == nil {
				t.Fatal("Load succeeded")
			}
		})
	}
}

func TestReadReference(t *testing.T) {
	set, err := Load(testFS(), "embedded")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := set.Get("alpha")
	for _, n := range []string{"a.md", "references/a.md", "a.md#usage", "references/a.md#a-section", " a.md # x "} {
		if got, err := a.ReadReference(n); err != nil || got != "ay" {
			t.Errorf("ReadReference(%q) = %q, %v", n, got, err)
		}
	}
	for _, n := range []string{"../beta/SKILL.md", "references/../SKILL.md", "/etc/passwd", "x.txt", "sub/c.md", "missing.md", "", "#a.md", "SKILL.md#a.md"} {
		if _, err := a.ReadReference(n); err == nil {
			t.Errorf("ReadReference(%q) succeeded", n)
		}
	}
}

func TestSystemPromptAndLoadSkillResult(t *testing.T) {
	set, err := Load(testFS(), "embedded")
	if err != nil {
		t.Fatal(err)
	}
	sp := set.SystemPrompt()
	for _, want := range []string{"load_skill", "read_reference", "- alpha: Use for alpha things, across two lines.", "- beta: Use for beta."} {
		if !strings.Contains(sp, want) {
			t.Errorf("SystemPrompt missing %q:\n%s", want, sp)
		}
	}
	if strings.Index(sp, "- alpha:") > strings.Index(sp, "- beta:") {
		t.Error("skills not in name order")
	}
	res, err := set.LoadSkillResult("alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Do alpha.", "a.md", "b.md", "read_reference"} {
		if !strings.Contains(res, want) {
			t.Errorf("LoadSkillResult missing %q:\n%s", want, res)
		}
	}
	if _, err := set.LoadSkillResult("nope"); err == nil {
		t.Error("LoadSkillResult(nope) succeeded")
	}
	empty, _ := Load(fstest.MapFS{}, "embedded")
	if empty.SystemPrompt() != "" {
		t.Error("empty set has a system prompt")
	}
}

func TestLoadWithOverride(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("beta/SKILL.md", "---\nname: beta\ndescription: Updated beta.\n---\nNew beta body.\n")
	write("gamma/SKILL.md", "---\nname: gamma\ndescription: Brand new.\n---\nGamma.\n")
	write("broken/SKILL.md", "no frontmatter at all\n")

	set, warnings, err := LoadWithOverride(testFS(), dir)
	if err != nil {
		t.Fatalf("LoadWithOverride: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "broken") {
		t.Errorf("warnings = %v", warnings)
	}
	if got := names(set.List()); got != "alpha,beta,gamma" {
		t.Fatalf("skills = %s", got)
	}
	b, _ := set.Get("beta")
	if b.Description != "Updated beta." || b.Source != dir {
		t.Errorf("beta = %+v", b)
	}
	if body, _ := b.Body(); body != "New beta body.\n" {
		t.Errorf("beta body = %q", body)
	}
	a, _ := set.Get("alpha")
	if a.Source != "embedded" {
		t.Errorf("alpha source = %q", a.Source)
	}

	set, warnings, err = LoadWithOverride(testFS(), filepath.Join(dir, "does-not-exist"))
	if err != nil || len(warnings) != 0 || len(set.List()) != 2 {
		t.Errorf("missing override dir: %v %v %d", err, warnings, len(set.List()))
	}
}

// The generic skills (site facts live in sites/)
// must load as-is.
func TestEmbeddedSkills(t *testing.T) {
	set, err := Load(skills.FS, "embedded")
	if err != nil {
		t.Fatalf("Load(skills.FS): %v", err)
	}
	for _, name := range []string{"slurm-user", "research-computing"} {
		k, ok := set.Get(name)
		if !ok {
			t.Fatalf("%s not embedded", name)
		}
		if k.Description == "" || len(k.References) == 0 {
			t.Errorf("%s: %+v", name, k)
		}
		if _, err := k.ReadReference(k.References[0]); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestGenericSkillsHaveNoSiteFacts lives in internal/site: it derives the
// forbidden terms from every embedded site profile.

func names(list []*Skill) string {
	var n []string
	for _, k := range list {
		n = append(n, k.Name)
	}
	return strings.Join(n, ",")
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// References in subfolders (<sub>/references/*.md), as a site skill keeps
// one folder per cluster.
func TestSubfolderReferences(t *testing.T) {
	fsys := fstest.MapFS{
		"site/SKILL.md":                       skillMD("site", "Site facts.", "Body.\n"),
		"site/references/access.md":           {Data: []byte("top")},
		"site/bigiron/references/slurm.md":    {Data: []byte("ww")},
		"site/bigiron/references/notes.txt":   {Data: []byte("not md")},
		"site/bigiron/references/deep/x.md":   {Data: []byte("too deep")},
		"site/bigiron/cluster.yaml":           {Data: []byte("name: ww")},
		"site/bigiron/QUESTIONS.md":           {Data: []byte("private")},
		"site/gpupod/references/pools.md":     {Data: []byte("mp")},
		"site/gpupod/diagrams/a.svg":          {Data: []byte("<svg/>")},
		"site/diagrams/references/x.md":       {Data: []byte("reserved")},
		"site/evals/references/x.md":          {Data: []byte("reserved")},
		"site/Bad_Name/references/x.md":       {Data: []byte("bad name")},
		"site/a/b/references/x.md":            {Data: []byte("too deep")},
		"site/diagrams/architecture.svg":      {Data: []byte("<svg/>")},
		"site/references/references/inner.md": {Data: []byte("nested")},
	}
	set, err := Load(fsys, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	k, _ := set.Get("site")
	if got := strings.Join(k.References, ","); got != "access.md,bigiron/slurm.md,gpupod/pools.md" {
		t.Fatalf("references = %s", got)
	}
	for n, want := range map[string]string{
		"access.md": "top", "references/access.md": "top", "../references/access.md": "top",
		"bigiron/slurm.md": "ww", "bigiron/references/slurm.md": "ww", "../bigiron/references/slurm.md": "ww",
		"../../bigiron/references/slurm.md": "ww", "./gpupod/pools.md": "mp", " gpupod/pools.md ": "mp",
		"bigiron/references/slurm.md#partitions": "ww", "../access.md#top": "top",
	} {
		if got, err := k.ReadReference(n); err != nil || got != want {
			t.Errorf("ReadReference(%q) = %q, %v; want %q", n, got, err, want)
		}
	}
	for _, n := range []string{
		"bigiron/cluster.yaml", "bigiron/QUESTIONS.md", "bigiron/references/notes.txt", "bigiron/notes.txt",
		"bigiron/deep/x.md", "bigiron/references/deep/x.md", "gpupod/diagrams/a.svg", "diagrams/architecture.svg",
		"diagrams/x.md", "evals/x.md", "Bad_Name/x.md", "a/b/x.md", "references/inner.md", "SKILL.md",
		"bigiron/../SKILL.md", "bigiron/references/../../SKILL.md", "/bigiron/slurm.md", "bigiron", "",
	} {
		if got, err := k.ReadReference(n); err == nil {
			t.Errorf("ReadReference(%q) = %q, want an error", n, got)
		} else if !strings.Contains(err.Error(), "bigiron/slurm.md") {
			t.Errorf("ReadReference(%q): error %q doesn't list the reference files", n, err)
		}
	}
	res, err := set.LoadSkillResult("site")
	if err != nil || !strings.Contains(res, "access.md, bigiron/slurm.md, gpupod/pools.md") {
		t.Errorf("LoadSkillResult = %q, %v", res, err)
	}
}

func TestSubfolderName(t *testing.T) {
	for d, want := range map[string]bool{
		"bigiron": true, "gpupod": true, "a": true, "arc-2": true, strings.Repeat("a", 63): true,
		strings.Repeat("a", 64): false, "references": false, "evals": false, "diagrams": false, "tools": false,
		"Wild": false, "-a": false, "a_b": false, "": false, ".": false, "..": false, "_template": false,
	} {
		if SubfolderName(d) != want {
			t.Errorf("SubfolderName(%q) = %v", d, !want)
		}
	}
}

func TestToolFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"demo/SKILL.md":         {Data: []byte("---\nname: demo\ndescription: d\n---\nbody\n")},
		"demo/references/a.md":  {Data: []byte("ref")},
		"demo/tools/README.md":  {Data: []byte("# tools")},
		"demo/tools/run.sh":     {Data: []byte("#!/bin/sh\necho hi\n")},
		"demo/tools/bin.dat":    {Data: []byte("a\x00b")},
		"demo/tools/.hidden.sh": {Data: []byte("h")},
		"demo/tools/_draft.sh":  {Data: []byte("d")},
		"demo/tools/sub/x.sh":   {Data: []byte("nested")},
	}
	for _, snap := range []bool{false, true} {
		var set *Set
		var err error
		if snap {
			dir := t.TempDir()
			for n, f := range fsys {
				p := filepath.Join(dir, n)
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, f.Data, 0o644)
			}
			set, _, err = LoadWithOverride(fstest.MapFS{}, dir)
		} else {
			set, err = Load(fsys, "t")
		}
		if err != nil {
			t.Fatal(err)
		}
		k, ok := set.Get("demo")
		if !ok {
			t.Fatalf("snap=%v: demo missing", snap)
		}
		if got := strings.Join(k.Tools, ","); got != "tools/README.md,tools/bin.dat,tools/run.sh" {
			t.Errorf("snap=%v: Tools = %s", snap, got)
		}
		if got, err := k.ReadReference("tools/README.md"); err != nil || got != "# tools" {
			t.Errorf("README: %q %v", got, err)
		}
		if got, err := k.ReadReference("tools/run.sh"); err != nil || !strings.Contains(got, "echo hi") {
			t.Errorf("run.sh: %q %v", got, err)
		}
		for _, bad := range []string{"tools/nope.sh", "tools/.hidden.sh", "tools/_draft.sh", "tools/sub/x.sh", "tools/bin.dat", "tools", "../../etc/passwd", "tools/../SKILL.md"} {
			if _, err := k.ReadReference(bad); err == nil {
				t.Errorf("snap=%v: ReadReference(%q) succeeded", snap, bad)
			}
		}
		res, err := set.LoadSkillResult("demo")
		if err != nil || !strings.Contains(res, "Scripts (") || !strings.Contains(res, "tools/run.sh") {
			t.Errorf("LoadSkillResult: %q %v", res, err)
		}
	}
}
