package slurm

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"labmill-client/internal/backend"
)

func TestScriptDirs(t *testing.T) {
	cases := []struct {
		name, script string
		want         []string
	}{
		{"rendered", "#!/bin/bash\n#SBATCH --job-name=x\n#SBATCH --chdir=/s/a\n#SBATCH --output=/s/a/%x-%j.out\npython t.py\n", []string{"/s/a"}},
		{"space and short", "#!/bin/bash\n#SBATCH -D /s/a\n#SBATCH -o logs/%x.out\n#SBATCH --error /s/err/e.txt\n", []string{"/s/a", "/s/a/logs", "/s/err"}},
		{"bare output", "#SBATCH --chdir /s/a\n#SBATCH --output=%x_%j.out\n", []string{"/s/a"}},
		{"relative without chdir", "#SBATCH --output=logs/x.out\n", nil},
		{"variable", "#SBATCH --chdir=$HOME/x\n#SBATCH --output=/s/$USER/o.out\n", nil},
		{"after the block", "#!/bin/bash\necho hi\n#SBATCH --chdir=/s/late\n", nil},
		{"comments and blanks in block", "#!/bin/bash\n# note\n\n#SBATCH --chdir=/s/a\n", []string{"/s/a"}},
		{"odd characters", "#SBATCH --chdir=/s/a;rm\n", nil},
	}
	for _, tc := range cases {
		if got := ScriptDirs(tc.script); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The mkdir comes from the script handed to Submit (the edited one), and the
// spec's work dir only stands in when the script has no --chdir.
func TestSubmitMkdirFollowsScript(t *testing.T) {
	r := &fakeRunner{replies: map[string]backend.Result{"mkdir": {}, "sbatch": {Stdout: "7\n"}}}
	script := "#!/bin/bash\n#SBATCH --chdir=/scratch/alice/edited\n#SBATCH --output=/scratch/alice/edited/logs/%x.out\necho\n"
	if _, err := (&Slurm{Runner: r}).Submit(context.Background(), script, spec()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[0], " "); got != "mkdir -p -- /scratch/alice/edited /scratch/alice/edited/logs" {
		t.Errorf("mkdir = %q", got)
	}
	r = &fakeRunner{replies: map[string]backend.Result{"mkdir": {}, "sbatch": {Stdout: "7\n"}}}
	if _, err := (&Slurm{Runner: r}).Submit(context.Background(), "#!/bin/bash\necho\n", spec()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.calls[0], " "); got != "mkdir -p -- /scratch/alice/run1" {
		t.Errorf("fallback mkdir = %q", got)
	}
	// An edit that moves the job to /tmp is refused before anything runs.
	r = &fakeRunner{}
	if _, err := (&Slurm{Runner: r}).Submit(context.Background(), "#SBATCH --chdir=/scratch/a\n#SBATCH --output=/tmp/o.out\n", spec()); err == nil || !strings.Contains(err.Error(), "node-local") || len(r.calls) != 0 {
		t.Errorf("err = %v, calls = %v", err, r.calls)
	}
}
