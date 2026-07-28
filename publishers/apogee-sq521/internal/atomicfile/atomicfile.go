// Package atomicfile replaces and removes files durably: a reader — including
// one running after the Pi loses power mid-write — sees either the complete
// old contents or the complete new contents, never a truncated or empty file,
// and never a change that appeared to succeed and then evaporated.
//
// The sequence is the usual write-temp, fsync, rename — plus an fsync of the
// *parent directory*, which is the step most implementations omit and the one
// this daemon actually needs. rename(2) is atomic with respect to concurrent
// readers, but on an SD card the new directory entry lives in the page cache
// until the filesystem commits it. A power cut in that window comes back with
// the rename lost, having reported success to the caller. Syncing the
// directory forces the entry to durable storage before we return.
//
// Both operations here are directory-entry changes, so both need that sync:
// Remove is as much a durability problem as WriteFile. A cleared timezone
// override that silently comes back after a power cut is the same class of bug
// as a saved one that silently does not.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile atomically replaces the file at path with data, creating it with
// perm if it does not exist.
//
// The temporary file is created in the target's own directory because rename(2)
// cannot cross filesystems; a shared temp dir would turn every write into a
// non-atomic copy, or fail outright, depending on how the state directory is
// mounted.
//
// On error nothing at path is disturbed and the temporary file is removed. The
// one exception is a failure to sync the parent directory, which happens after
// the rename: the new contents are in place but their durability is not
// guaranteed, and the error says so rather than pretending otherwise.
func WriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)

	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp file in %s: %w", dir, err)
	}
	tmp := f.Name()

	// Anything that fails before the rename leaves a stray temp file in the
	// state directory, which systemd will happily preserve across restarts
	// forever. Clean up on every error path, including the ones that return
	// from the middle of the sequence below.
	renamed := false
	defer func() {
		if err != nil {
			f.Close()
			if !renamed {
				os.Remove(tmp)
			}
		}
	}()

	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", tmp, err)
	}
	// os.CreateTemp always creates with 0600, so perm has to be applied
	// explicitly. Do it before the rename so the file is never visible at its
	// final path with the wrong mode. Chmod also sidesteps umask, which would
	// otherwise silently strip bits from a mode passed to a create call.
	if err = f.Chmod(perm); err != nil {
		return fmt.Errorf("atomicfile: chmod %s: %w", tmp, err)
	}
	// Sync before the rename, not after: the rename must not be able to reach
	// the disk ahead of the data it points at, or a power cut leaves the
	// directory entry resolving to an inode full of zeroes.
	if err = f.Sync(); err != nil {
		return fmt.Errorf("atomicfile: sync %s: %w", tmp, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("atomicfile: close %s: %w", tmp, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("atomicfile: rename %s to %s: %w", tmp, path, err)
	}
	renamed = true

	if err = SyncDir(dir); err != nil {
		return err
	}
	return nil
}

// Remove deletes path durably, syncing the parent directory so the deletion
// survives a power cut. A path that does not exist is not an error, which lets
// callers treat "clear this" as idempotent.
func Remove(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("atomicfile: remove %s: %w", path, err)
	}
	return SyncDir(filepath.Dir(path))
}

// SyncDir fsyncs a directory so that entries created or removed in it are
// durable. On Linux a read-only descriptor is sufficient for this, which is why
// the directory is merely opened rather than opened for writing — directories
// cannot be opened for writing at all.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("atomicfile: open directory %s: %w", dir, err)
	}
	defer d.Close()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("atomicfile: sync directory %s: %w", dir, err)
	}
	return nil
}
