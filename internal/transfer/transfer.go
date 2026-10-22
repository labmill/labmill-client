// Package transfer stages local data and moves it to and from the cluster.
//
// Staging builds a manifest (relative path, size, SHA-256) of exactly the
// files the user approved. Uploads and downloads use rclone as a library
// (its sftp backend, run in-process) with rclone's "external ssh" mode
// pointed at `labmill ssh-exec <alias>`. That reuses Labmill's own logged-in
// SSH connection through the helper, so a transfer never asks the user to
// log in again.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/rclone/rclone/backend/local" // registers the local backend
	_ "github.com/rclone/rclone/backend/sftp"  // registers the sftp backend
	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/filter"
	"github.com/rclone/rclone/fs/operations"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/fs/walk"
	"github.com/rclone/rclone/lib/encoder"

	"labmill-client/internal/store"
)

// BuildManifest walks paths (each absolute, or relative to root; files or
// directories) and returns one entry per regular file: Path relative to root
// with forward slashes, Size, and lowercase hex SHA-256. Symlinks and special
// files are not followed; their root-relative paths are returned in skipped.
// Hidden entries (a path component starting with ".") are skipped too and
// reported in skipped, unless the caller named that hidden path explicitly.
// A path equal to root (e.g. ".") stages everything under root.
// Any path that resolves outside root is an error (nothing outside the user's
// selection may be staged). Entries are sorted by Path and de-duplicated
// (overlapping selections). progress, if not nil, is called with bytes hashed
// so far and the total, at least once per file. ctx cancellation stops the
// walk.
func BuildManifest(ctx context.Context, root string, paths []string, progress func(done, total int64)) (entries []store.ManifestEntry, skipped []string, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", root, err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", root, err)
	}
	type file struct {
		abs, rel string
		size     int64
	}
	var files []file
	seen := map[string]bool{}
	skippedSet := map[string]bool{}
	var total int64

	for _, p := range paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		p = filepath.Clean(p)
		if !within(root, p) {
			return nil, nil, fmt.Errorf("%s is outside %s; only files inside the chosen folder can be sent", p, root)
		}
		// A path equal to root is walked as the resolved root (so a symlinked
		// root works); anything else is walked as named.
		walkStart, relBase := p, root
		if p == root {
			walkStart, relBase = realRoot, realRoot
		} else if realParent, err := filepath.EvalSymlinks(filepath.Dir(p)); err != nil {
			// A symlinked parent directory could lead out of root.
			return nil, nil, fmt.Errorf("resolve %s: %w", p, err)
		} else if !within(realRoot, realParent) {
			return nil, nil, fmt.Errorf("%s is outside %s; only files inside the chosen folder can be sent", p, root)
		}
		err := filepath.WalkDir(walkStart, func(path string, d fs.DirEntry, werr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if werr != nil {
				return werr
			}
			rel, err := filepath.Rel(relBase, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			// Hidden entries below the named path are never staged; the named
			// path itself (and its parents) may be hidden.
			if path != walkStart && strings.HasPrefix(d.Name(), ".") {
				skippedSet[rel] = true
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			switch {
			case d.IsDir():
				return nil
			case !d.Type().IsRegular():
				skippedSet[rel] = true
				return nil
			}
			if seen[rel] {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			seen[rel] = true
			files = append(files, file{abs: path, rel: rel, size: info.Size()})
			total += info.Size()
			return nil
		})
		if err != nil {
			return nil, nil, fmt.Errorf("scan %s: %w", p, err)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	var done int64
	for _, f := range files {
		sum, n, err := hashFile(ctx, f.abs, func(delta int64) {
			done += delta
			if progress != nil && done <= total {
				progress(done, total)
			}
		})
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", f.rel, err)
		}
		entries = append(entries, store.ManifestEntry{Path: f.rel, Size: n, SHA256: sum})
		if progress != nil && f.size == 0 {
			progress(done, total)
		}
	}
	for s := range skippedSet {
		skipped = append(skipped, s)
	}
	sort.Strings(skipped)
	return entries, skipped, nil
}

// within reports whether p is root or inside it (both cleaned, absolute).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// hashFile returns the hex SHA-256 and size of the file, reporting bytes read
// through step and honoring ctx.
func hashFile(ctx context.Context, path string, step func(int64)) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		k, rerr := f.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			n += int64(k)
			step(int64(k))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Totals returns the file count and total size of entries.
func Totals(entries []store.ManifestEntry) (files int, bytes int64) {
	for _, e := range entries {
		files++
		bytes += e.Size
	}
	return files, bytes
}

// Remote is a directory on the cluster reached over SFTP.
type Remote struct {
	// SSHCommand is the command rclone runs instead of its built-in SSH
	// client, e.g. [<Labmill executable>, "ssh-exec", "files"]. rclone appends
	// "-s sftp" for the SFTP subsystem. rclone never runs any other remote
	// command (remote hashing and shell detection are disabled); after a
	// download, Download itself runs `sha256sum` through the same command.
	SSHCommand []string
	// Dir is the remote directory: absolute, or relative to the login home.
	Dir string
	// Check, if not nil, is called once at the start of Upload and Download,
	// before rclone is set up or any process is spawned. A non-nil error is
	// returned to the caller verbatim and nothing else happens. The app layer
	// uses it to fail fast with a clear "not logged in" message (a probe that
	// connects through the helper without ever asking the user anything).
	Check func(ctx context.Context) error
	// OnReplace, if not nil, is called by Upload for each manifest file that
	// already exists on the cluster with a different size, just before the
	// new copy replaces it (rel is the manifest path, oldSize the size that
	// is being overwritten). It runs after the transfer started, so the
	// question "what will be replaced" is answered without any connection
	// before the user approved.
	OnReplace func(rel string, oldSize int64)
	// OnVerify, if not nil, is called by Download with the result of the
	// SHA-256 comparison after the files arrived (see Verification).
	OnVerify func(Verification)
}

// Progress reports transfer progress.
type Progress struct {
	Bytes, TotalBytes int64 // bytes transferred so far (never decreases) and in total
	Files, TotalFiles int
	// Checking is how many bytes of the current file the pre-send safety
	// re-check has read (0 when not checking). It is separate from Bytes so
	// progress never jumps ahead of what was actually sent.
	Checking int64
	// Current is the path of the file being processed (relative, as the
	// manifest or the download list names it); "" before the first file.
	Current string
}

// Limits on what Download will enumerate on the remote side, so a symlink
// loop or a huge tree cannot run away.
const maxRemoteDepth = 64

// maxRemoteFiles is a variable only so tests can lower it.
var maxRemoteFiles = 100_000

// Upload copies the manifest's files from root into dst.Dir, creating
// directories as needed, and returns an error if any file fails or ends up
// with a different size than in the manifest. Only manifest entries are sent:
// never anything else under root. Before each file is sent it is re-checked
// against the manifest (still a regular file reached without symlinks, same
// size, same SHA-256); otherwise Upload stops with "file changed since you
// approved the upload". If dst.Dir exists on the cluster as a file, Upload
// fails. Re-running after an interruption skips files already present with
// the same size and modification time (rclone's default comparison).
// progress, if not nil, is called at least once per second while data moves
// and once at the end. dst.OnReplace reports files that overwrite a
// different-sized remote file.
func Upload(ctx context.Context, root string, entries []store.ManifestEntry, dst Remote, progress func(Progress)) error {
	if err := runCheck(ctx, dst); err != nil {
		return err
	}
	ctx, done, err := rcloneContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	fsrc, err := newLocalFs(ctx, absRoot)
	if err != nil {
		return err
	}
	fdst, err := newSFTPFs(ctx, dst)
	if err != nil {
		return err
	}
	defer shutdownFs(fdst)
	return uploadEntries(ctx, absRoot, fsrc, fdst, entries, dst.OnReplace, progress)
}

// uploadEntries is the common part of Upload and UploadS3: each manifest
// entry is re-checked locally, copied from fsrc (the local root) to fdst and
// verified by size. (rclone's CopyFile also compares hashes after a copy when
// source and destination share one; see UploadS3.) ctx must come from
// rcloneContext.
func uploadEntries(ctx context.Context, absRoot string, fsrc, fdst rfs.Fs, entries []store.ManifestEntry, onReplace func(rel string, oldSize int64), progress func(Progress)) error {
	total := Progress{TotalFiles: len(entries)}
	for _, e := range entries {
		total.TotalBytes += e.Size
	}
	stop := pollProgress(ctx, total, progress)
	defer stop.finish(nil)

	for i, e := range entries {
		if err := cleanRel(e.Path); err != nil {
			return fmt.Errorf("manifest entry %q: %w", e.Path, err)
		}
		stop.setCurrent(e.Path)
		err := recheckLocal(ctx, absRoot, e, stop.setHashing)
		stop.setHashing(0)
		if err != nil {
			return err
		}
		if onReplace != nil {
			if old, err := fdst.NewObject(ctx, e.Path); err == nil && old.Size() != e.Size {
				onReplace(e.Path, old.Size())
			}
		}
		if err := operations.CopyFile(ctx, fdst, fsrc, e.Path, e.Path); err != nil {
			return fmt.Errorf("upload %s: %w", e.Path, err)
		}
		o, err := fdst.NewObject(ctx, e.Path)
		if err != nil {
			return fmt.Errorf("verify %s: %w", e.Path, err)
		}
		if o.Size() != e.Size {
			return fmt.Errorf("verify %s: remote size %d, expected %d", e.Path, o.Size(), e.Size)
		}
		stop.setFiles(i + 1)
	}
	// final report: everything verified
	p := total
	p.Bytes, p.Files = total.TotalBytes, total.TotalFiles
	stop.finish(&p)
	return nil
}

// recheckLocal confirms the file for e is still what the user approved: no
// symlink on the way below root, a regular file of the manifest's size and
// SHA-256. (A swap between this check and rclone opening the file is still
// possible; the post-upload size check and the user-owned directory bound
// that for the MVP.)
func recheckLocal(ctx context.Context, root string, e store.ManifestEntry, hashed func(int64)) error {
	changed := fmt.Errorf("file changed since you approved the upload: %s", e.Path)
	cur := root
	parts := strings.Split(path.Clean(e.Path), "/")
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("%w (%v)", changed, err)
		}
		if i < len(parts)-1 {
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				return changed
			}
			continue
		}
		if !fi.Mode().IsRegular() || fi.Size() != e.Size {
			return changed
		}
	}
	var soFar int64
	sum, n, err := hashFile(ctx, cur, func(k int64) {
		soFar += k
		if hashed != nil {
			hashed(soFar)
		}
	})
	if err != nil {
		return fmt.Errorf("%w (%v)", changed, err)
	}
	if n != e.Size || !strings.EqualFold(sum, e.SHA256) {
		return changed
	}
	return nil
}

