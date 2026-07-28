package dli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeState puts a state file on disk the way a previous run would have.
func writeState(t *testing.T, path string, s state) {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), stateFileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readState(t *testing.T, path string) state {
	t.Helper()
	s, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState(%s): %v", path, err)
	}
	return s
}

// ---------------------------------------------------------------------------
// resume
// ---------------------------------------------------------------------------

// The case the file exists for: a systemctl restart at 14:00 must not zero a
// half-finished day. Zeroing produces a saw-tooth that any downstream last()
// or max() reads as a real collapse in light.
func TestRestartMidDayResumesRatherThanZeroes(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	morning := time.Date(2026, 7, 28, 13, 59, 55, 0, loc)

	writeState(t, path, state{
		Version:            stateVersion,
		Zone:               "UTC",
		Day:                "2026-07-28",
		UmolSeconds:        21_000_000,
		PeakPPFD:           940,
		LitSeconds:         28_800,
		GapSeconds:         120,
		LastSampleUnixNano: morning.UnixNano(),
		LastPPFD:           700,
		YesterdayDay:       "2026-07-27",
		YesterdayDLI:       44.91,
	})

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path},
		time.Date(2026, 7, 28, 14, 0, 0, 0, loc))

	got := h.a.Snapshot()
	near(t, "DLI", got.DLI, 21.0)
	exact(t, "PeakPPFD", got.PeakPPFD, 940)
	near(t, "PhotoperiodHours", got.PhotoperiodHours, 8)
	near(t, "GapMinutes", got.GapMinutes, 2)
	near(t, "Yesterday", got.Yesterday, 44.91)
	if h.a.Day() != "2026-07-28" {
		t.Errorf("Day() = %q, want 2026-07-28", h.a.Day())
	}
	if !strings.Contains(h.log(), "resumed partial day") {
		t.Errorf("the resume should be logged, got:\n%s", h.log())
	}
}

// The resumed prev is not special-cased: it feeds the ordinary max-gap guard,
// and because a restart plus a serial settle plus the first poll always
// exceeds maxGap, it credits lastPpfd x maxGap and restarts.
func TestResumedPrevFeedsTheOrdinaryMaxGapGuard(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	last := time.Date(2026, 7, 28, 14, 0, 0, 0, loc)

	writeState(t, path, state{
		Version:            stateVersion,
		Zone:               "UTC",
		Day:                "2026-07-28",
		UmolSeconds:        10_000_000,
		LastSampleUnixNano: last.UnixNano(),
		LastPPFD:           900,
	})

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, last.Add(5*time.Minute))
	// The daemon comes back 5 min 3 s later — well past maxGap.
	h.a.Add(last.Add(5*time.Minute+3*time.Second), 850)

	got := h.a.Snapshot()
	// 10 000 000 + 900 * 60 = 10 054 000 µmol·s, and not one µmol of the
	// 303-second silence beyond the hold.
	near(t, "DLI", got.DLI, 10_054_000/umolPerMol)
	near(t, "GapMinutes", got.GapMinutes, (303-60)/60.0)
	// A naive trapezoid across the restart would have credited
	// (900+850)/2 * 303 = 265 125 µmol·s instead of 54 000.
	if got.DLI > 10.1 {
		t.Errorf("DLI = %v; the restart interval was integrated rather than held", got.DLI)
	}
}

// A state file with no prev (which is what a rollover writes) resumes the
// totals without inventing a sample to integrate from.
func TestResumeWithoutAPrevSampleIntegratesNothingUntilTheSecondSample(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	noon := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)

	writeState(t, path, state{
		Version: stateVersion, Zone: "UTC", Day: "2026-07-28", UmolSeconds: 5_000_000,
	})

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, noon)
	h.a.Add(noon, 800)
	near(t, "DLI after one sample", h.a.Snapshot().DLI, 5.0)
	h.a.Add(noon.Add(10*time.Second), 800)
	near(t, "DLI after two", h.a.Snapshot().DLI, (5_000_000+8000)/umolPerMol)
}

