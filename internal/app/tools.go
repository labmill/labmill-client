package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/anthropics/anthropic-sdk-go"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
	"labmill-client/internal/transfer"
)

// Event kinds recorded on a job. The store has no field for the staging root
// or the job spec, so tools keep them as events and read the latest one:
//
//	"staged": text is the local staging root (set by stage_data, read by upload_data)
//	"spec":   text is the JSON of the backend.JobSpec behind the drafted script
//	          (set by draft_job, read by submit_job, which needs it for backend.Submit)
const (
	eventStaged = "staged"
	eventSpec   = "spec"
	eventS3     = "s3" // "endpoint=<url>" the S3 store an upload went to
)

// errJobMoved aborts a job_status save whose job got a new scheduler ID.
var errJobMoved = errors.New("job moved to a new scheduler ID")

const (
	maxDetailLines = 200
	maxResultLines = 50
)

var (
	jobNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	remoteDirRe = regexp.MustCompile(`^[A-Za-z0-9._/+@,:=~-]+$`) // the backend WorkDir charset, relative
)

// errChanged is returned by a gated tool's Run when what it would do no longer
// matches what the user approved.
var errChanged = errors.New("This job changed since you approved it. Please review it again.")

// sameAsApproved checks the proposal rebuilt from the current store against
// the one the user approved.
func sameAsApproved(built, approved agent.Proposal) error {
	if built.Summary != approved.Summary || built.Details != approved.Details {
		return errChanged
	}
	return nil
}

type toolset struct {
	env *Env
	// sleep waits between job_status polls; nil means sleepCtx (tests inject a fake).
	sleep func(ctx context.Context, d time.Duration) error
	// missing counts preview_file calls for files that don't exist since the
	// last find_local_data or successful preview (localdata.go missingFile).
	missMu  sync.Mutex
	missing int
}

// ---- small helpers -------------------------------------------------------

func objSchema(props map[string]any, required ...string) anthropic.ToolInputSchemaParam {
	return anthropic.ToolInputSchemaParam{Properties: props, Required: required}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

func decode(input json.RawMessage, v any) error {
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	if err := json.Unmarshal(input, v); err != nil {
		return errors.New("I couldn't understand the details of that request. Please check the fields and try again.")
	}
	return nil
}

func (t *toolset) job(ctx context.Context, id int64) (store.Job, error) {
	j, err := t.env.Store.Job(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Job{}, fmt.Errorf("There's no job %d. Use list_jobs to see your jobs.", id)
	}
	if err != nil {
		return store.Job{}, fmt.Errorf("I couldn't read job %d from Labmill's job list: %w", id, err)
	}
	return j, nil
}

func (t *toolset) cluster(name string) (config.Cluster, error) {
	if t.env.Config != nil {
		if c, ok := t.env.Config.Cluster(name); ok {
			return c, nil
		}
	}
	return config.Cluster{}, fmt.Errorf("There's no cluster called %q. Use list_clusters to see the configured clusters.", name)
}

func (t *toolset) backendFor(ctx context.Context, c config.Cluster) (backend.Backend, error) {
	if t.env.Backend == nil {
		return nil, errors.New("Connecting to clusters isn't available right now.")
	}
	be, err := t.env.Backend(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("I couldn't connect to %s: %w", c.Name, err)
	}
	return be, nil
}

// jobBackend returns the backend of the cluster a submitted job runs on.
func (t *toolset) jobBackend(ctx context.Context, j store.Job) (backend.Backend, config.Cluster, error) {
	if j.RemoteID == "" {
		return nil, config.Cluster{}, fmt.Errorf("Job %d (%s) hasn't been submitted yet, so it has no status on a cluster.", j.ID, j.Name)
	}
	c, err := t.cluster(j.Cluster)
	if err != nil {
		return nil, c, err
	}
	be, err := t.backendFor(ctx, c)
	return be, c, err
}

// latestEvent returns the text of the newest event of the given kind.
func (t *toolset) latestEvent(ctx context.Context, jobID int64, kind string) (string, bool, error) {
	evs, err := t.env.Store.Events(ctx, jobID, 0)
	if err != nil {
		return "", false, fmt.Errorf("I couldn't read the history of job %d: %w", jobID, err)
	}
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == kind {
			return evs[i].Text, true, nil
		}
	}
	return "", false, nil
}

// transferHost is the SSH alias file transfers go through: the cluster's
// dedicated files_host when set (its shell host often has no sftp), else its
// ssh_host.
func transferHost(c config.Cluster) string {
	if c.FilesHost != "" {
		return c.FilesHost
	}
	return c.SSHHost
}

// via is the transfer host as an approval shows it: the SSH alias plus the
// real HostName when Env.HostName knows it, e.g. "files-host
// (files.example.edu)".
func (t *toolset) via(c config.Cluster) string {
	alias := transferHost(c)
	if t.env.HostName == nil || alias == "" {
		return alias
	}
	if h := t.env.HostName(alias); h != "" && h != alias {
		return alias + " (" + h + ")"
	}
	return alias
}

// viaLabel returns via when set, else the cluster's transfer alias.
func viaLabel(c config.Cluster, via string) string {
	if via != "" {
		return via
	}
	return transferHost(c)
}

func (t *toolset) remote(c config.Cluster, dir string) (transfer.Remote, error) {
	host := transferHost(c)
	if host == "" || t.env.SSHCommand == nil {
		return transfer.Remote{}, fmt.Errorf("Cluster %s has no login host set up, so files can't be transferred to or from it.", c.Name)
	}
	r := transfer.Remote{SSHCommand: t.env.SSHCommand(host), Dir: dir}
	if t.env.TransferCheck != nil {
		alias := host
		r.Check = func(ctx context.Context) error { return t.env.TransferCheck(ctx, alias) }
	}
	return r, nil
}

// s3Remote builds the S3 location for dir on an s3 cluster.
func s3Remote(c config.Cluster, dir string) (transfer.S3Remote, error) {
	if c.S3 == nil {
		return transfer.S3Remote{}, fmt.Errorf("Cluster %s is set to transfer through S3 but has no s3 settings.", c.Name)
	}
	return transfer.S3Remote{Bucket: c.S3.Bucket, Prefix: c.S3.Prefix, Dir: dir, Region: c.S3.Region, Profile: c.S3.Profile, Endpoint: c.S3.Endpoint, SSE: c.S3.SSE, KMSKeyID: c.S3.KMSKeyID}, nil
}

// s3Where describes an S3 location for approvals: the full s3:// path and the
// AWS profile that will be used (never any credentials).
func s3Where(r transfer.S3Remote) string {
	prof := "default AWS credentials"
	if r.Profile != "" {
		prof = "AWS profile " + r.Profile
	}
	if r.Endpoint != "" {
		host := r.Endpoint
		if u, err := url.Parse(r.Endpoint); err == nil && u.Host != "" {
			host = u.Host
		}
		prof += ", via " + host
	}
	if r.Region != "" {
		prof += ", region " + r.Region
	}
	switch r.SSE {
	case "aws:kms":
		prof += ", encrypted with KMS"
	case "AES256":
		prof += ", encrypted with S3-managed keys"
	}
	return fmt.Sprintf("%s (%s)", sanitize(r.String()), sanitize(prof))
}

// rcloneNote returns the rclone-lib parameter line(s) (see
// transfer.Remote.RcloneCommand) for the upload (upload true) or download of
// rels, or "" when the destination can't be built. It is shown in results and
// at the start of the transfer, never in an approval.
func (t *toolset) rcloneNote(c config.Cluster, s3 *transfer.S3Remote, dir, local string, rels []string, upload bool) string {
	var cmd string
	if s3 != nil {
		cmd = s3.RcloneCommand(local, rels, upload)
	} else if r, err := t.remote(c, dir); err == nil {
		cmd = r.RcloneCommand(local, rels, upload)
	} else {
		return ""
	}
	// A download has one line per path: cap them like the other long lists.
	note := strings.Join(capLines(strings.Split(sanitizeLines(cmd), "\n"), maxRcloneLines), "\n")
	if s3 == nil && !upload {
		// Download itself runs this after the copy.
		if r, err := t.remote(c, dir); err == nil {
			note += "\nthen: sha256sum on " + sanitize(r.Host()) + " (read-only)"
		}
	}
	return note
}

// safeJobName turns any text (a folder name with spaces such as
// "OneDrive - Example University", a Windows path) into a valid job name
// (jobNameRe): runs of other characters become one dash, the ends are
// trimmed, and it is cut to 64 characters. "job" when nothing is left.
func safeJobName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	n := strings.Trim(b.String(), "-._")
	if len(n) > 64 {
		n = strings.Trim(n[:64], "-._")
	}
	if n == "" {
		return "job"
	}
	return n
}

// maxRcloneLines caps the rclone-lib lines of a download.
const maxRcloneLines = 20

// startTransfer announces the transfer to the front end (Env.Transfer: the
// rclone-lib line, then throttled rclone-style stats) and returns the progress
// callback for transfer.Upload/Download and finish, which ends the live
// display and returns the one-line summary ("Transferred 4.0 GiB in 1m31s
// (45 MiB/s), 10 files"), or "" when nothing was reported (a failed or empty
// transfer). finish may be called more than once.
func (t *toolset) startTransfer(tool string, jobID int64, cmd string) (cb func(transfer.Progress), finish func() string) {
	emit := func(u transfer.Update) {
		if t.env.Transfer != nil {
			u.Tool = tool
			u.Current = sanitize(u.Current)
			t.env.Transfer(u)
		}
	}
	emit(transfer.Update{Kind: "start", Text: cmd, ETASeconds: -1})
	m := transfer.NewMeter()
	var last transfer.Progress
	seen, ended := false, false
	summary := ""
	base := t.progress(jobID)
	cb = func(p transfer.Progress) {
		if base != nil {
			base(p)
		}
		last, seen = p, true
		if u, ok := m.Observe(p); ok {
			emit(u)
		}
	}
	finish = func() string {
		if !ended {
			ended = true
			if seen && last.Files > 0 {
				summary = m.Summary(last)
			}
			emit(transfer.Update{Kind: "end", Text: summary, ETASeconds: -1})
		}
		return summary
	}
	return cb, finish
}

