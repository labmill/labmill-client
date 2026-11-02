package queueplan

import (
	"reflect"
	"testing"
	"time"
)

func TestParseMaxWall(t *testing.T) {
	h := time.Hour
	cases := []struct {
		in string
		d  time.Duration
		ok bool
	}{
		{"1-00:00:00", 24 * h, true},
		{"UNLIMITED", 0, true},
		{"infinite", 0, true},
		{"1-12", 36 * h, true},
		{"2-01:30", 49*h + 30*time.Minute, true},
		{"04:00:00", 4 * h, true},
		{"90", 90 * time.Minute, true},
		{"30:15", 30*time.Minute + 15*time.Second, true},
		{"", 0, false},
		{"NONE", 0, false},
		{"n/a", 0, false},
		{"1-2-3", 0, false},
		{"-1:00", 0, false},
	}
	for _, c := range cases {
		d, ok := ParseMaxWall(c.in)
		if d != c.d || ok != c.ok {
			t.Errorf("%q: got %v %v, want %v %v", c.in, d, ok, c.d, c.ok)
		}
	}
}

func TestParseQOS(t *testing.T) {
	out := "normal|||cluster|1.000000\npreempt|04:00:00||requeue|0.5\nhigh|1-00:00:00|preempt,normal|cluster|2\nbroken\n\n"
	qs := ParseQOS(out)
	if len(qs) != 3 {
		t.Fatalf("got %d QOS: %+v", len(qs), qs)
	}
	if qs[1].MaxWall != 4*time.Hour || qs[1].PreemptMode != "requeue" || qs[1].UsageFactor != 0.5 {
		t.Errorf("preempt: %+v", qs[1])
	}
	if !reflect.DeepEqual(qs[2].Preempt, []string{"preempt", "normal"}) || qs[2].UsageFactor != 2 {
		t.Errorf("high: %+v", qs[2])
	}
	if qs[0].MaxWall != 0 || qs[0].UsageFactor != 1 {
		t.Errorf("normal: %+v", qs[0])
	}
	if len(ParseQOS("")) != 0 || len(ParseQOS("sacctmgr: error: access denied\n")) != 0 {
		t.Error("empty or error output must give no QOS")
	}
}

func TestParseAssocQOS(t *testing.T) {
	a, def, ok := ParseAssocQOS("normal,preempt|normal\nnormal,low|normal\n")
	if !ok || def != "normal" || !reflect.DeepEqual(a, []string{"normal", "preempt", "low"}) {
		t.Errorf("got %v %q %v", a, def, ok)
	}
	for _, hidden := range []string{"", "\n", "|\n", "sacctmgr: error: Access/permission denied"} {
		if a, _, ok := ParseAssocQOS(hidden); ok || len(a) != 0 {
			t.Errorf("%q: hidden output must not be readable: %v %v", hidden, a, ok)
		}
	}
}

func TestParsePartitions(t *testing.T) {
	out := `PartitionName=main AllowGroups=ALL AllowAccounts=ALL AllowQos=ALL Default=YES MaxTime=7-00:00:00 Hidden=NO PriorityTier=10 PreemptMode=OFF
PartitionName=scavenge AllowQos=ALL Default=NO MaxTime=UNLIMITED Hidden=NO PriorityTier=1 PreemptMode=REQUEUE
PartitionName=lab AllowQos=labq,normal Default=NO MaxTime=2-00:00:00 Hidden=YES PriorityTier=20

`
	ps := ParsePartitions(out)
	if len(ps) != 3 {
		t.Fatalf("got %d: %+v", len(ps), ps)
	}
	if !ps[0].Default || ps[0].MaxWall != 7*24*time.Hour || ps[0].PriorityTier != 10 || len(ps[0].AllowQOS) != 0 {
		t.Errorf("main: %+v", ps[0])
	}
	if ps[1].MaxWall != 0 || ps[1].PreemptMode != "requeue" {
		t.Errorf("scavenge: %+v", ps[1])
	}
	if !ps[2].Hidden || !reflect.DeepEqual(ps[2].AllowQOS, []string{"labq", "normal"}) {
		t.Errorf("lab: %+v", ps[2])
	}
	if len(ParsePartitions("")) != 0 {
		t.Error("empty output must give no partition")
	}
}

func TestBuildProbe(t *testing.T) {
	parts := ParsePartitions(`PartitionName=main Default=YES MaxTime=7-00:00:00 PriorityTier=10 PreemptMode=OFF
PartitionName=scav Default=NO MaxTime=UNLIMITED PriorityTier=1 PreemptMode=REQUEUE
PartitionName=gpu Default=NO MaxTime=2-00:00:00 PriorityTier=10 PreemptMode=OFF
PartitionName=secret Default=NO MaxTime=1-00:00:00 Hidden=YES PriorityTier=1 PreemptMode=REQUEUE`)
	qos := ParseQOS("normal|||cluster|1\nlowprio|04:00:00||requeue|0.5\nurgent|1-00:00:00|lowprio|cluster|2\nlabq|||cluster|1\nplain|||cluster|1\n")
	as := Assoc{Allowed: []string{"normal", "lowprio", "labq"}, Default: "labq", Readable: true}
	pr := BuildProbe(parts, qos, as)
	if pr.Default.Name != "main" || pr.Hidden {
		t.Fatalf("default %+v hidden %v", pr.Default, pr.Hidden)
	}
	got := map[string]Candidate{}
	for _, c := range pr.Candidates {
		got[c.Name] = c
	}
	if len(got) != 5 || got["secret"].Name != "" || got["plain"].Name != "" || got["normal"].Name != "" {
		t.Fatalf("candidates: %+v", pr.Candidates)
	}
	if c := got["scav"]; c.Technique != Q3 || !c.StartsSoon || c.IsQOS {
		t.Errorf("scav: %+v", c)
	}
	if got["gpu"].Technique != Q2 {
		t.Errorf("gpu: %+v", got["gpu"])
	}
	if c := got["lowprio"]; c.Technique != Q3 || !c.Allowed || c.MaxWall != 4*time.Hour {
		t.Errorf("lowprio: %+v", c)
	}
	if c := got["urgent"]; c.Technique != Q6 || c.Allowed {
		t.Errorf("urgent must be Q6 and not allowed: %+v", c)
	}
	if c := got["labq"]; c.Technique != Q5 || !c.IsDefault || !c.Allowed {
		t.Errorf("labq: %+v", c)
	}
	// Hidden sacctmgr never sets Allowed, so no QOS reaches the ladder.
	hid := BuildProbe(parts, qos, Assoc{})
	if !hid.Hidden {
		t.Error("hidden not set")
	}
	for _, c := range hid.Candidates {
		if c.IsQOS && c.Allowed {
			t.Errorf("hidden output set Allowed: %+v", c)
		}
	}
	// The ladder from the built probe puts the preemptible QOS before Q1.
	l := ChooseQueue(pr, Job{Restartable: true, Estimate: time.Hour, TimeLimit: time.Hour}, Prefs{Use: "auto"})
	if len(l) < 2 || !l[0].Preemptive || l[len(l)-1].Tech != Q1 {
		t.Errorf("ladder: %+v", l)
	}
	if empty := BuildProbe(nil, nil, Assoc{}); empty.Default.Name != "" || len(empty.Candidates) != 0 {
		t.Errorf("empty: %+v", empty)
	}
}
