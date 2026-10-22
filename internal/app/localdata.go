package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"labmill-client/internal/agent"
	"labmill-client/internal/stall"
)

// Limits for find_local_data: the whole search stops after maxWalk entries or
// findTimeout, and each candidate folder's size is counted over at most
// maxStatEntries entries (the count is then reported as a lower bound).
const (
	maxWalk        = 50000
	maxStatEntries = 5000
	maxFindResults = 20
	maxPreview     = 4096
)

// findTimeout is find_local_data's search time; findGrace is the extra time a
// walk that ignores its deadline gets before it counts as stalled. Variables
// so tests can shorten them.
var (
	findTimeout = 2 * time.Second
	findGrace   = time.Second
)

// previewTimeout bounds one preview_file read; a file that doesn't answer in
// time is skipped (see internal/stall). A variable so tests can shorten it.
var previewTimeout = 5 * time.Second

// walkHook, when set (tests only), is called for every entry findDirs visits;
// it lets a test block the walk deterministically.
var walkHook func(p string)

// localFile is what preview_file reads: the part of *os.File it needs.
type localFile interface {
	io.ReadSeeker
	io.Closer
	Stat() (fs.FileInfo, error)
}

// openLocal opens a local file for reading; tests swap it for a reader that
// blocks.
var openLocal = func(p string) (localFile, error) { return os.Open(p) }

// foldPaths reports whether paths compare case-insensitively (the default
// macOS and Windows filesystems do; a lower-cased path must not slip past
// the home and system-folder rules).
func foldPaths() bool { return privacyOS == "darwin" || privacyOS == "windows" }

