package transfer

import (
	"context"
	"path"
	"time"

	rfs "github.com/rclone/rclone/fs"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// presentOnRemote returns the manifest paths whose copy on the cluster already
// has the manifest's size and SHA-256 (the cluster's own `sha256sum`, run
// through dst.SSHCommand like the download check). Those files need no
// transfer, whatever their modification time. Only files that exist with the
// manifest's size are asked about, so a first upload runs no command. When the
// check cannot run (no sha256sum, a timeout), it returns what it found so far:
// the rest is copied as usual. differ holds the files that exist with the
// manifest's size but other content: rclone's default size and time check could
// call them equal, so the caller copies them regardless of time. size holds the
// remote size of every manifest file that exists there. A file in sent (sent and
// verified by an earlier attempt of the same transfer) whose remote size and
// modification time are unchanged counts as present without a hash. Only files whose
// content was verified (copied by Upload, or equal by hash) enter sent; the mtime
// check does not see a rewrite that keeps size and mtime. Only a cancelled
// ctx is an error.
func presentOnRemote(ctx context.Context, dst Remote, fdst rfs.Fs, entries []store.ManifestEntry, step func(cur string), sent SentFiles) (present, differ map[string]bool, size map[string]int64, err error) {
	present, differ, size = map[string]bool{}, map[string]bool{}, map[string]int64{}
	var cand []store.ManifestEntry
	mtimes := map[string]time.Time{}
	for _, e := range entries {
		o, err := fdst.NewObject(ctx, e.Path)
		if err != nil {
			continue
		}
		size[e.Path] = o.Size()
		if o.Size() != e.Size {
			continue
		}
		if mt, ok := sent[e.Path]; ok && o.ModTime(ctx).Equal(mt) {
			present[e.Path] = true // sent and verified by an earlier attempt, untouched since: no hash
			continue
		}
		cand = append(cand, e)
		mtimes[e.Path] = o.ModTime(ctx)
	}
	for i := 0; i < len(cand); {
		n, qlen, nbytes := 0, 0, int64(0)
		for i+n < len(cand) && n < verifyBatchFiles {
			ql := len(backend.Quote([]string{path.Join(dst.Dir, cand[i+n].Path)})) + 1
			if n > 0 && (qlen+ql > verifyBatchQuote || nbytes+cand[i+n].Size > verifyBatchBytes) {
				break
			}
			qlen += ql
			nbytes += cand[i+n].Size
			n++
		}
		batch := cand[i : i+n]
		if step != nil {
			step(batch[0].Path)
		}
		rels := make([]string, n)
		for k, e := range batch {
			rels[k] = e.Path
		}
		sums, reason, err := remoteSums(ctx, dst, rels, nbytes)
		if err != nil {
			return nil, nil, nil, err
		}
		if reason != "" {
			return present, differ, size, nil
		}
		for _, e := range batch {
			switch sum, ok := sums[path.Join(dst.Dir, e.Path)]; {
			case ok && sum == e.SHA256:
				present[e.Path] = true
				if sent != nil {
					sent[e.Path] = mtimes[e.Path] // hash-verified: a retry need not hash it again
				}
			case ok:
				differ[e.Path] = true
			}
		}
		i += n
	}
	return present, differ, size, nil
}

// SentFiles records, per manifest path, the remote modification time of a file
// that Upload copied and verified. A caller that retries an upload passes the
// same map in Remote.Sent, so a retry does not hash these files again. A nil map
// is allowed: nothing is recorded.
type SentFiles map[string]time.Time
