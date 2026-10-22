package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"labmill-client/internal/config"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

func TestTransferRoutesThroughFilesHost(t *testing.T) {
	var checked []string
	ts := &toolset{env: &Env{
		SSHCommand:    func(alias string) []string { return []string{"labmill", "ssh-exec", alias} },
		TransferCheck: func(_ context.Context, alias string) error { checked = append(checked, alias); return nil },
	}}
	for _, tc := range []struct{ name, files, want string }{
		{"files_host set", "hpcfiles", "hpcfiles"},
		{"ssh_host fallback", "", "hpclogin"},
	} {
		c := config.Cluster{Name: "hpcone", Type: config.TypeSlurm, SSHHost: "hpclogin", FilesHost: tc.files}
		checked = nil
		r, err := ts.remote(c, "labmill/x")
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if got := r.SSHCommand[len(r.SSHCommand)-1]; got != tc.want {
			t.Errorf("%s: ssh-exec alias = %q, want %q", tc.name, got, tc.want)
		}
		if err := r.Check(context.Background()); err != nil || len(checked) != 1 || checked[0] != tc.want {
			t.Errorf("%s: check alias = %v (%v), want %q", tc.name, checked, err, tc.want)
		}
		up := uploadPlan{cluster: c, dir: "labmill/x", entries: []store.ManifestEntry{{Path: "a", Size: 1}}}
		if s := up.proposal().Summary; !strings.Contains(s, "through "+tc.want) {
			t.Errorf("%s: upload approval %q doesn't name the transfer host", tc.name, s)
		}
		fp := fetchPlan{cluster: c, job: store.Job{RemoteDir: "labmill/x"}, paths: []string{"out"}, localDir: "/tmp/r"}
		if s := fp.proposal().Summary; !strings.Contains(s, "through "+tc.want) {
			t.Errorf("%s: fetch approval %q doesn't name the transfer host", tc.name, s)
		}
	}
	// Only files_host: still usable.
	if _, err := ts.remote(config.Cluster{Name: "n", FilesHost: "f"}, "d"); err != nil {
		t.Errorf("files_host alone: %v", err)
	}
}

// Approvals show the real host name behind the alias, and Propose and Run
// build the same text.
func TestApprovalsShowHostName(t *testing.T) {
	ts := &toolset{env: &Env{HostName: func(alias string) string {
		if alias == "hpc-files" {
			return "files.example.edu"
		}
		return ""
	}}}
	c := config.Cluster{Name: "hpcone", Type: config.TypeSlurm, SSHHost: "hpclogin", FilesHost: "hpc-files"}
	via := ts.via(c)
	if via != "hpc-files (files.example.edu)" {
		t.Fatalf("via = %q", via)
	}
	up := uploadPlan{cluster: c, dir: "x", entries: []store.ManifestEntry{{Path: "a", Size: 1}}, via: via}
	if s := up.proposal().Summary; !strings.Contains(s, "through hpc-files (files.example.edu)") {
		t.Errorf("upload summary %q", s)
	}
	fp := fetchPlan{cluster: c, job: store.Job{RemoteDir: "x"}, paths: []string{"o"}, localDir: "/tmp/r", via: via}
	if s := fp.proposal().Summary; !strings.Contains(s, "through hpc-files (files.example.edu)") {
		t.Errorf("fetch summary %q", s)
	}
	// Unknown alias: just the alias; same input gives the same text.
	if got := ts.via(config.Cluster{SSHHost: "other"}); got != "other" {
		t.Errorf("via(other) = %q", got)
	}
	if up.proposal().Summary != (uploadPlan{cluster: c, dir: "x", entries: up.entries, via: ts.via(c)}).proposal().Summary {
		t.Error("proposal not stable")
	}
}

func TestReplacedNote(t *testing.T) {
	if replacedNote(nil) != "" {
		t.Error("note for no replaced files")
	}
	if got := replacedNote([]string{"a.txt"}); !strings.Contains(got, "Replaced 1 existing file on the cluster") || !strings.Contains(got, "a.txt") {
		t.Errorf("one: %q", got)
	}
	var many []string
	for i := 0; i < 13; i++ {
		many = append(many, fmt.Sprintf("f%02d", i))
	}
	got := replacedNote(many)
	if !strings.Contains(got, "Replaced 13 existing files") || !strings.Contains(got, "f09") || strings.Contains(got, "f10") || !strings.Contains(got, "and 3 more") {
		t.Errorf("many: %q", got)
	}
}

// The upload result reports files the transfer replaced on the cluster.
func TestUploadResultReportsReplaced(t *testing.T) {
	h := newHarness(t)
	out, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": "rep"})
	if err != nil {
		t.Fatal(err)
	}
	var staged struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(out), &staged); err != nil {
		t.Fatal(err)
	}
	h.env.Upload = func(_ context.Context, _ string, entries []store.ManifestEntry, dst transfer.Remote, _ func(transfer.Progress)) error {
		dst.OnReplace(entries[0].Path, 1)
		return nil
	}
	res, err := h.run(t, "upload_data", map[string]any{"job_id": staged.JobID, "cluster": "hpcone"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res, "Replaced 1 existing file on the cluster") {
		t.Errorf("result = %q", res)
	}
}
