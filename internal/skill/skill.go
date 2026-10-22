// Package skill is Labmill's runtime for Anthropic/Claude Code-format skills, so
// the same skills that work inside Claude Code work in a tool
// that has no Claude Code.
//
// A skill is a directory holding SKILL.md (YAML frontmatter with name and
// description, then a markdown body) and optionally references/*.md, plus
// references in subfolders (<sub>/references/*.md, e.g. one folder per
// cluster in a site skill). Claude
// sees only every skill's name and description up front (SystemPrompt). It
// pulls in a skill's body with the load_skill tool and individual reference
// files with read_reference, which is the same progressive disclosure Claude
// Code uses.
package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing/fstest"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// Skill is one loaded skill. Body and references are read lazily from the
// underlying filesystem, so large reference sets cost nothing until used.
type Skill struct {
	Name        string // frontmatter "name"; unique within a Set
	Description string // frontmatter "description", whitespace-normalized
	// References are the reference file names, sorted: "cli.md" for
	// references/cli.md, and "<sub>/cli.md" for <sub>/references/cli.md
	// (e.g. "bigiron/slurm.md"; see SubfolderName).
	References []string
	// Tools are the helper script and note files in <skill>/tools/, sorted,
	// named "tools/check-deps.sh". They are readable as text with
	// ReadReference and never run by Labmill.
	Tools  []string
	Source string // "embedded" or the override directory it came from
	// AllowedTools is the optional frontmatter "allowed-tools" (a YAML list
	// or a comma/space separated string), parsed but not enforced. It leaves
	// room to map skills to Labmill's typed tools later.
	AllowedTools []string

	fsys fs.FS  // filesystem the skill was loaded from
	dir  string // the skill's directory within fsys
}

// Set is the collection of skills available to the agent.
type Set struct {
	byName map[string]*Skill

	// Preamble, if set, is put at the top of every LoadSkillResult. The agent
	// uses it to tell Claude that skills written for a shell must be carried
	// out with Labmill's typed tools (or shown to the user), never by installing
	// software.
	Preamble string
}

// MaxFileSize caps SKILL.md and reference files from an override directory.
// Larger files are rejected with a warning at load time.
const MaxFileSize = 256 << 10

// ValidName reports whether name follows Anthropic's skill-name rule:
// ^[a-z0-9][a-z0-9-]{0,63}$.
func ValidName(name string) bool { return nameRE.MatchString(name) }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// SubfolderName reports whether dir may be a reference subfolder of a skill
// (<skill>/<dir>/references/*.md): ^[a-z0-9][a-z0-9-]{0,62}$ and not one of
// the reserved names "references", "evals", "diagrams" and "tools".
func SubfolderName(dir string) bool {
	return subRE.MatchString(dir) && dir != "references" && dir != "evals" && dir != "diagrams" && dir != "tools"
}

var subRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Load reads every top-level directory of fsys that contains a SKILL.md.
// Directories without SKILL.md are ignored. It is an error if a SKILL.md has
// no frontmatter, if name or description is missing or empty, if name differs
// from the directory name, if the name fails ValidName, or if two skills share
// a name. The frontmatter may start with a UTF-8 BOM, and its "---" marker
// lines may have trailing spaces or tabs. References are the regular *.md
// files (Type().IsRegular(): no symlinks, no directories) directly inside
// <dir>/references/ and <dir>/<sub>/references/ (site skills: References/) for every subfolder <sub>
// (a real directory, not a symlink) that passes SubfolderName (no deeper
// recursion).
// source is recorded in each Skill.Source.
func Load(fsys fs.FS, source string) (*Set, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("reading skills: %w", err)
	}
	set := &Set{byName: map[string]*Skill{}}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		k, err := loadDir(fsys, e.Name(), source, false)
		if err != nil {
			return nil, err
		}
		if k == nil {
			continue
		}
		if _, dup := set.byName[k.Name]; dup {
			return nil, fmt.Errorf("skill %q: duplicate name", k.Name)
		}
		set.byName[k.Name] = k
	}
	return set, nil
}

