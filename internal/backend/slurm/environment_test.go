package slurm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/rules"
)

func envSpec(e backend.Environment) backend.JobSpec {
	s := baseSpec()
	s.Environment = &e
	return s
}

func TestRenderEnvironment(t *testing.T) {
	tests := []struct {
		name string
		env  backend.Environment
		want []string
		not  []string
	}{
		{"modules", backend.Environment{Modules: []string{"gcc/12.2", "cuda/12.1"}},
			[]string{"module load gcc/12.2 cuda/12.1 || exit 1\n", "\npython train.py\n"}, []string{"LABMILL_ENVS", "labmill_once"}},
		{"venv", backend.Environment{Venv: &backend.VenvEnv{Requirements: "requirements.txt"}, SoftwareDir: "/sw/me"},
			[]string{
				`LABMILL_ENVS="${LABMILL_ENVS:-/sw/me}"`,
				"LABMILL_REQ=requirements.txt\nLABMILL_PY=python3\n",
				`"$LABMILL_PY" -c 'import sys;print(sys.version,sys.base_prefix)'; uname -m; } | sha256sum | cut -c1-16`,
				`LABMILL_ENV="$LABMILL_ENVS/venv-$(`,
				`until mkdir -- "$labmill_l"`, `rmdir -- "$labmill_l"' EXIT`, `touch -c -- "$labmill_l"`, `.stale.`, // the mkdir lock
				`"$LABMILL_PY" -m venv "$1"`, `pip" install -r "$LABMILL_REQ"`, `touch "$1/.labmill-ok"`,
				`. "$LABMILL_ENV/bin/activate" || exit 1`, "\npython train.py\n",
			}, []string{"rm -rf", "$HOME/.labmill/envs"}},
		{"venv with python", backend.Environment{Modules: []string{"python/3.11"}, Venv: &backend.VenvEnv{Requirements: "req/r.txt", Python: "python3.11"}},
			[]string{"module load python/3.11 || exit 1\n", "LABMILL_PY=python3.11\n", `uname -m`, `${LABMILL_ENVS:-$HOME/.labmill/envs}`}, nil},
		{"conda", backend.Environment{Modules: []string{"miniforge"}, Conda: &backend.CondaEnv{File: "environment.yml"}},
			[]string{"LABMILL_CONDA_FILE=environment.yml", `conda-$({ cat -- "$LABMILL_CONDA_FILE"; uname -m; }`, `mamba env create -p "$1"`, `conda env create -p "$1"`, `conda activate "$LABMILL_ENV" || exit 1`, "flock"}, nil},
		{"apptainer docker", backend.Environment{Apptainer: &backend.ApptainerEnv{Image: "docker://ubuntu:22.04"}},
			[]string{`command -v apptainer || command -v singularity`, `APPTAINER_CACHEDIR="$LABMILL_ENVS/apptainer-cache"`, "LABMILL_REF=docker://ubuntu:22.04\n",
				`LABMILL_IMG="$LABMILL_ENVS/sif/ubuntu_22.04-`, `pull "$1.tmp.$$" "$LABMILL_REF"`, `mv -- "$1.tmp.$$" "$1"`,
				`"$LABMILL_APPT" exec "$LABMILL_IMG" bash -c 'python train.py'`}, []string{"--nv"}},
		{"apptainer gpu sif", backend.Environment{Apptainer: &backend.ApptainerEnv{Image: "images/py.sif", GPU: true}},
			[]string{"LABMILL_IMG=images/py.sif\n", `"$LABMILL_APPT" exec --nv "$LABMILL_IMG" bash -c`}, []string{"labmill_build_sif() {"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&Slurm{}).Render(envSpec(tc.env))
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("unexpected %q in:\n%s", n, got)
				}
			}
			if strings.Index(got, "#SBATCH --chdir") > strings.Index(got, "Labmill: software environment") {
				t.Errorf("setup before the header:\n%s", got)
			}
		})
	}
}

