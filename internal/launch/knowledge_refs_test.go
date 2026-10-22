package launch

import (
	"testing"

	"labmill-client/internal/knowledge"
	"labmill-client/internal/site"
)

// A bundle whose site SKILL.md links to reference files that the skill
// runtime can't find is refused (the client keeps its embedded copy); both
// the References/ and the older references/ layouts are served.
func TestRequireSiteSkillReferences(t *testing.T) {
	st := fixtureSite(t)
	name := site.SkillName(st.Domain)
	skillMD := []byte("---\nname: " + name + "\ndescription: D.\n---\nSee [a](references/a.md).\n")
	for _, tc := range []struct {
		desc  string
		files map[string][]byte
		ok    bool
	}{
		{"References/ served", map[string][]byte{name + "/SKILL.md": skillMD, name + "/References/A.md": []byte("a")}, true},
		{"references/ served", map[string][]byte{name + "/SKILL.md": skillMD, name + "/references/a.md": []byte("a")}, true},
		{"links but none served", map[string][]byte{name + "/SKILL.md": skillMD, name + "/Docs/A.md": []byte("a")}, false},
		{"no links, none served", map[string][]byte{name + "/SKILL.md": []byte("---\nname: " + name + "\ndescription: D.\n---\nx\n")}, true},
	} {
		b := &knowledge.Bundle{Site: st.Domain, Files: tc.files}
		if err := requireSiteSkill(b, st); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.desc, err, tc.ok)
		}
	}
}
