package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/config"
)

// M43.2: Build wires the auto approver around the UI, reading gui.json at
// every proposal; the log gets a redacted line; secret proposals still ask.
func TestBuildAutoApprover(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("LABMILL_BROKER_URL", "")
	useFixtureSite(t)
	cfgPath, _ := config.DefaultPath()
	if err := config.Save(cfgPath, &config.Config{Site: fixtureDomain, Clusters: fixtureSite(t).Clusters}); err != nil {
		t.Fatal(err)
	}
	prefs := filepath.Join(filepath.Dir(cfgPath), "gui.json")
	if err := os.WriteFile(prefs, []byte(`{"approveAll": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, nil) // fakeUI.Approve always fails
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ap := rt.Agent.Approver
	d, err := ap.Approve(context.Background(), agent.Proposal{Tool: "remote_command", Summary: "Run on c: curl --token abc123secret https://x"})
	if err != nil || !d.Approved || !d.Auto {
		t.Fatalf("on: %+v, %v", d, err)
	}
	if _, err := ap.Approve(context.Background(), agent.Proposal{Tool: "remote_command", Summary: "Run: cat ~/.ssh/id_rsa", Secret: true}); err == nil {
		t.Error("a secret read was approved automatically")
	}
	log, _ := os.ReadFile(rt.LogPath)
	if !strings.Contains(string(log), "auto-approved") || !strings.Contains(string(log), "remote_command") || strings.Contains(string(log), "abc123secret") {
		t.Errorf("log = %q", log)
	}
	// Read at every proposal: a change applies at once.
	if err := os.WriteFile(prefs, []byte(`{"approveAll": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Approve(context.Background(), agent.Proposal{Tool: "upload_data"}); err == nil {
		t.Error("still approved automatically after the setting went off")
	}
}
