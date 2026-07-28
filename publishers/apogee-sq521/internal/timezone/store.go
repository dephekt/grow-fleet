package timezone

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/atomicfile"
)

// FileName is the basename of the override file inside the state directory.
const FileName = "timezone"

// Store persists the timezone override in $STATE_DIRECTORY.
//
// The file holds the raw POSIX bytes with no trailing newline and no wrapping
// format. That is deliberate: the value is already a single line of ASCII with
// a documented 63-byte ceiling, so JSON or key=value framing would add a parser
// — and a way for the parser to disagree with the writer — in exchange for
// nothing. A human can read the override with cat and fix it with a text
// editor, and a hand-edit that leaves a stray newline — what `echo ... > file`
// produces — is rejected by Parse's control-byte guard rather than silently
// applied under a zone name with a newline embedded in it.
type Store struct {
	dir string
}

// NewStore returns a Store rooted at dir, which is expected to be the
// STATE_DIRECTORY systemd creates for the unit.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Path is the full path of the override file.
func (s *Store) Path() string {
	return filepath.Join(s.dir, FileName)
}

// Load returns the persisted override, or "" when none is stored.
//
// A missing file is the normal unset case, not an error. Every other failure —
// a permissions problem, a directory where the file should be — is returned, so
// the caller can log the difference between "no override" and "cannot tell".
func (s *Store) Load() (string, error) {
	b, err := os.ReadFile(s.Path())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("timezone: read %s: %w", s.Path(), err)
	}
	return string(b), nil
}

// Save atomically writes posix to the override file with mode 0644.
func (s *Store) Save(posix string) error {
	if err := atomicfile.WriteFile(s.Path(), []byte(posix), 0o644); err != nil {
		return fmt.Errorf("timezone: save override: %w", err)
	}
	return nil
}

// Clear removes the override file. Removing a file that is not there is not an
// error, so callers can clear unconditionally.
func (s *Store) Clear() error {
	if err := atomicfile.Remove(s.Path()); err != nil {
		return fmt.Errorf("timezone: clear override: %w", err)
	}
	return nil
}
