package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/backend"
	"labmill-client/internal/sshconfig"
)

// HelperVersion is the helper's protocol version. It is part of the socket
// and lock file names (ssh-v<HelperVersion>.sock / .lock), so a Labmill never
// talks to a helper of another version: an old helper keeps serving old
// Labmill processes on its own socket and exits when idle, and a new Labmill
// starts a matching one (costing one extra login after an upgrade that bumps
// the version). Change it whenever the local protocol (user-name prefixes,
// relay encoding, hold requests) changes in a way an older or newer peer
// would misread.
const HelperVersion = "1"

// socketName is the helper socket's file name.
const socketName = "ssh-v" + HelperVersion + ".sock"

// SocketPath returns the per-user helper socket path:
// $XDG_RUNTIME_DIR/labmill/ssh-v<HelperVersion>.sock when XDG_RUNTIME_DIR is
// set and absolute, otherwise <home>/.local/state/labmill/ssh-v<HelperVersion>.sock.
// On Windows (AF_UNIX since Windows 10 1803) the same rule applies with
// <home> = the user profile.
func SocketPath() (string, error) {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "labmill", socketName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating helper socket: %w", err)
	}
	return filepath.Join(home, ".local", "state", "labmill", socketName), nil
}

// DialFunc dials an upstream alias; Dial satisfies it. Tests inject fakes.
type DialFunc func(ctx context.Context, p sshconfig.Paths, alias string, pr Prompter, opts LoginOptions) (*ssh.Client, error)

// HelperOptions configures Serve.
type HelperOptions struct {
	SocketPath  string          // required
	IdleTimeout time.Duration   // exit after this long with no local clients; 0 means 10m
	Paths       sshconfig.Paths // ssh config location for resolving aliases
	Login       LoginOptions    // base login options (TwoFactorHelp, PushTimeout, ...); User is per alias
	// LoginFor, when set, returns the login options for each upstream login
	// instead of Login, so a long-lived helper sees clusters (and their
	// institutions) added after it started.
	LoginFor func() LoginOptions
	Dial     DialFunc // nil means Dial
	// KeepaliveInterval and KeepaliveTimeout drive the upstream liveness
	// checks; zero means keepaliveInterval and keepaliveTimeout. Every
	// upstream login is probed with a keepalive request each interval and
	// closed when one gets no answer within the timeout. Before a login is
	// given to a new local client it is probed the same way (same timeout),
	// unless a keepalive answered within the last interval.
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
}

// Serve runs the connection helper until ctx is done or it has been idle for
// IdleTimeout. It creates the socket's directory (0700), refuses to start if
// another helper already answers on SocketPath, replaces a stale socket file
// (nothing listening) and makes the socket 0600.
//
// On the socket it runs an SSH server with an ephemeral host key. The client
// names the upstream alias as its SSH user name. Authentication is
// keyboard-interactive: if an upstream connection for that alias is alive the
// client is accepted without questions; otherwise the helper dials upstream
// with a Prompter that relays every Question and Notify to the connecting
// client as keyboard-interactive challenges (see relay encoding in
// EncodeRelay), so all prompts appear in the foreground UI. Upstream
// connections are cached per alias and dropped when they die.
//
// Every channel the client opens (session for exec/subsystem sftp,
// direct-tcpip, ...) is opened 1:1 on the upstream connection with the same
// type and extra data; data, stderr (extended data), EOF, close, and requests
// in both directions (with replies) are proxied faithfully, so exit-status and
// subsystems work unchanged.
//
// A Hold client (user name holdUser) is accepted without any question or
// upstream login; it keeps the helper from idle-expiring for as long as it
// stays connected and may ask which aliases are logged in (see Hold).
func Serve(ctx context.Context, opts HelperOptions) error {
	if opts.SocketPath == "" {
		return errors.New("helper: no socket path")
	}
	idle := opts.IdleTimeout
	if idle == 0 {
		idle = 10 * time.Minute
	}
	dial := opts.Dial
	if dial == nil {
		dial = Dial
	}
	dir := filepath.Dir(opts.SocketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("helper: %s is not a plain directory (symlink?); refusing to use it", dir)
	}
	if err := checkDirOwner(fi); err != nil {
		return fmt.Errorf("helper: %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	// The lock makes "check, remove stale socket, listen" atomic against a
	// concurrently starting helper, and is held for Serve's lifetime. It is named
	// after the socket (ssh-v1.sock -> ssh-v1.lock), so helpers of different
	// versions don't block each other.
	unlock, err := lockFile(filepath.Join(dir, strings.TrimSuffix(filepath.Base(opts.SocketPath), ".sock")+".lock"))
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return fmt.Errorf("helper already running on %s", opts.SocketPath)
		}
		return fmt.Errorf("helper: %w", err)
	}
	defer unlock()
	if c, err := net.DialTimeout("unix", opts.SocketPath, 2*time.Second); err == nil {
		c.Close()
		return fmt.Errorf("helper already running on %s", opts.SocketPath)
	}
	if _, err := os.Lstat(opts.SocketPath); err == nil {
		os.Remove(opts.SocketPath) // stale: nothing answers
	}
	ln, err := net.Listen("unix", opts.SocketPath)
	if err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	if err := os.Chmod(opts.SocketPath, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("helper: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		ln.Close()
		return err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		ln.Close()
		return err
	}

	loginCtx, cancelLogin := context.WithCancel(ctx)
	defer cancelLogin()
	h := &helper{opts: opts, dial: dial, ctx: loginCtx, upstream: map[string]*ssh.Client{},
		sconns: map[string]map[*ssh.ServerConn]struct{}{},
		locks:  map[string]*sync.Mutex{}, conns: map[net.Conn]struct{}{}, kaInterval: keepaliveInterval, kaTimeout: keepaliveTimeout, idleD: idle, fired: make(chan struct{}, 1),
		ups: map[*ssh.Client]*upState{}, gen: map[string]uint64{}}
	if opts.KeepaliveInterval > 0 {
		h.kaInterval = opts.KeepaliveInterval
	}
	if opts.KeepaliveTimeout > 0 {
		h.kaTimeout = opts.KeepaliveTimeout
	}
	h.signer = signer

	h.mu.Lock()
	h.idleT = time.AfterFunc(idle, h.idleFire)
	h.mu.Unlock()

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conns[c] = struct{}{}
			h.idleT.Stop()
			h.mu.Unlock()
			go h.serveConn(c)
		}
	}()

	select {
	case <-ctx.Done():
	case <-h.fired:
	}
	cancelLogin() // abort in-flight upstream logins
	ln.Close()
	h.mu.Lock()
	for c := range h.conns {
		c.Close()
	}
	for _, cl := range h.upstream {
		cl.Close()
	}
	h.mu.Unlock()
	os.Remove(opts.SocketPath)
	return nil
}

