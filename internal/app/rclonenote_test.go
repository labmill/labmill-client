package app

import (
	"fmt"
	"labmill-client/internal/config"
	"strings"
	"testing"

	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// The rclone-lib parameter line is not in the approvals; it is announced when
// the transfer starts (with live progress and an end notice) and is part of
// the result, with a one-line summary.
func TestRcloneNoteInResultAndLive(t *testing.T) {
	h := newHarness(t)
	var got []transfer.Update
	h.env.Transfer = func(u transfer.Update) { got = append(got, u) }
	ctx := t.Context()
	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data"}, "job_name": "seg-train"}); err != nil {
		t.Fatal(err)
	}
	p, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Details, "rclone") || strings.Contains(p.Summary, "rclone") {
		t.Errorf("the approval mentions rclone: %q", p.Details)
	}
	res, err := h.run(t, "upload_data", map[string]any{"job_id": 1, "cluster": "hpcone"})
	if err != nil {
		t.Fatal(err)
	}
	const want = "rclone-lib: copy --files-from-raw files.txt --no-traverse --sftp-ssh 'labmill ssh-exec hpclogin' --sftp-shell-type none --sftp-disable-hashcheck "
	if !strings.Contains(res, want) || !strings.Contains(res, ":sftp:labmill/seg-train  # files.txt lists the 3 approved files") || !strings.Contains(res, "\nTransferred 1 B in ") {
		t.Errorf("upload result = %q", res)
	}
	if len(got) < 3 || got[0].Kind != "start" || !strings.HasPrefix(got[0].Text, want) || got[0].Tool != "upload_data" ||
		got[1].Kind != "progress" || !strings.HasPrefix(got[1].Text, "Transferred: 1 B / 1 B, 100%") || got[len(got)-1].Kind != "end" || got[len(got)-1].Text == "" {
		t.Errorf("updates = %+v", got)
	}

	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "x", Cluster: "hpcone", RemoteDir: "/home/alice/x", RemoteID: "1", State: store.StateCompleted})
	in := mustJSON(map[string]any{"job_id": id, "paths": []string{"out/a.txt"}})
	fp, err := h.tools["fetch_results"].Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fp.Details, "rclone") {
		t.Errorf("the fetch approval mentions rclone: %q", fp.Details)
	}
	got = nil
	res, err = h.run(t, "fetch_results", map[string]any{"job_id": id, "paths": []string{"out/a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "\n\nrclone-lib: copy --sftp-ssh 'labmill ssh-exec hpclogin'") || !strings.Contains(res, ":sftp:/home/alice/x/out/a.txt ") {
		t.Errorf("fetch result = %q", res)
	}
	if !strings.Contains(res, "\nthen: sha256sum on hpclogin (read-only)") {
		t.Errorf("fetch result lacks the sha256sum note: %q", res)
	}
	if len(got) != 2 || got[0].Kind != "start" || got[1].Kind != "end" || got[1].Tool != "fetch_results" {
		t.Errorf("fetch updates = %+v", got) // the fake download reports no progress
	}
}

func TestRcloneNoteCapsDownloadLines(t *testing.T) {
	h := newHarness(t)
	ts := &toolset{env: h.env}
	var rels []string
	for i := 0; i < 30; i++ {
		rels = append(rels, fmt.Sprintf("f%d.txt", i))
	}
	got := ts.rcloneNote(config.Cluster{}, &transfer.S3Remote{Bucket: "b"}, "d", "/r", rels, false)
	lines := strings.Split(got, "\n")
	if len(lines) != maxRcloneLines+1 || !strings.Contains(lines[maxRcloneLines], "and 10 more") {
		t.Errorf("lines = %d, last %q", len(lines), lines[len(lines)-1])
	}
}