// A zone change invalidates the basis the day label was computed on, so the
// day is not resumed — and the operator gets both strings.
func TestZoneMismatchRefusesToResume(t *testing.T) {
	loc := time.UTC
	path := tempState(t)

	writeState(t, path, state{
		Version:      stateVersion,
		Zone:         "GMT0BST,M3.5.0/1,M10.5.0",
		Day:          "2026-07-28",
		UmolSeconds:  21_000_000,
		PeakPPFD:     940,
		YesterdayDLI: 44.91,
	})

	h := newHarness(t, Config{Location: loc, Zone: "UTC0", StatePath: path},
		time.Date(2026, 7, 28, 14, 0, 0, 0, loc))

	got := h.a.Snapshot()
	exact(t, "DLI", got.DLI, 0)
	exact(t, "PeakPPFD", got.PeakPPFD, 0)
	// Yesterday goes too: it was completed on a basis this process no longer
	// uses, so it is not this zone's yesterday.
	exact(t, "Yesterday", got.Yesterday, 0)

	logs := h.log()
	if !strings.Contains(logs, "level=WARN") {
		t.Errorf("a refused resume must be a WARN, got:\n%s", logs)
	}
	for _, want := range []string{"GMT0BST,M3.5.0/1,M10.5.0", "UTC0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log should name %q so an operator can see both bases, got:\n%s", want, logs)
		}
	}
}

func TestResumeAcrossDays(t *testing.T) {
	loc := time.UTC
	today := time.Date(2026, 7, 28, 9, 0, 0, 0, loc)

	for _, tc := range []struct {
		name          string
		storedDay     string
		wantYesterday float64
		wantLog       string
	}{
		{
			name:          "yesterday's file becomes today's yesterday",
			storedDay:     "2026-07-27",
			wantYesterday: 33.5,
			wantLog:       "carried yesterday forward",
		},
		{
			// The stored yesterdayDli by then describes two days before the
			// stored day, so publishing it would be worse than publishing
			// nothing.
			name:          "a multi-day gap clears yesterday",
			storedDay:     "2026-07-20",
			wantYesterday: 0,
			wantLog:       "older than yesterday",
		},
		{
			name:          "a file dated ahead of the clock clears yesterday",
			storedDay:     "2026-08-02",
			wantYesterday: 0,
			wantLog:       "dated ahead of the clock",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tempState(t)
			writeState(t, path, state{
				Version:            stateVersion,
				Zone:               "UTC",
				Day:                tc.storedDay,
				UmolSeconds:        33_500_000,
				PeakPPFD:           940,
				LitSeconds:         43_200,
				GapSeconds:         600,
				LastSampleUnixNano: today.Add(-12 * time.Hour).UnixNano(),
				LastPPFD:           500,
				YesterdayDay:       "1999-12-31",
				YesterdayDLI:       11.11,
			})

			h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, today)

			got := h.a.Snapshot()
			exact(t, "DLI", got.DLI, 0)
			exact(t, "PeakPPFD", got.PeakPPFD, 0)
			exact(t, "PhotoperiodHours", got.PhotoperiodHours, 0)
			exact(t, "GapMinutes", got.GapMinutes, 0)
			near(t, "Yesterday", got.Yesterday, tc.wantYesterday)
			if h.a.Day() != "2026-07-28" {
				t.Errorf("Day() = %q, want 2026-07-28", h.a.Day())
			}

			// The stale prev must not survive either: integrating from a
			// 12-hour-old sample would credit a bounded hold onto a day it
			// never touched.
			h.a.Add(today, 900)
			exact(t, "DLI after the first sample", h.a.Snapshot().DLI, 0)
			exact(t, "GapMinutes after the first sample", h.a.Snapshot().GapMinutes, 0)

			if !strings.Contains(h.log(), tc.wantLog) {
				t.Errorf("want a log line containing %q, got:\n%s", tc.wantLog, h.log())
			}
		})
	}
}

