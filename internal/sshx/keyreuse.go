package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
	"labmill-client/internal/textsafe"
)

// M45.3: before KeyGuide offers a new key it looks for a key the user
// already has that the key-only host accepts: first a key file on this
// computer that was not offered yet, then a key pair on the home
// institution's login node (copied here only after the user agrees).

// HomeKeys is where KeyGuide looks for the user's existing keys on the home
// institution's login node. A nil *HomeKeys skips the look.
type HomeKeys struct {
	// Alias is the home login host's ssh alias; it names the copied key file
	// ("id_ed25519_<Alias>").
	Alias string
	// Host is the home login host name shown to the user.
	Host string
	// Client returns a live connection to the home login host and a release
	// function (called when done), or nil when there is none. It must never
	// log in or ask anything: the step is skipped silently without a
	// connection that is already open.
	Client func(ctx context.Context) (*ssh.Client, func())
}

// maxPrivateKeySize bounds the private key file read from the home host.
const maxPrivateKeySize = 64 << 10

// limitWait is how long useExistingKey waits when the host refuses
// connections, before it asks the keys not yet asked. Tests shorten it.
var limitWait = 60 * time.Second

// hostLimitingError is what the guide ends with when the host still refuses
// connections after the wait; its text is for the user as it is. It matches
// ErrHostLimiting.
type hostLimitingError struct{ host string }

func (e *hostLimitingError) Error() string {
	return textsafe.Clean(e.host) + " is still refusing connections; wait a few minutes and try again"
}

func (e *hostLimitingError) Unwrap() error { return ErrHostLimiting }

// useExistingKey looks for a key of the user's that the host accepts and
// adds it to the ssh config for the alias. added reports that the caller
// should connect again. Failures to look (network, query) are shown and
// are not errors; a canceled context, a prompt error or a key that cannot
// be recorded (ErrKeySetupDeclined) is returned.
//
// The check is one QueryKeys call for the local candidates and the home
// keys together. It is skipped (with one line saying why) when the path to
// the host goes through a jump host that is not key-only: QueryKeys would
// have to sign in to it, with a password and a second factor, just for this.
// When the host refuses connections after several failed tries
// (ErrHostLimiting) and no key was accepted yet, it waits limitWait and asks
// the keys not yet asked once more; if the host still refuses it returns an
// error that wraps ErrHostLimiting instead of going on to a new key.
func (g *KeyGuide) useExistingKey(ctx context.Context) (added bool, err error) {
	if hop, jerr := nonKeyOnlyJump(g.Paths, g.Alias, g.QueryOpts); jerr == nil && hop != "" {
		g.Prompter.Notify("Labmill cannot check your existing keys without signing in to " + textsafe.Clean(hop) + " first, so it skips that check.")
		return false, nil
	}
	local, err := g.localCandidates(ctx)
	if err != nil {
		g.Prompter.Notify("Could not check the SSH keys on this computer: " + textsafe.Clean(err.Error()))
	}
	// Home keys: listed over a connection that is already open.
	var home []RemoteKey
	var homeClient *ssh.Client
	if g.Home != nil && g.Home.Client != nil {
		cl, release := g.Home.Client(ctx)
		if cl != nil {
			if release != nil {
				defer release()
			}
			keys, lerr := ListRemoteKeys(ctx, cl)
			switch {
			case lerr == nil:
				for _, k := range keys {
					if !g.known[k.Fingerprint] { // not already on this computer
						home = append(home, k)
					}
				}
				homeClient = cl
			case ctx.Err() != nil:
				return false, ctx.Err()
			default:
				g.Prompter.Notify("Could not list the SSH keys on " + g.homeHost() + ": " + textsafe.Clean(lerr.Error()))
			}
		}
	}
	if len(local)+len(home) == 0 {
		return false, nil
	}
	// maxRemoteKeys bounds the whole check (QueryKeys leaves the rest false).
	if len(local) > maxRemoteKeys {
		local = local[:maxRemoteKeys]
	}
	if room := maxRemoteKeys - len(local); len(home) > room {
		home = home[:room]
	}
	pubs := make([]ssh.PublicKey, 0, len(local)+len(home))
	for _, k := range local {
		pubs = append(pubs, k.PublicKey)
	}
	for _, k := range home {
		pubs = append(pubs, k.PublicKey)
	}
	ok, asked, qerr := queryKeys(ctx, g.Paths, g.Alias, g.Prompter, g.QueryOpts, pubs)
	if len(ok) < len(pubs) {
		ok = append(ok, make([]bool, len(pubs)-len(ok))...)
	}
	if qerr != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(qerr, ErrHostLimiting) && !slices.Contains(ok, true) && asked < len(pubs) {
		// The host refuses connections for a while after the failed tries.
		// Wait it out once and ask only the keys that were not asked yet:
		// going on to a new key now would make the user register one they
		// may not need.
		g.Prompter.Notify(g.Host + " is briefly refusing connections after several sign-in tries; Labmill waits a minute and checks again.")
		if err := sleepCtx(ctx, limitWait); err != nil {
			return false, err
		}
		ok2, _, qerr2 := queryKeys(ctx, g.Paths, g.Alias, g.Prompter, g.QueryOpts, pubs[asked:])
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		copy(ok[asked:], ok2)
		qerr = qerr2
		if errors.Is(qerr, ErrHostLimiting) && !slices.Contains(ok, true) {
			return false, &hostLimitingError{host: g.Host}
		}
	}
	// A failure leaves what was learned before it: an accepted key found
	// earlier still counts.
	if qerr != nil && !slices.Contains(ok, true) {
		g.Prompter.Notify("Could not check whether " + g.Host + " accepts your existing SSH keys: " + textsafe.Clean(qerr.Error()))
		return false, nil
	}
	for i, k := range local {
		if !ok[i] {
			continue
		}
		if _, aerr := sshconfig.AddLabmillIdentityFile(g.Paths, g.Alias, k.Source); aerr != nil {
			return false, fmt.Errorf("%w: Labmill does not edit your own ssh config; to use the key %s add the line \"IdentityFile %s\" under \"Host %s\" in ~/.ssh/config and sign in again", ErrKeySetupDeclined, k.Source, k.Source, g.Alias)
		}
		g.Prompter.Notify("Your SSH key " + k.Source + " (" + k.Fingerprint + ") is registered at " + g.siteLabel() + "; Labmill will use it.")
		return true, nil
	}
	for i, k := range home {
		if !ok[len(local)+i] {
			continue
		}
		done, cerr := g.askCopy(ctx, homeClient, k)
		if cerr != nil {
			return false, cerr
		}
		if done {
			return true, nil
		}
	}
	return false, nil
}