func runCheck(ctx context.Context, r Remote) error {
	if r.Check == nil {
		return nil
	}
	return r.Check(ctx)
}

// Download copies the remote files or directories named by paths (relative
// to src.Dir) into localDir, preserving their relative layout, and returns
// the local paths of the files (including ones already identical locally).
// A path that would land outside localDir is an error. Download never
// overwrites local data: if a local file exists where a download would land
// and differs from the remote file (size or modification time, 1 s
// tolerance) or is not a regular file, Download copies nothing and returns
// an error listing those files; files identical to the remote one (same size
// and mtime) are skipped. Remote enumeration is capped at 100000 files and
// a depth of 64 (symlink loops); beyond that Download fails. Each written
// file's size is verified against the remote size, and then its SHA-256
// against `sha256sum` run on the cluster through src.SSHCommand (batched; see
// Verification). A file whose sum differs is renamed to <name>.mismatch
// (never deleted) and left out of the result, but is not an error: OnVerify
// reports it and the caller warns the user. When the check can't run (no
// sha256sum, a host that allows no commands, a timeout) the files stay
// verified by size only and OnVerify says why. The progress callback gets a
// "checking SHA-256" Current while it runs.
func Download(ctx context.Context, src Remote, paths []string, localDir string, progress func(Progress)) (written []string, err error) {
	rels, err := downloadRels(paths)
	if err != nil {
		return nil, err
	}
	if err := runCheck(ctx, src); err != nil {
		return nil, err
	}
	ctx, done, err := rcloneContext(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return nil, err
	}
	fdst, err := newLocalFs(ctx, localDir)
	if err != nil {
		return nil, err
	}
	fsrc, err := newSFTPFs(ctx, src)
	if err != nil {
		return nil, err
	}
	defer shutdownFs(fsrc)

	subFs := func(rel string) (rfs.Fs, error) {
		return newSFTPFs(ctx, Remote{SSHCommand: src.SSHCommand, Dir: path.Join(src.Dir, rel)})
	}
	written, err = downloadFiles(ctx, fsrc, subFs, fdst, rels, localDir, progress)
	if err != nil || len(written) == 0 {
		return written, err
	}
	return verifyWritten(ctx, src, written, localDir, progress)
}

