package publish

import (
	"io"
	"io/fs"
	"os"
)

// fsys is the slice of *os.Root that Dir needs. It exists so tests can
// inject a failure at every step of a write.
type fsys interface {
	OpenFile(name string, flag int, perm fs.FileMode) (file, error)
	ReadFile(name string) ([]byte, error)
	Rename(oldname, newname string) error
	Link(oldname, newname string) error
	Remove(name string) error
	MkdirAll(name string, perm fs.FileMode) error
	SyncDir(name string) error
	FS() fs.FS
	Close() error
}

type file interface {
	io.Writer
	Chmod(mode fs.FileMode) error
	Sync() error
	Close() error
}

// rootFS adapts *os.Root: every path stays inside the root, symlinks
// included, so a crafted name cannot write elsewhere.
type rootFS struct {
	root *os.Root
}

var _ fsys = rootFS{}

func (r rootFS) OpenFile(name string, flag int, perm fs.FileMode) (file, error) {
	return r.root.OpenFile(name, flag, perm)
}

func (r rootFS) ReadFile(name string) ([]byte, error) {
	return r.root.ReadFile(name)
}

func (r rootFS) Rename(oldname, newname string) error {
	return r.root.Rename(oldname, newname)
}

func (r rootFS) Link(oldname, newname string) error {
	return r.root.Link(oldname, newname)
}

func (r rootFS) Remove(name string) error {
	return r.root.Remove(name)
}

func (r rootFS) MkdirAll(name string, perm fs.FileMode) error {
	return r.root.MkdirAll(name, perm)
}

// SyncDir flushes a directory so a completed rename survives a crash.
func (r rootFS) SyncDir(name string) error {
	d, err := r.root.Open(name)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

func (r rootFS) FS() fs.FS {
	return r.root.FS()
}

func (r rootFS) Close() error {
	return r.root.Close()
}
