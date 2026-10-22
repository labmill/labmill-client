package slurm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/rules"
)

func baseSpec() backend.JobSpec {
	return backend.JobSpec{Name: "seg", Command: "python train.py", WorkDir: "/storage/me/labmill/seg"}
}

func TestRenderLocalScratch(t *testing.T) {
	tests := []struct {
		name  string
		local rules.LocalScratch
		want  string
	}{
		{"env var with fallback path", rules.LocalScratch{Path: "/tmp", EnvVar: "TMPDIR"}, `LABMILL_WORK="${TMPDIR:-/tmp}/labmill_${SLURM_JOB_ID}"`},
		{"path only", rules.LocalScratch{Path: "/scratch"}, `LABMILL_WORK="/scratch/labmill_${SLURM_JOB_ID}"`},
		{"variable path", rules.LocalScratch{Path: "$TMPDIR"}, `LABMILL_WORK="${TMPDIR:?no node-local folder: TMPDIR is not set}/labmill_${SLURM_JOB_ID}"`},
		{"env var only", rules.LocalScratch{EnvVar: "SLURM_TMPDIR"}, `LABMILL_WORK="${SLURM_TMPDIR:?no node-local folder: SLURM_TMPDIR is not set}/labmill_${SLURM_JOB_ID}"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec()
			spec.LocalScratch = &tc.local
			got, err := (&Slurm{}).Render(spec)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(got, "\n")
			for _, w := range []string{
				tc.want,
				"#SBATCH --nodes=1",
				"#SBATCH --chdir=/storage/me/labmill/seg",
				"set -eo pipefail",
				"LABMILL_JOB=/storage/me/labmill/seg",
				`LABMILL_LOG="${SLURM_JOB_NAME}-${SLURM_JOB_ID}.out"`,
				`[ -z "${SLURM_RESTART_COUNT:-}" ] || rm -rf -- "$LABMILL_WORK"`,
				`mkdir -p "$LABMILL_WORK/job"`,
				`trap 'if rsync -a --update --exclude="/$LABMILL_LOG" "$LABMILL_WORK/job/" "$LABMILL_JOB/"; then rm -rf -- "$LABMILL_WORK"; else echo "Labmill: stage-out failed; results kept in $LABMILL_WORK on $(hostname)" >&2; exit 1; fi' EXIT`,
				`rsync -a --exclude="/$LABMILL_LOG" "$LABMILL_JOB/" "$LABMILL_WORK/job/"`,
				`cd "$LABMILL_WORK/job"`,
				"#SBATCH --signal=B:USR1@120",
				`trap 'kill -TERM -- "-$LABMILL_PID" 2>/dev/null || true; ( sleep 60; kill -KILL -- "-$LABMILL_PID" ) 2>/dev/null & LABMILL_DOG=$!; wait "$LABMILL_PID" 2>/dev/null || true; pkill -P "$LABMILL_DOG" 2>/dev/null || true; kill "$LABMILL_DOG" 2>/dev/null || true; exit 143' USR1`,
				"set -m", "set +m",
				"(", "set +e +o pipefail", "python train.py", ") &", "LABMILL_PID=$!", `wait "$LABMILL_PID"`,
			} {
				if !slices.Contains(lines, w) {
					t.Errorf("script lacks line %q:\n%s", w, got)
				}
			}
			// The trap is set before the stage-in; the program runs last.
			if strings.Index(got, "trap ") > strings.Index(got, `rsync -a --exclude="/$LABMILL_LOG" "$LABMILL_JOB/"`) || !strings.HasSuffix(got, "wait \"$LABMILL_PID\"\n") {
				t.Errorf("wrong order:\n%s", got)
			}
		})
	}
}

func TestRenderLocalScratchMultiLineCommand(t *testing.T) {
	spec := baseSpec()
	spec.LocalScratch = &rules.LocalScratch{Path: "/tmp"}
	spec.Command = "module load x\npython a.py\n"
	got, err := (&Slurm{}).Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "(\nset +e +o pipefail\nmodule load x\npython a.py\n) &\n") {
		t.Errorf("multi-line command not wrapped:\n%s", got)
	}
}

func TestRenderLocalScratchRefusesMultiNode(t *testing.T) {
	spec := baseSpec()
	spec.Nodes = 2
	spec.LocalScratch = &rules.LocalScratch{Path: "/tmp"}
	if _, err := (&Slurm{}).Render(spec); !errors.Is(err, backend.ErrInvalidSpec) || !strings.Contains(err.Error(), "single-node") {
		t.Errorf("err = %v", err)
	}
	spec.Nodes = 1
	if _, err := (&Slurm{}).Render(spec); err != nil {
		t.Errorf("one node: %v", err)
	}
	spec.LocalScratch = &rules.LocalScratch{Path: "relative"}
	if _, err := (&Slurm{}).Render(spec); !errors.Is(err, backend.ErrInvalidSpec) {
		t.Errorf("bad local path: %v", err)
	}
}

func TestRenderPreemptibleHeader(t *testing.T) {
	spec := baseSpec()
	spec.Partition, spec.QOS, spec.Requeue, spec.Nodes = "preempt", "lowprio", true, 2
	got, err := (&Slurm{}).Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"#SBATCH --partition=preempt\n", "#SBATCH --qos=lowprio\n", "#SBATCH --requeue\n", "#SBATCH --nodes=2\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q:\n%s", w, got)
		}
	}
	// Without the new fields the script is what it always was.
	plain, _ := (&Slurm{}).Render(baseSpec())
	for _, bad := range []string{"--qos", "--requeue", "--nodes", "trap"} {
		if strings.Contains(plain, bad) {
			t.Errorf("plain script has %q:\n%s", bad, plain)
		}
	}
	if !strings.HasSuffix(plain, "python train.py\n") {
		t.Errorf("plain script changed:\n%s", plain)
	}
	spec.QOS = "bad qos"
	if _, err := (&Slurm{}).Render(spec); !errors.Is(err, backend.ErrInvalidSpec) {
		t.Errorf("bad qos: %v", err)
	}
}

// The wrapper really works: it stages in, runs on the local folder, copies
// the results back and removes the local folder; a failing program still
// copies back; the live output file is left alone.
func TestLocalScratchScriptRuns(t *testing.T) {
	for _, tool := range []string{"bash", "rsync"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	for _, fail := range []bool{false, true} {
		job, local := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(job, "in.txt"), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		logPath := filepath.Join(job, "seg-77.out")
		if err := os.WriteFile(logPath, []byte("live log"), 0o644); err != nil {
			t.Fatal(err)
		}
		spec := baseSpec()
		// t.TempDir is under /tmp, which Render refuses as a work folder: render
		// with a shared-looking path and point the script at the real folder.
		spec.LocalScratch = &rules.LocalScratch{Path: "/nonexistent", EnvVar: "TMPDIR"}
		spec.Command = "cp in.txt out.txt\necho extra >> seg-77.out"
		if fail {
			spec.Command += "\nexit 3"
		}
		script, err := (&Slurm{}).Render(spec)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", strings.ReplaceAll(script, spec.WorkDir, job))
		cmd.Env = append(os.Environ(), "TMPDIR="+local, "SLURM_JOB_ID=77", "SLURM_JOB_NAME=seg")
		out, err := cmd.CombinedOutput()
		if (err != nil) != fail {
			t.Fatalf("fail=%v: err = %v\n%s", fail, err, out)
		}
		if b, _ := os.ReadFile(filepath.Join(job, "out.txt")); string(b) != "data" {
			t.Errorf("fail=%v: result not copied back (%q)\n%s", fail, b, out)
		}
		if b, _ := os.ReadFile(logPath); string(b) != "live log" {
			t.Errorf("fail=%v: the live output file was overwritten: %q", fail, b)
		}
		if left, _ := os.ReadDir(local); len(left) != 0 {
			t.Errorf("fail=%v: local folder not removed: %v", fail, left)
		}
	}
}

func TestEstimateStart(t *testing.T) {
	tests := []struct {
		name string
		res  backend.Result
		want backend.StartEstimate
		err  bool
	}{
		{"stderr message", backend.Result{Stderr: "sbatch: Job 1234 to start at 2026-10-15T14:20:00 using 4 processors on nodes n01 in partition preempt\n"},
			backend.StartEstimate{Start: "2026-10-15T14:20:00", Nodes: "n01", Partition: "preempt"}, false},
		{"short message", backend.Result{Stderr: "sbatch: Job 7 to start at 2026-10-15T14:20:00\n"}, backend.StartEstimate{Start: "2026-10-15T14:20:00"}, false},
		{"no estimate", backend.Result{Stderr: "sbatch: nothing useful\n"}, backend.StartEstimate{}, true},
		{"failed", backend.Result{ExitCode: 1, Stderr: "sbatch: error: invalid partition"}, backend.StartEstimate{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{replies: map[string]backend.Result{"sbatch": tc.res}}
			got, err := (&Slurm{Runner: r}).EstimateStart(context.Background(), "#!/bin/bash\necho hi\n")
			if (err != nil) != tc.err || got != tc.want {
				t.Errorf("got %+v, %v; want %+v (err %v)", got, err, tc.want, tc.err)
			}
			if len(r.calls) != 1 || strings.Join(r.calls[0], " ") != "sbatch --test-only" {
				t.Errorf("calls = %v", r.calls)
			}
		})
	}
}

func TestRenderMailOnEnd(t *testing.T) {
	spec := baseSpec()
	off, _ := (&Slurm{}).Render(spec)
	if strings.Contains(off, "--mail") {
		t.Errorf("mail line without the setting:\n%s", off)
	}
	spec.MailOnEnd = true
	on, err := (&Slurm{}).Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(on, "#SBATCH --mail-type=END,FAIL\n") || strings.Contains(on, "--mail-user") {
		t.Errorf("want mail-type only:\n%s", on)
	}
}
