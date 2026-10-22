package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const valid = `
clusters:
  - name: hpcone
    type: slurm
    ssh_host: hpclogin
  - name: second
    type: slurm
    ssh_host: hpclogin
    transfer: s3
    s3: {bucket: my-bucket}
    files_host: hpcfiles
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(c.Clusters))
	}
	ww, ok := c.Cluster("hpcone")
	if !ok || ww.Type != TypeSlurm || ww.SSHHost != "hpclogin" {
		t.Fatalf("hpcone = %+v, %v", ww, ok)
	}
	if got := ww.EffectiveTransfer(); got != TransferSFTP {
		t.Errorf("default transfer = %q, want sftp", got)
	}
	ra, _ := c.Cluster("second")
	if ra.EffectiveTransfer() != TransferS3 || ra.FilesHost != "hpcfiles" {
		t.Errorf("second = %+v", ra)
	}
	if _, ok := c.Cluster("nope"); ok {
		t.Error("Cluster(nope) found something")
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, yaml string
		wantSubstr []string
	}{
		{"unknown field", "clusters:\n  - name: a\n    type: slurm\n    ssh_host: h\n    hots: x\n", []string{"hots"}},
		{"missing name", "clusters:\n  - type: slurm\n    ssh_host: h\n", []string{"name"}},
		{"duplicate", "clusters:\n  - {name: a, type: slurm, ssh_host: h}\n  - {name: a, type: slurm, ssh_host: h}\n", []string{"duplicate", `"a"`}},
		{"bad type", "clusters:\n  - {name: a, type: kubernetes}\n", []string{"kubernetes"}},
		{"slurm no host", "clusters:\n  - {name: a, type: slurm}\n", []string{"ssh_host"}},
		{"bad transfer", "clusters:\n  - {name: a, type: slurm, ssh_host: h, transfer: rsync}\n", []string{"rsync"}},
		{"bad timezone", "clusters:\n  - {name: a, type: slurm, ssh_host: h, timezone: Mars/Olympus}\n", []string{"timezone", "Mars/Olympus"}},
		{"local timezone", "clusters:\n  - {name: a, type: slurm, ssh_host: h, timezone: Local}\n", []string{"timezone", "Local"}},
		{"slurm default project", "clusters:\n  - {name: a, type: slurm, ssh_host: h, default_project: p}\n", []string{"default_project"}},
		{"reports all", "clusters:\n  - {name: a, type: slurm}\n  - {name: r, type: nope}\n", []string{"ssh_host", "nope"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			for _, s := range tt.wantSubstr {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not mention %q", err, s)
				}
			}
		})
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope", "clusters.yaml"))
	if !errors.Is(err, ErrNoConfig) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load missing = %v, want ErrNoConfig and fs.ErrNotExist", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sub", "clusters.yaml")
	if err := Save(path, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Clusters) != 2 || !reflect.DeepEqual(got.Clusters[1], c.Clusters[1]) {
		t.Fatalf("round trip mismatch: %+v", got.Clusters)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
		}
		di, _ := os.Stat(filepath.Dir(path))
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v, want 0700", di.Mode().Perm())
		}
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	if err := Save(path, &Config{Clusters: []Cluster{{Name: "a", Type: TypeSlurm}}}); err == nil {
		t.Fatal("Save accepted an invalid config")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("invalid config was written anyway")
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	p, err := DefaultPath()
	if err != nil || p != filepath.Join("/xdg", "labmill", "clusters.yaml") {
		t.Fatalf("with XDG: %q, %v", p, err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	home, _ := os.UserHomeDir()
	p, err = DefaultPath()
	if err != nil || p != filepath.Join(home, ".config", "labmill", "clusters.yaml") {
		t.Fatalf("without XDG: %q, %v", p, err)
	}
}

func TestValidateRejectsBadAliases(t *testing.T) {
	for _, a := range []string{"a b", "*", "a,b", "a\nb", "x#y"} {
		c := &Config{Clusters: []Cluster{{Name: "n", Type: TypeSlurm, SSHHost: a}}}
		if err := c.Validate(); err == nil {
			t.Errorf("ssh_host %q accepted", a)
		}
		c = &Config{Clusters: []Cluster{{Name: "n", Type: TypeSlurm, SSHHost: "ok", FilesHost: a}}}
		if err := c.Validate(); err == nil {
			t.Errorf("files_host %q accepted", a)
		}
	}
	c := &Config{Clusters: []Cluster{{Name: "n", Type: TypeSlurm, SSHHost: "wild-wood.1", FilesHost: "files"}}}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
}

func TestSaveThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "link.yaml")
	if err := os.WriteFile(target, []byte("clusters: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	c := &Config{Clusters: []Cluster{{Name: "n", Type: TypeSlurm, SSHHost: "h"}}}
	if err := Save(link, c); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink replaced")
	}
	if _, err := Load(target); err != nil {
		t.Errorf("target not updated: %v", err)
	}
	os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "dangling.yaml"))
	if err := Save(filepath.Join(dir, "dangling.yaml"), c); err == nil {
		t.Error("dangling symlink accepted")
	}
}

func TestClusterLocation(t *testing.T) {
	loc, err := Cluster{}.Location()
	if err != nil || loc != time.UTC {
		t.Errorf("default = %v, %v; want UTC", loc, err)
	}
	c, err := Parse([]byte("clusters:\n  - {name: a, type: slurm, ssh_host: h, timezone: America/New_York}\n"))
	if err != nil {
		t.Fatal(err)
	}
	loc, err = c.Clusters[0].Location()
	if err != nil || loc.String() != "America/New_York" {
		t.Errorf("Location = %v, %v", loc, err)
	}
	// Round trip keeps the field.
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.Clusters[0].Timezone != "America/New_York" {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}

func TestS3Config(t *testing.T) {
	head := "clusters:\n  - {name: a, type: slurm, ssh_host: h%s}\n"
	good := []string{
		`, transfer: s3, s3: {bucket: my-bucket, prefix: labmill/x, region: us-west-2, profile: hpcx, endpoint: "https://s3.example.org"}`,
		`, transfer: s3, s3: {bucket: my.bucket-1, endpoint: "http://localhost:9000"}`,
		`, transfer: s3, s3: {bucket: abc, endpoint: "http://127.0.0.1:9000"}`,
		`, transfer: s3, s3: {bucket: abc, endpoint: "http://[::1]:9000"}`,
		`, s3: {bucket: abc}`,
		`, transfer: s3, s3: {bucket: abc, sse: AES256}`,
		`, transfer: s3, s3: {bucket: abc, sse: "aws:kms"}`,
		`, transfer: s3, s3: {bucket: abc, sse: "aws:kms", kms_key_id: "arn:aws:kms:us-west-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"}`,
		`, transfer: s3, s3: {bucket: abc, sse: "aws:kms", kms_key_id: alias/lab}`,
		`, transfer: sftp, s3: {bucket: abc}`,
	}
	for _, g := range good {
		if _, err := Parse([]byte(fmt.Sprintf(head, g))); err != nil {
			t.Errorf("%s: %v", g, err)
		}
	}
	bad := []struct{ y, want string }{
		{`, transfer: s3`, "no s3 block"},
		{`, transfer: sftp, s3: {bucket: Abc}`, "valid bucket"},
		{`, s3: {prefix: x}`, "s3.bucket is required"},
		{`, transfer: s3, s3: {bucket: abc, endpoint: "https://:443"}`, "https"},
		{`, transfer: s3, s3: {bucket: abc, endpoint: "https://example.org/?a=b"}`, "query string"},
		{`, transfer: s3, s3: {bucket: abc, endpoint: "https://example.org/#x"}`, "query string"},
		{`, transfer: s3, s3: {prefix: x}`, "s3.bucket is required"},
		{`, transfer: s3, s3: {bucket: ab}`, "valid bucket"},
		{`, transfer: s3, s3: {bucket: Abc}`, "valid bucket"},
		{`, transfer: s3, s3: {bucket: -abc}`, "valid bucket"},
		{`, transfer: s3, s3: {bucket: abc-}`, "valid bucket"},
		{`, transfer: s3, s3: {bucket: a..bc}`, "valid bucket"},
		{`, transfer: s3, s3: {bucket: 192.168.0.1}`, "valid bucket"},
		{`, transfer: s3, s3: {bucket: abc, prefix: /x}`, "s3.prefix"},
		{`, transfer: s3, s3: {bucket: abc, prefix: a/../b}`, "s3.prefix"},
		{`, transfer: s3, s3: {bucket: abc, region: "US West"}`, "s3.region"},
		{`, transfer: s3, s3: {bucket: abc, endpoint: "http://example.org"}`, "https"},
		{`, transfer: s3, s3: {bucket: abc, endpoint: "ftp://example.org"}`, "https"},
		{`, transfer: s3, s3: {bucket: abc, endpoint: "https://u:p@example.org"}`, "user name"},
		{`, transfer: s3, s3: {bucket: abc, sse: kms}`, "s3.sse"},
		{`, transfer: s3, s3: {bucket: abc, kms_key_id: alias/lab}`, "needs s3.sse"},
		{`, transfer: s3, s3: {bucket: abc, sse: AES256, kms_key_id: alias/lab}`, "needs s3.sse"},
		{`, transfer: s3, s3: {bucket: abc, sse: "aws:kms", kms_key_id: "my key"}`, "s3.kms_key_id"},
	}
	for _, b := range bad {
		_, err := Parse([]byte(fmt.Sprintf(head, b.y)))
		if err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%s: err = %v, want %q", b.y, err, b.want)
		}
	}
}

func TestS3UnknownKeyTypo(t *testing.T) {
	y := "clusters:\n  - {name: a, type: slurm, ssh_host: h, transfer: s3, s3: {bucket: abc, regoin: x}}\n"
	_, err := Parse([]byte(y))
	if err == nil {
		t.Fatal("want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown setting `regoin`") || !strings.Contains(msg, "allowed: bucket, prefix, region, profile, endpoint, sse, kms_key_id") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(msg, "credentials") || strings.Contains(msg, "config.S3") {
		t.Errorf("misleading or Go-internal text: %v", err)
	}
}

func TestS3RefusesCredentials(t *testing.T) {
	for _, key := range []string{"access_key_id", "secret_access_key", "access_key", "session_token"} {
		y := "clusters:\n  - {name: a, type: slurm, ssh_host: h, transfer: s3, s3: {bucket: abc, " + key + ": x}}\n"
		_, err := Parse([]byte(y))
		if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "credentials don't belong in clusters.yaml") || strings.Contains(err.Error(), "config.S3") {
			t.Errorf("%s: err = %v", key, err)
		}
	}
}

func TestS3String(t *testing.T) {
	if got := (S3{Bucket: "b"}).String(); got != "s3://b" {
		t.Errorf("got %q", got)
	}
	if got := (S3{Bucket: "b", Prefix: "p/q"}).String(); got != "s3://b/p/q" {
		t.Errorf("got %q", got)
	}
	if got := (S3{Bucket: "b", Prefix: "p/"}).String(); got != "s3://b/p" {
		t.Errorf("got %q", got)
	}
}

// The README's example must stay a valid clusters.yaml.
func TestREADMEExampleParses(t *testing.T) {
	data, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```yaml\\n(clusters:.*?)```").FindSubmatch(data)
	if m == nil {
		t.Fatal("no clusters yaml block in README.md")
	}
	if _, err := Parse(m[1]); err != nil {
		t.Errorf("README example: %v", err)
	}
}
