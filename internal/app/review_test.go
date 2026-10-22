package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

func (h *harness) write(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(h.home, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// stageUploadDraft runs stage_data, upload_data and draft_job for the mri
// folder and returns the job id.
func (h *harness) stageUploadDraft(t *testing.T, name string) int64 {
	t.Helper()
	out, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": name})
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		JobID int64 `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": st.JobID, "cluster": "hpcone"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "draft_job", map[string]any{"job_id": st.JobID, "cluster": "hpcone", "spec": map[string]any{"command": "python train.py"}}); err != nil {
		t.Fatal(err)
	}
	return st.JobID
}

// ---- finding 1: local privacy rules ----------------------------------------

func TestPrivacyPreviewRefusesSecrets(t *testing.T) {
	h := newHarness(t)
	key := h.write(t, ".ssh/id_ed25519", "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAASECRET\n")
	out, err := h.run(t, "preview_file", map[string]any{"path": key, "lines": 20})
	if err == nil || strings.Contains(out, "AAAASECRET") {
		t.Errorf("preview of ~/.ssh/id_ed25519: %q, %v", out, err)
	}
	// A secret-looking name is refused even in a plain folder.
	for _, name := range []string{"data/aws_credentials.txt", "data/server.pem", "data/Secret-notes.txt", "data/id_rsa", "data/vault.kdbx", "data/login.keychain-db", "data/tls.key", "data/.env.local"} {
		p := h.write(t, name, "TOPSECRETVALUE")
		if out, err := h.run(t, "preview_file", map[string]any{"path": p}); err == nil || strings.Contains(out, "TOPSECRETVALUE") {
			t.Errorf("preview of %s: %q, %v", name, out, err)
		}
	}
}

func TestPrivacyFindLocalData(t *testing.T) {
	h := newHarness(t)
	h.write(t, ".config/gcloud/legacy/adc.json", "{}")
	if out, err := h.run(t, "find_local_data", map[string]any{"query": "gcloud", "under": filepath.Join(h.home, ".config")}); err == nil {
		t.Errorf("find under ~/.config succeeded: %q", out)
	}
	out, err := h.run(t, "find_local_data", map[string]any{"query": "gcloud"})
	if err != nil || strings.Contains(out, "gcloud") {
		t.Errorf("find without under listed a hidden folder: %q, %v", out, err)
	}

	h.write(t, "data/mri-2026/aws_credentials.txt", "x")
	h.write(t, "data/mri-2026/id_rsa", "x")
	h.write(t, "data/mri-2026/.hidden-file", "x")
	out, err = h.run(t, "find_local_data", map[string]any{"query": "mri 2026"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3 files") || strings.Contains(out, "credentials") || strings.Contains(out, "id_rsa") || strings.Contains(out, ".hidden-file") {
		t.Errorf("find output lists or counts private files:\n%s", out)
	}
	// A folder whose own name looks secret is not listed either.
	h.write(t, "data/my-secret-project/a.txt", "x")
	if out, _ := h.run(t, "find_local_data", map[string]any{"query": "project"}); strings.Contains(out, "secret") {
		t.Errorf("secret-named folder listed: %q", out)
	}
}

func TestPrivacyStageData(t *testing.T) {
	h := newHarness(t)
	h.write(t, ".ssh/id_ed25519", "k")
	h.write(t, "data/mri-2026/aws_credentials.txt", "x")
	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{".ssh"}, "job_name": "x"}); err == nil {
		t.Error("stage_data of .ssh succeeded")
	}
	if _, err := h.run(t, "stage_data", map[string]any{"root": filepath.Join(h.home, ".ssh"), "paths": []string{"id_ed25519"}, "job_name": "x"}); err == nil {
		t.Error("stage_data with root ~/.ssh succeeded")
	}
	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026/aws_credentials.txt"}, "job_name": "x"}); err == nil {
		t.Error("stage_data of a credentials file succeeded")
	}
	// A folder that merely contains a secret-looking file stages without it.
	out, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data"}, "job_name": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		JobID   int64    `json:"job_id"`
		Files   int      `json:"files"`
		Bytes   int64    `json:"bytes"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 || !strings.Contains(strings.Join(st.Skipped, ","), "aws_credentials.txt") {
		t.Errorf("stage result = %+v", st)
	}
	m, _ := h.env.Store.Manifest(context.Background(), st.JobID)
	for _, e := range m {
		if strings.Contains(e.Path, "credentials") {
			t.Errorf("manifest holds %s", e.Path)
		}
	}
}

// Naming the root itself ("." or its full path) stages everything public.
func TestStageDataRootDot(t *testing.T) {
	h := newHarness(t)
	h.write(t, "data/mri-2026/aws_credentials.txt", "x")
	root := filepath.Join(h.home, "data/mri-2026")
	for i, p := range []string{".", root} {
		out, err := h.run(t, "stage_data", map[string]any{"root": root, "paths": []string{p}, "job_name": "dot" + string(rune('a'+i))})
		if err != nil {
			t.Fatalf("stage %q: %v", p, err)
		}
		if !strings.Contains(out, `"files":3`) || !strings.Contains(out, "aws_credentials.txt") {
			t.Errorf("stage %q: %s", p, out)
		}
	}
	// From the home folder, hidden and private top-level entries are left out.
	h.write(t, ".ssh/id_rsa", "k")
	out, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"."}, "job_name": "home"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "id_rsa") && !strings.Contains(out, `"skipped"`) {
		t.Errorf("unexpected: %s", out)
	}
	m, _ := h.env.Store.Manifest(context.Background(), 3)
	for _, e := range m {
		if strings.HasPrefix(e.Path, ".") || strings.Contains(e.Path, "credentials") {
			t.Errorf("home staging holds %s", e.Path)
		}
	}
}