// verifyNote is the text fetch_results adds about the SHA-256 check of an
// SFTP download ("" for S3, which compares MD5 itself): what was verified,
// what was checked by size only and why, and a warning naming every file that
// differs (kept as .mismatch) so the user fetches it again.
func verifyNote(v transfer.Verification) string {
	var out string
	switch {
	case v.SizeOnly != "" && v.Verified == 0:
		out = "\n(verified by size only: " + sanitize(v.SizeOnly) + ")"
	case v.SizeOnly != "":
		out = fmt.Sprintf("\nVerified %s (SHA-256); the other %d by size only (%s).", plural(v.Verified, "file", "files"), v.Unverified, sanitize(v.SizeOnly))
	case v.Verified > 0:
		out = "\nVerified " + plural(v.Verified, "file", "files") + " (SHA-256 matches the cluster's)."
	}
	if len(v.Failed) > 0 {
		out += "\nCould not be checksummed on the cluster (size matches, content not verified): " + sanitize(strings.Join(capLines(v.Failed, maxResultLines), ", "))
	}
	if len(v.Mismatched) > 0 {
		out += "\nWARNING: SHA-256 differs from the cluster's for " + sanitize(strings.Join(capLines(v.Mismatched, maxResultLines), ", ")) + ". The damaged copy is kept under that .mismatch name (nothing was deleted); fetch it again."
	}
	if len(v.Unrenamed) > 0 {
		out += "\nWARNING: SHA-256 differs from the cluster's for " + sanitize(strings.Join(capLines(v.Unrenamed, maxResultLines), ", ")) + " and it could not be renamed to .mismatch; don't use it, fetch it again."
	}
	return out
}

// withTransfer joins a result with the transfer summary and rclone-lib line.
func withTransfer(res, summary, cmd string) string {
	if summary != "" {
		res += "\n" + summary
	}
	if cmd != "" {
		res += "\n\n" + cmd
	}
	return res
}

// sanitizeLines sanitizes each line of s separately (keeping the newlines).
func sanitizeLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = sanitize(l)
	}
	return strings.Join(lines, "\n")
}

// s3Hint is appended to messages about a job whose data is in S3: the cluster
// can't see that folder, so the command has to move the data itself.
func s3Hint(loc string) string {
	return fmt.Sprintf("The command must copy the data from %s itself (for example `aws s3 cp --recursive %s .`) and write results back below %s so fetch_results can download them.", loc, loc, loc)
}

func (t *toolset) progress(jobID int64) func(transfer.Progress) {
	if t.env.Progress == nil {
		return nil
	}
	return func(p transfer.Progress) { t.env.Progress(jobID, p) }
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func capLines(lines []string, max int) []string {
	if len(lines) <= max {
		return lines
	}
	out := append([]string{}, lines[:max]...)
	return append(out, fmt.Sprintf("... and %d more", len(lines)-max))
}

// cleanText drops control characters so tool output stays printable.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

// checkRemoteDir validates the upload folder: a plain subfolder of the login
// home, or (allowAbs, for clusters reached over SFTP) a plain absolute folder
// such as a group storage path, in the character set the backends accept for
// work dirs. S3 folders are always relative to the configured prefix.
func checkRemoteDir(dir string, allowAbs bool) error {
	switch {
	case strings.TrimSpace(dir) == "":
		return errors.New("The remote folder can't be empty.")
	case strings.HasPrefix(dir, "/") && !allowAbs:
		return errors.New("The remote folder must be relative to the cluster's storage location, for example labmill/myjob.")
	case dir == "/":
		return errors.New("The remote folder can't be the top of the file system; please choose a folder such as labmill/myjob.")
	}
	if len(dir) > 1024 {
		return errors.New("The remote folder path is too long.")
	}
	abs := strings.HasPrefix(dir, "/")
	rel := strings.TrimPrefix(dir, "/")
	parts := strings.Split(rel, "/")
	for _, part := range parts {
		switch {
		case part == "..":
			return errors.New("The remote folder can't contain \"..\"; please choose a plain folder name.")
		case part == "~":
			return errors.New("The remote folder can't contain \"~\" as a folder name; write the path out in full.")
		case strings.HasPrefix(part, ".") && part != ".":
			return fmt.Errorf("The remote folder can't go through hidden folders such as %q.", part)
		case isSecretName(part):
			return fmt.Errorf("The remote folder can't go through %q; it looks like it holds secrets.", part)
		}
	}
	if !remoteDirRe.MatchString(rel) {
		return errors.New("The remote folder may only contain letters, digits and . _ / + @ , : = ~ -; please choose a plain folder name.")
	}
	if path.Clean(rel) != rel || rel == "." || strings.HasPrefix(rel, "/") {
		return errors.New("Use a plain folder path such as labmill/myjob (no \".\", empty parts or trailing slash).")
	}
	if abs {
		switch parts[0] {
		case "etc", "usr", "bin", "sbin", "lib", "lib64", "var", "proc", "sys", "dev", "boot", "root", "run", "opt", "srv", "tmp":
			return fmt.Errorf("The remote folder can't be inside the system folder /%s.", parts[0])
		}
		if len(parts) < 3 {
			return errors.New("An absolute remote folder must be at least three levels deep, such as /projects/mygroup/myjob.")
		}
	}
	return nil
}

// jobPollInterval is how often job_status checks the scheduler while waiting.
const jobPollInterval = 15 * time.Second

// sleepCtx sleeps for d or until ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tm.C:
		return nil
	}
}

// ---- read-only tools -----------------------------------------------------

func (t *toolset) listClusters() agent.Tool {
	return agent.Tool{
		Name:        "list_clusters",
		Description: "List the configured clusters with their type (slurm) and how files are transferred.",
		Schema:      objSchema(map[string]any{}),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var all []config.Cluster
			if t.env.Config != nil {
				all = t.env.Config.List()
			}
			if len(all) == 0 {
				return "No clusters are configured yet.", nil
			}
			var b strings.Builder
			for _, c := range all {
				fmt.Fprintf(&b, "- %s: type %s, transfer %s", c.Name, c.Type, c.EffectiveTransfer())
				if c.EffectiveTransfer() == config.TransferS3 && c.S3 != nil {
					fmt.Fprintf(&b, " (%s)", sanitize(c.S3.String()))
				}
				b.WriteString("\n")
			}
			return b.String(), nil
		},
	}
}

func (t *toolset) clusterInfo() agent.Tool {
	return agent.Tool{
		Name:        "cluster_info",
		Description: "Show the partitions of a cluster: state, time limit, GPU types and how many nodes are free.",
		Schema:      objSchema(map[string]any{"cluster": strProp("cluster name from list_clusters")}, "cluster"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Cluster string `json:"cluster"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			c, err := t.cluster(in.Cluster)
			if err != nil {
				return "", err
			}
			be, err := t.backendFor(ctx, c)
			if err != nil {
				return "", err
			}
			info, err := be.Info(ctx)
			if err != nil {
				return "", fmt.Errorf("I couldn't get the partition list from %s: %w", c.Name, err)
			}
			if len(info.Partitions) == 0 {
				return fmt.Sprintf("%s reports no partitions.", c.Name), nil
			}
			var b strings.Builder
			for _, p := range info.Partitions {
				fmt.Fprintf(&b, "- %s", p.Name)
				if p.Default {
					b.WriteString(" (default)")
				}
				fmt.Fprintf(&b, ": state %s, %d nodes (%d idle)", p.State, p.Nodes, p.IdleNodes)
				if p.MaxTime > 0 {
					fmt.Fprintf(&b, ", max time %s", p.MaxTime)
				}
				if len(p.GPUTypes) > 0 {
					fmt.Fprintf(&b, ", GPUs: %s", strings.Join(p.GPUTypes, ", "))
				}
				b.WriteString("\n")
			}
			b.WriteString(sanitizeLines(slurmPlacementNote(t.env.Site.SlurmFor(c.Name))))
			return b.String(), nil
		},
	}
}

// errSuffix is " (reason)" for a sanitized error, "" for nil.
func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return " (" + sanitize(err.Error()) + ")"
}

func (t *toolset) stageData() agent.Tool {
	return agent.Tool{
		Name:        "stage_data",
		Description: "Build the list of files (with sizes and checksums) to send for a job and create the job. Nothing is uploaded yet. root is the folder, paths are files or folders inside it. Without root it uses the user's chosen data folder, else their code folder.",
		Schema: objSchema(map[string]any{
			"root":     strProp("local folder that contains the data (from find_local_data; default: the chosen data folder, else the code folder)"),
			"paths":    strList("files or folders inside root, absolute or relative to root"),
			"job_name": strProp("short job name: starts with a letter or digit, then letters, digits, '.', '_' or '-'; at most 64 characters"),
		}, "paths", "job_name"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Root    string   `json:"root"`
				Paths   []string `json:"paths"`
				JobName string   `json:"job_name"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if !jobNameRe.MatchString(in.JobName) {
				return "", fmt.Errorf("The job name %q isn't allowed. Start with a letter or digit, then use up to 63 letters, digits, dots, dashes or underscores, without spaces. For example: %s (a folder name such as \"OneDrive - Example University\" is not a job name; the job name only names the folder on the cluster).", sanitize(in.JobName), safeJobName(in.JobName))
			}
			if len(in.Paths) == 0 {
				return "", errors.New("Tell me which files or folders to stage (paths).")
			}
			rootIn := t.resolve(in.Root)
			if rootIn == "" {
				if rootIn = t.defaultFolder(FolderData, FolderCode); rootIn == "" {
					return "", errors.New("Tell me which folder holds the files (root), or ask the user to choose their data folder.")
				}
			}
			root, err := t.insideRoots(rootIn)
			if err != nil {
				return "", err
			}
			paths, skipped, err := t.stagePaths(root, in.Paths)
			if err != nil {
				return "", err
			}
			entries, bskipped, err := transfer.BuildManifest(ctx, root, paths, nil)
			if err != nil {
				return "", fmt.Errorf("I couldn't stage those files: %w", err)
			}
			// Secret-looking names are never sent, wherever they sit.
			kept := entries[:0]
			for _, e := range entries {
				if checkRel(e.Path) != nil {
					skipped = append(skipped, e.Path)
					continue
				}
				kept = append(kept, e)
			}
			entries = kept
			skipped = append(skipped, bskipped...)
			sort.Strings(skipped)
			for i := range skipped {
				skipped[i] = sanitize(skipped[i])
			}
			if len(entries) == 0 {
				return "", errors.New("I found no regular files in those paths, so there is nothing to stage.")
			}
			files, size := transfer.Totals(entries)
			id, err := t.env.Store.CreateJob(ctx, store.Job{
				Name:     in.JobName,
				State:    store.StateStaging,
				LocalDir: filepath.Join(t.env.ResultsDir, in.JobName),
			})
			if err != nil {
				return "", fmt.Errorf("I couldn't create the job: %w", err)
			}
			if err := t.env.Store.SetManifest(ctx, id, entries); err != nil {
				return "", fmt.Errorf("I couldn't save the file list: %w", err)
			}
			if err := t.env.Store.AddEvent(ctx, id, eventStaged, root); err != nil {
				return "", fmt.Errorf("I couldn't save the staging folder: %w", err)
			}
			if skipped == nil {
				skipped = []string{}
			}
			out, _ := json.Marshal(map[string]any{
				"job_id": id, "files": files, "bytes": size, "skipped": capLines(skipped, maxResultLines),
			})
			return string(out), nil
		},
	}
}