// samePath reports whether a and b are the same path (case-insensitive on
// macOS and Windows).
func samePath(a, b string) bool {
	if foldPaths() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func within(root, p string) bool {
	if foldPaths() {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// roots returns the allowed roots: LocalRoots plus the Workspace folders
// that lie outside all of them (CheckFolder accepted those when they were
// chosen).
func (t *toolset) roots() []string {
	out := append([]string(nil), t.env.LocalRoots...)
	for _, f := range t.env.Workspace.folders() {
		// A folder whose resolution changed since it was chosen (a symlink
		// swapped in) is no longer allowed.
		if real, err := evalLinks(f); err != nil || real != f {
			continue
		}
		inside := false
		for _, r := range t.env.LocalRoots {
			if rr, err := evalLinks(r); err == nil && within(rr, f) {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, f)
		}
	}
	return out
}

// realRoots returns the allowed roots with symlinks resolved (unresolvable
// ones are dropped).
func (t *toolset) realRoots() []string {
	var out []string
	for _, r := range t.roots() {
		if rr, err := evalLinks(r); err == nil {
			out = append(out, rr)
		}
	}
	return out
}

// resolve turns a relative path into an absolute one against the Workspace
// folders (code first, then data): the first where it exists, else the
// first folder. Absolute paths, and every path when no folder is chosen,
// are returned unchanged (insideRoots then refuses a relative one).
func (t *toolset) resolve(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	ws := t.env.Workspace.folders()
	if len(ws) == 0 {
		return p
	}
	for _, f := range ws {
		cand := filepath.Join(f, p)
		if _, err := os.Lstat(cand); err == nil {
			return cand
		}
	}
	return filepath.Join(ws[0], p)
}

// defaultFolder returns the first chosen Workspace folder of kinds ("" when
// none is chosen).
func (t *toolset) defaultFolder(kinds ...string) string {
	for _, k := range kinds {
		if f := t.env.Workspace.Folder(k); f != "" {
			return f
		}
	}
	return ""
}

// insideRoots resolves p (symlinks included) and returns it if it lies inside
// one of the LocalRoots and passes the privacy rules relative to that root.
// Symlinks that lead out of the roots, or into private places, are refused.
func (t *toolset) insideRoots(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("I need a folder or file path.")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q isn't a full path; please give the complete path.", sanitize(p))
	}
	real, err := evalLinks(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("I can't find or open %s.", sanitize(p))
	}
	var private error
	for _, rr := range t.realRoots() {
		if !within(rr, real) {
			continue
		}
		if err := checkUnder(rr, real); err != nil {
			private = err
			continue
		}
		return real, nil
	}
	if private != nil {
		return "", private
	}
	return "", fmt.Errorf("%s is outside the folders I'm allowed to look at (%s).", sanitize(p), sanitize(strings.Join(t.roots(), ", ")))
}

// skipper returns the predicate the directory walks use to leave out private
// entries: hidden, secret-looking, and the system folders directly under a
// root.
func (t *toolset) skipper() func(p, name string) bool {
	roots := t.realRoots()
	return func(p, name string) bool {
		parent := filepath.Dir(p)
		top := false
		for _, r := range roots {
			if r == parent {
				top = true
			}
		}
		return privateComponent(name, top) || hiddenAttr(p)
	}
}

func (t *toolset) findLocalData() agent.Tool {
	return agent.Tool{
		Name:        "find_local_data",
		Description: "Search the user's allowed folders for directories whose path matches all the words of the query. Returns path, file count, total size, a few sample file names and the newest modification time. Hidden folders and files that look like they hold passwords or keys are skipped. Without under it searches the user's chosen code and data folders when they chose any, else all allowed folders.",
		Schema: objSchema(map[string]any{
			"query": strProp("words that should appear in the folder path, e.g. \"mri 2026\""),
			"under": strProp("optional folder to search in (default: the chosen code and data folders, else all allowed folders)"),
		}, "query"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Query string `json:"query"`
				Under string `json:"under"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			t.resetMissing()
			words := strings.Fields(strings.ToLower(in.Query))
			if len(words) == 0 {
				return "", errors.New("Tell me what to look for (query), for example part of a folder name.")
			}
			outer := ctx
			ctx, cancel := context.WithTimeout(ctx, findTimeout)
			defer cancel()
			// Everything that touches the disk runs under stall.Run: one stat,
			// readdir or read blocked on a stalled network mount or a cloud
			// placeholder file would otherwise hang the tool, as ctx is only
			// checked between entries. found is shared with the walk, which may
			// still be running (abandoned) afterwards.
			var mu sync.Mutex
			var shared []dirInfo
			emit := func(d dirInfo) {
				mu.Lock()
				shared = append(shared, d)
				mu.Unlock()
			}
			_, err := stall.Run(outer, stall.Default, "the folder search", findTimeout+findGrace, func() (struct{}, error) {
				var starts []string
				switch ws := t.env.Workspace.folders(); {
				case in.Under != "":
					u, err := t.insideRoots(t.resolve(in.Under))
					if err != nil {
						return struct{}{}, err
					}
					starts = []string{u}
				case len(ws) > 0:
					for _, f := range ws {
						u, err := t.insideRoots(f)
						if err != nil {
							continue
						}
						nested := false
						for _, s := range starts {
							if within(s, u) {
								nested = true
							}
						}
						if !nested {
							// u may contain folders already collected: drop them.
							kept := starts[:0]
							for _, s := range starts {
								if !within(u, s) {
									kept = append(kept, s)
								}
							}
							starts = append(kept, u)
						}
					}
					if len(starts) == 0 {
						starts = t.realRoots()
					}
				default:
					starts = t.realRoots()
				}
				budget := maxWalk
				skip := t.skipper()
				for _, start := range starts {
					budget = findDirs(ctx, start, words, budget, skip, emit)
				}
				return struct{}{}, nil
			})
			stalled := ""
			switch {
			case stall.IsStall(err):
				stalled = sanitize(err.Error()) + "."
			case err != nil:
				return "", err
			}
			mu.Lock()
			found := append([]dirInfo(nil), shared...)
			mu.Unlock()
			if len(found) == 0 {
				if stalled != "" {
					return stalled + " I found no matching folders before that.", nil
				}
				return "No folders matched. Try fewer or different words.", nil
			}
			sort.SliceStable(found, func(i, j int) bool { return found[i].newest.After(found[j].newest) })
			if len(found) > maxFindResults {
				found = found[:maxFindResults]
			}
			var b strings.Builder
			for _, d := range found {
				more := ""
				if d.truncated {
					more = "+"
				}
				fmt.Fprintf(&b, "%s\n  %d%s files, %s, newest change %s", sanitize(d.path), d.files, more, humanSize(d.size), d.newest.Format("2006-01-02"))
				if len(d.samples) > 0 {
					fmt.Fprintf(&b, "; examples: %s", sanitize(strings.Join(d.samples, ", ")))
				}
				b.WriteString("\n")
			}
			if stalled != "" {
				b.WriteString(stalled + " These are the folders found before that.\n")
			}
			return b.String(), nil
		},
	}
}

type dirInfo struct {
	path      string
	files     int
	size      int64
	samples   []string
	newest    time.Time
	truncated bool
}

// findDirs walks start and reports (through emit) the topmost directories whose path (relative
// to start's parent-free view, i.e. start itself and below) contains all words.
// It returns the remaining entry budget.
func findDirs(ctx context.Context, start string, words []string, budget int, skip func(p, name string) bool, emit func(dirInfo)) int {
	filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if walkHook != nil {
			walkHook(p)
		}
		if ctx.Err() != nil || budget <= 0 {
			return filepath.SkipAll
		}
		budget--
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p == start {
			return nil
		}
		if skip(p, d.Name()) {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(start, p)
		low := strings.ToLower(filepath.ToSlash(rel))
		for _, w := range words {
			if !strings.Contains(low, w) {
				return nil
			}
		}
		emit(dirStats(ctx, p, skip))
		return filepath.SkipDir
	})
	return budget
}

// dirStats counts files under dir, skipping private entries, over at most
// maxStatEntries entries.
func dirStats(ctx context.Context, dir string, skip func(p, name string) bool) dirInfo {
	info := dirInfo{path: dir}
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			info.truncated = true
			return filepath.SkipAll
		}
		if n++; n > maxStatEntries {
			info.truncated = true
			return filepath.SkipAll
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p != dir && skip(p, d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		info.files++
		info.size += fi.Size()
		if fi.ModTime().After(info.newest) {
			info.newest = fi.ModTime()
		}
		if len(info.samples) < 5 {
			info.samples = append(info.samples, d.Name())
		}
		return nil
	})
	return info
}

// Preview limits. Data files (CSV, tables, JSON, anything not recognized as
// documentation or code) keep the small sample (privacy: "small samples" of
// the user's data); documentation and code files may be read in bounded
// pages, because the model must read a README or a script to choose an image
// or build a job, and these files are not the user's data.
const (
	maxDataLines   = 20
	defaultData    = 10
	maxTextLines   = 200
	defaultText    = 100
	maxTextBytes   = 16 << 10
	maxPreviewScan = 4 << 20 // bytes read while skipping to start_line
)

// textExts are source and documentation extensions. ".txt" is deliberately
// absent (a .txt is often a data table); see isDocOrCode. Notebooks, JSON,
// tables, sequences, structures, logs and outputs are data.
var textExts = map[string]bool{
	".md": true, ".markdown": true, ".rst": true, ".adoc": true,
	".py": true, ".sh": true, ".bash": true, ".zsh": true, ".f90": true, ".f": true, ".f95": true,
	".cu": true, ".cuh": true, ".c": true, ".cc": true, ".cpp": true, ".cxx": true, ".h": true, ".hpp": true,
	".r": true, ".jl": true, ".m": true, ".pl": true, ".rb": true, ".go": true, ".rs": true,
	".java": true, ".js": true, ".ts": true,
	".yaml": true, ".yml": true, ".toml": true, ".cfg": true, ".ini": true, ".cmake": true,
	".sbatch": true, ".slurm": true, ".mk": true,
}

var textNames = map[string]bool{
	"cmakelists.txt": true, "makefile": true, "dockerfile": true,
	"environment.yml": true, "pyproject.toml": true,
}

// docPrefixes name documentation files by their first letters; the extension
// must then be empty or a documentation one (readme and license: any).
var docPrefixes = []string{"readme", "license", "copying", "changelog", "install", "notes", "requirements"}

// maxPageFile is the largest file preview_file pages through; bigger ones
// get only the data sample.
const maxPageFile = 1 << 20

// skipLine discards one line (however long); io.EOF means there was none.
func skipLine(r *bufio.Reader) error {
	for {
		_, err := r.ReadSlice('\n')
		if err != bufio.ErrBufferFull {
			return err
		}
	}
}

// isDocOrCode reports whether name is a documentation or source file that
// preview_file may page through (see the limits above).
func isDocOrCode(name string) bool {
	n := strings.ToLower(filepath.Base(name))
	if textNames[n] {
		return true
	}
	ext := filepath.Ext(n)
	if textExts[ext] {
		return true
	}
	for _, p := range docPrefixes {
		if !strings.HasPrefix(n, p) {
			continue
		}
		switch {
		case p == "readme" || p == "license":
			return true
		case ext == "" || ext == ".txt" || ext == ".md" || ext == ".rst" || ext == ".markdown":
			return true
		}
	}
	return false
}

// redactRe matches a key that names a credential followed by its value.
var redactRe = regexp.MustCompile(`(?i)([\w.\-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth)[\w.\-]*["']?\s*[:=]\s*)\S.*`)

// redactSecrets keeps the key of lines that assign a credential and hides
// the value.
func redactSecrets(text string) string {
	return redactRe.ReplaceAllString(text, "${1}«redacted»")
}

func (t *toolset) previewFile() agent.Tool {
	return agent.Tool{
		Name:        "preview_file",
		Description: "Show lines of a text file inside the allowed folders. Data files (CSV, tables, sequences, structures, .txt data, JSON, notebooks, logs and anything not listed below) are a small sample: the first lines only (default 10, at most 20 and 4 KiB). Documentation and code files (README*, LICENSE*, requirements*.txt, md, rst, py, sh, R, jl, c, cpp, h, cu, f90, yaml, toml, cfg, CMakeLists.txt, Makefile, Dockerfile and similar; at most 1 MiB; values of keys such as password, token or api_key are hidden) can be read in pages: default 100 lines, at most 200 lines and 16 KiB per call; pass start_line (1-based) to continue where the last page stopped (the reply says which start_line is next). Binary files are refused. Read the README and the build files (CMakeLists.txt, Makefile, setup.py, pyproject.toml, requirements) completely, page by page with start_line, before you decide how to build and run the code. Never guess file names (run.sh, build.sh, ...): list the folder first with find_local_data (or use the staged file list) and preview only files that exist; a name that does not exist is answered with the names that do. A relative path is looked up in the user's chosen code folder, then the data folder.",
		Schema: objSchema(map[string]any{
			"path":       strProp("full path of the file, or a path relative to the chosen code or data folder"),
			"lines":      intProp("number of lines (data files: default 10, max 20; documentation and code: default 100, max 200); alias max_lines"),
			"max_lines":  intProp("same as lines"),
			"start_line": intProp("first line to show, 1-based (default 1); documentation and code files only"),
		}, "path"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Path      string `json:"path"`
				Lines     int    `json:"lines"`
				MaxLines  int    `json:"max_lines"`
				StartLine int    `json:"start_line"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if in.Lines <= 0 {
				in.Lines = in.MaxLines
			}
			if in.StartLine < 0 {
				return "", fmt.Errorf("start_line must be 1 or more.")
			}
			if in.StartLine == 0 {
				in.StartLine = 1
			}
			// Everything that touches the file runs under previewTimeout: a read
			// on a stalled network mount or a cloud placeholder file can block
			// for minutes (internal/stall).
			res, err := stall.Run(ctx, stall.Default, sanitize(filepath.Base(in.Path)), previewTimeout, func() (string, error) {
				return t.previewRead(in.Path, in.Lines, in.StartLine)
			})
			if stall.IsStall(err) {
				return "", errors.New(sanitize(err.Error()) + ".")
			}
			if err == nil {
				t.resetMissing()
			}
			return res, err
		},
	}
}

// maxMissingListing is how many actual file names a failed preview of a
// missing file lists, and missingStopAfter the number of missing-file
// previews in a row (without a find_local_data or a successful preview
// between) after which the message tells Claude to stop guessing.
const (
	maxMissingListing = 20
	missingStopAfter  = 3
)

func (t *toolset) resetMissing() {
	t.missMu.Lock()
	t.missing = 0
	t.missMu.Unlock()
}

// missingFile is preview_file's answer for a path that doesn't exist (M38.1):
// it says so, points to find_local_data, and lists up to maxMissingListing
// names from the folder the path was in, so Claude can pick a real one.
// The listing follows the privacy rules (no hidden or secret-looking
// entries) and is only given for a folder inside the allowed roots.
func (t *toolset) missingFile(p string) error {
	t.missMu.Lock()
	t.missing++
	n := t.missing
	t.missMu.Unlock()
	dir, name := filepath.Dir(p), filepath.Base(p)
	msg := fmt.Sprintf("%s doesn't exist", sanitize(name))
	var names []string
	more := 0
	if real, err := t.insideRoots(dir); err == nil {
		msg += " in " + sanitize(filepath.Base(real))
		if ents, err := os.ReadDir(real); err == nil {
			skip := t.skipper()
			for _, e := range ents {
				if skip(filepath.Join(real, e.Name()), e.Name()) {
					continue
				}
				if len(names) == maxMissingListing {
					more++
					continue
				}
				nm := e.Name()
				if e.IsDir() {
					nm += "/"
				}
				names = append(names, sanitize(nm))
			}
		}
	}
	msg += "; list the folder (find_local_data) instead of guessing names."
	if len(names) > 0 {
		msg += " It holds: " + strings.Join(names, ", ")
		if more > 0 {
			msg += fmt.Sprintf(", and %d more", more)
		}
		msg += "."
	}
	if n >= missingStopAfter {
		msg += fmt.Sprintf(" That is %d previews of files that don't exist in a row: stop guessing names. If the code has no build or run script, read the README and the build files that exist, and write the scripts as extra files of draft_job (they are created on the cluster).", n)
	}
	return errors.New(msg)
}

// previewRead is preview_file's work on one file (see the tool's description
// for the rules); the caller bounds its time.
func (t *toolset) previewRead(path string, lines, startLine int) (string, error) {
	in := struct {
		Path      string
		Lines     int
		StartLine int
	}{path, lines, startLine}
	resolved := t.resolve(in.Path)
	if filepath.IsAbs(resolved) {
		if _, err := os.Lstat(resolved); errors.Is(err, fs.ErrNotExist) {
			return "", t.missingFile(resolved)
		}
	}
	real, err := t.insideRoots(resolved)
	if err != nil {
		return "", err
	}
	// Check before opening: opening a FIFO or device could hang.
	if fi, err := os.Lstat(real); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s isn't a regular file.", sanitize(in.Path))
	}
	f, err := openLocal(real)
	if err != nil {
		return "", fmt.Errorf("I can't open %s.", sanitize(in.Path))
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s isn't a regular file.", sanitize(in.Path))
	}
	doc := isDocOrCode(real)
	note := ""
	if fi, err := f.Stat(); doc && err == nil && fi.Size() > maxPageFile {
		doc = false
		note = "\n[this documentation or code file is over 1 MiB, so I only show the first lines]"
	}
	maxLines, maxBytes, def := maxDataLines, maxPreview, defaultData
	if doc {
		maxLines, maxBytes, def = maxTextLines, maxTextBytes, defaultText
	} else if in.StartLine > 1 {
		return "", fmt.Errorf("%s is a data file: I only show a sample from its start, so start_line isn't available. Documentation and code files can be paged.", sanitize(in.Path))
	}
	if in.Lines <= 0 {
		in.Lines = def
	}
	if in.Lines > maxLines {
		in.Lines = maxLines
	}
	// Binary check on the head of the file, whatever the page.
	head := make([]byte, maxPreview)
	hn, _ := f.Read(head)
	if bytes.IndexByte(head[:hn], 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file, so I won't show it.", sanitize(in.Path))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("I can't read %s.", sanitize(in.Path))
	}
	lr := &io.LimitedReader{R: f, N: maxPreviewScan}
	r := bufio.NewReader(lr)
	// Skip to start_line.
	for n := 1; n < in.StartLine; n++ {
		if err := skipLine(r); err != nil {
			if err == io.EOF {
				return "", fmt.Errorf("start_line %d is past the end of %s.", in.StartLine, sanitize(in.Path))
			}
			return "", fmt.Errorf("I can't read %s.", sanitize(in.Path))
		}
	}
	var out strings.Builder
	shown := 0
	more := false
	for shown < in.Lines {
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		if out.Len()+len(line) > maxBytes {
			if shown == 0 {
				line = line[:maxBytes]
				shown++
			}
			more = true
			break
		}
		out.WriteString(line)
		shown++
		if err != nil {
			break
		}
	}
	if !more {
		if _, err := r.Peek(1); err == nil {
			more = true
		}
	}
	text := out.String()
	if strings.IndexByte(text, 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file, so I won't show it.", sanitize(in.Path))
	}
	if !utf8.ValidString(text) {
		text = strings.ToValidUTF8(text, "?")
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	res := cleanText(text)
	if doc {
		res = redactSecrets(res)
	}
	res += note
	if lr.N == 0 {
		res += "\n[the rest is beyond what I page through]"
	}
	if doc && more {
		res += fmt.Sprintf("\n[lines %d-%d shown; more follows: use start_line %d]", in.StartLine, in.StartLine+shown-1, in.StartLine+shown)
	}
	return res, nil
}
