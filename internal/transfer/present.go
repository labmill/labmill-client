package transfer

import (
	"context"
	"path"

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
// call them equal, so the caller copies them regardless of time. Only a cancelled
// ctx is an error.
func presentOnRemote(ctx context.Context, dst Remote, fdst rfs.Fs, entries []store.ManifestEntry, step func(cur string)) (present, differ map[string]bool, err error) {
	var cand []store.ManifestEntry
	for _, e := range entries {
		if o, err := fdst.NewObject(ctx, e.Path); err == nil && o.Size() == e.Size {
			cand = append(cand, e)
		}
	}
	present, differ = map[string]bool{}, map[string]bool{}
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
			return nil, nil, err
		}
		if reason != "" {
			return present, differ, nil
		}
		for _, e := range batch {
			switch sum, ok := sums[path.Join(dst.Dir, e.Path)]; {
			case ok && sum == e.SHA256:
				present[e.Path] = true
			case ok:
				differ[e.Path] = true
			}
		}
		i += n
	}
	return present, differ, nil
}