// stagePaths checks the paths the user (or Claude) named for stage_data against
// the privacy rules and returns the paths to hand to BuildManifest. A path
// naming a private place is refused. A path equal to root is expanded to the
// root's non-private children (so a root such as the home folder never even
// reads ~/Library); the private children are returned as skipped.
func (t *toolset) stagePaths(root string, in []string) (paths, skipped []string, err error) {
	for _, rp := range in {
		if hasControl(rp) {
			return nil, nil, errors.New("A path contains an unusual character; please use plain file names.")
		}
		abs := rp
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, abs)
		}
		abs = filepath.Clean(abs)
		if !within(root, abs) {
			paths = append(paths, rp) // BuildManifest refuses it with the right message
			continue
		}
		if abs != root {
			if err := checkUnder(root, abs); err != nil {
				return nil, nil, err
			}
			// A junction or symlink inside the folder must not lead out of it
			// or into a private place. A path that doesn't exist is left to
			// BuildManifest, which reports it; any other failure to resolve
			// it is a refusal.
			real, err := evalLinks(abs)
			switch {
			case errors.Is(err, fs.ErrNotExist):
			case err != nil:
				return nil, nil, fmt.Errorf("I can't check where %s leads, so I won't stage it.", sanitize(rp))
			default:
				realRoot, rerr := evalLinks(root)
				if rerr != nil {
					realRoot = root
				}
				if !within(realRoot, real) {
					return nil, nil, fmt.Errorf("%s leads outside the folder, so I won't stage it.", sanitize(rp))
				}
				if err := checkUnder(realRoot, real); err != nil {
					return nil, nil, err
				}
			}
			paths = append(paths, rp)
			continue
		}
		kids, err := os.ReadDir(root)
		if err != nil {
			return nil, nil, fmt.Errorf("I can't read %s: %w", sanitize(root), err)
		}
		for _, k := range kids {
			if checkUnder(root, filepath.Join(root, k.Name())) != nil {
				skipped = append(skipped, k.Name())
				continue
			}
			paths = append(paths, filepath.Join(root, k.Name()))
		}
	}
	return paths, skipped, nil
}

