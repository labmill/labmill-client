package backend

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestQuoteRoundTripThroughShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	args := []string{"printf", "%s|", "plain", "with space", "it's", `"dq"`, "$(rm -rf /)", "`id`", "a;b", "", "new\nline", "*", "~", "back\\slash"}
	out, err := exec.Command("/bin/sh", "-c", Quote(args)).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(args[2:], "|") + "|"
	if string(out) != want {
		t.Errorf("shell saw %q, want %q (quoted: %s)", out, want, Quote(args))
	}
}

func TestQuoteSafeArgsUnquoted(t *testing.T) {
	if got := Quote([]string{"sbatch", "--parsable"}); got != "sbatch --parsable" {
		t.Errorf("got %q", got)
	}
	if got := Quote([]string{"tail", "-n", "100", "--", "/scratch/a b/x.out"}); got != "tail -n 100 -- '/scratch/a b/x.out'" {
		t.Errorf("got %q", got)
	}
}

func validSpec() JobSpec {
	return JobSpec{Name: "train-1", Partition: "gpu", GPUs: 2, GPUType: "a100", CPUs: 8, MemoryGB: 32,
		Command: "python train.py", WorkDir: "/scratch/alice/run1", Env: map[string]string{"SEED": "1"}}
}

func TestValidate(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec: %v", err)
	}
	bad := map[string]func(*JobSpec){
		"empty name":     func(s *JobSpec) { s.Name = "" },
		"name chars":     func(s *JobSpec) { s.Name = "a b" },
		"name injection": func(s *JobSpec) { s.Name = "x\n#SBATCH --uid=0" },
		"long name":      func(s *JobSpec) { s.Name = strings.Repeat("a", 65) },
		"neg gpus":       func(s *JobSpec) { s.GPUs = -1 },
		"neg cpus":       func(s *JobSpec) { s.CPUs = -1 },
		"neg mem":        func(s *JobSpec) { s.MemoryGB = -1 },
		"neg time":       func(s *JobSpec) { s.Time = -1 },
		"relative wd":    func(s *JobSpec) { s.WorkDir = "scratch/x" },
		"wd newline":     func(s *JobSpec) { s.WorkDir = "/x\n#SBATCH --x" },
		"partition":      func(s *JobSpec) { s.Partition = "gpu;rm" },
		"gpu type":       func(s *JobSpec) { s.GPUType = "a100 --x" },
		"env name":       func(s *JobSpec) { s.Env = map[string]string{"1BAD": "x"} },
		"empty command":  func(s *JobSpec) { s.Command = "  " },
	}
	for name, mut := range bad {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mut(&s)
			if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("err = %v, want ErrInvalidSpec", err)
			}
		})
	}
}