func TestPrivacyRules(t *testing.T) {
	for _, n := range []string{"id_rsa", "ID_ED25519", "a.pem", "a.KEY", "a.p12", "a.pfx", "a.kdbx", "login.keychain", "x.keychain-db", "aws_Credentials.json", "my-SECRET", ".netrc", ".env", ".env.prod"} {
		if !isSecretName(n) {
			t.Errorf("%q should look secret", n)
		}
	}
	for _, n := range []string{"data.csv", "keys.txt", "idea.txt", "monkey", "scan001.dcm", "environment.yml"} {
		if isSecretName(n) {
			t.Errorf("%q should not look secret", n)
		}
	}
	old := privacyOS
	defer func() { privacyOS = old }()
	for goos, name := range map[string]string{"darwin": "Library", "windows": "AppData"} {
		privacyOS = goos
		if checkRel(name+"/Preferences/x.plist") == nil {
			t.Errorf("%s: %s allowed", goos, name)
		}
		if checkRel("data/"+name+"/x") != nil {
			t.Errorf("%s: nested %s refused", goos, name)
		}
	}
	privacyOS = "linux"
	if checkRel("Library/x") != nil || checkRel("AppData/x") != nil {
		t.Error("Library/AppData refused on linux")
	}
	if checkRel("a/.b/c") == nil || checkRel("a/b/c") != nil {
		t.Error("dot component rule wrong")
	}
}

func TestPrivacyLibraryOffLimits(t *testing.T) {
	old := privacyOS
	privacyOS = "darwin"
	defer func() { privacyOS = old }()
	h := newHarness(t)
	h.write(t, "Library/Preferences/x.plist", "x")
	h.write(t, "data/mri-2026/Library/y.txt", "y")
	if _, err := h.run(t, "preview_file", map[string]any{"path": filepath.Join(h.home, "Library/Preferences/x.plist")}); err == nil {
		t.Error("preview in ~/Library succeeded")
	}
	if out, _ := h.run(t, "find_local_data", map[string]any{"query": "preferences"}); strings.Contains(out, "Preferences") {
		t.Errorf("find listed ~/Library: %q", out)
	}
	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"Library"}, "job_name": "lib"}); err == nil {
		t.Error("stage of ~/Library succeeded")
	}
}

// ---- finding 3: upload_data guards ------------------------------------------

func TestUploadAfterSubmitRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": "j"})
	h.env.Store.UpdateJob(ctx, 1, func(j *store.Job) error {
		j.RemoteID, j.Cluster, j.State, j.Script = "4242", "hpcone", store.StateRunning, "x"
		return nil
	})
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": 1, "cluster": "hpcone"}); err == nil {
		t.Error("upload after submit succeeded")
	}
	if j, _ := h.env.Store.Job(ctx, 1); j.State != store.StateRunning || j.RemoteDir != "" || len(h.uploads) != 0 {
		t.Errorf("job changed or uploaded: %+v", j)
	}
}

func TestUploadClusterFixedOnceDrafted(t *testing.T) {
	h := newHarness(t)
	id := h.stageUploadDraft(t, "j")
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": id, "cluster": "second"}); err == nil {
		t.Error("upload to another cluster after drafting succeeded")
	}
	// The same cluster is fine (re-upload of the same data).
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": id, "cluster": "hpcone"}); err != nil {
		t.Errorf("re-upload to the same cluster: %v", err)
	}
}

