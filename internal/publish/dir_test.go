package publish

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openDir(t *testing.T, precompress bool) (*Dir, string) {
	t.Helper()
	root := t.TempDir()
	d, err := Open(root, precompress)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, root
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWriteReportsChangesOnly(t *testing.T) {
	d, root := openDir(t, false)
	name := "v1/latest.json"

	changed, err := d.Write(name, []byte("one"))
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	path := filepath.Join(root, name)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644", before.Mode().Perm())
	}

	time.Sleep(10 * time.Millisecond)
	changed, err = d.Write(name, []byte("one"))
	if err != nil || changed {
		t.Fatalf("same content: changed=%v err=%v", changed, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("unchanged content touched the mtime: the ETag would change")
	}

	if changed, err = d.Write(name, []byte("two")); err != nil || !changed {
		t.Fatalf("new content: changed=%v err=%v", changed, err)
	}
	if got := readFile(t, path); string(got) != "two" {
		t.Errorf("content %q", got)
	}
}

func TestWritePrecompressAndHeal(t *testing.T) {
	d, root := openDir(t, true)
	name := "v1/latest.json"
	data := []byte(strings.Repeat(`{"rates":1}`, 50))
	if _, err := d.Write(name, data); err != nil {
		t.Fatal(err)
	}
	gzPath := filepath.Join(root, name+".gz")
	first := readFile(t, gzPath)
	got, err := Gunzip(first, 1<<20)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("sidecar does not decompress to the file: %v", err)
	}

	// A stale sidecar next to an unchanged file is repaired.
	if err := os.WriteFile(gzPath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := d.Write(name, data); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if healed := readFile(t, gzPath); !bytes.Equal(healed, first) {
		t.Error("sidecar not repaired, or compression is not deterministic")
	}
}

func TestWriteOnce(t *testing.T) {
	d, root := openDir(t, true)
	name := "v1/history/2026/2026-11-01.json"
	created, err := d.WriteOnce(name, []byte("day"))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	created, err = d.WriteOnce(name, []byte("other"))
	if err != nil || created {
		t.Fatalf("second write: created=%v err=%v", created, err)
	}
	if got := readFile(t, filepath.Join(root, name)); string(got) != "day" {
		t.Errorf("history file overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, name+".gz")); err != nil {
		t.Errorf("no sidecar: %v", err)
	}
	assertNoTemp(t, root)
}

func TestRootCannotBeEscaped(t *testing.T) {
	d, _ := openDir(t, false)
	for _, name := range []string{"../escape.json", "/etc/escape.json", "v1/../../escape.json"} {
		if _, err := d.Write(name, []byte("x")); err == nil {
			t.Errorf("wrote %s outside the root", name)
		}
	}
}

func TestSweep(t *testing.T) {
	d, root := openDir(t, false)
	stray := filepath.Join(root, "v1", TempPrefix+"deadbeef")
	if err := os.MkdirAll(filepath.Dir(stray), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stray, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Sweep(); err != nil {
		t.Fatal(err)
	}
	assertNoTemp(t, root)
}

func assertNoTemp(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(e.Name(), TempPrefix) {
			t.Errorf("temporary file left: %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

var errInjected = errors.New("injected failure")

// faultFS fails the n-th mutating operation (counting from 1), including
// writes, syncs and closes of files it opened.
type faultFS struct {
	fsys
	failAt int
	step   int
}

func (f *faultFS) tick() error {
	f.step++
	if f.step == f.failAt {
		return errInjected
	}
	return nil
}

func (f *faultFS) OpenFile(name string, flag int, perm fs.FileMode) (file, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	fl, err := f.fsys.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &faultFile{file: fl, fs: f}, nil
}

func (f *faultFS) Rename(o, n string) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.fsys.Rename(o, n)
}

func (f *faultFS) Link(o, n string) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.fsys.Link(o, n)
}

func (f *faultFS) MkdirAll(name string, perm fs.FileMode) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.fsys.MkdirAll(name, perm)
}

func (f *faultFS) SyncDir(name string) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.fsys.SyncDir(name)
}

type faultFile struct {
	file
	fs *faultFS
}

func (f *faultFile) Write(b []byte) (int, error) {
	if err := f.fs.tick(); err != nil {
		return 0, err
	}
	return f.file.Write(b)
}

func (f *faultFile) Chmod(m fs.FileMode) error {
	if err := f.fs.tick(); err != nil {
		return err
	}
	return f.file.Chmod(m)
}

func (f *faultFile) Sync() error {
	if err := f.fs.tick(); err != nil {
		return err
	}
	return f.file.Sync()
}

func (f *faultFile) Close() error {
	err := f.fs.tick()
	if cerr := f.file.Close(); err == nil {
		err = cerr
	}
	return err
}

// After a failure at any step the tree holds either the complete old or
// the complete new content, the sidecar never lags behind a new plain
// file, no temporary file is left, and the next run converges.
func TestWriteFailureAtEveryStep(t *testing.T) {
	const name = "v1/latest.json"
	oldData, newData := []byte(`{"v":"old"}`), []byte(`{"v":"new"}`)
	for failAt := 1; ; failAt++ {
		root := t.TempDir()
		clean, err := Open(root, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := clean.Write(name, oldData); err != nil {
			t.Fatal(err)
		}
		faulty := &faultFS{fsys: clean.fs, failAt: failAt}
		d := &Dir{fs: faulty, precompress: true}

		_, err = d.Write(name, newData)
		if err == nil {
			if failAt < 3 {
				t.Fatal("no step failed: the fault injector is broken")
			}
			_ = clean.Close()
			return // every step has been covered
		}
		if !errors.Is(err, errInjected) {
			t.Fatalf("step %d: unexpected error %v", failAt, err)
		}
		plain := readFile(t, filepath.Join(root, name))
		if !bytes.Equal(plain, oldData) && !bytes.Equal(plain, newData) {
			t.Fatalf("step %d: torn file %q", failAt, plain)
		}
		gz := readFile(t, filepath.Join(root, name+".gz"))
		if bytes.Equal(plain, newData) && !gunzipEqual(gz, newData) {
			t.Fatalf("step %d: new plain file with a stale sidecar", failAt)
		}
		assertNoTemp(t, root)

		if _, err := clean.Write(name, newData); err != nil {
			t.Fatalf("step %d: recovery write: %v", failAt, err)
		}
		if gz := readFile(t, filepath.Join(root, name+".gz")); !gunzipEqual(gz, newData) {
			t.Fatalf("step %d: sidecar not converged", failAt)
		}
		_ = clean.Close()
	}
}

func TestWriteOnceFailureAtEveryStep(t *testing.T) {
	const name = "v1/history/2026/2026-11-01.json"
	data := []byte(`{"day":1}`)
	for failAt := 1; ; failAt++ {
		root := t.TempDir()
		clean, err := Open(root, false)
		if err != nil {
			t.Fatal(err)
		}
		d := &Dir{fs: &faultFS{fsys: clean.fs, failAt: failAt}}
		_, err = d.WriteOnce(name, data)
		if err == nil {
			_ = clean.Close()
			return
		}
		if b, rerr := os.ReadFile(filepath.Join(root, name)); rerr == nil && !bytes.Equal(b, data) {
			t.Fatalf("step %d: torn history file %q", failAt, b)
		}
		assertNoTemp(t, root)
		if _, err := clean.WriteOnce(name, data); err != nil {
			t.Fatalf("step %d: recovery: %v", failAt, err)
		}
		if b := readFile(t, filepath.Join(root, name)); !bytes.Equal(b, data) {
			t.Fatalf("step %d: history file %q after recovery", failAt, b)
		}
		_ = clean.Close()
	}
}

func TestSweepLeavesGitAlone(t *testing.T) {
	d, root := openDir(t, false)
	inGit := filepath.Join(root, ".git", "objects", TempPrefix+"x")
	if err := os.MkdirAll(filepath.Dir(inGit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inGit, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inGit); err != nil {
		t.Errorf("sweep touched .git: %v", err)
	}
}