// yesterdayDay dates yesterdayDli, and resume checks the two agree before
// republishing the total. This is the field's second job — the first is
// telling a human reading the file what that number is the DLI *of* — and
// without the check a hand-edited or half-written file could put an arbitrary
// day's total under a label meaning "the day before today".
func TestResumeDropsAYesterdayThatIsNotDatedTheDayBefore(t *testing.T) {
	loc := time.UTC
	noon := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)

	for _, tc := range []struct {
		name          string
		yesterdayDay  string
		yesterdayDLI  float64
		wantYesterday float64
		wantWarn      bool
	}{
		{
			name:         "dated the day before: kept",
			yesterdayDay: "2026-07-27", yesterdayDLI: 33.5, wantYesterday: 33.5,
		},
		{
			name:         "dated two days back: dropped",
			yesterdayDay: "2026-07-26", yesterdayDLI: 33.5, wantYesterday: 0, wantWarn: true,
		},
		{
			name:         "undated: dropped",
			yesterdayDay: "", yesterdayDLI: 33.5, wantYesterday: 0, wantWarn: true,
		},
		{
			// Nothing to publish and nothing to complain about, which is the
			// shape a rollover after a multi-day gap leaves behind.
			name:         "no yesterday at all: silent",
			yesterdayDay: "", yesterdayDLI: 0, wantYesterday: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tempState(t)
			writeState(t, path, state{
				Version: stateVersion, Zone: "UTC", Day: "2026-07-28",
				UmolSeconds:  12_000_000,
				YesterdayDay: tc.yesterdayDay, YesterdayDLI: tc.yesterdayDLI,
			})

			h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, noon)

			near(t, "Yesterday", h.a.Snapshot().Yesterday, tc.wantYesterday)
			// The day itself is still resumed either way; only the stale
			// yesterday is dropped.
			near(t, "DLI", h.a.Snapshot().DLI, 12.0)

			warned := strings.Contains(h.log(), "not dated the day before")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log:\n%s", warned, tc.wantWarn, h.log())
			}
		})
	}
}