type helper struct {
	opts   HelperOptions
	dial   DialFunc
	ctx    context.Context
	signer ssh.Signer // ephemeral host key; each connection gets its own ServerConfig

	mu                    sync.Mutex
	upstream              map[string]*ssh.Client
	sconns                map[string]map[*ssh.ServerConn]struct{} // authenticated local conns per alias
	locks                 map[string]*sync.Mutex
	conns                 map[net.Conn]struct{}
	idleD                 time.Duration
	kaInterval, kaTimeout time.Duration
	idleT                 *time.Timer
	fired                 chan struct{}
	ups                   map[*ssh.Client]*upState // liveness per upstream login
	gen                   map[string]uint64        // per alias, raised by forget; guarded by mu
}

// upState is the liveness record of one upstream login (guarded by
// helper.mu).
type upState struct {
	// lastOK is when a keepalive last got an answer, on the wall clock
	// (Round(0) strips the monotonic reading, so time spent suspended
	// counts and a laptop that slept never looks recently checked).
	lastOK time.Time
	// probe is the reuse keepalive in flight, if any; callers share it.
	probe *probeRun
}

// probeRun is one keepalive request; done closes when it got an answer
// (ok) or failed.
type probeRun struct {
	start time.Time // wall clock, Round(0)
	done  chan struct{}
	ok    bool
}

// probeHook, when set by a test, runs each time a reuse probe sends its
// keepalive request.
var probeHook func()

func (h *helper) idleFire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.conns) == 0 {
		select {
		case h.fired <- struct{}{}:
		default:
		}
	}
}

func (h *helper) cached(alias string) *ssh.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.upstream[alias]
}

