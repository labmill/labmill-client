// Package sshconfig manages Labmill's SSH settings in OpenSSH format, so a user
// who later moves to a terminal can simply run `ssh <alias>`.
//
// Labmill owns one file, ~/.ssh/config.d/labmill, and adds a single
// "Include config.d/labmill" line at the top of ~/.ssh/config (it must be at
// the top: an Include after a Host/Match block only applies inside that
// block). The user's own entries are never rewritten. Labmill's Go SSH client
// resolves aliases by reading the Labmill file first and the user's file second,
// first value wins, which is exactly what OpenSSH does with the Include at the
// top, without depending on the parser following Include.
package sshconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/kevinburke/ssh_config"
)

// IncludeLine is the directive EnsureInclude puts at the top of ~/.ssh/config.
const IncludeLine = "Include config.d/labmill"

// Host is one Host block Labmill writes or resolves.
type Host struct {
	Alias        string // the Host pattern, a single literal name
	HostName     string // real DNS name; empty means same as Alias
	User         string
	Port         int    // 0 means the default, 22
	ProxyJump    string // jump host(s) in OpenSSH syntax, e.g. "bastion" or "a,b"
	IdentityFile string // optional private key path; Lookup sets it to IdentityFiles[0]

	// IdentityFiles are all private key paths, in config order (OpenSSH tries
	// every IdentityFile line, not just the first). Lookup fills it and keeps
	// IdentityFile as its first element for compatibility. Render and Validate
	// use IdentityFiles when it is not empty and IdentityFile otherwise.
	IdentityFiles []string
}

// Paths locates the SSH configuration files. Tests point SSHDir at a temp dir.
type Paths struct {
	SSHDir string // usually <home>/.ssh, on Windows too
}

// DefaultPaths returns Paths for the current user's home directory.
func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("find home directory: %w", err)
	}
	return Paths{SSHDir: filepath.Join(home, ".ssh")}, nil
}

// UserConfig is the user's own config file, <SSHDir>/config.
func (p Paths) UserConfig() string { return filepath.Join(p.SSHDir, "config") }

// LabmillConfig is the file Labmill owns, <SSHDir>/config.d/labmill.
func (p Paths) LabmillConfig() string { return filepath.Join(p.SSHDir, "config.d", "labmill") }

// Render produces the full content of the Labmill file for hosts, starting with
// a comment saying the file is managed by Labmill. Per host it writes Host,
// HostName (if set), User (if set), Port (if not 0/22), ProxyJump (if set),
// IdentityFile (if set) and "ServerAliveInterval 30". When goos is not
// "windows" it also writes "ControlMaster auto",
// "ControlPath ~/.ssh/controlmasters/%r@%h:%p" and "ControlPersist 10m";
// Win32-OpenSSH errors on those, so they are left out on Windows. Hosts are
// written in the given order; output is deterministic.
func Render(hosts []Host, goos string) []byte {
	var b bytes.Buffer
	b.WriteString("# Managed by Labmill. Edit with care: Labmill may rewrite this file.\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "\nHost %s\n", h.Alias)
		if h.HostName != "" {
			fmt.Fprintf(&b, "    HostName %s\n", h.HostName)
		}
		if h.User != "" {
			fmt.Fprintf(&b, "    User %s\n", h.User)
		}
		if h.Port != 0 && h.Port != 22 {
			fmt.Fprintf(&b, "    Port %d\n", h.Port)
		}
		if h.ProxyJump != "" {
			fmt.Fprintf(&b, "    ProxyJump %s\n", h.ProxyJump)
		}
		for _, id := range h.identityFiles() {
			if strings.ContainsAny(id, " \t") {
				id = `"` + id + `"`
			}
			fmt.Fprintf(&b, "    IdentityFile %s\n", id)
		}
		b.WriteString("    ServerAliveInterval 30\n")
		if goos != "windows" {
			b.WriteString("    ControlMaster auto\n")
			b.WriteString("    ControlPath ~/.ssh/controlmasters/%r@%h:%p\n")
			b.WriteString("    ControlPersist 10m\n")
		}
	}
	return b.Bytes()
}

