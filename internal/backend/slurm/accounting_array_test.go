package slurm

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
)

func TestParseSlurmMem(t *testing.T) {
	for in, want := range map[string]int64{"16G": 16 << 30, "2T": 2 << 40, "4Gc": 0, "": 0, "0": 0, "1.50G": 3 << 29, "4000Mn": 4000 << 20, "x": 0} {
		if got := parseSlurmMem(in); got != want {
			t.Errorf("parseSlurmMem(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAccountingArray(t *testing.T) {
	out := "9_1|COMPLETED|0:0|None|00:01:00|01:00:00|4G||n1\n9_2|OUT_OF_MEMORY|0:125|None|00:02:00|01:00:00|4G||n2\n9_3|COMPLETED|0:0|None|00:01:00|01:00:00|4G||n3\n"
	r := &fakeRunner{replies: map[string]backend.Result{"sacct": {Stdout: out}}}
	got, err := (&Slurm{Runner: r}).Accounting(context.Background(), "9")
	if err != nil || got.ExitCode != "0:125" || got.NodeList != "n2" || !strings.Contains(got.State, "2 COMPLETED, 1 OUT_OF_MEMORY") {
		t.Fatalf("got %+v, %v", got, err)
	}
	if last := strings.Join(r.calls[len(r.calls)-1], " "); !strings.Contains(last, "-j 9_2 ") {
		t.Errorf("step query not limited to the task: %s", last)
	}
}
