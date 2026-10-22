package sshx

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"labmill-client/internal/sshx/sshtest"
)

func TestShellStartCommand(t *testing.T) {
	for _, c := range []struct{ dir, want string }{
		{"", ""},
		{"/data/proj", "cd -- /data/proj && exec $SHELL -l"},
		{"/data/my group", "cd -- '/data/my group' && exec $SHELL -l"},
		{"it's; rm -rf /", `cd -- 'it'\''s; rm -rf /' && exec $SHELL -l`},
		{"~", `cd -- "$HOME" && exec $SHELL -l`},
		{"~/my proj", `cd -- "$HOME"/'my proj' && exec $SHELL -l`},
		{"$(id)", "cd -- '$(id)' && exec $SHELL -l"},
		{"-x", "cd -- -x && exec $SHELL -l"},
	} {
		if got := ShellStartCommand(c.dir); got != c.want {
			t.Errorf("ShellStartCommand(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

// syncBuf is a goroutine-safe buffer.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunShell(t *testing.T) {
	shell := func(_ string, stdin io.Reader, stdout, _ io.Writer) int {
		io.WriteString(stdout, "welcome\n")
		sc := bufio.NewScanner(stdin)
		for sc.Scan() {
			if sc.Text() == "exit" {
				return 7
			}
			io.WriteString(stdout, "got:"+sc.Text()+"\n")
		}
		return 1
	}
	srv, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", AllowPTY: true, Shell: shell}, time.Minute)
	cl, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	inR, inW := io.Pipe()
	resize := make(chan [2]int, 1)
	var out, errb syncBuf
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := RunShell(context.Background(), cl, ShellOptions{
			Term: "screen", Width: 100, Height: 30, Resize: resize,
			Stdin: inR, Stdout: &out, Stderr: &errb,
		})
		done <- result{code, err}
	}()
	waitFor(t, "welcome", func() bool { return strings.Contains(out.String(), "welcome") })
	io.WriteString(inW, "ls\n")
	waitFor(t, "echo", func() bool { return strings.Contains(out.String(), "got:ls") })
	resize <- [2]int{120, 40}
	waitFor(t, "window-change", func() bool { return len(srv.WindowChanges()) == 1 })
	io.WriteString(inW, "exit\n")
	r := <-done
	if r.err != nil || r.code != 7 {
		t.Fatalf("code=%d err=%v", r.code, r.err)
	}
	if ptys := srv.PTYRequests(); len(ptys) != 1 || ptys[0] != (sshtest.PTYRequest{Term: "screen", Width: 100, Height: 30}) {
		t.Fatalf("pty requests = %+v", ptys)
	}
	if wc := srv.WindowChanges(); wc[0] != [2]int{120, 40} {
		t.Fatalf("window changes = %v", wc)
	}
	if srv.Shells() != 1 || len(srv.Execs()) != 0 {
		t.Fatalf("shells=%d execs=%v", srv.Shells(), srv.Execs())
	}
	if srv.Logins() != 1 {
		t.Fatalf("logins = %d, want the one sign-in", srv.Logins())
	}
}

func TestRunShellDefaultsAndDir(t *testing.T) {
	srv, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", AllowPTY: true, Commands: map[string]sshtest.Handler{
		"cd": func(string, io.Reader, io.Writer, io.Writer) int { return 5 },
	}}, time.Minute)
	cl, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	code, err := RunShell(context.Background(), cl, ShellOptions{Dir: "/data/my group", Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil || code != 5 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if execs := srv.Execs(); len(execs) != 1 || execs[0] != "cd -- '/data/my group' && exec $SHELL -l" {
		t.Fatalf("execs = %q", execs)
	}
	// TERM unset and size unknown: the defaults.
	if ptys := srv.PTYRequests(); len(ptys) != 1 || ptys[0] != (sshtest.PTYRequest{Term: DefaultTerm, Width: 80, Height: 24}) {
		t.Fatalf("pty requests = %+v", ptys)
	}
}

func TestRunShellRefusedPTY(t *testing.T) {
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw"}, time.Minute)
	cl, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	if _, err := RunShell(context.Background(), cl, ShellOptions{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v", err)
	}
}

// An open shell keeps the helper (and the login) alive past its idle timeout.
func TestRunShellKeepsHelperUp(t *testing.T) {
	release := make(chan struct{})
	shell := func(_ string, stdin io.Reader, stdout, _ io.Writer) int {
		io.WriteString(stdout, "up\n")
		<-release
		return 0
	}
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", AllowPTY: true, Shell: shell}, 300*time.Millisecond)
	cl, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var out syncBuf
	done := make(chan error, 1)
	go func() {
		_, err := RunShell(context.Background(), cl, ShellOptions{Stdin: strings.NewReader(""), Stdout: &out, Stderr: io.Discard})
		done <- err
	}()
	waitFor(t, "shell", func() bool { return strings.Contains(out.String(), "up") })
	time.Sleep(900 * time.Millisecond) // three idle periods
	c2, err := ConnectCached(context.Background(), "hpcx", ConnectOptions{SocketPath: sock})
	if err != nil {
		t.Fatalf("helper or login gone while a shell is open: %v", err)
	}
	c2.Close()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestErrShellSignaledExitCode(t *testing.T) {
	if got := (&ErrShellSignaled{Sig: syscall.SIGTERM}).ExitCode(); got != 128+int(syscall.SIGTERM) {
		t.Errorf("exit code = %d", got)
	}
	if (&ErrShellSignaled{Sig: syscall.SIGTERM}).Error() == "" {
		t.Error("empty message")
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingReader) Read([]byte) (int, error) {
	close(b.started)
	<-b.release
	return 0, io.EOF
}

// guardedReader.stop waits for the read in flight, and later reads return EOF.
func TestGuardedReaderStopWaits(t *testing.T) {
	br := &blockingReader{started: make(chan struct{}), release: make(chan struct{})}
	g := newGuardedReader(br)
	go g.Read(make([]byte, 1))
	<-br.started
	stopped := make(chan struct{})
	go func() { g.stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("stop returned with a read in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(br.release)
	<-stopped
	if _, err := g.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("read after stop: %v", err)
	}
}

// A channel that closes without an exit status (a dropped connection) is an
// error, not success.
func TestRunShellDroppedConnection(t *testing.T) {
	shell := func(string, io.Reader, io.Writer, io.Writer) int { return 0 }
	_, sock, _ := helperEnv(t, sshtest.Options{User: "alice", Password: "pw", AllowPTY: true, Shell: shell, ShellNoStatus: true}, time.Minute)
	cl, err := Connect(context.Background(), "hpcx", &fakePrompter{answers: map[QuestionKind]string{AskPassword: "pw"}}, ConnectOptions{SocketPath: sock, Spawn: noSpawn})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	_, err = RunShell(context.Background(), cl, ShellOptions{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, ErrShellClosed) {
		t.Fatalf("err = %v, want ErrShellClosed", err)
	}
}
