package app

import (
	"strings"
	"testing"
	"testing/fstest"

	"labmill-client/internal/skill"
)

func TestSystemPromptSiteSkill(t *testing.T) {
	h := newHarness(t)
	h.env.Site = SiteInfo{Name: "Example University", Skill: "site-example-edu", DocsURL: "https://docs.example.edu"}
	sp := SystemPrompt(h.env)
	for _, want := range []string{"Example University", `"site-example-edu"`, "load_skill", "never guess", "https://docs.example.edu"} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	h.env.Site = SiteInfo{}
	if sp := SystemPrompt(h.env); !strings.Contains(sp, "There is no site skill") {
		t.Error("prompt without a site skill doesn't say so")
	}
	h.env.Site = SiteInfo{Skill: "site-generic", Generic: true}
	if sp := SystemPrompt(h.env); !strings.Contains(sp, "no site profile") || !strings.Contains(sp, `"site-generic"`) {
		t.Error("generic prompt doesn't name the fallback skill")
	}
}

func TestSystemPromptSkillToolMap(t *testing.T) {
	h := newHarness(t)
	set, err := skill.Load(fstest.MapFS{
		"other-user/SKILL.md": {Data: []byte("---\nname: other-user\ndescription: Other things.\n---\nbody\n")},
	}, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	h.env.Skills = set
	sp := SystemPrompt(h.env)
	for _, want := range []string{
		"sinfo", "cluster_info", "scontrol show job", "job_status",
		"AskUserQuestion: ask_user", "preview_file", "sbatch", "draft_job", "scancel: cancel_job",
	} {
		if !strings.Contains(sp, want) {
			t.Errorf("tool map missing %q", want)
		}
	}
	// Every tool the map names exists.
	names := map[string]bool{}
	for _, tl := range Tools(h.env) {
		names[tl.Name] = true
	}
	for _, w := range strings.FieldsFunc(skillToolMap, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if strings.Contains(w, "_") && !strings.HasPrefix(w, "no_") && !names[w] && w != "load_skill" {
			t.Errorf("tool map names %q, which is not a tool", w)
		}
	}
	if !strings.Contains(SkillPreamble, "How skill steps map") {
		t.Error("preamble doesn't point to the tool map")
	}
}

// A rejected password stops the sign-in loop: one failure, then wait for the user.
func TestSystemPromptPasswordRejectedStops(t *testing.T) {
	p := SystemPrompt(newHarness(t).env)
	for _, want := range []string{"Never start another sign-in", "wait until the user says they want to try again", "failed a moment ago", "Check your password before trying again", "a sign-in at this institution failed", "accepted none of the offered sign-in methods", "lock the account"} {
		if !strings.Contains(p, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	// "authentication denied" alone is not a password trigger.
	if strings.Contains(p, `("authentication denied"`) || strings.Contains(p, `"check the username and password"`) {
		t.Error("prompt treats a bare authentication denied as a password rejection")
	}
}
