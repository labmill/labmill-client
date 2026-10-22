package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
)

// Extra script files of a job (M38.3). A sample often has no build or run
// script; draft_job's files field lets Claude write small ones (for example
// labmill-build.sh and labmill-run.sh) from the README and build files. They are
// never created on the user's laptop: they are shown in the submit review
// below the job script, and submit_job writes them into the job's folder on
// the cluster (mode 0755) over the login-node runner, before it submits.

// jobFile is one extra script file of a job.
type jobFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

const (
	eventFiles = "files" // text is the JSON []jobFile behind the review (set by draft_job)

	// maxJobFiles and maxJobFileBytes bound the extra files: they are small
	// text scripts for a human to read.
	maxJobFiles     = 8
	maxJobFileBytes = 64 << 10

	// fileMarkerPrefix starts the line that introduces a file in the submit
	// review's text (the script comes first, then each file after its marker).
	fileMarkerPrefix = "##### Labmill writes the file "
	fileMarkerSuffix = " (mode 0755) in the job folder on the cluster before it submits #####"
)

var jobFileNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// validateJobFiles checks the files draft_job (or an edited review) carries:
// plain names (no slash, no leading dot), no duplicates, text up to
// maxJobFileBytes without control or invisible characters, and no line that
// looks like a review marker. It returns the files with CRLF turned into LF
// and a final newline.
func validateJobFiles(files []jobFile) ([]jobFile, error) {
	if len(files) > maxJobFiles {
		return nil, fmt.Errorf("At most %d extra files per job (got %d).", maxJobFiles, len(files))
	}
	seen := map[string]bool{}
	out := make([]jobFile, 0, len(files))
	for _, f := range files {
		if !jobFileNameRe.MatchString(f.Name) {
			return nil, fmt.Errorf("File name %q isn't allowed: use a plain name such as labmill-run.sh (letters, digits, dots, dashes, underscores; no slash, no leading dot, at most 64 characters).", sanitize(f.Name))
		}
		if seen[f.Name] {
			return nil, fmt.Errorf("File %s is given twice.", f.Name)
		}
		seen[f.Name] = true
		c := strings.ReplaceAll(f.Content, "\r\n", "\n")
		if strings.TrimSpace(c) == "" {
			return nil, fmt.Errorf("File %s is empty.", f.Name)
		}
		if len(c) > maxJobFileBytes {
			return nil, fmt.Errorf("File %s is %d bytes; extra files are small scripts of at most %d KiB.", f.Name, len(c), maxJobFileBytes>>10)
		}
		if err := backend.CheckText("file "+f.Name, c, true); err != nil {
			return nil, fmt.Errorf("File %s isn't valid: %w", f.Name, err)
		}
		for _, line := range strings.Split(c, "\n") {
			if strings.HasPrefix(line, fileMarkerPrefix) {
				return nil, fmt.Errorf("File %s has a line that starts like Labmill's review marker; change it.", f.Name)
			}
		}
		if !strings.HasSuffix(c, "\n") {
			c += "\n"
		}
		out = append(out, jobFile{f.Name, c})
	}
	return out, nil
}

// hasFileMarker reports whether a line of text starts like a review marker.
func hasFileMarker(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, fileMarkerPrefix) {
			return true
		}
	}
	return false
}

// reviewWithFiles is the submit review's text: the script, then each file
// after its marker line.
func reviewWithFiles(script string, files []jobFile) string {
	if len(files) == 0 {
		return script
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(script, "\n"))
	b.WriteString("\n")
	for _, f := range files {
		b.WriteString("\n" + fileMarkerPrefix + f.Name + fileMarkerSuffix + "\n")
		b.WriteString(f.Content)
	}
	return b.String()
}

// splitReviewFiles is the inverse of reviewWithFiles: the script and the
// files in the (possibly edited) review text. A marker the user damaged is
// an error rather than a silently dropped file.
func splitReviewFiles(text string) (string, []jobFile, error) {
	if !strings.Contains(text, fileMarkerPrefix) {
		return text, nil, nil
	}
	lines := strings.SplitAfter(text, "\n")
	var script strings.Builder
	var files []jobFile
	cur := -1
	for _, l := range lines {
		trim := strings.TrimRight(l, "\r\n")
		if strings.HasPrefix(trim, fileMarkerPrefix) {
			name, ok := strings.CutSuffix(strings.TrimPrefix(trim, fileMarkerPrefix), fileMarkerSuffix)
			if !ok {
				return "", nil, errors.New("A file marker line in the review was changed. Keep the lines that start with \"" + strings.TrimSpace(fileMarkerPrefix) + "\" as they are, or remove the file with its marker.")
			}
			files = append(files, jobFile{Name: name})
			cur = len(files) - 1
			continue
		}
		if cur < 0 {
			script.WriteString(l)
		} else {
			files[cur].Content += l
		}
	}
	// The blank line before each marker belongs to the layout, not the script.
	for i := range files {
		files[i].Content = strings.TrimRight(files[i].Content, "\n")
	}
	files, err := validateJobFiles(files)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimRight(script.String(), "\n") + "\n", files, nil
}

