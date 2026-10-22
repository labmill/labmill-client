package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/store"
)

// Another job of the user (any origin) working in the folder, or a queue
// that can't be read, stops the cleanup before anything is removed.
func TestCleanupStopsWhenQueueShowsJobInFolder(t *testing.T) {
	dir := "/scratch/me/labmill/seg"
	tests := []struct {
		name string
		set  func(f *cleanupFixture)
		msg  string
	}{
		{"job in the folder", func(f *cleanupFixture) { f.squeueOut = "77 " + dir + "\n" }, "Job 77"},
		{"job below the folder", func(f *cleanupFixture) { f.squeueOut = "5 /elsewhere\n78 " + dir + "/sub/x\n" }, "Job 78"},
		{"squeue fails", func(f *cleanupFixture) { f.squeueFail = true }, "couldn't check the queue"},
		{"job in the real path", func(f *cleanupFixture) {
			f.realDir = "/lustre/me/labmill/seg"
			f.squeueOut = "79 /lustre/me/labmill/seg/x\n"
		}, "Job 79"},
		{"old Slurm without --me", func(f *cleanupFixture) { f.noMe = true; f.squeueOut = "80 " + dir + "\n" }, "Job 80"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCleanupFixture(t, scratchGlobal, dir)
			f.downloaded(t, "a")
			tc.set(f)
			tool := f.h.tools["cleanup_job"]
			p, _ := tool.Propose(jobIn(f.id))
			_, err := tool.Run(context.Background(), jobIn(f.id), agent.Decision{Approved: true, Proposal: p})
			if err == nil || !strings.Contains(err.Error(), tc.msg) || len(f.rm) != 0 {
				t.Errorf("err = %v, rm %v", err, f.rm)
			}
			for _, c := range f.calls() {
				if c[4] == "sha256sum" || c[4] == "rm" || c[4] == "rmdir" {
					t.Errorf("ran %v", c)
				}
			}
		})
	}
	// Jobs elsewhere don't stop it.
	f := newCleanupFixture(t, scratchGlobal, dir)
	f.downloaded(t, "a")
	f.squeueOut = "9 /scratch/me/labmill/seg2\n"
	tool := f.h.tools["cleanup_job"]
	p, _ := tool.Propose(jobIn(f.id))
	if _, err := tool.Run(context.Background(), jobIn(f.id), agent.Decision{Approved: true, Proposal: p}); err != nil || len(f.rm) != 1 {
		t.Errorf("err = %v, rm %v", err, f.rm)
	}
}

// An active job drafted with a work_dir has an empty RemoteDir; its script's
// --chdir still blocks.
func TestCleanupBlockedByChdirOfActiveJob(t *testing.T) {
	dir := "/scratch/me/labmill/seg"
	for _, script := range []string{
		"#!/bin/bash\n#SBATCH --chdir=" + dir + "\n./run\n",
		"#!/bin/bash\n#SBATCH --chdir=" + dir + "/sub\n",
		"#!/bin/bash\n#SBATCH -D \"" + dir + "\"\n",
		"#!/bin/bash\ncd " + dir + " && ./run\n",
		"#!/bin/bash\n./run > " + dir + "/out.log\n",
	} {
		f := newCleanupFixture(t, scratchGlobal, dir)
		f.downloaded(t, "a")
		f.h.env.Store.CreateJob(context.Background(), store.Job{Name: "wd", Cluster: "hpcone", Script: script, State: store.StateRunning})
		if _, err := f.h.tools["cleanup_job"].Propose(jobIn(f.id)); err == nil || !strings.Contains(err.Error(), "still active in the same folder") {
			t.Errorf("script %q: err = %v", script, err)
		}
	}
	f := newCleanupFixture(t, scratchGlobal, dir)
	f.downloaded(t, "a")
	f.h.env.Store.CreateJob(context.Background(), store.Job{Name: "wd", Cluster: "hpcone", Script: "#SBATCH --chdir=" + dir + "2\ncd " + dir + "_old\n", State: store.StateRunning})
	f.h.env.Store.CreateJob(context.Background(), store.Job{Name: "done", Cluster: "hpcone", Script: "#SBATCH --chdir=" + dir + "\n", State: store.StateFetched})
	if _, err := f.h.tools["cleanup_job"].Propose(jobIn(f.id)); err != nil {
		t.Errorf("unrelated chdir blocked: %v", err)
	}
}
