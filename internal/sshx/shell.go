package sshx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"labmill-client/internal/backend"
)

// DefaultTerm is the terminal type requested when the local TERM is unset.
const DefaultTerm = "xterm-256color"

// ShellStartCommand returns the remote command that starts the user's login
// shell in dir ("cd -- 'dir' && exec $SHELL -l"), or "" for dir "" (a plain
// login shell). A leading "~" or "~/" is kept as $HOME so it still expands
// when quoted; everything else is quoted for a POSIX login shell.
func ShellStartCommand(dir string) string {
	if dir == "" {
		return ""
	}
	var target string
	switch {
	case dir == "~":
		target = `"$HOME"`
	case strings.HasPrefix(dir, "~/"):
		target = `"$HOME"/` + backend.Quote([]string{dir[2:]})
	default:
		target = backend.Quote([]string{dir})
	}
	return "cd -- " + target + " && exec $SHELL -l"
}

// ShellOptions configures RunShell. The shell is the user's own: nothing
// typed or shown in it is logged or kept by Labmill.
type ShellOptions struct {
	// Dir, if not empty, is the folder the shell starts in (ShellStartCommand).
	Dir string
	// Term is the terminal type to request; "" means DefaultTerm.
	Term string
	// Width and Height are the initial terminal size in cells; 0 means 80x24.
	Width, Height int
	// Resize delivers new sizes (width, height) while the shell runs; nil
	// means the size never changes.
	Resize <-chan [2]int
	// Stdin, Stdout and Stderr are the user's terminal (or test doubles).
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// RunShell opens an interactive session with a pseudo-terminal on cl (a
// connection obtained through the helper, so no new sign-in), runs the
// user's login shell (or, with opts.Dir, a shell started in that folder) and
// bridges the terminal until it ends. It returns the remote exit status.
// Window-change requests follow opts.Resize. When ctx ends the session is
// closed. The caller owns cl and puts the local terminal in raw mode
// (see ShellExec).
func RunShell(ctx context.Context, cl *ssh.Client, opts ShellOptions) (int, error) {
	w, h := opts.Width, opts.Height
	if w <= 0 || h <= 0 {
		w, h = 80, 24
	}
	termName := opts.Term
	if termName == "" {
		termName = DefaultTerm
	}
	sess, err := cl.NewSession()
	if err != nil {
		return 0, fmt.Errorf("opening session: %w", err)
	}
	defer sess.Close()
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}
	if err := sess.RequestPty(termName, h, w, modes); err != nil {
		return 0, fmt.Errorf("asking for a terminal: %w", err)
	}
	sess.Stdout, sess.Stderr = opts.Stdout, opts.Stderr
	in, err := sess.StdinPipe()
	if err != nil {
		return 0, err
	}
	if cmd := ShellStartCommand(opts.Dir); cmd != "" {
		err = sess.Start(cmd)
	} else {
		err = sess.Shell()
	}
	if err != nil {
		return 0, fmt.Errorf("starting the shell: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		copyUntil(ended, in, opts.Stdin)
		in.Close()
	}()
	if opts.Resize != nil {
		go func() {
			for {
				select {
				case <-ended:
					return
				case sz, ok := <-opts.Resize:
					if !ok {
						return
					}
					if sz[0] > 0 && sz[1] > 0 {
						sess.WindowChange(sz[1], sz[0])
					}
				}
			}
		}()
	}
	err = sess.Wait()
	if cerr := ctx.Err(); cerr != nil {
		return 0, cerr
	}
	var ee *ssh.ExitError
	var em *ssh.ExitMissingError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &em):
		return 0, ErrShellClosed
	case errors.As(err, &ee):
		return ee.ExitStatus(), nil
	}
	return 0, err
}

// ErrShellClosed is returned by RunShell when the channel closed without an
// exit status (the connection dropped).
var ErrShellClosed = errors.New("the connection closed before the shell finished")

// ErrShellSignaled is returned by ShellExec.Run when the process was asked to
// stop (SIGTERM, SIGHUP) while the shell was open. The terminal has been
// restored; callers should quit (the CLI exits with 128 plus the signal
// number, see ExitCode).
type ErrShellSignaled struct{ Sig os.Signal }

func (e *ErrShellSignaled) Error() string { return "stopped by " + e.Sig.String() }

