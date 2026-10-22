package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/config"
	"labmill-client/internal/sshconfig"
	"labmill-client/internal/store"
)

type chooserFunc func(agent.Choice) agent.ChoiceAnswer

func (f chooserFunc) Choose(_ context.Context, c agent.Choice) (agent.ChoiceAnswer, error) {
	return f(c), nil
}

// oldConfig is the fixture's setup as an older Labmill wrote it: no files_host.
func oldConfig(t *testing.T) (sshconfig.Paths, string, *config.Config) {
	t.Helper()
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	cfgPath := filepath.Join(t.TempDir(), "labmill", "clusters.yaml")
	cfg, err := WriteSetup(p, cfgPath, fixtureSite(t), SetupAnswers{Username: "alice"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Clusters {
		cfg.Clusters[i].FilesHost = ""
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	// And the transfer host's ssh entry is missing too.
	login := sshconfig.Render([]sshconfig.Host{{Alias: "ex-login", HostName: "login.hpc.example.edu", User: "alice"}}, "linux")
	if err := os.WriteFile(p.LabmillConfig(), login, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, cfgPath, cfg
}

func TestPendingFilesHosts(t *testing.T) {
	_, cfgPath, cfg := oldConfig(t)
	got := pendingFilesHosts(fixtureSite(t), cfg, readFilesHostAsked(cfgPath))
	if len(got) != 1 || got[0].Cluster != "bigiron" || got[0].Template.HostName != "files.example.edu" {
		t.Fatalf("pending = %+v", got)
	}
	if got := pendingFilesHosts(fixtureSite(t), cfg, map[string]bool{"bigiron": true}); len(got) != 0 {
		t.Errorf("asked cluster still pending: %+v", got)
	}
}

func TestOfferFilesHostYesUpdatesConfig(t *testing.T) {
	p, cfgPath, cfg := oldConfig(t)
	var q string
	ch := chooserFunc(func(c agent.Choice) agent.ChoiceAnswer { q = c.Question; return agent.ChoiceAnswer{Index: 0} })
	notes := offerFilesHosts(context.Background(), ch, p, cfgPath, fixtureSite(t), cfg, "linux")
	if !strings.Contains(q, "files.example.edu") || len(notes) != 1 || !strings.Contains(notes[0], "next time Labmill starts") {
		t.Fatalf("question %q notes %v", q, notes)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	bi, _ := loaded.Cluster("bigiron")
	if bi.FilesHost == "" {
		t.Fatalf("files_host not saved: %+v", bi)
	}
	if cur, _ := cfg.Cluster("bigiron"); cur.FilesHost != "" {
		t.Errorf("in-memory config was mutated: %+v", cur)
	}
	h, found, err := sshconfig.Lookup(p, bi.FilesHost)
	if err != nil || !found || h.HostName != "files.example.edu" || h.User != "alice" {
		t.Errorf("ssh entry = %+v %v %v", h, found, err)
	}
	if _, found, _ := sshconfig.Lookup(p, "ex-login"); !found {
		t.Error("existing Labmill ssh entry was lost")
	}
	if m, _ := filepath.Glob(cfgPath + ".labmill-backup-*"); len(m) != 1 {
		t.Errorf("clusters.yaml backups = %v, want 1", m)
	}
	// Remembered: not asked again.
	asked := false
	again := chooserFunc(func(agent.Choice) agent.ChoiceAnswer { asked = true; return agent.ChoiceAnswer{} })
	offerFilesHosts(context.Background(), again, p, cfgPath, fixtureSite(t), cfg, "linux")
	if asked {
		t.Error("asked a second time")
	}
}

func TestOfferFilesHostNoAndSkipped(t *testing.T) {
	p, cfgPath, cfg := oldConfig(t)
	skip := chooserFunc(func(agent.Choice) agent.ChoiceAnswer { return agent.ChoiceAnswer{Skipped: true} })
	offerFilesHosts(context.Background(), skip, p, cfgPath, fixtureSite(t), cfg, "linux")
	if len(pendingFilesHosts(fixtureSite(t), cfg, readFilesHostAsked(cfgPath))) != 1 {
		t.Error("a dismissed question must be asked again next start")
	}
	no := chooserFunc(func(agent.Choice) agent.ChoiceAnswer { return agent.ChoiceAnswer{Index: 1} })
	offerFilesHosts(context.Background(), no, p, cfgPath, fixtureSite(t), cfg, "linux")
	if bi, _ := cfg.Cluster("bigiron"); bi.FilesHost != "" {
		t.Errorf("declined but files_host = %q", bi.FilesHost)
	}
	if len(pendingFilesHosts(fixtureSite(t), cfg, readFilesHostAsked(cfgPath))) != 0 {
		t.Error("a declined offer must be remembered")
	}
}

func TestApplyFilesHostReusesUserAlias(t *testing.T) {
	p, cfgPath, cfg := oldConfig(t)
	os.WriteFile(p.UserConfig(), []byte("Host myfiles\n    HostName files.example.edu\n"), 0o600)
	o := pendingFilesHosts(fixtureSite(t), cfg, nil)[0]
	if err := applyFilesHost(p, cfgPath, cfg, o, "linux", time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if bi, _ := loaded.Cluster("bigiron"); bi.FilesHost != "myfiles" {
		t.Errorf("files_host = %q, want the user's alias", bi.FilesHost)
	}
}

func TestSessionRunsFirstTurnOnce(t *testing.T) {
	ui := &fakeUI{done: make(chan error, 4)}
	n := 0
	s := &Session{agent: &agent.Agent{Model: scripted(t, reply("one"), reply("two")), Tools: agent.NewRegistry(), Approver: ui, OnEvent: ui.Event}, ui: ui,
		firstTurn: func(context.Context) { n++ }}
	s.Send(context.Background(), "a")
	<-ui.done
	s.Send(context.Background(), "b")
	<-ui.done
	if n != 1 {
		t.Errorf("firstTurn ran %d times, want 1", n)
	}
}

func TestSetFilesHostYAMLKeepsCommentsAndOrder(t *testing.T) {
	in := "# my clusters\nclusters:\n  # the big one\n  - name: bigiron\n    type: slurm\n    ssh_host: ex-login # login\n    transfer: sftp\n  - name: other\n    type: slurm\n    ssh_host: ex-login\n"
	out, err := setFilesHostYAML([]byte(in), "bigiron", "ex-files")
	if err != nil {
		t.Fatal(err)
	}
	want := "# my clusters\nclusters:\n  # the big one\n  - name: bigiron\n    type: slurm\n    ssh_host: ex-login # login\n    files_host: ex-files\n    transfer: sftp\n  - name: other\n    type: slurm\n    ssh_host: ex-login\n"
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestBackupConfigNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clusters.yaml")
	now := time.Now()
	for i, content := range []string{"one", "two"} {
		os.WriteFile(path, []byte(content), 0o600)
		if err := backupConfig(path, now); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}
	m, _ := filepath.Glob(path + ".labmill-backup-*")
	if len(m) != 2 {
		t.Fatalf("backups = %v, want 2", m)
	}
	seen := map[string]bool{}
	for _, f := range m {
		b, _ := os.ReadFile(f)
		seen[string(b)] = true
	}
	if !seen["one"] || !seen["two"] {
		t.Errorf("a backup was overwritten: %v", seen)
	}
}

func TestOfferFilesHostFailedYesIsOfferedAgain(t *testing.T) {
	p, cfgPath, cfg := oldConfig(t)
	good, _ := os.ReadFile(cfgPath)
	os.WriteFile(cfgPath, []byte("clusters: [broken"), 0o600)
	yes := chooserFunc(func(agent.Choice) agent.ChoiceAnswer { return agent.ChoiceAnswer{Index: 0} })
	notes := offerFilesHosts(context.Background(), yes, p, cfgPath, fixtureSite(t), cfg, "linux")
	if len(notes) != 1 || !strings.Contains(notes[0], "couldn't add") {
		t.Fatalf("notes = %v", notes)
	}
	os.WriteFile(cfgPath, good, 0o600)
	if len(pendingFilesHosts(fixtureSite(t), cfg, readFilesHostAsked(cfgPath))) != 1 {
		t.Error("a failed Yes must be offered again")
	}
}

// offerFilesHosts must not touch cfg while the monitor reads it.
func TestOfferFilesHostsRacesWithMonitor(t *testing.T) {
	f := newMonitorFixture(t)
	f.job(t, store.Job{Name: "a", Cluster: "hpcone", Backend: "slurm", RemoteID: "9", State: store.StateRunning, Message: "ok"})
	p, cfgPath, cfg := oldConfig(t)
	f.m.cfg = cfg
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			f.m.check(context.Background())
		}
	}()
	yes := chooserFunc(func(agent.Choice) agent.ChoiceAnswer { return agent.ChoiceAnswer{Index: 0} })
	offerFilesHosts(context.Background(), yes, p, cfgPath, fixtureSite(t), cfg, "linux")
	<-done
}

func TestOfferFilesHostNamesSignIn(t *testing.T) {
	for _, tc := range []struct{ name, want, notWant string }{{"", "sign-in request", "Duo"}, {"Duo", "Duo push", ""}} {
		p, cfgPath, cfg := oldConfig(t)
		s := fixtureSite(t)
		s.TwoFactorName = tc.name
		var q string
		ch := chooserFunc(func(c agent.Choice) agent.ChoiceAnswer { q = c.Question; return agent.ChoiceAnswer{Index: 1} })
		offerFilesHosts(context.Background(), ch, p, cfgPath, s, cfg, "linux")
		if !strings.Contains(q, tc.want) || (tc.notWant != "" && strings.Contains(q, tc.notWant)) {
			t.Errorf("name %q: question %q", tc.name, q)
		}
	}
}
