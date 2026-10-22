// Package sshx is Labmill's Go-native SSH layer (no OpenSSH binary is used).
//
// Pieces:
//   - Login (this file): connect to one host with the user's own account. A
//     dialog-driven keyboard-interactive login: username, password, then the
//     Duo-style 2FA menu, answered with its "Duo Push" option automatically,
//     with setup help when no push arrives. Host keys are checked against ~/.ssh/known_hosts
//     (OpenSSH format), and an unknown host is confirmed by the user once
//     (trust on first use).
//   - Dial: follow a ProxyJump chain (from package sshconfig), logging in on
//     each hop and tunneling the next hop through the previous one.
//   - Helper (helper.go): one background process per user that holds the
//     authenticated connections and shares them with every Labmill process
//     (TUI, GUI, Agent) over a local socket. It speaks plain SSH on that
//     socket: an in-process SSH server whose channels (exec, subsystem sftp,
//     direct-tcpip) are proxied 1:1 onto the upstream connection. When the
//     helper must log in upstream, it relays the upstream keyboard-interactive
//     challenges back to the connecting Labmill process through the local
//     server's own keyboard-interactive auth, so all prompts appear in the
//     foreground UI and the helper itself needs no UI. It exits after 10
//     idle minutes, like ControlPersist.
package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"labmill-client/internal/sshconfig"
)

// QuestionKind classifies what the login dialog is asking.
type QuestionKind int

const (
	AskUsername QuestionKind = iota
	AskPassword              // secret; never echoed, never stored
	AskPasscode              // a 2FA passcode the user chose to type
	AskHostKey               // confirm an unknown host key (answer "yes" to trust)
	AskOther                 // any other server prompt, shown verbatim

	// The key kinds belong to the SSH-key sign-in and the key setup of
	// key-only hosts (see KeyGuide).
	AskKeyPassphrase // secret; the passphrase of an existing private key (empty skips the key)
	AskKeyCreate     // yes/no: create a new SSH key for this host (answer "yes" to create)
	AskNewPassphrase // secret; optional passphrase for a key being created (empty means none)
	AskKeyUpload     // Prompt: what to do; Data: the public key line; answer "yes" once it is uploaded
	AskKeyCopy       // yes/no: copy a private key from the home cluster to this computer (Prompt names the file, source, destination and the risk)
)

// Question is one prompt for the user.
type Question struct {
	Kind   QuestionKind
	Host   string // the host being logged in to, e.g. "login.example.edu"
	Prompt string // text to show (server prompt, or the host-key fingerprint message)
	Echo   bool   // whether typed input may be shown
	// Data is a machine-readable value that goes with Prompt and that the UI
	// may offer to copy: for AskKeyUpload the public key line. It is not
	// carried through the helper relay (the key setup runs in the client).
	Data string
}

// Prompter is the UI side of the login dialog. The TUI implements it with Huh
// forms; the helper implements it by relaying to the connecting client.
type Prompter interface {
	// Ask shows q and returns the user's answer. A canceled dialog returns an
	// error, which aborts the login. Implementations must return promptly
	// (with an error) when ctx is canceled: Login cancels it when it gives up
	// (push timeout, caller cancel) and relies on that to finish.
	Ask(ctx context.Context, q Question) (string, error)
	// Notify shows a status line, e.g. "Approve the sign-in request on your phone".
	// An empty msg clears the line: Login/Connect send it when a sign-in
	// wait ends, however it ended.
	Notify(msg string)
}

// LoginOptions tunes Login.
type LoginOptions struct {
	// User is the account name if already known (from ssh config); otherwise
	// the user is asked (AskUsername).
	User string
	// PushTimeout bounds how long Login waits for the 2FA push to be approved
	// after answering the Duo menu, or after the password on servers that push
	// without a menu. Zero means 60s.
	PushTimeout time.Duration
	// TwoFactorHelp is site-specific text (with a link) explaining how to set
	// up 2FA, shown via ErrTwoFactor when no push arrives or the server says
	// the user isn't enrolled.
	TwoFactorHelp string
	// TwoFactorName is the site's 2FA product name for user-facing text
	// (e.g. "Duo"). Empty means generic wording ("sign-in request").
	TwoFactorName string
	// PreferPasscode answers the Duo menu by asking the user for a passcode
	// (AskPasscode) instead of sending a push.
	PreferPasscode bool
	// KnownHosts is the known_hosts file; "" means <SSHDir>/known_hosts.
	KnownHosts string
	// Signers are optional public-key signers tried before keyboard-interactive
	// (e.g. from the ssh-agent or IdentityFile). Nil means none.
	Signers []ssh.Signer
	// KeyOnlyHosts are host names (as in the ssh config's HostName) of
	// servers that accept SSH keys only. Dial marks the hops whose host is
	// listed as KeyOnly. Names compare case-insensitively.
	KeyOnlyHosts []string
	// Hosts holds per-host overrides of TwoFactorHelp and TwoFactorName,
	// keyed by lowercase host name (as in the ssh config's HostName): a
	// session that reaches several institutions shows each host's own 2FA
	// text. Nil means every host uses the fields above.
	Hosts map[string]HostLogin
	// KeyOnly says the server being logged in to accepts SSH keys only, so a
	// rejection is reported as ErrKeyRejected (or ErrNoKey when no key was
	// offered) instead of "check the username and password", and Dial asks
	// for the passphrase of an encrypted key (AskKeyPassphrase). Dial sets it
	// from KeyOnlyHosts.
	KeyOnly bool
}

// HostLogin is one host's own 2FA text (LoginOptions.Hosts).
type HostLogin struct {
	TwoFactorHelp string
	TwoFactorName string
}