func TestFailedUploadKeepsDraftedState(t *testing.T) {
	h := newHarness(t)
	id := h.stageUploadDraft(t, "j")
	ctx := context.Background()
	before, _ := h.env.Store.Job(ctx, id)
	h.env.Upload = func(context.Context, string, []store.ManifestEntry, transfer.Remote, func(transfer.Progress)) error {
		return errors.New("link down")
	}
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": id, "cluster": "hpcone"}); err == nil {
		t.Fatal("failed upload reported success")
	}
	after, _ := h.env.Store.Job(ctx, id)
	if after.State != before.State || after.Script != before.Script || after.RemoteDir != before.RemoteDir {
		t.Errorf("job changed by a failed upload:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestUploadRechecksStagedRoot(t *testing.T) {
	h := newHarness(t)
	h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": "j"})
	ctx := context.Background()
	// A stored root outside the allowed folders is refused.
	h.env.Store.AddEvent(ctx, 1, eventStaged, "/etc")
	if _, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone"})); err == nil {
		t.Error("upload from a root outside LocalRoots proposed")
	}
	h.env.Store.AddEvent(ctx, 1, eventStaged, filepath.Join(h.home, ".hidden"))
	if _, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone"})); err == nil {
		t.Error("upload from a hidden root proposed")
	}
}

// ---- finding 4: approval integrity ------------------------------------------

func TestApprovalIntegrity(t *testing.T) {
	ctx := context.Background()
	t.Run("upload manifest changed", func(t *testing.T) {
		h := newHarness(t)
		h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": "j"})
		in := mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone"})
		p, err := h.tools["upload_data"].Propose(in)
		if err != nil {
			t.Fatal(err)
		}
		h.env.Store.SetManifest(ctx, 1, []store.ManifestEntry{{Path: "data/mri-2026/scan001.dcm", Size: 1, SHA256: "ab"}})
		if _, err := h.tools["upload_data"].Run(ctx, in, agent.Decision{Approved: true, Proposal: p}); err == nil || !strings.Contains(err.Error(), "changed since") {
			t.Errorf("upload Run after manifest change: %v", err)
		}
		if len(h.uploads) != 0 {
			t.Error("uploaded anyway")
		}
	})
	t.Run("upload destination differs", func(t *testing.T) {
		h := newHarness(t)
		h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": "j"})
		p, _ := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone", "remote_dir": "a/b"}))
		_, err := h.tools["upload_data"].Run(ctx, mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone", "remote_dir": "other"}), agent.Decision{Approved: true, Proposal: p})
		if err == nil {
			t.Error("Run with different input than the approved proposal succeeded")
		}
	})
	t.Run("submit cluster changed", func(t *testing.T) {
		h := newHarness(t)
		id := h.stageUploadDraft(t, "j")
		in := mustJSON(map[string]any{"job_id": id})
		p, _ := h.tools["submit_job"].Propose(in)
		h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.Cluster = "second"; return nil })
		if _, err := h.tools["submit_job"].Run(ctx, in, agent.Decision{Approved: true, Proposal: p}); err == nil {
			t.Error("submit after the cluster changed succeeded")
		}
		if len(h.be.submitted) != 0 {
			t.Error("submitted anyway")
		}
	})
	t.Run("cancel cluster changed", func(t *testing.T) {
		h := newHarness(t)
		id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "x", Cluster: "hpcone", RemoteID: "77", State: store.StateRunning})
		in := mustJSON(map[string]any{"job_id": id})
		p, _ := h.tools["cancel_job"].Propose(in)
		h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.RemoteID = "99"; return nil })
		if _, err := h.tools["cancel_job"].Run(ctx, in, agent.Decision{Approved: true, Proposal: p}); err == nil || len(h.be.canceled) != 0 {
			t.Errorf("cancel after the remote id changed: %v %v", err, h.be.canceled)
		}
	})
	t.Run("fetch folder changed", func(t *testing.T) {
		h := newHarness(t)
		id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "x", Cluster: "hpcone", RemoteDir: "/home/alice/x", RemoteID: "1", State: store.StateCompleted})
		in := mustJSON(map[string]any{"job_id": id, "paths": []string{"out"}})
		p, _ := h.tools["fetch_results"].Propose(in)
		h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.RemoteDir = "/home/alice/other"; return nil })
		if _, err := h.tools["fetch_results"].Run(ctx, in, agent.Decision{Approved: true, Proposal: p}); err == nil {
			t.Error("fetch after the remote dir changed succeeded")
		}
	})
}

// ---- finding 5: upload breakdown --------------------------------------------

