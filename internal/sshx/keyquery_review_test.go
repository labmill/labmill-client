package sshx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/sshx/sshtest"
)

func TestQueryKeysNoKeysNoConnection(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	// Nothing listens on port 1: any dial would fail.
	if err := sshconfig.WriteHosts(p, []sshconfig.Host{{Alias: "orca", HostName: "127.0.0.1", User: "alice", Port: 1}}, "linux"); err != nil {
		t.Fatal(err)
	}
	pr := &scriptPrompter{}
	got, err := QueryKeys(context.Background(), p, "orca", pr, LoginOptions{}, nil)
	if err != nil || len(got) != 0 || len(pr.asked) != 0 {
		t.Fatalf("got %v, %v, asked %v", got, err, pr.asked)
	}
}

func TestListRemoteKeysControlCharName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX sh")
	}
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A newline in the name would otherwise let the line after it pose as
	// another file's entry.
	evil := "x\nid_real"
	writeFile(t, filepath.Join(sshDir, evil), "PRIVATE")
	writeFile(t, filepath.Join(sshDir, evil+".pub"), keyLine(edPub(t), "evil"))
	writeFile(t, filepath.Join(sshDir, "id_real"), "PRIVATE")
	writeFile(t, filepath.Join(sshDir, "id_real.pub"), keyLine(edPub(t), "real"))
	c, _ := execClient(t, map[string]sshtest.Handler{"sh": shHandler(t, home)})
	keys, err := ListRemoteKeys(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != "id_real" || keys[0].Comment != "real" {
		t.Errorf("keys = %+v", keys)
	}
	// The parser rejects such names too, whatever the script printed.
	if got := parseRemoteKeys("a\x01b\t" + keyLine(edPub(t), "c")); len(got) != 0 {
		t.Errorf("parser kept %+v", got)
	}
}

func TestLocalKeysNeedPrivateFile(t *testing.T) {
	p := sshconfig.Paths{SSHDir: filepath.Join(t.TempDir(), ".ssh")}
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		t.Fatal(err)
	}
	useAgent(t, agent.NewKeyring())
	writeFile(t, filepath.Join(p.SSHDir, "id_ed25519.pub"), keyLine(edPub(t), "stray"))
	got, err := LocalKeys(context.Background(), p, "orca")
	if err != nil || len(got) != 0 {
		t.Fatalf("a .pub without its private key was listed: %v, %v", got, err)
	}
}

// TestQuerySignerRSAAlgorithms: ssh-rsa is offered last, so a server without
// server-sig-algs can still be queried.
func TestQuerySignerRSAAlgorithms(t *testing.T) {
	got := querySigner{pub: rsaPub(t)}.Algorithms()
	if len(got) != 3 || got[2] != ssh.KeyAlgoRSA {
		t.Errorf("algorithms = %v", got)
	}
}