// challengeHook, when set by a test, runs in every keyboard-interactive
// challenge callback right after it is counted and before it disarms the push
// timer, to simulate a slow callback.
var challengeHook func(seq int64)

// PasswordCareNote is added to every password rejection that is not a 2FA
// failure, so nobody retries blindly into an account lockout.
const PasswordCareNote = "Check your password before trying again: many institutions lock the account after repeated failed sign-ins."

// deniedError is the login failure when the server accepted none of the
// methods and no 2FA step was involved. When a password was sent it wraps
// ErrPasswordRejected and carries the site's sign-in help and PasswordCareNote;
// otherwise (no password was ever asked for) it is a plain ErrAuthDenied with
// neutral text, since the password is not the suspect.
func deniedError(addr string, opts LoginOptions, passwordSent bool, detail string) error {
	if !passwordSent {
		msg := "the server accepted none of the offered sign-in methods"
		if detail != "" {
			msg += "; " + detail
		} else {
			msg += "; check the username"
		}
		return fmt.Errorf("ssh login to %s: %w (%s)", addr, ErrAuthDenied, msg)
	}
	if detail == "" {
		detail = "check the username and password"
	}
	s := ""
	if opts.TwoFactorHelp != "" {
		s += "\n" + opts.TwoFactorHelp
	}
	return fmt.Errorf("ssh login to %s: %w (%s)%s\n%s", addr, ErrPasswordRejected, detail, s, PasswordCareNote)
}

// ErrTwoFactor reports that the 2FA step failed: no push approval within
// PushTimeout, or the server said the account isn't enrolled. Its message
// includes LoginOptions.TwoFactorHelp. Test with errors.Is.
var ErrTwoFactor = errors.New("two-factor authentication did not complete")

// ErrAuthDenied reports that the server rejected the credentials (wrong
// username or password) without a 2FA push having been sent. Test with
// errors.Is.
var ErrAuthDenied = errors.New("authentication denied")

// passwordRejectedErr is the type of ErrPasswordRejected; it prints like
// ErrAuthDenied and unwraps to it.
type passwordRejectedErr struct{}

func (passwordRejectedErr) Error() string { return ErrAuthDenied.Error() }
func (passwordRejectedErr) Unwrap() error { return ErrAuthDenied }

// ErrPasswordRejected is the ErrAuthDenied case where a password was sent and
// the server refused it (no 2FA step). errors.Is(err, ErrAuthDenied) holds
// too. Callers use it to avoid retrying a password (account lockout).
var ErrPasswordRejected error = passwordRejectedErr{}

// ErrKeyRejected reports that a key-only server (LoginOptions.KeyOnly)
// accepted none of the keys offered. Test with errors.Is.
var ErrKeyRejected = errors.New("the server did not accept your SSH key")

// ErrNoKey reports that a key-only server (LoginOptions.KeyOnly) was reached
// while there was no usable SSH key to offer. Test with errors.Is.
var ErrNoKey = errors.New("no usable SSH key to offer")

// ErrHostKeyMismatch reports a host whose key differs from known_hosts. This
// is never overridable from the dialog: the user must fix known_hosts.
var ErrHostKeyMismatch = errors.New("host key does not match known_hosts")

// IsDuoMenu reports whether a keyboard-interactive question is a Duo-style 2FA
// menu (e.g. "Passcode or option (1-3): "), case-insensitive.
func IsDuoMenu(question string) bool {
	return strings.Contains(strings.ToLower(question), "passcode or option")
}

// IsNotEnrolled reports whether a server instruction/question says the account
// has no 2FA enrollment (e.g. contains "enroll" together with "not" or a URL).
func IsNotEnrolled(text string) bool {
	l := strings.ToLower(text)
	if !strings.Contains(l, "enroll") {
		return false
	}
	return strings.Contains(l, "not") || strings.Contains(l, "http://") || strings.Contains(l, "https://")
}

// Login connects to addr ("host:port") over conn (already-dialed transport,
// e.g. TCP, or a channel from a previous hop) and authenticates as described
// in the package doc: Signers first (if any), then keyboard-interactive; the
// plain "password" method is used only when the server does not offer
// keyboard-interactive (so a wrong password is not counted twice toward an
// account lockout). Keyboard-interactive answers: hidden prompts mentioning
// "password" → AskPassword (other hidden prompts → AskOther without echo); a
// Duo menu → the number of its "Duo Push" option, read from the menu text,
// plus Notify (AskPasscode when PreferPasscode or when the menu has no push
// option); not-enrolled text → abort with ErrTwoFactor; anything else →
// AskOther. The password is asked at most once per Login and reused if the
// server re-asks in the same login. Servers that push without a menu are
// handled too: after the password round, or on a zero-question round that
// mentions "push" or "duo", Login tells the user to approve the request and
// waits up to PushTimeout (each new push restarts the wait). Host keys: the
// algorithms of keys already in known_hosts are offered first, so a server
// with several key types presents one that can be verified; known → verify;
// unknown → AskHostKey with the SHA256 fingerprint, "yes" appends an
// OpenSSH-format line to known_hosts; mismatch → ErrHostKeyMismatch. When the
// known key is RSA only the rsa-sha2-256/512 signatures are offered, never
// plain ssh-rsa (SHA-1), matching OpenSSH 8.8+. The push wait measures only
// time spent waiting for the server: it pauses while the user answers a
// later prompt (passcode, TOTP). If the server rejects the login after a push
// was sent, the error wraps ErrTwoFactor and says the push may have been denied
// or timed out. The remote address is normalized (a non-TCP conn such as a
// net.Pipe is checked against addr), so known_hosts works over any transport.
// When Login gives up, any open dialog is canceled through its context.
//
// Limitation: a server that pushes without any menu (no Duo menu question)
// gives no sign that a push was sent beyond the round that mentions it, and
// when such a server rejects the login after the password the failure is
// reported generically (ErrAuthDenied unless a push round was seen), because
// a denied push cannot be told apart from a wrong password.
//
// With PreferPasscode the password is checked in the same round as the
// passcode, so a wrong password can surface as "passcode rejected".
func Login(ctx context.Context, conn net.Conn, addr string, pr Prompter, opts LoginOptions) (*ssh.Client, error) {
	return login(ctx, conn, addr, pr, opts, nil)
}

