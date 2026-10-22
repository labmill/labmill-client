package sshconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Listing and editing hosts for the desktop app's Settings page (M29). Only
// Labmill's own file is ever written; the user's ~/.ssh/config (and the files
// it includes) is read to list its hosts and is never rewritten.

// Entry is one host for the Settings page.
type Entry struct {
	Host
	// InLabmill: the alias is defined in Labmill's own file (editable there);
	// false means the user's own config defines it (shown read-only).
	InLabmill bool
}

// managedKeys are the directives the Settings page edits in a Labmill host
// block, in the order new ones are written.
var managedKeys = []string{"HostName", "User", "Port", "ProxyJump"}

// ListHosts returns the hosts a user can pick for a cluster: first every
// literal single-name Host block of Labmill's file (InLabmill, with the values
// written in that block itself: HostName, User, Port and ProxyJump; Port 0
// when the block sets none), then those of the user's config and the files
// it includes (resolved with Lookup, the values ssh would use), each alias
// once. Wildcard and multi-pattern blocks are left out. Missing files are
// empty.
func ListHosts(p Paths) ([]Entry, error) {
	var out []Entry
	seen := map[string]bool{}
	data, err := os.ReadFile(p.LabmillConfig())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", p.LabmillConfig(), err)
	}
	lines := splitLines(string(data))
	for _, b := range hostBlocksIn(lines) {
		if seen[b.alias] {
			continue
		}
		seen[b.alias] = true
		h := Host{Alias: b.alias}
		for _, l := range lines[b.start+1 : b.end] {
			key, args := parseDirective(l)
			if len(args) == 0 {
				continue
			}
			switch {
			case strings.EqualFold(key, "HostName") && h.HostName == "":
				h.HostName = args[0]
			case strings.EqualFold(key, "User") && h.User == "":
				h.User = args[0]
			case strings.EqualFold(key, "Port") && h.Port == 0:
				if n, err := strconv.Atoi(args[0]); err == nil && n > 0 && n <= 65535 {
					h.Port = n
				}
			case strings.EqualFold(key, "ProxyJump") && h.ProxyJump == "":
				h.ProxyJump = args[0]
			case strings.EqualFold(key, "IdentityFile"):
				h.IdentityFiles = append(h.IdentityFiles, args[0])
			}
		}
		if len(h.IdentityFiles) > 0 {
			h.IdentityFile = h.IdentityFiles[0]
		}
		out = append(out, Entry{Host: h, InLabmill: true})
	}
	var ucfgs []cfgSeg
	if err := p.loadFile(p.UserConfig(), 0, nil, &ucfgs); err != nil {
		return nil, err
	}
	for _, seg := range ucfgs {
		for _, blk := range hostBlocks(seg) {
			if len(blk.Patterns) != 1 {
				continue
			}
			pat := blk.Patterns[0].String()
			if strings.ContainsAny(pat, "*?!") || !seg.reqAllows(pat) || seen[pat] {
				continue
			}
			seen[pat] = true
			h, _, err := Lookup(p, pat)
			if err != nil {
				h = Host{Alias: pat}
			}
			out = append(out, Entry{Host: h})
		}
	}
	return out, nil
}

// PutLabmillHost writes h into Labmill's file: when a literal "Host <alias>"
// block exists there, its HostName, User, Port (0 or 22: none) and ProxyJump
// lines are set to h's values (an empty value removes the line, a missing
// one is added after the block's other host lines) and every other line of
// the file stays as it is; otherwise a new block (Render's format for goos)
// is appended. h must pass Validate. It creates <SSHDir>/config.d (0700) and,
// except on Windows, <SSHDir>/controlmasters, and writes atomically (0600).
// It never touches the user's own config; the caller makes sure an alias
// that the user's config defines isn't added here (it would override it).
// created reports whether a new block was appended.
func PutLabmillHost(p Paths, h Host, goos string) (created bool, err error) {
	c, err := PutLabmillHosts(p, []Host{h}, goos)
	if err != nil {
		return false, err
	}
	return c[0], nil
}

