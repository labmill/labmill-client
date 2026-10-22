// Package sshtest provides an in-process SSH server for tests. It mimics the
// typical campus login flow: keyboard-interactive password, then a Duo-style 2FA menu
// where "1" sends a push. It also offers scripted exec commands (fake
// sinfo/sbatch, ...), the sftp subsystem, and direct-tcpip forwarding
// (for ProxyJump and SOCKS tests).
package sshtest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Duo selects the 2FA behavior after a correct password.
type Duo int

const (
	DuoNone            Duo = iota // no 2FA: password alone logs in
	DuoApprove                    // "1" → push approved after PushDelay; a correct Passcode also works
	DuoNever                      // "1" → push never approved (the server waits until the client gives up)
	DuoAutoPushNever              // no menu: the server auto-pushes ("Autopushing login request to phone...", zero questions) and never gets approval
	DuoAutoPushApprove            // like DuoAutoPushNever, but the push is approved after PushDelay
	DuoNotEnrolled                // instead of the menu, an instruction saying the user isn't enrolled, then failure
	DuoDeny                       // the push answer is accepted but the push is denied after PushDelay
)

// DuoInstruction and DuoQuestion are what the server sends for the 2FA menu
// (modeled on a Duo prompt).
const (
	DuoInstruction = "Duo two-factor login for %s\n\nEnter a passcode or select one of the following options:\n\n 1. Duo Push to XXX-XXX-1234\n 2. Phone call to XXX-XXX-1234\n 3. SMS passcodes to XXX-XXX-1234\n"
	DuoQuestion    = "Passcode or option (1-3): "
	// AutoPushInstruction is the zero-question round sent for DuoAutoPush*.
	AutoPushInstruction = "Autopushing login request to phone...\n"
	// NotEnrolledInstruction is sent for DuoNotEnrolled.
	NotEnrolledInstruction = "You are not enrolled in Duo two-factor authentication. Please enroll at https://duo.example.edu/enroll before logging in.\n"
)

// Handler runs a scripted command. args is everything after the command name
// (verbatim). It returns the exit status.
type Handler func(args string, stdin io.Reader, stdout, stderr io.Writer) int

// Options configures a Server.
type Options struct {
	User      string        // accepted username (required)
	Password  string        // accepted password (required)
	Duo       Duo           // 2FA behavior
	PushDelay time.Duration // DuoApprove: how long the "push" takes
	Passcode  string        // DuoApprove: a passcode accepted instead of the push option
	// DuoMenu overrides the menu text (default: DuoInstruction with the user
	// name filled in). PushOption is the answer the server treats as "Duo
	// Push" (default "1"); set it to match the number DuoMenu advertises.
	DuoMenu    string
	PushOption string
	// MenuInQuestion sends the menu text inside the question, with an empty
	// instruction, like OpenSSH+PAM does.
	MenuInQuestion bool
	// AuthorizedKeys, if not empty, enables public-key login for these keys
	// (in addition to the password/2FA flow). A key-authenticated login skips
	// the password and 2FA entirely.
	AuthorizedKeys []ssh.PublicKey
	// Commands maps a command name (the first word of an exec request) to its
	// handler. Unknown commands write "<name>: command not found" to stderr
	// and exit 127.
	Commands map[string]Handler
	// SFTPRoot, if set, serves the "sftp" subsystem with this directory as the
	// working directory (github.com/pkg/sftp server; not a chroot, test use
	// only).
	SFTPRoot string
	// SFTPAcceptThenClose, if > 0 (and SFTPRoot is empty), makes the server
	// accept the "sftp" subsystem, read exactly that many bytes, send
	// exit-status 1 and close the channel without any output (a real
	// behavior).
	SFTPAcceptThenClose int
	// AllowTCPForward enables direct-tcpip channels (the server dials the
	// requested host:port itself).
	AllowTCPForward bool
	// HostKeyTypes lists the host key types the server offers ("ed25519",
	// "ecdsa"); empty means just "ed25519". HostKey is the first one.
	HostKeyTypes []string
	// ExtraHiddenPrompt, if set, is asked (hidden, i.e. no echo) right after
	// the password, in its own round; the login is denied
	// unless the answer equals ExtraHiddenAnswer.
	ExtraHiddenPrompt string
	ExtraHiddenAnswer string
	// MaxAuthTries, if not zero, sets ssh.ServerConfig.MaxAuthTries (zero
	// keeps the x/crypto default of 6, negative means unlimited).
	MaxAuthTries int
	// KeyOnly makes the server accept public keys only, like a cluster that
	// disables password login: no keyboard-interactive, no password. Password
	// may then be empty, and AuthorizedKeys says who may log in.
	KeyOnly bool
	// PasswordAuth also offers the plain "password" method (User and
	// Password, no 2FA), after keyboard-interactive, like servers that list both.
	PasswordAuth bool
	// AllowPTY accepts "pty-req" on sessions (refused otherwise, like a
	// server with PermitTTY no). No terminal is emulated: the command's
	// handler sees the same channel either way. PTYs counts the requests.
	AllowPTY bool
	// Shell, if set, answers "shell" requests: the handler runs with the
	// channel as stdin/stdout/stderr (args is ""), its return value is sent
	// as the exit status, and the channel is closed. Window-change requests
	// are recorded (WindowChanges) while it runs. PTY requests are recorded
	// with their terminal name and size (PTYRequests).
	Shell Handler
	// ShellNoStatus makes the shell end by closing the channel without an
	// exit status (a dropped connection).
	ShellNoStatus bool
}

