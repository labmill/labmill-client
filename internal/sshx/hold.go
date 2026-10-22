package sshx

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/startup"
)

// holdUser is the helper login name of a Hold client. The colon makes it an
// invalid host alias, so an older helper that doesn't know it takes it for an
// alias it can't resolve and fails the handshake (or asks a question, which
// Hold refuses) without logging in to anything.
const holdUser = "labmill-hold:"

// loggedInRequest is the global request a Hold client sends to learn which
// aliases the helper holds a live upstream login for. The reply payload is
// the aliases, sorted, one per line.
const loggedInRequest = "logged-in@labmill"

// ErrNoHelper is returned by Hold when no helper is listening on the socket.
var ErrNoHelper = errors.New("no connection helper is running")

// HoldConn is a connection to the helper that keeps it running: the helper
// never idle-expires while a client is connected, and a HoldConn is such a
// client that carries no SSH traffic. Closing it lets the normal idle expiry
// (HelperOptions.IdleTimeout, 10 minutes by default) start again.
type HoldConn struct {
	cl *ssh.Client
}

// Hold connects to a running helper as a hold client. It never starts a
// helper (nothing listening means ErrNoHelper), never logs in upstream and
// never asks anything. opts.Spawn and opts.Wait are ignored.
func Hold(ctx context.Context, opts ConnectOptions) (*HoldConn, error) {
	sock := opts.SocketPath
	if sock == "" {
		var err error
		if sock, err = SocketPath(); err != nil {
			return nil, err
		}
	}
	cfg := &ssh.ClientConfig{
		User:            holdUser,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Auth: []ssh.AuthMethod{ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
			// A challenge means an older helper took the name for an alias and
			// wants a login: refuse it.
			return nil, errors.New("helper asked a question on a hold connection")
		})},
	}
	cl, err := dialHelper(ctx, sock, cfg)
	if err != nil {
		var de *dialError
		if errors.As(err, &de) && nothingListening(de.err) {
			return nil, ErrNoHelper
		}
		return nil, err
	}
	return &HoldConn{cl: cl}, nil
}

// LoggedIn returns the aliases the helper holds a live upstream login for,
// sorted. An older helper that doesn't know the request returns an error.
func (h *HoldConn) LoggedIn() ([]string, error) {
	ok, payload, err := h.cl.SendRequest(loggedInRequest, true, nil)
	if err != nil {
		return nil, fmt.Errorf("ask helper: %w", err)
	}
	if !ok {
		return nil, errors.New("helper doesn't report its logins (older version)")
	}
	if len(payload) == 0 {
		return nil, nil
	}
	return strings.Split(string(payload), "\n"), nil
}

// forgetRequest asks the helper to close and forget its upstream logins of
// the aliases in the payload (one per line), so the next connect signs in
// again with the SSH settings as they are then (a changed account name).
const forgetRequest = "forget-logins@labmill"

// forgetUnsupportedHook, when set by a test, makes the helper refuse the
// forget request like an older helper does.
var forgetUnsupportedHook func() bool

// ErrForgetUnsupported is returned by Forget when the running helper is an
// older one that doesn't know the request.
var ErrForgetUnsupported = errors.New("helper can't forget logins (older version)")

// ErrStillLoggedIn is returned by ForgetLogins when an older helper can't
// sign out and still holds a login for one of the aliases.
var ErrStillLoggedIn = errors.New("signing out isn't possible with the running helper: restart the computer, or wait 10 minutes after quitting labmill")

// Forget makes the helper close and forget its logins for aliases. An older
// helper that doesn't know the request returns ErrForgetUnsupported.
func (h *HoldConn) Forget(aliases []string) error {
	ok, _, err := h.cl.SendRequest(forgetRequest, true, []byte(strings.Join(aliases, "\n")))
	if err != nil {
		return fmt.Errorf("ask helper: %w", err)
	}
	if !ok {
		return ErrForgetUnsupported
	}
	return nil
}

// ForgetLogins connects to a running helper and makes it forget its logins
// for aliases. With no helper running there is nothing to forget (nil). An
// older helper that can't forget is fine when it holds no login for any of
// the aliases (the usual case right after a rejected sign-in); otherwise the
// result is ErrStillLoggedIn.
func ForgetLogins(ctx context.Context, opts ConnectOptions, aliases []string) error {
	hc, err := Hold(ctx, opts)
	if errors.Is(err, ErrNoHelper) {
		return nil
	}
	if err != nil {
		return err
	}
	defer hc.Close()
	err = hc.Forget(aliases)
	if !errors.Is(err, ErrForgetUnsupported) {
		return err
	}
	in, err := hc.LoggedIn()
	if err != nil {
		return err
	}
	for _, a := range aliases {
		if slices.Contains(in, a) {
			return ErrStillLoggedIn
		}
	}
	return nil
}

