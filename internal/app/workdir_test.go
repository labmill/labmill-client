package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/config"
	"labmill-client/internal/rules"
)

// 33.3: a job without data and without a work_dir runs in <login home>/labmill/<name>.
func TestDraftJobDefaultWorkDir(t *testing.T) {
	h := newHarness(t)
	out, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": map[string]any{"name": "gpu-check", "command": "nvidia-smi"}})
	if err != nil || !strings.Contains(out, "--chdir=/home/alice/labmill/gpu-check") || !strings.Contains(out, "Work folder: /home/alice/labmill/gpu-check") {
		t.Fatalf("default folder: %q %v", out, err)
	}
	// An explicit folder wins and gets no default note.
	out, err = h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": map[string]any{"name": "other", "command": "true", "work_dir": "/home/alice/mine"}})
	if err != nil || !strings.Contains(out, "--chdir=/home/alice/mine") || strings.Contains(out, "Work folder:") {
		t.Errorf("explicit folder: %q %v", out, err)
	}
}

func TestDraftJobDefaultWorkDirNoHome(t *testing.T) {
	h := newHarness(t)
	h.env.RemoteHome = func(context.Context, config.Cluster) (string, error) { return "", context.DeadlineExceeded }
	_, err := h.run(t, "draft_job", map[string]any{"cluster": "hpcone", "spec": map[string]any{"name": "gpu-check", "command": "true"}})
	if err == nil || !strings.Contains(err.Error(), "home folder") {
		t.Errorf("error %v", err)
	}
}

// 33.2: a draft with time_minutes=0 and no memory names the partition defaults.
func TestDraftJobPartitionDefaults(t *testing.T) {
	h, rr := slurmHarness(t, rules.SlurmHints{})
	rr.scontrol = "PartitionName=compute AllowGroups=ALL AllowAccounts=ALL DefaultTime=04:00:00 MaxTime=7-00:00:00 DefMemPerCPU=2000\n" +
		"PartitionName=preempt AllowAccounts=ALL DefaultTime=NONE MaxTime=02:00:00 DefMemPerNode=8000\n"
	out, err := h.run(t, "draft_job", draftSpec(nil, nil))
	if err != nil || !strings.Contains(out, "time limit: 4h0m0s") || !strings.Contains(out, "memory: 2000 MB per CPU") || !strings.Contains(out, "partition compute") {
		t.Fatalf("defaults: %q %v", out, err)
	}
	// An explicit time and memory leave nothing to say.
	out, err = h.run(t, "draft_job", draftSpec(nil, map[string]any{"time_minutes": 30, "memory_gb": 4}))
	if err != nil || strings.Contains(out, "Defaults from partition") {
		t.Errorf("explicit: %q %v", out, err)
	}
	// No DefaultTime: the maximum applies.
	out, err = h.run(t, "draft_job", draftSpec(nil, map[string]any{"partition": "preempt"}))
	if err != nil || !strings.Contains(out, "time limit: 2h0m0s (the partition maximum)") || !strings.Contains(out, "8000 MB per node") {
		t.Errorf("max: %q %v", out, err)
	}
}

func TestClusterInfoPartitionDefaults(t *testing.T) {
	h, rr := slurmHarness(t, rules.SlurmHints{})
	rr.scontrol = "PartitionName=compute AllowAccounts=lab1,lab2 DefaultTime=04:00:00 MaxTime=7-00:00:00 DefMemPerCPU=2000\n"
	out, err := h.run(t, "cluster_info", map[string]any{"cluster": "hpcone"})
	if err != nil || !strings.Contains(out, "default time 4h0m0s") || !strings.Contains(out, "default memory 2000 MB per CPU") || !strings.Contains(out, "only accounts: lab1, lab2") {
		t.Errorf("info: %q %v", out, err)
	}
}
