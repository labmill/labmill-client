//go:build unix

package slurm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"labmill-client/internal/rules"
)

func needTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "rsync"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
}

func TestSignalLead(t *testing.T) {
	for _, tc := range []struct {
		limit time.Duration
		want  int
	}{{0, 120}, {5 * time.Minute, 60}, {time.Hour, 360}, {24 * time.Hour, 600}} {
		if got := signalLead(tc.limit); got != tc.want {
			t.Errorf("signalLead(%v) = %d, want %d", tc.limit, got, tc.want)
		}
	}
	spec := baseSpec()
	spec.Time = time.Hour
	spec.LocalScratch = &rules.LocalScratch{Path: "/tmp"}
	got, _ := (&Slurm{}).Render(spec)
	if !strings.Contains(got, "#SBATCH --signal=B:USR1@360\n") {
		t.Errorf("no signal line:\n%s", got)
	}
}

// A program that changes a file straight in the shared job folder (by its
// absolute path) keeps its change: the stale staged-in copy does not win.
func TestLocalScratchKeepsFilesChangedInTheSharedFolder(t *testing.T) {
	needTools(t)
	job, local := t.TempDir(), t.TempDir()
	res := filepath.Join(job, "res.txt")
	if err := os.WriteFile(res, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(res, past, past); err != nil {
		t.Fatal(err)
	}
	spec := baseSpec()
	spec.LocalScratch = &rules.LocalScratch{Path: "/nonexistent", EnvVar: "TMPDIR"}
	spec.Command = "echo new > " + spec.WorkDir + "/res.txt"
	script, err := (&Slurm{}).Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", strings.ReplaceAll(script, spec.WorkDir, job))
	cmd.Env = append(os.Environ(), "TMPDIR="+local, "SLURM_JOB_ID=78", "SLURM_JOB_NAME=seg")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(res); string(b) != "new\n" {
		t.Errorf("res.txt = %q, want the program's change", b)
	}
}

// runScript runs the rendered script under bash with the shared folder job
// and the node-local folder local; the exit code is returned.
func runScript(t *testing.T, command string, job, local string, signalAfter func(pid string, cmd *exec.Cmd)) int {
	t.Helper()
	spec := baseSpec()
	spec.LocalScratch = &rules.LocalScratch{Path: "/nonexistent", EnvVar: "TMPDIR"}
	spec.Command = command
	script, err := (&Slurm{}).Render(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", strings.ReplaceAll(script, spec.WorkDir, job))
	cmd.Env = append(os.Environ(), "TMPDIR="+local, "SLURM_JOB_ID=79", "SLURM_JOB_NAME=seg")
	if signalAfter == nil {
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode()
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	signalAfter(filepath.Join(local, "labmill_79", "job", "pid.txt"), cmd)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the script did not stop")
	}
	return cmd.ProcessState.ExitCode()
}

// The exit status of the program is the script's, as before the wrapper.
func TestLocalScratchExitCodes(t *testing.T) {
	needTools(t)
	for _, tc := range []struct {
		command string
		want    int
	}{{"true", 0}, {"exit 3", 3}, {"false", 1}} {
		if got := runScript(t, tc.command, t.TempDir(), t.TempDir(), nil); got != tc.want {
			t.Errorf("%q: exit %d, want %d", tc.command, got, tc.want)
		}
	}
}

// USR1 (sent before the time limit) stops the program and its children (the
// whole process group, no orphan), lets a TERM handler write a checkpoint,
// stages everything out, removes the local folder and exits 143.
func TestLocalScratchUSR1StagesOut(t *testing.T) {
	needTools(t)
	job, local := t.TempDir(), t.TempDir()
	command := `bash -c 'echo $$ > pid.txt; trap "echo checkpoint > ckpt.txt; exit 0" TERM; while :; do sleep 1; done'`
	got := runScript(t, command, job, local, func(pidFile string, cmd *exec.Cmd) {
		for i := 0; i < 100; i++ { // wait until the program has started
			if b, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(b), "\n") {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		if err := cmd.Process.Signal(syscall.SIGUSR1); err != nil {
			t.Fatal(err)
		}
	})
	if got != 143 {
		t.Errorf("exit %d, want 143", got)
	}
	if b, _ := os.ReadFile(filepath.Join(job, "ckpt.txt")); string(b) != "checkpoint\n" {
		t.Errorf("checkpoint not staged out: %q", b)
	}
	if left, _ := os.ReadDir(local); len(left) != 0 {
		t.Errorf("local folder not removed: %v", left)
	}
	b, err := os.ReadFile(filepath.Join(job, "pid.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("the program (pid %d) is still running after USR1", pid)
	}
}
