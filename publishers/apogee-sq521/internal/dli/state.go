package dli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/atomicfile"
)

// StateFileName is the state file's basename inside $STATE_DIRECTORY.
const StateFileName = "dli-state.json"

// stateVersion is the on-disk schema version. A file carrying anything else is
// ignored rather than guessed at: a future version might mean a different unit
// for umolSeconds, and a downgrade that silently reinterpreted it would
// publish a wrong DLI all day without anyone noticing.
const stateVersion = 1

// stateFileMode is deliberately 0o644. The file holds no secret, and an
// operator reading it at 2 a.m. should not need sudo.
const stateFileMode = 0o644

// StatePath returns the state file path inside a systemd StateDirectory, or ""
// when no directory was granted (which disables persistence).
func StatePath(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, StateFileName)
}

// state is both the live day accumulator and the on-disk format.
//
// JSON, not gob or a binary struct, because the audience for this file is a
// human diagnosing a day that looks wrong — the whole value of persistence is
// lost if the only way to inspect it is to write a program.
//
// umolSeconds is the raw accumulator rather than a rounded DLI: persisting the
// published number would re-quantise it on every restart, and the error would
// compound across a day of restarts instead of cancelling.
//
// zone records the POSIX TZ string under which day was computed, so a restart
// after a zone change can detect the mismatch instead of appending today's
// seconds to a day that was measured on a different basis. It is stamped by
// write from the Accumulator's own zone rather than tracked in the live
// struct, so there is one source of truth even after SetLocation.
//
// The same goes for lastSampleUnixNano and lastPpfd below: the live prev is a
// time.Time held beside this struct, and these two fields are only ever filled
// in on the way out.
//
// lastSampleUnixNano and lastPpfd are the integrator's prev. They exist so
// that resume feeds the same max-gap guard every other interval goes through;
// see Accumulator.resume. Zero means "no previous sample": the Unix epoch is
// not a plausible reading, and using a sentinel avoids a second bool that
// could disagree with it.
//
// yesterdayDay dates yesterdayDli. It earns its place twice over: a human
// reading this file cannot otherwise tell whether yesterdayDli is the day
// before day or a leftover from further back, and resume uses the pair's
// agreement as a cheap corruption check before republishing the total. Both
// fields are written together and only together, by rollover and by resume.
type state struct {
	Version            int     `json:"version"`
	Zone               string  `json:"zone"`
	Day                string  `json:"day"`
	UmolSeconds        float64 `json:"umolSeconds"`
	PeakPPFD           float64 `json:"peakPpfd"`
	LitSeconds         float64 `json:"litSeconds"`
	GapSeconds         float64 `json:"gapSeconds"`
	LastSampleUnixNano int64   `json:"lastSampleUnixNano"`
	LastPPFD           float64 `json:"lastPpfd"`
	YesterdayDay       string  `json:"yesterdayDay"`
	YesterdayDLI       float64 `json:"yesterdayDli"`
}

// errNoState reports that the file could not be used. It is always advisory:
// New logs it and starts the day from zero.
var errNoState = errors.New("no usable state")

// loadState reads and validates the state file.
func loadState(path string) (state, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return state{}, fmt.Errorf("%w: %w", errNoState, err)
	}

	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return state{}, fmt.Errorf("%w: %s: %w", errNoState, path, err)
	}
	if s.Version != stateVersion {
		return state{}, fmt.Errorf("%w: %s: version %d, want %d", errNoState, path, s.Version, stateVersion)
	}
	if s.Day == "" {
		return state{}, fmt.Errorf("%w: %s: empty day label", errNoState, path)
	}

	// Every accumulator here is a count of something that only goes up, so a
	// negative or non-finite value is a corrupt or hand-edited file rather
	// than a state we wrote. Letting one through would poison the day it was
	// resumed into — a negative umolSeconds subtracts light that fell, and a
	// NaN makes the whole day NaN and unrecoverable until midnight.
	//
	// (json.Unmarshal rejects the bare tokens NaN and Infinity, which are not
	// JSON, but 1e400 parses as +Inf and a minus sign parses fine.)
	for _, f := range [...]float64{s.UmolSeconds, s.PeakPPFD, s.LitSeconds, s.GapSeconds, s.LastPPFD, s.YesterdayDLI} {
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return state{}, fmt.Errorf("%w: %s: implausible value %v", errNoState, path, f)
		}
	}

	return s, nil
}

// write persists the current state. It must be called with the lock held, and
// is a no-op when no state path was configured.
func (a *Accumulator) write() error {
	if a.statePath == "" {
		return nil
	}

	s := a.st
	s.Version = stateVersion
	s.Zone = a.zone
	if a.prev != nil {
		s.LastSampleUnixNano = a.prev.t.UnixNano()
		s.LastPPFD = a.prev.ppfd
	} else {
		s.LastSampleUnixNano = 0
		s.LastPPFD = 0
	}

	// Indented, with a trailing newline: this file is read by people.
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("dli: marshal state: %w", err)
	}
	// atomicfile.WriteFile is the shared CreateTemp -> Sync -> Rename ->
	// parent-dir Sync helper. Both syncs matter and for different reasons: the
	// first gets the bytes onto the medium so the rename cannot publish an
	// inode full of zeros, and the second gets the *directory entry* down so
	// the rename itself survives a power cut. This runs on a Pi's SD card,
	// where yanking the power mid-write is the normal way the machine stops.
	if err := atomicfile.WriteFile(a.statePath, append(b, '\n'), stateFileMode); err != nil {
		return fmt.Errorf("dli: persist state: %w", err)
	}
	return nil
}
