package sshx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"labmill-client/internal/backend"
	"labmill-client/internal/sshx/sshtest"
)

func TestExecCommandAndSubsystem(t *testing.T) {
	root := t.TempDir()
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", SFTPRoot: root, Commands: map[string]sshtest.Handler{
		"echo": func(args string, stdin io.Reader, stdout, stderr io.Writer) int {
			b, _ := io.ReadAll(stdin)
			io.WriteString(stdout, "args="+args+" in="+string(b))
			io.WriteString(stderr, "err")
			return 3
		},
	}}, time.Minute)
	// log in once so a live upstream exists
	c, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	opts := ExecOptions{Connect: ConnectOptions{SocketPath: sock, Spawn: noSpawn}}

	var out, errb bytes.Buffer
	code, err := Exec(context.Background(), "hpcx", []string{"echo", "a", "b"}, strings.NewReader("hello"), &out, &errb, opts)
	if err != nil || code != 3 || out.String() != "args=a b in=hello" || errb.String() != "err" {
		t.Fatalf("code=%d err=%v out=%q err=%q", code, err, out.String(), errb.String())
	}

	// sftp subsystem over bridged pipes
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		code, err := Exec(context.Background(), "hpcx", []string{"-s", "sftp"}, inR, outW, io.Discard, opts)
		outW.Close()
		if err == nil && code != 0 {
			t.Errorf("sftp exit code %d", code)
		}
		done <- err
	}()
	sc, err := sftp.NewClientPipe(outR, inW)
	if err != nil {
		select {
		case e := <-done:
			t.Fatalf("sftp: %v; exec: %v", err, e)
		case <-time.After(time.Second):
		}
		t.Fatal(err)
	}
	f, err := sc.Create("viaexec.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("data"))
	f.Close()
	sc.Close() // closes the pipe write side: EOF to remote
	if err := <-done; err != nil {
		t.Fatalf("Exec sftp: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "viaexec.txt"))
	if err != nil || string(b) != "data" {
		t.Fatalf("remote file %q %v", b, err)
	}
}

func TestExecNotLoggedIn(t *testing.T) {
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw"}, time.Minute)
	start := time.Now()
	_, err := Exec(context.Background(), "hpcx", []string{"true"}, strings.NewReader(""), io.Discard, io.Discard,
		ExecOptions{Connect: ConnectOptions{SocketPath: sock, Spawn: noSpawn}})
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("took too long")
	}
	// no helper at all
	_, err = Exec(context.Background(), "hpcx", []string{"true"}, strings.NewReader(""), io.Discard, io.Discard,
		ExecOptions{Connect: ConnectOptions{SocketPath: filepath.Join(t.TempDir(), "x.sock"), Spawn: noSpawn, Wait: 200 * time.Millisecond}})
	if err == nil {
		t.Fatal("expected error")
	}
}

type spyReader struct {
	r     io.Reader
	reads atomic.Int32
}

func (s *spyReader) Read(p []byte) (int, error) {
	s.reads.Add(1)
	return s.r.Read(p)
}

// After the remote command ends, the stdin copier must stop: it may finish
// the Read it is blocked in, but it must not read (or write) again.
func TestExecStdinCopierStopsWhenSessionEnds(t *testing.T) {
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", Commands: map[string]sshtest.Handler{
		"quick": func(args string, stdin io.Reader, stdout, stderr io.Writer) int {
			// Not instant: a handler that returns at once can close the
			// channel before the client has read the exec reply (EOF).
			time.Sleep(100 * time.Millisecond)
			return 0
		},
	}}, time.Minute)
	c, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	pr, pw := io.Pipe()
	defer pw.Close()
	spy := &spyReader{r: pr}
	code, err := Exec(context.Background(), "hpcx", []string{"quick"}, spy, io.Discard, io.Discard,
		ExecOptions{Connect: ConnectOptions{SocketPath: sock, Spawn: noSpawn}})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	before := spy.reads.Load()
	if before != 1 {
		t.Fatalf("reads before = %d, want the one blocked Read", before)
	}
	pw.Write([]byte("late data")) // releases the blocked Read
	time.Sleep(200 * time.Millisecond)
	if after := spy.reads.Load(); after != before {
		t.Errorf("stdin still being read after the session ended: %d -> %d", before, after)
	}
}