// ExitCode is the conventional shell exit status for the signal (128+n), or
// 143 when the signal has no number.
func (e *ErrShellSignaled) ExitCode() int {
	if s, ok := e.Sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 143
}

// ShellExec runs RunShell on the real terminal. It implements Bubble Tea's
// ExecCommand (Run, SetStdin, SetStdout, SetStderr), so the TUI can suspend
// itself while the shell runs. The terminal streams it is given are
// ignored: the shell always uses the process's own terminal, which must be
// interactive.
type ShellExec struct {
	Ctx    context.Context
	Client *ssh.Client // closed when the shell ends
	Alias  string      // for the log line only
	Dir    string
	// Note is written to the terminal before the shell starts ("" for none),
	// e.g. "Type exit to come back to Labmill."
	Note string
	// Logf receives "shell opened/closed on <alias>" only; never content.
	Logf func(format string, args ...any)
	// ExitCode is the remote exit status after Run returned nil.
	ExitCode int
}

// SetStdin, SetStdout and SetStderr satisfy tea.ExecCommand.
func (*ShellExec) SetStdin(io.Reader)  {}
func (*ShellExec) SetStdout(io.Writer) {}
func (*ShellExec) SetStderr(io.Writer) {}

// Run puts the local terminal in raw mode, runs the shell and restores the
// terminal on every exit path (also on a panic or SIGTERM/SIGHUP, which end
// the shell and come back as *ErrShellSignaled).
func (s *ShellExec) Run() error {
	defer s.Client.Close()
	ctx := s.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	in, out := os.Stdin, os.Stdout
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) {
		return errors.New("a shell needs an interactive terminal")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	restoreConsole := prepareConsole(in, out)
	defer restoreConsole()
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return fmt.Errorf("setting up the terminal: %w", err)
	}
	var once sync.Once
	restore := func() { once.Do(func() { term.Restore(int(in.Fd()), old) }) }
	defer restore()
	var signaled atomic.Value // os.Signal
	sigs := make(chan os.Signal, 1)
	notifyTermination(sigs)
	defer signal.Stop(sigs)
	go func() {
		select {
		case sig := <-sigs:
			signaled.Store(&sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	w, h, _ := term.GetSize(int(out.Fd()))
	done := make(chan struct{})
	rd := newGuardedReader(newInputReader(in, done))
	stopInput := func() {
		close(done)
		rd.stop() // no read of the terminal is left running
	}
	defer func() {
		select {
		case <-done:
		default:
			stopInput()
		}
	}()
	if s.Note != "" {
		fmt.Fprint(out, s.Note+"\r\n")
	}
	if s.Logf != nil {
		s.Logf("shell opened on %s", s.Alias)
		defer s.Logf("shell closed on %s", s.Alias)
	}
	code, err := RunShell(ctx, s.Client, ShellOptions{
		Dir: s.Dir, Term: os.Getenv("TERM"), Width: w, Height: h,
		Resize: watchResize(done, int(out.Fd())),
		Stdin:  rd, Stdout: out, Stderr: out,
	})
	stopInput()
	restore()
	s.ExitCode = code
	if p, ok := signaled.Load().(*os.Signal); ok && p != nil {
		return &ErrShellSignaled{Sig: *p}
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrShellClosed):
		return fmt.Errorf("the connection to %s closed: %w", s.Alias, err)
	case errors.Is(err, context.Canceled):
		return nil
	}
	return err
}

// guardedReader counts reads in flight so stop can wait for them: the
// underlying readers return within a poll interval once done is closed.
type guardedReader struct {
	r        io.Reader
	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
	stopped  bool
}

func newGuardedReader(r io.Reader) *guardedReader {
	g := &guardedReader{r: r}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *guardedReader) Read(p []byte) (int, error) {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return 0, io.EOF
	}
	g.inflight++
	g.mu.Unlock()
	n, err := g.r.Read(p)
	g.mu.Lock()
	g.inflight--
	g.cond.Broadcast()
	g.mu.Unlock()
	return n, err
}

// stop makes later reads return EOF and waits for the one in flight.
func (g *guardedReader) stop() {
	g.mu.Lock()
	g.stopped = true
	for g.inflight > 0 {
		g.cond.Wait()
	}
	g.mu.Unlock()
}
