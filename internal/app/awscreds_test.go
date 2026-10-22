package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
)

const (
	testKeyID  = "AKIAEXAMPLEKEYID123"
	testSecret = "wJalrEXAMPLEsecretVALUE/abc123"
)

// awsTestFiles writes a config and credentials file and points the SDK at them.
func awsTestFiles(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	conf := filepath.Join(d, "config")
	cred := filepath.Join(d, "credentials")
	os.WriteFile(conf, []byte("[default]\nregion = us-west-2\n[profile work]\nsso_session = s\nsso_account_id = 1\n[profile adm]\nrole_arn = arn:aws:iam::1:role/x\nsource_profile = lab\n[profile proc]\ncredential_process = echo\n[sso-session s]\nsso_start_url = https://x\n[profile bad name]\nregion = x\n"), 0o600)
	os.WriteFile(cred, []byte("[lab]\naws_access_key_id = "+testKeyID+"\naws_secret_access_key = "+testSecret+"\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", conf)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", cred)
	t.Setenv("AWS_PROFILE", "")
}

func TestListAWSProfilesKinds(t *testing.T) {
	awsTestFiles(t)
	got := map[string]string{}
	for _, p := range listAWSProfiles() {
		got[p.Name] = p.Kind
	}
	want := map[string]string{"default": "other", "work": "sso", "adm": "role", "proc": "process", "lab": "static"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
}

type stdinRunner struct {
	got []string
	in  string
}

func (s *stdinRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (backend.Result, error) {
	b, _ := io.ReadAll(stdin)
	s.in = string(b)
	s.got = argv
	return backend.Result{}, nil
}

func TestS3CredentialsToolNoValuesAndAlwaysAsks(t *testing.T) {
	awsTestFiles(t)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAFROMENVIRONMENT") // the named profile must win
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fromenvironment")
	sr := &stdinRunner{}
	h := remoteHarness(t, sr)
	tool := h.tools["s3_credentials_to_cluster"]

	list, err := h.run(t, "s3_credentials_to_cluster", map[string]any{})
	if err != nil || !strings.Contains(list, "lab (static)") || !strings.Contains(list, "work (sso)") {
		t.Fatalf("list: %q, %v", list, err)
	}
	in := json.RawMessage(`{"cluster":"hpcone","profile":"lab"}`)
	p, err := tool.Propose(in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Secret {
		t.Error("proposal must be Secret so automatic approval never covers it")
	}
	asked := 0
	auto := &agent.AutoApprover{Next: approverFunc(func(agent.Proposal) agent.Decision { asked++; return agent.Decision{} }), On: func() bool { return true }}
	if d, _ := auto.Approve(context.Background(), p); d.Approved || asked != 1 {
		t.Errorf("auto-approve covered it: %+v asked=%d", d, asked)
	}
	for _, s := range []string{p.Summary, p.Details} {
		if strings.Contains(s, testKeyID) || strings.Contains(s, testSecret) {
			t.Errorf("value in proposal: %q", s)
		}
	}
	if !strings.Contains(p.Details, "[labmill-lab]") || !strings.Contains(p.Details, "static") {
		t.Errorf("details: %q", p.Details)
	}
	res, err := tool.Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res, testKeyID) || strings.Contains(res, testSecret) || !strings.Contains(res, "AWS_PROFILE=labmill-lab") {
		t.Errorf("result: %q", res)
	}
	if a := strings.Join(sr.got, " "); strings.Contains(a, testSecret) || strings.Contains(a, testKeyID) {
		t.Errorf("value in argv: %v", sr.got)
	}
	if !strings.Contains(sr.in, "aws_access_key_id = "+testKeyID) || !strings.Contains(sr.in, "[labmill-lab]") {
		t.Errorf("stdin: %q", sr.in)
	}
	if _, err := tool.Run(context.Background(), in, agent.Decision{}); err == nil {
		t.Error("ran without approval")
	}
}

func TestS3CredentialsToolRefusals(t *testing.T) {
	awsTestFiles(t)
	h := remoteHarness(t, &stdinRunner{})
	if _, err := h.run(t, "s3_credentials_to_cluster", map[string]any{"cluster": "hpcone", "profile": "../x"}); err == nil {
		t.Error("bad name accepted")
	}
	if _, err := h.run(t, "s3_credentials_to_cluster", map[string]any{"cluster": "hpcone", "profile": "nope"}); err == nil {
		t.Error("unknown profile accepted")
	}
	if out, _ := h.run(t, "s3_credentials_to_cluster", map[string]any{"cluster": "second", "profile": "lab"}); strings.Contains(out, "can only be copied") {
		t.Errorf("second: %q", out)
	}
	h.env.Site.NoS3Credentials = true
	if out, _ := h.run(t, "s3_credentials_to_cluster", map[string]any{"cluster": "hpcone", "profile": "lab"}); !strings.Contains(out, "turns off") {
		t.Errorf("site switch: %q", out)
	}
}

// The remote script, run by a real sh against a temp HOME: it replaces only
// its own section, keeps the rest, and sets the modes.
func TestAWSRemoteScript(t *testing.T) {
	for _, tool := range []string{"sh", "awk", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not available")
		}
	}
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".aws"), 0o755)
	cred := filepath.Join(home, ".aws", "credentials")
	os.WriteFile(cred, []byte("[default]\nk = 1\n[labmill-lab]\nold = 1\n[other]\nk = 2\n"), 0o644)
	run := func(sec string) {
		cmd := exec.Command("sh", "-c", backend.Quote(awsRemoteArgv("lab")))
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		cmd.Stdin = bytes.NewReader([]byte(sec))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("[labmill-lab]\n# expires 2030-01-01T00:00:00Z\nnew = 2\n")
	run("[labmill-lab]\nnew = 3\n")
	b, _ := os.ReadFile(cred)
	want := "[default]\nk = 1\n[other]\nk = 2\n[labmill-lab]\nnew = 3\n"
	if string(b) != want {
		t.Errorf("got %q want %q", b, want)
	}
	for p, mode := range map[string]os.FileMode{cred: 0o600, filepath.Dir(cred): 0o700} {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != mode {
			t.Errorf("%s mode %v want %v", p, fi.Mode().Perm(), mode)
		}
	}
	if ents, _ := os.ReadDir(filepath.Dir(cred)); len(ents) != 1 {
		t.Errorf("temp files left: %v", ents)
	}
}
