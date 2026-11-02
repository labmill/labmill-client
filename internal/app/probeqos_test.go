package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
)

const qosSample = `@@PART
PartitionName=main AllowGroups=ALL Default=YES MaxTime=2-00:00:00 PriorityTier=10 PreemptMode=OFF
PartitionName=scav AllowGroups=ALL Default=NO MaxTime=12:00:00 PriorityTier=1 PreemptMode=REQUEUE
@@QOS
normal|||cluster|1.000000
@@ASSOC
normal|normal
`

func TestProbeQOS(t *testing.T) {
	h := newHarness(t)
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: qosSample}, nil
	}}
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) { return fr, nil }
	out, err := h.run(t, "probe_qos", map[string]any{"cluster": "hpcone", "restartable": true, "hours": 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Default partition: main", "1. Q3 scav", "#SBATCH --partition=scav #SBATCH --requeue"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Q1") {
		t.Errorf("the normal queue must end the ladder:\n%s", out)
	}
	// Not restartable: no preemptible entry.
	out, _ = h.run(t, "probe_qos", map[string]any{"cluster": "hpcone", "restartable": false, "hours": 2})
	if strings.Contains(out, "Q3") {
		t.Errorf("a job that cannot restart must not get a preemptible queue:\n%s", out)
	}
	if len(fr.calls) != 2 || strings.Contains(fr.calls[0][2], "sacctmgr -i") || strings.Contains(fr.calls[0][2], "sbatch") {
		t.Errorf("probe must be one read-only script: %v", fr.calls)
	}
}

func TestProbeQOSHiddenSacctmgr(t *testing.T) {
	h := newHarness(t)
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) {
		return &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
			return backend.Result{Stdout: "@@PART\nPartitionName=main Default=YES MaxTime=1-00:00:00 PriorityTier=1\n@@QOS\n@@ASSOC\n"}, nil
		}}, nil
	}
	out, err := h.run(t, "probe_qos", map[string]any{"cluster": "hpcone", "restartable": true, "hours": 1})
	if err != nil || !strings.Contains(out, "low confidence") {
		t.Errorf("err=%v:\n%s", err, out)
	}
	h.env.LoginRunner = nil
	if _, err := h.run(t, "probe_qos", map[string]any{"cluster": "hpcone", "restartable": true, "hours": 1}); err == nil {
		t.Error("no runner must fail")
	}
}
func TestDraftQueueAuto(t *testing.T) {
	probe := func(h *harness, calls *int) {
		h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) {
			return &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
				*calls++
				return backend.Result{Stdout: qosSample}, nil
			}}, nil
		}
	}
	t.Run("checkpointing job gets the preemptible partition", func(t *testing.T) {
		h, _ := slurmHarness(t, rules.SlurmHints{})
		var calls int
		probe(h, &calls)
		out, err := h.run(t, "draft_job", draftSpec(map[string]any{"queue": "auto", "checkpoint": true}, map[string]any{"time_minutes": 60}))
		if err != nil || calls == 0 || !strings.Contains(out, "#SBATCH --partition=scav\n") || !strings.Contains(out, "#SBATCH --requeue\n") {
			t.Errorf("err=%v calls=%d:\n%s", err, calls, out)
		}
	})
	t.Run("no checkpoint, no probe", func(t *testing.T) {
		h, _ := slurmHarness(t, rules.SlurmHints{})
		var calls int
		probe(h, &calls)
		out, err := h.run(t, "draft_job", draftSpec(map[string]any{"queue": "auto"}, map[string]any{"time_minutes": 60}))
		if err != nil || calls != 0 || strings.Contains(out, "scav") {
			t.Errorf("err=%v calls=%d:\n%s", err, calls, out)
		}
	})
}