// readCapped reads a file, failing if it is larger than MaxFileSize.
func readCapped(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, MaxFileSize)
	}
	return data, nil
}

// loadDir loads the skill in dir, or returns nil, nil if dir has no SKILL.md.
// With snapshot set, the skill's SKILL.md and references are read into memory
// (each capped at MaxFileSize) and the skill is backed by that copy, not by
// fsys.
func loadDir(fsys fs.FS, dir, source string, snapshot bool) (*Skill, error) {
	skillPath := path.Join(dir, "SKILL.md")
	var data []byte
	var err error
	if snapshot {
		if err := checkSkillFile(fsys, dir); err != nil {
			return nil, fmt.Errorf("skill %q: %w", dir, err)
		}
		data, err = readCapped(fsys, skillPath)
	} else {
		data, err = fs.ReadFile(fsys, skillPath)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill %q: %w", dir, err)
	}
	fm, _, err := parseSkillMD(data)
	if err != nil {
		return nil, fmt.Errorf("skill %q: %w", dir, err)
	}
	if fm.Name == "" {
		return nil, fmt.Errorf("skill %q: frontmatter has no name", dir)
	}
	if !ValidName(fm.Name) {
		return nil, fmt.Errorf("skill %q: invalid name %q (want lowercase letters, digits and hyphens, at most 64 characters)", dir, fm.Name)
	}
	if fm.Name != dir {
		return nil, fmt.Errorf("skill %q: frontmatter name %q differs from directory name", dir, fm.Name)
	}
	desc := strings.Join(strings.Fields(fm.Description), " ")
	if desc == "" {
		return nil, fmt.Errorf("skill %q: frontmatter has no description", dir)
	}
	k := &Skill{Name: fm.Name, Description: desc, Source: source, AllowedTools: parseAllowedTools(fm.AllowedTools), fsys: fsys, dir: dir}
	addRefs := func(sub string) error {
		refs, err := fs.ReadDir(fsys, path.Join(dir, sub, refDirName(fsys, path.Join(dir, sub))))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		for _, r := range refs {
			if r.Type().IsRegular() && strings.HasSuffix(r.Name(), ".md") {
				k.References = append(k.References, path.Join(sub, r.Name()))
			}
		}
		return nil
	}
	if err := addRefs(""); err != nil {
		return nil, fmt.Errorf("skill %q: %w", dir, err)
	}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("skill %q: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 && SubfolderName(e.Name()) {
			if err := addRefs(e.Name()); err != nil {
				return nil, fmt.Errorf("skill %q: %w", dir, err)
			}
		}
	}
	sort.Strings(k.References)
	tools, err := fs.ReadDir(fsys, path.Join(dir, "tools"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("skill %q: %w", dir, err)
	}
	for _, t := range tools {
		n := t.Name()
		if t.Type().IsRegular() && !strings.HasPrefix(n, ".") && !strings.HasPrefix(n, "_") {
			k.Tools = append(k.Tools, "tools/"+t.Name())
		}
	}
	sort.Strings(k.Tools)
	if snapshot {
		mem := fstest.MapFS{skillPath: {Data: data}}
		for _, r := range k.References {
			p := path.Join(dir, refPath(fsys, dir, r))
			rd, err := readCapped(fsys, p)
			if err != nil {
				return nil, fmt.Errorf("skill %q: reference %q: %w", dir, r, err)
			}
			mem[p] = &fstest.MapFile{Data: rd}
		}
		for _, t := range k.Tools {
			p := path.Join(dir, t)
			rd, err := readCapped(fsys, p)
			if err != nil {
				return nil, fmt.Errorf("skill %q: tool file %q: %w", dir, t, err)
			}
			mem[p] = &fstest.MapFile{Data: rd}
		}
		k.fsys = mem
	}
	return k, nil
}

