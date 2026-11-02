package queueplan

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func names(o []Option) string {
	var s []string
	for _, x := range o {
		s = append(s, x.Tech+":"+x.Name)
	}
	return strings.Join(s, ",")
}

var scav = Candidate{Name: "scav", IsQOS: true, Technique: Q3, MaxWall: 24 * time.Hour, Allowed: true, StartsSoon: true}
var long = Candidate{Name: "long", Technique: Q2, MaxWall: 7 * 24 * time.Hour, Allowed: true}
var pr = Probe{Default: Candidate{Name: "std"}, Candidates: []Candidate{long, scav}}

func TestChooseQueue(t *testing.T) {
	short := Job{Restartable: true, Estimate: 2 * time.Hour}
	for _, c := range []struct {
		name string
		pr   Probe
		j    Job
		p    Prefs
		want string
	}{
		{"preemptible first", pr, short, Prefs{}, "Q3:scav,Q1:std"},
		{"off, plain job", pr, short, Prefs{Use: "off"}, "Q1:std"},
		{"stated need", pr, Job{NeedOther: true, Estimate: 50 * time.Hour}, Prefs{}, "Q2:long,Q1:std"},
		{"not restartable and long", pr, Job{Estimate: 10 * time.Hour}, Prefs{}, "Q1:std"},
		{"short without checkpoint", pr, Job{Estimate: 3 * time.Hour}, Prefs{}, "Q1:std"},
		{"time limit too long", pr, Job{Restartable: true, Estimate: time.Hour, TimeLimit: 10 * time.Hour}, Prefs{}, "Q1:std"},
		{"not allowed", Probe{Default: pr.Default, Candidates: []Candidate{{Name: "x", Technique: Q3, StartsSoon: true}}}, short, Prefs{}, "Q1:std"},
		{"not sooner", Probe{Default: pr.Default, Candidates: []Candidate{{Name: "x", Technique: Q3, Allowed: true}}}, short, Prefs{}, "Q1:std"},
		{"priority only when asked", Probe{Default: pr.Default, Candidates: []Candidate{{Name: "hi", Technique: Q6, Allowed: true, UsageFactor: 2}}}, short, Prefs{}, "Q1:std"},
		{"cancel mode multi node", Probe{Default: pr.Default, Candidates: []Candidate{{Name: "c", Technique: Q3, Allowed: true, StartsSoon: true, CancelMode: true}}}, Job{Restartable: true, MultiNode: true}, Prefs{}, "Q1:std"},
		{"by request", Probe{Default: pr.Default, Candidates: []Candidate{{Name: "r", Technique: Q3, Allowed: true, StartsSoon: true, ByRequest: true}}}, short, Prefs{}, "Q1:std"},
	} {
		if got := names(ChooseQueue(c.pr, c.j, c.p)); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	hi := Candidate{Name: "hi", IsQOS: true, Technique: Q6, Allowed: true, UsageFactor: 2}
	lab := Candidate{Name: "lab", IsQOS: true, Technique: Q5, Allowed: true}
	labDef := lab
	labDef.IsDefault = true
	arr := Job{ArrayShort: true, Estimate: time.Hour}
	for _, c := range []struct {
		name  string
		cands []Candidate
		j     Job
		want  string
	}{
		{"asked priority goes before preemptible", []Candidate{scav, hi}, Job{WantsHigh: true, Restartable: true, Estimate: time.Hour}, "Q6:hi,Q3:scav,Q1:std"},
		{"named QOS first", []Candidate{scav, lab}, Job{NamedQOS: "lab", Restartable: true, Estimate: time.Hour}, "Q5:lab,Q3:scav,Q1:std"},
		{"lab QOS not default, not named", []Candidate{lab}, Job{}, "Q1:std"},
		{"lab QOS is default", []Candidate{labDef}, Job{}, "Q5:lab,Q1:std"},
		{"short array without checkpoint", []Candidate{scav}, arr, "Q3:scav,Q1:std"},
	} {
		if got := names(ChooseQueue(Probe{Default: pr.Default, Candidates: c.cands}, c.j, Prefs{})); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	if !ChooseQueue(Probe{Default: pr.Default, Candidates: []Candidate{hi}}, Job{WantsHigh: true}, Prefs{})[0].NeedsAsk {
		t.Error("usage factor above 1 must ask")
	}
	if !ChooseQueue(Probe{Default: pr.Default, Hidden: true, Candidates: []Candidate{scav}}, short, Prefs{})[0].NeedsAsk {
		t.Error("hidden sacctmgr must ask")
	}
	if !ChooseQueue(pr, short, Prefs{Use: "ask"})[0].NeedsAsk {
		t.Error("use=ask must ask")
	}
}

func TestAlwaysEndsInQ1(t *testing.T) {
	for _, p := range []Probe{{}, pr, {Candidates: []Candidate{scav}}} {
		l := ChooseQueue(p, Job{}, Prefs{})
		if len(l) == 0 || l[len(l)-1].Tech != Q1 {
			t.Fatalf("%v", l)
		}
	}
}

func TestNext(t *testing.T) {
	l := ChooseQueue(pr, Job{Restartable: true, Estimate: time.Hour, NeedOther: true}, Prefs{}) // scav, long, std
	if o, _, _ := Next(l, []Attempt{{l[0], "P4"}}, Prefs{}); o.Name != "scav" {
		t.Fatalf("one stop retries preemptible: %v", o)
	}
	h := []Attempt{{l[0], "P4"}, {l[0], "P4"}}
	if o, _, _ := Next(l, h, Prefs{MaxStops: 2}); o.Tech != Q1 {
		t.Fatalf("after max stops straight to Q1: %v", o)
	}
	for _, code := range []string{"P1", "P2", "P5"} {
		o, ask, _ := Next(l, []Attempt{{l[0], code}}, Prefs{})
		if o.Tech != Q1 || ask {
			t.Fatalf("%s: %v ask=%v", code, o, ask)
		}
	}
	o, ask, _ := Next(l, []Attempt{{l[0], "P6"}}, Prefs{})
	if o.Name != "long" || !ask {
		t.Fatalf("P6 asks: %v %v", o, ask)
	}
	if _, ask, _ = Next(l, []Attempt{{l[0], "P7"}}, Prefs{}); !ask {
		t.Fatal("P7 asks")
	}
	// first attempt: ask when the option needs approval
	if _, ask, _ = Next(ChooseQueue(pr, Job{Restartable: true, Estimate: time.Hour}, Prefs{Use: "ask"}), nil, Prefs{Use: "ask"}); !ask {
		t.Fatal("use=ask asks on the first attempt")
	}
	q1 := Option{Tech: Q1, Name: "std"}
	if _, _, err := Next([]Option{q1}, []Attempt{{q1, "P3"}}, Prefs{}); !errors.Is(err, ErrNoOption) {
		t.Fatalf("a failed Q1 is not repeated: %v", err)
	}
	// Three stops with the default MaxStops go to Q1, not to a stop.
	h = []Attempt{{l[0], "P4"}, {l[0], "P4"}, {l[0], "P4"}}
	if o, _, err := Next(l, h, Prefs{}); err != nil || o.Tech != Q1 {
		t.Fatalf("after 3 stops: %v %v", o, err)
	}
	h = []Attempt{{l[0], "P3"}, {l[1], "P3"}, {l[2], "P3"}}
	if _, _, err := Next(l, h, Prefs{}); !errors.Is(err, ErrNoOption) {
		t.Fatalf("3-attempt limit: %v", err)
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		text, state string
		r           int
		long        bool
		want        string
	}{
		{"sbatch: error: Invalid qos specification", "", 0, false, "P2"},
		{"Requested time limit is invalid (missing or exceeds some limit)", "", 0, false, "P3"},
		{"sbatch: error: Job violates accounting/QOS policy", "", 0, false, "P2"},
		{"AssocGrpCpuLimit", "PENDING", 0, false, "P7"},
		{"ReqNodeNotAvail, Reserved for maintenance", "PENDING", 0, false, "P7"},
		{"sbatch: unrecognized option '--requeue'", "", 0, false, "P1"},
		{"", "PREEMPTED", 0, false, "P4"},
		{"", "REQUEUED", 4, false, "P5"},
		{"", "TIMEOUT", 0, false, "P6"},
		{"Priority", "PENDING", 0, true, "P7"},
		{"ok", "COMPLETED", 0, false, ""},
	} {
		if got := Classify(c.text, c.state, c.r, c.long); got != c.want {
			t.Errorf("%q: got %q want %q", c.text, got, c.want)
		}
	}
}

func TestSbatchAndRecord(t *testing.T) {
	l, err := SbatchLines(Option{Tech: Q3, Name: "scav", IsQOS: true, Requeue: true})
	if err != nil || strings.Join(l, "|") != "#SBATCH --qos=scav|#SBATCH --requeue|#SBATCH --signal=B:TERM@120" {
		t.Fatal(l, err)
	}
	l, _ = SbatchLines(Option{Tech: Q1, Name: "std"})
	if strings.Join(l, "|") != "#SBATCH --no-requeue" {
		t.Fatal(l)
	}
	for _, bad := range []string{"a\nrm -rf x", "a b", "$(x)", ""} {
		if _, err := SbatchLines(Option{Tech: Q2, Name: bad}); bad != "" && err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if Record(Attempt{Option{Name: "a"}, "P2"}, "x", "d").Confidence != "high" || Record(Attempt{Option{}, "P4"}, "", "d").Confidence != "medium" || Record(Attempt{Option{}, "P7"}, "", "d").Confidence != "low" {
		t.Fatal("confidence")
	}
}