// WriteHosts validates every host (Host.Validate) and refuses to write
// anything if one fails. Otherwise it writes Render(hosts, goos) to LabmillConfig atomically (temp file +
// rename), creating <SSHDir> (0700) and config.d (0700) as needed, file mode
// 0600. When goos is not "windows" it also creates <SSHDir>/controlmasters
// (0700) so the ControlPath works for terminal ssh.
func WriteHosts(p Paths, hosts []Host, goos string) error {
	for _, h := range hosts {
		if err := h.Validate(); err != nil {
			return fmt.Errorf("refusing to write ssh config: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.LabmillConfig()), 0o700); err != nil {
		return fmt.Errorf("create ssh config directory: %w", err)
	}
	if goos != "windows" {
		if err := os.MkdirAll(filepath.Join(p.SSHDir, "controlmasters"), 0o700); err != nil {
			return fmt.Errorf("create controlmasters directory: %w", err)
		}
	}
	return atomicWrite(p.LabmillConfig(), Render(hosts, goos))
}

// EnsureInclude makes IncludeLine the first non-comment, non-blank directive
// of UserConfig.
//   - Missing file: create it (0600, SSHDir 0700) containing only the line.
//   - Line already the first directive (case-insensitive keyword, surrounding
//     whitespace ignored): do nothing, changed=false, no backup.
//   - Otherwise: copy the current file to "<config>.labmill-backup-<now in
//     20060102-150405 format>" (0600), then write the new content (the line, a
//     blank line, then the original content unchanged, minus any other copy of
//     IncludeLine) atomically. changed=true.
//
// The original content is otherwise preserved byte for byte.
func EnsureInclude(p Paths, now time.Time) (changed bool, err error) {
	path := p.UserConfig()
	// Write through a symlinked ~/.ssh/config instead of replacing the link.
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&fs.ModeSymlink != 0 {
		target, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			return false, fmt.Errorf("resolve symlink %s: %w", path, rerr)
		}
		path = target
	}
	orig, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(p.SSHDir, 0o700); err != nil {
			return false, fmt.Errorf("create %s: %w", p.SSHDir, err)
		}
		return true, atomicWrite(path, []byte(IncludeLine+"\n"))
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	const bom = "\xef\xbb\xbf"
	text := string(orig)
	prefix := ""
	if strings.HasPrefix(text, bom) {
		prefix = bom
		text = strings.TrimPrefix(text, bom)
	}
	eol := "\n"
	if i := strings.Index(text, "\n"); i > 0 && text[i-1] == '\r' {
		eol = "\r\n"
	}

	lines := strings.SplitAfter(text, "\n")
	first := true
	var rest strings.Builder
	for _, line := range lines {
		t := strings.TrimSpace(line)
		isInclude := p.isIncludeLine(t)
		if t != "" && !strings.HasPrefix(t, "#") {
			if first && isInclude {
				return false, nil
			}
			first = false
		}
		if isInclude {
			continue
		}
		rest.WriteString(line)
	}

	backup := path + ".labmill-backup-" + now.Format("20060102-150405")
	bf, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create backup %s: %w", backup, err)
	}
	if _, err := bf.Write(orig); err != nil {
		bf.Close()
		return false, fmt.Errorf("write backup %s: %w", backup, err)
	}
	if err := bf.Close(); err != nil {
		return false, fmt.Errorf("write backup %s: %w", backup, err)
	}

	content := prefix + IncludeLine + eol
	if rest.Len() > 0 {
		content += eol + rest.String()
	}
	return true, atomicWrite(path, []byte(content))
}