// authenticate is the keyboard-interactive callback: it reuses a live
// upstream connection or logs in, relaying prompts to the client.
func (h *helper) authenticate(connCtx context.Context, conn ssh.ConnMetadata, challenge ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	if conn.User() == holdUser {
		// Hold: a client that only keeps the helper alive and asks what is
		// logged in. It never names an alias, so nothing can log in.
		return &ssh.Permissions{}, nil
	}
	alias, cachedOnly := splitHelperUser(conn.User())
	if cachedOnly {
		// ConnectCached: never log in, and never wait for a login another
		// client is running; only an already-open upstream that still
		// answers counts.
		if h.live(alias) {
			return &ssh.Permissions{}, nil
		}
		return nil, ErrNotConnected
	}
	h.mu.Lock()
	lk := h.locks[alias]
	if lk == nil {
		lk = &sync.Mutex{}
		h.locks[alias] = lk
	}
	h.mu.Unlock()
	lk.Lock()
	defer lk.Unlock()
	h.mu.Lock()
	startGen := h.gen[alias]
	h.mu.Unlock()
	// A login that stopped answering is dropped here, so this client gets a
	// fresh login (with its prompts) instead of a connection that hangs.
	if h.live(alias) {
		return &ssh.Permissions{}, nil
	}
	// The login is aborted when the helper shuts down (h.ctx) or this local
	// connection goes away (connCtx).
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	stop := context.AfterFunc(connCtx, cancel)
	defer stop()
	client, err := h.dial(ctx, h.opts.Paths, alias, relayPrompter{challenge}, h.loginOptions())
	if err != nil {
		// Tell the client why (best effort) so it can show the real reason;
		// not when the login was aborted (the client is gone, or another
		// challenge may be in flight).
		if ctx.Err() == nil {
			challenge("", EncodeRelayError(err), nil, nil)
		}
		return nil, err
	}
	h.mu.Lock()
	if h.ctx.Err() != nil {
		// Serve began shutting down while we dialed: it has already (or is
		// about to) close the cached upstreams, so don't cache this one.
		h.mu.Unlock()
		client.Close()
		return nil, fmt.Errorf("helper is shutting down: %w", h.ctx.Err())
	}
	if h.gen[alias] != startGen {
		// forget ran while we signed in: this login may carry the old
		// account name, so it is discarded.
		h.mu.Unlock()
		client.Close()
		err := errors.New("the sign-in was reset while it ran; try again")
		challenge("", EncodeRelayError(err), nil, nil)
		return nil, err
	}
	h.upstream[alias] = client
	h.mu.Unlock()
	done := make(chan struct{})
	go keepalive(client, done, h.kaInterval, h.kaTimeout, func() { h.markOK(client) })
	go func() {
		client.Wait()
		close(done)
		h.drop(alias, client)
		h.mu.Lock()
		delete(h.ups, client)
		h.mu.Unlock()
	}()
	return &ssh.Permissions{}, nil
}

// markOK records that a keepalive on up got an answer now.
func (h *helper) markOK(up *ssh.Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stateLocked(up).lastOK = time.Now().Round(0)
}

// stateLocked returns up's liveness record, creating it. h.mu must be held.
func (h *helper) stateLocked(up *ssh.Client) *upState {
	st := h.ups[up]
	if st == nil {
		st = &upState{}
		h.ups[up] = st
	}
	return st
}

// recentlyOK reports that a keepalive on up answered within the last
// keepalive interval and none is waiting for an answer now.
func (h *helper) recentlyOK(up *ssh.Client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ups[up]
	return st != nil && st.probe == nil && !st.lastOK.IsZero() && time.Now().Round(0).Sub(st.lastOK) < h.kaInterval
}

// probe sends a keepalive request on up, or returns the one already in
// flight. A request that gets an answer records lastOK.
func (h *helper) probe(up *ssh.Client) *probeRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.stateLocked(up)
	if st.probe != nil {
		return st.probe
	}
	p := &probeRun{start: time.Now().Round(0), done: make(chan struct{})}
	st.probe = p
	if probeHook != nil {
		probeHook()
	}
	go func() {
		_, _, err := up.SendRequest("keepalive@openssh.com", true, nil)
		h.mu.Lock()
		p.ok = err == nil
		if p.ok {
			st.lastOK = time.Now().Round(0)
		}
		st.probe = nil
		h.mu.Unlock()
		close(p.done)
	}()
	return p
}

// drop forgets client as the upstream of alias (if it still is) and closes
// the local clients that use it, so they notice and connect again.
func (h *helper) drop(alias string, client *ssh.Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.upstream[alias] == client {
		delete(h.upstream, alias)
		for sc := range h.sconns[alias] {
			sc.Close()
		}
	}
}

// live reports whether alias has an upstream login that still answers: a
// keepalive answered within the last keepalive interval, or one sent now
// (or already in flight) answers within the keepalive timeout, counted from
// when it was sent. A login that doesn't is dropped and closed (in the
// background: Close may wait on a dead link), like the background
// keepalive would.
func (h *helper) live(alias string) bool {
	up := h.cached(alias)
	if up == nil {
		return false
	}
	if h.recentlyOK(up) {
		return true
	}
	p := h.probe(up)
	wait := h.kaTimeout - time.Now().Round(0).Sub(p.start)
	if wait < 0 {
		wait = 0
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.done:
		if p.ok {
			return true
		}
	case <-t.C:
		select {
		case <-p.done: // answered at the last moment
			if p.ok {
				return true
			}
		default:
		}
	case <-h.ctx.Done():
		return false
	}
	h.drop(alias, up)
	go up.Close()
	return false
}

// serveRequests answers a local client's global requests. A keepalive is
// answered only once the upstream login of alias answered one (so the
// client learns whether the cluster still answers, not just the helper);
// when the upstream is gone the local connection is closed instead (drop).
// Replies go out in order, as SSH requires. Anything else is refused.
func (h *helper) serveRequests(alias string, reqs <-chan *ssh.Request) {
	for r := range reqs {
		if r.Type != "keepalive@openssh.com" || !r.WantReply {
			if r.WantReply {
				r.Reply(false, nil)
			}
			continue
		}
		up := h.cached(alias)
		if up == nil {
			r.Reply(false, nil)
			continue
		}
		p := h.probe(up)
		<-p.done
		if p.ok {
			r.Reply(false, nil) // like OpenSSH: an answer, not a feature
		}
	}
}

