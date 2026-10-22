package slurm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/backend"
)

// venvRig runs the rendered venv script with a stub python3 and a slow stub
// pip that logs every install.
type venvRig struct {
	t               *testing.T
	home, job, logf string
	script          string
}

func newVenvRig(t *testing.T) *venvRig {
	t.Helper()
	for _, tool := range []string{"bash", "sha256sum", "find", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	root := t.TempDir()
	r := &venvRig{t: t, home: filepath.Join(root, "home"), job: filepath.Join(root, "job"), logf: filepath.Join(root, "pip.log")}
	bin := filepath.Join(root, "bin")
	for _, d := range []string{bin, r.home, r.job} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\n[ \"$1\" != -c ] || { echo '3.11 /usr'; exit 0; }\nmkdir -p \"$3/bin\"\n" +
		"printf '#!/bin/sh\\nsleep ${LABMILL_PIPSLEEP:-1}\\necho \"$@\" >> \"$LABMILL_PIPLOG\"\\n' > \"$3/bin/pip\"\nchmod +x \"$3/bin/pip\"\n: > \"$3/bin/activate\"\n"
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.job, "requirements.txt"), []byte("numpy==1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := envSpec(backend.Environment{Venv: &backend.VenvEnv{Requirements: "requirements.txt"}})
	s.Command = "echo ran"
	var err error
	if r.script, err = (&Slurm{}).Render(s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return r
}

func (r *venvRig) run() string {
	cmd := exec.Command("bash", "-c", r.script)
	cmd.Dir = r.job
	cmd.Env = append(os.Environ(), "HOME="+r.home, "LABMILL_PIPLOG="+r.logf, "LABMILL_ENVS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Errorf("script failed: %v\n%s", err, out)
	}
	return string(out)
}

func (r *venvRig) installs() int {
	data, _ := os.ReadFile(r.logf)
	return strings.Count(string(data), "install -r requirements.txt")
}

func (r *venvRig) envs() string { return filepath.Join(r.home, ".labmill", "envs") }

// Two jobs that start together build the venv once.
func TestRenderedVenvConcurrentRuns(t *testing.T) {
	r := newVenvRig(t)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); r.run() }()
	}
	wg.Wait()
	if n := r.installs(); n != 1 {
		t.Errorf("pip install ran %d times, want 1", n)
	}
	if left, _ := filepath.Glob(filepath.Join(r.envs(), "*.lock.d")); len(left) != 0 {
		t.Errorf("lock folders left behind: %v", left)
	}
}

// A folder without the marker is a crashed build: it is moved aside, not
// used and not deleted, and the venv is built again.
func TestRenderedVenvIncompleteMovedAside(t *testing.T) {
	r := newVenvRig(t)
	r.run()
	dirs, _ := filepath.Glob(filepath.Join(r.envs(), "venv-????????????????"))
	if len(dirs) != 1 {
		t.Fatalf("venv folders = %v", dirs)
	}
	if err := os.Remove(filepath.Join(dirs[0], ".labmill-ok")); err != nil {
		t.Fatal(err)
	}
	r.run()
	if n := r.installs(); n != 2 {
		t.Errorf("pip install ran %d times, want 2 (rebuild)", n)
	}
	if aside, _ := filepath.Glob(dirs[0] + ".incomplete.*"); len(aside) != 1 {
		t.Errorf("want the broken folder moved aside, got %v", aside)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], ".labmill-ok")); err != nil {
		t.Errorf("rebuilt venv has no marker: %v", err)
	}
}

// A lock folder older than the wait limit (left by a killed job) is moved
// aside and the build goes on; a fresh one is not touched.
func TestRenderedVenvStaleLock(t *testing.T) {
	r := newVenvRig(t)
	r.run()
	dirs, _ := filepath.Glob(filepath.Join(r.envs(), "venv-????????????????"))
	if len(dirs) != 1 {
		t.Fatalf("venv folders = %v", dirs)
	}
	if err := os.Remove(filepath.Join(dirs[0], ".labmill-ok")); err != nil {
		t.Fatal(err)
	}
	lock := dirs[0] + ".lock.d"
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	r.run()
	if stale, _ := filepath.Glob(lock + ".stale.*"); len(stale) != 1 {
		t.Errorf("want the stale lock moved aside, got %v", stale)
	}
	if _, err := os.Stat(filepath.Join(dirs[0], ".labmill-ok")); err != nil {
		t.Errorf("build did not go on: %v", err)
	}
	if _, err := os.Stat(lock); err == nil {
		t.Errorf("lock not released")
	}
}

// A build longer than the stale limit keeps its lock fresh, so a second job
// that starts late does not break in and build again; no refresher is left.
func TestRenderedVenvLongBuildKeepsLock(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not installed")
	}
	r := newVenvRig(t)
	t.Setenv("LABMILL_PIPSLEEP", "6")
	t.Setenv("LABMILL_LOCK_STALE_MIN", "0.025")         // 1.5 s
	refresh := fmt.Sprintf("0.%d", 300+os.Getpid()%400) // unique, so parallel test runs do not match each other
	t.Setenv("LABMILL_LOCK_REFRESH", refresh)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.run() }()
	time.Sleep(3 * time.Second)
	r.run()
	wg.Wait()
	if n := r.installs(); n != 1 {
		t.Errorf("pip install ran %d times, want 1", n)
	}
	if stale, _ := filepath.Glob(filepath.Join(r.envs(), "*.lock.d.stale.*")); len(stale) != 0 {
		t.Errorf("live lock moved aside: %v", stale)
	}
	// A stop that lands just as the refresher starts a new sleep can leave
	// that one sleep to run out (at most one interval; it does nothing), so
	// wait longer than one interval before looking.
	time.Sleep(1500 * time.Millisecond)
	if out, err := exec.Command("pgrep", "-f", "sleep "+refresh).Output(); err == nil {
		t.Errorf("refresher left running: %s", out)
	}
}

func TestRenderEnvironmentGPUAddsNv(t *testing.T) {
	s := envSpec(backend.Environment{Apptainer: &backend.ApptainerEnv{Image: "images/py.sif"}})
	s.GPUs = 1
	got, err := (&Slurm{}).Render(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `exec --nv "$LABMILL_IMG"`) {
		t.Errorf("no --nv for a GPU job:\n%s", got)
	}
}
