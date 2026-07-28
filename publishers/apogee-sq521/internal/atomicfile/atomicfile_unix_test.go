//go:build unix

package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWriteFileModeSurvivesUmask pins the reason WriteFile calls Chmod rather
// than passing a mode to a create call: the umask masks bits off a create, but
// not off a chmod. Under a 077 umask a created-with-0644 file would come out
// 0600, and the state file would silently become owner-only.
//
// syscall.Umask is process-wide, so this must not run in parallel with anything
// that creates files.
func TestWriteFileModeSurvivesUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })

	path := filepath.Join(t.TempDir(), "state")
	if err := WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %#o under a 077 umask, want 0644", got)
	}
}