func (t *toolset) draftJob() agent.Tool {
	return agent.Tool{
		Name:        "draft_job",
		Description: "Write the job script for a staged job on a cluster and save it on the job (nothing is submitted). work_dir defaults to the folder the data was uploaded to. When the uploaded code has no build or run script, read its README and build files (all pages) and pass the steps you derive as files (for example labmill-build.sh and labmill-run.sh): Labmill writes them into the job's folder on the cluster, never on this computer, after the user approved the review; make the command call them (bash labmill-run.sh). Only small text scripts, shown to the user with the job script. For a job that needs no files from this computer (for example one that only runs a program already on the cluster), leave out job_id: a new job is created from spec.name, and spec.work_dir is required. To redraft such a job, pass the job_id returned.",
		Schema: objSchema(map[string]any{
			"job_id":  intProp("job id from stage_data; leave out (or 0) for a job that needs no files from this computer"),
			"cluster": strProp("cluster name"),
			"scratch": map[string]any{
				"type":        "string",
				"enum":        []string{scratchLocal, scratchGlobal, scratchJobFolder},
				"description": "Where the job keeps its working data. local = the compute node's own disk (single-node jobs only; Labmill adds the stage-in, run, stage-out and removes the local folder after a good copy), when cluster_info names a node-local scratch; global = a folder on the cluster's shared scratch (upload_data to a remote_dir below it first), when cluster_info names one; job_folder (default) = the job's own folder. Prefer local for a single-node job, else global, else job_folder",
			},
			"preemptible": map[string]any{
				"type":        "boolean",
				"description": "Leave out to use the cluster's preemptible partition when cluster_info names one and the job fits (it is tried first, resubmitted automatically up to two more times, then once on the normal partition); false for a job that must not be interrupted or is longer than that partition allows; true only to insist",
			},
			"environment": map[string]any{
				"type":        "object",
				"description": "The software the job needs, written as setup lines before the command. Use the site's own module first (module spider; modules lists them, prerequisites first); build your own only when the site has none or its version is more than a year old. At most one of venv, conda and apptainer; modules combine with any. venv and conda environments are built once per requirements file hash and reused by later jobs (never cleaned up); the files are in the job folder (upload them or pass them as files)",
				"properties": map[string]any{
					"modules": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "module names for module load, for example the Python or CUDA module"},
					"venv": map[string]any{"type": "object", "description": "a Python venv from a requirements file", "properties": map[string]any{
						"requirements": strProp("requirements file, relative path in the job folder"),
						"python":       strProp("interpreter command such as python3.11 (default python3); put the module that provides it in modules"),
					}, "required": []string{"requirements"}},
					"conda": map[string]any{"type": "object", "description": "a conda environment from an environment file; load the site's conda module in modules", "properties": map[string]any{
						"file": strProp("environment.yml, relative path in the job folder"),
					}, "required": []string{"file"}},
					"apptainer": map[string]any{"type": "object", "description": "run the command in an Apptainer image (the image needs bash: the command runs as bash -c); a docker:// image is pulled once into a cache; load the apptainer module in modules if the site needs it", "properties": map[string]any{
						"image": strProp("docker://name:tag or a path to a .sif file"),
						"gpu":   map[string]any{"type": "boolean", "description": "add --nv so the container sees the GPUs (Labmill adds it anyway when spec.gpus is above 0)"},
					}, "required": []string{"image"}},
				},
			},
			"files": map[string]any{
				"type":        "array",
				"description": "optional small script files Labmill writes into the job's folder on the cluster (mode 0755) when the user approves the submit; shown in the review with the job script; a file that already exists there with other content is never overwritten, so pick new names. For code without a build or run script: labmill-build.sh, labmill-run.sh",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":    strProp("plain file name such as labmill-run.sh: no slash, no leading dot"),
						"content": strProp("the script text (at most 64 KiB), starting with a #! line"),
					},
					"required": []string{"name", "content"},
				},
			},
			"spec": map[string]any{
				"type":        "object",
				"description": "what to run and the resources it needs",
				"properties": map[string]any{
					"name":         strProp("job name (defaults to the job's name)"),
					"partition":    strProp("partition; empty for the cluster default. On a cluster with a preemptible partition Labmill submits there first and uses this one (when you name it) as the fallback after the failed attempts"),
					"nodes":        intProp("number of nodes; 0 or 1 for a single-node job (the default); local scratch needs a single node"),
					"gpus":         intProp("number of GPUs, 0 for none"),
					"gpu_type":     strProp("GPU type such as a100; empty for any"),
					"cpus":         intProp("CPU cores; 0 for the default"),
					"memory_gb":    intProp("memory in GB; 0 for the default"),
					"time_minutes": intProp("time limit in minutes; 0 for the partition default"),
					"command":      strProp("what to run, e.g. python train.py --epochs 10"),
					"work_dir":     strProp("absolute folder on the cluster to run in (defaults to the uploaded data folder); it must be on shared storage, not /tmp, /var/tmp or /dev/shm"),
					"env":          map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "extra environment variables"},
				},
				"required": []string{"command"},
			},
		}, "cluster", "spec"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				JobID       int64                `json:"job_id"`
				Cluster     string               `json:"cluster"`
				Files       []jobFile            `json:"files"`
				Scratch     string               `json:"scratch"`
				Preemptible *bool                `json:"preemptible"`
				Environment *backend.Environment `json:"environment"`
				Spec        struct {
					Name        string            `json:"name"`
					Partition   string            `json:"partition"`
					Nodes       int               `json:"nodes"`
					GPUs        int               `json:"gpus"`
					GPUType     string            `json:"gpu_type"`
					CPUs        int               `json:"cpus"`
					MemoryGB    int               `json:"memory_gb"`
					TimeMinutes int               `json:"time_minutes"`
					Command     string            `json:"command"`
					WorkDir     string            `json:"work_dir"`
					Env         map[string]string `json:"env"`
				} `json:"spec"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			// Without job_id: a job that needs no local data, created below
			// once its script renders.
			noData := in.JobID == 0
			var j store.Job
			if noData {
				if !jobNameRe.MatchString(in.Spec.Name) {
					return "", fmt.Errorf("Give the new job a name (spec.name) such as gpu-check: start with a letter or digit, then up to 63 letters, digits, dots, dashes or underscores, without spaces (got %q).", sanitize(in.Spec.Name))
				}
				if strings.TrimSpace(in.Spec.WorkDir) == "" {
					return "", errors.New("A job without uploaded data needs a work_dir: an absolute folder on a shared filesystem, such as under your home or a storage folder, never /tmp, /var/tmp or /dev/shm (node-local, the output would be lost).")
				}
				j.Name = in.Spec.Name
				// The same name again: refuse if that job holds data, else
				// redraft the existing no-data job instead of duplicating it.
				prev, err := t.env.Store.Jobs(ctx, store.JobFilter{})
				if err != nil {
					return "", fmt.Errorf("I couldn't look up your jobs: %w", err)
				}
				for _, p := range prev {
					if p.Name != j.Name || p.RemoteID != "" {
						continue
					}
					man, err := t.env.Store.Manifest(ctx, p.ID)
					if err != nil {
						return "", fmt.Errorf("I couldn't look up job %d: %w", p.ID, err)
					}
					if p.RemoteDir != "" || len(man) > 0 {
						return "", fmt.Errorf("Job %d already has this name and its data; pass job_id %d to draft for it.", p.ID, p.ID)
					}
					if j.ID == 0 {
						j = p
					}
				}
			} else {
				var err error
				if j, err = t.job(ctx, in.JobID); err != nil {
					return "", err
				}
			}
			c, err := t.cluster(in.Cluster)
			if err != nil {
				return "", err
			}
			if j.RemoteID != "" {
				return "", fmt.Errorf("Job %d (%s) was already submitted, so its script can't be changed. Stage a new job instead.", j.ID, j.Name)
			}
			if j.RemoteDir != "" && j.Cluster != "" && j.Cluster != c.Name {
				return "", fmt.Errorf("The data for job %d is on %s, not %s. Pick %s or upload the data again.", j.ID, j.Cluster, c.Name, j.Cluster)
			}
			s := in.Spec
			if s.Nodes < 0 {
				return "", errors.New("The number of nodes can't be negative.")
			}
			if s.TimeMinutes < 0 {
				return "", errors.New("The time limit can't be negative.")
			}
			spec := backend.JobSpec{
				Name: s.Name, Partition: s.Partition, GPUs: s.GPUs, GPUType: s.GPUType, CPUs: s.CPUs,
				MemoryGB: s.MemoryGB, Time: time.Duration(s.TimeMinutes) * time.Minute, Command: s.Command, Nodes: s.Nodes,
				WorkDir: s.WorkDir, Env: s.Env, Environment: in.Environment,
			}
			if spec.Name == "" {
				spec.Name = j.Name
			}
			if spec.WorkDir == "" && strings.HasPrefix(j.RemoteDir, "s3://") {
				return "", fmt.Errorf("Job %d's data is in S3 at %s, not on the cluster. Give work_dir as an absolute folder on the cluster. %s", j.ID, sanitize(j.RemoteDir), s3Hint(sanitize(j.RemoteDir)))
			}
			if spec.WorkDir == "" {
				spec.WorkDir = j.RemoteDir
			}
			if spec.WorkDir == "" {
				return "", errors.New("I don't know where to run the job. Upload the data first, or give a work_dir on the cluster.")
			}
			be, err := t.backendFor(ctx, c)
			if err != nil {
				return "", err
			}
			plan, err := t.planSlurm(ctx, c, be, j, &spec, slurmAsk{Scratch: in.Scratch, Preemptible: in.Preemptible})
			if err != nil {
				return "", err
			}
			script, err := be.Render(spec)
			if err != nil {
				return "", fmt.Errorf("That job setup isn't valid: %w", err)
			}
			plan.Estimate = estimateStart(ctx, be, script)
			specJSON, err := json.Marshal(spec)
			if err != nil {
				return "", fmt.Errorf("I couldn't save the job settings: %w", err)
			}
			files, err := validateJobFiles(in.Files)
			if err != nil {
				return "", err
			}
			if hasFileMarker(script) {
				return "", errors.New("The job script has a line that starts like Labmill's review marker for extra files; change it.")
			}
			if len(files) > 0 {
				if err := t.checkJobFilesTarget(c, spec); err != nil {
					return "", err
				}
			}
			if noData && j.ID == 0 {
				j.ID, err = t.env.Store.CreateJob(ctx, store.Job{
					Name: j.Name, Cluster: c.Name, Backend: be.Type(), State: store.StateDraft, Script: script,
					LocalDir: filepath.Join(t.env.ResultsDir, j.Name),
				})
				if err != nil {
					return "", fmt.Errorf("I couldn't create the job: %w", err)
				}
			} else {
				err = t.env.Store.UpdateJob(ctx, j.ID, func(j *store.Job) error {
					j.Script, j.Cluster, j.Backend, j.State = script, c.Name, be.Type(), store.StateDraft
					return nil
				})
				if err != nil {
					return "", fmt.Errorf("I couldn't save the script: %w", err)
				}
			}
			if err := t.env.Store.AddEvent(ctx, j.ID, eventSpec, string(specJSON)); err != nil {
				return "", fmt.Errorf("I couldn't save the job settings: %w", err)
			}
			planJSON, err := json.Marshal(plan)
			if err == nil {
				err = t.env.Store.AddEvent(ctx, j.ID, eventPlan, string(planJSON))
			}
			if err != nil {
				return "", fmt.Errorf("I couldn't save the job's placement plan: %w", err)
			}
			if err := t.saveJobFiles(ctx, j.ID, files); err != nil {
				return "", fmt.Errorf("I couldn't save the job's files: %w", err)
			}
			out := fmt.Sprintf("Drafted the script for job %d on %s (not submitted yet):\n%s", j.ID, c.Name, script)
			out += "\n\n" + plan.answer() + "\nTell the user this plan: they approve it, including the resubmissions, when they approve the submit."
			if note := t.failureNote(ctx, j.ID, c); note != "" {
				out += "\n\n" + note
			}
			if len(files) > 0 {
				names := make([]string, len(files))
				for i, f := range files {
					names[i] = f.Name
				}
				out += fmt.Sprintf("\n\nExtra files (shown to the user in the review below the script; written into %s on the cluster, mode 0755, when the user approves the submit): %s. Call them from the job command, for example bash %s.", sanitize(spec.WorkDir), strings.Join(names, ", "), names[0])
			}
			return out, nil
		},
	}
}

// maxWaitSeconds caps job_status's wait_seconds: a wait blocks the chat, and
// the background job monitor reports finished jobs anyway.
const maxWaitSeconds = 120

func (t *toolset) jobStatus() agent.Tool {
	return agent.Tool{
		Name:        "job_status",
		Description: "Ask the cluster for the current state of a submitted job and save it. Jobs run in the background and Labmill tells the user when one finishes, so normally call it without wait_seconds. Only when the user explicitly asks you to wait for a job, call job_status once with wait_seconds (at most 120) instead of calling it repeatedly; it checks every 15 seconds and returns when the job finishes or the wait is over. While it waits, the user can't use the chat.",
		Schema: objSchema(map[string]any{
			"job_id":       intProp("job id"),
			"wait_seconds": intProp("optional, only when the user asked to wait: keep checking every 15 seconds for up to this many seconds (0-120, default 0) until the job is finished"),
		}, "job_id"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				JobID       int64 `json:"job_id"`
				WaitSeconds int   `json:"wait_seconds"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if in.WaitSeconds < 0 || in.WaitSeconds > maxWaitSeconds {
				return "", fmt.Errorf("wait_seconds must be between 0 and %d.", maxWaitSeconds)
			}
			j, err := t.job(ctx, in.JobID)
			if err != nil {
				return "", err
			}
			be, c, err := t.jobBackend(ctx, j)
			if err != nil {
				return "", err
			}
			status := func() (backend.Status, error) {
				st, err := be.Status(ctx, j.RemoteID)
				if err != nil {
					return st, fmt.Errorf("I couldn't get the status of job %d from %s: %w", j.ID, c.Name, err)
				}
				return st, nil
			}
			st, err := status()
			if err != nil {
				return "", err
			}
			sleep := t.sleep
			if sleep == nil {
				sleep = sleepCtx
			}
			for waited := time.Duration(0); in.WaitSeconds > 0 && st.State != store.StateCompleted && !st.State.Terminal() && waited < time.Duration(in.WaitSeconds)*time.Second; {
				step := min(jobPollInterval, time.Duration(in.WaitSeconds)*time.Second-waited)
				if err := sleep(ctx, step); err != nil {
					return "", err
				}
				waited += step
				if st, err = status(); err != nil {
					return "", err
				}
			}
			// A preempted job of an approved plan is resubmitted, not ended
			// (M61.3); the monitor does the same for jobs nobody asks about.
			if (st.State == store.StateFailed || st.State == store.StateCanceled) && t.env.Resubmitter != nil &&
				(j.State == store.StateSubmitted || j.State == store.StatePending || j.State == store.StateRunning) &&
				t.env.Resubmitter.Handle(ctx, j, be, st) {
				return fmt.Sprintf("Job %d (%s on %s): %s (%s). Labmill is resubmitting it as planned; it said so in the chat. Check again later with job_status.", j.ID, j.Name, c.Name, st.State, st.Reason), nil
			}
			state := j.State
			if st.State.Valid() {
				state = st.State
				// Results already on the laptop stay "fetched".
				if j.State == store.StateFetched && st.State == store.StateCompleted {
					state = store.StateFetched
				}
				oldRemote := j.RemoteID
				err := t.env.Store.UpdateJob(ctx, j.ID, func(cur *store.Job) error {
					if cur.RemoteID != oldRemote {
						return errJobMoved // resubmitted meanwhile: st is about the old ID
					}
					cur.State, cur.Message = state, st.Reason
					return nil
				})
				if errors.Is(err, errJobMoved) {
					return fmt.Sprintf("Job %d (%s on %s) was resubmitted as another Slurm job while I asked; check it again with job_status.", j.ID, j.Name, c.Name), nil
				}
				if err != nil {
					return "", fmt.Errorf("I got the status but couldn't save it: %w", err)
				}
				// The first to see a job end is this tool, not the background
				// monitor: let the automatic download (M60.1) know.
				if t.env.OnJobEnded != nil && (state == store.StateCompleted || state == store.StateFailed) &&
					(j.State == store.StateSubmitted || j.State == store.StatePending || j.State == store.StateRunning) {
					t.env.OnJobEnded(j.ID)
				}
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Job %d (%s, %s on %s): %s", j.ID, j.Name, j.RemoteID, c.Name, st.State)
			if st.Reason != "" {
				fmt.Fprintf(&b, " (%s)", st.Reason)
			}
			if st.Node != "" {
				fmt.Fprintf(&b, ", node %s", st.Node)
			}
			if !st.StartTime.IsZero() {
				fmt.Fprintf(&b, ", started %s", st.StartTime.Format(time.RFC3339))
			}
			if !st.EndTime.IsZero() {
				fmt.Fprintf(&b, ", ended %s, exit code %d", st.EndTime.Format(time.RFC3339), st.ExitCode)
			}
			return b.String(), nil
		},
	}
}