func TestUploadProposalBreakdown(t *testing.T) {
	h := newHarness(t)
	h.write(t, "proj/top.txt", "t")
	h.write(t, "proj/scans/a.dcm", "aaaa")
	h.write(t, "proj/scans/b.dcm", "bb")
	h.write(t, "proj/labels/l.csv", "l")
	h.run(t, "stage_data", map[string]any{"root": filepath.Join(h.home, "proj"), "paths": []string{"."}, "job_name": "p"})
	p, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Details, "By folder:\n") {
		t.Fatalf("details do not start with the breakdown:\n%s", p.Details)
	}
	for _, want := range []string{"scans/: 2 files, 6 B", "labels/: 1 file, 1 B", "(files directly in the folder): 1 file, 1 B"} {
		if !strings.Contains(p.Details, want) {
			t.Errorf("missing %q in:\n%s", want, p.Details)
		}
	}
	if i, j := strings.Index(p.Details, "By folder:"), strings.Index(p.Details, "Files:"); i != 0 || j < 0 {
		t.Errorf("breakdown must come before the file list:\n%s", p.Details)
	}
}

// ---- finding 6: fetch_results -------------------------------------------------

func TestFetchLocalDirAndNames(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "res", Cluster: "hpcone", RemoteDir: "/home/alice/x", RemoteID: "1", State: store.StateCompleted})
	propose := func(in map[string]any) (agent.Proposal, error) {
		in["job_id"] = id
		return h.tools["fetch_results"].Propose(mustJSON(in))
	}
	for name, in := range map[string]map[string]any{
		"outside roots":   {"paths": []string{"a"}, "local_dir": "/root/.ssh"},
		"/etc":            {"paths": []string{"a"}, "local_dir": "/etc"},
		"hidden in home":  {"paths": []string{"a"}, "local_dir": filepath.Join(h.home, ".ssh")},
		"hidden below":    {"paths": []string{"a"}, "local_dir": filepath.Join(h.home, "labmill-results", ".x")},
		"secret name":     {"paths": []string{"a"}, "local_dir": filepath.Join(h.home, "credentials")},
		"ctrl in local":   {"paths": []string{"a"}, "local_dir": filepath.Join(h.home, "x\x1b[2K")},
		"ctrl in path":    {"paths": []string{"r\x1b[2K\rDownload r from hpcone"}},
		"newline in path": {"paths": []string{"a\nb"}},
		"dotdot path":     {"paths": []string{"../x"}},
		"absolute path":   {"paths": []string{"/etc/passwd"}},
	} {
		if p, err := propose(in); err == nil {
			t.Errorf("%s: accepted, summary %q", name, p.Summary)
		}
	}
	// Allowed: the default folder, a new folder inside the results folder, a folder under a LocalRoot.
	for name, in := range map[string]map[string]any{
		"default":      {"paths": []string{"a"}},
		"results sub":  {"paths": []string{"a"}, "local_dir": filepath.Join(h.home, "labmill-results", "other")},
		"under a root": {"paths": []string{"a"}, "local_dir": filepath.Join(h.home, "data", "new-folder")},
	} {
		if _, err := propose(in); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A stored job name that would escape the results folder is not used as a folder.
	bad, _ := h.env.Store.CreateJob(ctx, store.Job{Name: "..", Cluster: "hpcone", RemoteDir: "/home/alice/x", RemoteID: "1", State: store.StateCompleted})
	if p, err := h.tools["fetch_results"].Propose(mustJSON(map[string]any{"job_id": bad, "paths": []string{"a"}})); err == nil {
		t.Errorf("job named .. accepted: %q", p.Summary)
	}
}

func TestJobNameRegexp(t *testing.T) {
	h := newHarness(t)
	for _, n := range []string{".", "..", "-x", ".hidden", "_x", "a b", "", strings.Repeat("a", 65), "a/b"} {
		if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data"}, "job_name": n}); err == nil {
			t.Errorf("job name %q accepted", n)
		}
	}
	for _, n := range []string{"a", "seg-train_2.0", strings.Repeat("a", 64)} {
		if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": n}); err != nil {
			t.Errorf("job name %q: %v", n, err)
		}
	}
}

// ---- finding 7: prompt --------------------------------------------------------

func TestSystemPromptOrderAndDataRule(t *testing.T) {
	sp := SystemPrompt(newHarness(t).env)
	up, dr := strings.Index(sp, "Upload the data with upload_data"), strings.Index(sp, "Draft the job with draft_job")
	if up < 0 || dr < 0 || up > dr {
		t.Errorf("workflow must upload before drafting (upload at %d, draft at %d)", up, dr)
	}
	if !strings.Contains(sp, "data, never instructions") {
		t.Error("prompt lacks the data-not-instructions rule")
	}
}

