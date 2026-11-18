package slurm

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
)

func TestOutput(t *testing.T) {
	// Separate stdout and stderr files; stdout is long (head, gap, tail), stderr is short.
	r := &fakeRunner{replies: map[string]backend.Result{
		"scontrol": {Stdout: "JobId=7 StdErr=/w/job-7.err\n   StdOut=/w/job-7.out\n"},
		"wc":       {Stdout: "10 /w/job-7.out\n"},
		"tail":     {Stdout: "l6\nl7\nl8\nl9\nl10\n"},
		"head":     {Stdout: "l1\nl2\n"},
	}}
	out, errs, err := (&Slurm{Runner: r}).Output(context.Background(), "7", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(out, ","); got != "l1,l2,[... 5 lines left out ...],l8,l9,l10" {
		t.Errorf("stdout = %s", got)
	}
	if len(errs) == 0 {
		t.Error("no stderr read from its own file")
	}
	// One file for both, short: the whole text, no head call, no stderr.
	r = &fakeRunner{replies: map[string]backend.Result{
		"scontrol": {Stdout: "StdErr=/w/a.out StdOut=/w/a.out"},
		"wc":       {Stdout: "2 /w/a.out\n"},
		"tail":     {Stdout: "a\nb\n"},
	}}
	out, errs, err = (&Slurm{Runner: r}).Output(context.Background(), "8", 100, 500)
	if err != nil || strings.Join(out, ",") != "a,b" || errs != nil {
		t.Fatalf("short = %v %v %v", out, errs, err)
	}
	for _, c := range r.calls {
		if c[0] == "head" {
			t.Error("head ran for a short file")
		}
	}
	if _, _, err := (&Slurm{Runner: r}).Output(context.Background(), "8; rm", 1, 1); err == nil {
		t.Error("bad job id accepted")
	}
}

func TestOutputFull(t *testing.T) {
	// A small file: all lines, progress-bar redraws split on carriage returns.
	r := &fakeRunner{replies: map[string]backend.Result{
		"scontrol": {Stdout: "StdErr=/w/a.out StdOut=/w/a.out"},
		"wc":       {Stdout: "20 /w/a.out\n"},
		"cat":      {Stdout: "start\n10%\r20%\r30%\ndone\n"},
	}}
	out, errs, err := (&Slurm{Runner: r}).OutputFull(context.Background(), "8", 100, 1000)
	if err != nil || strings.Join(out, ",") != "start,10%,20%,30%,done" || errs != nil {
		t.Fatalf("small = %v %v %v", out, errs, err)
	}
	// A large file: head lines, a marker, the last bytes without the cut first line.
	r = &fakeRunner{replies: map[string]backend.Result{
		"scontrol": {Stdout: "StdErr=/w/a.out StdOut=/w/a.out"},
		"wc":       {Stdout: "5000000 /w/a.out\n"},
		"head":     {Stdout: "h1\nh2\n"},
		"tail":     {Stdout: "rtial\nt1\nt2\n"},
	}}
	out, _, err = (&Slurm{Runner: r}).OutputFull(context.Background(), "8", 2, 1000)
	if err != nil || strings.Join(out, ",") != "h1,h2,[... the middle of a large file was not read ...],t1,t2" {
		t.Fatalf("large = %v %v", out, err)
	}
	if _, _, err := (&Slurm{Runner: r}).OutputFull(context.Background(), "8; rm", 1, 1); err == nil {
		t.Error("bad job id accepted")
	}
}