func (t *toolset) listJobs() agent.Tool {
	return agent.Tool{
		Name:        "list_jobs",
		Description: "List the user's jobs with their state.",
		Schema:      objSchema(map[string]any{"active_only": map[string]any{"type": "boolean", "description": "only jobs that are not finished"}}),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				ActiveOnly bool `json:"active_only"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			jobs, err := t.env.Store.Jobs(ctx, store.JobFilter{ActiveOnly: in.ActiveOnly})
			if err != nil {
				return "", fmt.Errorf("I couldn't read the job list: %w", err)
			}
			if len(jobs) == 0 {
				return "You have no jobs yet.", nil
			}
			var lines []string
			for _, j := range jobs {
				l := fmt.Sprintf("- job %d %s: %s", j.ID, j.Name, j.State)
				if j.Cluster != "" {
					l += " on " + j.Cluster
				}
				if j.RemoteID != "" {
					l += " (" + j.RemoteID + ")"
				}
				if j.Message != "" {
					l += ", " + j.Message
				}
				lines = append(lines, l)
			}
			return strings.Join(capLines(lines, 100), "\n"), nil
		},
	}
}

func (t *toolset) clusterJobs() agent.Tool {
	return agent.Tool{
		Name: "cluster_jobs",
		Description: "List jobs on a cluster as the scheduler sees them: what is queued or running now and what finished in the last N days, " +
			"including jobs that were not started through Labmill. These are the user's own jobs. Use list_jobs for the jobs Labmill submitted.",
		Schema: objSchema(map[string]any{
			"cluster": strProp("cluster name from list_clusters (may be omitted when only one cluster is configured)"),
			"days":    intProp("also show jobs that finished in the last N days, 0 to 30 (default 7; 0 = only queued and running jobs)"),
			"limit":   intProp("at most this many jobs, 1 to 200 (default 50)"),
		}),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			in := struct {
				Cluster string `json:"cluster"`
				Days    *int   `json:"days"`
				Limit   int    `json:"limit"`
			}{}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			days := 7
			if in.Days != nil {
				days = *in.Days
			}
			if days < 0 || days > 30 {
				return "", errors.New("days must be between 0 and 30.")
			}
			if in.Limit == 0 {
				in.Limit = 50
			}
			if in.Limit < 1 || in.Limit > 200 {
				return "", errors.New("limit must be between 1 and 200.")
			}
			name := in.Cluster
			var all []config.Cluster
			if t.env.Config != nil {
				all = t.env.Config.List()
			}
			if name == "" && len(all) == 1 {
				name = all[0].Name
			}
			if name == "" && len(all) > 1 {
				var names []string
				for _, cl := range all {
					names = append(names, cl.Name)
				}
				return "Say which cluster: " + strings.Join(names, ", ") + ".", nil
			}
			c, err := t.cluster(name)
			if err != nil {
				return "", err
			}
			be, err := t.backendFor(ctx, c)
			if err != nil {
				return "", err
			}
			lister, ok := be.(backend.Lister)
			if !ok {
				return fmt.Sprintf("Listing all your jobs isn't available for %s (%s) yet. list_jobs shows the jobs Labmill submitted.", c.Name, be.Type()), nil
			}
			now := time.Now()
			if t.env.Now != nil {
				now = t.env.Now()
			}
			var since time.Time
			if days > 0 {
				since = now.AddDate(0, 0, -days)
			}
			// Ask for one more than shown, to know whether to say "N more".
			jobs, err := lister.Jobs(ctx, since, in.Limit+1)
			var note string
			switch {
			case errors.Is(err, backend.ErrNoAccounting):
				note = "Note: finished jobs aren't available on this cluster, so only queued and running jobs are shown."
			case errors.Is(err, backend.ErrTruncated):
				note = "Note: " + err.Error() + "."
			case err != nil:
				return "", fmt.Errorf("I couldn't list your jobs on %s: %w", c.Name, err)
			}
			if len(jobs) == 0 {
				out := fmt.Sprintf("No queued or running jobs on %s", c.Name)
				if days > 0 && note == "" {
					out += fmt.Sprintf(", and none finished in the last %s", plural(days, "day", "days"))
				}
				out += "."
				if note != "" {
					out += "\n" + note
				}
				return out, nil
			}
			more := 0
			if len(jobs) > in.Limit {
				more, jobs = 1, jobs[:in.Limit]
			}
			var lines []string
			for _, j := range jobs {
				lines = append(lines, formatRemoteJob(j, now))
			}
			if more > 0 {
				lines = append(lines, "... more jobs not shown (raise limit or lower days)")
			}
			if note != "" {
				lines = append(lines, note)
			}
			head := fmt.Sprintf("Jobs on %s (times in your local time zone)", c.Name)
			return head + ":\n" + strings.Join(lines, "\n"), nil
		},
	}
}

// formatRemoteJob renders one line: ID name state [partition] [node] [reason]
// [started/ended]. Text from the cluster has control characters removed.
func formatRemoteJob(j backend.RemoteJob, now time.Time) string {
	l := fmt.Sprintf("- %s %s: %s", oneLine(j.ID), clip(oneLine(j.Name), 60), j.State)
	if j.Partition != "" {
		l += ", " + clip(oneLine(j.Partition), 40)
	}
	if j.Node != "" {
		l += ", on " + clip(oneLine(j.Node), 60)
	}
	if j.Reason != "" {
		l += ", " + clip(oneLine(j.Reason), 60)
	}
	const layout = "Jan 2 15:04"
	if !j.Start.IsZero() {
		l += ", started " + j.Start.Local().Format(layout)
	} else if !j.Submit.IsZero() && (j.State == store.StatePending) {
		l += ", submitted " + j.Submit.Local().Format(layout)
	}
	if !j.End.IsZero() {
		l += ", ended " + j.End.Local().Format(layout)
	}
	return l
}

// oneLine cleans s and turns newlines and tabs into spaces.
func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(cleanText(s))
}

// clip shortens s to at most n runes, marking the cut with "...".
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-3]) + "..."
}

func (t *toolset) jobLogs() agent.Tool {
	return agent.Tool{
		Name:        "job_logs",
		Description: "Show the last lines of a submitted job's output (default 100, at most 500).",
		Schema: objSchema(map[string]any{
			"job_id": intProp("job id"),
			"lines":  intProp("how many lines from the end (default 100, max 500)"),
		}, "job_id"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				JobID int64 `json:"job_id"`
				Lines int   `json:"lines"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			if in.Lines <= 0 {
				in.Lines = 100
			}
			if in.Lines > 500 {
				in.Lines = 500
			}
			j, err := t.job(ctx, in.JobID)
			if err != nil {
				return "", err
			}
			be, c, err := t.jobBackend(ctx, j)
			if err != nil {
				return "", err
			}
			out, err := be.Logs(ctx, j.RemoteID, in.Lines)
			if err != nil {
				return "", fmt.Errorf("I couldn't get the output of job %d from %s: %w", j.ID, c.Name, err)
			}
			if strings.TrimSpace(out) == "" {
				return "The job has produced no output yet.", nil
			}
			const maxBytes = 32 << 10
			if len(out) > maxBytes {
				out = "(earlier output cut)\n" + out[len(out)-maxBytes:]
			}
			return cleanText(out), nil
		},
	}
}

// ---- approval-gated tools ------------------------------------------------

type uploadPlan struct {
	job     store.Job
	cluster config.Cluster
	root    string // resolved local staging root
	dir     string // remote dir: absolute, or relative to the home folder on the transfer host (S3: below the prefix)
	entries []store.ManifestEntry
	s3      *transfer.S3Remote // set for an s3 cluster
	rclone  string             // equivalent stand-alone rclone command, see rcloneNote
	via     string             // transfer host as shown in the approval ("alias (hostname)"); empty = the alias
}