func TestRenderEnvironmentInvalid(t *testing.T) {
	two := backend.Environment{Venv: &backend.VenvEnv{Requirements: "r.txt"}, Conda: &backend.CondaEnv{File: "e.yml"}}
	for name, e := range map[string]backend.Environment{
		"two kinds":      two,
		"bad module":     {Modules: []string{"gcc; rm -rf /"}},
		"dash module":    {Modules: []string{"-x"}},
		"absolute req":   {Venv: &backend.VenvEnv{Requirements: "/etc/passwd"}},
		"dotdot req":     {Venv: &backend.VenvEnv{Requirements: "../r.txt"}},
		"dotdot inside":  {Venv: &backend.VenvEnv{Requirements: "a/../../r.txt"}},
		"dash req":       {Venv: &backend.VenvEnv{Requirements: "-r"}},
		"quote req":      {Venv: &backend.VenvEnv{Requirements: "r'.txt"}},
		"python slash":   {Venv: &backend.VenvEnv{Requirements: "r.txt", Python: "/bin/sh"}},
		"conda dotdot":   {Conda: &backend.CondaEnv{File: "../e.yml"}},
		"image option":   {Apptainer: &backend.ApptainerEnv{Image: "docker://-x"}},
		"image space":    {Apptainer: &backend.ApptainerEnv{Image: "docker://a b"}},
		"image no sif":   {Apptainer: &backend.ApptainerEnv{Image: "/tmp/x"}},
		"image dotdot":   {Apptainer: &backend.ApptainerEnv{Image: "../x.sif"}},
		"image empty":    {Apptainer: &backend.ApptainerEnv{}},
		"software dir":   {Modules: []string{"a"}, SoftwareDir: "rel"},
		"software quote": {Modules: []string{"a"}, SoftwareDir: `/a"b`},
	} {
		if _, err := (&Slurm{}).Render(envSpec(e)); !errors.Is(err, backend.ErrInvalidSpec) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec", name, err)
		}
	}
}

// The setup is inside the subshell that runs the command, after the
// stage-in and before the command, so it runs on the node and in the
// staged folder.
func TestRenderEnvironmentLocalScratch(t *testing.T) {
	s := envSpec(backend.Environment{Modules: []string{"gcc"}, Venv: &backend.VenvEnv{Requirements: "r.txt"}})
	s.LocalScratch = &rules.LocalScratch{Path: "/tmp"}
	got, err := (&Slurm{}).Render(s)
	if err != nil {
		t.Fatal(err)
	}
	var order []int
	for _, m := range []string{`cd "$LABMILL_WORK/job"`, "set +e +o pipefail\n", "module load gcc || exit 1", "labmill_once ", "\npython train.py\n) &"} {
		i := strings.Index(got, m)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", m, got)
		}
		order = append(order, i)
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Errorf("wrong order at %d in:\n%s", i, got)
		}
	}
}

// Runs the rendered venv script with stand-ins for python3 (a stub that makes
// a venv-like folder) twice: the second run reuses the environment.
func TestRenderedVenvBuildsOnce(t *testing.T) {
	for _, tool := range []string{"bash", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	root := t.TempDir()
	bin, home, job := filepath.Join(root, "bin"), filepath.Join(root, "home"), filepath.Join(root, "job")
	for _, d := range []string{bin, home, job} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := "#!/bin/sh\n[ \"$1\" != -c ] || { echo '3.11 /usr'; exit 0; }\n# stand-in for: python3 -m venv DIR\nmkdir -p \"$3/bin\"\n" +
		"printf '#!/bin/sh\\necho \"$@\" >> \"$LABMILL_PIPLOG\"\\n' > \"$3/bin/pip\"\nchmod +x \"$3/bin/pip\"\n: > \"$3/bin/activate\"\n"
	if err := os.WriteFile(filepath.Join(bin, "python3"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "requirements.txt"), []byte("numpy==1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := envSpec(backend.Environment{Venv: &backend.VenvEnv{Requirements: "requirements.txt"}})
	s.Command = "echo ran"
	script, err := (&Slurm{}).Render(s)
	if err != nil {
		t.Fatal(err)
	}
	pipLog := filepath.Join(root, "pip.log")
	run := func() string {
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = job
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "LABMILL_PIPLOG="+pipLog, "LABMILL_ENVS=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "ran") {
		t.Errorf("command did not run: %q", out)
	}
	run()
	data, _ := os.ReadFile(pipLog)
	if n := strings.Count(string(data), "install -r requirements.txt"); n != 1 {
		t.Errorf("pip install ran %d times, want 1:\n%s", n, data)
	}
	envs, _ := filepath.Glob(filepath.Join(home, ".labmill", "envs", "venv-????????????????", ".labmill-ok"))
	if len(envs) != 1 {
		t.Errorf("want one finished venv folder under $HOME/.labmill/envs, got %v", envs)
	}
	// A changed requirements file is a new environment; the old one stays.
	if err := os.WriteFile(filepath.Join(job, "requirements.txt"), []byte("numpy==2.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run()
	envs, _ = filepath.Glob(filepath.Join(home, ".labmill", "envs", "venv-????????????????", ".labmill-ok"))
	if len(envs) != 2 {
		t.Errorf("want two venv folders after a change, got %v", envs)
	}
}