// keepaliveInterval and keepaliveTimeout drive the upstream liveness probe
// (variables so tests can shorten them).
var (
	keepaliveInterval = 30 * time.Second
	keepaliveTimeout  = 15 * time.Second
)

// keepalive probes client every keepaliveInterval until done is closed and
// closes client when a probe fails or gets no answer within timeout; onOK
// (if not nil) runs after each answered probe.
func keepalive(client *ssh.Client, done <-chan struct{}, interval, timeout time.Duration, onOK func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		res := make(chan error, 1)
		go func() {
			_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
			res <- err
		}()
		select {
		case err := <-res:
			if err != nil {
				client.Close()
				return
			}
			if onOK != nil {
				onOK()
			}
		case <-time.After(timeout):
			client.Close()
			return
		case <-done:
			return
		}
	}
}

// relayPrompter forwards upstream prompts to the local client as
// keyboard-interactive challenges.
type relayPrompter struct {
	challenge ssh.KeyboardInteractiveChallenge
}

func (r relayPrompter) Ask(ctx context.Context, q Question) (string, error) {
	type res struct {
		a   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ins, prompt, echo := EncodeRelay(q)
		answers, err := r.challenge("", ins, []string{prompt}, []bool{echo})
		if err == nil && len(answers) != 1 {
			err = errors.New("helper: client sent no answer")
		}
		if err != nil {
			ch <- res{err: err}
			return
		}
		ch <- res{a: answers[0]}
	}()
	select {
	case v := <-ch:
		return v.a, v.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (r relayPrompter) Notify(msg string) {
	r.challenge("", notifyPrefix+msg, nil, nil)
}

const (
	relayPrefix      = "labmill-relay:"
	notifyPrefix     = "labmill-relay-notify:"
	relayErrorPrefix = "labmill-relay-error:"
)

func (h *helper) serveConn(c net.Conn) {
	defer func() {
		c.Close()
		h.mu.Lock()
		delete(h.conns, c)
		if len(h.conns) == 0 {
			h.idleT.Reset(h.idleD)
		}
		h.mu.Unlock()
	}()
	connCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := &ssh.ServerConfig{KeyboardInteractiveCallback: func(conn ssh.ConnMetadata, ch ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
		return h.authenticate(connCtx, conn, ch)
	}}
	cfg.AddHostKey(h.signer)
	// A read error (the local client went away) ends connCtx, aborting a login
	// that is waiting for that client's answers.
	sc, chans, reqs, err := ssh.NewServerConn(&cancelConn{Conn: c, cancel: cancel}, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	if sc.User() == holdUser {
		h.serveHold(chans, reqs)
		return
	}
	alias, _ := splitHelperUser(sc.User())
	go h.serveRequests(alias, reqs)
	h.mu.Lock()
	if h.upstream[alias] == nil {
		// the upstream died between authentication and now
		h.mu.Unlock()
		return
	}
	if h.sconns[alias] == nil {
		h.sconns[alias] = map[*ssh.ServerConn]struct{}{}
	}
	h.sconns[alias][sc] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.sconns[alias], sc)
		h.mu.Unlock()
	}()
	for nc := range chans {
		up := h.cached(alias)
		if up == nil {
			nc.Reject(ssh.ConnectionFailed, "upstream connection is gone")
			continue
		}
		go proxyChannel(up, nc)
	}
}

// cancelConn cancels a context when a Read fails.
type cancelConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *cancelConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.cancel()
	}
	return n, err
}

func proxyChannel(up *ssh.Client, nc ssh.NewChannel) {
	upCh, upReqs, err := up.OpenChannel(nc.ChannelType(), nc.ExtraData())
	if err != nil {
		var oe *ssh.OpenChannelError
		if errors.As(err, &oe) {
			nc.Reject(oe.Reason, oe.Message)
		} else {
			nc.Reject(ssh.ConnectionFailed, err.Error())
		}
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		upCh.Close()
		return
	}

	var upDone, clientDone sync.WaitGroup // upstream->client data+requests; client requests
	// reqMu is held while a client request is forwarded upstream and answered,
	// so ch is never closed between the upstream's reply and ours (the client
	// would see EOF instead of the reply when the command ends right away).
	var reqMu sync.Mutex

	// client -> upstream data (ends when the client sends EOF or closes).
	// Client-to-upstream extended data (stderr) is not proxied: no SSH
	// client sends any, and CloseWrite must not race a concurrent stderr write.
	go func() {
		io.Copy(upCh, ch)
		upCh.CloseWrite()
	}()

	// upstream -> client data; EOF only after both streams are drained.
	upDone.Add(2)
	var streams sync.WaitGroup
	streams.Add(2)
	go func() {
		defer upDone.Done()
		defer streams.Done()
		io.Copy(ch, upCh)
	}()
	go func() {
		defer streams.Done()
		io.Copy(ch.Stderr(), upCh.Stderr())
	}()
	go func() {
		defer upDone.Done()
		streams.Wait()
		ch.CloseWrite()
	}()

	// client requests -> upstream
	clientDone.Add(1)
	go func() {
		defer clientDone.Done()
		for r := range reqs {
			reqMu.Lock()
			ok, err := upCh.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok && err == nil, nil)
			}
			reqMu.Unlock()
		}
		// the client closed the channel
		upCh.Close()
	}()

	// upstream requests -> client (exit-status, exit-signal, ...)
	upDone.Add(1)
	go func() {
		defer upDone.Done()
		for r := range upReqs {
			ok, err := ch.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				r.Reply(ok && err == nil, nil)
			}
		}
	}()

	upDone.Wait() // everything from upstream has been delivered
	reqMu.Lock()  // let a request in flight be answered first
	reqMu.Unlock()
	ch.Close()
	clientDone.Wait()
	upCh.Close()
}

