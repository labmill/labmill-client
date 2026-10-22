package slurm

import (
	"context"
	"testing"
	"time"

	"labmill-client/internal/livetest"
)

// TestLiveReadOnly parses real sinfo/squeue output from a real cluster. It
// runs only with LABMILL_LIVE=1 and a live master for LABMILL_LIVE_ALIAS
// (no default; skipped when unset), and only runs read-only commands.
func TestLiveReadOnly(t *testing.T) {
	alias := livetest.Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s := &Slurm{Runner: livetest.Runner{Alias: alias}}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatalf("Info on the real cluster: %v", err)
	}
	if len(info.Partitions) == 0 {
		t.Fatal("no partitions parsed")
	}
	defaults := 0
	for _, p := range info.Partitions {
		if p.Name == "" || p.Nodes <= 0 {
			t.Errorf("odd partition %+v", p)
		}
		if p.Default {
			defaults++
		}
		t.Logf("%-20s default=%-5v state=%-6s nodes=%3d idle=%3d max=%-10v gpus=%v", p.Name, p.Default, p.State, p.Nodes, p.IdleNodes, p.MaxTime, p.GPUTypes)
	}
	if defaults > 1 {
		t.Errorf("%d default partitions", defaults)
	}
	// squeue for the current user must run and parse (even if empty).
	res, err := s.Runner.Run(ctx, []string{"squeue", "-h", "--me", "-o", "%i|%T|%r|%N"}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("squeue --me: %v %+v", err, res)
	}
	t.Logf("own jobs:\n%s", res.Stdout)
	jobs, err := s.Jobs(ctx, time.Now().AddDate(0, 0, -30), 20)
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	t.Logf("jobs in the last 30 days: %d", len(jobs))
}