// PTYRequest is a recorded "pty-req".
type PTYRequest struct {
	Term          string
	Width, Height int
}

// Server is a running test server.
type Server struct {
	Addr     string                   // "127.0.0.1:<port>"
	HostKey  ssh.PublicKey            // the server's (random, per-server) ed25519 host key
	HostKeys map[string]ssh.PublicKey // by type name in Options.HostKeyTypes

	opts Options
	ln   net.Listener
	cfg  *ssh.ServerConfig
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	mu     sync.Mutex
	logins int
	keys   []ssh.PublicKey // authorized public keys
	execs  []string
	ptys   int
	ptyReq []PTYRequest
	winch  [][2]int
	shells int
	conns  map[string]*trackConn // by remote address
}

// trackConn is a net.Conn that signals when it is closed.
type trackConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *trackConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// New starts a Server on 127.0.0.1:0. It is closed automatically via
// t.Cleanup. It fails the test if opts.User or opts.Password is empty.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	if opts.User == "" || (opts.Password == "" && !opts.KeyOnly) {
		t.Fatal("sshtest: Options.User and Options.Password are required (Password not with KeyOnly)")
	}
	types := opts.HostKeyTypes
	if len(types) == 0 {
		types = []string{"ed25519"}
	}
	var signers []ssh.Signer
	for _, ty := range types {
		var priv any
		var err error
		switch ty {
		case "ed25519":
			_, priv, err = ed25519.GenerateKey(rand.Reader)
		case "ecdsa":
			priv, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		default:
			t.Fatalf("sshtest: unknown host key type %q", ty)
		}
		if err != nil {
			t.Fatal(err)
		}
		sg, err := ssh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		signers = append(signers, sg)
	}
	signer := signers[0]
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Addr:     ln.Addr().String(),
		HostKey:  signer.PublicKey(),
		HostKeys: map[string]ssh.PublicKey{},
		opts:     opts,
		ln:       ln,
		done:     make(chan struct{}),
		conns:    map[string]*trackConn{},
	}
	s.cfg = &ssh.ServerConfig{KeyboardInteractiveCallback: s.countLogin(s.keyboardInteractive), MaxAuthTries: opts.MaxAuthTries}
	if opts.KeyOnly {
		s.cfg.KeyboardInteractiveCallback = nil
	}
	if opts.PasswordAuth {
		s.cfg.PasswordCallback = func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == opts.User && string(pw) == opts.Password {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("sshtest: permission denied")
		}
	}
	s.keys = append(s.keys, opts.AuthorizedKeys...)
	if len(opts.AuthorizedKeys) > 0 || opts.KeyOnly {
		s.cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, k := range s.keys {
				if bytes.Equal(k.Marshal(), key.Marshal()) {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, errors.New("sshtest: key not authorized")
		}
		// A public-key query (RFC 4252 §7) only reaches PublicKeyCallback; a
		// login is counted once the signature has been verified.
		s.cfg.VerifiedPublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey, *ssh.Permissions, string) (*ssh.Permissions, error) {
			s.mu.Lock()
			s.logins++
			s.mu.Unlock()
			return &ssh.Permissions{}, nil
		}
	}
	for i, sg := range signers {
		s.cfg.AddHostKey(sg)
		s.HostKeys[types[i]] = sg.PublicKey()
	}
	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(func() { s.Close() })
	return s
}

// Authorize makes key a public key the server accepts from now on (like
// uploading it to the site's key registry). It needs a server created with
// KeyOnly or AuthorizedKeys, which enable public-key login.
func (s *Server) Authorize(key ssh.PublicKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, key)
}

// Logins returns how many connections have completed authentication so far
// (lets tests prove a connection was reused rather than re-dialed).
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// Execs returns the exec commands received so far, in order (full command
// strings).
func (s *Server) Execs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.execs...)
}

// PTYs returns how many pseudo-terminal requests the server accepted.
func (s *Server) PTYs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ptys
}

// PTYRequests returns the accepted pty-req requests (terminal name, size).
func (s *Server) PTYRequests() []PTYRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PTYRequest(nil), s.ptyReq...)
}

