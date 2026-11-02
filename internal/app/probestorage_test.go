package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
)

const probeSample = `TMPDIR=/tmp/user
HOME=/home/user
@@DF
Filesystem Type 1024-blocks Used Available Capacity Mounted on
/dev/sda1 ext4 524288000 1 262144000 50% /tmp
srv:/h nfs 209715200 1 104857600 50% /home
tmpfs tmpfs 16777216 1 16000000 1% /dev/shm
@@NODES
NodeName=n1 Gres=gpu:2 CfgTRES=cpu=8 TmpDisk=500000 State=IDLE
@@WS
`

func TestProbeStorage(t *testing.T) {
	h := newHarness(t)
	fr := &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
		return backend.Result{Stdout: probeSample}, nil
	}}
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) { return fr, nil }
	out, err := h.run(t, "probe_storage", map[string]any{"cluster": "hpcone", "need_gb": 50, "hours": 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Ladder, first entry first:", "1. ", "home free: 100 GB", "node TmpDisk: 488 GB"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "S6") {
		t.Errorf("the ladder must end in the home folder (S6):\n%s", out)
	}
	if len(fr.calls) != 1 || fr.calls[0][0] != "sh" || strings.Contains(fr.calls[0][2], "rm ") || strings.Contains(fr.calls[0][2], "mkdir") {
		t.Errorf("probe must be one read-only script: %v", fr.calls)
	}
}

func TestProbeStorageEmptyOutput(t *testing.T) {
	h := newHarness(t)
	h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) { return &fakeRunner{}, nil }
	out, err := h.run(t, "probe_storage", map[string]any{"cluster": "hpcone", "need_gb": 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "S6") {
		t.Errorf("empty probe must fall back to home:\n%s", out)
	}
	if _, err := h.run(t, "probe_storage", map[string]any{"cluster": "nope", "need_gb": 10}); err == nil {
		t.Error("unknown cluster must fail")
	}
	h.env.LoginRunner = nil
	if _, err := h.run(t, "probe_storage", map[string]any{"cluster": "hpcone", "need_gb": 10}); err == nil {
		t.Error("no runner must fail")
	}
}

func TestDraftScratchAuto(t *testing.T) {
	probe := func(h *harness, out string, calls *int) {
		h.env.LoginRunner = func(context.Context, config.Cluster) (backend.Runner, error) {
			return &fakeRunner{fn: func(context.Context, []string) (backend.Result, error) {
				*calls++
				return backend.Result{Stdout: out}, nil
			}}, nil
		}
	}
	t.Run("probe finds TMPDIR", func(t *testing.T) {
		h, _ := slurmHarness(t, rules.SlurmHints{})
		var calls int
		probe(h, probeSample, &calls)
		out, err := h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "auto"}, nil))
		if err != nil {
			t.Fatal(err)
		}
		if calls == 0 || !strings.Contains(out, "Storage: node-local scratch $TMPDIR") || !strings.Contains(out, "not tested on a compute node") {
			t.Errorf("calls=%d, draft:\n%s", calls, out)
		}
	})
	t.Run("profile wins, no probe", func(t *testing.T) {
		h, _ := slurmHarness(t, rules.SlurmHints{LocalScratch: testLocal})
		var calls int
		probe(h, probeSample, &calls)
		out, err := h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "auto"}, nil))
		if err != nil || calls != 0 || !strings.Contains(out, "site profile names") {
			t.Errorf("err=%v calls=%d:\n%s", err, calls, out)
		}
	})
	t.Run("probe fails, job folder", func(t *testing.T) {
		h, _ := slurmHarness(t, rules.SlurmHints{})
		h.env.LoginRunner = nil
		out, err := h.run(t, "draft_job", draftSpec(map[string]any{"scratch": "auto"}, nil))
		if err != nil || !strings.Contains(out, "Storage: the job runs in its own folder") || !strings.Contains(out, "no node-local scratch was used") {
			t.Errorf("err=%v:\n%s", err, out)
		}
	})
}