// Lookup resolves alias the way OpenSSH would with the Include at the top:
// keywords are looked up in LabmillConfig first, then UserConfig; for each
// keyword the first value found wins, except IdentityFile, of which all
// values are returned (Host.IdentityFiles; IdentityFile is the first). Host
// patterns are matched case-sensitively and Match host lists
// case-insensitively, as OpenSSH does. HostName
// defaults to alias, with %h (the alias) and %% expanded; an allowed
// "Match host" block is matched against the HostName resolved so far, or the
// alias if none is set yet. An unsupported %-token in HostName (anything
// but %h and %%) is an error, as for ssh. "ProxyJump none" means no jump (ProxyJump is
// empty). Port defaults to 22. Missing files are treated as empty. If neither
// file has a Host block that matches alias literally or by pattern, Lookup
// still returns a Host with HostName=alias (plain `ssh alias` would work too)
// and found=false.
func Lookup(p Paths, alias string) (h Host, found bool, err error) {
	cfgs, err := load(p)
	if err != nil {
		return Host{}, false, err
	}
	vals, err := resolve(cfgs, alias)
	if err != nil {
		return Host{}, false, err
	}
	first := func(key string) string {
		if v := vals[key]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	h = Host{
		Alias:         alias,
		HostName:      first("hostname"),
		User:          first("user"),
		Port:          22,
		ProxyJump:     first("proxyjump"),
		IdentityFiles: vals["identityfile"],
	}
	if len(h.IdentityFiles) > 0 {
		h.IdentityFile = h.IdentityFiles[0]
	}
	if strings.EqualFold(h.ProxyJump, "none") {
		h.ProxyJump = ""
	}
	if h.HostName == "" {
		h.HostName = alias
	}
	if v := first("port"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			return Host{}, false, fmt.Errorf("ssh config: invalid Port %q for %s", v, alias)
		}
		h.Port = n
	}
	for _, seg := range cfgs {
		if !seg.reqAllows(alias) {
			continue
		}
		for _, blk := range hostBlocks(seg) {
			if blockMatches(blk, alias) {
				return h, true, nil
			}
		}
	}
	return h, false, nil
}

// resolve evaluates the blocks of cfgs in order for alias and returns the
// values per lower-cased keyword, in config order. Host blocks match the
// alias; blocks rewritten from "Match host" match the HostName resolved so
// far, falling back to the alias. HostName is expanded (%h, %%) when it is
// first set.
func resolve(cfgs []cfgSeg, alias string) (map[string][]string, error) {
	vals := map[string][]string{}
	dec := decisions{}
	for _, seg := range cfgs {
		if !seg.reqMatch(alias, vals, dec) {
			continue
		}
		for _, blk := range seg.c.Hosts {
			if !blockApplies(blk, alias, vals, dec) {
				continue
			}
			for _, n := range blk.Nodes {
				kv, ok := n.(*ssh_config.KV)
				if !ok || kv.Value == "" {
					continue
				}
				key := strings.ToLower(kv.Key)
				switch key {
				case "identityfile":
					vals[key] = append(vals[key], kv.Value)
				case "hostname":
					if len(vals[key]) == 0 {
						hn, err := expandHostName(kv.Value, alias)
						if err != nil {
							return nil, err
						}
						vals[key] = []string{hn}
					}
				default:
					if len(vals[key]) == 0 {
						vals[key] = []string{kv.Value}
					}
				}
			}
		}
	}
	return vals, nil
}

// expandHostName expands %h (the alias as given) and %% in a HostName value.
// These are the only tokens OpenSSH accepts for HostName (readconf.c calls
// percent_expand with just "h"); any other token, or a trailing lone "%", is
// an error, as it is for ssh.
func expandHostName(v, alias string) (string, error) {
	if !strings.Contains(v, "%") {
		return v, nil
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '%' {
			b.WriteByte(v[i])
			continue
		}
		if i+1 >= len(v) {
			return "", fmt.Errorf("ssh config: HostName %q ends with a lone %%", v)
		}
		i++
		switch v[i] {
		case 'h':
			b.WriteString(alias)
		case '%':
			b.WriteByte('%')
		default:
			return "", fmt.Errorf("ssh config: HostName %q uses unsupported token %%%c (only %%h and %%%% are allowed)", v, v[i])
		}
	}
	return b.String(), nil
}

// hostMatches is ssh_config.Host.Matches with negation handling. OpenSSH
// matches Host patterns case-sensitively but "Match host" lists
// case-insensitively (verified with ssh -G, OpenSSH 9.6), so fold selects
// the latter.
func hostMatches(blk *ssh_config.Host, name string, fold bool) bool {
	if fold {
		name = strings.ToLower(name)
	}
	found := false
	for _, pat := range blk.Patterns {
		text := pat.String()
		if fold {
			text = strings.ToLower(text)
		}
		neg := strings.HasPrefix(text, "!")
		lp, err := ssh_config.NewPattern(strings.TrimPrefix(text, "!"))
		if err != nil || !(&ssh_config.Host{Patterns: []*ssh_config.Pattern{lp}}).Matches(name) {
			continue
		}
		if neg {
			return false
		}
		found = true
	}
	return found
}