// EncodeRelay encodes a Question as a keyboard-interactive challenge
// (instruction, prompt, echo) for the helper-to-client relay. The
// instruction is "labmill-relay:<kind>:<host>" (kind as an integer); the
// prompt and echo carry Question.Prompt and Question.Echo. Notify messages
// are encoded as instruction "labmill-relay-notify:<msg>" with no questions.
func EncodeRelay(q Question) (instruction, prompt string, echo bool) {
	return relayPrefix + strconv.Itoa(int(q.Kind)) + ":" + q.Host, q.Prompt, q.Echo
}

// DecodeRelay reverses EncodeRelay. ok is false for challenges that weren't
// produced by EncodeRelay.
func DecodeRelay(instruction, prompt string, echo bool) (q Question, ok bool) {
	rest, found := strings.CutPrefix(instruction, relayPrefix)
	if !found {
		return Question{}, false
	}
	kind, host, found := strings.Cut(rest, ":")
	if !found {
		return Question{}, false
	}
	k, err := strconv.Atoi(kind)
	if err != nil {
		return Question{}, false
	}
	return Question{Kind: QuestionKind(k), Host: host, Prompt: prompt, Echo: echo}, true
}

// EncodeRelayError encodes an upstream login failure as the instruction of a
// zero-question keyboard-interactive challenge:
// "labmill-relay-error:<code>:<err.Error()>". The code is 2fa, hostkey, key,
// nokey, password, denied or canceled when errors.Is matches ErrTwoFactor,
// ErrHostKeyMismatch, ErrKeyRejected, ErrNoKey, ErrPasswordRejected,
// ErrAuthDenied or context.Canceled (checked in that order), else error.
func EncodeRelayError(err error) string {
	code := "error"
	switch {
	case errors.Is(err, ErrTwoFactor):
		code = "2fa"
	case errors.Is(err, ErrHostKeyMismatch):
		code = "hostkey"
	case errors.Is(err, ErrKeyRejected):
		code = "key"
	case errors.Is(err, ErrNoKey):
		code = "nokey"
	case errors.Is(err, ErrPasswordRejected):
		code = "password"
	case errors.Is(err, ErrAuthDenied):
		code = "denied"
	case errors.Is(err, context.Canceled):
		code = "canceled"
	}
	return relayErrorPrefix + code + ":" + err.Error()
}

// relayedError is an upstream failure received through the helper. Error is
// the original message; Unwrap returns the matching sentinel (nil for code
// "error"), so errors.Is(err, ErrTwoFactor) etc. work for callers.
type relayedError struct {
	sentinel error
	msg      string
}

func (e *relayedError) Error() string { return e.msg }
func (e *relayedError) Unwrap() error { return e.sentinel }

// DecodeRelayError reverses EncodeRelayError. ok is false when instruction
// was not produced by it. The returned error's message is the original
// message and errors.Is reports the sentinel its code stands for.
func DecodeRelayError(instruction string) (error, bool) {
	rest, found := strings.CutPrefix(instruction, relayErrorPrefix)
	if !found {
		return nil, false
	}
	code, msg, found := strings.Cut(rest, ":")
	if !found {
		return nil, false
	}
	var sentinel error
	switch code {
	case "2fa":
		sentinel = ErrTwoFactor
	case "hostkey":
		sentinel = ErrHostKeyMismatch
	case "key":
		sentinel = ErrKeyRejected
	case "nokey":
		sentinel = ErrNoKey
	case "password":
		sentinel = ErrPasswordRejected
	case "denied":
		sentinel = ErrAuthDenied
	case "canceled":
		sentinel = context.Canceled
	case "error":
	default:
		return nil, false
	}
	return &relayedError{sentinel: sentinel, msg: msg}, true
}

// ConnectOptions configures Connect.
type ConnectOptions struct {
	SocketPath string // "" means SocketPath()
	// Spawn starts a helper in the background when none is listening. nil
	// means: run os.Executable() with the argument "ssh-helper", detached
	// from this process (own session/process group), stdin/stdout to
	// os.DevNull and stderr appended to <state dir>/labmill/ssh-helper.log.
	// Connect only spawns when nothing is listening on the socket (not
	// found / connection refused); other dial errors are returned as is.
	Spawn func() error
	// Wait bounds how long Connect waits for a freshly spawned helper's
	// socket to appear. 0 means 5s.
	Wait time.Duration
}

