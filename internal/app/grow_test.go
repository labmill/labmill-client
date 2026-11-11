package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

func TestGrowTimeAndMem(t *testing.T) {
	for in, want := range map[string]string{"30": "0-01:00:00", "10:30": "0-00:21:00", "04:00:00": "0-08:00:00", "1-12": "3-00:00:00", "2-00:00:00": "4-00:00:00"} {
		if got, ok := growTime(in); !ok || got != want {
			t.Errorf("growTime(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"UNLIMITED", "0", "", "1:2:3:4", "x"} {
		if _, ok := growTime(bad); ok {
			t.Errorf("growTime(%q) must fail", bad)
		}
	}
	for in, want := range map[string]string{"4G": "8G", "2000M": "4000M", "512": "1024"} {
		if got, ok := growMem(in); !ok || got != want {
			t.Errorf("growMem(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := growMem("0G"); ok {
		t.Error("growMem(0G) must fail")
	}
}

func TestGrowScript(t *testing.T) {
	s := "#!/bin/bash\n#SBATCH --job-name=t\n#SBATCH -t 02:00:00\n#SBATCH --mem=4G\n#SBATCH --chdir=/w\necho --time=01:00:00\n"
	out, ch, ok := growScript(s, "time")
	if !ok || !strings.Contains(out, "#SBATCH --time=0-04:00:00\n") || !strings.Contains(out, "--mem=4G") || !strings.Contains(out, "echo --time=01:00:00") || len(ch) != 1 {
		t.Errorf("time: %v %v\n%s", ok, ch, out)
	}
	out, _, ok = growScript(s, "memory")
	if !ok || !strings.Contains(out, "#SBATCH --mem=8G\n") || !strings.Contains(out, "-t 02:00:00") {
		t.Errorf("memory: %v\n%s", ok, out)
	}
	if _, _, ok := growScript("#!/bin/bash\n#SBATCH --job-name=t\necho hi\n", "time"); ok {
		t.Error("a script without a time line can't be grown")
	}
}

func growJob(t *testing.T, f *resubFixture, script string) store.Job {
	t.Helper()
	ctx := context.Background()
	id, err := f.db.CreateJob(ctx, store.Job{Name: "g", Cluster: "hpcone", Backend: "slurm", RemoteID: "300",
		State: store.StateRunning, Script: script, RemoteDir: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.db.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestResubmitGrowOnce(t *testing.T) {
	f := newResubFixture(t, nil)
	j := growJob(t, f, "#!/bin/bash\n#SBATCH --time=01:00:00\n#SBATCH --mem=2G\necho x\n")
	be := &resubBackend{}
	ctx := context.Background()
	if !f.rs.Handle(ctx, j, be, failed("TIMEOUT", 0)) {
		t.Fatalf("TIMEOUT must be resubmitted; notes %v", f.notes)
	}
	if len(be.submitted) != 1 || !strings.Contains(be.submitted[0], "--time=0-02:00:00") || !strings.Contains(be.submitted[0], "--mem=2G") {
		t.Fatalf("submitted %q", be.submitted)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "more time") {
		t.Errorf("notes %v", f.notes)
	}
	j2, _ := f.db.Job(ctx, j.ID)
	if j2.RemoteID != "201" {
		t.Fatalf("remote id %s", j2.RemoteID)
	}
	// A second failure is final: once only.
	if f.rs.Handle(ctx, j2, be, failed("OUT_OF_MEMORY", 0)) || len(be.submitted) != 1 {
		t.Errorf("second growing resubmission: %q", be.submitted)
	}
	n, err := f.db.Events(ctx, j.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := countGrown(n); got != 1 {
		t.Errorf("grow events = %d", got)
	}
}

func TestResubmitGrowNoLine(t *testing.T) {
	f := newResubFixture(t, nil)
	j := growJob(t, f, "#!/bin/bash\n#SBATCH --job-name=g\necho x\n")
	be := &resubBackend{}
	if f.rs.Handle(context.Background(), j, be, failed("OUT_OF_MEMORY", 0)) || len(be.submitted) != 0 {
		t.Error("nothing to raise: no resubmission")
	}
	if len(f.notes) != 1 {
		t.Errorf("notes %v", f.notes)
	}
}

func TestResubmitIgnoresOtherFailures(t *testing.T) {
	f := newResubFixture(t, nil)
	j := growJob(t, f, "#!/bin/bash\n#SBATCH --time=01:00:00\n")
	be := &resubBackend{}
	for _, st := range []backend.Status{failed("FAILED", 1), failed("NODE_FAIL", 0), {State: store.StateCompleted}} {
		if f.rs.Handle(context.Background(), j, be, st) {
			t.Errorf("%v must not be resubmitted", st.Reason)
		}
	}
}

func countGrown(evs []store.Event) int {
	n := 0
	for _, e := range evs {
		if e.Kind == eventResubmitted && strings.Contains(e.Text, `"grow"`) {
			n++
		}
	}
	return n
}
