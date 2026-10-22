package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/textsafe"
)

// M45.2: before a key-only host (auth: key) asks the user to create and
// upload a new key, Labmill checks which keys the user already has are
// registered there: the key pairs in ~/.ssh on the home institution's login
// node, the laptop's ~/.ssh and the SSH agent. The check uses the
// public-key query of RFC 4252 §7, which needs only the public key: no
// private key is read, copied or used for it.

// ErrQueryUnsupported reports that the server answered a public-key query
// in a way that tells nothing about the key (for example it closed the
// connection before any query). Test with errors.Is.
var ErrQueryUnsupported = errors.New("the server did not answer the public-key query")

// RemoteKey is a public key found on a remote host. Only public data: the
// private key it belongs to is never read at this step.
type RemoteKey struct {
	// Name is the private key's file name in ~/.ssh on that host (the .pub
	// file's name without ".pub"), e.g. "id_ed25519".
	Name string
	// Type is the key type, e.g. "ssh-ed25519".
	Type string
	// Fingerprint is the SHA256 fingerprint ("SHA256:…").
	Fingerprint string
	// Comment is the .pub file's comment, cleaned for display (may be "").
	Comment string
	// PublicKey is the parsed key.
	PublicKey ssh.PublicKey

	// rawName is Name as the host sent it, for the command that reads the
	// file; Name is cleaned for display.
	rawName string
}

// ErrHostLimiting reports that the host closed the connection before the
// SSH handshake, which is how OpenSSH's per-source penalties (and similar
// rate limits) refuse a client after several failed logins. Waiting a
// minute and trying again usually works.
var ErrHostLimiting = errors.New("the server is refusing connections for a while after several tries")

// maxRemoteKeys bounds how many .pub files ListRemoteKeys returns and
// QueryKeys checks (servers count each rejected query toward MaxAuthTries,
// and every few keys cost another connection).
const maxRemoteKeys = 16

// maxKeysPerConn is how many keys QueryKeys offers on one connection: sshd's
// default MaxAuthTries is 6 and every rejected query counts.
const maxKeysPerConn = 5

// queryPause is the pause between two query connections, so that a server's
// per-source penalties (OpenSSH 9.8+, default 5 s per failed login) don't
// stack up: longer than that penalty. Tests shorten it.
var queryPause = 7 * time.Second