// Connect returns an *ssh.Client for alias through the helper, spawning the
// helper if nothing listens on the socket. Relayed prompts (DecodeRelay) are
// answered with pr; anything else is an error. The local hop uses
// ssh.InsecureIgnoreHostKey: the socket is protected by its 0600 mode inside
// a 0700 directory, and the helper's host key is ephemeral.
func Connect(ctx context.Context, alias string, pr Prompter, opts ConnectOptions) (*ssh.Client, error) {
	// Whatever the outcome (login done by the helper, already held, failed,
	// canceled), a sign-in notice relayed meanwhile is cleared on return.
	cp := &clearingPrompter{Prompter: pr}
	pr = cp
	defer func() {
		if cp.shown.Load() {
			cp.Prompter.Notify("")
		}
	}()
	sock := opts.SocketPath
	if sock == "" {
		var err error
		if sock, err = SocketPath(); err != nil {
			return nil, err
		}
	}
	// relayed holds the upstream failure the helper reported, if any; the
	// handshake may wrap or replace the callback's error.
	var relayed error
	// promptErr is the error the Prompter returned (e.g. a user cancel), kept
	// so errors.Is works through the handshake's wrapping.
	var promptErr error
	failure := func(err error) error {
		switch {
		case relayed != nil:
			return relayed
		case promptErr != nil:
			return promptErr
		}
		return err
	}
	cfg := &ssh.ClientConfig{
		User:            alias,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Auth: []ssh.AuthMethod{ssh.KeyboardInteractive(func(_, instr string, qs []string, echos []bool) ([]string, error) {
			if re, ok := DecodeRelayError(instr); ok {
				relayed = re
				return nil, re
			}
			if msg, ok := strings.CutPrefix(instr, notifyPrefix); ok {
				pr.Notify(msg)
				return make([]string, len(qs)), nil
			}
			if len(qs) != 1 {
				return nil, fmt.Errorf("helper sent an unexpected challenge %q", instr)
			}
			q, ok := DecodeRelay(instr, qs[0], echos[0])
			if !ok {
				return nil, fmt.Errorf("helper sent an unexpected challenge %q", instr)
			}
			a, err := pr.Ask(ctx, q)
			if err != nil {
				promptErr = err
				return nil, err
			}
			return []string{a}, nil
		})},
	}
	cl, err := dialHelper(ctx, sock, cfg)
	if err == nil {
		return cl, nil
	}
	var de *dialError
	if !errors.As(err, &de) {
		return nil, failure(err)
	}
	if !nothingListening(de.err) {
		return nil, fmt.Errorf("connecting to helper: %w", de.err)
	}
	spawn := opts.Spawn
	if spawn == nil {
		spawn = spawnHelper
	}
	wait := opts.Wait
	if wait == 0 {
		wait = 5 * time.Second
	}
	// A spawned helper can lose the lock to another helper that is just coming
	// up (or still exiting) and quit; if nothing answers within wait, spawn
	// once more.
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := spawn(); err != nil {
			return nil, fmt.Errorf("starting helper: %w", err)
		}
		deadline := time.Now().Add(wait)
		for {
			cl, err := dialHelper(ctx, sock, cfg)
			if err == nil {
				return cl, nil
			}
			if !errors.As(err, &de) {
				return nil, failure(err)
			}
			lastErr = de.err
			if time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	msg := fmt.Sprintf("helper did not come up on %s", sock)
	if lp := helperLogPath(); lp != "" {
		msg += fmt.Sprintf(" (see its log at %s)", lp)
	}
	return nil, fmt.Errorf("%s: %w", msg, lastErr)
}

// cachedOnlyPrefix marks a local login (the SSH user name on the helper
// socket) that must not log in upstream: the helper accepts it only while it
// already holds a live connection for the alias after the prefix. An older
// helper that doesn't know the prefix treats the whole name as an alias,
// which resolves to no host, so it fails without asking anything.
const cachedOnlyPrefix = "labmill-cached-only:"

// splitHelperUser returns the alias of a helper login name and whether it
// asked for an existing connection only.
func splitHelperUser(user string) (alias string, cachedOnly bool) {
	if a, ok := strings.CutPrefix(user, cachedOnlyPrefix); ok {
		return a, true
	}
	return user, false
}

// ErrNotConnected is returned by ConnectCached when there is no open login
// for the alias to reuse (no helper running, or the helper holds none).
var ErrNotConnected = errors.New("not logged in")

// ConnectCached returns an *ssh.Client for alias only when the helper
// already holds a live login for it. It never starts a helper, never logs in
// and never asks anything: background work (the job monitor) uses it so it
// can't prompt the user or trigger a 2FA push. Without such a login it
// returns ErrNotConnected; ctx errors are returned as is. opts.Spawn and
// opts.Wait are ignored.
func ConnectCached(ctx context.Context, alias string, opts ConnectOptions) (*ssh.Client, error) {
	sock := opts.SocketPath
	if sock == "" {
		var err error
		if sock, err = SocketPath(); err != nil {
			return nil, err
		}
	}
	cfg := &ssh.ClientConfig{
		User:            cachedOnlyPrefix + alias,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Auth: []ssh.AuthMethod{ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) {
			// Any challenge means the helper wants a login (or reports a
			// failure): refuse it.
			return nil, ErrNotConnected
		})},
	}
	cl, err := dialHelper(ctx, sock, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrNotConnected
	}
	return cl, nil
}

