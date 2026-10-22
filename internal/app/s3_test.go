package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// An s3 cluster is approved with its full s3:// destination and AWS profile
// (never credentials), uploads and downloads use the S3 hooks, and the SFTP
// hooks are never used as a fallback.
func TestS3Cluster(t *testing.T) {
	h := newHarness(t)
	h.env.Config.Clusters = append(h.env.Config.Clusters, config.Cluster{
		Name: "bucket", Type: config.TypeSlurm, SSHHost: "hpclogin", Transfer: config.TransferS3,
		S3: &config.S3{Bucket: "my-bucket", Prefix: "labmill", Region: "us-west-2", Profile: "lab"},
	})
	sftpUploads := len(h.uploads)
	h.env.Download = func(context.Context, transfer.Remote, []string, string, func(transfer.Progress)) ([]string, error) {
		t.Error("SFTP download used for an s3 cluster")
		return nil, errors.New("must not be called")
	}
	var gotUp, gotDown []transfer.S3Remote
	var gotPaths []string
	var gotEntries int
	h.env.UploadS3 = func(_ context.Context, root string, entries []store.ManifestEntry, dst transfer.S3Remote, _ func(transfer.Progress)) error {
		gotUp = append(gotUp, dst)
		gotEntries = len(entries)
		return nil
	}
	h.env.DownloadS3 = func(_ context.Context, src transfer.S3Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
		gotDown = append(gotDown, src)
		gotPaths = paths
		return []string{filepath.Join(localDir, paths[0])}, nil
	}

	if _, err := h.run(t, "stage_data", map[string]any{"root": filepath.Join(h.home, "data"), "paths": []string{"mri-2026"}, "job_name": "j1"}); err != nil {
		t.Fatal(err)
	}
	up := h.tools["upload_data"]
	in := mustJSON(map[string]any{"job_id": 1, "cluster": "bucket"})
	p, err := up.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Summary, "s3://my-bucket/labmill/j1 (AWS profile lab, region us-west-2)") {
		t.Errorf("summary = %q", p.Summary)
	}
	if len(gotUp) != 0 {
		t.Error("upload ran before approval")
	}
	out, err := up.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "s3://my-bucket/labmill/j1") {
		t.Errorf("run output = %q", out)
	}
	want := transfer.S3Remote{Bucket: "my-bucket", Prefix: "labmill", Dir: "j1", Region: "us-west-2", Profile: "lab"}
	if len(gotUp) != 1 || gotUp[0] != want || gotEntries != 3 {
		t.Errorf("UploadS3 got %+v (%d entries), want %+v", gotUp, gotEntries, want)
	}
	if len(h.uploads) != sftpUploads {
		t.Error("SFTP upload hook was invoked")
	}

	j, err := h.env.Store.Job(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if j.RemoteDir != "s3://my-bucket/labmill/j1" || j.Cluster != "bucket" {
		t.Errorf("job = %+v", j)
	}

	fr := h.tools["fetch_results"]
	fin := mustJSON(map[string]any{"job_id": 1, "paths": []string{"out.txt"}})
	fp, err := fr.Propose(fin)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp.Summary, "s3://my-bucket/labmill/j1 (AWS profile lab, region us-west-2)") {
		t.Errorf("fetch summary = %q", fp.Summary)
	}
	if len(gotDown) != 0 {
		t.Error("download ran before approval")
	}
	if _, err := fr.Run(context.Background(), fin, agent.Decision{Approved: true, Proposal: fp}); err != nil {
		t.Fatal(err)
	}
	wantDown := transfer.S3Remote{Bucket: "my-bucket", Prefix: "labmill", Dir: "j1", Region: "us-west-2", Profile: "lab"}
	if len(gotDown) != 1 || gotDown[0] != wantDown || len(gotPaths) != 1 || gotPaths[0] != "out.txt" {
		t.Errorf("DownloadS3 got %+v %v", gotDown, gotPaths)
	}

	// A job whose folder is not inside the cluster's S3 location is refused.
	id, err := h.env.Store.CreateJob(context.Background(), store.Job{Name: "r", Cluster: "bucket", RemoteDir: "/home/x/r", RemoteID: "1", State: store.StateCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fr.Propose(mustJSON(map[string]any{"job_id": id, "paths": []string{"out.txt"}})); err == nil || !strings.Contains(err.Error(), "isn't inside this cluster's S3 location") {
		t.Errorf("fetch Propose err = %v", err)
	}

	lc, err := h.run(t, "list_clusters", map[string]any{})
	if err != nil || !strings.Contains(lc, "transfer s3 (s3://my-bucket/labmill)") {
		t.Errorf("list_clusters = %q, %v", lc, err)
	}
}

// s3Harness returns a harness with an s3 cluster whose uploads and downloads
// succeed without touching a network.
func s3Harness(t *testing.T, endpoint string) *harness {
	t.Helper()
	h := newHarness(t)
	h.env.Config.Clusters = append(h.env.Config.Clusters, config.Cluster{
		Name: "bucket", Type: config.TypeSlurm, SSHHost: "hpclogin", Transfer: config.TransferS3,
		S3: &config.S3{Bucket: "my-bucket", Prefix: "labmill", Region: "us-west-2", Endpoint: endpoint},
	})
	oldBackend := h.env.Backend
	h.env.Backend = func(ctx context.Context, c config.Cluster) (backend.Backend, error) {
		if c.Name == "bucket" {
			return h.be, nil
		}
		return oldBackend(ctx, c)
	}
	h.env.UploadS3 = func(context.Context, string, []store.ManifestEntry, transfer.S3Remote, func(transfer.Progress)) error {
		return nil
	}
	h.env.DownloadS3 = func(_ context.Context, _ transfer.S3Remote, paths []string, localDir string, _ func(transfer.Progress)) ([]string, error) {
		return []string{filepath.Join(localDir, paths[0])}, nil
	}
	return h
}

func TestS3DraftNeedsClusterWorkDir(t *testing.T) {
	h := s3Harness(t, "")
	if _, err := h.run(t, "stage_data", map[string]any{"root": filepath.Join(h.home, "data"), "paths": []string{"mri-2026"}, "job_name": "j1"}); err != nil {
		t.Fatal(err)
	}
	out, err := h.run(t, "upload_data", map[string]any{"job_id": 1, "cluster": "bucket"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "aws s3 cp --recursive s3://my-bucket/labmill/j1") || !strings.Contains(out, "work_dir") {
		t.Errorf("upload message lacks the S3 hint: %q", out)
	}
	_, err = h.run(t, "draft_job", map[string]any{"job_id": 1, "cluster": "bucket", "spec": map[string]any{"command": "python train.py"}})
	if err == nil || !strings.Contains(err.Error(), "data is in S3 at s3://my-bucket/labmill/j1, not on the cluster") || !strings.Contains(err.Error(), "fetch_results") {
		t.Fatalf("draft without work_dir: %v", err)
	}
	out, err = h.run(t, "draft_job", map[string]any{"job_id": 1, "cluster": "bucket", "spec": map[string]any{"command": "python train.py", "work_dir": "/home/u/run1"}})
	if err != nil {
		t.Fatalf("draft with work_dir: %v", err)
	}
	if !strings.Contains(out, "/home/u/run1") {
		t.Errorf("script lacks the work dir: %q", out)
	}
	if strings.Contains(out, "--chdir=s3://") || strings.Contains(out, "chdir s3://") {
		t.Errorf("s3 URL used as work dir: %q", out)
	}
}

// Data uploaded through one S3 endpoint is not fetched through another.
func TestS3FetchRefusesOtherEndpoint(t *testing.T) {
	h := s3Harness(t, "http://minio-a:9000")
	if _, err := h.run(t, "stage_data", map[string]any{"root": filepath.Join(h.home, "data"), "paths": []string{"mri-2026"}, "job_name": "j1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": 1, "cluster": "bucket"}); err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"job_id": 1, "paths": []string{"out.txt"}}
	p, err := h.tools["fetch_results"].Propose(mustJSON(in))
	if err != nil || !strings.Contains(p.Summary, "via minio-a:9000") || !strings.Contains(p.Summary, "region us-west-2") {
		t.Fatalf("propose = %+v, %v", p, err)
	}
	for i := range h.env.Config.Clusters {
		if h.env.Config.Clusters[i].Name == "bucket" {
			h.env.Config.Clusters[i].S3.Endpoint = "http://minio-b:9000"
		}
	}
	if _, err := h.tools["fetch_results"].Propose(mustJSON(in)); err == nil || !strings.Contains(err.Error(), "different S3 endpoint") {
		t.Errorf("propose after endpoint change: %v", err)
	}
}

// A fetch that finds nothing is an error and the job is not marked fetched.
func TestFetchNothingFoundNotMarkedFetched(t *testing.T) {
	h := s3Harness(t, "")
	h.env.DownloadS3 = func(context.Context, transfer.S3Remote, []string, string, func(transfer.Progress)) ([]string, error) {
		return nil, errors.New("download out: nothing found at that path in the job's folder")
	}
	if _, err := h.run(t, "stage_data", map[string]any{"root": filepath.Join(h.home, "data"), "paths": []string{"mri-2026"}, "job_name": "j1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "upload_data", map[string]any{"job_id": 1, "cluster": "bucket"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.run(t, "fetch_results", map[string]any{"job_id": 1, "paths": []string{"out"}}); err == nil || !strings.Contains(err.Error(), "nothing found") {
		t.Fatalf("fetch err = %v", err)
	}
	j, _ := h.env.Store.Job(context.Background(), 1)
	if j.State == store.StateFetched {
		t.Errorf("job marked fetched: %+v", j)
	}
}

func TestNormEndpoint(t *testing.T) {
	for _, c := range [][2]string{
		{"http://MinIO:9000/", "http://minio:9000"},
		{"HTTPS://s3.example.org", "https://s3.example.org"},
		{"", ""},
	} {
		if got := normEndpoint(c[0]); got != c[1] {
			t.Errorf("normEndpoint(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	if normEndpoint("http://a:9000/") != normEndpoint("http://A:9000") {
		t.Error("harmless edits must compare equal")
	}
	if normEndpoint("http://a:9000") == normEndpoint("http://b:9000") {
		t.Error("different hosts must differ")
	}
}