// WindowChanges returns the window-change requests received (width, height).
func (s *Server) WindowChanges() [][2]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][2]int(nil), s.winch...)
}

// Shells returns how many shell requests were accepted.
func (s *Server) Shells() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shells
}

// KnownHostsLine returns an OpenSSH known_hosts line for this server
// ("[127.0.0.1]:<port> ssh-ed25519 AAAA..."), via knownhosts.Line.
func (s *Server) KnownHostsLine() string {
	return knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, s.HostKey)
}

// KnownHostsLineFor is KnownHostsLine for the host key of the given type.
func (s *Server) KnownHostsLineFor(ty string) string {
	return knownhosts.Line([]string{knownhosts.Normalize(s.Addr)}, s.HostKeys[ty])
}

// KnownHostsLineHashed is a hashed (|1|...) known_hosts line for type ty.
func (s *Server) KnownHostsLineHashed(ty string) string {
	return knownhosts.Line([]string{knownhosts.HashHostname(knownhosts.Normalize(s.Addr))}, s.HostKeys[ty])
}

// Close stops the server and closes all its connections.
func (s *Server) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.ln.Close()
		s.mu.Lock()
		for _, c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	})
	s.wg.Wait()
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		tc := &trackConn{Conn: nc, closed: make(chan struct{})}
		key := nc.RemoteAddr().String()
		s.mu.Lock()
		select {
		case <-s.done:
			s.mu.Unlock()
			tc.Close()
			return
		default:
		}
		s.conns[key] = tc
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(tc, key)
	}
}