// dialError marks a failure to reach the socket at all (nobody listening),
// as opposed to a failed handshake.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

func dialHelper(ctx context.Context, sock string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &dialError{err}
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	sc, chans, reqs, err := ssh.NewClientConn(c, "labmill-helper", cfg)
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("helper login: %w", err)
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

// spawnHelper starts "<this executable> ssh-helper" detached from this
// process.
func spawnHelper() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	var logw io.Writer = null
	if f := openHelperLog(); f != nil {
		defer f.Close()
		logw = f
	}
	cmd := exec.Command(exe, "ssh-helper")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, logw
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// helperLogPath returns <state dir>/labmill/ssh-helper.log, or "" when no home
// directory can be found. The state dir is $XDG_STATE_HOME when absolute,
// else <home>/.local/state.
func helperLogPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "labmill", "ssh-helper.log")
}

// openHelperLog opens helperLogPath for appending (0600), or returns nil when
// that isn't possible.
func openHelperLog() *os.File {
	p := helperLogPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}

// ExecOptions configures Exec.
type ExecOptions struct {
	Connect ConnectOptions // how to reach the helper
}

// Exec is the core of `labmill ssh-exec <alias> [-s <subsystem> | <command>...]`,
// the external-ssh command rclone runs. It connects to alias through the
// helper with a Prompter that never asks anything (a background transfer
// must not prompt): if the helper has no live connection for alias, Exec
// fails with an error saying the user must log in in Labmill first. With
// args ["-s", name] it requests that subsystem; otherwise it runs
// strings.Join(args, " ") as the remote command (rclone's convention: the
// command arrives already shell-quoted). stdin, stdout and stderr are bridged
// until the remote side finishes. It returns the remote exit status (0 for a
// subsystem that ended cleanly).
func Exec(ctx context.Context, alias string, args []string, stdin io.Reader, stdout, stderr io.Writer, opts ExecOptions) (int, error) {
	cl, err := Connect(ctx, alias, noAskPrompter{alias}, opts.Connect)
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	if len(args) == 2 && args[0] == "-s" {
		if args[1] == "sftp" {
			return execSFTP(ctx, cl, stdin, stdout, stderr)
		}
		return execSubsystem(ctx, cl, args[1], stdin, stdout, stderr)
	}
	return execCommand(ctx, cl, strings.Join(args, " "), stdin, stdout, stderr)
}

// execCommand runs command on a new session of cl, bridging stdio, and
// returns the remote exit status.
func execCommand(ctx context.Context, cl *ssh.Client, command string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	sess, err := cl.NewSession()
	if err != nil {
		return 0, fmt.Errorf("opening session: %w", err)
	}
	defer sess.Close()
	sess.Stdout, sess.Stderr = stdout, stderr
	in, err := sess.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := sess.Start(command); err != nil {
		return 0, err
	}
	// Closing the connection when ctx is done unblocks Wait.
	stop := context.AfterFunc(ctx, func() { cl.Close() })
	defer stop()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		copyUntil(ended, in, stdin)
		in.Close() // EOF to the remote side
	}()
	err = sess.Wait()
	if cerr := ctx.Err(); cerr != nil {
		return 0, cerr
	}
	var ee *ssh.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitStatus(), nil
	}
	return 0, err
}

// sftpServerCommand runs the first sftp-server binary that exists, for hosts
// whose sshd has no working "sftp" subsystem (a login host that accepts the
// subsystem request and then closes the channel without a byte, while
// running /usr/libexec/openssh/sftp-server as a command works). It is
// quoted for the remote login shell (bash, csh, fish) and run by sh.
var sftpServerCommand = backend.Quote([]string{"sh", "-c",
	`for p in /usr/libexec/openssh/sftp-server /usr/lib/openssh/sftp-server /usr/lib/ssh/sftp-server /usr/libexec/sftp-server; do [ -x "$p" ] && exec "$p"; done; echo "Labmill: no sftp-server found on this host" >&2; exit 127`})