// login is Login. keyboardInteractiveRan, when not nil, is set to whether the
// server ever sent a keyboard-interactive challenge, so a caller can tell a
// disconnect before 2FA (too many failed keys) from one after it.
func login(ctx context.Context, conn net.Conn, addr string, pr Prompter, opts LoginOptions, keyboardInteractiveRan *bool) (*ssh.Client, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A sign-in notice ("Approve the … on your phone") is cleared with an
	// empty Notify on every way out of Login (success, failure, timeout,
	// cancel), so no front end keeps showing it after the wait is over.
	var noticeShown atomic.Bool
	notifyWait := func(msg string) {
		noticeShown.Store(true)
		pr.Notify(msg)
	}
	defer func() {
		if noticeShown.Load() {
			pr.Notify("")
		}
	}()
	fail := func(err error) (*ssh.Client, error) {
		conn.Close()
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ask := func(kind QuestionKind, prompt string, echo bool) (string, error) {
		return pr.Ask(ctx, Question{Kind: kind, Host: host, Prompt: prompt, Echo: echo})
	}
	user := opts.User
	if user == "" {
		if user, err = ask(AskUsername, "Username for "+host+":", true); err != nil {
			return fail(err)
		}
	}
	khPath := opts.KnownHosts
	if khPath == "" {
		paths, err := sshconfig.DefaultPaths()
		if err != nil {
			return fail(err)
		}
		khPath = filepath.Join(paths.SSHDir, "known_hosts")
	}
	timeout := opts.PushTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	// cause records the real reason a handshake failed; x/crypto wraps or
	// replaces errors from callbacks, so prefer ours.
	var causeMu sync.Mutex
	var cause error
	setCause := func(err error) error {
		causeMu.Lock()
		defer causeMu.Unlock()
		if cause == nil {
			cause = err
		}
		return err
	}
	// sig tells the main loop to arm (true) or disarm (false) the push timer.
	// The timer only runs while Login waits for the server: every challenge
	// callback disarms it first and re-arms it when it returns in a waiting
	// state, so the user's time at a dialog never counts.
	type sigMsg struct {
		arm bool
		seq int64 // the challenge callback that sent it
	}
	sig := make(chan sigMsg, 16)
	signal := func(arm bool, seq int64) {
		select {
		case sig <- sigMsg{arm, seq}:
		case <-ctx.Done():
		}
	}
	// challenges counts challenge callbacks, incremented as the callback's very
	// first step. When the push timer expires, a count different from the one
	// that armed it means a newer challenge has arrived whose disarm signal is
	// not queued yet, so the expiry is not a real silence from the server.
	var challenges atomic.Int64
	remote := normalizeRemote(addr, conn.RemoteAddr())
	var password string
	havePassword := false
	getPassword := func() (string, error) {
		if havePassword {
			return password, nil
		}
		p, err := ask(AskPassword, "Password for "+user+"@"+host+":", false)
		if err != nil {
			return "", setCause(err)
		}
		password, havePassword = p, true
		return p, nil
	}
	tfErr := func(why string) error {
		msg := why
		if opts.TwoFactorHelp != "" {
			msg += "\n" + opts.TwoFactorHelp
		}
		return fmt.Errorf("%w: %s", ErrTwoFactor, msg)
	}
	twoFactor := func(why string) error { return setCause(tfErr(why)) }
	const pushRejected = "the login was rejected after the 2FA push was sent; the push may have been denied or timed out"
	// pushSent records that the user was actually asked to approve a push
	// (menu answered with the push option, or an informational push round).
	var kiAttempted, waitNotified, pushSent atomic.Bool
	// passcodeSent records that a passcode (or other typed answer) went to the
	// server after the password, so a rejection is a 2FA failure.
	var passcodeSent atomic.Bool
	const passcodeRejected = "the passcode was rejected"
	challenge := func(_, instruction string, questions []string, echos []bool) ([]string, error) {
		seq := challenges.Add(1)
		if challengeHook != nil {
			challengeHook(seq)
		}
		signal(false, seq)
		kiAttempted.Store(true)
		if IsNotEnrolled(instruction) {
			return nil, twoFactor("the server says this account is not enrolled")
		}
		answers := make([]string, len(questions))
		answeredPassword, waiting := false, false
		for i, q := range questions {
			echo := i < len(echos) && echos[i]
			// OpenSSH+PAM servers put the menu text inside the question and
			// leave the instruction empty, so look at both.
			combined := instruction + "\n" + q
			switch {
			case IsNotEnrolled(combined):
				return nil, twoFactor("the server says this account is not enrolled")
			case IsDuoMenu(q):
				opt, ok := duoPushOption(combined)
				if opts.PreferPasscode || !ok {
					a, err := ask(AskPasscode, nameOr(opts.TwoFactorName, "Passcode:", "%s passcode:"), true)
					if err != nil {
						return nil, setCause(err)
					}
					answers[i] = a
					if havePassword {
						passcodeSent.Store(true)
					}
					break
				}
				answers[i] = opt
				notifyWait("Approve the " + SignInRequest(opts.TwoFactorName) + " for " + shortHost(host) + " on your phone\u2026")
				waiting = true
				pushSent.Store(true)
			case !echo && strings.Contains(strings.ToLower(q), "password"):
				p, err := getPassword()
				if err != nil {
					return nil, err
				}
				answers[i] = p
				answeredPassword = true
			default:
				a, err := ask(AskOther, q, echo)
				if err != nil {
					return nil, setCause(err)
				}
				answers[i] = a
				if havePassword {
					passcodeSent.Store(true)
				}
			}
		}
		// Some servers push without any menu: either right after the password,
		// or with an informational round that says so.
		if len(questions) == 0 {
			l := strings.ToLower(instruction)
			if strings.Contains(l, "push") || strings.Contains(l, "duo") {
				waiting = true
				pushSent.Store(true)
			}
		}
		if answeredPassword {
			waiting = true
		}
		if waiting {
			if !waitNotified.Swap(true) {
				notifyWait(waitNotice(opts.TwoFactorName))
			}
			signal(true, seq)
		}
		return answers, nil
	}
	passwordMethod := func() (string, error) {
		if kiAttempted.Load() {
			// Don't send the same password a second time to a server that
			// already rejected it via keyboard-interactive.
			if passcodeSent.Load() {
				return "", setCause(tfErr(passcodeRejected))
			}
			if pushSent.Load() {
				return "", setCause(tfErr(pushRejected))
			}
			return "", setCause(deniedError(addr, opts, havePassword, ""))
		}
		return getPassword()
	}

	hostKey := hostKeyCheck(khPath, host, remote, func(prompt string) (string, error) { return ask(AskHostKey, prompt, true) }, setCause)

	var methods []ssh.AuthMethod
	if len(opts.Signers) > 0 {
		methods = append(methods, ssh.PublicKeys(opts.Signers...))
	}
	methods = append(methods,
		ssh.KeyboardInteractive(challenge),
		ssh.PasswordCallback(passwordMethod))
	algos, err := knownHostAlgorithms(khPath, addr, remote)
	if err != nil {
		return fail(err)
	}
	cfg := &ssh.ClientConfig{User: user, Auth: methods, HostKeyCallback: hostKey, HostKeyAlgorithms: algos}

	type result struct {
		client *ssh.Client
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
		if err != nil {
			resCh <- result{err: err}
			return
		}
		resCh <- result{client: ssh.NewClient(c, chans, reqs)}
	}()
	// abandon returns without waiting for the handshake goroutine, but makes
	// sure a late success doesn't leak a connection.
	abandon := func(err error) (*ssh.Client, error) {
		conn.Close()
		go func() {
			if r := <-resCh; r.client != nil {
				r.client.Close()
			}
		}()
		return nil, setCause(err)
	}
	var timer *time.Timer
	var timeoutC <-chan time.Time
	var armedSeq int64 // the challenge that armed the timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	apply := func(m sigMsg) {
		arm := m.arm
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		if !arm {
			timeoutC = nil
			return
		}
		if timer == nil {
			timer = time.NewTimer(timeout)
		} else {
			timer.Reset(timeout)
		}
		timeoutC = timer.C
		armedSeq = m.seq
	}
	for {
		select {
		case r := <-resCh:
			if r.err != nil {
				conn.Close()
				if keyboardInteractiveRan != nil {
					*keyboardInteractiveRan = kiAttempted.Load()
				}
				causeMu.Lock()
				defer causeMu.Unlock()
				if cause != nil {
					return nil, cause
				}
				if passcodeSent.Load() {
					return nil, fmt.Errorf("ssh login to %s: %w (%v)", addr, tfErr(passcodeRejected), r.err)
				}
				if pushSent.Load() {
					return nil, fmt.Errorf("ssh login to %s: %w (%v)", addr, tfErr(pushRejected), r.err)
				}
				if strings.Contains(r.err.Error(), "unable to authenticate") {
					if opts.KeyOnly && !kiAttempted.Load() {
						if len(opts.Signers) == 0 {
							return nil, fmt.Errorf("ssh login to %s: %w: the server accepts only SSH keys (%v)", addr, ErrNoKey, r.err)
						}
						return nil, fmt.Errorf("ssh login to %s: %w: it accepts only SSH keys, so the public key must be registered with the site first (%v)", addr, ErrKeyRejected, r.err)
					}
					return nil, deniedError(addr, opts, havePassword, r.err.Error())
				}
				return nil, fmt.Errorf("ssh login to %s: %w", addr, r.err)
			}
			return r.client, nil
		case m := <-sig:
			apply(m)
		case <-timeoutC:
			// A disarm may be queued behind the expiry (a dialog just opened):
			// honor pending signals before giving up.
			drained := false
		drain:
			for {
				select {
				case m := <-sig:
					apply(m)
					drained = true
				default:
					break drain
				}
			}
			if drained {
				continue
			}
			// A newer challenge callback has started but its disarm signal has
			// not arrived: the server is not silent, so don't give up. The
			// callback re-arms the timer if it ends up waiting.
			if challenges.Load() != armedSeq {
				timeoutC = nil
				continue
			}
			_, err := abandon(tfErr(fmt.Sprintf("no approval within %s", timeout)))
			return nil, err
		case <-ctx.Done():
			_, err := abandon(ctx.Err())
			return nil, err
		}
	}
}

