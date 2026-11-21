package app

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"labmill-client/internal/store"
)

// Workflow managers (owner queue (a)): stage_data names the manager when the
// staged file names show one, so the model reads the workflow files and runs
// the manager's controller as one small job (skills: research-computing,
// workflow-managers.md). Only file names are looked at; no file is read.

// workflowFiles returns, per manager, the staged paths that show it, shortest
// first, at most 5 each.
func workflowFiles(entries []store.ManifestEntry) map[string][]string {
	found := map[string][]string{}
	for _, e := range entries {
		base := strings.ToLower(path.Base(e.Path))
		var mgr string
		switch {
		case base == "snakefile" || strings.HasSuffix(base, ".smk"):
			mgr = "Snakemake"
		case base == "nextflow.config" || strings.HasSuffix(base, ".nf"):
			mgr = "Nextflow"
		default:
			continue
		}
		found[mgr] = append(found[mgr], e.Path)
	}
	for m, ps := range found {
		sort.Slice(ps, func(i, j int) bool { return len(ps[i]) < len(ps[j]) || len(ps[i]) == len(ps[j]) && ps[i] < ps[j] })
		if len(ps) > 5 {
			ps = ps[:5]
		}
		found[m] = ps
	}
	return found
}

// workflowNote is the hint for stage_data's answer, "" when no manager shows.
func workflowNote(root string, entries []store.ManifestEntry) string {
	found := workflowFiles(entries)
	if len(found) == 0 {
		return ""
	}
	var parts []string
	for _, m := range []string{"Snakemake", "Nextflow"} {
		if ps := found[m]; len(ps) > 0 {
			names := make([]string, len(ps))
			for i, p := range ps {
				names[i] = sanitize(filepath.Join(root, p))
			}
			parts = append(parts, m+" ("+strings.Join(names, ", ")+")")
		}
	}
	return "Workflow manager files found: " + strings.Join(parts, "; ") +
		". Read them (preview_file, all pages) and follow the workflow-managers skill: do not write one big script for the whole pipeline. " +
		"Run the manager as one small controller job that submits the steps to Slurm, with its work folder on shared scratch. Show the user the plan."
}
