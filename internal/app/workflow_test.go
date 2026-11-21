package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/store"
)

func TestWorkflowNote(t *testing.T) {
	e := func(ps ...string) []store.ManifestEntry {
		var out []store.ManifestEntry
		for _, p := range ps {
			out = append(out, store.ManifestEntry{Path: p})
		}
		return out
	}
	if got := workflowNote("/r", e("a.py", "data/x.csv", "notes.nfo")); got != "" {
		t.Errorf("false hit: %q", got)
	}
	got := workflowNote("/r", e("proj/workflow/Snakefile", "proj/rules/map.SMK", "proj/main.nf", "proj/nextflow.config"))
	for _, want := range []string{"Snakemake (/r/proj/rules/map.SMK, /r/proj/workflow/Snakefile)", "Nextflow (/r/proj/main.nf, /r/proj/nextflow.config)", "one small controller job"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
}

func TestStageDataNamesWorkflowManager(t *testing.T) {
	h := newHarness(t)
	p := filepath.Join(h.home, "data", "flow", "Snakefile")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("rule all:\n  input: 'x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/flow"}, "job_name": "flow"})
	if err != nil || !strings.Contains(out, `"workflow":"Workflow manager files found: Snakemake`) {
		t.Fatalf("%q %v", out, err)
	}
	// The workflow files can be read page by page.
	out, err = h.run(t, "preview_file", map[string]any{"path": h.home + "/data/flow/Snakefile"})
	if err != nil || !strings.Contains(out, "rule all") {
		t.Fatalf("preview: %q %v", out, err)
	}
}