// PutLabmillHosts is PutLabmillHost for several hosts in one atomic write: every
// host must pass Validate, the changes are built in memory one after the
// other, and Labmill's file is replaced once (nothing is written when any host
// fails). created[i] reports whether hs[i] was appended as a new block.
func PutLabmillHosts(p Paths, hs []Host, goos string) (created []bool, err error) {
	for _, h := range hs {
		if err := h.Validate(); err != nil {
			return nil, err
		}
	}
	path := p.LabmillConfig()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	text := string(data)
	for _, h := range hs {
		var c bool
		text, c = putHostText(text, h, goos)
		created = append(created, c)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create ssh config directory: %w", err)
	}
	if goos != "windows" {
		_ = os.MkdirAll(filepath.Join(p.SSHDir, "controlmasters"), 0o700)
	}
	if err := atomicWrite(path, []byte(text)); err != nil {
		return nil, err
	}
	return created, nil
}

// putHostText returns text (Labmill's file) with h set as PutLabmillHost
// describes; created reports whether a new block was appended.
func putHostText(data string, h Host, goos string) (out string, created bool) {
	lines := splitLines(data)
	var blk *lineBlock
	for _, b := range hostBlocksIn(lines) {
		if b.alias == h.Alias {
			b := b
			blk = &b
			break
		}
	}
	if blk == nil {
		block := string(Render([]Host{h}, goos))
		text := data
		if strings.TrimSpace(text) != "" {
			// The file already has the header comment.
			if i := strings.IndexByte(block, '\n'); i >= 0 {
				block = block[i+1:]
			}
			if !strings.HasSuffix(text, "\n") {
				text += "\n"
			}
		} else {
			text = ""
		}
		out = text + block
		created = true
	} else {
		port := ""
		if h.Port != 0 && h.Port != 22 {
			port = strconv.Itoa(h.Port)
		}
		vals := map[string]string{"hostname": h.HostName, "user": h.User, "port": port, "proxyjump": h.ProxyJump}
		indent := "    "
		body := append([]string(nil), lines[blk.start+1:blk.end]...)
		for _, l := range body {
			if k, _ := parseDirective(l); k != "" {
				indent = l[:len(l)-len(strings.TrimLeft(l, " \t"))]
				break
			}
		}
		var kept []string
		done := map[string]bool{}
		pos := 0 // missing keys go after the last host line kept (else first)
		for _, l := range body {
			key, _ := parseDirective(l)
			lk := strings.ToLower(key)
			v, managed := vals[lk]
			if !managed {
				kept = append(kept, l)
				continue
			}
			if done[lk] || v == "" {
				continue // a duplicate, or a value that is now empty
			}
			done[lk] = true
			kept = append(kept, indent+canonicalKey(lk)+" "+v+lineEnding(l))
			pos = len(kept)
		}
		var add []string
		for _, k := range managedKeys {
			lk := strings.ToLower(k)
			if v := vals[lk]; v != "" && !done[lk] {
				add = append(add, indent+k+" "+v+"\n")
			}
		}
		kept = append(kept[:pos], append(add, kept[pos:]...)...)
		var b strings.Builder
		for _, l := range lines[:blk.start+1] {
			b.WriteString(l)
		}
		if !strings.HasSuffix(lines[blk.start], "\n") {
			b.WriteString("\n")
		}
		for _, l := range kept {
			b.WriteString(l)
		}
		for _, l := range lines[blk.end:] {
			b.WriteString(l)
		}
		out = b.String()
	}
	return out, created
}