// checkSkillFile makes sure dir/SKILL.md, if present, is a regular file (or a
// symlink to one) before it is opened: opening a FIFO or device would block
// startup. A missing SKILL.md is fine (readCapped reports it as not-exist),
// but a dangling symlink is an error so it surfaces as a warning.
func checkSkillFile(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil // let the read report it
	}
	for _, e := range entries {
		if e.Name() != "SKILL.md" {
			continue
		}
		typ := e.Type()
		if typ&fs.ModeSymlink != 0 {
			info, err := fs.Stat(fsys, path.Join(dir, "SKILL.md"))
			if errors.Is(err, fs.ErrNotExist) {
				return errors.New("SKILL.md is a dangling symlink")
			}
			if err != nil {
				return fmt.Errorf("SKILL.md: %w", err)
			}
			typ = info.Mode().Type()
		}
		if !typ.IsRegular() {
			return errors.New("SKILL.md is not a regular file")
		}
	}
	return nil
}

type frontmatter struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	AllowedTools any    `yaml:"allowed-tools"`
}

// parseAllowedTools accepts a YAML list or a comma/space separated string.
// Separators inside parentheses don't split, so `Bash(git status:*), Read`
// gives ["Bash(git status:*)", "Read"]. Parentheses nest; a stray ")" is
// kept as text, and an unclosed "(" swallows the rest of the entry's text
// (the whole remainder becomes one token).
func parseAllowedTools(v any) []string {
	var out []string
	add := func(s string) {
		depth := 0
		var cur strings.Builder
		flush := func() {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
		for _, r := range s {
			switch {
			case r == '(':
				depth++
			case r == ')' && depth > 0:
				depth--
			case depth == 0 && (r == ',' || unicode.IsSpace(r)):
				flush()
				continue
			}
			cur.WriteRune(r)
		}
		flush()
	}
	switch t := v.(type) {
	case string:
		add(t)
	case []any:
		for _, e := range t {
			if e != nil {
				add(fmt.Sprint(e))
			}
		}
	}
	return out
}

// isMarker reports whether line is a frontmatter "---" marker line, allowing
// trailing spaces and tabs.
func isMarker(line string) bool {
	return strings.TrimRight(line, " \t") == "---"
}

// parseSkillMD splits SKILL.md into its YAML frontmatter and body. The file
// must start (after an optional UTF-8 BOM) with a "---" line; the frontmatter
// ends at the next "---" line. CRLF line endings are accepted.
func parseSkillMD(data []byte) (fm frontmatter, body string, err error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(text, "\n")
	if !isMarker(lines[0]) {
		return fm, "", errors.New("SKILL.md has no frontmatter (must start with ---)")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if isMarker(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return fm, "", errors.New("SKILL.md frontmatter is not terminated by ---")
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fm); err != nil {
		return fm, "", fmt.Errorf("parsing frontmatter: %w", err)
	}
	rest := lines[end+1:]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	return fm, strings.Join(rest, "\n"), nil
}

// LoadWithOverride loads the embedded skills, then the skills in overrideDir
// (e.g. ~/.config/labmill/skills, where updated skills are dropped). An override
// skill replaces the embedded skill with the same name; new names are added.
//
// Override skills are read completely into memory at load time, through an
// os.Root opened on overrideDir, so symlinks can never reach files outside it
// and later edits on disk can't desynchronize a running session. Files larger
// than MaxFileSize are rejected. A skill directory that is a symlink resolving
// inside overrideDir is allowed; one escaping it is skipped with a warning.
// Only relative symlinks that stay inside overrideDir work: os.Root rejects
// absolute symlink targets (even ones pointing inside the directory) and
// "..", so those skills are skipped with a warning. A SKILL.md that is not a
// regular file (FIFO, device, directory) or is a dangling symlink is skipped
// with a warning too.
//
// The override directory can never disable the embedded skills: a missing
// overrideDir is silently ignored, and any other problem (empty path, not a
// directory, unreadable, an invalid skill) is returned in warnings while the
// embedded set (plus any valid overrides) is still returned. err is only for
// failures loading the embedded skills themselves.
func LoadWithOverride(embedded fs.FS, overrideDir string) (set *Set, warnings []error, err error) {
	set, err = Load(embedded, "embedded")
	if err != nil {
		return nil, nil, err
	}
	if overrideDir == "" {
		return set, []error{errors.New("override skills: empty directory path")}, nil
	}
	root, err := os.OpenRoot(overrideDir)
	if errors.Is(err, fs.ErrNotExist) {
		return set, nil, nil
	}
	if err != nil {
		return set, []error{fmt.Errorf("override skills: %w", err)}, nil
	}
	defer root.Close()
	over := newRootFS(root)
	entries, err := fs.ReadDir(over, ".")
	if err != nil {
		return set, []error{fmt.Errorf("reading override skills: %w", err)}, nil
	}
	for _, e := range entries {
		isDir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			// Stat through the root: follows symlinks that stay inside it and
			// fails for ones that escape.
			info, err := fs.Stat(over, e.Name())
			if err != nil {
				warnings = append(warnings, fmt.Errorf("skill %q: %w", e.Name(), err))
				continue
			}
			isDir = info.IsDir()
		}
		if !isDir {
			continue
		}
		k, err := loadDir(over, e.Name(), overrideDir, true)
		if err != nil {
			warnings = append(warnings, err)
			continue
		}
		if k != nil {
			set.byName[k.Name] = k
		}
	}
	return set, warnings, nil
}

// List returns all skills sorted by name.
func (s *Set) List() []*Skill {
	list := make([]*Skill, 0, len(s.byName))
	for _, k := range s.byName {
		list = append(list, k)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Get returns the skill with the given name.
func (s *Set) Get(name string) (*Skill, bool) {
	k, ok := s.byName[name]
	return k, ok
}

// Body returns the SKILL.md content after the frontmatter, with leading blank
// lines trimmed.
func (k *Skill) Body() (string, error) {
	data, err := fs.ReadFile(k.fsys, path.Join(k.dir, "SKILL.md"))
	if err != nil {
		return "", fmt.Errorf("skill %q: %w", k.Name, err)
	}
	_, body, err := parseSkillMD(data)
	if err != nil {
		return "", fmt.Errorf("skill %q: %w", k.Name, err)
	}
	return body, nil
}

// refDirName is the name of the reference folder inside base: "References"
// when that folder exists (site skills, whose names start with an uppercase
// letter), else "references" (the generic skills).
func refDirName(fsys fs.FS, base string) string {
	if info, err := fs.Stat(fsys, path.Join(base, "References")); err == nil && info.IsDir() {
		return "References"
	}
	return "references"
}

// refPath maps a References entry to its path inside the skill directory:
// "cli.md" → "references/cli.md", "bigiron/slurm.md" →
// "bigiron/references/slurm.md" (or "References" in a site skill).
func refPath(fsys fs.FS, dir, ref string) string {
	if sub, file, ok := strings.Cut(ref, "/"); ok {
		return path.Join(sub, refDirName(fsys, path.Join(dir, sub)), file)
	}
	return path.Join(refDirName(fsys, dir), ref)
}

// ReadReference returns the content of one reference file. name may be given
// as the References entry ("cli.md", "bigiron/slurm.md") or as the path
// inside the skill ("references/cli.md", "bigiron/references/slurm.md").
// Leading "../" elements are dropped, so a link written relative to another
// reference file ("../bigiron/references/slurm.md") works too, and so does
// a link to a section ("cli.md#login": the "#fragment" is dropped, the whole
// file is returned). Only files listed in References can be read; anything
// else (SKILL.md, other files, absolute paths) is an error that lists the
// reference files.
func (k *Skill) ReadReference(name string) (string, error) {
	n := strings.TrimSpace(name)
	if i := strings.IndexByte(n, '#'); i >= 0 {
		n = strings.TrimSpace(n[:i])
	}
	n = path.Clean(n)
	for strings.HasPrefix(n, "../") {
		n = n[3:]
	}
	if slices.Contains(k.Tools, n) {
		data, err := fs.ReadFile(k.fsys, path.Join(k.dir, n))
		if err != nil {
			return "", fmt.Errorf("skill %q: %w", k.Name, err)
		}
		if len(data) > MaxFileSize || bytes.IndexByte(data, 0) >= 0 {
			return "", fmt.Errorf("skill %q: %q is not a text file of at most %d bytes", k.Name, name, MaxFileSize)
		}
		return string(data), nil
	}
	if rest, ok := cutFoldPrefix(n, "references/"); ok {
		n = rest
	} else if parts := strings.Split(n, "/"); len(parts) == 3 && strings.EqualFold(parts[1], "references") {
		n = parts[0] + "/" + parts[2]
	}
	if !slices.Contains(k.References, n) { // an exact match wins
		if i := slices.IndexFunc(k.References, func(r string) bool { return strings.EqualFold(r, n) }); i >= 0 {
			n = k.References[i] // site files start with an uppercase letter; Claude may not
		}
	}
	if !slices.Contains(k.References, n) {
		all := append(slices.Clone(k.References), k.Tools...)
		if len(all) == 0 {
			return "", fmt.Errorf("skill %q has no reference file %q (it has no reference files)", k.Name, name)
		}
		return "", fmt.Errorf("skill %q has no reference file %q (its reference files: %s)", k.Name, name, strings.Join(all, ", "))
	}
	data, err := fs.ReadFile(k.fsys, path.Join(k.dir, refPath(k.fsys, k.dir, n)))
	if err != nil {
		return "", fmt.Errorf("skill %q: %w", k.Name, err)
	}
	return string(data), nil
}

// cutFoldPrefix is strings.CutPrefix ignoring ASCII case in the prefix.
func cutFoldPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// skillInstructions is the usage rule SystemPrompt puts before the list.
const skillInstructions = "You have skills: packaged expertise for specific kinds of tasks. When the user's request matches a skill's description, call the load_skill tool with the skill name before answering, and follow the instructions it returns. Load each skill at most once per conversation. If the skill body points to reference files, fetch them with the read_reference tool. Skills are guidance; they never override Labmill's approval rules."

// Instructions is SystemPrompt without the list of skills: just the usage
// rule. It returns "" for an empty set.
func (s *Set) Instructions() string {
	if len(s.List()) == 0 {
		return ""
	}
	return skillInstructions
}

// SystemPrompt renders the section of the agent's system prompt that
// advertises the skills: a short instruction to call load_skill when a request
// matches a skill's description (and read_reference for details the body
// points to), to load each skill at most once per conversation, and that
// skills are guidance that never override Labmill's approval rules; followed by
// one "- <name>: <description>" line per skill in name order. It returns ""
// for an empty set. Output is deterministic (prompt-cache friendly).
func (s *Set) SystemPrompt() string {
	list := s.List()
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(skillInstructions)
	b.WriteString("\n\nAvailable skills:\n")
	for _, k := range list {
		fmt.Fprintf(&b, "- %s: %s\n", k.Name, k.Description)
	}
	return b.String()
}

// LoadSkillResult is what the load_skill tool returns to Claude: s.Preamble
// (if set) at the top, then the body, then a footer naming the exact call,
// e.g. `Reference files (read_reference skill="slurm-user", file=...): slurm.md,
// workloads.md`, or a note that there are none.
func (s *Set) LoadSkillResult(name string) (string, error) {
	k, ok := s.Get(name)
	if !ok {
		return "", fmt.Errorf("unknown skill %q", name)
	}
	body, err := k.Body()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if s.Preamble != "" {
		b.WriteString(s.Preamble)
		if !strings.HasSuffix(s.Preamble, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	if len(k.References) == 0 {
		b.WriteString("\n(This skill has no reference files.)\n")
	} else {
		b.WriteString("\nReference files (read_reference skill=\"" + k.Name + "\", file=...): " + strings.Join(k.References, ", ") + "\n")
	}
	if len(k.Tools) > 0 {
		b.WriteString("Scripts (read-only text, read with read_reference skill=\"" + k.Name + "\", file=...; Labmill never runs them, show the user what they do or carry the steps out with your own tools): " + strings.Join(k.Tools, ", ") + "\n")
	}
	return b.String(), nil
}