// jobFilesOf returns the files saved by the job's latest draft.
func (t *toolset) jobFilesOf(ctx context.Context, jobID int64) ([]jobFile, error) {
	text, ok, err := t.latestEvent(ctx, jobID, eventFiles)
	if err != nil || !ok {
		return nil, err
	}
	var files []jobFile
	if json.Unmarshal([]byte(text), &files) != nil {
		return nil, fmt.Errorf("I can't read the files saved for job %d. Run draft_job again.", jobID)
	}
	return files, nil
}

// checkJobFilesTarget says whether files can be written for this job: the
// login-node runner must exist.
func (t *toolset) checkJobFilesTarget(c config.Cluster, spec backend.JobSpec) error {
	if t.env.LoginRunner == nil {
		return errors.New("Writing files on the cluster isn't available right now.")
	}
	return nil
}

// writeScript is the sh program that writes one file. It is one line joined
// with ';' (a newline inside a quoted word breaks a csh/tcsh login shell) and
// has no '!' (history expansion). $1 is the folder, $2 the file name, $3 the
// expected byte count: stdin goes to a temp file in the folder, and a short
// write (dropped connection) is exit 4, so a truncated script is never put in
// place. An existing file is left alone when its content is the same and is
// never replaced otherwise (exit 3); the target is created by ln, which is
// atomic and fails when the name exists (mv -n on coreutils 8.x exits 0 when
// it skips). The result is chmod 755.
const writeScript = `d=$1; f=$2; n=$3; ` +
	`if [ -L "$d" ]; then echo "$d is a symbolic link" >&2; exit 5; fi; ` +
	`mkdir -p -- "$d" || exit 4; ` +
	`t=$(mktemp "$d/.labmill-file.XXXXXX") || exit 4; ` +
	`trap 'rm -f -- "$t"' EXIT; ` +
	`cat > "$t" || exit 4; ` +
	`[ "$(wc -c < "$t")" -eq "$n" ] || { echo "short write" >&2; exit 4; }; ` +
	`if [ -e "$d/$f" ] || [ -L "$d/$f" ]; then ` +
	`if [ -L "$d/$f" ]; then exit 3; fi; cmp -s "$t" "$d/$f" && exit 0; exit 3; fi; ` +
	`chmod 755 "$t" && ln "$t" "$d/$f" || { if [ -e "$d/$f" ]; then exit 3; fi; exit 4; }`

// writeJobFiles writes files into dir on the cluster c through its login
// node. It stops at the first failure.
func (t *toolset) writeJobFiles(ctx context.Context, c config.Cluster, dir string, files []jobFile) error {
	if len(files) == 0 {
		return nil
	}
	if t.env.LoginRunner == nil {
		return errors.New("Writing files on the cluster isn't available right now.")
	}
	r, err := t.env.LoginRunner(ctx, c)
	if err != nil {
		return fmt.Errorf("I couldn't connect to %s to write the files: %w", c.Name, err)
	}
	for _, f := range files {
		res, err := r.Run(ctx, []string{"sh", "-c", writeScript, "sh", dir, f.Name, strconv.Itoa(len(f.Content))}, strings.NewReader(f.Content))
		if err != nil {
			return fmt.Errorf("I couldn't write %s on %s: %w", f.Name, c.Name, err)
		}
		switch res.ExitCode {
		case 0:
		case 3:
			return fmt.Errorf("%s already exists in %s on %s with different content, and I don't overwrite files. Draft again with another file name (the job script must then call that name).", f.Name, sanitize(dir), c.Name)
		default:
			return fmt.Errorf("I couldn't write %s in %s on %s (exit %d): %s", f.Name, sanitize(dir), c.Name, res.ExitCode, sanitize(strings.TrimSpace(res.Stderr)))
		}
	}
	return nil
}

// saveJobFiles records the files of a draft; a redraft without files clears
// an earlier set.
func (t *toolset) saveJobFiles(ctx context.Context, jobID int64, files []jobFile) error {
	if len(files) == 0 {
		if _, ok, err := t.latestEvent(ctx, jobID, eventFiles); err != nil || !ok {
			return err
		}
	}
	b, err := json.Marshal(files)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		b = []byte("[]")
	}
	return t.env.Store.AddEvent(ctx, jobID, eventFiles, string(b))
}