// hostKeyCheck returns the host key callback shared by Login and QueryKeys:
// it checks the key against the known_hosts file khPath (host is the name
// shown to the user, remote the connection's address), refuses a changed key
// (ErrHostKeyMismatch) and asks through ask about an unknown one, appending
// the line to khPath when the user agrees. setCause records the real reason
// for a failure.
func hostKeyCheck(khPath, host string, remote net.Addr, ask func(prompt string) (string, error), setCause func(error) error) ssh.HostKeyCallback {
	return func(hostname string, _ net.Addr, key ssh.PublicKey) error {
		var cb ssh.HostKeyCallback
		if _, err := os.Stat(khPath); err == nil {
			if cb, err = knownhosts.New(khPath); err != nil {
				return setCause(fmt.Errorf("reading %s: %w", khPath, err))
			}
		} else if !os.IsNotExist(err) {
			return setCause(err)
		}
		var kerr *knownhosts.KeyError
		if cb != nil {
			err := cb(hostname, remote, key)
			if err == nil {
				return nil
			}
			if !errors.As(err, &kerr) {
				return setCause(err)
			}
			if len(kerr.Want) > 0 {
				return setCause(fmt.Errorf("%w: the identity of %s changed unexpectedly, so Labmill will not connect. This can mean someone is intercepting the connection. Do not continue; contact your HPC support (the saved identity is in %s)", ErrHostKeyMismatch, host, khPath))
			}
		}
		prompt := fmt.Sprintf("This is the first time you connect to %s, so Labmill cannot yet check that it is the real server.\nIts identity fingerprint is %s.\nIf your HPC center publishes this fingerprint, compare it. Otherwise it is normally safe to continue the first time.\nContinue and remember this server? (yes/no)",
			host, ssh.FingerprintSHA256(key))
		a, err := ask(prompt)
		if err != nil {
			return setCause(err)
		}
		if a = strings.ToLower(strings.TrimSpace(a)); a != "yes" && a != "y" {
			return setCause(errors.New("host key not trusted"))
		}
		if err := appendKnownHost(khPath, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)); err != nil {
			return setCause(err)
		}
		return nil
	}
}

