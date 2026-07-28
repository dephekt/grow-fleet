package timezone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const londonTZ = "GMT0BST,M3.5.0/1,M10.5.0"

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load on an empty directory: %v", err)
	}
	if got != "" {
		t.Errorf("Load on an empty directory = %q, want an empty string", got)
	}

	if err := s.Save(londonTZ); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err = s.Load()
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if got != londonTZ {
		t.Errorf("Load = %q, want %q", got, londonTZ)
	}
}

// TestStoreWritesExactBytes pins the on-disk format: the raw POSIX string, no
// trailing newline, no framing. grow-app compares our retained state topic
// byte-for-byte, and the state topic is fed from this file after a restart, so
// a stray byte here becomes a permanent reconcile loop.
func TestStoreWritesExactBytes(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(londonTZ); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	if string(raw) != londonTZ {
		t.Errorf("file contents = %q, want exactly %q", raw, londonTZ)
	}
}

func TestStoreFileMode(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save(londonTZ); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// os.CreateTemp makes the file 0600; if the explicit Chmod were dropped
	// this would be 0600 and the assertion would catch it.
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %#o, want 0644", got)
	}
}

// TestStoreSaveIsAtomic checks the observable half of atomicity: the write goes
// via a temporary file that is renamed into place, and no temporary is left
// behind. The durability half — the parent-directory fsync — is not observable
// from userspace without cutting power, so it is covered by review rather than
// by a test that would only be pretending.
func TestStoreSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	for _, v := range []string{londonTZ, "UTC0", "CST6CDT,M3.2.0,M11.1.0"} {
		if err := s.Save(v); err != nil {
			t.Fatalf("Save(%q): %v", v, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("state directory holds %d entries (%s), want just %q",
			len(entries), strings.Join(names, ", "), FileName)
	}
	if entries[0].Name() != FileName {
		t.Errorf("entry = %q, want %q", entries[0].Name(), FileName)
	}
}

// TestStoreSaveReplacesInPlace confirms each Save produces a new inode, which
// is what a rename-based write looks like from outside. The manager tests rely
// on this to detect writes without sleeping for a filesystem timestamp.
func TestStoreSaveReplacesInPlace(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Save(londonTZ); err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if err := s.Save("UTC0"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if os.SameFile(before, after) {
		t.Error("Save reused the same inode, so it overwrote in place rather " +
			"than renaming a fresh file over the old one")
	}
}

func TestStoreClear(t *testing.T) {
	s := NewStore(t.TempDir())

	// Clearing when nothing is stored must succeed, so callers can clear
	// unconditionally.
	if err := s.Clear(); err != nil {
		t.Errorf("Clear on an empty directory: %v", err)
	}

	if err := s.Save(londonTZ); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("state file still present after Clear (stat error = %v)", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load after Clear: %v", err)
	}
	if got != "" {
		t.Errorf("Load after Clear = %q, want an empty string", got)
	}
}

// TestStoreLoadReportsRealErrors separates "no override" from "cannot tell".
// Returning "" for both would make an unreadable state directory look exactly
// like a fresh install and silently drop a configured override.
func TestStoreLoadReportsRealErrors(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: readable, present, and not a
	// regular file, so ReadFile fails with something other than ErrNotExist.
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	s := NewStore(dir)
	got, err := s.Load()
	if err == nil {
		t.Fatalf("Load returned %q and no error, want an error", got)
	}
	if got != "" {
		t.Errorf("Load returned %q alongside an error, want an empty string", got)
	}
}

// TestStorePath pins the literal filename as well as the join.
//
// Every other test here refers to FileName symbolically, so none of them would
// notice the constant changing — but the path is operator-facing: it appears in
// the unit's runbook, and renaming it would silently orphan the override an
// already-deployed daemon persisted, reverting the timezone on upgrade with no
// error anywhere.
func TestStorePath(t *testing.T) {
	if FileName != "timezone" {
		t.Errorf("FileName = %q, want %q", FileName, "timezone")
	}
	s := NewStore("/var/lib/apogee")
	if got, want := s.Path(), "/var/lib/apogee/timezone"; got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