func (t *toolset) uploadPlan(ctx context.Context, input json.RawMessage) (uploadPlan, error) {
	var in struct {
		JobID     int64  `json:"job_id"`
		Cluster   string `json:"cluster"`
		RemoteDir string `json:"remote_dir"`
	}
	var p uploadPlan
	if err := decode(input, &in); err != nil {
		return p, err
	}
	j, err := t.job(ctx, in.JobID)
	if err != nil {
		return p, err
	}
	c, err := t.cluster(in.Cluster)
	if err != nil {
		return p, err
	}
	switch {
	case j.RemoteID != "":
		return p, fmt.Errorf("Job %d (%s) was already submitted, so its data can't be uploaded again. Stage a new job instead.", j.ID, j.Name)
	case strings.TrimSpace(j.Script) != "" && j.Cluster != "" && j.Cluster != c.Name:
		return p, fmt.Errorf("Job %d already has a script for %s, so its data can't go to %s. Upload to %s, or stage a new job.", j.ID, j.Cluster, c.Name, j.Cluster)
	}
	entries, err := t.env.Store.Manifest(ctx, j.ID)
	if err != nil {
		return p, fmt.Errorf("I couldn't read the file list for job %d: %w", j.ID, err)
	}
	stagedRoot, ok, err := t.latestEvent(ctx, j.ID, eventStaged)
	if err != nil {
		return p, err
	}
	if len(entries) == 0 || !ok {
		return p, fmt.Errorf("Job %d has no staged files. Use stage_data first.", j.ID)
	}
	// The stored root and file list are re-checked: they must still be inside
	// the allowed folders and public.
	root, err := t.insideRoots(stagedRoot)
	if err != nil {
		return p, fmt.Errorf("The folder staged for job %d can't be used any more: %w", j.ID, err)
	}
	for _, e := range entries {
		if err := checkRel(e.Path); err != nil {
			return p, fmt.Errorf("The staged files for job %d include a private file: %w", j.ID, err)
		}
	}
	dir := in.RemoteDir
	if dir == "" {
		dir = "labmill/" + j.Name
		if c.EffectiveTransfer() == config.TransferS3 {
			dir = j.Name // directly under the configured prefix
		}
	}
	if err := checkRemoteDir(dir, c.EffectiveTransfer() != config.TransferS3); err != nil {
		return p, err
	}
	plan := uploadPlan{job: j, cluster: c, root: root, dir: dir, entries: entries, via: t.via(c)}
	if c.EffectiveTransfer() == config.TransferS3 {
		// Data never silently takes a different route than the configured one.
		r, err := s3Remote(c, dir)
		if err != nil {
			return p, err
		}
		plan.s3 = &r
		plan.rclone = t.rcloneNote(c, &r, dir, root, uploadRels(entries), true)
		return plan, nil
	}
	if _, err := t.remote(c, dir); err != nil {
		return p, err
	}
	plan.rclone = t.rcloneNote(c, nil, dir, root, uploadRels(entries), true)
	return plan, nil
}

// maxFolderLines caps the per-folder breakdown in an upload proposal.
const maxFolderLines = 50

func (p uploadPlan) proposal() agent.Proposal {
	files, size := transfer.Totals(p.entries)
	type group struct {
		files int
		size  int64
	}
	groups := map[string]*group{}
	var names []string
	lines := make([]string, len(p.entries))
	for i, e := range p.entries {
		top := "(files directly in the folder)"
		if k := strings.IndexByte(e.Path, '/'); k >= 0 {
			top = e.Path[:k] + "/"
		}
		g := groups[top]
		if g == nil {
			g = &group{}
			groups[top] = g
			names = append(names, top)
		}
		g.files++
		g.size += e.Size
		lines[i] = fmt.Sprintf("%s (%s)", sanitize(e.Path), humanSize(e.Size))
	}
	sort.Strings(names)
	folders := make([]string, len(names))
	for i, n := range names {
		g := groups[n]
		folders[i] = fmt.Sprintf("%s: %s, %s", sanitize(n), plural(g.files, "file", "files"), humanSize(g.size))
	}
	details := "By folder:\n" + strings.Join(capLines(folders, maxFolderLines), "\n") +
		"\n\nFiles:\n" + strings.Join(capLines(lines, maxDetailLines), "\n") +
		fmt.Sprintf("\n\nTotal: %s, %s", plural(files, "file", "files"), humanSize(size))
	shown := p.dir
	if !strings.HasPrefix(shown, "/") {
		shown = "~/" + shown // relative to the login home, never confusable with an absolute path
	}
	dest := fmt.Sprintf("%s:%s (through %s)", p.cluster.Name, sanitize(shown), sanitize(viaLabel(p.cluster, p.via)))
	if p.s3 != nil {
		dest = s3Where(*p.s3)
	}
	return agent.Proposal{
		Tool:    "upload_data",
		Summary: fmt.Sprintf("Upload %s (%s) from %s to %s", plural(files, "file", "files"), humanSize(size), sanitize(p.root), dest),
		Details: details,
	}
}

func (t *toolset) uploadData() agent.Tool {
	return agent.Tool{
		Name:        "upload_data",
		Description: "Upload a staged job's files to a cluster. The user must approve the file list and destination. remote_dir is a folder on the cluster (relative to your home folder, or an absolute path such as a group storage folder), or below the cluster's S3 prefix for S3 clusters (relative only). It defaults to labmill/<job name> (S3 clusters: <job name>).",
		Schema: objSchema(map[string]any{
			"job_id":     intProp("job id from stage_data"),
			"cluster":    strProp("destination cluster"),
			"remote_dir": strProp("folder on the cluster (relative to your home folder, or absolute such as /projects/mygroup/myjob), or below the cluster's S3 prefix for S3 clusters, e.g. labmill/myjob (default labmill/<job name>; S3: <job name>)"),
		}, "job_id", "cluster"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			p, err := t.uploadPlan(context.Background(), input)
			if err != nil {
				return agent.Proposal{}, err
			}
			return p.proposal(), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			p, err := t.uploadPlan(ctx, input)
			if err != nil {
				return "", err
			}
			if err := sameAsApproved(p.proposal(), d.Proposal); err != nil {
				return "", err
			}
			j, c, dir := p.job, p.cluster, p.dir
			if (p.s3 != nil && t.env.UploadS3 == nil) || (p.s3 == nil && t.env.Upload == nil) {
				return "", errors.New("Uploading isn't available right now.")
			}
			dst, _ := t.remote(c, dir)
			var replaced []string // existing remote files overwritten with a different size
			dst.OnReplace = func(rel string, _ int64) { replaced = append(replaced, rel) }
			prevState, prevMessage := j.State, j.Message
			err = t.env.Store.UpdateJob(ctx, j.ID, func(j *store.Job) error { j.State = store.StateUploading; return nil })
			if err != nil {
				return "", fmt.Errorf("I couldn't update job %d before uploading: %w", j.ID, err)
			}
			var upErr error
			cb, finish := t.startTransfer("upload_data", j.ID, p.rclone)
			defer finish()
			if p.s3 != nil {
				upErr = t.env.UploadS3(ctx, p.root, p.entries, *p.s3, cb)
			} else {
				upErr = t.env.Upload(ctx, p.root, p.entries, dst, cb)
			}
			summary := finish()
			if err := upErr; err != nil {
				// A failed upload leaves the job as it was (a drafted job stays drafted).
				rerr := t.env.Store.UpdateJob(ctx, j.ID, func(j *store.Job) error {
					j.State, j.Message = prevState, prevMessage
					return nil
				})
				msg := fmt.Sprintf("The upload to %s didn't finish: %v. You can run it again; files already sent are skipped.", c.Name, err)
				if rerr != nil {
					msg += fmt.Sprintf(" (I also couldn't restore the job's state: %v)", rerr)
				}
				return "", errors.New(msg)
			}
			remoteDir := dir
			var homeErr error
			var home string
			switch {
			case p.s3 != nil:
				remoteDir = p.s3.String() // the job's data lives at this S3 location
			case path.IsAbs(dir):
				// An absolute folder (e.g. group storage) needs no home lookup.
			case t.env.RemoteHome == nil:
				homeErr = errors.New("remote home lookup isn't available")
			default:
				if home, homeErr = t.env.RemoteHome(ctx, c); homeErr == nil {
					remoteDir = path.Join(home, dir)
				}
			}
			if p.s3 != nil {
				if err := t.env.Store.AddEvent(ctx, j.ID, eventS3, "endpoint="+normEndpoint(p.s3.Endpoint)); err != nil {
					return "", fmt.Errorf("The files were uploaded but I couldn't record where: %w", err)
				}
			}
			err = t.env.Store.UpdateJob(ctx, j.ID, func(j *store.Job) error {
				j.Cluster, j.Backend, j.RemoteDir, j.State, j.Message = c.Name, string(c.Type), remoteDir, store.StateDraft, ""
				return nil
			})
			if err != nil {
				return "", fmt.Errorf("The files were uploaded but I couldn't record that: %w", err)
			}
			if homeErr != nil {
				return "", fmt.Errorf("The files were uploaded to %s:%s, but I couldn't find the full path of that folder (%v). Give draft_job a work_dir as an absolute path.", c.Name, dir, homeErr)
			}
			files, size := transfer.Totals(p.entries)
			if p.s3 != nil {
				return withTransfer(fmt.Sprintf("Uploaded %s (%s) to %s. The data is in S3, not on the cluster's disks. When you draft the job, give work_dir as an absolute folder on the cluster. %s", plural(files, "file", "files"), humanSize(size), sanitize(remoteDir), s3Hint(sanitize(remoteDir))), summary, p.rclone), nil
			}
			msg := fmt.Sprintf("Uploaded %s (%s) to %s:%s.", plural(files, "file", "files"), humanSize(size), c.Name, remoteDir) + replacedNote(replaced)
			return withTransfer(msg, summary, p.rclone), nil
		},
	}
}

// maxReplacedShown is how many replaced file names the upload result lists.
const maxReplacedShown = 10