// shortHost trims a DNS name to what a person recognizes in a notice: the
// last two labels (domain and TLD) are dropped from names with three or more
// labels ("files.login.example.edu" becomes "files.login"). IP addresses and
// short names are returned as they are.
func shortHost(host string) string {
	if net.ParseIP(host) != nil {
		return host
	}
	labels := strings.Split(host, ".")
	if len(labels) < 3 {
		return host
	}
	return strings.Join(labels[:len(labels)-2], ".")
}

// dialHop opens the transport to addr: TCP for the first hop, else a channel
// through the previous hop's client.
func dialHop(ctx context.Context, hops []*ssh.Client, addr string) (net.Conn, error) {
	if len(hops) == 0 {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	return hops[len(hops)-1].DialContext(ctx, "tcp", addr)
}

// tooManyAuthFailures reports whether err is the server disconnecting because
// the client used up its authentication attempts.
func tooManyAuthFailures(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "too many authentication failures")
}

// normalizeRemote returns a remote address whose String() is "host:port", as
// knownhosts requires: the conn's own address when it has that form, else one
// derived from addr (nil, net.Pipe, a tunneled channel conn, ...).
func normalizeRemote(addr string, remote net.Addr) net.Addr {
	if remote != nil {
		if r, ok := remote.(*net.TCPAddr); !ok || r != nil {
			if _, _, err := net.SplitHostPort(remote.String()); err == nil {
				return remote
			}
		}
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return &net.TCPAddr{}
	}
	port, _ := strconv.Atoi(portStr)
	return &net.TCPAddr{IP: net.ParseIP(host), Port: port}
}

// knownHostAlgorithms returns the host key algorithms to offer for addr:
// those of the keys known_hosts already holds for it first (so the server
// presents a key we can verify, as OpenSSH does), then every other plain-key
// algorithm. It returns nil (Go's default order) when nothing is known.
// Hashed (|1|) and [host]:port entries are matched by knownhosts itself: a
// random probe key never matches, so the KeyError lists every known key.
func knownHostAlgorithms(khPath, addr string, remote net.Addr) ([]string, error) {
	cb, err := knownhosts.New(khPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", khPath, err)
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	probe, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	remote = normalizeRemote(addr, remote)
	var kerr *knownhosts.KeyError
	if !errors.As(cb(addr, remote, probe), &kerr) || len(kerr.Want) == 0 {
		return nil, nil
	}
	var out []string
	add := func(a string) {
		for _, o := range out {
			if o == a {
				return
			}
		}
		out = append(out, a)
	}
	for _, k := range kerr.Want {
		if t := k.Key.Type(); t == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA512)
			add(ssh.KeyAlgoRSASHA256)
		} else {
			add(t)
		}
	}
	for _, a := range ssh.SupportedAlgorithms().HostKeys {
		if !strings.Contains(a, "-cert-") {
			add(a)
		}
	}
	return out, nil
}

var (
	duoPushRE    = regexp.MustCompile(`(?i)(\d+)\.\s*Duo Push`)
	menuOptionRE = regexp.MustCompile(`(?m)^\s*\d+\.\s*\S`)
)

// duoPushOption finds the menu number of "Duo Push" in a Duo menu's
// instruction text. When the text lists numbered options but none is a push,
// ok is false (the caller asks for a passcode); when it lists no options at
// all (unparseable), it falls back to "1".
func duoPushOption(instruction string) (opt string, ok bool) {
	if m := duoPushRE.FindStringSubmatch(instruction); m != nil {
		return m[1], true
	}
	return "1", !menuOptionRE.MatchString(instruction)
}