// nonKeyOnlyJump returns the first jump host on the way to alias that is not
// listed in opts.KeyOnlyHosts ("" when all are, or there are none).
func nonKeyOnlyJump(p sshconfig.Paths, alias string, opts LoginOptions) (string, error) {
	var chain []hopSpec
	if err := flatten(p, hopSpec{name: alias}, nil, &chain); err != nil {
		return "", err
	}
	for _, h := range chain {
		cfg, _, err := sshconfig.Lookup(p, h.name)
		if err != nil {
			return "", err
		}
		if !hostListed(cfg.HostName, opts.KeyOnlyHosts) {
			return cfg.HostName, nil
		}
	}
	return "", nil
}

// siteLabel is the institution (or host) name for texts.
func (g *KeyGuide) siteLabel() string {
	if g.SiteName != "" {
		return g.SiteName
	}
	return g.Host
}

// localCandidates returns the key files in the ssh folder that Dial did not
// offer for the alias (it offers the alias's IdentityFile lines, else the
// default names, and the agent's keys), and records in g.known the
// fingerprints of every local key so the home step skips keys that are
// already here.
func (g *KeyGuide) localCandidates(ctx context.Context) ([]LocalKey, error) {
	all, err := LocalKeys(ctx, g.Paths, g.Alias)
	if err != nil {
		return nil, err
	}
	offered := map[string]bool{}
	cfg, _, lerr := sshconfig.Lookup(g.Paths, g.Alias)
	files := cfg.IdentityFiles
	if len(files) == 0 && cfg.IdentityFile != "" {
		files = []string{cfg.IdentityFile}
	}
	if lerr != nil || len(files) == 0 {
		files = nil
		for _, n := range defaultKeyNames {
			files = append(files, "~/.ssh/"+n)
		}
	}
	for _, f := range files {
		offered[f] = true
	}
	var cands []LocalKey
	seen := map[string]bool{}
	for _, k := range all {
		seen[k.Fingerprint] = true
		if k.Source != "agent" && !offered[k.Source] {
			cands = append(cands, k)
		}
	}
	// Other key pairs in the ssh folder (custom names).
	matches, _ := filepath.Glob(filepath.Join(g.Paths.SSHDir, "*.pub"))
	for _, m := range matches {
		name := strings.TrimSuffix(filepath.Base(m), ".pub")
		cfgPath := filepath.Join(g.Paths.SSHDir, name)
		if filepath.Base(g.Paths.SSHDir) == ".ssh" {
			cfgPath = "~/.ssh/" + name
		}
		if offered[cfgPath] {
			continue
		}
		if fi, err := os.Stat(filepath.Join(g.Paths.SSHDir, name)); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		pk, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			continue
		}
		if fp := ssh.FingerprintSHA256(pk); !seen[fp] {
			seen[fp] = true
			cands = append(cands, LocalKey{Source: cfgPath, Fingerprint: fp, PublicKey: pk})
		}
	}
	g.known = seen
	if len(cands) > maxRemoteKeys {
		cands = cands[:maxRemoteKeys]
	}
	return cands, nil
}