// downloadRels cleans and validates the user's relative download paths.
func downloadRels(paths []string) ([]string, error) {
	rels := make([]string, len(paths))
	for i, p := range paths {
		p = path.Clean(filepath.ToSlash(p))
		if err := cleanRel(p); err != nil {
			return nil, fmt.Errorf("download %q: %w", paths[i], err)
		}
		rels[i] = p
	}
	for i, rel := range rels {
		if err := checkLocalName(runtime.GOOS, rel); err != nil {
			return nil, fmt.Errorf("download %q: %w", paths[i], err)
		}
	}
	return rels, nil
}

// downloadFiles is the common part of Download and DownloadS3: it resolves
// rels against fsrc (subFs opens the Fs for a directory below it), refuses to
// replace local files, copies into fdst and verifies sizes. ctx must come from
// rcloneContext.
func downloadFiles(ctx context.Context, fsrc rfs.Fs, subFs func(rel string) (rfs.Fs, error), fdst rfs.Fs, rels []string, localDir string, progress func(Progress)) (written []string, err error) {
	// Resolve every path to a list of files first (gives totals, the
	// conflict check and the result list).
	type item struct {
		rel   string
		size  int64
		mtime time.Time
	}
	var items []item
	seen := map[string]bool{}
	found := 0 // objects seen, counting duplicates
	add := func(rel string, o rfs.Object) error {
		found++
		if err := cleanRel(rel); err != nil {
			return fmt.Errorf("remote file %q: %w", rel, err)
		}
		if err := checkLocalName(runtime.GOOS, rel); err != nil {
			return fmt.Errorf("remote file %q: %w", rel, err)
		}
		if len(items) >= maxRemoteFiles {
			return fmt.Errorf("more than %d remote files; download a smaller selection", maxRemoteFiles)
		}
		if !seen[rel] {
			seen[rel] = true
			items = append(items, item{rel, o.Size(), o.ModTime(ctx)})
		}
		return nil
	}
	for _, rel := range rels {
		o, err := fsrc.NewObject(ctx, rel)
		if err == nil {
			if err := add(rel, o); err != nil {
				return nil, err
			}
			continue
		}
		// Only "not a file" answers lead to treating it as a directory;
		// any other failure (permissions, connection) is an error.
		if !errors.Is(err, rfs.ErrorObjectNotFound) && !errors.Is(err, rfs.ErrorIsDir) {
			return nil, fmt.Errorf("download %s: %w", rel, err)
		}
		foundBefore := found
		sub, err := subFs(rel)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", rel, err)
		}
		defer shutdownFs(sub)
		lctx, cancel := context.WithCancel(ctx)
		var addErr error
		var mu sync.Mutex // rclone may call back from several goroutines
		err = walk.ListR(lctx, sub, "", false, maxRemoteDepth+1, walk.ListAll, func(entries rfs.DirEntries) error {
			mu.Lock()
			defer mu.Unlock()
			if addErr != nil {
				return addErr
			}
			for _, e := range entries {
				if strings.Count(e.Remote(), "/")+1 > maxRemoteDepth {
					addErr = fmt.Errorf("remote tree deeper than %d levels (symlink loop?)", maxRemoteDepth)
					return addErr
				}
				if o, ok := e.(rfs.Object); ok {
					if addErr = add(path.Join(rel, o.Remote()), o); addErr != nil {
						return addErr
					}
				}
			}
			return nil
		})
		cancel()
		mu.Lock()
		if addErr != nil {
			err = addErr
		}
		mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", rel, err)
		}
		if found == foundBefore {
			return nil, fmt.Errorf("download %s: nothing found at that path in the job's folder", rel)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })

	// Never replace local data.
	var conflicts []string
	for _, it := range items {
		fi, err := os.Lstat(filepath.Join(localDir, filepath.FromSlash(it.rel)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("download %s: %w", it.rel, err)
		case !fi.Mode().IsRegular() || fi.Size() != it.size || fi.ModTime().Sub(it.mtime).Abs() >= time.Second: // rclone treats exactly 1s as different
			conflicts = append(conflicts, it.rel)
		}
	}
	if len(conflicts) > 0 {
		list := conflicts
		if len(list) > 20 {
			list = append(append([]string(nil), list[:20]...), fmt.Sprintf("... and %d more", len(conflicts)-20))
		}
		return nil, fmt.Errorf("these files already exist in %s and would be replaced: %s", localDir, strings.Join(list, ", "))
	}

	total := Progress{TotalFiles: len(items)}
	for _, it := range items {
		total.TotalBytes += it.size
	}
	stop := pollProgress(ctx, total, progress)
	defer stop.finish(nil)
	for i, it := range items {
		stop.setCurrent(it.rel)
		if err := operations.CopyFile(ctx, fdst, fsrc, it.rel, it.rel); err != nil {
			return written, fmt.Errorf("download %s: %w", it.rel, err)
		}
		local := filepath.Join(localDir, filepath.FromSlash(it.rel))
		fi, err := os.Stat(local)
		if err != nil {
			return written, fmt.Errorf("verify %s: %w", it.rel, err)
		}
		if fi.Size() != it.size {
			return written, fmt.Errorf("verify %s: local size %d, remote size %d", it.rel, fi.Size(), it.size)
		}
		written = append(written, local)
		stop.setFiles(i + 1)
	}
	p := total
	p.Bytes, p.Files = total.TotalBytes, total.TotalFiles
	stop.finish(&p)
	return written, nil
}

