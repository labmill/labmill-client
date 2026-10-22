package skill_test

import (
	"io/fs"
	"testing"

	"labmill-client/internal/site"
	"labmill-client/internal/skill"
	"labmill-client/skills"
)

// maxDescription is the Agent Skills spec's cap on a frontmatter description.
const maxDescription = 1024

func TestDescriptionLength(t *testing.T) {
	check := func(name string, fsys fs.FS) {
		set, err := skill.Load(fsys, "embedded")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(set.List()) == 0 {
			t.Fatalf("%s: no skills loaded", name)
		}
		for _, k := range set.List() {
			if n := len([]rune(k.Description)); n > maxDescription {
				t.Errorf("%s/%s: description is %d characters, max %d", name, k.Name, n, maxDescription)
			}
		}
	}
	check("skills", skills.FS)
	// Every site skill, merged with the generic ones as Labmill loads them.
	for _, s := range site.List() {
		fsys, err := s.SkillsFS(skills.FS)
		if err != nil {
			t.Fatalf("%s: %v", s.Domain, err)
		}
		check("site "+s.Domain, fsys)
	}
}
