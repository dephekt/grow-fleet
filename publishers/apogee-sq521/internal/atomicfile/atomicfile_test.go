// The two fsync calls in WriteFile — the file before the rename and the parent
// directory after it — are deliberately untested, because they have no
// observable effect that a test can reach: fsync changes only what survives a
// power cut, and both a syncing and a non-syncing implementation behave
// identically to every syscall a test can make. Removing either one passes this
// entire suite. They are held in place by review and by the comments on
// WriteFile explaining what each one is for, rather than by a test that would
// only be asserting that the call compiles.
//
// Everything else about the write sequence — the rename, the temp file's
// location and cleanup, and the explicit chmod — is observable, and is pinned
// below.
package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileCreates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")

	if err := WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("contents = %q, want %q", got, "hello")
	}
}

func TestWriteFileReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")

	if err := WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// A shorter second write over a longer first would leave trailing bytes if
	// this truncated in place instead of renaming.
	if string(got) != "second" {
		t.Errorf("contents = %q, want %q", got, "second")
	}
}

func TestWriteFileShorterThanPrevious(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")

	if err := WriteFile(path, []byte("a long initial value"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := WriteFile(path, []byte("short"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "short" {
		t.Errorf("contents = %q, want %q — leftover bytes from the longer write", got, "short")
	}
}

// TestWriteFileMode checks the explicit Chmod. os.CreateTemp always creates
// 0600, so without it every file this package writes would come out
// owner-only regardless of the perm argument.
func TestWriteFileMode(t *testing.T) {
	for _, perm := range []os.FileMode{0o644, 0o600, 0o666} {
		t.Run(perm.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state")
			if err := WriteFile(path, []byte("x"), perm); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if got := fi.Mode().Perm(); got != perm {
				t.Errorf("mode = %#o, want %#o", got, perm)
			}
		})
	}
}

// TestWriteFileNewInode confirms the rename: each write lands a fresh inode
// rather than truncating the existing one. That is what makes a concurrent
// reader see either the whole old file or the whole new one.
func TestWriteFileNewInode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if err := WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if os.SameFile(before, after) {
		t.Error("the second write reused the inode, so it was not atomic")
	}
}

func TestWriteFileLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	for i := 0; i < 5; i++ {
		if err := WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "state" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just [state]", names)
	}
}

// TestWriteFileCleansUpOnFailure drives the error path: the rename cannot
// succeed because a directory occupies the target name, and the temporary file
// must not be left behind.
func TestWriteFileCleansUpOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	err := WriteFile(path, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("WriteFile succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "atomicfile:") {
		t.Errorf("error %q is not attributed to this package", err)
	}

	entries, err2 := os.ReadDir(dir)
	if err2 != nil {
		t.Fatalf("ReadDir: %v", err2)
	}
	if len(entries) != 1 || entries[0].Name() != "state" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v after a failed write, want just [state]; "+
			"a temporary file was leaked", names)
	}
}

// TestWriteFileTempStaysInTargetDirectory pins where the temporary file is
// created. rename(2) cannot cross filesystems, so a temp file in the process
// temp directory would make every write fail with EXDEV the moment
// $STATE_DIRECTORY is a different mount from /tmp — which on this daemon's Pi
// it is, /tmp being a tmpfs.
//
// That failure cannot be reproduced here without mount privileges, so the
// property is pinned the other way round: with TMPDIR pointing at a directory
// that does not exist, a write that used os.CreateTemp("", ...) could not even
// create its temporary, while one that uses the target's own directory is
// unaffected.
func TestWriteFileTempStaysInTargetDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))

	// Confirm the environment override actually reaches os.CreateTemp, so this
	// test cannot pass because TMPDIR was ignored.
	if f, err := os.CreateTemp("", "probe*"); err == nil {
		name := f.Name()
		f.Close()
		os.Remove(name)
		t.Skipf("TMPDIR is not honoured on this platform (created %s)", name)
	}

	path := filepath.Join(dir, "state")
	if err := WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v — the temporary is not being created in the "+
			"target's own directory", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "x" {
		t.Errorf("contents = %q, want %q", got, "x")
	}
}

func TestWriteFileMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent", "state")
	if err := WriteFile(path, []byte("x"), 0o644); err == nil {
		t.Error("WriteFile into a missing directory succeeded, want an error")
	}
}

func TestWriteFileEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("contents = %q, want empty", got)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	if err := WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file still present after Remove (stat error = %v)", err)
	}
}

// TestRemoveMissingIsNotAnError keeps "clear this" idempotent for callers.
func TestRemoveMissingIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-created")
	if err := Remove(path); err != nil {
		t.Errorf("Remove of a missing file = %v, want nil", err)
	}
}

func TestRemoveReportsRealErrors(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory cannot be removed with unlink/rmdir semantics,
	// which produces an error that is not ErrNotExist.
	target := filepath.Join(dir, "occupied")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := Remove(target); err == nil {
		t.Error("Remove of a non-empty directory succeeded, want an error")
	}
}

func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Errorf("SyncDir: %v", err)
	}
	if err := SyncDir(filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Error("SyncDir on a missing directory succeeded, want an error")
	}
}
