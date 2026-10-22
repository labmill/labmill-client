package skill

import (
	"fmt"
	"io/fs"
	"os"
)

// rootFS is an fs.FS over an os.Root whose Open refuses anything but regular
// files and directories. It opens with O_NONBLOCK (Unix) and checks the type
// of the opened file (not of a path looked at earlier), so a SKILL.md or
// reference that is, or becomes, a FIFO or device is refused without blocking.
type rootFS struct {
	root  *os.Root
	inner fs.FS // root.FS(), for ReadDir and Stat
}

func newRootFS(root *os.Root) *rootFS { return &rootFS{root: root, inner: root.FS()} }

func (r *rootFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	f, err := r.root.OpenFile(name, os.O_RDONLY|openFlags, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() && !fi.IsDir() {
		f.Close()
		return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("not a regular file")}
	}
	return f, nil
}

func (r *rootFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(r.inner, name) }

func (r *rootFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(r.inner, name) }