func (g *KeyGuide) homeHost() string {
	if g.Home != nil && g.Home.Host != "" {
		return g.Home.Host
	}
	return "your home cluster"
}

// askCopy asks whether to copy key k from the home host and does it on yes.
// A "no" or a failed copy returns false without an error.
func (g *KeyGuide) askCopy(ctx context.Context, cl *ssh.Client, k RemoteKey) (bool, error) {
	name := freeKeyName(g.Paths.SSHDir, "id_"+keyTypeShort(k.Type)+"_"+keyNameUnsafe.ReplaceAllString(g.Home.Alias, "_"))
	shown := filepath.Join(g.Paths.SSHDir, name)
	if filepath.Base(g.Paths.SSHDir) == ".ssh" {
		shown = "~/.ssh/" + name
	}
	hh := g.homeHost()
	prompt := g.Host + " already accepts the SSH key " + k.Name + " (" + k.Fingerprint + ") that is on " + hh + ".\n\n" +
		"Labmill can copy it:\n  from " + hh + ":~/.ssh/" + k.Name + "\n  to   " + shown + " (on this computer)\n\n" +
		"Warning: this puts a copy of a private key on this computer. Anyone who can read that file can sign in as you wherever the key is accepted. " +
		"Labmill uses the key only for your own sign-ins and never shows it to Claude.\n\n" +
		"Choose No to create a separate key for this computer instead.\n\nCopy the key to this computer? (yes/no)"
	a, err := g.Prompter.Ask(ctx, Question{Kind: AskKeyCopy, Host: g.Host, Prompt: prompt, Echo: true})
	if err != nil {
		return false, err
	}
	if a = strings.ToLower(strings.TrimSpace(a)); a != "yes" && a != "y" {
		return false, nil
	}
	cfgPath, pass, err := g.copyKeyFile(ctx, cl, k, name)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		g.Prompter.Notify("Could not copy the key " + k.Name + ": " + textsafe.Clean(err.Error()))
		return false, nil
	}
	if _, aerr := sshconfig.AddLabmillIdentityFile(g.Paths, g.Alias, cfgPath); aerr != nil {
		return false, fmt.Errorf("%w: the key was copied to %s, but Labmill does not edit your own ssh config; add the line \"IdentityFile %s\" under \"Host %s\" in ~/.ssh/config and sign in again", ErrKeySetupDeclined, cfgPath, cfgPath, g.Alias)
	}
	protection := "It has no passphrase, so anyone who gets the file can use it; consider protecting it with ssh-keygen -p."
	if pass {
		protection = "It is protected by its passphrase, which Labmill asks for at each sign-in and never stores."
	}
	g.Prompter.Notify("Copied the SSH key " + k.Name + " from " + hh + " to " + cfgPath + " (" + k.Fingerprint + "); Labmill will use it. " + protection)
	return true, nil
}

