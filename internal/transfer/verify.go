package transfer

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"labmill-client/internal/backend"
)

// Verification is what Download found when it compared the downloaded files'
// SHA-256 with the cluster's (Remote.OnVerify receives it). A mismatch is not
// a Download error: the caller decides what to tell the user.
type Verification struct {
	// Verified is how many files matched (SHA-256 of the local file equals
	// `sha256sum` run on the cluster).
	Verified int
	// Mismatched are the local paths whose content differs from the remote
	// file. Download renamed each to <name>.mismatch (never deleted), so a
	// retry fetches the file again. They are not in Download's result list.
	Mismatched []string
	// Unrenamed are local paths whose content differs but that could not be
	// renamed (so they are still under their normal name). Not in Download's
	// result list either.
	Unrenamed []string
	// Failed are the relative paths `sha256sum` itself reported unreadable
	// (permissions, vanished). They stay in the result list, unverified.
	Failed []string
	// Unverified is how many files were verified by size only because the
	// check could not run (see SizeOnly). Failed files are not counted.
	Unverified int
	// SizeOnly is the reason the check could not run for the Unverified
	// files ("sha256sum not available on <host>", a timeout, ...); "" when
	// every file was checked.
	SizeOnly string
}

const (
	verifyBatchFiles = 200             // files per sha256sum call
	verifyBatchQuote = 16 << 10        // bytes of quoted command per call (Windows CreateProcess, remote MAX_ARG_STRLEN)
	verifyBatchBytes = int64(16) << 30 // file bytes per call, so one call stays within its timeout
)

// verifyBase and verifyRate give a batch's timeout: verifyBase plus the
// batch's bytes at verifyRate bytes per second. Variables so tests can lower
// them.
var (
	verifyBase = 2 * time.Minute
	verifyRate = int64(10 << 20)
)

// verifyTimeout is a variable so tests can give single batches their own budget.
var verifyTimeout = func(n int64) time.Duration {
	return verifyBase + time.Duration(n/verifyRate)*time.Second
}

// Host is the name used in messages about the cluster: the ssh-exec alias, or
// "the cluster".
func (r Remote) Host() string {
	if n := len(r.SSHCommand); n >= 2 && r.SSHCommand[n-2] == "ssh-exec" {
		return r.SSHCommand[n-1]
	}
	return "the cluster"
}

// verifyWritten checks the files Download wrote (local paths below localDir)
// with SHA-256 and returns the ones to report as downloaded (not the
// mismatching ones). Only a cancelled ctx is an error.
func verifyWritten(ctx context.Context, src Remote, written []string, localDir string, progress func(Progress)) ([]string, error) {
	rels := make([]string, len(written))
	sizes := make([]int64, len(written))
	var total int64
	for i, w := range written {
		r, err := filepath.Rel(localDir, w)
		if err != nil {
			return written, fmt.Errorf("verify %s: %w", w, err)
		}
		rels[i] = filepath.ToSlash(r)
		if fi, err := os.Stat(w); err == nil {
			sizes[i] = fi.Size()
		}
		total += sizes[i]
	}
	v, err := verifyDownloads(ctx, src, rels, sizes, localDir, func(done int, cur string) {
		if progress != nil {
			progress(Progress{Bytes: total, TotalBytes: total, Files: len(rels), TotalFiles: len(rels), Current: fmt.Sprintf("checking SHA-256 (%d of %d files): %s", done, len(rels), cur)})
		}
	})
	if src.OnVerify != nil {
		src.OnVerify(v)
	}
	if err != nil {
		return written, fmt.Errorf("verify downloads: %w", err)
	}
	bad := map[string]bool{}
	for _, m := range v.Unrenamed {
		bad[m] = true
	}
	var ok []string
	for _, w := range written {
		// A renamed mismatch no longer exists under its name.
		if _, err := os.Lstat(w); err == nil && !bad[w] {
			ok = append(ok, w)
		}
	}
	return ok, nil
}