// appendKnownHost appends line to the known_hosts file, creating it (0600) and
// its directory (0700) if needed. A missing final newline in the existing file
// is added first so the new entry starts on its own line.
func appendKnownHost(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	prefix := ""
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], fi.Size()-1); err == nil && last[0] != '\n' {
			prefix = "\n"
		}
	}
	if _, err := fmt.Fprintln(f, prefix+line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Dial resolves alias with sshconfig.Lookup, follows its ProxyJump chain
// (comma-separated hops, max depth 8, no cycles), and logs in on every hop
// with Login, tunneling each next hop through the previous client
// (client.DialContext("tcp", host:port) + ssh.NewClientConn). As in OpenSSH,
// only the first hop of a ProxyJump list is itself resolved through its own
// ProxyJump; later hops are reached from the hop before them. On every hop,
// the keys of all the host's IdentityFile lines are offered in config order
// (after opts.Signers), skipping files that are missing or encrypted, except
// on a key-only host (LoginOptions.KeyOnlyHosts), where the passphrase of an
// encrypted key is asked for (AskKeyPassphrase) and empty skips the key.
// On a key-only host only: when the host has no IdentityFile line, the
// default keys ~/.ssh/id_ed25519, id_ecdsa and id_rsa are tried instead
// (those that exist, are private to the user and can be read), keys held by
// the SSH agent follow (see agentSigners), and an encrypted key file whose
// .pub is in the agent is skipped without a passphrase question. Password
// hosts are offered IdentityFile keys only. At most
// maxOfferedKeys keys are offered per hop (opts.Signers first, then the files
// in order); the rest are dropped silently. Every refused key counts as a
// failed attempt against the server's MaxAuthTries (default 6), so offering
// many keys could use up the tries before keyboard-interactive (password and
// 2FA) gets its turn. OpenSSH has the same limit but sends only the keys the
// user lists; Labmill may be pointed at a config with many. The
// returned client is for the final host; closing it closes the intermediate
// hops too.
func Dial(ctx context.Context, p sshconfig.Paths, alias string, pr Prompter, opts LoginOptions) (*ssh.Client, error) {
	hops, _, err := dialChain(ctx, p, alias, pr, opts, false)
	if err != nil {
		return nil, err
	}
	final := hops[len(hops)-1]
	if len(hops) > 1 {
		inter := hops[:len(hops)-1]
		go func() {
			final.Wait()
			for i := len(inter) - 1; i >= 0; i-- {
				inter[i].Close()
			}
		}()
	}
	return final, nil
}

// dialChain is Dial's hop loop: it logs in to every hop of alias's ProxyJump
// chain and, unless skipFinal, to alias itself. It returns the clients in
// order and the last hop of the whole chain (alias itself, even with
// skipFinal, which then leaves it for the caller to connect to).
func dialChain(ctx context.Context, p sshconfig.Paths, alias string, pr Prompter, opts LoginOptions, skipFinal bool) ([]*ssh.Client, hopSpec, error) {
	var chain []hopSpec
	if err := flatten(p, hopSpec{name: alias}, nil, &chain); err != nil {
		return nil, hopSpec{}, err
	}
	chain = append(chain, hopSpec{name: alias})
	lastSpec := chain[len(chain)-1]
	if skipFinal {
		chain = chain[:len(chain)-1]
	}
	if opts.KnownHosts == "" {
		opts.KnownHosts = filepath.Join(p.SSHDir, "known_hosts")
	}
	var hops []*ssh.Client
	closeAll := func() {
		for i := len(hops) - 1; i >= 0; i-- {
			hops[i].Close()
		}
	}
	for _, h := range chain {
		cfg, _, err := sshconfig.Lookup(p, h.name)
		if err != nil {
			closeAll()
			return nil, hopSpec{}, err
		}
		port := cfg.Port
		if h.port != 0 {
			port = h.port
		}
		addr := net.JoinHostPort(cfg.HostName, strconv.Itoa(port))
		o := opts
		o.KeyOnly = opts.KeyOnly || hostListed(cfg.HostName, opts.KeyOnlyHosts)
		if hl, ok := opts.Hosts[strings.ToLower(cfg.HostName)]; ok {
			o.TwoFactorHelp, o.TwoFactorName = hl.TwoFactorHelp, hl.TwoFactorName
		}
		var askPass func(shown string) (string, error)
		if o.KeyOnly {
			askPass = func(shown string) (string, error) {
				return pr.Ask(ctx, Question{Kind: AskKeyPassphrase, Host: cfg.HostName,
					Prompt: "Passphrase for the SSH key " + shown + " (leave empty to skip this key):"})
			}
		}
		// Default key files and the agent's keys are offered to key-only
		// hosts only: a password host would see every key (and could count
		// them against MaxAuthTries) for nothing.
		var agentSigs []ssh.Signer
		closeAgent := func() {}
		lo := loadOpts{}
		if o.KeyOnly {
			agentSigs, closeAgent = agentSigners(ctx)
			lo.defaults = true
			lo.agent = map[string]bool{}
			for _, a := range agentSigs {
				lo.agent[ssh.FingerprintSHA256(a.PublicKey())] = true
			}
		}
		fileSigners, err := identityLoad(p, cfg, opts.Signers, pr.Notify, askPass, lo)
		if err != nil {
			closeAgent()
			closeAll()
			return nil, hopSpec{}, err
		}
		sigs := append(slices.Clone(opts.Signers), fileSigners...)
		sigs = append(sigs, uniqueSigners(agentSigs, sigs)...)
		o.Signers = capSigners(sigs)
		switch {
		case h.user != "":
			o.User = h.user
		case cfg.User != "":
			o.User = cfg.User
		}
		conn, err := dialHop(ctx, hops, addr)
		if err != nil {
			closeAgent()
			closeAll()
			return nil, hopSpec{}, fmt.Errorf("connecting to %s: %w", addr, err)
		}
		var kiRan bool
		c, err := login(ctx, conn, addr, pr, o, &kiRan)
		if err != nil && o.KeyOnly && tooManyAuthFailures(err) {
			err = fmt.Errorf("ssh login to %s: %w: the server stopped after too many keys were offered; name the right key with IdentityFile and IdentitiesOnly yes in the ssh config, or remove unused keys from the agent (%v)", addr, ErrKeyRejected, err)
		} else if err != nil && len(o.Signers) > 0 && !kiRan && tooManyAuthFailures(err) {
			// The server gave up on the keys before password + 2FA got a turn
			// (a low MaxAuthTries): dial this hop again without any key.
			o.Signers = nil
			if conn, err = dialHop(ctx, hops, addr); err != nil {
				closeAgent()
				closeAll()
				return nil, hopSpec{}, fmt.Errorf("connecting to %s: %w", addr, err)
			}
			c, err = Login(ctx, conn, addr, pr, o)
		}
		closeAgent() // signing happens during the handshake only
		if err != nil {
			closeAll()
			return nil, hopSpec{}, err
		}
		hops = append(hops, c)
	}
	return hops, lastSpec, nil
}

// identitySigners loads the private keys named by cfg's IdentityFile lines,
// in config order, as OpenSSH tries them. (Default key files are offered only
// on key-only hosts, by the key-only path in Dial.) Files that are missing,
// unreadable, not a private key, or passphrase-protected are skipped
// silently: the password and 2FA login follows anyway. On non-Windows systems
// a key file that is accessible to group or others (mode&0o077 != 0) is
// skipped too, as OpenSSH refuses an "UNPROTECTED PRIVATE KEY FILE" (noted
// once through the Prompter by Dial). Keys are de-duplicated by public-key
// fingerprint, among the files and against existing (the caller's own
// signers). A leading "~/" and "%d" mean the home directory, taken as the
// parent of p.SSHDir.
func identitySigners(p sshconfig.Paths, cfg sshconfig.Host, existing []ssh.Signer) []ssh.Signer {
	return identitySignersNote(p, cfg, existing, nil)
}

// notedLooseKeys remembers the key files already reported as too open, so the
// notice appears once per process (the helper's life, or one Labmill run).
var notedLooseKeys sync.Map

// identitySignersNote is identitySigners that also calls note (when not nil)
// with a one-line notice, once per file per process, for each configured key
// skipped because its permissions are too open (e.g. keys on a WSL /mnt/c
// drive, which show 0777): without it the user would never learn why the key
// wasn't used.
func identitySignersNote(p sshconfig.Paths, cfg sshconfig.Host, existing []ssh.Signer, note func(string)) []ssh.Signer {
	out, _ := identityLoad(p, cfg, existing, note, nil, loadOpts{})
	return out
}

// defaultKeyNames are the private key file names tried in p.SSHDir when a host
// names no IdentityFile, in the order they are offered.
var defaultKeyNames = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// expandKeyPath expands a leading "~" and "%d" in an IdentityFile value to
// the home directory home.
func expandKeyPath(f, home string) string {
	switch {
	case f == "~":
		f = home
	case strings.HasPrefix(f, "~/"):
		f = filepath.Join(home, f[2:])
	}
	return strings.ReplaceAll(f, "%d", home)
}

// keyFileIsPrivate reports whether the key file f may be used: always on
// Windows (OpenSSH checks ACLs there, which Labmill doesn't), else only when
// neither group nor others have any access. A file that can't be statted is
// reported usable (reading it fails later).
func keyFileIsPrivate(f string) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	fi, err := os.Stat(f)
	return err != nil || fi.Mode().Perm()&0o077 == 0
}

