package transfer

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	rfs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/walk"
)

// RemoteFile is one file below a remote folder: its slash-separated path
// relative to that folder and its size.
type RemoteFile struct {
	Path string
	Size int64
}

// List returns every file below src.Dir over SFTP, sorted by path (M60.1:
// the automatic download sizes the results before it fetches them). It
// reads nothing but names and sizes and is capped like Download (100000
// files, depth 64; beyond that List fails). Symbolic links (to files or
// folders) are not listed and not followed, so a link to data outside the
// folder never shows up.
func List(ctx context.Context, src Remote) ([]RemoteFile, error) {
	if err := runCheck(ctx, src); err != nil {
		return nil, err
	}
	ctx, done, err := rcloneContext(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	fsrc, err := newSFTPFsWith(ctx, src, map[string]string{"skip_links": "true"})
	if err != nil {
		return nil, err
	}
	defer shutdownFs(fsrc)
	return listFiles(ctx, fsrc)
}

// ListS3 is List for s3://Bucket/Prefix/Dir/; nothing outside that prefix
// is listed.
func ListS3(ctx context.Context, src S3Remote) ([]RemoteFile, error) {
	rootPath, err := src.rootPath()
	if err != nil {
		return nil, err
	}
	ctx, done, err := rcloneContext(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	fsrc, err := newS3Fs(ctx, src, rootPath)
	if err != nil {
		return nil, src.explain(err)
	}
	defer shutdownFs(fsrc)
	files, err := listFiles(ctx, fsrc)
	if err != nil {
		return nil, src.explain(err)
	}
	return files, nil
}

// listFiles walks fsrc (an Fs from rcloneContext's context) and returns its
// files sorted by path.
func listFiles(ctx context.Context, fsrc rfs.Fs) ([]RemoteFile, error) {
	var files []RemoteFile
	var mu sync.Mutex // rclone may call back from several goroutines
	var walkErr error
	err := walk.ListR(ctx, fsrc, "", false, maxRemoteDepth+1, walk.ListAll, func(entries rfs.DirEntries) error {
		mu.Lock()
		defer mu.Unlock()
		if walkErr != nil {
			return walkErr
		}
		for _, e := range entries {
			if strings.Count(e.Remote(), "/")+1 > maxRemoteDepth {
				walkErr = fmt.Errorf("remote tree deeper than %d levels (symlink loop?)", maxRemoteDepth)
				return walkErr
			}
			o, ok := e.(rfs.Object)
			if !ok {
				continue
			}
			if len(files) >= maxRemoteFiles {
				walkErr = fmt.Errorf("more than %d remote files", maxRemoteFiles)
				return walkErr
			}
			files = append(files, RemoteFile{Path: o.Remote(), Size: o.Size()})
		}
		return nil
	})
	mu.Lock()
	defer mu.Unlock()
	if walkErr != nil {
		err = walkErr
	}
	if err != nil {
		return nil, fmt.Errorf("list the job's folder: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