// FindAliasForHostName returns the first alias (in LabmillConfig, then
// UserConfig) whose Host block sets HostName equal to hostname
// (case-insensitive), or whose single literal pattern equals hostname. Only
// literal patterns count (no wildcards, no negations). Used to reuse a user's
// existing alias instead of adding a duplicate.
func FindAliasForHostName(p Paths, hostname string) (alias string, ok bool, err error) {
	cfgs, err := load(p)
	if err != nil {
		return "", false, err
	}
	alias, ok = aliasIn(cfgs, hostname)
	return alias, ok, nil
}

// aliasIn finds the first literal alias for hostname in cfgs. A HostName
// counts when it follows an Include inside the Host block too: the text after
// such an Include (and the included files) is loaded as separate blocks scoped
// to the same Host line. Pieces that came from an Include count for an alias
// only if every enclosing block is a plain Host block matching that alias
// (see reqAllows), so a HostName never gets attributed to an alias ssh would
// not apply it to.
func aliasIn(cfgs []cfgSeg, hostname string) (string, bool) {
	for _, seg := range cfgs {
		for _, blk := range hostBlocks(seg) {
			if len(blk.Patterns) != 1 {
				continue
			}
			pat := blk.Patterns[0].String()
			if strings.ContainsAny(pat, "*?!") || !seg.reqAllows(pat) {
				continue
			}
			if strings.EqualFold(pat, hostname) && effectiveHostName(cfgs, pat, hostname) {
				return pat, true
			}
			if hasHostName(blk, hostname) && effectiveHostName(cfgs, pat, hostname) {
				return pat, true
			}
			for _, s2 := range cfgs {
				if !s2.reqAllows(pat) {
					continue
				}
				for _, sb := range s2.c.Hosts {
					if isScopeBlock(sb) && len(sb.Patterns) == 1 && sb.Patterns[0].String() == pat && hasHostName(sb, hostname) && effectiveHostName(cfgs, pat, hostname) {
						return pat, true
					}
				}
				// Top-level directives of a file included from within a Host
				// block named pat (blocks inside it are conditional, see cfgSeg).
				if s2.inBlockNamed(pat) && hasHostName(s2.c.Hosts[0], hostname) && effectiveHostName(cfgs, pat, hostname) {
					return pat, true
				}
			}
		}
	}
	return "", false
}

// effectiveHostName reports whether ssh would connect alias to hostname: the
// first HostName value obtained across all blocks and includes (as Lookup
// resolves it) equals hostname. A HostName that a block earlier in the
// config already overrode does not make alias an alias for hostname.
func effectiveHostName(cfgs []cfgSeg, alias, hostname string) bool {
	vals, err := resolve(cfgs, alias)
	if err != nil {
		return false
	}
	hn := alias
	if v := vals["hostname"]; len(v) > 0 {
		hn = v[0]
	}
	return strings.EqualFold(hn, hostname)
}

// inBlockNamed reports whether one of the enclosing blocks of s is a plain
// Host block whose only pattern is exactly pat.
func (s cfgSeg) inBlockNamed(pat string) bool {
	for _, r := range s.req {
		if !isMatchHostBlock(r) && len(r.Patterns) == 1 && r.Patterns[0].String() == pat {
			return true
		}
	}
	return false
}

func hasHostName(blk *ssh_config.Host, hostname string) bool {
	for _, n := range blk.Nodes {
		if kv, ok := n.(*ssh_config.KV); ok && strings.EqualFold(kv.Key, "HostName") && strings.EqualFold(kv.Value, hostname) {
			return true
		}
	}
	return false
}

// isScopeBlock reports whether blk is a marked continuation of a Host block
// (not a Match one).
func isScopeBlock(blk *ssh_config.Host) bool {
	return strings.Contains(blk.EOLComment, "labmill-match") && !isMatchHostBlock(blk)
}

// UniqueAlias returns want if no Host pattern in either file equals it
// (case-insensitive), otherwise want-2, want-3, ... the first free one.
func UniqueAlias(p Paths, want string) (string, error) {
	cfgs, err := load(p)
	if err != nil {
		return "", err
	}
	taken := func(a string) bool {
		for _, seg := range cfgs {
			for _, blk := range hostBlocks(seg) {
				for _, pat := range blk.Patterns {
					if strings.EqualFold(pat.String(), a) {
						return true
					}
				}
			}
		}
		return false
	}
	alias := want
	for n := 2; taken(alias); n++ {
		alias = fmt.Sprintf("%s-%d", want, n)
	}
	return alias, nil
}

