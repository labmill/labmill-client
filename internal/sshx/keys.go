package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"

	"labmill-client/internal/sshconfig"
)

// ErrKeySetupDeclined reports that the user declined (or canceled) the key
// setup of a key-only host, or never confirmed the upload. Test with
// errors.Is.
var ErrKeySetupDeclined = errors.New("SSH key setup was not completed")

// KeyPair is an SSH key pair Labmill created.
type KeyPair struct {
	// Path is the private key file; Path+".pub" holds the public key.
	Path string
	// ConfigPath is how Path is written in the ssh config's IdentityFile
	// line: "~/.ssh/<name>" when the key lives in the user's home .ssh
	// folder, else Path.
	ConfigPath string
	// PublicKey is the one-line public key (authorized_keys format, with a
	// comment), the text to register with the site.
	PublicKey string
	// Fingerprint is the key's SHA256 fingerprint.
	Fingerprint string
}

var keyNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// GenerateKey creates a new ed25519 key pair in p.SSHDir (created with mode
// 0700 when missing). The private key file is written with mode 0600 in
// OpenSSH format, encrypted with passphrase when that is not empty; the
// public key file (.pub) gets mode 0644. An existing file is never
// overwritten or changed: the name is "id_ed25519" when neither it nor its
// .pub exists, else "id_ed25519_labmill_<alias>", else that with "_2", "_3", …
// On Windows the files carry the ACL they inherit from the user's profile
// folder, which is what OpenSSH for Windows expects (only the user, SYSTEM
// and Administrators); Labmill does not touch ACLs.
func GenerateKey(p sshconfig.Paths, alias, passphrase string) (KeyPair, error) {
	if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
		return KeyPair{}, fmt.Errorf("creating %s: %w", p.SSHDir, err)
	}
	safe := keyNameUnsafe.ReplaceAllString(alias, "_")
	if safe == "" {
		safe = "host"
	}
	exists := func(name string) bool {
		for _, f := range []string{name, name + ".pub"} {
			if _, err := os.Lstat(filepath.Join(p.SSHDir, f)); err == nil || !errors.Is(err, os.ErrNotExist) {
				return true
			}
		}
		return false
	}
	name := "id_ed25519"
	if exists(name) {
		name = "id_ed25519_labmill_" + safe
		for i := 2; exists(name) && i < 100; i++ {
			name = fmt.Sprintf("id_ed25519_labmill_%s_%d", safe, i)
		}
		if exists(name) {
			return KeyPair{}, fmt.Errorf("no free key file name for %s in %s", alias, p.SSHDir)
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generating key: %w", err)
	}
	comment := "labmill-" + safe
	var blk *pem.Block
	if passphrase == "" {
		blk, err = ssh.MarshalPrivateKey(priv, comment)
	} else {
		blk, err = ssh.MarshalPrivateKeyWithPassphrase(priv, comment, []byte(passphrase))
	}
	if err != nil {
		return KeyPair{}, fmt.Errorf("encoding key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return KeyPair{}, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + comment
	path := filepath.Join(p.SSHDir, name)
	if err := writeNewFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return KeyPair{}, err
	}
	if err := writeNewFile(path+".pub", []byte(line+"\n"), 0o644); err != nil {
		return KeyPair{}, err
	}
	kp := KeyPair{Path: path, ConfigPath: path, PublicKey: line, Fingerprint: ssh.FingerprintSHA256(sshPub)}
	if filepath.Base(p.SSHDir) == ".ssh" {
		kp.ConfigPath = "~/.ssh/" + name
	}
	return kp, nil
}

// writeNewFile creates path with mode perm and content, failing when it
// already exists.
func writeNewFile(path string, content []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Close()
}

// CountUsableKeys counts the SSH keys Dial would offer for alias: the keys of
// its IdentityFile lines, or the default key files when it has none, that
// exist, are private to the user and parse (an encrypted key counts: the
// passphrase is asked at sign-in), plus the keys the SSH agent holds.
func CountUsableKeys(p sshconfig.Paths, alias string) int {
	cfg, _, err := sshconfig.Lookup(p, alias)
	if err != nil {
		cfg = sshconfig.Host{}
	}
	files := cfg.IdentityFiles
	if len(files) == 0 && cfg.IdentityFile != "" {
		files = []string{cfg.IdentityFile}
	}
	home := filepath.Dir(p.SSHDir)
	if len(files) == 0 {
		for _, n := range defaultKeyNames {
			files = append(files, filepath.Join(p.SSHDir, n))
		}
	}
	n := 0
	seen := map[string]bool{}
	for _, f := range files {
		f = expandKeyPath(f, home)
		if !keyFileIsPrivate(f) {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var missing *ssh.PassphraseMissingError
		if _, err := ssh.ParsePrivateKey(data); err != nil && !errors.As(err, &missing) {
			continue
		}
		if !seen[f] {
			seen[f] = true
			n++
		}
	}
	sigs, closeAgent := agentSigners(context.Background())
	closeAgent()
	return n + len(sigs)
}

// KeyUploadInfo is the site's text on registering a public key (from
// Site.yaml key_upload); either field may be empty.
type KeyUploadInfo struct {
	URL         string // https page
	Instruction string // one line
}

// KeyGuide walks the user through setting up an SSH key for a key-only host:
// create a key (GenerateKey), show the public key with the site's upload
// instructions, wait for the user to confirm the upload, then log in with
// the key. All questions go through Prompter (AskKeyCreate, AskNewPassphrase,
// AskKeyUpload); the private key and any passphrase stay on this computer.
type KeyGuide struct {
	Paths    sshconfig.Paths
	Alias    string // the ssh config alias of the host
	Host     string // the host name, for the texts
	Upload   KeyUploadInfo
	Prompter Prompter
	// SiteName is the institution that runs the host, for the texts ("" uses
	// Host).
	SiteName string
	// QueryOpts is what QueryKeys gets for the checks of existing keys
	// (KeyOnlyHosts for the jump hosts, ...).
	QueryOpts LoginOptions
	// Home, when set, lets Run look for an accepted key on the user's home
	// institution's login node (M45.3). nil skips that look.
	Home *HomeKeys

	pair  *KeyPair        // the key created in this run, once there is one
	known map[string]bool // fingerprints of the keys on this computer
}

// maxUploadChecks bounds how often Run asks the user to upload a key again
// after the server rejected it.
const maxUploadChecks = 3

// Run connects with connect, creating a key first when needed. connect is
// called to log in (through the helper in the app, Dial in tests) and returns
// ErrKeyRejected or ErrNoKey (errors.Is) when the key-only host refused.
//
//   - It always logs in first (the helper may already hold a login, or see
//     an agent this process does not).
//   - Before creating a key it looks for one the user already has that the
//     host accepts (useExistingKey): a key file on this computer that was
//     not offered, or, after the user agrees, a key copied from the home
//     login node; it adds the key to the ssh config and logs in again.
//   - With no key to offer (ErrNoKey) it goes to the setup: ask to create a
//     key, show it, wait for the upload, log in.
//   - If the server rejects the keys it has (ErrKeyRejected) it offers a new
//     key for this host once.
//   - If the new key is rejected (not uploaded yet, or the registry is slow)
//     it asks the user to upload it again, up to three times.
//
// Declining any question returns ErrKeySetupDeclined. Other connect errors
// are returned as they are.
func (g *KeyGuide) Run(ctx context.Context, connect func(context.Context) (*ssh.Client, error)) (*ssh.Client, error) {
	checks := 0
	offered := false
	looked := false
	for {
		cl, err := connect(ctx)
		if err == nil {
			return cl, nil
		}
		if !errors.Is(err, ErrKeyRejected) && !errors.Is(err, ErrNoKey) {
			return nil, err
		}
		switch {
		case g.pair != nil:
			// Our key is not accepted (yet).
			if checks++; checks >= maxUploadChecks {
				return nil, err
			}
			if aerr := g.askUpload(ctx, true); aerr != nil {
				return nil, aerr
			}
		case !looked:
			// Before a new key: one the user already has that the host
			// accepts (M45.3).
			looked = true
			added, uerr := g.useExistingKey(ctx)
			if uerr != nil {
				return nil, uerr
			}
			if !added {
				offered = true
				if cerr := g.createAndUpload(ctx, !errors.Is(err, ErrNoKey)); cerr != nil {
					return nil, cerr
				}
			}
		case !offered:
			offered = true
			if cerr := g.createAndUpload(ctx, !errors.Is(err, ErrNoKey)); cerr != nil {
				return nil, cerr
			}
		default:
			return nil, err
		}
	}
}

// createAndUpload asks to create a key, creates it, records it in the ssh
// config and asks the user to upload it. rejected says the server refused
// the keys that already exist.
func (g *KeyGuide) createAndUpload(ctx context.Context, rejected bool) error {
	where := g.siteLabel()
	prompt := "No SSH key of yours is registered at " + where + " yet. " + g.Host + " accepts only SSH keys, not passwords, and Labmill found no SSH key it can use.\n\n" +
		"Labmill can create a new key now. The private key stays on this computer in " + sshDirShown(g.Paths) +
		"; the public key is what you register with " + where + ".\n\nCreate a new SSH key? (yes/no)"
	if rejected {
		prompt = "No SSH key of yours is registered at " + where + " yet. " + g.Host + " did not accept the SSH keys you have; it accepts only keys that are registered with the site.\n\n" +
			"Labmill can create a new key for it now. The private key stays on this computer in " + sshDirShown(g.Paths) +
			"; the public key is what you register with " + where + ".\n\nCreate a new SSH key? (yes/no)"
	}
	a, err := g.Prompter.Ask(ctx, Question{Kind: AskKeyCreate, Host: g.Host, Prompt: prompt, Echo: true})
	if err != nil {
		return err
	}
	if a = strings.ToLower(strings.TrimSpace(a)); a != "yes" && a != "y" {
		return fmt.Errorf("%w: create an SSH key and register it with the site, then try again", ErrKeySetupDeclined)
	}
	pass, err := g.askNewPassphrase(ctx)
	if err != nil {
		return err
	}
	kp, err := GenerateKey(g.Paths, g.Alias, pass)
	if err != nil {
		return err
	}
	g.pair = &kp
	if _, err := sshconfig.AddLabmillIdentityFile(g.Paths, g.Alias, kp.ConfigPath); err != nil && kp.ConfigPath != "~/.ssh/id_ed25519" {
		// The alias belongs to the user's own ssh config, which Labmill never
		// edits: say what to add and go on with the upload.
		g.Prompter.Notify("Labmill does not edit your own ssh config. To use the new key, add the line \"IdentityFile " + kp.ConfigPath + "\" under \"Host " + g.Alias + "\" in ~/.ssh/config.")
	}
	g.Prompter.Notify("Created the SSH key " + kp.ConfigPath + " (" + kp.Fingerprint + ").")
	return g.askUpload(ctx, false)
}

// askNewPassphrase asks for an optional passphrase for the new key, twice
// when one is given.
func (g *KeyGuide) askNewPassphrase(ctx context.Context) (string, error) {
	for try := 0; try < 3; try++ {
		p1, err := g.Prompter.Ask(ctx, Question{Kind: AskNewPassphrase, Host: g.Host,
			Prompt: "Optional passphrase to protect the new key (leave empty for none). Labmill asks for it at each sign-in and never stores it:"})
		if err != nil {
			return "", err
		}
		if p1 == "" {
			return "", nil
		}
		p2, err := g.Prompter.Ask(ctx, Question{Kind: AskNewPassphrase, Host: g.Host, Prompt: "Type the passphrase again:"})
		if err != nil {
			return "", err
		}
		if p1 == p2 {
			return p1, nil
		}
		g.Prompter.Notify("The two passphrases differ; try again.")
	}
	return "", fmt.Errorf("%w: the passphrases never matched", ErrKeySetupDeclined)
}

// askUpload shows the public key with the site's upload instructions and
// waits for the user's confirmation. again says the server did not accept
// the key at the last try.
func (g *KeyGuide) askUpload(ctx context.Context, again bool) error {
	var b strings.Builder
	if again {
		b.WriteString(g.Host + " did not accept the key yet. It can take a few minutes after the upload; check that the full line was uploaded.\n\n")
	}
	b.WriteString("Register this public key with " + g.siteLabel() + ", then choose \"I've added it, try again\". The private key never leaves this computer.\n")
	if g.Upload.Instruction != "" {
		b.WriteString("\n" + g.Upload.Instruction + "\n")
	} else {
		b.WriteString("\nUpload it as the site's documentation describes.\n")
	}
	if g.Upload.URL != "" {
		b.WriteString("\n" + g.Upload.URL + "\n")
	}
	a, err := g.Prompter.Ask(ctx, Question{Kind: AskKeyUpload, Host: g.Host, Prompt: b.String(), Echo: true, Data: g.pair.PublicKey})
	if err != nil {
		return err
	}
	if a = strings.ToLower(strings.TrimSpace(a)); a != "yes" && a != "y" {
		return fmt.Errorf("%w: the key is in %s.pub; register it with the site and sign in again", ErrKeySetupDeclined, g.pair.Path)
	}
	return nil
}

// sshDirShown is the key folder as shown to the user.
func sshDirShown(p sshconfig.Paths) string {
	if filepath.Base(p.SSHDir) == ".ssh" {
		return "~/.ssh"
	}
	return p.SSHDir
}
