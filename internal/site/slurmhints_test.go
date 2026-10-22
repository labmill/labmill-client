package site

import (
	"strings"
	"testing"

	"labmill-client/internal/rules"
)

const slurmHead = "name: c\ntype: slurm\nssh_host: h\ntransfer: sftp\n"

func TestParseClusterSlurmHints(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want rules.SlurmHints
		bad  string // "" = valid; else a substring of the error
	}{
		{"none", slurmHead, rules.SlurmHints{}, ""},
		{"all", slurmHead + `
preemptible: {partition: preempt, qos: lowprio, requeue: true, fallback_partition: compute}
local_scratch: {path: /lscratch, env_var: TMPDIR, size_gb: 500}
global_scratch: {path: "/scratch/<user>", purge_days: 14}
`, rules.SlurmHints{
			Preemptible:   &rules.Preemptible{Partition: "preempt", QOS: "lowprio", Requeue: true, FallbackPartition: "compute"},
			LocalScratch:  &rules.LocalScratch{Path: "/lscratch", EnvVar: "TMPDIR", SizeGB: 500},
			GlobalScratch: &rules.GlobalScratch{Path: "/scratch/<user>", PurgeDays: 14},
		}, ""},
		{"unknown key", slurmHead + "preemptible: {partition: p, speed: fast}\n", rules.SlurmHints{}, "speed"},
		{"invalid value", slurmHead + "local_scratch: {path: tmp}\n", rules.SlurmHints{}, "local_scratch.path"},
		{"preemptible empty", slurmHead + "preemptible: {requeue: true}\n", rules.SlurmHints{}, "partition or a qos"},
		{"no template", "global_scratch: {path: /s}\n", rules.SlurmHints{}, "only for a slurm cluster"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cf, err := ParseCluster("c", []byte(tc.yaml))
			if tc.bad != "" {
				if err == nil || !strings.Contains(err.Error(), tc.bad) {
					t.Fatalf("err = %v, want one containing %q", err, tc.bad)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := cf.Slurm
			if (got.Preemptible == nil) != (tc.want.Preemptible == nil) || got.Preemptible != nil && *got.Preemptible != *tc.want.Preemptible {
				t.Errorf("preemptible = %+v, want %+v", got.Preemptible, tc.want.Preemptible)
			}
			if (got.LocalScratch == nil) != (tc.want.LocalScratch == nil) || got.LocalScratch != nil && *got.LocalScratch != *tc.want.LocalScratch {
				t.Errorf("local_scratch = %+v, want %+v", got.LocalScratch, tc.want.LocalScratch)
			}
			if (got.GlobalScratch == nil) != (tc.want.GlobalScratch == nil) || got.GlobalScratch != nil && *got.GlobalScratch != *tc.want.GlobalScratch {
				t.Errorf("global_scratch = %+v, want %+v", got.GlobalScratch, tc.want.GlobalScratch)
			}
		})
	}
}

// Every embedded Slurm profile with hints passes validation (they are parsed
// by Load), and SlurmHintsFor finds them by the template's cluster name.
func TestEmbeddedSlurmHints(t *testing.T) {
	for _, s := range List() {
		for _, f := range s.Folders {
			h := s.SlurmHintsFor(templateName(f))
			if h.Zero() {
				continue
			}
			if f.Template == nil || f.Template.Type != "slurm" {
				t.Errorf("%s/%s: hints on a non-Slurm cluster", s.Domain, f.Dir)
			}
			if err := h.Validate(); err != nil {
				t.Errorf("%s/%s: %v", s.Domain, f.Dir, err)
			}
		}
	}
	if got := (Site{}).SlurmHintsFor("nope"); !got.Zero() {
		t.Errorf("unknown cluster has hints: %+v", got)
	}
}

func templateName(f ClusterFolder) string {
	if f.Template == nil {
		return ""
	}
	return f.Template.Name
}