// verifyDownloads compares each file in rels (below localDir; sizes are the
// local sizes) with the cluster's `sha256sum` over the same ssh-exec
// connection, in batches. If the check cannot run (no sha256sum, a host that
// allows no commands, a timeout), the files not yet checked count as verified
// by size only and the reason is in the result. Only a cancelled ctx is an
// error.
func verifyDownloads(ctx context.Context, src Remote, rels []string, sizes []int64, localDir string, step func(done int, cur string)) (Verification, error) {
	var v Verification
	for i := 0; i < len(rels); {
		n, qlen, nbytes := 0, 0, int64(0)
		for i+n < len(rels) && n < verifyBatchFiles {
			ql := len(backend.Quote([]string{path.Join(src.Dir, rels[i+n])})) + 1
			if n > 0 && (qlen+ql > verifyBatchQuote || nbytes+sizes[i+n] > verifyBatchBytes) {
				break
			}
			qlen += ql
			nbytes += sizes[i+n]
			n++
		}
		batch := rels[i : i+n]
		if step != nil {
			step(i, batch[0])
		}
		sums, reason, err := remoteSums(ctx, src, batch, nbytes)
		if err != nil {
			return v, err
		}
		if reason != "" {
			v.SizeOnly = reason
			v.Unverified = len(rels) - i
			return v, nil
		}
		for _, rel := range batch {
			want, ok := sums[path.Join(src.Dir, rel)]
			if !ok {
				v.Failed = append(v.Failed, rel)
				continue
			}
			local := filepath.Join(localDir, filepath.FromSlash(rel))
			got, _, err := hashFile(ctx, local, func(int64) {})
			if err != nil {
				if ctx.Err() != nil {
					return v, ctx.Err()
				}
				v.Failed = append(v.Failed, rel)
				continue
			}
			if got == want {
				v.Verified++
				continue
			}
			if to, ok := markMismatch(local); ok {
				v.Mismatched = append(v.Mismatched, to)
			} else {
				v.Unrenamed = append(v.Unrenamed, local)
			}
		}
		i += n
	}
	return v, nil
}

// markMismatch renames a local file to <name>.mismatch (then .mismatch2, ...;
// nothing is overwritten or deleted) and returns the new path. ok is false
// when the rename failed (the file keeps its name).
func markMismatch(local string) (string, bool) {
	for i := 1; ; i++ {
		to := local + ".mismatch"
		if i > 1 {
			to = fmt.Sprintf("%s.mismatch%d", local, i)
		}
		if _, err := os.Lstat(to); err == nil {
			continue
		}
		if err := os.Rename(local, to); err != nil {
			return local, false
		}
		return to, true
	}
}

// sumArgv is the command run on the cluster: `sha256sum -- <paths below
// src.Dir>`.
func sumArgv(src Remote, rels []string) []string {
	argv := []string{"sha256sum", "--"}
	for _, rel := range rels {
		argv = append(argv, path.Join(src.Dir, rel))
	}
	return argv
}

// remoteSums runs the sha256sum command (quoted with backend.Quote) through
// src.SSHCommand and returns the sums keyed by the path as given. fallback is
// non-empty when the check can't run here (no sha256sum, a host that allows
// no commands, timeout, ssh-exec failure): it is the reason, and the files
// are verified by size only. Files sha256sum itself could not read are
// missing from sums. Only a cancelled ctx is an error.
func remoteSums(ctx context.Context, src Remote, rels []string, bytes_ int64) (sums map[string]string, fallback string, err error) {
	timeout := verifyTimeout(bytes_)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(append([]string(nil), src.SSHCommand[1:]...), backend.Quote(sumArgv(src, rels)))
	cmd := exec.CommandContext(cctx, src.SSHCommand[0], args...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	host := src.Host()
	if cctx.Err() != nil {
		return nil, fmt.Sprintf("checking SHA-256 on %s took longer than %v", host, timeout.Round(time.Second)), nil
	}
	sums = parseSums(stdout.Bytes())
	code := 0
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return nil, fmt.Sprintf("couldn't run sha256sum on %s: %v", host, runErr), nil
		}
		code = ee.ExitCode()
	}
	// sha256sum's own diagnostics start with "sha256sum: " (a shell's
	// "command not found" or a restricted shell's refusal start with the
	// shell's name).
	ownDiag := false
	for _, l := range strings.Split(stderr.String(), "\n") {
		if strings.HasPrefix(l, "sha256sum: ") {
			ownDiag = true
		}
	}
	if code == 255 || code == 127 || (len(sums) == 0 && !ownDiag) {
		return nil, fmt.Sprintf("sha256sum not available on %s", host), nil
	}
	return sums, "", nil
}

// parseSums reads sha256sum output: "<64 hex>", a space, a space or "*", then
// the name. GNU escapes a name holding a backslash, newline or carriage
// return as a leading "\" with \\, \n and \r inside.
func parseSums(out []byte) map[string]string {
	sums := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		escaped := strings.HasPrefix(line, `\`)
		line = strings.TrimPrefix(line, `\`)
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			continue
		}
		sum, name := line[:64], line[66:]
		if _, err := hex.DecodeString(sum); err != nil {
			continue
		}
		if escaped {
			name = unescapeName(name)
		}
		sums[name] = strings.ToLower(sum)
	}
	return sums
}

func unescapeName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
				i++
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
