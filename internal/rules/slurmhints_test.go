package rules

import (
	"strings"
	"testing"
)

func TestSlurmHintsValidate(t *testing.T) {
	tests := []struct {
		name string
		h    SlurmHints
		bad  string // "" = valid; else a substring of the error
	}{
		{"zero", SlurmHints{}, ""},
		{"all", SlurmHints{
			Preemptible:   &Preemptible{Partition: "preempt", QOS: "lowprio", Requeue: true, FallbackPartition: "compute"},
			LocalScratch:  &LocalScratch{Path: "/tmp", EnvVar: "TMPDIR", SizeGB: 400},
			GlobalScratch: &GlobalScratch{Path: "/scratch/<group>", PurgeDays: 90},
		}, ""},
		{"software dir", SlurmHints{SoftwareDir: "/software/mygroup"}, ""},
		{"software dir relative", SlurmHints{SoftwareDir: "software"}, "software_dir"},
		{"software dir dotdot", SlurmHints{SoftwareDir: "/a/../b"}, "software_dir"},
		{"qos only", SlurmHints{Preemptible: &Preemptible{QOS: "low"}}, ""},
		{"variable path", SlurmHints{LocalScratch: &LocalScratch{Path: "$TMPDIR/jobs"}}, ""},
		{"env var only", SlurmHints{LocalScratch: &LocalScratch{EnvVar: "SLURM_TMPDIR"}}, ""},
		{"preemptible empty", SlurmHints{Preemptible: &Preemptible{Requeue: true}}, "partition or a qos"},
		{"partition chars", SlurmHints{Preemptible: &Preemptible{Partition: "a b"}}, "preemptible.partition"},
		{"qos leading dash", SlurmHints{Preemptible: &Preemptible{Partition: "p", QOS: "-x"}}, "preemptible.qos"},
		{"fallback same", SlurmHints{Preemptible: &Preemptible{Partition: "p", FallbackPartition: "p"}}, "must differ"},
		{"local empty", SlurmHints{LocalScratch: &LocalScratch{SizeGB: 5}}, "path or env_var"},
		{"local relative", SlurmHints{LocalScratch: &LocalScratch{Path: "tmp"}}, "local_scratch.path"},
		{"local root", SlurmHints{LocalScratch: &LocalScratch{Path: "/"}}, "local_scratch.path"},
		{"local dotdot", SlurmHints{LocalScratch: &LocalScratch{Path: "/tmp/../x"}}, "local_scratch.path"},
		{"local injection", SlurmHints{LocalScratch: &LocalScratch{Path: "/tmp/$(rm)"}}, "local_scratch.path"},
		{"local trailing slash", SlurmHints{LocalScratch: &LocalScratch{Path: "/tmp/"}}, "local_scratch.path"},
		{"env var name", SlurmHints{LocalScratch: &LocalScratch{Path: "/tmp", EnvVar: "A-B"}}, "env_var"},
		{"negative size", SlurmHints{LocalScratch: &LocalScratch{Path: "/tmp", SizeGB: -1}}, "size_gb"},
		{"global empty", SlurmHints{GlobalScratch: &GlobalScratch{}}, "global_scratch.path is required"},
		{"global relative", SlurmHints{GlobalScratch: &GlobalScratch{Path: "scratch"}}, "global_scratch.path"},
		{"global root", SlurmHints{GlobalScratch: &GlobalScratch{Path: "/"}}, "global_scratch.path"},
		{"global unclosed", SlurmHints{GlobalScratch: &GlobalScratch{Path: "/s/<user"}}, "placeholder"},
		{"global trailing slash", SlurmHints{GlobalScratch: &GlobalScratch{Path: "/s/"}}, "global_scratch.path"},
		{"purge range", SlurmHints{GlobalScratch: &GlobalScratch{Path: "/s", PurgeDays: -2}}, "purge_days"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.h.Validate()
			switch {
			case tc.bad == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.bad != "" && (err == nil || !strings.Contains(err.Error(), tc.bad)):
				t.Errorf("error = %v, want one containing %q", err, tc.bad)
			}
		})
	}
}

func TestGlobalScratchCovers(t *testing.T) {
	tests := []struct {
		path, dir string
		want      bool
	}{
		{"/scratch", "/scratch", true},
		{"/scratch", "/scratch/me/labmill/j", true},
		{"/scratch", "/scratchy/j", false},
		{"/scratch/<group>", "/scratch/lab/labmill/j", true},
		{"/scratch/<group>", "/scratch", false},
		{"/share/<user>", "/share/me", true},
		{"/home", "/storage/x", false},
	}
	for _, tc := range tests {
		if got := (GlobalScratch{Path: tc.path}).Covers(tc.dir); got != tc.want {
			t.Errorf("%s covers %s = %v, want %v", tc.path, tc.dir, got, tc.want)
		}
	}
}
