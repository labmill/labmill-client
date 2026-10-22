package site

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestOthersAndSkillsFSAll(t *testing.T) {
	fsys := fstest.MapFS{}
	for _, d := range []string{"b.edu", "a.edu", "c.edu"} {
		siteFixture(fsys, d, false)
	}
	list := listFS(fsys)
	got := othersOf(list, "b.edu")
	if len(got) != 2 || got[0].Name != "a.edu U" || got[1].Name != "c.edu U" {
		t.Fatalf("othersOf = %v", got)
	}
	if got := othersOf(append(list, Generic()), "b.edu"); len(got) != 2 {
		t.Errorf("generic must not be an other: %v", got)
	}
	home := list[1]
	generic := fstest.MapFS{"gen/SKILL.md": {Data: []byte("x")}}
	all, err := SkillsFSAll(generic, home, got)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"gen/SKILL.md", "site-a-edu/SKILL.md", "site-b-edu/SKILL.md", "site-c-edu/SKILL.md"} {
		if _, err := fs.Stat(all, p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	// A clash with any site skill is an error, not only the home one.
	clash := fstest.MapFS{"site-c-edu/SKILL.md": {Data: []byte("x")}}
	if _, err := SkillsFSAll(clash, home, got); err == nil {
		t.Error("clash with another site's skill not reported")
	}
	// The home site listed among the others is added once.
	if _, err := SkillsFSAll(generic, home, list); err != nil {
		t.Errorf("duplicate home: %v", err)
	}
}

func TestClusterNames(t *testing.T) {
	s, err := Parse([]byte(fixtureYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClusterNames(); len(got) != 2 || got[0] != "bigiron" || got[1] != "second" {
		t.Errorf("ClusterNames = %v", got)
	}
}

// A problem with another institution's skill is a warning; with the home
// skill it is an error.
func TestSkillsFSSessionOtherSiteProblems(t *testing.T) {
	fsys := fstest.MapFS{}
	for _, d := range []string{"a.edu", "b.edu"} {
		siteFixture(fsys, d, false)
	}
	list := listFS(fsys)
	generic := fstest.MapFS{"site-b-edu/SKILL.md": {Data: []byte("x")}, "gen/SKILL.md": {Data: []byte("x")}}
	// b.edu's skill clashes with a generic directory: skipped as an other.
	out, warns, err := SkillsFSSession(generic, list[0], list[1:])
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "b.edu U") {
		t.Fatalf("other clash: warns %v err %v", warns, err)
	}
	if _, err := fs.Stat(out, "site-a-edu/SKILL.md"); err != nil {
		t.Errorf("home skill missing: %v", err)
	}
	// As the home site the same clash is fatal, and SkillsFSAll stays strict.
	if _, _, err := SkillsFSSession(generic, list[1], nil); err == nil {
		t.Error("home clash not fatal")
	}
	if _, err := SkillsFSAll(generic, list[0], list[1:]); err == nil {
		t.Error("SkillsFSAll must stay strict")
	}
}