// replacedNote is the sentence appended to an upload result when the upload
// overwrote files that were already on the cluster with a different size
// ("" when none): the count and up to maxReplacedShown names.
func replacedNote(rels []string) string {
	if len(rels) == 0 {
		return ""
	}
	shown := rels
	if len(shown) > maxReplacedShown {
		shown = shown[:maxReplacedShown]
	}
	names := make([]string, len(shown))
	for i, r := range shown {
		names[i] = sanitize(r)
	}
	note := fmt.Sprintf(" Replaced %s on the cluster (they had a different size): %s", plural(len(rels), "existing file", "existing files"), strings.Join(names, ", "))
	if more := len(rels) - len(shown); more > 0 {
		note += fmt.Sprintf(", and %d more", more)
	}
	return note + "."
}

// jobWorkDir is the work folder of the job's latest draft ("" when unknown).
func (t *toolset) jobWorkDir(ctx context.Context, jobID int64) string {
	text, ok, err := t.latestEvent(ctx, jobID, eventSpec)
	var spec backend.JobSpec
	if err != nil || !ok || json.Unmarshal([]byte(text), &spec) != nil {
		return ""
	}
	return spec.WorkDir
}

func (t *toolset) submitJob() agent.Tool {
	return agent.Tool{
		Name:          "submit_job",
		Description:   "Submit a drafted job. The user reviews (and may edit) the exact script before it is submitted.",
		Schema:        objSchema(map[string]any{"job_id": intProp("job id with a drafted script")}, "job_id"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			j, err := t.submittable(context.Background(), input)
			if err != nil {
				return agent.Proposal{}, err
			}
			files, err := t.jobFilesOf(context.Background(), j.ID)
			if err != nil {
				return agent.Proposal{}, err
			}
			p := submitProposal(j, files, t.jobWorkDir(context.Background(), j.ID), t.planReview(context.Background(), j.ID))
			if pf := t.preflight(context.Background(), j); pf != "" {
				p.Summary += preflightMark + pf // read-only; M64.2
			}
			return p, nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			j, err := t.submittable(ctx, input)
			if err != nil {
				return "", err
			}
			stored, err := t.jobFilesOf(ctx, j.ID)
			if err != nil {
				return "", err
			}
			// The job as stored now must still be the one the user saw: same
			// name, cluster and drafted script. Only then is the job's
			// cluster used below.
			built := submitProposal(j, stored, t.jobWorkDir(ctx, j.ID), t.planReview(ctx, j.ID))
			if err := sameAsApproved(built, withoutPreflight(built, d.Proposal)); err != nil {
				return "", err
			}
			// What runs is exactly what the user approved: the edited text, or
			// the proposal's details; never the script as currently stored.
			script := d.Edited
			if script == "" {
				script = d.Proposal.Details
			}
			if strings.TrimSpace(script) == "" {
				return "", errors.New("There is no script to submit.")
			}
			// The review carries the extra files below the script; what the
			// user left (or edited) is what gets written.
			script, files, err := splitReviewFiles(script)
			if err != nil {
				return "", err
			}
			specJSON, ok, err := t.latestEvent(ctx, j.ID, eventSpec)
			if err != nil {
				return "", err
			}
			var spec backend.JobSpec
			if !ok || json.Unmarshal([]byte(specJSON), &spec) != nil {
				return "", fmt.Errorf("I can't find the settings for job %d. Run draft_job again.", j.ID)
			}
			c, err := t.cluster(j.Cluster)
			if err != nil {
				return "", err
			}
			be, err := t.backendFor(ctx, c)
			if err != nil {
				return "", err
			}
			if len(files) > 0 {
				if err := t.checkJobFilesTarget(c, spec); err != nil {
					return "", err
				}
				if err := t.writeJobFiles(ctx, c, spec.WorkDir, files); err != nil {
					return "", err
				}
			}
			id, err := be.Submit(ctx, script, spec)
			if err != nil {
				return "", fmt.Errorf("%s didn't accept the job: %w", c.Name, err)
			}
			// Record even if the user quit right after the submit.
			err = t.env.Store.UpdateJob(context.WithoutCancel(ctx), j.ID, func(j *store.Job) error {
				j.RemoteID, j.Script, j.State, j.Message = id, script, store.StateSubmitted, ""
				return nil
			})
			if err != nil {
				return "", fmt.Errorf("The job was submitted to %s as %s, but I couldn't record that: %w", c.Name, id, err)
			}
			return fmt.Sprintf("Submitted job %d (%s) to %s. The cluster's job ID is %s.", j.ID, j.Name, c.Name, id), nil
		},
	}
}

// submitProposal shows the job's script for approval.
func submitProposal(j store.Job, files []jobFile, workDir, plan string) agent.Proposal {
	summary := fmt.Sprintf("Submit job %s to %s", sanitize(j.Name), sanitize(j.Cluster))
	if plan != "" {
		summary += ". " + plan // M61: storage, partition, fallback plan and start estimate
	}
	if len(files) > 0 {
		names := make([]string, len(files))
		for i, f := range files {
			names[i] = f.Name
		}
		summary += ". Before it submits, Labmill also writes " + plural(len(files), "file", "files") + " into " + sanitize(workDir) + " on the cluster (mode 0755, shown below the script): " + sanitize(strings.Join(names, ", "))
	}
	if j.Backend == string(config.TypeSlurm) {
		if dirs := slurm.ScriptDirs(j.Script); len(dirs) > 0 {
			summary += ". From the submitted script's --chdir/--output folders it first runs `mkdir -p -- " + sanitize(strings.Join(dirs, " ")) + "` (creates them if missing), then sbatch"
		}
	}
	return agent.Proposal{
		Tool:     "submit_job",
		Summary:  summary,
		Details:  reviewWithFiles(j.Script, files),
		Editable: true,
	}
}

func (t *toolset) submittable(ctx context.Context, input json.RawMessage) (store.Job, error) {
	var in struct {
		JobID int64 `json:"job_id"`
	}
	if err := decode(input, &in); err != nil {
		return store.Job{}, err
	}
	j, err := t.job(ctx, in.JobID)
	if err != nil {
		return j, err
	}
	switch {
	case j.RemoteID != "":
		return j, fmt.Errorf("Job %d (%s) was already submitted (ID %s).", j.ID, j.Name, j.RemoteID)
	case strings.TrimSpace(j.Script) == "" || j.Cluster == "":
		return j, fmt.Errorf("Job %d has no drafted script yet. Use draft_job first.", j.ID)
	}
	return j, nil
}

func (t *toolset) cancelJob() agent.Tool {
	return agent.Tool{
		Name:          "cancel_job",
		Description:   "Cancel a submitted job. The user must approve.",
		Schema:        objSchema(map[string]any{"job_id": intProp("job id")}, "job_id"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			j, err := t.cancelable(context.Background(), input)
			if err != nil {
				return agent.Proposal{}, err
			}
			return cancelProposal(j), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			j, err := t.cancelable(ctx, input)
			if err != nil {
				return "", err
			}
			if err := sameAsApproved(cancelProposal(j), d.Proposal); err != nil {
				return "", err
			}
			be, c, err := t.jobBackend(ctx, j)
			if err != nil {
				return "", err
			}
			if err := be.Cancel(ctx, j.RemoteID); err != nil {
				return "", fmt.Errorf("I couldn't cancel job %d on %s: %w", j.ID, c.Name, err)
			}
			err = t.env.Store.UpdateJob(ctx, j.ID, func(j *store.Job) error { j.State = store.StateCanceled; return nil })
			if err != nil {
				return "", fmt.Errorf("The job was canceled on %s but I couldn't record that: %w", c.Name, err)
			}
			return fmt.Sprintf("Canceled job %d (%s) on %s.", j.ID, j.Name, c.Name), nil
		},
	}
}

func cancelProposal(j store.Job) agent.Proposal {
	return agent.Proposal{Tool: "cancel_job", Summary: fmt.Sprintf("Cancel job %s (%s) on %s", sanitize(j.Name), sanitize(j.RemoteID), sanitize(j.Cluster))}
}

func (t *toolset) cancelable(ctx context.Context, input json.RawMessage) (store.Job, error) {
	var in struct {
		JobID int64 `json:"job_id"`
	}
	if err := decode(input, &in); err != nil {
		return store.Job{}, err
	}
	j, err := t.job(ctx, in.JobID)
	if err != nil {
		return j, err
	}
	if j.RemoteID == "" {
		return j, fmt.Errorf("Job %d (%s) hasn't been submitted, so there is nothing to cancel.", j.ID, j.Name)
	}
	if j.State.Terminal() {
		return j, fmt.Errorf("Job %d (%s) is already %s.", j.ID, j.Name, j.State)
	}
	return j, nil
}

type fetchPlan struct {
	job      store.Job
	cluster  config.Cluster
	paths    []string
	localDir string
	s3       *transfer.S3Remote // set for an s3 cluster
	rclone   string             // equivalent stand-alone rclone command, see rcloneNote
	via      string             // transfer host as shown in the approval ("alias (hostname)"); empty = the alias
	auto     bool               // an automatic download (M60.1): a failed job stays failed
}

// uploadRels returns the manifest's paths.
func uploadRels(entries []store.ManifestEntry) []string {
	rels := make([]string, len(entries))
	for i, e := range entries {
		rels[i] = e.Path
	}
	return rels
}

