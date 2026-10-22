package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/backend"
)

// The SDK's credential_process error holds the process output (the keys); it
// must not reach the model.
func TestS3CredentialsProcessErrorHasNoSecret(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	awsTestFiles(t)
	d := t.TempDir()
	script := filepath.Join(d, "p.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho '{\"Version\":1,\"AccessKeyId\":\""+testKeyID+"\",\"SecretAccessKey\":\""+testSecret+"\",\"Expiration\":\"not-a-time\"}'\n"), 0o700)
	conf := os.Getenv("AWS_CONFIG_FILE")
	f, _ := os.OpenFile(conf, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("[profile bad]\ncredential_process = " + script + "\n")
	f.Close()
	h := remoteHarness(t, &stdinRunner{})
	_, err := h.run(t, "s3_credentials_to_cluster", map[string]any{"cluster": "hpcone", "profile": "bad"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), testKeyID) {
		t.Errorf("secret in error: %v", err)
	}
}

func TestAWSRemoteScriptIsOneLine(t *testing.T) {
	if strings.ContainsAny(awsRemoteScript, "\n!") {
		t.Errorf("script has a newline or '!': %q", awsRemoteScript)
	}
}

// A symlinked credentials file stays a link; its target is updated.
func TestAWSRemoteScriptSymlink(t *testing.T) {
	for _, tool := range []string{"sh", "awk", "mktemp", "readlink"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".aws"), 0o755)
	target := filepath.Join(t.TempDir(), "real")
	os.WriteFile(target, []byte("[other]\nk = 2\n"), 0o600)
	link := filepath.Join(home, ".aws", "credentials")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks")
	}
	cmd := exec.Command("sh", "-c", backend.Quote(awsRemoteArgv("lab")))
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader("[labmill-lab]\nnew = 1\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link replaced by a file")
	}
	if b, _ := os.ReadFile(target); string(b) != "[other]\nk = 2\n[labmill-lab]\nnew = 1\n" {
		t.Errorf("target: %q", b)
	}
}