func (s *Server) serve(tc *trackConn, key string) {
	defer s.wg.Done()
	defer func() {
		tc.Close()
		s.mu.Lock()
		delete(s.conns, key)
		s.mu.Unlock()
	}()
	sc, chans, reqs, err := ssh.NewServerConn(tc, s.cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	var cwg sync.WaitGroup
	for nc := range chans {
		cwg.Add(1)
		go func(nc ssh.NewChannel) {
			defer cwg.Done()
			switch nc.ChannelType() {
			case "session":
				s.session(nc)
			case "direct-tcpip":
				s.directTCPIP(nc)
			default:
				nc.Reject(ssh.UnknownChannelType, "unsupported channel type")
			}
		}(nc)
	}
	sc.Close()
	cwg.Wait()
}

// waitClosed blocks until the server or the connection is closed (or a
// generous safety timeout passes, so a leaked server can never hang a test
// binary forever).
func (s *Server) waitClosed(conn ssh.ConnMetadata) {
	s.mu.Lock()
	tc := s.conns[conn.RemoteAddr().String()]
	s.mu.Unlock()
	var closed chan struct{}
	if tc != nil {
		closed = tc.closed
	}
	select {
	case <-s.done:
	case <-closed:
	case <-time.After(2 * time.Minute):
	}
}

// countLogin counts a login when the auth callback accepts it, which is
// before the client sees the success. Counting after ssh.NewServerConn
// returns would race with a client that checks Logins right after Connect.
func (s *Server) countLogin(cb func(ssh.ConnMetadata, ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error)) func(ssh.ConnMetadata, ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	return func(c ssh.ConnMetadata, ch ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
		p, err := cb(c, ch)
		if err == nil {
			s.mu.Lock()
			s.logins++
			s.mu.Unlock()
		}
		return p, err
	}
}

func (s *Server) keyboardInteractive(conn ssh.ConnMetadata, client ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
	deny := fmt.Errorf("sshtest: permission denied")
	if conn.User() != s.opts.User {
		return nil, deny
	}
	ans, err := client("", "", []string{"Password: "}, []bool{false})
	if err != nil || len(ans) != 1 || ans[0] != s.opts.Password {
		return nil, deny
	}
	if s.opts.ExtraHiddenPrompt != "" {
		ans, err = client("", "", []string{s.opts.ExtraHiddenPrompt}, []bool{false})
		if err != nil || len(ans) != 1 || ans[0] != s.opts.ExtraHiddenAnswer {
			return nil, deny
		}
	}
	ok := &ssh.Permissions{}
	switch s.opts.Duo {
	case DuoNone:
		return ok, nil
	case DuoNotEnrolled:
		client("", NotEnrolledInstruction, nil, nil)
		return nil, deny
	case DuoAutoPushNever, DuoAutoPushApprove:
		client("", AutoPushInstruction, nil, nil)
		if s.opts.Duo == DuoAutoPushApprove {
			select {
			case <-time.After(s.opts.PushDelay):
				return ok, nil
			case <-s.done:
				return nil, deny
			}
		}
		s.waitClosed(conn)
		return nil, deny
	}
	menu := s.opts.DuoMenu
	if menu == "" {
		menu = fmt.Sprintf(DuoInstruction, conn.User())
	}
	instr, question := menu, DuoQuestion
	if s.opts.MenuInQuestion {
		instr, question = "", menu+DuoQuestion
	}
	push := s.opts.PushOption
	if push == "" {
		push = "1"
	}
	ans, err = client("", instr, []string{question}, []bool{true})
	if err != nil || len(ans) != 1 {
		return nil, deny
	}
	switch {
	case ans[0] == push && s.opts.Duo == DuoApprove:
		select {
		case <-time.After(s.opts.PushDelay):
			return ok, nil
		case <-s.done:
			return nil, deny
		}
	case ans[0] == push && s.opts.Duo == DuoDeny:
		select {
		case <-time.After(s.opts.PushDelay):
		case <-s.done:
		}
		return nil, deny
	case ans[0] == push: // DuoNever
		s.waitClosed(conn)
		return nil, deny
	case s.opts.Duo == DuoApprove && s.opts.Passcode != "" && ans[0] == s.opts.Passcode:
		return ok, nil
	}
	return nil, deny
}

func (s *Server) session(nc ssh.NewChannel) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		switch req.Type {
		case "exec":
			var p struct{ Command string }
			if ssh.Unmarshal(req.Payload, &p) != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			s.mu.Lock()
			s.execs = append(s.execs, p.Command)
			s.mu.Unlock()
			status := s.runCommand(p.Command, ch)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
			return
		case "pty-req":
			if !s.opts.AllowPTY {
				req.Reply(false, nil)
				continue
			}
			var p struct {
				Term         string
				W, H, PW, PH uint32
				Modes        string
			}
			ssh.Unmarshal(req.Payload, &p)
			s.mu.Lock()
			s.ptys++
			s.ptyReq = append(s.ptyReq, PTYRequest{Term: p.Term, Width: int(p.W), Height: int(p.H)})
			s.mu.Unlock()
			req.Reply(true, nil)
		case "window-change":
			var p struct{ W, H, PW, PH uint32 }
			ssh.Unmarshal(req.Payload, &p)
			s.mu.Lock()
			s.winch = append(s.winch, [2]int{int(p.W), int(p.H)})
			s.mu.Unlock()
			if req.WantReply {
				req.Reply(true, nil)
			}
		case "shell":
			if s.opts.Shell == nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			s.mu.Lock()
			s.shells++
			s.mu.Unlock()
			go func() {
				status := s.opts.Shell("", ch, ch, ch.Stderr())
				if !s.opts.ShellNoStatus {
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
				}
				ch.Close()
			}()
		case "subsystem":
			var p struct{ Name string }
			if ssh.Unmarshal(req.Payload, &p) != nil || p.Name != "sftp" || (s.opts.SFTPRoot == "" && s.opts.SFTPAcceptThenClose <= 0) {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			if s.opts.SFTPRoot == "" {
				io.ReadFull(ch, make([]byte, s.opts.SFTPAcceptThenClose))
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{1}))
				return
			}
			srv, err := sftp.NewServer(ch, sftp.WithServerWorkingDirectory(s.opts.SFTPRoot))
			if err != nil {
				return
			}
			srv.Serve()
			// Like OpenSSH's sftp-server, report a clean exit (before Close,
			// which closes the channel).
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			srv.Close()
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func (s *Server) runCommand(cmd string, ch ssh.Channel) int {
	cmd = strings.TrimSpace(cmd)
	// Like a shell: skip a leading `env VAR=value ...` prefix.
	for strings.HasPrefix(cmd, "env ") {
		rest := strings.TrimSpace(cmd[4:])
		for {
			w, r, _ := strings.Cut(rest, " ")
			if !strings.Contains(w, "=") {
				break
			}
			rest = strings.TrimSpace(r)
		}
		cmd = rest
	}
	parts := strings.SplitN(cmd, " ", 2)
	name, args := parts[0], ""
	if len(parts) == 2 {
		args = parts[1]
	}
	h, ok := s.opts.Commands[name]
	if !ok {
		fmt.Fprintf(ch.Stderr(), "%s: command not found\n", name)
		return 127
	}
	return h(args, ch, ch, ch.Stderr())
}

func (s *Server) directTCPIP(nc ssh.NewChannel) {
	if !s.opts.AllowTCPForward {
		nc.Reject(ssh.Prohibited, "tcp forwarding disabled")
		return
	}
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &p); err != nil {
		nc.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	up, err := net.Dial("tcp", net.JoinHostPort(p.Host, fmt.Sprint(p.Port)))
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		up.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	// Closing either side (or the server) tears the whole pipe down.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(up, ch)
		if tc, ok := up.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(ch, up)
		ch.CloseWrite()
	}()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-s.done:
			up.Close()
			ch.Close()
		case <-finished:
		}
	}()
	wg.Wait()
	up.Close()
	ch.Close()
}