// copyKeyFile reads the private key k.Name from the home host, checks that
// it belongs to the public key the target host accepted, and writes it as a
// new file (never over an existing one). The contents stay in memory and
// in that file. It returns the path as written in an IdentityFile line.
func (g *KeyGuide) copyKeyFile(ctx context.Context, cl *ssh.Client, k RemoteKey, name string) (cfgPath string, encrypted bool, err error) {
	data, err := readRemoteFile(ctx, cl, k.rawName)
	if err != nil {
		return "", false, err
	}
	defer clear(data)
	pub, encrypted, err := privateKeyPublic(data)
	if err != nil {
		return "", false, err
	}
	if !bytes.Equal(pub.Marshal(), k.PublicKey.Marshal()) {
		return "", false, errors.New("the private key file does not match its public key; not copied")
	}
	if err := os.MkdirAll(g.Paths.SSHDir, 0o700); err != nil {
		return "", false, fmt.Errorf("creating %s: %w", g.Paths.SSHDir, err)
	}
	path := filepath.Join(g.Paths.SSHDir, name)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k.PublicKey)))
	if k.Comment != "" {
		line += " " + k.Comment
	}
	// The .pub first: a failure after it leaves no private key behind.
	if err := writeNewFile(path+".pub", []byte(line+"\n"), 0o644); err != nil {
		return "", false, err
	}
	if err := writeNewFile(path, data, 0o600); err != nil {
		return "", false, err
	}
	if filepath.Base(g.Paths.SSHDir) == ".ssh" {
		return "~/.ssh/" + name, encrypted, nil
	}
	return path, encrypted, nil
}

// privateKeyPublic returns the public key of a private key without its
// passphrase: an encrypted key in OpenSSH format still carries its public
// key. Errors never include key material.
func privateKeyPublic(data []byte) (pub ssh.PublicKey, encrypted bool, err error) {
	raw, err := ssh.ParseRawPrivateKey(data)
	if err == nil {
		sg, serr := ssh.NewSignerFromKey(raw)
		if serr != nil {
			return nil, false, errors.New("the private key is of an unsupported type; not copied")
		}
		return sg.PublicKey(), false, nil
	}
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if missing.PublicKey == nil {
			return nil, true, errors.New("the private key is encrypted in a format that does not show its public key, so it cannot be checked; not copied")
		}
		return missing.PublicKey, true, nil
	}
	return nil, false, errors.New("the file is not a private key Labmill can read; not copied")
}

// readRemoteFile reads ~/.ssh/<name> on the host with one fixed command
// (name has no "/" or control characters: parseRemoteKeys). The contents
// are returned in memory only; they are never logged or put in an error.
func readRemoteFile(ctx context.Context, cl *ssh.Client, name string) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, "/\x00") || strings.IndexFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		return nil, errors.New("unusual key file name")
	}
	ctx, cancel := context.WithTimeout(ctx, readKeyTimeout)
	defer cancel()
	sess, err := cl.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close() // also ends a cat that is still writing
	out, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	script := `cd "$HOME/.ssh" 2>/dev/null && cat -- ` + shQuote(name)
	if err := sess.Start("sh -c " + shQuote(script)); err != nil {
		return nil, err
	}
	type res struct {
		data []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		d, rerr := io.ReadAll(io.LimitReader(out, maxPrivateKeySize+1))
		// Only a file within the limit is waited for: cat of a larger one
		// (or a FIFO) would block once the channel window is full.
		if rerr == nil && len(d) <= maxPrivateKeySize {
			rerr = sess.Wait()
		}
		ch <- res{d, rerr}
	}()
	select {
	case r := <-ch:
		if len(r.data) > maxPrivateKeySize {
			clear(r.data)
			return nil, errors.New("the key file is too large")
		}
		if r.err != nil {
			clear(r.data)
			return nil, errors.New("reading the key file on the home host failed")
		}
		return r.data, nil
	case <-ctx.Done():
		sess.Close()
		if err := ctx.Err(); errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("reading the key file on the home host timed out")
		}
		return nil, ctx.Err()
	}
}

// readKeyTimeout bounds reading a private key from the home host.
var readKeyTimeout = 30 * time.Second

// keyTypeShort is the short key type for file names: "ed25519", "rsa", ...
func keyTypeShort(t string) string {
	switch {
	case t == ssh.KeyAlgoED25519:
		return "ed25519"
	case t == ssh.KeyAlgoRSA:
		return "rsa"
	case strings.HasPrefix(t, "ecdsa-"):
		return "ecdsa"
	}
	return "key"
}

// freeKeyName returns base, or base_2, base_3, ... : the first name for which
// neither the file nor its .pub exists in dir.
func freeKeyName(dir, base string) string {
	exists := func(n string) bool {
		for _, f := range []string{n, n + ".pub"} {
			if _, err := os.Lstat(filepath.Join(dir, f)); err == nil || !errors.Is(err, os.ErrNotExist) {
				return true
			}
		}
		return false
	}
	name := base
	for i := 2; exists(name) && i < 1000; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	return name
}