// isIncludeLine reports whether the trimmed line is an Include of Labmill's own
// file: keyword case-insensitive, optional "=", optionally quoted path, a
// trailing "# comment" allowed, and the path given as config.d/labmill,
// ~/.ssh/config.d/labmill or the absolute <SSHDir>/config.d/labmill.
func (p Paths) isIncludeLine(t string) bool {
	key, args := parseDirective(t)
	if !strings.EqualFold(key, "Include") || len(args) != 1 {
		return false
	}
	a := args[0]
	switch a {
	case "config.d/labmill", "~/.ssh/config.d/labmill":
		return true
	}
	abs := p.LabmillConfig()
	return a == abs || filepath.ToSlash(a) == filepath.ToSlash(abs) || filepath.Clean(a) == abs
}

// parseDirective splits one config line into its keyword and arguments. The
// keyword ends at whitespace or '='; one optional '=' separates it from the
// arguments; arguments may be double-quoted; an unquoted word starting with
// '#' begins a comment. Blank and comment lines give an empty key.
func parseDirective(line string) (key string, args []string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", nil
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return line, nil
	}
	key = line[:i]
	rest := strings.TrimLeft(line[i:], " \t")
	if strings.HasPrefix(rest, "=") {
		rest = strings.TrimLeft(rest[1:], " \t")
	}
	for rest != "" {
		if rest[0] == '#' {
			break
		}
		var w string
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				w, rest = rest[1:], ""
			} else {
				w, rest = rest[1:1+end], rest[2+end:]
			}
		} else {
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			w, rest = rest[:end], rest[end:]
		}
		args = append(args, w)
		rest = strings.TrimLeft(rest, " \t")
	}
	return key, args
}

const (
	skipPrefix  = "# labmill-skip: "
	matchMarker = "# labmill-match" // EOL comment on a Host line that scopes an Include (not an alias)
	// matchHostMarker marks a Host line rewritten from Match host/all; its
	// values apply by the HostName resolved so far (see resolve). It contains
	// matchMarker, so hostBlocks drops it as well.
	matchHostMarker = "# labmill-match-host" // followed by ":<id>"
	maxIncDepth     = 4
)

// matchSeq numbers the Match blocks (see decisions); IDs only need to be unique.
var matchSeq atomic.Int64

// neverHost is a scope line that matches no alias ("!*" negates everything).
const neverHost = "Host !* " + matchMarker

// segment is one piece of a preprocessed file, in source order: either text
// for the parser library, or an Include to be expanded in place.
type segment struct {
	text    string   // parser-ready text (when include is nil)
	include []string // arguments of an Include line
	scope   string   // for an include: the enclosing Host line, "" at top level
}

// preprocess prepares one file's text for the parser library and splits it at
// its Include lines, so the caller can load the included files in place (as
// OpenSSH does). It comments out every Match block that is not exactly
// "Match all" or "Match host <list>", rewrites the allowed ones as "Host"
// lines (marked with matchHostMarker) so the library applies their values to
// matching aliases, and normalizes Host lines to single-space separated
// patterns. An Include inside a Host or Match block is scoped to that block:
// the text following it is prefixed with the block's Host line (marked, so it
// does not define an alias), and the include segment records that line so the
// loader can make the included files' blocks conditional on it (see cfgSeg).
func preprocess(data []byte) []segment {
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	var segs []segment
	var b strings.Builder
	cur := ""
	start := func() {
		b.Reset()
		if cur != "" {
			b.WriteString(cur + "\n")
		}
	}
	start()
	skipping := false
	for _, line := range strings.SplitAfter(text, "\n") {
		key, args := parseDirective(line)
		body := strings.TrimRight(line, "\r\n")
		nl := line[len(body):]
		switch {
		case strings.EqualFold(key, "Match"):
			skipping = false
			if pats, ok := matchHostPatterns(args); ok {
				cur = fmt.Sprintf("Host %s %s:%d", strings.Join(pats, " "), matchHostMarker, matchSeq.Add(1))
				b.WriteString(cur + nl)
			} else {
				skipping = true
				b.WriteString(skipPrefix + body + nl)
			}
		case strings.EqualFold(key, "Host"):
			skipping = false
			out := line
			if len(args) > 0 && !strings.ContainsAny(strings.Join(args, ""), " \t\"'") {
				out = "Host " + strings.Join(args, " ")
				if c := trailingComment(body); c != "" {
					out += " " + c
				}
				out += nl
				cur = "Host " + strings.Join(args, " ") + " " + matchMarker
			} else {
				cur = neverHost // an Include below must not apply anywhere
			}
			b.WriteString(out)
		case skipping:
			b.WriteString(skipPrefix + body + nl)
		case strings.EqualFold(key, "Include"):
			if b.Len() > 0 {
				segs = append(segs, segment{text: b.String()})
			}
			segs = append(segs, segment{include: args, scope: cur})
			start()
		default:
			b.WriteString(line)
		}
	}
	if b.Len() > 0 {
		segs = append(segs, segment{text: b.String()})
	}
	return segs
}

