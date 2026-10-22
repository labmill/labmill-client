package app

import (
	"encoding/json"
	"strings"
	"testing"

	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/skill"
	"labmill-client/skills"
)

// TestRuntimeTextHoldsNoSiteFacts: the system prompt and every tool's
// description and input schema stay free of site facts (host_path roots,
// domains, cluster and host names). Those come from the site profile at
// runtime, so the fixed text is the same for every site.
func TestRuntimeTextHoldsNoSiteFacts(t *testing.T) {
	h := newHarness(t)
	// A fixture cluster set unlike the harness's, so only fixed text can match.
	h.env.Config = &config.Config{Clusters: []config.Cluster{
		{Name: "bigcluster", Type: config.TypeSlurm, SSHHost: "bigjump"},
	}}
	set, err := skill.Load(skills.FS, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	h.env.Skills = set
	h.env.Site = SiteInfo{
		Name:  "Example University",
		Skill: "site-example-edu",
		Clusters: map[string]ClusterHints{
			"bigcluster": {Slurm: rules.SlurmHints{GlobalScratch: &rules.GlobalScratch{Path: "/fixture-root"}}},
		},
	}
	// The fixture's own private terms.
	terms := []string{
		"/storage", "/fixture-root", "hpclogin", "hpcfiles", "hpcone", "hpc.",
	}
	check := func(what, text string) {
		t.Helper()
		lower := strings.ToLower(text)
		for _, term := range terms {
			if strings.Contains(lower, term) {
				t.Errorf("%s names the site term %q", what, term)
			}
		}
	}
	sp := SystemPrompt(h.env)
	// Without the skill-to-tool map in the prompt the test would not cover it.
	if !strings.Contains(sp, "How skill steps map to Labmill's tools") {
		t.Fatal("system prompt lacks the skillToolMap section")
	}
	check("SystemPrompt", sp)
	for _, tool := range Tools(h.env) {
		check("tool "+tool.Name+" description", tool.Description)
		b, err := json.Marshal(tool.Schema)
		if err != nil {
			t.Fatal(err)
		}
		check("tool "+tool.Name+" schema", string(b))
	}
}
