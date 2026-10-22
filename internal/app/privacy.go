package app

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// The local privacy rules (see Tools): one check shared by every tool that
// looks at, stages or writes local files.

// privacyOS is the OS whose system folders are off limits; a variable so
// tests can exercise the macOS and Windows rules everywhere.
var privacyOS = runtime.GOOS

// isSecretName reports whether a file or folder name looks like it holds
// secrets: id_*, *.pem, *.key, *.p12, *.pfx, *.kdbx, *.keychain*, names
// containing "credential" or "secret", .netrc and .env*.
func isSecretName(name string) bool {
	// Upper-then-lower folds lookalikes such as "ſ" (long s) and "ı" (dotless
	// i) to their ASCII letters, as case-insensitive filesystems may.
	l := strings.ToLower(strings.ToUpper(name))
	switch {
	case strings.HasPrefix(l, "id_"),
		strings.HasSuffix(l, ".pem"), strings.HasSuffix(l, ".key"), strings.HasSuffix(l, ".p12"),
		strings.HasSuffix(l, ".pfx"), strings.HasSuffix(l, ".kdbx"),
		strings.Contains(l, ".keychain"),
		strings.Contains(l, "credential"), strings.Contains(l, "secret"),
		strings.Contains(l, "password"), strings.Contains(l, "passwd"),
		l == ".netrc", strings.HasPrefix(l, ".env"):
		return true
	}
	return false
}

// sensitiveName folds name the way isSecretName compares names.
func sensitiveName(name string) string { return strings.ToLower(strings.ToUpper(name)) }

// hardSecrets are the files and folders that hold sign-in secrets and are
// never read or touched on the user's own computer, not even with approval.
// It is the strict core of the privacy rule above: isSecretName and
// privateComponent decide what needs approval, these decide what is refused
// outright. Names are compared folded (sensitiveName). The rule is for local
// files only: Labmill puts no limits on what runs on a cluster (remote.go).
var hardSecrets = struct {
	dirs     []string    // any path component: everything inside is refused
	bases    []string    // file names anywhere
	pairs    [][2]string // a folder directly followed by an entry (and everything inside it)
	suffixes []string    // file name endings (private keys, password safes)
	prefixes []string    // file name beginnings (SSH keys)
	history  string      // hidden file names ending in this (shell histories)
}{
	dirs:     []string{".ssh", ".gnupg", ".kube", ".azure"},
	bases:    []string{".netrc", ".git-credentials", ".pgpass", "authentication.json", "rclone.conf", ".vault-token", ".s3cfg", ".boto", ".pypirc", ".npmrc"},
	pairs:    [][2]string{{".aws", "credentials"}, {".docker", "config.json"}, {".config", "gcloud"}, {"gh", "hosts.yml"}, {"huggingface", "token"}, {".huggingface", "token"}},
	suffixes: []string{".pem", ".key", ".p12", ".pfx", ".kdbx"},
	prefixes: []string{"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"},
	history:  "_history",
}

// hardSecretPath reports whether p (absolute or relative, '/'-separated,
// maybe with a leading ~) names one of hardSecrets or lies inside one.
func hardSecretPath(p string) bool {
	var comps []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." {
			comps = append(comps, sensitiveName(c))
		}
	}
	hs := hardSecrets
	for i, c := range comps {
		for _, d := range hs.dirs {
			if c == d {
				return true
			}
		}
		for _, b := range hs.bases {
			if c == b {
				return true
			}
		}
		if i > 0 {
			for _, pr := range hs.pairs {
				if comps[i-1] == pr[0] && c == pr[1] {
					return true
				}
			}
		}
		for _, s := range hs.suffixes {
			if strings.HasSuffix(c, s) && len(c) > len(s) {
				return true
			}
		}
		for _, pre := range hs.prefixes {
			if strings.HasPrefix(c, pre) {
				return true
			}
		}
		if strings.HasPrefix(c, ".") && strings.HasSuffix(c, hs.history) {
			return true
		}
	}
	return false
}

// privateComponent reports whether one path component is off limits. top is
// true for the first component below a root, where ~/Library (macOS) and
// AppData (Windows) live.
func privateComponent(name string, top bool) bool {
	if strings.HasPrefix(name, ".") || isSecretName(name) {
		return true
	}
	if top {
		switch l := strings.ToLower(name); {
		case privacyOS == "darwin" && l == "library", privacyOS == "windows" && l == "appdata":
			return true
		}
	}
	return false
}

// checkRel applies the privacy rules to a path relative to a root.
func checkRel(rel string) error {
	rel = filepath.ToSlash(rel)
	if hardSecretPath(rel) {
		return fmt.Errorf("%s holds sign-in secrets, so I won't use it.", sanitize(rel))
	}
	for i, c := range strings.Split(rel, "/") {
		if c == "" || c == "." {
			continue
		}
		if privateComponent(c, i == 0) {
			return fmt.Errorf("%s is private (hidden, a system folder, or it looks like it holds passwords or keys), so I won't use it.", sanitize(rel))
		}
	}
	return nil
}

// checkUnder applies the privacy rules to abs, which lies inside base: its
// components below base must pass checkRel and, on Windows, none may carry
// the hidden attribute.
func checkUnder(base, abs string) error {
	rel, err := filepath.Rel(base, abs)
	if err != nil {
		return fmt.Errorf("I can't use %s.", sanitize(abs))
	}
	if foldPaths() && within(base, abs) && strings.HasPrefix(rel, "..") {
		// base and abs differ only in case: skip base's components.
		n := len(strings.Split(filepath.ToSlash(filepath.Clean(base)), "/"))
		parts := strings.Split(filepath.ToSlash(filepath.Clean(abs)), "/")
		if n > len(parts) {
			n = len(parts)
		}
		rel = filepath.FromSlash(strings.Join(parts[n:], "/"))
	}
	if err := checkRel(rel); err != nil {
		return err
	}
	cur := base
	for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
		if c == "" || c == "." {
			continue
		}
		cur = filepath.Join(cur, c)
		if hiddenAttr(cur) {
			return fmt.Errorf("%s is hidden, so I won't use it.", sanitize(rel))
		}
	}
	return nil
}

// hasControl reports whether s contains a control or invisible formatting
// character.
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// sanitize replaces control and invisible formatting characters (ESC, bidi
// overrides, ...) and invalid UTF-8 with '?', so a file name can't change how
// text is displayed to the user or to Claude.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}