// trailingComment returns the unquoted "# ..." comment ending line, or "".
func trailingComment(line string) string {
	inQuote := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '"':
			inQuote = !inQuote
		case c == '#' && !inQuote && i > 0 && (line[i-1] == ' ' || line[i-1] == '\t'):
			return line[i:]
		}
	}
	return ""
}

// matchHostPatterns returns the host patterns of a Match line that is exactly
// "all" or "host <list>", where <list> is one comma-separated argument without
// whitespace. Any further word would be another criterion (!exec, version,
// ...) that we can't evaluate, so such a line is rejected.
func matchHostPatterns(args []string) ([]string, bool) {
	if len(args) == 1 && strings.EqualFold(args[0], "all") {
		return []string{"*"}, true
	}
	if len(args) != 2 || !strings.EqualFold(args[0], "host") {
		return nil, false
	}
	if strings.ContainsAny(args[1], " \t\"'=") {
		return nil, false
	}
	var pats []string
	for _, f := range strings.Split(args[1], ",") {
		if f != "" {
			pats = append(pats, f)
		}
	}
	return pats, len(pats) > 0
}

// load parses LabmillConfig then UserConfig, with the files each Includes
// expanded in place (resolved against SSHDir, never the real home). Missing
// files are skipped. The result is in source order.
func load(p Paths) ([]cfgSeg, error) {
	var cfgs []cfgSeg
	for _, path := range []string{p.LabmillConfig(), p.UserConfig()} {
		if err := p.loadFile(path, 0, nil, &cfgs); err != nil {
			return nil, err
		}
	}
	return cfgs, nil
}

// cfgSeg is one parsed piece of a config file. req lists the Host/Match
// blocks of the including files that enclose the Include it came from: as in
// OpenSSH, every block of the piece applies only while all of them match
// (an Include inside a block that doesn't match the host is not processed at
// all), on top of the piece's own Host/Match lines.
type cfgSeg struct {
	c   *ssh_config.Config
	req []*ssh_config.Host
}

// reqMatch reports whether every enclosing block of the segment matches. A
// Match host block compares against the HostName resolved so far, as in resolve.
func (s cfgSeg) reqMatch(alias string, vals map[string][]string, dec decisions) bool {
	for _, r := range s.req {
		if !blockApplies(r, alias, vals, dec) {
			return false
		}
	}
	return true
}

// decisions records, per Match block ID, whether the block matched when
// resolve reached its own Match line. OpenSSH decides that once: a HostName
// set later in the block does not re-evaluate it, so an Include (or the text
// after it) inside the block keeps the original decision.
type decisions map[int]bool

// matchID returns the ID preprocess gave a block rewritten from a Match line,
// or -1 if it has none.
func matchID(blk *ssh_config.Host) int {
	_, rest, ok := strings.Cut(blk.EOLComment, "labmill-match-host:")
	if !ok {
		return -1
	}
	n := 0
	i := 0
	for ; i < len(rest) && rest[i] >= '0' && rest[i] <= '9'; i++ {
		n = n*10 + int(rest[i]-'0')
	}
	if i == 0 {
		return -1
	}
	return n
}

