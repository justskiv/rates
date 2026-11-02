// Package publish writes files atomically under a directory that cannot
// be escaped. Readers (Caddy, git, Pages) only ever see a complete old or
// a complete new file.
package publish

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// TempPrefix starts the name of every temporary file.
const TempPrefix = ".tmp-"

const (
	_fileMode = 0o644
	_dirMode  = 0o755
)

// Dir writes files atomically under a root that cannot be escaped (os.Root).
type Dir struct {
	fs          fsys
	precompress bool
}

// Open creates path if needed and opens it as a root. With precompress
// every Write also maintains a deterministic name.gz sidecar.
func Open(dir string, precompress bool) (*Dir, error) {
	if err := os.MkdirAll(dir, _dirMode); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Dir{fs: rootFS{root: root}, precompress: precompress}, nil
}

// Close releases the root.
func (d *Dir) Close() error {
	return d.fs.Close()
}

// FS returns a read-only view of the directory.
func (d *Dir) FS() fs.FS {
	return d.fs.FS()
}

// ReadFile reads name relative to the root.
func (d *Dir) ReadFile(name string) ([]byte, error) {
	return d.fs.ReadFile(name)
}

// Write replaces name when data differs from what is on disk; it reports
// whether anything changed. With precompress the .gz sidecar is written first
// and the plain file last: the plain file is the commit point. An unchanged
// plain file still gets its sidecar repaired if the two disagree.
func (d *Dir) Write(name string, data []byte) (changed bool, err error) {
	old, err := d.fs.ReadFile(name)
	switch {
	case err == nil && bytes.Equal(old, data):
		return false, d.healSidecar(name, data)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	if err := d.writeSidecar(name, data); err != nil {
		return false, err
	}
	if err := d.replace(name, data); err != nil {
		return false, err
	}
	return true, nil
}

// WriteOnce creates name via link(2) and leaves an existing file alone: the
// file system, not discipline, keeps it immutable. created is false when
// the file already existed.
func (d *Dir) WriteOnce(name string, data []byte) (created bool, err error) {
	existing, err := d.fs.ReadFile(name)
	switch {
	case err == nil:
		return false, d.healSidecar(name, existing)
	case !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	if err := d.writeSidecar(name, data); err != nil {
		return false, err
	}
	err = d.viaTemp(name, data, func(tmp string) error {
		return d.fs.Link(tmp, name)
	})
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

// Sweep removes temporary files left by an interrupted run.
func (d *Dir) Sweep() error {
	var stray []string
	err := fs.WalkDir(d.fs.FS(), ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() && e.Name() == ".git" {
			return fs.SkipDir // a git checkout of the branch: not ours to touch
		}
		if !e.IsDir() && strings.HasPrefix(e.Name(), TempPrefix) {
			stray = append(stray, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, name := range stray {
		if err := d.fs.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (d *Dir) healSidecar(name string, data []byte) error {
	if !d.precompress {
		return nil
	}
	gz, err := d.fs.ReadFile(name + ".gz")
	if err == nil && gunzipEqual(gz, data) {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return d.writeSidecar(name, data)
}

func (d *Dir) writeSidecar(name string, data []byte) error {
	if !d.precompress {
		return nil
	}
	gz, err := Gzip(data)
	if err != nil {
		return err
	}
	return d.replace(name+".gz", gz)
}

// replace writes data to name atomically:
// temp file -> write -> chmod -> fsync -> close -> rename -> fsync dir.
func (d *Dir) replace(name string, data []byte) error {
	return d.viaTemp(name, data, func(tmp string) error {
		return d.fs.Rename(tmp, name)
	})
}

// viaTemp writes data to a fresh temporary file next to name, calls commit
// with its path, and removes whatever is left of the temporary file.
func (d *Dir) viaTemp(name string, data []byte, commit func(tmp string) error) error {
	dir := path.Dir(name)
	if err := d.fs.MkdirAll(dir, _dirMode); err != nil {
		return err
	}
	tmp := path.Join(dir, TempPrefix+randomSuffix())
	f, err := d.fs.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, _fileMode)
	if err != nil {
		return err
	}
	// After a successful rename the temporary name is gone and Remove
	// fails harmlessly; after link(2) or a failure it must go.
	defer func() { _ = d.fs.Remove(tmp) }()

	if err := writeAndSync(f, data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := commit(tmp); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	if err := d.fs.SyncDir(dir); err != nil {
		return fmt.Errorf("sync dir of %s: %w", name, err)
	}
	return nil
}

func writeAndSync(f file, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		// Chmod overrides the umask: published files must be world-readable.
		err = f.Chmod(_fileMode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func randomSuffix() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}

// Gzip compresses deterministically: no name, zero mtime, default level,
// so the same input always gives the same bytes.
func Gzip(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Gunzip decompresses data with a size limit.
func Gunzip(data []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, errors.New("gzip: decompressed size over limit")
	}
	return out, zr.Close()
}

func gunzipEqual(gz, want []byte) bool {
	got, err := Gunzip(gz, int64(len(want)))
	return err == nil && bytes.Equal(got, want)
}
