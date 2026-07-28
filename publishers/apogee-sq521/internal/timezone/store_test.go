package timezone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// A unit that granted no StateDirectory must not leave a file behind.
//
// filepath.Join("", "timezone") is the relative path "timezone", so an
// unguarded Store would write the override into the process's working
// directory — "/" for a systemd unit, and whatever the operator happened to be
// standing in for a hand-run one. Persistence is disabled instead, matching
// dli.StatePath's treatment of the same empty value.
func TestAStoreWithNoDirectoryPersistsNothing(t *testing.T) {
	// A directory the test owns, so a regression writes here and is caught
	// rather than scattering files across the repository.
	t.Chdir(t.TempDir())

	s := NewStore("")
	if s.Enabled() {
		t.Error("a Store with no directory reports itself enabled")
	}
	if got := s.Path(); got != "" {
		t.Errorf("Path() = %q, want \"\"; a relative path would land in the working directory", got)
	}

	if err := s.Save(londonTZ); err != nil {
		t.Errorf("Save = %v, want nil", err)
	}
	if got, err := s.Load(); err != nil || got != "" {
		t.Errorf("Load = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := s.Clear(); err != nil {
		t.Errorf("Clear = %v, want nil", err)
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the working directory: %v", err)
	}
	for _, e := range entries {
		t.Errorf("a disabled Store created %q in the working directory", e.Name())
	}
}

// The Manager must run on a disabled Store without warning about writes that
// were never going to happen.
func TestManagerOnADisabledStore(t *testing.T) {
	t.Chdir(t.TempDir())

	m := NewManager(NewStore(""), time.UTC, nil)
	m.Start()
	if got := m.State(); got != "" {
		t.Errorf("State after Start = %q, want \"\"", got)
	}

	if got := m.Apply(londonTZ); got != londonTZ {
		t.Errorf("Apply = %q, want the accepted bytes echoed", got)
	}
	if got := m.Location().String(); got != londonTZ {
		t.Errorf("Location = %q, want the applied zone", got)
	}
	// The no-op short-circuit must still hold: a re-push of the same value is
	// acknowledged without pretending a write failed.
	if got := m.Apply(londonTZ); got != londonTZ {
		t.Errorf("re-Apply = %q, want the same echo", got)
	}
}