// Every way the file can be unusable starts the day from zero at INFO, because
// a first run is normal and a sensor that refuses to publish is worse than one
// that lost this morning.
func TestUnusableStateFilesStartFromZeroAtInfo(t *testing.T) {
	for _, tc := range []struct {
		name string
		// write returns the path to hand to New; "" for "no file at all".
		write func(t *testing.T, dir string) string
	}{
		{
			name:  "missing",
			write: func(t *testing.T, dir string) string { return filepath.Join(dir, StateFileName) },
		},
		{
			name: "truncated JSON",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, []byte(`{"version":1,"day":"2026-`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "empty file",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "a future schema version",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, []byte(`{"version":2,"zone":"UTC","day":"2026-07-28","umolSeconds":1}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "version zero, i.e. the field absent",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, []byte(`{"zone":"UTC","day":"2026-07-28","umolSeconds":1}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "no day label",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, []byte(`{"version":1,"zone":"UTC","umolSeconds":1}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "a negative accumulator",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, []byte(`{"version":1,"zone":"UTC","day":"2026-07-28","umolSeconds":-5}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "an overflowing literal that parses as +Inf",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, []byte(`{"version":1,"zone":"UTC","day":"2026-07-28","umolSeconds":1e400}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			// The one accumulator the NaN/Inf/negative sweep over the float
			// fields cannot cover. lastSampleUnixNano resumes straight into
			// prev, where it feeds the ordinary max-gap guard: a sample dated
			// before the epoch books five decades of uncredited time into
			// gapSeconds and publishes a dli_gap of ~3e7 minutes for a day that
			// is minutes old.
			name: "a last-sample timestamp before the epoch",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.WriteFile(p, []byte(`{"version":1,"zone":"UTC","day":"2026-07-28","umolSeconds":1,"lastSampleUnixNano":-1000000000000000000}`), 0o644); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "a directory where the file should be",
			write: func(t *testing.T, dir string) string {
				p := filepath.Join(dir, StateFileName)
				if err := os.Mkdir(p, 0o755); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.write(t, t.TempDir())
			h := newHarness(t, Config{Location: time.UTC, Zone: "UTC", StatePath: path},
				time.Date(2026, 7, 28, 14, 0, 0, 0, time.UTC))

			got := h.a.Snapshot()
			exact(t, "DLI", got.DLI, 0)
			exact(t, "Yesterday", got.Yesterday, 0)
			if h.a.Day() != "2026-07-28" {
				t.Errorf("Day() = %q, want today", h.a.Day())
			}

			logs := h.log()
			if !strings.Contains(logs, "no usable state file") {
				t.Errorf("want the reason logged, got:\n%s", logs)
			}
			if strings.Contains(logs, "level=WARN") || strings.Contains(logs, "level=ERROR") {
				t.Errorf("a first run is normal and must not be a WARN, got:\n%s", logs)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// writing
// ---------------------------------------------------------------------------

// Persisting every poll would be 8640 SD-card writes a day, so writes coalesce
// to one per minute of sample time. The mark advances on the first sample and
// then every 60 s.
func TestPersistCoalescesToOncePerMinute(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, start)

	for i := range 31 {
		ts := start.Add(time.Duration(i) * 10 * time.Second)
		h.a.Add(ts, 500)

		want := start.Add(time.Duration(i/6) * 60 * time.Second)
		if got := readState(t, path).LastSampleUnixNano; got != want.UnixNano() {
			t.Fatalf("after sample %d (%s) the file records %s, want %s",
				i, ts.Format("15:04:05"),
				time.Unix(0, got).UTC().Format("15:04:05"), want.Format("15:04:05"))
		}
	}
}

// A clock that steps backwards must not stall the writes for however long the
// step was: the cadence is keyed off sample timestamps, so a naive
// "t - lastPersist >= interval" would go quiet until the clock caught up.
func TestPersistAfterABackwardsClockStep(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, start)
	h.a.Add(start, 500)                     // first sample: writes
	h.a.Add(start.Add(10*time.Second), 500) // 5000 µmol·s, inside the window: no write
	if got := readState(t, path).UmolSeconds; got != 0 {
		t.Fatalf("test setup drifted: the coalesced write should not have happened yet (%v)", got)
	}

	// NTP steps the clock back half an hour. The write has to land anyway, and
	// the accumulated 5000 µmol·s is how we can see that it did.
	h.a.Add(start.Add(-30*time.Minute), 500)
	s := readState(t, path)
	near(t, "persisted umolSeconds after the step", s.UmolSeconds, 5000)

	// The persisted prev is the *latest* sample, not the stepped-back one.
	// Rewinding it would re-integrate [11:30, 12:00:10] on the next forward
	// sample — and would do it across a restart too, since this is the file a
	// restart resumes from.
	if got, want := s.LastSampleUnixNano, start.Add(10*time.Second).UnixNano(); got != want {
		t.Errorf("the file records a prev at %s, want the un-rewound %s",
			time.Unix(0, got).UTC().Format("15:04:05"), time.Unix(0, want).UTC().Format("15:04:05"))
	}
}

func TestFlushWritesImmediately(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, start)
	h.a.Add(start, 500)
	h.a.Add(start.Add(10*time.Second), 500) // inside the coalescing window

	before := readState(t, path)
	if before.UmolSeconds != 0 {
		t.Fatalf("test setup drifted: the coalesced write should not have happened yet (%v)", before.UmolSeconds)
	}

	if err := h.a.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	after := readState(t, path)
	near(t, "persisted umolSeconds", after.UmolSeconds, 5000)
	if after.LastSampleUnixNano != start.Add(10*time.Second).UnixNano() {
		t.Error("Flush should persist the current prev, not the last coalesced one")
	}
}

// A rollover drops prev, and the file has to say so.
//
// The trap is that a resumed state carries the previous run's
// lastSampleUnixNano in the same struct the accumulator writes back, so a
// rollover that forgot to clear it would persist a sample from the day before
// midnight. The next restart would then resume that sample against the new day
// and credit it a bounded hold — light that fell yesterday, filed today.
func TestRolloverPersistsNoPrevEvenAfterAResume(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	late := time.Date(2026, 7, 28, 23, 59, 55, 0, loc)

	writeState(t, path, state{
		Version: stateVersion, Zone: "UTC", Day: "2026-07-28",
		UmolSeconds:        30_000_000,
		LastSampleUnixNano: late.UnixNano(),
		LastPPFD:           700,
	})

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, late)
	// A long silence over midnight: the rollover runs with prev dropped, and
	// the new day is seeded from the current sample afterwards.
	morning := time.Date(2026, 7, 29, 6, 0, 0, 0, loc)
	h.a.Add(morning, 0)

	if got := readState(t, path).LastSampleUnixNano; got != 0 {
		t.Errorf("the rollover persisted a prev at %s; it should have cleared it",
			time.Unix(0, got).UTC().Format(time.RFC3339))
	}

	// And the consequence, end to end: a restart from that file must not
	// credit yesterday's 700 µmol against the new day.
	restart := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, morning.Add(5*time.Minute))
	restart.a.Add(morning.Add(5*time.Minute), 0)
	exact(t, "DLI after restarting into the new day", restart.a.Snapshot().DLI, 0)
}

func TestPersistedFileIsHumanReadableAndComplete(t *testing.T) {
	loc := time.UTC
	path := tempState(t)
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)

	h := newHarness(t, Config{Location: loc, Zone: "GMT0BST,M3.5.0/1,M10.5.0", StatePath: path}, start)
	h.a.Add(start, 800)
	h.a.Add(start.Add(10*time.Second), 900)
	if err := h.a.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Readable without sudo. CreateTemp makes files 0600, so this only holds
	// because write() asks for stateFileMode — and a file a human cannot read
	// defeats the point of writing JSON. The literal is spelled out rather
	// than compared against stateFileMode, which would make the assertion
	// self-referential and unable to notice the constant changing.
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %v, want 0644", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(raw)
	if !strings.HasSuffix(text, "}\n") {
		t.Errorf("the file should be indented JSON ending in a newline, got:\n%s", text)
	}
	if !strings.Contains(text, "\n  \"day\"") {
		t.Errorf("the file should be indented for a human, got:\n%s", text)
	}
	// Every field the schema promises, by its documented JSON name.
	for _, key := range []string{
		"version", "zone", "day", "umolSeconds", "peakPpfd", "litSeconds",
		"gapSeconds", "lastSampleUnixNano", "lastPpfd", "yesterdayDay", "yesterdayDli",
	} {
		if !strings.Contains(text, `"`+key+`"`) {
			t.Errorf("missing field %q in:\n%s", key, text)
		}
	}

	s := readState(t, path)
	if s.Zone != "GMT0BST,M3.5.0/1,M10.5.0" {
		t.Errorf("zone = %q, want the POSIX string the day was computed under", s.Zone)
	}
	// The raw accumulator, not a rounded DLI: persisting the published number
	// would re-quantise it on every restart.
	near(t, "umolSeconds", s.UmolSeconds, 8500)
	exact(t, "lastPpfd", s.LastPPFD, 900)
	exact(t, "peakPpfd", s.PeakPPFD, 900)
}

// A whole day's state has to round-trip so that a restart at any point resumes
// exactly where it left off.
func TestStateRoundTripsThroughARestart(t *testing.T) {
	loc := mustLoad(t, "Europe/London")
	path := tempState(t)
	const zone = "GMT0BST,M3.5.0/1,M10.5.0"

	start := time.Date(2026, 7, 28, 5, 0, 0, 0, loc)
	first := newHarness(t, Config{Location: loc, Zone: zone, StatePath: path}, start)
	var last time.Time
	for i := range 600 {
		last = start.Add(time.Duration(i) * 10 * time.Second)
		first.a.Add(last, 400+float64(i%50))
	}
	if err := first.a.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	want := first.a.Snapshot()

	second := newHarness(t, Config{Location: loc, Zone: zone, StatePath: path}, last.Add(time.Minute))
	if got := second.a.Snapshot(); got != want {
		t.Errorf("after a restart the reading is %+v, want %+v", got, want)
	}
}

func TestPersistenceDisabledIsANoOp(t *testing.T) {
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Location: time.UTC, StatePath: ""}, start)

	h.a.Add(start, 500)
	h.a.Add(start.Add(10*time.Second), 500)
	if err := h.a.Flush(); err != nil {
		t.Errorf("Flush with persistence disabled should succeed silently, got %v", err)
	}
	near(t, "DLI", h.a.Snapshot().DLI, 5000/umolPerMol)
}

// A filesystem that cannot be written to is a degraded state, not a fatal one:
// the daemon keeps measuring and complains once a minute rather than once a
// sample.
func TestUnwritableStateDoesNotStopIntegration(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent does not exist, so CreateTemp fails every time.
	path := filepath.Join(dir, "gone", StateFileName)
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	h := newHarness(t, Config{Location: time.UTC, StatePath: path}, start)
	for i := range 13 {
		h.a.Add(start.Add(time.Duration(i)*10*time.Second), 600)
	}

	near(t, "DLI", h.a.Snapshot().DLI, 600*120/umolPerMol)
	if got := strings.Count(h.log(), "could not persist state"); got != 3 {
		t.Errorf("want 3 warnings over 120 s of samples (one per coalescing window), got %d:\n%s", got, h.log())
	}
}

// ---------------------------------------------------------------------------
// atomic write
// ---------------------------------------------------------------------------

// Temp-file debris from an interrupted write must be inert: loadState only
// ever opens the real path, and a leftover is neither read nor mistaken for
// the state.
func TestStrayTempFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	path := StatePath(dir)
	loc := time.UTC

	writeState(t, path, state{
		Version: stateVersion, Zone: "UTC", Day: "2026-07-28", UmolSeconds: 7_000_000,
	})
	// What a power cut between CreateTemp and Rename leaves behind, holding
	// half a record. The name follows atomicfile.WriteFile's template.
	debris := filepath.Join(dir, "."+StateFileName+".tmp2544019678")
	if err := os.WriteFile(debris, []byte(`{"version":1,"day":"1970-01-01","umolSec`), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path},
		time.Date(2026, 7, 28, 14, 0, 0, 0, loc))

	near(t, "DLI", h.a.Snapshot().DLI, 7.0)
	if _, err := os.Stat(debris); err != nil {
		t.Errorf("the stray file should be left alone for a human to find: %v", err)
	}
}

// A successful write leaves exactly one file behind.
func TestAtomicWriteLeavesNoDebris(t *testing.T) {
	dir := t.TempDir()
	path := StatePath(dir)
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	h := newHarness(t, Config{Location: time.UTC, Zone: "UTC", StatePath: path}, start)
	for i := range 20 {
		h.a.Add(start.Add(time.Duration(i)*60*time.Second), 600)
	}
	if err := h.a.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != StateFileName {
		t.Errorf("directory holds %v, want just [%s]", names, StateFileName)
	}
}

func TestStatePath(t *testing.T) {
	if got := StatePath(""); got != "" {
		t.Errorf("StatePath(\"\") = %q, want \"\" so an absent StateDirectory disables persistence", got)
	}
	if got := StatePath("/var/lib/apogee-sq521"); got != "/var/lib/apogee-sq521/"+StateFileName {
		t.Errorf("StatePath = %q", got)
	}
}
