package app

import (
	"strings"
	"testing"
	"testing/fstest"

	"labmill-client/internal/skill"
)

// The two prompt variants: the full one lists every skill and the site
// facts; the broker-index one leaves them out (the broker's index block
// carries them) and keeps the instruction to load the site skill and the
// skill tool map.
func TestSystemPromptFullAndBrokerIndexVariants(t *testing.T) {
	h := newHarness(t)
	set, err := skill.Load(fstest.MapFS{
		"alpha/SKILL.md":            {Data: []byte("---\nname: alpha\ndescription: Alpha skill, unique-desc-marker.\n---\nbody\n")},
		"site-example-edu/SKILL.md": {Data: []byte("---\nname: site-example-edu\ndescription: Example specifics.\n---\nbody\n")},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	h.env.Skills = set
	h.env.Site = SiteInfo{Name: "Example University", Skill: "site-example-edu", DocsURL: "https://docs.example.edu/hpc"}

	full := SystemPrompt(h.env)
	if SystemPromptFor(h.env, false) != full {
		t.Fatal("SystemPrompt must equal SystemPromptFor(env, false)")
	}
	for _, want := range []string{"unique-desc-marker", "Example University", "https://docs.example.edu/hpc", `"site-example-edu"`, "How skill steps map"} {
		if !strings.Contains(full, want) {
			t.Errorf("full prompt lacks %q", want)
		}
	}

	trimmed := SystemPromptFor(h.env, true)
	if SystemPromptFor(h.env, true) != trimmed {
		t.Error("trimmed prompt not deterministic")
	}
	if len(trimmed) >= len(full) {
		t.Errorf("trimmed prompt (%d) is not shorter than the full one (%d)", len(trimmed), len(full))
	}
	for _, gone := range []string{"unique-desc-marker", "Example University", "https://docs.example.edu/hpc"} {
		if strings.Contains(trimmed, gone) {
			t.Errorf("trimmed prompt still has %q", gone)
		}
	}
	for _, keep := range []string{`"site-example-edu"`, "call the load_skill tool", "never override Labmill's approval rules", "listed in the cluster know-how index above", "load_skill", "How skill steps map", "Configured clusters", "hpcone"} {
		if !strings.Contains(trimmed, keep) {
			t.Errorf("trimmed prompt lacks %q", keep)
		}
	}
}

// Without a site skill there is nothing to point at, so the trimmed variant
// keeps the full site paragraph.
func TestSystemPromptBrokerIndexWithoutSiteSkillKeepsSiteParagraph(t *testing.T) {
	h := newHarness(t)
	h.env.Site = SiteInfo{Name: "Example University"}
	got := SystemPromptFor(h.env, true)
	if !strings.Contains(got, "Example University") || !strings.Contains(got, "no site skill") {
		t.Errorf("site paragraph missing:\n%s", got)
	}
}

// The generic-site paragraph ("no profile, never guess") isn't in the broker's
// index, so it stays in the trimmed prompt.
func TestSystemPromptBrokerIndexKeepsGenericParagraph(t *testing.T) {
	h := newHarness(t)
	h.env.Site = SiteInfo{Skill: "site-generic", Generic: true}
	got := SystemPromptFor(h.env, true)
	if !strings.Contains(got, "no site profile in Labmill") || !strings.Contains(got, "Never guess one") {
		t.Errorf("generic paragraph missing:\n%s", got)
	}
}
