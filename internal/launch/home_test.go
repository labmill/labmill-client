package launch

import (
	"context"
	"io"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshx/sshtest"
)

func dialTest(t *testing.T, srv *sshtest.Server) *ssh.Client {
	t.Helper()
	cl, err := ssh.Dial("tcp", srv.Addr, &ssh.ClientConfig{
		User:            "alice",
		Auth:            []ssh.AuthMethod{ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) { return []string{"pw"}[:len(qs)], nil })},
		HostKeyCallback: ssh.FixedHostKey(srv.HostKey),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

// A transfer host with sftp reports its home through SFTP, even
// if it refuses commands.
func TestHomeOverSFTP(t *testing.T) {
	root := t.TempDir()
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", SFTPRoot: root})
	got, err := homeOver(context.Background(), dialTest(t, srv))
	if err != nil || got == "" || got[0] != '/' {
		t.Fatalf("home %q, err %v", got, err)
	}
	if len(srv.Execs()) != 0 {
		t.Errorf("ran commands although sftp answered: %v", srv.Execs())
	}
}

// A login host without the sftp subsystem (login.hpc) falls back to
// printenv HOME.
func TestHomeOverPrintenvFallback(t *testing.T) {
	srv := sshtest.New(t, sshtest.Options{User: "alice", Password: "pw", Commands: map[string]sshtest.Handler{
		"printenv": func(args string, _ io.Reader, stdout, _ io.Writer) int {
			io.WriteString(stdout, "/home/pi/alice\n")
			return 0
		},
	}})
	if got, err := homeOver(context.Background(), dialTest(t, srv)); err != nil || got != "/home/pi/alice" {
		t.Fatalf("home %q, err %v", got, err)
	}
}
