package launch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/agent/agenttest"
	"labmill-client/internal/app"
	"labmill-client/internal/config"
	"labmill-client/internal/site"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx"
	"labmill-client/internal/transfer"
)

func TestWriteSetup(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	ex := fixtureSite(t)
	// The user already has an alias for the login host: reuse it.
	os.MkdirAll(p.SSHDir, 0o700)
	os.WriteFile(p.UserConfig(), []byte("Host myjump\n    HostName login.hpc.example.edu\n    User bob\n    ControlPath ~/.ssh/my-cp-%C\n"), 0o600)
	cfgPath := filepath.Join(t.TempDir(), "labmill", "clusters.yaml")
	cfg, err := WriteSetup(p, cfgPath, ex, SetupAnswers{Username: "alice"}, "linux")
	if err != nil {
		t.Fatalf("WriteSetup: %v", err)
	}
	bi, ok := cfg.Cluster("bigiron")
	if !ok || bi.SSHHost != "myjump" {
		t.Errorf("bigiron = %+v (should reuse the existing myjump alias)", bi)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil || len(loaded.Clusters) != len(ex.Clusters) {
		t.Fatalf("saved config: %v %+v", err, loaded)
	}
	b, _ := os.ReadFile(p.UserConfig())
	if !strings.HasPrefix(string(b), sshconfig.IncludeLine) {
		t.Errorf("Include not added:\n%s", b)
	}
	yb, _ := os.ReadFile(p.LabmillConfig())
	if strings.Contains(string(yb), "Host myjump") {
		t.Errorf("the user's own alias was rewritten into Labmill's file:\n%s", yb)
	}
	if !strings.Contains(string(yb), "Host ex-files") {
		t.Errorf("ex-files missing from Labmill's file:\n%s", yb)
	}
	jump, found, err := sshconfig.Lookup(p, "myjump")
	if err != nil || !found || jump.User != "bob" || !strings.Contains(string(b), "ControlPath ~/.ssh/my-cp-%C") {
		t.Errorf("user's myjump settings lost: %+v %v %v", jump, found, err)
	}
	// Re-running is idempotent: Labmill's own aliases are rewritten, not duplicated.
	if _, err := WriteSetup(p, cfgPath, ex, SetupAnswers{Username: "alice"}, "linux"); err != nil {
		t.Fatal(err)
	}
	yb2, _ := os.ReadFile(p.LabmillConfig())
	if strings.Count(string(yb2), "Host ex-files") != 1 {
		t.Errorf("re-run duplicated hosts:\n%s", yb2)
	}
	files, found, err := sshconfig.Lookup(p, bi.FilesHost)
	if err != nil || !found || files.HostName != "files.example.edu" || files.User != "alice" {
		t.Errorf("files host = %+v %v %v", files, found, err)
	}
	sc, _ := cfg.Cluster("second")
	if sc.SSHHost != "myjump" {
		t.Errorf("second = %+v", sc)
	}
}

// scripted is a fake broker (agenttest) that streams the replies in order
// and fails past the last one.
func scripted(t *testing.T, replies ...string) agent.Model {
	return agenttest.New(t, replies...).Client(t)
}

func reply(text string) string {
	b, _ := json.Marshal(text)
	return `{"id":"m","type":"message","role":"assistant","model":"x","stop_reason":"end_turn","content":[{"type":"text","text":` + string(b) + `}],"usage":{"input_tokens":1,"output_tokens":1}}`
}

// fakeUI is a UI that records turn completions.
type fakeUI struct{ done chan error }

func (f *fakeUI) Approve(context.Context, agent.Proposal) (agent.Decision, error) {
	return agent.Decision{}, errors.New("no approvals in tests")
}
func (f *fakeUI) Choose(context.Context, agent.Choice) (agent.ChoiceAnswer, error) {
	return agent.ChoiceAnswer{Skipped: true}, nil
}
func (f *fakeUI) Ask(context.Context, sshx.Question) (string, error) {
	return "", errors.New("no prompts in tests")
}
func (f *fakeUI) Notify(string)            {}
func (f *fakeUI) Event(agent.Event)        {}
func (f *fakeUI) Done(err error)           { f.done <- err }
func (f *fakeUI) JobNotice(string)         {}
func (f *fakeUI) Transfer(transfer.Update) {}

func TestSessionKeepsHistoryAndSignalsDone(t *testing.T) {
	ui := &fakeUI{done: make(chan error, 4)}
	done := ui.done
	s := &Session{agent: &agent.Agent{Model: scripted(t, reply("one"), reply("two")), Tools: agent.NewRegistry(), Approver: ui, OnEvent: ui.Event}, ui: ui}
	s.Send(context.Background(), "first")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.Send(context.Background(), "second")
	<-done
	if len(s.history) != 4 {
		t.Errorf("history = %d messages, want 4", len(s.history))
	}
	// A failing turn still signals Done with the error.
	s.Send(context.Background(), "third")
	if err := <-done; err == nil {
		t.Error("expected an error from the exhausted model")
	}
}

func TestSessionWait(t *testing.T) {
	s := &Session{}
	if !s.Wait(time.Second) {
		t.Error("idle session: Wait timed out")
	}
	s.mu.Lock()
	if s.Wait(50 * time.Millisecond) {
		t.Error("busy session: Wait returned early")
	}
	s.mu.Unlock()
	if !s.Wait(time.Second) {
		t.Error("released session: Wait timed out")
	}
}

func TestAppendLogAndStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "labmill.log")
	for i := 0; i < 2; i++ {
		if err := appendLog(path, "skill warnings", []string{"a\nb", "c"}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	if strings.Count(string(b), "skill warnings") != 2 || !strings.Contains(string(b), "  a b\n") {
		t.Errorf("log = %q", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", fi.Mode())
		}
	}
	got := statusLine(&config.Config{Clusters: []config.Cluster{{Name: "big", Type: config.TypeSlurm}}})
	if got != "big (Slurm)" {
		t.Errorf("status = %q", got)
	}
}

func TestBuildWiresTransferCheck(t *testing.T) {
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
	var steps []string
	rt, err := BuildWithProgress(context.Background(), &fakeUI{done: make(chan error, 1)}, nil, func(s string) { steps = append(steps, s) })
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if rt.Env.TransferCheck == nil {
		t.Error("TransferCheck not wired")
	}
	want := []string{StartLoadingSettings, StartOpeningDatabase, StartLoadingSkills, StartReady}
	if strings.Join(steps, "|") != strings.Join(want, "|") {
		t.Errorf("startup steps = %q, want %q", steps, want)
	}
}

// A failing Build stops reporting at the step that failed.
func TestBuildProgressStopsOnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	var steps []string
	_, err := BuildWithProgress(context.Background(), &fakeUI{done: make(chan error, 1)}, nil, func(s string) { steps = append(steps, s) })
	if err == nil {
		t.Fatal("Build without configuration or setup succeeded")
	}
	if strings.Join(steps, "|") != StartLoadingSettings {
		t.Errorf("steps = %q", steps)
	}
}