// A host whose sshd refuses the sftp subsystem, or accepts it and closes
// the channel without output (seen on one site), still serves SFTP when sftp-server is
// run as a command: Exec replays the client's first bytes to it.
func TestExecSFTPFallsBackToServerCommand(t *testing.T) {
	for _, tc := range []struct {
		name        string
		acceptClose int
	}{{"refused", 0}, {"accept-then-close", 5}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", SFTPAcceptThenClose: tc.acceptClose, Commands: map[string]sshtest.Handler{
				"sh": func(args string, stdin io.Reader, stdout, stderr io.Writer) int {
					buf := make([]byte, 5)
					if _, err := io.ReadFull(stdin, buf); err != nil {
						return 1
					}
					io.WriteString(stdout, "hi:"+string(buf))
					rest, _ := io.ReadAll(stdin)
					io.WriteString(stdout, "|"+string(rest))
					return 0
				},
			}}, time.Minute)
			c, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			opts := ExecOptions{Connect: ConnectOptions{SocketPath: sock, Spawn: noSpawn}}
			inR, inW := io.Pipe()
			var out bytes.Buffer
			done := make(chan error, 1)
			go func() {
				code, err := Exec(context.Background(), "hpcx", []string{"-s", "sftp"}, inR, &out, io.Discard, opts)
				if err == nil && code != 0 {
					err = fmt.Errorf("exit code %d", code)
				}
				done <- err
			}()
			io.WriteString(inW, "hello")       // consumed by the first attempt, so only the replay can deliver it
			time.Sleep(200 * time.Millisecond) // later bytes must reach the second attempt
			io.WriteString(inW, "world")
			inW.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if out.String() != "hi:hello|world" {
				t.Fatalf("got %q", out.String())
			}
			execs := srv.Execs()
			if len(execs) != 1 || execs[0] != sftpServerCommand {
				t.Fatalf("execs = %q, want exactly %q", execs, sftpServerCommand)
			}
		})
	}
}

func TestSFTPServerCommandQuoting(t *testing.T) {
	words, err := backend.SplitWords(sftpServerCommand)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 3 || words[0] != "sh" || words[1] != "-c" || !strings.Contains(words[2], "exit 127") || !strings.Contains(words[2], `exec "$p"`) {
		t.Fatalf("words = %q", words)
	}
}

func TestChunkReaderReplay(t *testing.T) {
	chunks := make(chan []byte, 4)
	chunks <- []byte("abcdef")
	r := &chunkReader{chunks: chunks, stop: make(chan struct{}), record: true}
	buf := make([]byte, 2)
	if n, _ := r.Read(buf); n != 2 {
		t.Fatal(n)
	}
	close(r.stop)
	got, err := r.replayBytes() // delivered "ab" plus the undelivered "cdef"
	if err != nil || string(got) != "abcdef" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestChunkReaderReplayOverflow(t *testing.T) {
	chunks := make(chan []byte, 2)
	chunks <- bytes.Repeat([]byte("x"), maxReplay)
	chunks <- []byte("y")
	r := &chunkReader{chunks: chunks, stop: make(chan struct{}), record: true}
	buf := make([]byte, 64*1024)
	for i := 0; i < maxReplay/len(buf); i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.replayBytes(); err != nil {
		t.Fatalf("exactly maxReplay must still replay: %v", err)
	}
	if _, err := r.Read(buf); err != nil { // one more byte tips it over
		t.Fatal(err)
	}
	if _, err := r.replayBytes(); err == nil {
		t.Fatal("overflowed recording must not replay")
	}
}