// ListRemoteKeys lists the public keys in ~/.ssh on the host client is
// connected to (the user's home institution login node) with one fixed,
// read-only command that prints each "*.pub" file's name and first line
// (POSIX sh given as one single-line, single-quoted word, so it works when
// the user's login shell is csh/tcsh, and when there are no .pub files). A .pub file that doesn't parse, or whose private key
// file is missing, is skipped. A .pub whose name contains a control
// character, or that is a symlink pointing outside ~/.ssh, is skipped. At most
// maxRemoteKeys are returned, sorted by Name. Nothing but .pub files is read.
func ListRemoteKeys(ctx context.Context, client *ssh.Client) ([]RemoteKey, error) {
	if client == nil {
		return nil, errors.New("listing remote keys: no connection")
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("listing remote keys: %w", err)
	}
	defer sess.Close()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	done := make(chan error, 1)
	go func() { done <- sess.Run("sh -c " + shQuote(listKeysScript)) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		sess.Close()
		return nil, fmt.Errorf("listing remote keys: %w", ctx.Err())
	}
	if err != nil {
		if msg := strings.TrimSpace(textsafe.Clean(errOut.String())); msg != "" {
			return nil, fmt.Errorf("listing remote keys: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("listing remote keys: %w", err)
	}
	return parseRemoteKeys(out.String()), nil
}

// QueryKeys asks the key-only host alias (resolved through p like Dial,
// including ProxyJump hops and known_hosts checks; an unknown host key is
// handled like Dial does, through pr) whether it accepts each of pubs for the
// alias's user, and returns one bool per key in the same order. opts is what
// Dial gets (KeyOnlyHosts, TwoFactorName, PreferPasscode, KnownHosts, ...); it
// applies to the jump hosts, which are logged in to like Dial does. With no
// keys it returns at once without connecting.
//
// It opens one connection per up to maxKeysPerConn keys (plus one more after
// each accepted key, which ends its connection) and authenticates with
// signers that expose the public keys but whose Sign never signs: it records
// that the server answered SSH_MSG_USERAUTH_PK_OK and returns errQueryStop,
// so the connection ends before login. A key the server rejects is false. No
// password, keyboard-interactive or 2FA method is offered, the agent and
// key files are not used, and nothing is written except a new known_hosts
// line the user accepted. Between two connections it pauses queryPause. ctx
// bounds the whole check. A connection that fails to dial or handshake
// returns the results gathered so far (keys not yet asked stay false) with
// that error (wrapped); when the host closes the connection before the
// handshake, as OpenSSH's per-source penalties do, the error wraps
// ErrHostLimiting. Partial results come back only for failures after the
// first query connection started; a failure before that (jump host login,
// ssh config lookup, the username question) returns nil and the error. A
// server that disconnects after fewer failed keys than maxKeysPerConn
// (MaxAuthTries) makes later connections offer fewer keys at once.
func QueryKeys(ctx context.Context, p sshconfig.Paths, alias string, pr Prompter, opts LoginOptions, pubs []ssh.PublicKey) ([]bool, error) {
	res, _, err := queryKeys(ctx, p, alias, pr, opts, pubs)
	return res, err
}

// queryKeys is QueryKeys that also returns how many of the leading pubs were
// asked (and so have a final answer in the result) when it returns: all
// checked ones on success, fewer after an error. The keys from asked on are
// still unknown.
func queryKeys(ctx context.Context, p sshconfig.Paths, alias string, pr Prompter, opts LoginOptions, pubs []ssh.PublicKey) ([]bool, int, error) {
	res := make([]bool, len(pubs))
	if len(pubs) == 0 {
		return res, 0, nil
	}
	checked := pubs
	if len(checked) > maxRemoteKeys {
		checked = checked[:maxRemoteKeys] // the rest stay false
	}
	// Jump hosts are logged in to like Dial does; only the final host is
	// queried instead.
	hops, last, err := dialChain(ctx, p, alias, pr, opts, true)
	if err != nil {
		return nil, 0, err
	}
	defer func() {
		for i := len(hops) - 1; i >= 0; i-- {
			hops[i].Close()
		}
	}()
	cfg, _, err := sshconfig.Lookup(p, alias)
	if err != nil {
		return nil, 0, err
	}
	port := cfg.Port
	if last.port != 0 {
		port = last.port
	}
	addr := net.JoinHostPort(cfg.HostName, strconv.Itoa(port))
	user := cfg.User
	if last.user != "" {
		user = last.user
	}
	if user == "" {
		if user, err = pr.Ask(ctx, Question{Kind: AskUsername, Host: cfg.HostName, Prompt: "Username for " + cfg.HostName + ":", Echo: true}); err != nil {
			return nil, 0, err
		}
	}
	khPath := opts.KnownHosts
	if khPath == "" {
		khPath = filepath.Join(p.SSHDir, "known_hosts")
	}
	batch := maxKeysPerConn
	start := 0
	for conns := 0; start < len(checked); conns++ {
		if conns > 0 {
			if err := sleepCtx(ctx, queryPause); err != nil {
				return res, start, err
			}
		}
		end := min(start+batch, len(checked))
		n, hit, err := queryBatch(ctx, hops, addr, user, khPath, cfg.HostName, pr, checked[start:end])
		if err != nil {
			return res, start, err
		}
		if hit >= 0 {
			res[start+hit] = true
		}
		if hit < 0 && n < end-start {
			// The server cut the connection after n keys (its limit on
			// failed tries is lower than ours): ask fewer at a time.
			batch = max(n, 1)
		}
		start += n
	}
	return res, start, nil
}

// sleepCtx waits d or until ctx is done (then it returns ctx.Err()).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LocalKeys returns the public keys Labmill could use on the laptop for
// alias: the agent's keys (with the same deadline as agentSigners) and the
// default key files and the alias's IdentityFile entries in p.SSHDir whose
// .pub exists (Source "agent" or the file path in ConfigPath form). Private
// key files are not read.
func LocalKeys(ctx context.Context, p sshconfig.Paths, alias string) ([]LocalKey, error) {
	cfg, _, err := sshconfig.Lookup(p, alias)
	if err != nil {
		return nil, err
	}
	var out []LocalKey
	seen := map[string]bool{}
	add := func(src string, pk ssh.PublicKey) {
		fp := ssh.FingerprintSHA256(pk)
		if !seen[fp] {
			seen[fp] = true
			out = append(out, LocalKey{Source: src, Fingerprint: fp, PublicKey: pk})
		}
	}
	sigs, closeAgent := agentSigners(ctx)
	for _, sg := range sigs {
		add("agent", sg.PublicKey())
	}
	closeAgent()
	home := filepath.Dir(p.SSHDir)
	files := slices.Clone(cfg.IdentityFiles)
	if len(files) == 0 && cfg.IdentityFile != "" {
		files = []string{cfg.IdentityFile}
	}
	for _, n := range defaultKeyNames {
		files = append(files, "~/.ssh/"+n)
	}
	for _, f := range files {
		path := expandKeyPath(f, home)
		if strings.HasPrefix(f, "~/.ssh/") {
			// The default names (and ~/.ssh entries) live in p.SSHDir,
			// which tests and non-standard setups move.
			path = filepath.Join(p.SSHDir, f[len("~/.ssh/"):])
		}
		// Metadata only: the private key must exist, it is never read.
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path + ".pub")
		if err != nil {
			continue
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			continue
		}
		add(f, pk)
	}
	return out, ctx.Err()
}

// LocalKey is a public key available on the laptop.
type LocalKey struct {
	Source      string // "agent", or the key file as written in an IdentityFile line ("~/.ssh/id_ed25519")
	Fingerprint string
	PublicKey   ssh.PublicKey
}

// errQueryStop is what the query signer's Sign returns: the server said it
// would accept the key, so the check ends there without signing.
var errQueryStop = errors.New("sshx: public-key query answered")

// listKeysScript is the fixed POSIX sh program ListRemoteKeys runs, on one
// line (csh/tcsh reject a newline inside a single-quoted word) and without
// single quotes or "!" (it is passed to sh -c in single quotes). It prints
// "<name>TAB<first line>" for every ~/.ssh/*.pub that is a regular file (or a
// symlink that stays inside ~/.ssh), whose name has no control character and
// that has a private-key sibling file of the same name without ".pub". Only
// the .pub files are read, never the private key.
var listKeysScript = strings.Join([]string{
	`cd "$HOME/.ssh" 2>/dev/null || exit 0`,
	`d=$(pwd -P)`,
	`for f in *.pub`,
	`do [ -f "$f" ] || continue`,
	`case "$f" in *[[:cntrl:]]*) continue;; esac`,
	`n=${f%.pub}`,
	`[ -f "$n" ] || continue`,
	`if [ -L "$f" ]`,
	`then r=$(readlink -f "$f" 2>/dev/null)`,
	`case "$r" in "$d"/*) ;; *) continue;; esac`,
	`fi`,
	`line=`,
	`IFS= read -r line < "$f" || [ -n "$line" ] || continue`,
	`printf "%s\t%s\n" "$n" "$line"`,
	`done`,
	`exit 0`,
}, "; ")

// shQuote quotes s as one POSIX shell word in single quotes.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseRemoteKeys parses listKeysScript's output: lines that don't parse are
// skipped, the result is sorted by name and capped at maxRemoteKeys.
func parseRemoteKeys(out string) []RemoteKey {
	var keys []RemoteKey
	for _, line := range strings.Split(out, "\n") {
		name, rest, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok || name == "" || strings.ContainsAny(name, "/") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			continue
		}
		pk, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
		if err != nil {
			continue
		}
		keys = append(keys, RemoteKey{
			Name:        textsafe.Clean(name),
			rawName:     name,
			Type:        pk.Type(),
			Fingerprint: ssh.FingerprintSHA256(pk),
			Comment:     strings.TrimSpace(textsafe.Clean(comment)),
			PublicKey:   pk,
		})
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	if len(keys) > maxRemoteKeys {
		keys = keys[:maxRemoteKeys]
	}
	return keys
}

// querySigner exposes a public key and never signs: Sign (reached only after
// the server answered SSH_MSG_USERAUTH_PK_OK) records that and stops. In a
// batch, hit (when set) holds the index of the first accepted key.
type querySigner struct {
	pub      ssh.PublicKey
	accepted *atomic.Bool
	idx      int32
	hit      *atomic.Int32
	reached  *atomic.Int32 // highest idx whose key the auth loop looked at
}

// PublicKey is called once per key as the auth loop reaches it, right before
// the key is offered to the server.
func (q querySigner) PublicKey() ssh.PublicKey {
	if q.reached != nil {
		for {
			cur := q.reached.Load()
			if cur >= q.idx || q.reached.CompareAndSwap(cur, q.idx) {
				break
			}
		}
	}
	return q.pub
}

// stop records that the server accepted this key and ends the query.
func (q querySigner) stop() (*ssh.Signature, error) {
	if q.accepted != nil {
		q.accepted.Store(true)
	}
	if q.hit != nil {
		q.hit.CompareAndSwap(-1, q.idx)
	}
	return nil, errQueryStop
}

func (q querySigner) Sign(_ io.Reader, _ []byte) (*ssh.Signature, error) { return q.stop() }

func (q querySigner) SignWithAlgorithm(_ io.Reader, _ []byte, _ string) (*ssh.Signature, error) {
	return q.stop()
}

// Algorithms lists the signature algorithms to query with: RSA keys use the
// rsa-sha2 pair first, then ssh-rsa for a server that lists no algorithms
// (the query never signs, so the weaker one is harmless), others their own
// type.
func (q querySigner) Algorithms() []string {
	if q.pub.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{q.pub.Type()}
}

// queryBatch connects to addr once (through hops) and offers pubs, in order,
// with query signers; it returns the index of the first key the server
// accepts (the query stops there, since the signer refuses to sign), or -1
// when it rejects all of them. Keys before the returned index were rejected;
// keys after it were not asked.
//
// n is how many of pubs got an answer: hit+1 for an accepted key, len(pubs)
// when all were rejected. When the server disconnects after the host key was
// accepted because of too many failed tries (its MaxAuthTries is below
// len(pubs)), n counts the keys offered before the last one (at least 1); the
// last one is not counted because the server may have hung up before judging
// it. hit is -1 then, and the caller offers fewer keys at once.
func queryBatch(ctx context.Context, hops []*ssh.Client, addr, user, khPath, host string, pr Prompter, pubs []ssh.PublicKey) (int, int, error) {
	conn, err := dialHop(ctx, hops, addr)
	if err != nil {
		return 0, -1, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	remote := normalizeRemote(addr, conn.RemoteAddr())
	var cause error
	setCause := func(err error) error {
		if cause == nil {
			cause = err
		}
		return err
	}
	var hostKeyOK, hostKeySeen atomic.Bool
	check := hostKeyCheck(khPath, host, remote, func(prompt string) (string, error) {
		return pr.Ask(ctx, Question{Kind: AskHostKey, Host: host, Prompt: prompt, Echo: true})
	}, setCause)
	hostKey := func(hostname string, r net.Addr, key ssh.PublicKey) error {
		hostKeySeen.Store(true)
		err := check(hostname, r, key)
		hostKeyOK.Store(err == nil)
		return err
	}
	algos, err := knownHostAlgorithms(khPath, addr, remote)
	if err != nil {
		return 0, -1, err
	}
	var hit, reached atomic.Int32
	hit.Store(-1)
	reached.Store(-1)
	var accepted atomic.Bool
	signers := make([]ssh.Signer, len(pubs))
	for i, pub := range pubs {
		signers[i] = querySigner{pub: pub, accepted: &accepted, idx: int32(i), hit: &hit, reached: &reached}
	}
	cfg := &ssh.ClientConfig{
		User:              user,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: algos,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err == nil { // a server that accepts "none" auth gets here: never leak a login
		ssh.NewClient(c, chans, reqs).Close()
		return 0, -1, fmt.Errorf("%w: the login completed", ErrQueryUnsupported)
	}
	switch {
	case ctx.Err() != nil:
		return 0, -1, ctx.Err()
	case hit.Load() >= 0:
		return int(hit.Load()) + 1, int(hit.Load()), nil
	case cause != nil:
		return 0, -1, cause
	case strings.Contains(err.Error(), "unable to authenticate"):
		return len(pubs), -1, nil
	case strings.Contains(err.Error(), "no common public key signature algorithm"):
		return len(pubs), -1, nil // these keys' types can't be queried here: not accepted
	case hostKeyOK.Load() && tooManyTries(err) && reached.Load() >= 0:
		return min(max(int(reached.Load()), 1), len(pubs)), -1, nil
	case hostKeyOK.Load():
		return 0, -1, fmt.Errorf("%w: %v", ErrQueryUnsupported, err)
	case !hostKeySeen.Load() && closedEarly(err):
		return 0, -1, fmt.Errorf("%s: %w", host, ErrHostLimiting)
	}
	return 0, -1, fmt.Errorf("ssh query to %s: %w", addr, err)
}

// tooManyTries reports whether err is the server's disconnect for too many
// failed tries (SSH_DISCONNECT_PROTOCOL_ERROR, reason 2, "Too many
// authentication failures"). x/crypto keeps the disconnect message type
// private, so this goes by its text.
func tooManyTries(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "too many authentication") || strings.Contains(msg, "ssh: disconnect, reason 2")
}

// closedEarly reports whether err says the peer closed the connection
// before the handshake got anywhere.
func closedEarly(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if errors.Is(err, syscall.Errno(10054)) { // WSAECONNRESET on Windows
		return true
	}
	msg := err.Error()
	return strings.HasSuffix(msg, "EOF") || strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "forcibly closed")
}
