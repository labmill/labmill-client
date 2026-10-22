package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/ssh"
)

// maxCapture caps each of stdout and stderr captured by SSHRunner.
const maxCapture = 4 << 20

// TruncatedMarker ends a Result.Stdout or Stderr that SSHRunner cut at its
// capture limit (4 MiB): what precedes it is the first part of the output.
const TruncatedMarker = "\n[output truncated]\n"

// SSHRunner is a Runner that executes commands on a remote host through an
// established SSH connection. The remote login shell parses the command, so
// argv is passed through Quote.
type SSHRunner struct {
	Client *ssh.Client
}

// capBuffer keeps the first maxCapture bytes and discards the rest.
type capBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	room := maxCapture - c.buf.Len()
	if room >= len(p) {
		c.buf.Write(p)
	} else {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		c.truncated = true
	}
	return len(p), nil // keep draining so the remote command doesn't block
}

func (c *capBuffer) String() string {
	if c.truncated {
		return c.buf.String() + TruncatedMarker
	}
	return c.buf.String()
}

// Run implements Runner. stdin must not be reused after Run returns: the SSH
// library's stdin pump may still be reading it.
func (r *SSHRunner) Run(ctx context.Context, argv []string, stdin io.Reader) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	sess, err := newSession(ctx, r.Client)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return Result{}, cerr
		}
		return Result{}, fmt.Errorf("open ssh session: %w", err)
	}
	// No deferred Close: on cancel a synchronous Close could block on a dead
	// link (it waits for the channel's write lock). Close on the other paths.
	var stdout, stderr capBuffer
	sess.Stdin = stdin
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	// Run in the background so a dead link can't hold us past ctx. On
	// cancel we close the session (also in the background: Close may block
	// on a dead link) and return at once; the goroutine ends when the
	// session does.
	runErr := make(chan error, 1)
	go func() { runErr <- sess.Run(Quote(argv)) }()
	select {
	case <-ctx.Done():
		// If the command finished at the same moment, prefer its result:
		// a successful sbatch's job ID must never be dropped.
		select {
		case err = <-runErr:
		default:
			go sess.Close()
			return Result{}, ctx.Err()
		}
	case err = <-runErr:
	}
	sess.Close()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	// A canceled context only matters if the run itself failed: never drop
	// the output (a job ID) of a command that succeeded.
	if cerr := ctx.Err(); cerr != nil && err != nil {
		return res, cerr
	}
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitStatus()
		return res, nil
	case errors.As(err, &missing):
		return res, fmt.Errorf("remote command ended without an exit status: %w", err)
	default:
		return res, fmt.Errorf("run remote command: %w", err)
	}
}

// newSession opens a session on cl, giving up when ctx ends: on a link that
// stopped answering (a laptop that slept, a dropped network) the channel
// open never gets a reply, and a caller's timeout must still apply. A
// session that opens after ctx ended is closed.
func newSession(ctx context.Context, cl *ssh.Client) (*ssh.Session, error) {
	type opened struct {
		s   *ssh.Session
		err error
	}
	ch := make(chan opened, 1)
	go func() {
		s, err := cl.NewSession()
		ch <- opened{s, err}
	}()
	select {
	case o := <-ch:
		return o.s, o.err
	case <-ctx.Done():
		go func() {
			if o := <-ch; o.s != nil {
				o.s.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

var _ Runner = (*SSHRunner)(nil)