// blockApplies reports whether blk matches alias (or, for a block rewritten
// from Match host, the HostName resolved so far in vals). A Match block is
// evaluated once, at its first occurrence, and the result is kept in dec for
// the continuation blocks that follow an Include inside it.
func blockApplies(blk *ssh_config.Host, alias string, vals map[string][]string, dec decisions) bool {
	name := alias
	isMatch := isMatchHostBlock(blk)
	id := matchID(blk)
	if id >= 0 {
		if d, ok := dec[id]; ok {
			return d
		}
	}
	if isMatch && len(vals["hostname"]) > 0 {
		name = vals["hostname"][0]
	}
	d := hostMatches(blk, name, isMatch)
	if id >= 0 {
		dec[id] = d
	}
	return d
}

// scopeBlock parses a scope line made by preprocess into its Host block.
func scopeBlock(line string) *ssh_config.Host {
	c, err := ssh_config.DecodeBytes([]byte(line + "\n"))
	if err != nil || len(c.Hosts) < 2 {
		return nil
	}
	return c.Hosts[1]
}

func (p Paths) loadFile(path string, depth int, req []*ssh_config.Host, cfgs *[]cfgSeg) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, seg := range preprocess(data) {
		if seg.include == nil && seg.text != "" {
			c, err := ssh_config.DecodeBytes([]byte(seg.text))
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			*cfgs = append(*cfgs, cfgSeg{c: c, req: req})
			continue
		}
		if depth >= maxIncDepth {
			continue
		}
		inner := req
		if seg.scope != "" {
			sb := scopeBlock(seg.scope)
			if sb == nil {
				continue
			}
			inner = append(append([]*ssh_config.Host(nil), req...), sb)
		}
		for _, inc := range seg.include {
			for _, f := range p.expandInclude(inc) {
				if sameFile(f, p.LabmillConfig()) || sameFile(f, p.UserConfig()) {
					continue // the roots are loaded by load itself
				}
				if err := p.loadFile(f, depth+1, inner, cfgs); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func sameFile(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

// expandInclude resolves one Include argument to existing regular files:
// "~/.ssh/..." and relative paths are taken relative to SSHDir, absolute paths
// as is, and globs expand in sorted order. Other "~" paths are ignored.
func (p Paths) expandInclude(arg string) []string {
	pat := arg
	switch {
	case strings.HasPrefix(pat, "~/.ssh/"):
		pat = filepath.Join(p.SSHDir, filepath.FromSlash(strings.TrimPrefix(pat, "~/.ssh/")))
	case strings.HasPrefix(pat, "~"):
		return nil
	case !filepath.IsAbs(pat):
		pat = filepath.Join(p.SSHDir, filepath.FromSlash(pat))
	}
	matches, err := filepath.Glob(pat)
	if err != nil {
		return nil
	}
	// Like OpenSSH's glob, don't match dotfiles (editor swap files) unless
	// the pattern's last element itself starts with a dot.
	dotOK := strings.HasPrefix(filepath.Base(pat), ".")
	var out []string
	for _, m := range matches {
		if !dotOK && strings.HasPrefix(filepath.Base(m), ".") {
			continue
		}
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
			out = append(out, m)
		}
	}
	return out
}

func isMatchHostBlock(blk *ssh_config.Host) bool {
	return strings.Contains(blk.EOLComment, "labmill-match-host")
}

// hostBlocks returns the real Host blocks of seg: it drops the parser's
// implicit leading "Host *" (always Hosts[0]) and the blocks rewritten from
// Match lines, which configure values but don't define aliases. Blocks of a
// file included from within another block are returned too; whether they
// define an alias depends on the candidate (see reqAllows).
func hostBlocks(seg cfgSeg) []*ssh_config.Host {
	c := seg.c
	if len(c.Hosts) < 2 {
		return nil
	}
	var out []*ssh_config.Host
	for _, h := range c.Hosts[1:] {
		if strings.Contains(h.EOLComment, "labmill-match") {
			continue
		}
		out = append(out, h)
	}
	return out
}

// reqAllows reports whether the Include chain that produced seg is open for
// the literal alias name: every enclosing block must be a plain Host block
// (not a Match one, whose outcome depends on the HostName) that matches name.
func (s cfgSeg) reqAllows(name string) bool {
	for _, r := range s.req {
		if isMatchHostBlock(r) || !hostMatches(r, name, false) {
			return false
		}
	}
	return true
}

// blockMatches reports whether blk matches alias through at least one
// pattern that is more specific than a bare wildcard, so that "Host *" does
// not make every alias count as configured.
func blockMatches(blk *ssh_config.Host, alias string) bool {
	if !hostMatches(blk, alias, false) {
		return false
	}
	for _, pat := range blk.Patterns {
		str := pat.String()
		if strings.HasPrefix(str, "!") || strings.Trim(str, "*?") == "" {
			continue
		}
		if hostMatches(&ssh_config.Host{Patterns: []*ssh_config.Pattern{pat}}, alias, false) {
			return true
		}
	}
	return false
}

// AtomicWrite is atomicWrite for other packages.
func AtomicWrite(path string, data []byte) error { return atomicWrite(path, data) }

// atomicWrite writes data to path via a temp file in the same directory,
// with mode 0600.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".labmill-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name) // our own temp file
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(name, path); err != nil {
		return fail(err)
	}
	return nil
}

// ValidAlias reports whether s can be written as a single literal Host
// pattern: not empty, not starting with '-' (it would read as an ssh option),
// no whitespace or control characters, and none of
// * ? ! , # " ' = (wildcards, negation, list separators, comments, quoting).
// config.Validate uses it for ssh_host and files_host.
func ValidAlias(s string) error {
	if s == "" {
		return errors.New("alias is empty")
	}
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("alias %q must not start with '-'", s)
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`*?!,#"'=`, r) {
			return fmt.Errorf("alias %q contains invalid character %q", s, r)
		}
	}
	return nil
}