// Local-name encodings per GOOS. rclone's local backend writes names with
// encoder.OS, whose value is chosen by build tags in
// lib/encoder/os_{darwin,windows,other}.go, so Labmill (which checks names for
// a goos it is not necessarily built for) mirrors the three values here;
// TestLocalMaskMatchesRclone fails if the one for the current GOOS drifts.
const (
	maskOther  = encoder.Base
	maskDarwin = encoder.Base | encoder.EncodeInvalidUtf8
	maskWin    = encoder.Base | encoder.EncodeWin | encoder.EncodeBackSlash |
		encoder.EncodeCtl | encoder.EncodeRightSpace | encoder.EncodeRightPeriod |
		encoder.EncodeInvalidUtf8
)

// localMask returns rclone's local-backend encoder mask for goos.
func localMask(goos string) encoder.MultiEncoder {
	switch goos {
	case "windows":
		return maskWin
	case "darwin":
		return maskDarwin
	}
	return maskOther
}

// checkLocalName refuses remote names that would be saved on goos under a
// different name than the one shown to the user: a path component is refused
// when rclone's local encoder for goos (localMask) changes it
// (FromStandardName(comp) != comp). That covers, on windows, the characters
// : * ? " < > | \, control characters and a trailing space or dot; on darwin
// and windows invalid UTF-8; and on every OS the quote rune U+201B and, on
// linux/darwin, the control pictures U+2401-U+241F and U+2421, which rclone
// renames. U+FF0F and U+2400 round-trip unchanged and are accepted.
//
// Remote names holding a control character (or a control picture, on linux
// and darwin) never reach the disk under a mapped name on windows, because
// both are refused there, so no two remote names can collide locally.
func checkLocalName(goos, rel string) error {
	enc := localMask(goos)
	for _, comp := range strings.Split(rel, "/") {
		if comp == "." || comp == ".." {
			continue // path syntax, not file names; Download handles them
		}
		if enc.FromStandardName(comp) != comp {
			return fmt.Errorf("the name %q contains a character or ending that the local disk (%s) does not allow, so it would be saved under a different name; rename it on the cluster first", rel, goos)
		}
	}
	return nil
}