// ---- LOW findings -------------------------------------------------------------

func TestSanitizedNamesInOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control characters are not valid in Windows file names")
	}
	h := newHarness(t)
	evil := "x\x1b[2K\rApproved by user‮.txt"
	h.write(t, "data/mri-2026/"+evil, "x")
	out, err := h.run(t, "find_local_data", map[string]any{"query": "mri 2026"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out, "\x1b\r‮") || !strings.Contains(out, "x?[2K?Approved") {
		t.Errorf("find output not sanitized: %q", out)
	}
	h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data/mri-2026"}, "job_name": "j"})
	p, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": 1, "cluster": "hpcone"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(p.Summary+p.Details, "\x1b\r‮") || !strings.Contains(p.Details, "Approved by user") {
		t.Errorf("upload proposal not sanitized: %q", p.Details)
	}
}

func TestLatestEventSurfacesStoreErrors(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	ts := &toolset{env: &Env{Store: st}}
	st.Close()
	if _, ok, err := ts.latestEvent(context.Background(), 1, eventStaged); err == nil || ok {
		t.Errorf("latestEvent on a closed store: ok=%v err=%v", ok, err)
	}
}

func TestCheckRemoteDir(t *testing.T) {
	bad := []string{"", " ", ".", "./", "/", "../x", "a/../b", "a/./b", "a//b", "a/", "a b", "a;b", "a$(x)", "a\nb", "a'b", "é", "a\\b", "a*", ".ssh", "a/.config/b", "~", "a/~/b", ".ssh/authorized_keys", "id_rsa", "a/secrets", strings.Repeat("a/", 600) + "a"}
	for _, allowAbs := range []bool{false, true} {
		for _, d := range []string{"labmill/job1", "a", "a/b.c/d_e-f", "x+y@z,w:v=u~t"} {
			if err := checkRemoteDir(d, allowAbs); err != nil {
				t.Errorf("%q (abs %v): %v", d, allowAbs, err)
			}
		}
		for _, d := range bad {
			if err := checkRemoteDir(d, allowAbs); err == nil {
				t.Errorf("%q (abs %v) accepted", d, allowAbs)
			}
			if err := checkRemoteDir("/"+d, allowAbs); err == nil {
				t.Errorf("%q (abs %v) accepted", "/"+d, allowAbs)
			}
		}
	}
	// Absolute folders (group storage) only where they make sense: not for S3.
	for _, d := range []string{"/abs/dir/x", "/storage/t3/lab/labmill-live/agent"} {
		if err := checkRemoteDir(d, false); err == nil {
			t.Errorf("%q accepted without allowAbs", d)
		}
		if err := checkRemoteDir(d, true); err != nil {
			t.Errorf("%q: %v", d, err)
		}
	}
	for _, d := range []string{"//abs", "/abs/../x", "/abs/", "/abs//x", "/a b", "/../x",
		"/home/u/.ssh", "/home/u/.config/x", "/home", "/storage", "/home/u", "/etc/x/y", "/usr/local/x", "/tmp/a/b", "/root/a/b", "/home/~/x"} {
		if err := checkRemoteDir(d, true); err == nil {
			t.Errorf("%q accepted", d)
		}
	}
}

func TestFetchResultsChecksEachDestination(t *testing.T) {
	h := newHarness(t)
	id, _ := h.env.Store.CreateJob(context.Background(), store.Job{Name: "f", Cluster: "hpcone", Backend: "slurm", RemoteID: "1", RemoteDir: "/home/alice/labmill/f", State: store.StateCompleted})
	bad := []string{".ssh/authorized_keys", ".bashrc"}
	if runtime.GOOS == "darwin" {
		bad = append(bad, "Library/LaunchAgents/x.plist")
	}
	for _, rp := range bad {
		_, err := h.tools["fetch_results"].Propose(mustJSON(map[string]any{"job_id": id, "paths": []string{rp}, "local_dir": h.home}))
		if err == nil {
			t.Errorf("fetch %q into home was proposed", rp)
		}
	}
	if _, err := h.tools["fetch_results"].Propose(mustJSON(map[string]any{"job_id": id, "paths": []string{"results/model.pt"}, "local_dir": h.home})); err != nil {
		t.Errorf("normal fetch refused: %v", err)
	}
}

func TestSecretNameLookalikes(t *testing.T) {
	for _, n := range []string{"\u017fecret.txt", "\u0131d_rsa"} {
		if !isSecretName(n) {
			t.Errorf("isSecretName(%q) = false", n)
		}
	}
}