// loadOpts tunes identityLoad.
type loadOpts struct {
	// defaults adds the default key files when the host has no IdentityFile.
	defaults bool
	// agent holds the fingerprints of the agent's keys; an encrypted key
	// file whose .pub fingerprint is among them is skipped without a
	// passphrase question (nil: no agent information).
	agent map[string]bool
}

// agentHasPub reports whether the public key file pubFile (authorized_keys
// format) names a key whose fingerprint is in agent.
func agentHasPub(agent map[string]bool, pubFile string) bool {
	data, err := os.ReadFile(pubFile)
	if err != nil {
		return false
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey(data)
	return err == nil && agent[ssh.FingerprintSHA256(pk)]
}

// identityLoad is identitySignersNote with passphrase support: when askPass
// is not nil, the passphrase of an encrypted key is asked for with it (shown
// is the key's display name; the answer is used, never kept), at most three
// times per key, and an empty answer skips the key. An error from askPass
// (the dialog was canceled) aborts the load.
func identityLoad(p sshconfig.Paths, cfg sshconfig.Host, existing []ssh.Signer, note func(string), askPass func(shown string) (string, error), lo loadOpts) ([]ssh.Signer, error) {
	files := cfg.IdentityFiles
	if len(files) == 0 && cfg.IdentityFile != "" {
		files = []string{cfg.IdentityFile}
	}
	configured := len(files) > 0
	home := filepath.Dir(p.SSHDir)
	if !configured && lo.defaults {
		for _, n := range defaultKeyNames {
			files = append(files, filepath.Join(p.SSHDir, n))
		}
	}
	seen := map[string]bool{}
	for _, e := range existing {
		seen[ssh.FingerprintSHA256(e.PublicKey())] = true
	}
	display := func(f string) string {
		if rel, err := filepath.Rel(home, f); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + filepath.ToSlash(rel)
		}
		return f
	}
	var out []ssh.Signer
	var loose []string // display names of newly skipped keys, noted after the loop
	for _, f := range files {
		f = expandKeyPath(f, home)
		if !keyFileIsPrivate(f) {
			if note != nil && configured {
				if _, dup := notedLooseKeys.Load(f); !dup {
					loose = append(loose, display(f))
					notedLooseKeys.Store(f, true)
				}
			}
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		sg, err := ssh.ParsePrivateKey(data)
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) && lo.agent != nil && agentHasPub(lo.agent, f+".pub") {
			continue // the agent holds this key: no passphrase needed
		}
		if errors.As(err, &missing) && askPass != nil {
			sg = nil
			for try := 0; try < 3; try++ {
				pass, aerr := askPass(display(f))
				if aerr != nil {
					return nil, aerr
				}
				if pass == "" {
					break
				}
				if sg, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(pass)); err == nil {
					break
				}
				sg = nil
			}
			if sg == nil {
				continue
			}
		} else if err != nil {
			continue
		}
		fp := ssh.FingerprintSHA256(sg.PublicKey())
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, sg)
	}
	for _, shown := range loose {
		if len(out) == 0 && len(existing) == 0 {
			note("Skipped key " + shown + ": permissions too open; using password and 2FA instead.")
		} else {
			note("Skipped key " + shown + ": permissions too open.")
		}
	}
	return out, nil
}