// resolveLoose resolves symlinks in the part of p that exists and appends the
// rest, so a folder that will be created can be checked.
func resolveLoose(p string) (string, error) {
	p = filepath.Clean(p)
	var rest []string
	for cur := p; ; {
		if r, err := evalLinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r, nil
		}
		if _, err := os.Lstat(cur); err == nil {
			return "", fmt.Errorf("I can't resolve %s.", sanitize(cur))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("I can't resolve %s.", sanitize(p))
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// localDirOK checks that a download folder (absolute) lies inside the results
// folder or a LocalRoot and passes the privacy rules.
func (t *toolset) localDirOK(local string) error {
	real, err := resolveLoose(local)
	if err != nil {
		return err
	}
	bases := t.roots()
	if t.env.ResultsDir != "" {
		bases = append([]string{t.env.ResultsDir}, bases...)
	}
	var private error
	for _, b := range bases {
		rb, err := resolveLoose(b)
		if err != nil || !within(rb, real) {
			continue
		}
		if err := checkUnder(rb, real); err != nil {
			private = err
			continue
		}
		return nil
	}
	if private != nil {
		return private
	}
	return fmt.Errorf("%s is outside the folders I may write to (your results folder and %s).", sanitize(local), sanitize(strings.Join(t.roots(), ", ")))
}

func (t *toolset) fetchPlan(ctx context.Context, input json.RawMessage) (fetchPlan, error) {
	var in struct {
		JobID    int64    `json:"job_id"`
		Paths    []string `json:"paths"`
		LocalDir string   `json:"local_dir"`
	}
	var p fetchPlan
	if err := decode(input, &in); err != nil {
		return p, err
	}
	j, err := t.job(ctx, in.JobID)
	if err != nil {
		return p, err
	}
	if j.RemoteDir == "" || j.Cluster == "" {
		return p, fmt.Errorf("Job %d (%s) has no folder on a cluster to download from.", j.ID, sanitize(j.Name))
	}
	c, err := t.cluster(j.Cluster)
	if err != nil {
		return p, err
	}
	var s3 *transfer.S3Remote
	if c.EffectiveTransfer() == config.TransferS3 {
		// The job's folder was recorded as s3://bucket/prefix/dir by upload_data.
		r, err := s3Remote(c, "")
		if err != nil {
			return p, err
		}
		base := r.String() + "/"
		if !strings.HasPrefix(j.RemoteDir, base) || strings.TrimPrefix(j.RemoteDir, base) == "" {
			return p, fmt.Errorf("Job %d's folder %s isn't inside this cluster's S3 location %s, so I can't download from it.", j.ID, sanitize(j.RemoteDir), sanitize(r.String()))
		}
		r.Dir = strings.TrimPrefix(j.RemoteDir, base)
		if rec, ok, err := t.latestEvent(ctx, j.ID, eventS3); err != nil {
			return p, err
		} else if ok && normEndpoint(strings.TrimPrefix(rec, "endpoint=")) != normEndpoint(r.Endpoint) {
			return p, fmt.Errorf("Job %d's data was uploaded through a different S3 endpoint (%s) than cluster %s uses now (%q), so I won't download from it.", j.ID, sanitize(strings.TrimPrefix(rec, "endpoint=")), c.Name, sanitize(r.Endpoint))
		}
		s3 = &r
	} else if _, err := t.remote(c, j.RemoteDir); err != nil {
		return p, err
	}
	if len(in.Paths) == 0 {
		return p, errors.New("Tell me which files or folders to download (paths, relative to the job's folder).")
	}
	for _, rp := range in.Paths {
		if strings.TrimSpace(rp) == "" || strings.HasPrefix(rp, "/") {
			return p, fmt.Errorf("The path %q must be relative to the job's folder %s.", sanitize(rp), sanitize(j.RemoteDir))
		}
		if hasControl(rp) {
			return p, errors.New("A path contains an unusual character; please use plain file names.")
		}
		for _, part := range strings.Split(filepath.ToSlash(rp), "/") {
			if part == ".." {
				return p, fmt.Errorf("The path %q goes outside the job's folder.", rp)
			}
		}
	}
	if hasControl(in.LocalDir) {
		return p, errors.New("The local folder contains an unusual character; please use a plain folder name.")
	}
	local := in.LocalDir
	if local == "" {
		if t.env.ResultsDir == "" {
			return p, errors.New("No results folder is set up; give a local_dir inside your allowed folders.")
		}
		if !jobNameRe.MatchString(j.Name) {
			return p, fmt.Errorf("Job %d has a name I can't use as a folder name; give a local_dir.", j.ID)
		}
		local = filepath.Join(t.env.ResultsDir, j.Name)
	}
	if local, err = filepath.Abs(local); err != nil {
		return p, fmt.Errorf("I can't use %q as a local folder: %w", sanitize(in.LocalDir), err)
	}
	if err := t.localDirOK(local); err != nil {
		return p, err
	}
	// Each file lands at local/<path>: check the real destination too, so a
	// path like ".ssh/authorized_keys" or one through a symlinked folder
	// can't write where local_dir itself wouldn't be allowed.
	for _, rp := range in.Paths {
		if err := t.localDirOK(filepath.Join(local, filepath.FromSlash(rp))); err != nil {
			return p, fmt.Errorf("Downloading %q would write somewhere I'm not allowed to: %v", sanitize(rp), err)
		}
	}
	dir := j.RemoteDir
	if s3 != nil {
		dir = ""
	}
	return fetchPlan{job: j, cluster: c, paths: in.Paths, localDir: local, s3: s3, via: t.via(c),
		rclone: t.rcloneNote(c, s3, dir, local, in.Paths, false)}, nil
}

func (p fetchPlan) proposal() agent.Proposal {
	from := fmt.Sprintf("%s:%s (through %s)", sanitize(p.cluster.Name), sanitize(p.job.RemoteDir), sanitize(viaLabel(p.cluster, p.via)))
	if p.s3 != nil {
		from = s3Where(*p.s3)
	}
	return agent.Proposal{
		Tool:    "fetch_results",
		Summary: fmt.Sprintf("Download %s from %s to %s", sanitize(strings.Join(p.paths, ", ")), from, sanitize(p.localDir)),
	}
}

// download runs an approved (or automatic, M60.1) fetch plan: it downloads
// the paths, checks them against the cluster, and marks the job fetched
// unless a file's SHA-256 differs. summary is the transfer's closing line.
func (t *toolset) download(ctx context.Context, p fetchPlan) (written []string, verified transfer.Verification, summary string, err error) {
	if (p.s3 != nil && t.env.DownloadS3 == nil) || (p.s3 == nil && t.env.Download == nil) {
		return nil, verified, "", errors.New("Downloading isn't available right now.")
	}
	// One download of a job at a time: a fetch_results of the model and an
	// automatic download of the same job don't run side by side.
	if err := lockJob(ctx, t.env.Store, p.job.ID, "download"); err != nil {
		return nil, verified, "", fmt.Errorf("Another download of this job was still running: %w", err)
	}
	defer unlockJob(t.env.Store, p.job.ID, "download")
	cb, finish := t.startTransfer("fetch_results", p.job.ID, p.rclone)
	defer finish()
	if p.s3 != nil {
		written, err = t.env.DownloadS3(ctx, *p.s3, p.paths, p.localDir, cb)
	} else {
		src, _ := t.remote(p.cluster, p.job.RemoteDir)
		src.OnVerify = func(v transfer.Verification) { verified = v }
		written, err = t.env.Download(ctx, src, p.paths, p.localDir, cb)
	}
	summary = finish()
	if err != nil {
		return nil, verified, summary, fmt.Errorf("The download from %s didn't finish: %w", p.cluster.Name, err)
	}
	// A job with a file that differs from the cluster's isn't "fetched"
	// yet: the state stays, so the results are fetched again.
	if len(verified.Mismatched) == 0 && len(verified.Unrenamed) == 0 {
		t.recordDownloaded(ctx, p, written, verified)
		err = t.env.Store.UpdateJob(ctx, p.job.ID, func(j *store.Job) error {
			// An automatic download keeps a failed job failed (the failure
			// stays visible); the folder is recorded either way.
			if !(p.auto && j.State == store.StateFailed) {
				j.State = store.StateFetched
			}
			j.LocalDir, j.Downloaded = p.localDir, true
			return nil
		})
		if err != nil {
			return nil, verified, summary, fmt.Errorf("The files were downloaded but I couldn't record that: %w", err)
		}
	}
	return written, verified, summary, nil
}

func (t *toolset) fetchResults() agent.Tool {
	return agent.Tool{
		Name:        "fetch_results",
		Description: "Download results from a job's folder on the cluster to this computer. The user must approve. local_dir defaults to the results folder for this job.",
		Schema: objSchema(map[string]any{
			"job_id":    intProp("job id"),
			"paths":     strList("files or folders to download, relative to the job's folder on the cluster"),
			"local_dir": strProp("local folder to put them in, inside your results folder or allowed folders (default: the Labmill results folder for this job)"),
		}, "job_id", "paths"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			p, err := t.fetchPlan(context.Background(), input)
			if err != nil {
				return agent.Proposal{}, err
			}
			return p.proposal(), nil
		},
		Run: func(ctx context.Context, input json.RawMessage, d agent.Decision) (string, error) {
			p, err := t.fetchPlan(ctx, input)
			if err != nil {
				return "", err
			}
			if err := sameAsApproved(p.proposal(), d.Proposal); err != nil {
				return "", err
			}
			written, verified, summary, err := t.download(ctx, p)
			if err != nil {
				return "", err
			}
			sort.Strings(written)
			for i := range written {
				written[i] = sanitize(written[i])
			}
			out := withTransfer(fmt.Sprintf("Downloaded %s to %s:\n%s%s", plural(len(written), "file", "files"), sanitize(p.localDir),
				strings.Join(capLines(written, maxResultLines), "\n"), verifyNote(verified)), summary, p.rclone)
			if offer := t.cleanupOffer(ctx, p.job.ID); offer != "" {
				out += "\n\n" + offer
			}
			return out, nil
		},
	}
}

// normEndpoint normalizes an S3 endpoint URL for comparison: lowercase scheme
// and host, no trailing slash, so a harmless config edit doesn't block a
// fetch. An empty endpoint (AWS) stays empty.
func normEndpoint(e string) string {
	e = strings.TrimRight(strings.TrimSpace(e), "/")
	u, err := url.Parse(e)
	if err != nil || u.Host == "" {
		return e
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return u.String()
}