// Build restores the chosen folders from workspace.json (shared by the
// terminal and the desktop app) and the agent appends them to the system
// prompt every turn.
func TestBuildRestoresWorkspace(t *testing.T) {
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
	home, _ := os.UserHomeDir()
	code := filepath.Join(home, "proj")
	if err := os.MkdirAll(code, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := app.NewWorkspace(home, app.WorkspacePath(cfgPath))
	if _, err := ws.SetFolder(app.FolderCode, code); err != nil {
		t.Fatal(err)
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if got := rt.Env.Workspace.Folder(app.FolderCode); got != code {
		t.Errorf("restored code folder = %q, want %q", got, code)
	}
	if rt.Agent.MaxTurns != DefaultMaxTurns {
		t.Errorf("session MaxTurns = %d, want %d", rt.Agent.MaxTurns, DefaultMaxTurns)
	}
	if rt.Agent.SystemExtra == nil || !strings.Contains(rt.Agent.SystemExtra(), "The user's code folder is \""+code) {
		t.Error("the agent does not carry the chosen folders")
	}
	if strings.Contains(rt.Agent.System, code) {
		t.Error("the fixed system prompt names the folder (it belongs in SystemExtra)")
	}
}

// Build loads the site the config names and its skill next to the generic
// skills; an unknown site falls back to generic with a warning.
func TestBuildSiteAndSkills(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("LABMILL_BROKER_URL", "")
	useFixtureSite(t)
	ex := fixtureSite(t)
	cfgPath, _ := config.DefaultPath()
	if err := config.Save(cfgPath, &config.Config{Site: fixtureDomain, Clusters: ex.Clusters}); err != nil {
		t.Fatal(err)
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Site.Domain != fixtureDomain {
		t.Errorf("site = %q", rt.Site.Domain)
	}
	for _, name := range []string{"slurm-user", "research-computing", ex.Skill} {
		if _, ok := rt.Env.Skills.Get(name); !ok {
			t.Errorf("skill %s not loaded", name)
		}
	}
	if !strings.Contains(rt.Agent.System, ex.Skill) || !strings.Contains(rt.Agent.System, "Example University") || !strings.Contains(rt.Agent.System, "How skill steps map") {
		t.Error("system prompt lacks the site, its skill or the tool map")
	}
	rt.Close()

	// No site named, or an unknown one: no home institution (M55); see
	// home_required_test.go.
}

func TestBuildFirstRunChoosesSite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("LABMILL_BROKER_URL", "")
	var offered string
	setup := func(_ context.Context, s site.Site) (SetupAnswers, error) {
		offered = s.Domain
		return SetupAnswers{Site: site.GenericDomain, Username: "dana", LoginHost: "hpc.example.edu"}, nil
	}
	rt, err := Build(context.Background(), &fakeUI{done: make(chan error, 1)}, setup)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if offered != site.Default().Domain {
		t.Errorf("setup offered %q first", offered)
	}
	cfgPath, _ := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil || cfg.Site != site.GenericDomain {
		t.Fatalf("saved config: %+v %v", cfg, err)
	}
	if _, ok := cfg.Cluster("hpc"); !ok || rt.Site.Domain != site.GenericDomain {
		t.Errorf("clusters = %+v, site %q", cfg.Clusters, rt.Site.Domain)
	}
}