// uniqueSigners returns those of extra whose public key is not already among
// have.
func uniqueSigners(extra, have []ssh.Signer) []ssh.Signer {
	seen := map[string]bool{}
	for _, h := range have {
		seen[ssh.FingerprintSHA256(h.PublicKey())] = true
	}
	var out []ssh.Signer
	for _, e := range extra {
		if fp := ssh.FingerprintSHA256(e.PublicKey()); !seen[fp] {
			seen[fp] = true
			out = append(out, e)
		}
	}
	return out
}

// hostListed reports whether host is one of names (case-insensitive).
func hostListed(host string, names []string) bool {
	for _, n := range names {
		if strings.EqualFold(n, host) {
			return true
		}
	}
	return false
}

// maxOfferedKeys is the most public keys offered per hop; see Dial.
const maxOfferedKeys = 5

// capSigners keeps the first maxOfferedKeys signers.
func capSigners(s []ssh.Signer) []ssh.Signer {
	if len(s) > maxOfferedKeys {
		return s[:maxOfferedKeys]
	}
	return s
}

const maxJumpDepth = 8

// hopSpec is one ProxyJump entry: a host or alias with optional user/port
// overrides.
type hopSpec struct {
	name string
	user string
	port int
}

// parseJump splits a ProxyJump value into hops. "none" or "" yields no hops.
func parseJump(v string) ([]hopSpec, error) {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "none") {
		return nil, nil
	}
	var out []hopSpec
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(part), "ssh://"), "/")
		if part == "" {
			continue
		}
		var h hopSpec
		if i := strings.LastIndex(part, "@"); i >= 0 {
			h.user, part = part[:i], part[i+1:]
		}
		if i := strings.LastIndex(part, ":"); i >= 0 && !strings.HasSuffix(part, "]") && (strings.Count(part, ":") == 1 || strings.Contains(part, "]:")) {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 || n > 65535 {
				return nil, fmt.Errorf("invalid ProxyJump hop %q", part)
			}
			h.port, part = n, part[:i]
		}
		h.name = strings.Trim(part, "[]")
		if h.name == "" {
			return nil, fmt.Errorf("invalid ProxyJump %q", v)
		}
		out = append(out, h)
	}
	return out, nil
}

// flatten appends the hops needed to reach h (not h itself) to out, in
// connection order. Following OpenSSH, only the first hop of h's ProxyJump
// list is resolved through its own ProxyJump (recursively); later hops are
// reached from the hop before them.
func flatten(p sshconfig.Paths, h hopSpec, stack []string, out *[]hopSpec) error {
	if len(stack) >= maxJumpDepth {
		return fmt.Errorf("ProxyJump chain too deep (more than %d hops)", maxJumpDepth)
	}
	for _, s := range stack {
		if s == h.name {
			return fmt.Errorf("ProxyJump cycle: %s -> %s", strings.Join(stack, " -> "), h.name)
		}
	}
	cfg, _, err := sshconfig.Lookup(p, h.name)
	if err != nil {
		return err
	}
	jumps, err := parseJump(cfg.ProxyJump)
	if err != nil {
		return err
	}
	stack = append(append([]string(nil), stack...), h.name)
	for i, j := range jumps {
		if i == 0 {
			if err := flatten(p, j, stack, out); err != nil {
				return err
			}
		}
		*out = append(*out, j)
	}
	return nil
}

// nameOr returns generic when name is empty, else format filled with name.
func nameOr(name, generic, format string) string {
	if name == "" {
		return generic
	}
	return fmt.Sprintf(format, name)
}

// SignInRequest names what the user approves on their phone, for notices:
// "sign-in request" without a site 2FA product name, "<Name> push" with one.
func SignInRequest(name string) string {
	if name == "" {
		return "sign-in request"
	}
	return name + " push"
}

// isSignInNotice reports whether msg is one of the push-wait notices Login
// shows (as opposed to an informational one such as a created key).
func isSignInNotice(msg string) bool {
	return strings.HasPrefix(msg, "Approve the ") || strings.HasPrefix(msg, "If your phone shows")
}

// clearingPrompter forwards to a Prompter and remembers whether a sign-in
// notice went through, so Connect can clear it when it returns.
type clearingPrompter struct {
	Prompter
	shown atomic.Bool
}

func (c *clearingPrompter) Notify(msg string) {
	if msg == "" {
		c.shown.Store(false)
	} else if isSignInNotice(msg) {
		c.shown.Store(true)
	}
	c.Prompter.Notify(msg)
}

// waitNotice is the status line shown while the 2FA approval is pending.
func waitNotice(name string) string {
	if name == "" {
		return "If your phone shows a sign-in request, approve it."
	}
	return "If your phone shows a sign-in request from " + name + ", approve it."
}