// Validate checks that h can be rendered safely: a valid Alias (ValidAlias);
// HostName, User and ProxyJump free of whitespace, control characters, '#'
// and quotes, and not starting with '-' (ProxyJump may contain ',' '@' ':' for "a,user@b:22"); Port in
// 0..65535; IdentityFile free of control characters and '"' (Render quotes it
// when it contains spaces). A value that fails would otherwise inject extra
// directives (e.g. "h\n ProxyCommand ...") into a file every ssh reads.
func (h Host) Validate() error {
	if err := ValidAlias(h.Alias); err != nil {
		return err
	}
	for _, f := range []struct{ name, v string }{
		{"HostName", h.HostName}, {"User", h.User}, {"ProxyJump", h.ProxyJump},
	} {
		if strings.HasPrefix(f.v, "-") {
			return fmt.Errorf("host %s: %s %q must not start with '-'", h.Alias, f.name, f.v)
		}
		for _, r := range f.v {
			if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(`#"'`, r) {
				return fmt.Errorf("host %s: %s %q contains invalid character %q", h.Alias, f.name, f.v, r)
			}
		}
	}
	if h.Port < 0 || h.Port > 65535 {
		return fmt.Errorf("host %s: invalid Port %d", h.Alias, h.Port)
	}
	for _, id := range h.identityFiles() {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("host %s: empty IdentityFile", h.Alias)
		}
		for _, r := range id {
			if unicode.IsControl(r) || r == '"' {
				return fmt.Errorf("host %s: IdentityFile %q contains invalid character %q", h.Alias, id, r)
			}
		}
	}
	return nil
}

// identityFiles is IdentityFiles, or IdentityFile alone when that is empty.
func (h Host) identityFiles() []string {
	if len(h.IdentityFiles) > 0 {
		return h.IdentityFiles
	}
	if h.IdentityFile != "" {
		return []string{h.IdentityFile}
	}
	return nil
}

// AliasSource is like FindAliasForHostName but also reports whether the alias
// was found in Labmill's own file (inLabmill) or in the user's config (or a file
// it includes). First-run setup uses it: an alias from the user's own config
// is referenced, never rewritten into Labmill's file, since Labmill's file is read
// first and would override the user's settings.
func AliasSource(p Paths, hostname string) (alias string, inLabmill, ok bool, err error) {
	var ycfgs, ucfgs []cfgSeg
	if err := p.loadFile(p.LabmillConfig(), 0, nil, &ycfgs); err != nil {
		return "", false, false, err
	}
	if alias, ok = aliasIn(ycfgs, hostname); ok {
		return alias, true, true, nil
	}
	if err := p.loadFile(p.UserConfig(), 0, nil, &ucfgs); err != nil {
		return "", false, false, err
	}
	alias, ok = aliasIn(ucfgs, hostname)
	return alias, false, ok, nil
}
