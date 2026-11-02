package workdisk

import (
	"errors"
	"strings"
	"testing"
)

func ids(o []Option) string {
	var s []string
	for _, x := range o {
		s = append(s, x.ID)
	}
	return strings.Join(s, ",")
}

func TestChooseOrder(t *testing.T) {
	big := Facts{
		TmpDirEnv: "/tmp/job", NodeTmpDiskGB: 500, Gres: []string{"lscratch:100", "pfsdir"}, PerJobGres: "pfsdir", HasWorkspace: true,
		SharedRoot: "/scratch", SharedFreeGB: -1, HomeFreeGB: -1, FromCompute: true,
		Mounts: []Mount{{"/tmp/job", "xfs", 500, 400}, {"/tmp", "xfs", 100, 90}, {"/lscratch", "xfs", 800, 700}},
	}
	for _, c := range []struct {
		name string
		f    Facts
		j    Job
		p    Prefs
		want string
	}{
		{"local first", big, Job{NeedGB: 50}, Prefs{}, "L1,L2,L3,L4,L5,S4,S1,S2,S6"},
		{"shared preferred skips local", big, Job{NeedGB: 50}, Prefs{Prefer: "shared"}, "S4,S1,S2,S6"},
		{"multi node", big, Job{NeedGB: 50, MultiNode: true}, Prefs{}, "S4,S1,S2,S6"},
		{"nothing known", Facts{HomeFreeGB: -1, SharedFreeGB: -1}, Job{NeedGB: 10}, Prefs{}, "L1,S6"},
		{"tmpfs tmpdir too big for memory", Facts{TmpDirEnv: "/dev/shm", Mounts: []Mount{{"/dev/shm", "tmpfs", 64, 64}}}, Job{NeedGB: 40, MemGB: 64}, Prefs{}, "S6"},
		{"high profile failure drops", Facts{HasWorkspace: true, Mounts: []Mount{{"/x", "xfs", 1, 1}}, FailedByProfile: []Failure{{Technique: "S1", Confidence: "high"}}}, Job{NeedGB: 1}, Prefs{}, "S6"},
		{"expired high failure stays", Facts{HasWorkspace: true, Today: "2026-10-26", Mounts: []Mount{{"/x", "xfs", 1, 1}}, FailedByProfile: []Failure{{Technique: "S1", Confidence: "high", Date: "2026-01-01"}}}, Job{NeedGB: 1}, Prefs{}, "S1,S6"},
		{"medium failure goes last", Facts{HasWorkspace: true, SharedRoot: "/s", SharedFreeGB: -1, Mounts: []Mount{{"/x", "xfs", 1, 1}}, FailedByProfile: []Failure{{Technique: "S1", Confidence: "medium"}}}, Job{NeedGB: 1}, Prefs{}, "S2,S1,S6"},
		{"full mount with need 0", Facts{Mounts: []Mount{{"/lscratch", "xfs", 10, 0}}}, Job{}, Prefs{}, "S6"},
		{"unknown free space", Facts{Mounts: []Mount{{"/lscratch", "xfs", 10, -1}}}, Job{NeedGB: 1}, Prefs{}, "S6"},
		{"full shared root", Facts{SharedRoot: "/s", SharedFreeGB: 0, Mounts: []Mount{{"/x", "xfs", 1, 1}}}, Job{NeedGB: 1}, Prefs{}, "S6"},
		{"per-job gres needs the site name", Facts{Gres: []string{"pfsdir"}, Mounts: []Mount{{"/x", "xfs", 1, 1}}}, Job{NeedGB: 1}, Prefs{}, "S6"},
	} {
		if got := ids(Choose(c.f, c.j, c.p)); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// The owner rule: the ladder is never empty and always ends in home, even when the profile lists S6 as failed.
func TestChooseAlwaysEndsInHome(t *testing.T) {
	facts := []Facts{{}, {FailedByProfile: []Failure{{Technique: "S6", Confidence: "high"}}}, {HasWorkspace: true, Gres: []string{"pfsdir"}, PerJobGres: "pfsdir"}}
	for i, f := range facts {
		for _, j := range []Job{{}, {NeedGB: 1 << 20}, {MultiNode: true}} {
			for _, p := range []Prefs{{}, {Prefer: "shared"}} {
				l := Choose(f, j, p)
				if len(l) == 0 || l[len(l)-1].ID != S6 {
					t.Fatalf("case %d: ladder %s", i, ids(l))
				}
			}
		}
	}
}

func TestHomeQuota(t *testing.T) {
	l := Choose(Facts{HomeFreeGB: 15}, Job{NeedGB: 10}, Prefs{})
	if !l[len(l)-1].LowSpace {
		t.Fatal("15 GB home for 10 GB staged is below twice the size")
	}
	if l = Choose(Facts{HomeFreeGB: 20}, Job{NeedGB: 10}, Prefs{}); l[len(l)-1].LowSpace {
		t.Fatal("20 GB is enough")
	}
	if l = Choose(Facts{HomeFreeGB: -1}, Job{NeedGB: 10}, Prefs{}); l[len(l)-1].LowSpace {
		t.Fatal("unknown is no warning")
	}
}

func TestNext(t *testing.T) {
	l1, l2 := Option{ID: L1, Kind: Local, Path: "/tmp/j"}, Option{ID: L2, Kind: Local, SizeGB: 50}
	l4 := Option{ID: L4, Kind: Local, Path: "/lscratch"}
	s2, s3 := Option{ID: S2, Kind: Shared, Path: "/scratch"}, Option{ID: S3, Kind: Shared, Path: "/lustre"}
	s6 := Option{ID: S6, Kind: Shared}
	ladder := []Option{l1, l2, l4, s2, s3, s6}
	f := Facts{NodeTmpDiskGB: 100}

	o, ask, err := Next(ladder, nil, Prefs{}, f)
	if err != nil || o.ID != L1 || ask {
		t.Fatalf("first: %v %v %v", o, ask, err)
	}
	o, ask, _ = Next(ladder, []Attempt{{l1, "F3"}}, Prefs{}, f)
	if o.ID != L2 || ask {
		t.Fatalf("same class: %v ask=%v", o, ask)
	}
	if _, ask, _ = Next(ladder, []Attempt{{l1, "F3"}}, Prefs{OnFailure: "ask"}, f); !ask {
		t.Fatal("on_failure=ask must ask")
	}
	// F4/F6 on $TMPDIR: L2 uses the same disk, so it is skipped too.
	o, _, _ = Next(ladder, []Attempt{{l1, "F6"}}, Prefs{}, f)
	if o.ID != L4 {
		t.Fatalf("tmp-based skipped: %v", o)
	}
	// F5 leaves the local class with a question.
	o, ask, _ = Next(ladder, []Attempt{{l4, "F5"}}, Prefs{}, f)
	if o.ID != S2 || !ask {
		t.Fatalf("F5: %v ask=%v", o, ask)
	}
	// F3 on a shared option goes to the next shared type.
	if o, _, _ = Next(ladder, []Attempt{{s2, "F3"}}, Prefs{}, f); o.ID != L1 && o.ID != S3 {
		t.Fatalf("F3 shared: %v", o)
	}
	if o, _, _ = Next(ladder[3:], []Attempt{{s2, "F3"}}, Prefs{}, f); o.ID != S3 {
		t.Fatalf("F3 shared next type: %v", o)
	}
	// F1 retries the same option with a smaller size.
	o, _, _ = Next(ladder, []Attempt{{Option{ID: L2, Kind: Local, SizeGB: 500}, "F1"}}, Prefs{}, f)
	if o.ID != L2 || o.SizeGB != 80 {
		t.Fatalf("F1 shrink: %v", o)
	}
	// ... but only once.
	h := []Attempt{{Option{ID: L2, Kind: Local, SizeGB: 500}, "F1"}, {Option{ID: L2, Kind: Local, SizeGB: 80}, "F1"}}
	if o, _, _ = Next(ladder, h, Prefs{}, f); o.ID == L2 && o.SizeGB >= 80 {
		t.Fatalf("F1 repeated: %v", o)
	}
	h = []Attempt{{l1, "F3"}, {l2, "F3"}, {l4, "F3"}}
	if _, _, err = Next(ladder, h, Prefs{}, f); !errors.Is(err, ErrNoOption) {
		t.Fatalf("limit: %v", err)
	}
}

func TestClassify(t *testing.T) {
	loc, sh := Option{ID: L4, Kind: Local}, Option{ID: S2, Kind: Shared}
	for _, c := range []struct {
		text, state string
		o           Option
		want        string
	}{
		{"sbatch: error: Invalid generic resource (gres) specification", "", loc, "F2"},
		{"sbatch: error: Requested node configuration is not available", "", loc, "F1"},
		{"cp: error writing 'x': No space left on device", "FAILED", sh, "F4"},
		{"cp: error writing 'x': No space left on device", "FAILED", loc, "F5"},
		{"No space left on device (tmpfs)", "FAILED", sh, "F6"},
		{"No space left", "OUT_OF_MEMORY", loc, "F6"},
		{"mkdir: cannot create directory '/lscratch/u': Permission denied", "", loc, "F3"},
		{"labmill: work dir fell back to home", "", loc, "F3"},
		{"cp: cannot open 'in.dat': Permission denied", "", loc, ""},
		{"Permission denied (publickey)", "", sh, ""},
		{"ws_allocate: error: failed", "", sh, "F7"},
		{"all fine", "COMPLETED", loc, ""},
	} {
		if got := Classify(c.text, c.state, c.o); got != c.want {
			t.Errorf("%q (%s): got %q want %q", c.text, c.o.Kind, got, c.want)
		}
	}
}

func TestShrinkAndRecord(t *testing.T) {
	o, ok := Shrink(Option{ID: L2, SizeGB: 500}, Facts{NodeTmpDiskGB: 100})
	if !ok || o.SizeGB != 80 {
		t.Fatalf("%v %v", o, ok)
	}
	if _, ok = Shrink(Option{SizeGB: 10}, Facts{NodeTmpDiskGB: 100}); ok {
		t.Fatal("no shrink when already small")
	}
	r := Record(Attempt{Option{ID: L3}, "F2"}, "a\n  b", "2026-10-26", true)
	if r.Confidence != "high" || r.Detail != "a b" {
		t.Fatalf("%+v", r)
	}
	if d := Record(Attempt{Option{ID: L3}, "F2"}, strings.Repeat("é", 300), "d", true).Detail; len([]rune(d)) != 200 {
		t.Fatal("rune cut")
	}
	if Record(Attempt{Option{ID: L3}, "F5"}, "", "d", true).Confidence != "low" {
		t.Fatal("F5 is low")
	}
}

func TestScript(t *testing.T) {
	sb, setup, err := Script(Option{ID: L2, Kind: Local, SizeGB: 20}, "job1")
	if err != nil || sb[0] != "#SBATCH --tmp=20G" {
		t.Fatalf("%v %v", sb, err)
	}
	all := strings.Join(setup, "\n")
	if !strings.Contains(all, "lm_stage_out && rm -rf") {
		t.Fatal("rm must follow a successful stage-out")
	}
	for _, w := range []string{"mkdir -p", "trap", "lm_stage_out", "fell back", `-z "$LM_BASE"`} {
		if !strings.Contains(all, w) {
			t.Fatalf("local setup lacks %q:\n%s", w, all)
		}
	}
	// Shared folders, workspaces and home are never removed by the script.
	for _, o := range []Option{{ID: S2, Kind: Shared, Path: "/scratch"}, {ID: S3, Kind: Shared, Path: "/lustre"}, {ID: S1, Kind: Shared, Path: "/ws/u-x"}, {ID: S6, Kind: Shared}, {ID: S4, Kind: Shared, Gres: "pfsdir"}} {
		_, setup, err := Script(o, "j")
		if err != nil {
			t.Fatal(o.ID, err)
		}
		if a := strings.Join(setup, "\n"); strings.Contains(a, "rm -") || strings.Contains(a, "trap") {
			t.Fatalf("%s removes files:\n%s", o.ID, a)
		}
	}
	if sb, _, _ := Script(Option{ID: L3, Kind: Local, Gres: "lscratch", Path: "/lscratch", SizeGB: 5}, "j"); sb[0] != "#SBATCH --gres=lscratch:5" {
		t.Fatal(sb)
	}
	for _, o := range []Option{{ID: L4, Path: "/x; rm -rf /"}, {ID: L4, Path: "$HOME"}, {ID: S1}, {ID: L3}, {ID: S4}} {
		if _, _, err = Script(o, "j"); err == nil {
			t.Fatalf("%+v accepted", o)
		}
	}
	for _, n := range []string{"j k", "a/b", "$x", ""} {
		if _, _, err = Script(Option{ID: S6}, n); err == nil {
			t.Fatalf("name %q accepted", n)
		}
	}
}

func TestWSDays(t *testing.T) {
	for _, c := range []struct{ h, max, want int }{{0, 0, 2}, {24, 0, 3}, {25, 0, 4}, {100, 5, 5}} {
		if got := wsDays(c.h, c.max); got != c.want {
			t.Errorf("%v: %d", c, got)
		}
	}
}