// RemoveLabmillHost removes the literal "Host <alias>" block from Labmill's file
// (with the comment lines directly above the next block left in place). It
// fails when Labmill's file has no such block. The user's config is never
// touched.
func RemoveLabmillHost(p Paths, alias string) error {
	path := p.LabmillConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := splitLines(string(data))
	for _, b := range hostBlocksIn(lines) {
		if b.alias != alias {
			continue
		}
		end := b.end
		if end < len(lines) {
			for end > b.start+1 && strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#") {
				end--
			}
		}
		var sb strings.Builder
		for _, l := range lines[:b.start] {
			sb.WriteString(l)
		}
		for _, l := range lines[end:] {
			sb.WriteString(l)
		}
		return atomicWrite(path, []byte(sb.String()))
	}
	return fmt.Errorf("%s is not defined in %s", alias, path)
}

// AddLabmillIdentityFile adds an "IdentityFile <path>" line to the literal
// "Host <alias>" block of Labmill's file, after the block's last directive,
// unless the block already names path. Nothing else in the file changes.
// It fails when Labmill's file has no such block (an alias the user's own
// config defines is never edited: Labmill does not touch that file). path must
// be free of control characters and '"'; paths with spaces are quoted.
// changed reports whether a line was added.
func AddLabmillIdentityFile(p Paths, alias, path string) (changed bool, err error) {
	if path == "" || strings.ContainsAny(path, "\"\r\n") || strings.IndexFunc(path, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false, fmt.Errorf("IdentityFile %q is not a plain path", path)
	}
	file := p.LabmillConfig()
	data, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", file, err)
	}
	lines := splitLines(string(data))
	for _, b := range hostBlocksIn(lines) {
		if b.alias != alias {
			continue
		}
		indent, last := "    ", b.start
		for i := b.start + 1; i < b.end; i++ {
			key, args := parseDirective(lines[i])
			if key == "" {
				continue
			}
			last = i
			indent = lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " \t"))]
			if strings.EqualFold(key, "IdentityFile") && len(args) > 0 && args[0] == path {
				return false, nil
			}
		}
		val := path
		if strings.ContainsAny(val, " \t") {
			val = `"` + val + `"`
		}
		eol := lineEnding(lines[last])
		var sb strings.Builder
		for _, l := range lines[:last+1] {
			sb.WriteString(l)
		}
		if !strings.HasSuffix(lines[last], "\n") {
			sb.WriteString(eol)
		}
		sb.WriteString(indent + "IdentityFile " + val + eol)
		for _, l := range lines[last+1:] {
			sb.WriteString(l)
		}
		if err := atomicWrite(file, []byte(sb.String())); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("%s is not defined in %s", alias, file)
}

// lineBlock is a Host block of Labmill's file by line: the "Host" line at
// start, the block running until the next Host or Match line (end,
// exclusive).
type lineBlock struct {
	alias      string
	start, end int
}

// hostBlocksIn returns the blocks of lines whose Host line names exactly one
// literal alias (no wildcard, negation or quotes), in file order.
func hostBlocksIn(lines []string) []lineBlock {
	var out []lineBlock
	cur := -1
	var alias string
	flush := func(end int) {
		if cur >= 0 && alias != "" {
			out = append(out, lineBlock{alias: alias, start: cur, end: end})
		}
	}
	for i, l := range lines {
		key, args := parseDirective(l)
		if !strings.EqualFold(key, "Host") && !strings.EqualFold(key, "Match") {
			continue
		}
		flush(i)
		cur, alias = i, ""
		if strings.EqualFold(key, "Host") && len(args) == 1 && ValidAlias(args[0]) == nil {
			alias = args[0]
		}
	}
	flush(len(lines))
	return out
}

// splitLines splits s after every newline, keeping the line endings (the
// last line has none when s doesn't end in a newline).
func splitLines(s string) []string {
	out := strings.SplitAfter(s, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// lineEnding is the line ending to use for a rewritten line l.
func lineEnding(l string) string {
	switch {
	case strings.HasSuffix(l, "\r\n"):
		return "\r\n"
	default:
		return "\n"
	}
}

func canonicalKey(lk string) string {
	for _, k := range managedKeys {
		if strings.ToLower(k) == lk {
			return k
		}
	}
	return lk
}