// forget closes and forgets the upstream logins of aliases and the local
// connections that use them.
func (h *helper) forget(aliases []string) {
	h.mu.Lock()
	var ups []*ssh.Client
	for _, a := range aliases {
		// A sign-in in flight for a (it holds the alias lock, which is not
		// taken here) notices the new generation and discards its login.
		h.gen[a]++
		if up := h.upstream[a]; up != nil {
			ups = append(ups, up)
			delete(h.upstream, a)
			delete(h.ups, up)
		}
		for sc := range h.sconns[a] {
			sc.Close()
		}
	}
	h.mu.Unlock()
	for _, up := range ups {
		go up.Close()
	}
}

// Wait blocks until the connection to the helper ends.
func (h *HoldConn) Wait() error { return h.cl.Wait() }

// Close ends the hold; the helper's idle timer starts once no other client
// is connected.
func (h *HoldConn) Close() error { return h.cl.Close() }

// serveHold serves a hold client: it opens no channels and answers
// loggedInRequest until the client goes away.
func (h *helper) serveHold(chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	go func() {
		for nc := range chans {
			nc.Reject(ssh.Prohibited, "hold connections carry no channels")
		}
	}()
	for req := range reqs {
		switch req.Type {
		case loggedInRequest:
			req.Reply(true, []byte(strings.Join(h.loggedIn(), "\n")))
		case forgetRequest:
			if forgetUnsupportedHook != nil && forgetUnsupportedHook() {
				req.Reply(false, nil) // acts like an older helper
				continue
			}
			var aliases []string
			if len(req.Payload) > 0 {
				aliases = strings.Split(string(req.Payload), "\n")
			}
			h.forget(aliases)
			req.Reply(true, nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// loggedIn lists the aliases with a live upstream connection, sorted.
func (h *helper) loggedIn() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.upstream))
	for a := range h.upstream {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// Keeper keeps a HoldConn to the helper while a long-running front end (the
// desktop app, also while its window is hidden in the tray) is running, so
// the helper's logins never idle-expire under it. It only attaches to a
// helper that is already running (a login started it); it never starts one,
// never logs in and never triggers a 2FA push. After Run returns, the
// helper's normal idle expiry applies again.
type Keeper struct {
	Options ConnectOptions // SocketPath ("" = SocketPath()); Spawn and Wait are ignored
	// Interval is how often Run asks the helper what is logged in and, while
	// no helper is running, how often it tries to attach. 0 means 15 s.
	Interval time.Duration
	// ErrorBackoff is how long Run waits after a handshake error other than
	// ErrNoHelper (an older helper that rejects the hold login: retrying
	// every Interval would only repeat it). 0 means 10 minutes.
	ErrorBackoff time.Duration

	dial func(context.Context, ConnectOptions) (*HoldConn, error) // Hold unless a test replaces it

	mu      sync.Mutex
	held    bool
	aliases []string
}

// Run holds the helper until ctx is done, re-attaching every Interval when
// no helper is running or the connection to it ended.
func (k *Keeper) Run(ctx context.Context) {
	interval := k.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	backoff := k.ErrorBackoff
	if backoff <= 0 {
		backoff = 10 * time.Minute
	}
	dial := k.dial
	if dial == nil {
		dial = Hold
	}
	first := true
	for {
		wait := interval
		endDial := func() {}
		if first {
			endDial = startup.Span("background: ssh helper attach (first)")
			first = false
		}
		hc, err := dial(ctx, k.Options)
		endDial()
		if err == nil {
			k.hold(ctx, hc, interval)
			hc.Close()
		} else if !errors.Is(err, ErrNoHelper) {
			wait = backoff
		}
		k.set(false, nil)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// hold polls hc every interval until it dies or ctx is done.
func (k *Keeper) hold(ctx context.Context, hc *HoldConn, interval time.Duration) {
	dead := make(chan struct{})
	go func() { hc.Wait(); close(dead) }()
	for {
		aliases, _ := hc.LoggedIn() // an older helper: held, logins unknown
		k.set(true, aliases)
		select {
		case <-ctx.Done():
			return
		case <-dead:
			return
		case <-time.After(interval):
		}
	}
}

func (k *Keeper) set(held bool, aliases []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.held, k.aliases = held, aliases
}

// LoggedIn returns the latest snapshot: the aliases the helper is logged in
// to and whether the Keeper currently holds the helper. It never blocks on
// the helper.
func (k *Keeper) LoggedIn() (aliases []string, held bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.aliases), k.held
}
