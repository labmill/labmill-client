package backend

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/sshx/sshtest"
)

func okSpec() JobSpec {
	return JobSpec{Name: "j", Command: "echo hi", WorkDir: "/scratch/a_b-1/x.y+z@h,k:v=w~"}
}

func TestWorkDirAlphabet(t *testing.T) {
	if err := okSpec().Validate(); err != nil {
		t.Fatalf("good work dir rejected: %v", err)
	}
	for _, wd := range []string{"/a#b", `/a"b`, `/a\b`, "/a%b", "/a$b", "/a`b", "/a'b", "/a b", "/a\tb", "/a\nb", "/é", "/a;b", "/a*b", "rel/path"} {
		s := okSpec()
		s.WorkDir = wd
		if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("WorkDir %q: err = %v, want ErrInvalidSpec", wd, err)
		}
	}
}

func TestCommandRejectsSbatchDirective(t *testing.T) {
	for _, c := range []string{"#SBATCH --uid=0", "echo hi\n  #SBATCH --uid=0\n", "echo\n\t#SBATCH --x"} {
		s := okSpec()
		s.Command = c
		if err := s.Validate(); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("Command %q: err = %v", c, err)
		}
	}
	s := okSpec()
	s.Command = "echo '#SBATCH is fine mid-line'\n# plain comment"
	if err := s.Validate(); err != nil {
		t.Errorf("benign command rejected: %v", err)
	}
}

func TestQuoteEquals(t *testing.T) {
	if got := Quote([]string{"env", "A=b", "--x=y", "a:b"}); got != "env 'A=b' --x=y a:b" {
		t.Errorf("got %q", got)
	}
	if runtime.GOOS == "windows" {
		return
	}
	out, err := exec.Command("/bin/sh", "-c", Quote([]string{"printf", "%s|", "=x", "A=b"})).Output()
	if err != nil || string(out) != "=x|A=b|" {
		t.Errorf("out = %q, %v", out, err)
	}
}

func TestSSHRunnerCancelReturnsPromptly(t *testing.T) {
	release := make(chan struct{})
	r := dialTest(t, map[string]sshtest.Handler{
		"hang": func(_ string, _ io.Reader, _, _ io.Writer) int { <-release; return 0 },
	})
	t.Cleanup(func() { close(release) }) // runs before the server's Close (LIFO)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, []string{"hang"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Run took %v after cancel", time.Since(start))
	}
}

func TestSSHRunnerSuccessSurvivesLaterCancel(t *testing.T) {
	r := dialTest(t, map[string]sshtest.Handler{
		"echo1": func(_ string, _ io.Reader, out, _ io.Writer) int { io.WriteString(out, "42\n"); return 0 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	res, err := r.Run(ctx, []string{"echo1"}, nil)
	cancel()
	if err != nil || !strings.Contains(res.Stdout, "42") {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