// skipShutdown is a variable only so tests can disable shutdownFs: ending the
// ssh-exec process while the test binary runs triggers rclone's own data race
// on exec.Cmd.ProcessState (see leakTB in transfer_test.go).
var skipShutdown bool

// shutdownFs releases an rclone Fs's resources (for sftp: the ssh-exec
// process), so idle helpers don't linger after a transfer.
func shutdownFs(f rfs.Fs) {
	if skipShutdown {
		return
	}
	if sd, ok := f.(rfs.Shutdowner); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sd.Shutdown(ctx)
	}
}

// cleanRel rejects paths that are absolute or climb out via "..".
func cleanRel(p string) error {
	p = filepath.ToSlash(p)
	if p == "" || strings.HasPrefix(p, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return fmt.Errorf("%q is not a relative path", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return fmt.Errorf("%q leaves the target directory", p)
	}
	return nil
}

var rcloneOnce sync.Once

// rcloneContext prepares rclone for in-process use and returns a context
// with its own accounting stats group and a function that removes it.
//
// Isolation, honestly: rclone's package init() has already run by the time
// this code does. It may create an empty ~/.config/rclone directory and it
// has registered every RCLONE_* environment variable as a default for its
// global options. We never read or write a config file (the config path is
// /dev/null and every backend is built from a local configmap), and we
// replace the global options and filter in the returned context with fresh
// defaults, so RCLONE_* variables (dry run, excludes, ...) have no effect.
// Backend options are not taken from the environment either: see
// optionDefaults.
func rcloneContext(ctx context.Context) (context.Context, func(), error) {
	var err error
	rcloneOnce.Do(func() {
		err = config.SetConfigPath(os.DevNull)
		rfs.GetConfig(context.Background()).LogLevel = rfs.LogLevelCritical
	})
	if err != nil {
		return ctx, nil, fmt.Errorf("configuring rclone: %w", err)
	}
	fresh := new(rfs.ConfigInfo)
	if err := configstruct.Set(optionDefaults(rfs.ConfigOptionsInfo, nil), fresh); err != nil {
		return ctx, nil, fmt.Errorf("configuring rclone: %w", err)
	}
	fresh.DeleteMode = rfs.DeleteModeDefault
	fresh.LogLevel = rfs.LogLevelCritical
	ctx, ci := rfs.AddConfig(ctx)
	*ci = *fresh
	flt, err := filter.NewFilter(&filter.Options{
		MinAge: rfs.DurationOff, MaxAge: rfs.DurationOff,
		MinSize: rfs.SizeSuffix(-1), MaxSize: rfs.SizeSuffix(-1),
	})
	if err != nil {
		return ctx, nil, fmt.Errorf("configuring rclone: %w", err)
	}
	ctx = filter.ReplaceConfig(ctx, flt)
	group := fmt.Sprintf("labmill-%d", groupSeq.Add(1))
	ctx = accounting.WithStatsGroup(ctx, group)
	accounting.StatsGroup(ctx, group)
	return ctx, func() { removeStatsGroup(ctx, group) }, nil
}

// removeStatsGroup drops a stats group (rclone exports this only as the rc
// call core/stats-delete).
func removeStatsGroup(ctx context.Context, group string) {
	if call := rc.Calls.Get("core/stats-delete"); call != nil {
		call.Fn(ctx, rc.Params{"group": group})
	}
}

var groupSeq atomic.Int64

// optionDefaults returns a complete configmap for opts: every option's
// default value (not the environment's) overlaid with overrides. Passing
// this to a backend's NewFs directly keeps RCLONE_*, RCLONE_CONFIG_* and
// config-file values out, and the map is local, so a backend's own
// m.Set never reaches a global store.
func optionDefaults(opts rfs.Options, overrides map[string]string) configmap.Simple {
	m := configmap.Simple{}
	for _, o := range opts {
		o.Value = nil // o is a copy; use the default
		m[o.Name] = o.String()
	}
	for k, v := range overrides {
		m[k] = v
	}
	return m
}

// newBackendFs builds a Fs of the named rclone backend directly through its
// NewFs, with optionDefaults plus overrides.
func newBackendFs(ctx context.Context, backend, root string, overrides map[string]string) (rfs.Fs, error) {
	info, err := rfs.Find(backend)
	if err != nil {
		return nil, err
	}
	return info.NewFs(ctx, "labmill", root, optionDefaults(info.Options, overrides))
}

func newLocalFs(ctx context.Context, dir string) (rfs.Fs, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	f, err := newBackendFs(ctx, "local", abs, nil)
	if err != nil {
		return nil, fmt.Errorf("local %s: %w", dir, err)
	}
	return f, nil
}

// newSFTPFs builds rclone's sftp backend in-process, with no config file
// and no environment: see optionDefaults. With "ssh" set rclone runs that
// command (adding "-s sftp") instead of its built-in SSH client; the list is
// encoded with rclone's own SpaceSepList rules. rclone runs no commands on
// the cluster: shell_type "none" skips shell detection and about, and remote
// hashing is disabled, so files are compared by size and modification time.
// A directory that is a file on the cluster is an error.
func newSFTPFs(ctx context.Context, r Remote) (rfs.Fs, error) {
	return newSFTPFsWith(ctx, r, nil)
}

// newSFTPFsWith is newSFTPFs with more rclone options (for example
// skip_links for a listing that must not follow symbolic links).
func newSFTPFsWith(ctx context.Context, r Remote, extra map[string]string) (rfs.Fs, error) {
	if len(r.SSHCommand) == 0 {
		return nil, fmt.Errorf("transfer: no ssh command")
	}
	opts := map[string]string{
		"ssh":               rfs.SpaceSepList(r.SSHCommand).String(),
		"disable_hashcheck": "true",
		"shell_type":        "none",
		"known_hosts_file":  "none", // host keys are checked by the helper, not rclone
	}
	for k, v := range extra {
		opts[k] = v
	}
	f, err := newBackendFs(ctx, "sftp", r.Dir, opts)
	if errors.Is(err, rfs.ErrorIsFile) {
		return nil, fmt.Errorf("the destination %s is a file on the cluster", r.Dir)
	}
	if err != nil {
		return nil, fmt.Errorf("remote %s: %w", r.Dir, err)
	}
	return f, nil
}

// poller reports progress from rclone's stats at 1 Hz until finish is called.
type poller struct {
	ctx    context.Context
	total  Progress
	report func(Progress)
	mu     sync.Mutex
	files  int
	hashed int64  // bytes hashed so far in the pre-send re-check of the current file
	cur    string // the file being processed
	quit   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

func pollProgress(ctx context.Context, total Progress, report func(Progress)) *poller {
	pl := &poller{ctx: ctx, total: total, report: report, quit: make(chan struct{})}
	if report == nil {
		return pl
	}
	pl.wg.Add(1)
	go func() {
		defer pl.wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-pl.quit:
				return
			case <-t.C:
				pl.emit(nil)
			}
		}
	}()
	return pl
}

// emit reports final, or a snapshot of the stats when final is nil.
func (pl *poller) emit(final *Progress) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if final != nil {
		pl.report(*final)
		return
	}
	p := pl.total
	p.Bytes = min(accounting.Stats(pl.ctx).GetBytes(), p.TotalBytes)
	p.Checking = pl.hashed
	p.Files = pl.files
	p.Current = pl.cur
	pl.report(p)
}

// setHashing records how many bytes of the current file the pre-send re-hash
// has read, so the next report moves instead of looking stuck. 0 clears it.
func (pl *poller) setHashing(n int64) {
	pl.mu.Lock()
	pl.hashed = n
	pl.mu.Unlock()
}

func (pl *poller) setCurrent(rel string) {
	pl.mu.Lock()
	pl.cur = rel
	pl.mu.Unlock()
}

func (pl *poller) setFiles(n int) {
	pl.mu.Lock()
	pl.files = n
	pl.mu.Unlock()
}

// finish stops the poller and makes the last report: final if not nil, else a
// snapshot (used on failure). Only the first call has an effect.
func (pl *poller) finish(final *Progress) {
	pl.once.Do(func() {
		close(pl.quit)
		pl.wg.Wait()
		if pl.report != nil {
			pl.emit(final)
		}
	})
}
