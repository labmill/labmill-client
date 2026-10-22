package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const editFixture = `# Labmill clusters (my notes stay)
site: example.edu
clusters:
  # the HPC cluster
  - name: hpcone
    type: slurm
    ssh_host: hpclogin # login host
    transfer: sftp
  - name: second
    type: slurm
    ssh_host: hpclogin
    s3:
      bucket: my-bucket # staging
      region: us-west-2
`

func TestPutClusterYAMLKeepsComments(t *testing.T) {
	c := Cluster{Name: "hpcone", Type: TypeSlurm, SSHHost: "hpc", Transfer: TransferSFTP, Timezone: "America/Los_Angeles"}
	out, err := PutClusterYAML([]byte(editFixture), "hpcone", c)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	t.Logf("edited:\n%s", s)
	for _, want := range []string{"# Labmill clusters (my notes stay)", "# the HPC cluster", "ssh_host: hpc # login host", "timezone: America/Los_Angeles", "bucket: my-bucket # staging"} {
		if !strings.Contains(s, want) {
			t.Errorf("result lacks %q:\n%s", want, s)
		}
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := cfg.Cluster("hpcone")
	if got != c {
		t.Errorf("hpcone = %+v, want %+v", got, c)
	}
	if i, j := strings.Index(s, "transfer: sftp"), strings.Index(s, "timezone:"); i < 0 || j < i {
		t.Errorf("timezone should follow transfer:\n%s", s)
	}
}

func TestPutClusterYAMLRenameS3AndAppend(t *testing.T) {
	c := Cluster{Name: "gpu", Type: TypeSlurm, SSHHost: "hpc2", S3: &S3{Bucket: "other-bucket", Profile: "hpcx"}}
	out, err := PutClusterYAML([]byte(editFixture), "second", c)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cfg.Cluster("gpu")
	if !ok || got.SSHHost != "hpc2" || got.S3 == nil || *got.S3 != (S3{Bucket: "other-bucket", Profile: "hpcx"}) {
		t.Errorf("gpu = %+v (s3 %+v)", got, got.S3)
	}
	if _, ok := cfg.Cluster("second"); ok {
		t.Error("the old name is still there")
	}
	if !strings.Contains(string(out), "bucket: other-bucket # staging") {
		t.Errorf("the bucket comment was lost:\n%s", out)
	}
	// Remove the s3 block, then append a new cluster.
	c.S3 = nil
	out, err = PutClusterYAML(out, "gpu", c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "s3:") {
		t.Errorf("the s3 block is still there:\n%s", out)
	}
	out, err = PutClusterYAML(out, "", Cluster{Name: "new", Type: TypeSlurm, SSHHost: "x"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 3 || cfg.Clusters[2].Name != "new" {
		t.Errorf("clusters = %+v", cfg.Clusters)
	}
}

func TestPutClusterYAMLErrors(t *testing.T) {
	if _, err := PutClusterYAML([]byte(editFixture), "nope", Cluster{Name: "nope", Type: TypeSlurm, SSHHost: "x"}); err == nil {
		t.Error("an unknown cluster was accepted")
	}
	// Invalid result: Parse rejects it.
	_, err := PutClusterYAML([]byte(editFixture), "hpcone", Cluster{Name: "hpcone", Type: TypeSlurm})
	var fe FieldError
	if !errors.As(err, &fe) || fe.Field != "ssh_host" {
		t.Errorf("err = %v, want the ssh_host field error", err)
	}
	if _, err := PutClusterYAML([]byte("- a\n- b\n"), "", Cluster{Name: "x", Type: TypeSlurm, SSHHost: "x"}); err == nil {
		t.Error("a document that isn't a mapping was accepted")
	}
}

func TestPutClusterYAMLEmptyDocument(t *testing.T) {
	out, err := PutClusterYAML(nil, "", Cluster{Name: "a", Type: TypeSlurm, SSHHost: "h"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(out)
	if err != nil || len(cfg.Clusters) != 1 {
		t.Fatalf("cfg=%+v err=%v\n%s", cfg, err, out)
	}
}

func TestRemoveClusterAndSetSite(t *testing.T) {
	out, err := RemoveClusterYAML([]byte(editFixture), "hpcone")
	if err != nil {
		t.Fatal(err)
	}
	out, err = SetSiteYAML(out, "generic")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site != "generic" || len(cfg.Clusters) != 1 || cfg.Clusters[0].Name != "second" {
		t.Errorf("cfg = %+v", cfg)
	}
	if !strings.Contains(string(out), "# Labmill clusters (my notes stay)") {
		t.Errorf("the top comment was lost:\n%s", out)
	}
	if _, err := RemoveClusterYAML(out, "hpcone"); err == nil {
		t.Error("removing a missing cluster worked")
	}
	if _, err := SetSiteYAML(out, "Not A Domain"); err == nil {
		t.Error("an invalid site was accepted")
	}
	out, err = SetSiteYAML(out, "")
	if err != nil || strings.Contains(string(out), "site:") {
		t.Errorf("site not removed (err=%v):\n%s", err, out)
	}
}

func TestFieldErrors(t *testing.T) {
	cfg := &Config{Site: "BAD", Clusters: []Cluster{
		{Name: "a", Type: TypeSlurm, S3: &S3{Bucket: "B"}},
		{Type: "nope"},
	}}
	got := map[string]bool{}
	for _, fe := range cfg.FieldErrors() {
		got[fe.Field+"@"+string(rune('0'+fe.Cluster+1))] = true
	}
	for _, want := range []string{"site@0", "ssh_host@1", "s3.bucket@1", "name@2", "type@2"} {
		if !got[want] {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `cluster "a": slurm cluster needs ssh_host`) || !strings.Contains(err.Error(), "cluster 2: missing name") {
		t.Errorf("Validate = %v", err)
	}
}

func TestBackupAndWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clusters.yaml")
	if b, err := Backup(path, time.Now()); err != nil || b != "" {
		t.Fatalf("backup of a missing file = %q, %v", b, err)
	}
	if err := WriteFile(path, []byte(editFixture)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	b1, err := Backup(path, now)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Backup(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if b1 == b2 || !strings.HasSuffix(b1, ".labmill-backup-20261004-120000") || !strings.HasSuffix(b2, "-1") {
		t.Errorf("backups %q %q", b1, b2)
	}
	data, err := os.ReadFile(b1)
	if err != nil || string(data) != editFixture {
		t.Errorf("backup content %q, %v", data, err)
	}
	if fi, err := os.Stat(path); err != nil || (fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/') {
		t.Errorf("mode %v, %v", fi.Mode(), err)
	}
}