// execSFTP serves rclone's `-s sftp`: it requests the sftp subsystem and, if
// that produces no output at all (refused, or accepted and closed at once),
// replays what the client had sent so far to sftp-server run as a command.
// stdin is read by a single goroutine so that the abandoned first attempt
// never swallows bytes meant for the second.
func execSFTP(ctx context.Context, cl *ssh.Client, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	chunks := make(chan []byte, 16)
	go func() {
		defer close(chunks)
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	first := &chunkReader{chunks: chunks, stop: make(chan struct{}), record: true}
	cw := &countWriter{w: stdout}
	code, err := execSubsystem(ctx, cl, "sftp", first, cw, io.Discard)
	close(first.stop)
	if ctx.Err() != nil || cw.n > 0 {
		return code, err
	}
	// Nothing came back: fall back to running sftp-server directly.
	replay, err := first.replayBytes()
	if err != nil {
		return 0, err
	}
	second := &chunkReader{chunks: chunks, stop: make(chan struct{}), pending: replay}
	defer close(second.stop)
	return execCommand(ctx, cl, sftpServerCommand, second, stdout, stderr)
}

// chunkReader reads the chunks of a shared stdin. Reads return EOF once stop
// is closed, without taking another chunk. mu is held for a whole Read, so
// replayBytes called after stop is closed waits for an in-flight Read and
// sees every chunk that Read took.
type chunkReader struct {
	chunks  <-chan []byte
	stop    chan struct{}
	record  bool // keep a copy of everything taken, in sent
	mu      sync.Mutex
	pending []byte // taken from chunks but not yet delivered
	sent    []byte // delivered bytes, at most maxReplay
	over    bool   // more than maxReplay bytes were delivered
}

// replayBytes returns everything the client has sent so far: delivered bytes
// plus any taken-but-undelivered remainder. It fails if the recording
// overflowed maxReplay, because a truncated replay would corrupt the stream.
func (r *chunkReader) replayBytes() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.over {
		return nil, fmt.Errorf("sftp subsystem failed after more than %d bytes were sent; cannot retry with sftp-server", maxReplay)
	}
	out := append([]byte(nil), r.sent...)
	return append(out, r.pending...), nil
}

const maxReplay = 1 << 20

func (r *chunkReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.pending) == 0 {
		select {
		case <-r.stop:
			return 0, io.EOF
		default:
		}
		select {
		case <-r.stop:
			return 0, io.EOF
		case c, ok := <-r.chunks:
			if !ok {
				return 0, io.EOF
			}
			r.pending = c
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	if r.record {
		if r.over || len(r.sent)+n > maxReplay {
			r.over = true
		} else {
			r.sent = append(r.sent, p[:n]...)
		}
	}
	return n, nil
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.w.Write(p)
}

// copyUntil copies src to dst until EOF, an error, or ended is closed. It
// checks ended after every read, so once the session is over nothing more is
// written; a Read already blocked inside src (e.g. an idle terminal) cannot
// be interrupted from here and returns when src next produces data or
// closes, which at the latest is when the process exits.
func copyUntil(ended <-chan struct{}, dst io.Writer, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		select {
		case <-ended:
			return
		default:
		}
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// execSubsystem runs a subsystem on a raw session channel (ssh.Session.Wait
// refuses sessions started with RequestSubsystem).
func execSubsystem(ctx context.Context, cl *ssh.Client, name string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	ch, reqs, err := cl.OpenChannel("session", nil)
	if err != nil {
		return 0, fmt.Errorf("opening session: %w", err)
	}
	defer ch.Close()
	ok, err := ch.SendRequest("subsystem", true, ssh.Marshal(struct{ Name string }{name}))
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("remote refused subsystem %q", name)
	}
	stop := context.AfterFunc(ctx, func() { cl.Close() })
	defer stop()
	ended := make(chan struct{})
	defer close(ended)
	go func() {
		copyUntil(ended, ch, stdin)
		ch.CloseWrite()
	}()
	var copies sync.WaitGroup
	copies.Add(2)
	go func() { defer copies.Done(); io.Copy(stdout, ch) }()
	go func() { defer copies.Done(); io.Copy(stderr, ch.Stderr()) }()
	// No exit-status at all (e.g. the helper connection dropped) is a
	// failure, not a clean end.
	status := 255
	for r := range reqs { // closed when the channel closes
		switch r.Type {
		case "exit-status":
			var p struct{ Status uint32 }
			if ssh.Unmarshal(r.Payload, &p) == nil {
				status = int(p.Status)
			}
		case "exit-signal":
			status = 255
		}
		if r.WantReply {
			r.Reply(false, nil)
		}
	}
	copies.Wait()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return status, nil
}

// noAskPrompter refuses every question: a background transfer never prompts.
type noAskPrompter struct{ alias string }

func (p noAskPrompter) Ask(context.Context, Question) (string, error) {
	return "", fmt.Errorf("not logged in to %s - open Labmill and log in first", p.alias)
}
func (noAskPrompter) Notify(string) {}

// loginOptions are the options for the next upstream login.
func (h *helper) loginOptions() LoginOptions {
	if h.opts.LoginFor != nil {
		return h.opts.LoginFor()
	}
	return h.opts.Login
}
