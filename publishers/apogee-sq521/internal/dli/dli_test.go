package dli

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// harness wires an Accumulator to a recording rollover hook, a captured log and
// a state file in a temp dir. Every test drives simulated time; nothing here
// ever sleeps or reads the wall clock.
type harness struct {
	a      *Accumulator
	logs   *bytes.Buffer
	path   string
	events []Event
	// onEvent, when set, runs inside the rollover hook before the event is
	// recorded. It is how a test asserts on *ordering* against side effects
	// such as the state file.
	onEvent func(Event)
}

// newHarness builds an Accumulator whose "now" at construction is start.
//
// cfg.StatePath is used verbatim, so it is empty — persistence off — unless
// the test asks for a file with tempState. That is not just tidiness: every
// persist fsyncs the file and its parent directory, and a test that drives a
// whole simulated day at a 10 s cadence writes 1440 times. Leaving it off
// keeps the suite in single-digit seconds.
func newHarness(t *testing.T, cfg Config, start time.Time) *harness {
	t.Helper()
	h := &harness{logs: &bytes.Buffer{}, path: cfg.StatePath}

	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	cfg.Now = func() time.Time { return start }
	cfg.Logger = slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg.OnRollover = func(e Event) {
		if h.onEvent != nil {
			h.onEvent(e)
		}
		h.events = append(h.events, e)
	}

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.a = a
	return h
}

// kinds returns the recorded event kinds, for ordering assertions.
func (h *harness) kinds() []EventKind {
	out := make([]EventKind, len(h.events))
	for i, e := range h.events {
		out[i] = e.Kind
	}
	return out
}

// only returns the single recorded event of kind k, failing otherwise.
func (h *harness) only(t *testing.T, k EventKind) Event {
	t.Helper()
	var found []Event
	for _, e := range h.events {
		if e.Kind == k {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %v event, got %d (all: %v)", k, len(found), h.kinds())
	}
	return found[0]
}

func (h *harness) log() string { return h.logs.String() }

// tempState returns a state-file path in a fresh temp dir, for the tests that
// are actually about persistence.
func tempState(t *testing.T) string {
	t.Helper()
	return StatePath(t.TempDir())
}

// close enough for values assembled from float64 sums of exact quantities.
const eps = 1e-9

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > eps*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %.9f, want %.9f (diff %.3g)", what, got, want, got-want)
	}
}

func exact(t *testing.T, what string, got, want float64) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want exactly %v", what, got, want)
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("zone %s unavailable (%v); this test needs the system tzdata", name, err)
	}
	return loc
}

// ---------------------------------------------------------------------------
// integration rule
// ---------------------------------------------------------------------------

// A whole simulated day of a square-wave photoperiod, integrated against a
// value computed by hand:
//
//	06:00:00 -> 18:00:00 at 800 µmol, 0 elsewhere, sampled every 10 s.
//
//	dawn edge  [05:59:50,06:00:00]  (0+800)/2 * 10       =      4 000
//	plateau    [06:00:00,17:59:50]  800 * 43 190         = 34 552 000
//	dusk edge  [17:59:50,18:00:00]  (800+0)/2 * 10       =      4 000
//	                                                       ----------
//	                                                       34 560 000 µmol·s
//	                                                     = 34.56 mol
//
// Photoperiod, under the same linear interior: the dawn edge crosses 20 µmol
// one fortieth of the way in, so 0.975 * 10 s of it counts, and the dusk edge
// likewise; 43 190 + 9.75 + 9.75 = 43 209.5 s.
func TestSquareWaveDayMatchesHandComputedIntegral(t *testing.T) {
	start := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Location: time.UTC}, start)

	dawn := start.Add(6 * time.Hour)
	dusk := start.Add(18 * time.Hour)
	for ts := start; ts.Before(start.Add(24 * time.Hour)); ts = ts.Add(10 * time.Second) {
		ppfd := 0.0
		if !ts.Before(dawn) && ts.Before(dusk) {
			ppfd = 800
		}
		h.a.Add(ts, ppfd)
	}

	got := h.a.Snapshot()
	near(t, "DLI", got.DLI, 34.56)
	near(t, "PhotoperiodHours", got.PhotoperiodHours, 43209.5/3600)
	exact(t, "PeakPPFD", got.PeakPPFD, 800)
	exact(t, "GapMinutes", got.GapMinutes, 0)
	exact(t, "Yesterday", got.Yesterday, 0)

	if len(h.events) != 0 {
		t.Errorf("no rollover expected inside one day, got %v", h.kinds())
	}
}

// The trapezoid rule has to be observably a trapezoid. A square wave cannot
// show this — its two edges cancel, so left-rectangle, right-rectangle and
// trapezoid all give 34.56 mol above — so pin it on a single ramp where the
// three rules disagree by a factor of two.
func TestTrapezoidRuleIsNotARectangle(t *testing.T) {
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Location: time.UTC}, start)

	h.a.Add(start, 0)
	h.a.Add(start.Add(10*time.Second), 1000)

	// trapezoid (0+1000)/2 * 10 = 5000 µmol·s. Left-rectangle would credit 0,
	// right-rectangle 10 000.
	near(t, "DLI", h.a.Snapshot().DLI, 5000/umolPerMol)
}

func TestNegativeReadingsClampAndNonFiniteAreDropped(t *testing.T) {
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	t.Run("darkness clamps to zero rather than a debt", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC}, start)
		h.a.Add(start, -3)
		h.a.Add(start.Add(10*time.Second), -5)
		exact(t, "DLI", h.a.Snapshot().DLI, 0)
		exact(t, "PeakPPFD", h.a.Snapshot().PeakPPFD, 0)
	})

	t.Run("a clamped sample still bounds the trapezoid at zero", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC}, start)
		h.a.Add(start, -4)
		h.a.Add(start.Add(10*time.Second), 100)
		// (0+100)/2 * 10 = 500, not (-4+100)/2 * 10 = 480.
		near(t, "DLI", h.a.Snapshot().DLI, 500/umolPerMol)
	})

	for _, tc := range []struct {
		name string
		ppfd float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	} {
		t.Run(tc.name+" is dropped", func(t *testing.T) {
			h := newHarness(t, Config{Location: time.UTC}, start)
			h.a.Add(start, 500)
			h.a.Add(start.Add(10*time.Second), tc.ppfd)
			h.a.Add(start.Add(20*time.Second), 500)
			// The bad sample never becomes prev, so the interval integrated is
			// the full 20 s at a steady 500.
			near(t, "DLI", h.a.Snapshot().DLI, 10000/umolPerMol)
			if got := h.a.Snapshot().DLI; math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("DLI is not finite: %v", got)
			}
		})
	}
}

func TestLitFraction(t *testing.T) {
	for _, tc := range []struct {
		name        string
		p0, p1      float64
		want        float64
		threshold   float64
		wantExactly bool
	}{
		{name: "both above", p0: 100, p1: 200, threshold: 20, want: 1, wantExactly: true},
		{name: "both below", p0: 1, p1: 19, threshold: 20, want: 0, wantExactly: true},
		{name: "both exactly at threshold counts as lit", p0: 20, p1: 20, threshold: 20, want: 1, wantExactly: true},
		{name: "rising through", p0: 0, p1: 800, threshold: 20, want: 0.975},
		{name: "falling through", p0: 800, p1: 0, threshold: 20, want: 0.975},
		{name: "rising to just above", p0: 0, p1: 40, threshold: 20, want: 0.5},
		{name: "negative threshold counts everything", p0: 0, p1: 0, threshold: -1, want: 1, wantExactly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := litFraction(tc.p0, tc.p1, tc.threshold)
			if tc.wantExactly {
				exact(t, "litFraction", got, tc.want)
				return
			}
			near(t, "litFraction", got, tc.want)
		})
	}
}

// ---------------------------------------------------------------------------
// max-gap guard
// ---------------------------------------------------------------------------

// Each branch of the guard, driven from the same two samples with only the
// interval changing. The threshold case is the point of the table: dt exactly
// equal to maxGap must take the trapezoid branch, and one nanosecond more must
// not.
func TestMaxGapBranches(t *testing.T) {
	const (
		p0 = 600.0
		p1 = 200.0
	)
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	maxGap := 60 * time.Second

	for _, tc := range []struct {
		name        string
		dt          time.Duration
		wantUmolSec float64
		wantGapSec  float64
		wantLitSec  float64
	}{
		{
			name: "well inside the gap: trapezoid",
			dt:   10 * time.Second,
			// (600+200)/2 * 10
			wantUmolSec: 4000,
			wantLitSec:  10,
		},
		{
			name: "exactly maxGap: still a trapezoid",
			dt:   60 * time.Second,
			// (600+200)/2 * 60
			wantUmolSec: 24000,
			wantLitSec:  60,
		},
		{
			name: "one nanosecond over: bounded hold on prev",
			dt:   60*time.Second + 1,
			// 600 * 60, and the remaining 1 ns is gap
			wantUmolSec: 36000,
			wantGapSec:  1e-9,
			wantLitSec:  60,
		},
		{
			name: "a long outage credits the same bounded hold",
			dt:   6 * time.Hour,
			// 600 * 60 regardless of how long the silence lasted
			wantUmolSec: 36000,
			wantGapSec:  6*3600 - 60,
			wantLitSec:  60,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh day per case, and the interval kept inside it.
			h := newHarness(t, Config{Location: time.UTC, MaxGap: maxGap}, start)
			h.a.Add(start, p0)
			h.a.Add(start.Add(tc.dt), p1)

			got := h.a.Snapshot()
			near(t, "DLI", got.DLI, tc.wantUmolSec/umolPerMol)
			near(t, "GapMinutes", got.GapMinutes, tc.wantGapSec/60)
			near(t, "PhotoperiodHours", got.PhotoperiodHours, tc.wantLitSec/3600)
		})
	}
}

// The scenario the guard exists for: 1000 µmol at 19:59:50, the publisher
// dies, and it comes back at 06:00 the next morning reading 0.
//
// The outage is 4 h 0 m 10 s + 6 h = 36 010 s. A naive trapezoid across it
// credits (1000+0)/2 * 36 010 = 1.8005e7 µmol·s = 18.005 mol to the evening —
// roughly a 40% inflation of a normal day, filed against the wrong date. The
// bounded hold credits 1000 * 60 s = 0.06 mol and nothing else.
func TestOvernightOutageDoesNotFabricateLight(t *testing.T) {
	loc := time.UTC
	evening := time.Date(2026, 7, 28, 19, 59, 50, 0, loc)
	morning := time.Date(2026, 7, 29, 6, 0, 0, 0, loc)

	h := newHarness(t, Config{Location: loc}, evening)
	h.a.Add(evening, 1000)
	h.a.Add(morning, 0)

	final := h.only(t, RolloverFinal)
	if final.Day != "2026-07-28" {
		t.Errorf("final event day = %q, want 2026-07-28", final.Day)
	}

	// The measurement, exactly.
	near(t, "completed day DLI", final.Reading.DLI, 60000/umolPerMol)

	// And explicitly not the naive answer, nor anything near it. Both the span
	// and the naive total are asserted exactly, so a change to either endpoint
	// fails here rather than quietly making the doc comment above wrong — an
	// earlier revision of this test said 36 610 s and 18.3 mol, and nothing
	// caught it because the guard was only "somewhere between 18 and 19".
	if got := morning.Sub(evening); got != 36010*time.Second {
		t.Fatalf("test setup drifted: the outage is %v, want 36010s", got)
	}
	naive := (1000 + 0) / 2.0 * morning.Sub(evening).Seconds() / umolPerMol
	near(t, "the naive bridge this guard rejects", naive, 18.005)
	if final.Reading.DLI > 0.1 {
		t.Errorf("completed day DLI = %v mol; the naive bridge would give %v and anything "+
			"above 0.1 means light was fabricated across the outage", final.Reading.DLI, naive)
	}

	// The uncredited span is divided at midnight. 19:59:50 to midnight is
	// 4 h 0 m 10 s; the first 60 s of that is the bounded hold, leaving
	// 14 350 s of the old day unmeasured — and all 6 h of the new day before
	// its first sample.
	near(t, "completed day GapMinutes", final.Reading.GapMinutes, (4*3600+10-60)/60.0)
	near(t, "completed day PhotoperiodHours", final.Reading.PhotoperiodHours, 60/3600.0)

	today := h.a.Snapshot()
	exact(t, "new day DLI", today.DLI, 0)
	near(t, "new day GapMinutes", today.GapMinutes, 360)
	near(t, "new day Yesterday", today.Yesterday, 60000/umolPerMol)
	if h.a.Day() != "2026-07-29" {
		t.Errorf("Day() = %q, want 2026-07-29", h.a.Day())
	}
}

// PhotoperiodHours has to follow the same guard, or a gap makes the two
// disagree about whether time was measured at all.
func TestPhotoperiodFollowsTheMaxGapRule(t *testing.T) {
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	t.Run("dark prev credits no photoperiod across a gap", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC}, start)
		h.a.Add(start, 5) // below the threshold
		h.a.Add(start.Add(2*time.Hour), 900)
		exact(t, "PhotoperiodHours", h.a.Snapshot().PhotoperiodHours, 0)
	})

	t.Run("lit prev credits exactly maxGap across a gap", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC}, start)
		h.a.Add(start, 900)
		h.a.Add(start.Add(2*time.Hour), 5)
		near(t, "PhotoperiodHours", h.a.Snapshot().PhotoperiodHours, 60/3600.0)
	})
}

func TestConfiguredMaxGapIsHonoured(t *testing.T) {
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Location: time.UTC, MaxGap: 20 * time.Second}, start)

	h.a.Add(start, 600)
	h.a.Add(start.Add(30*time.Second), 200)

	// Under the 60 s default this would be a trapezoid worth 12 000 µmol·s.
	near(t, "DLI", h.a.Snapshot().DLI, 600*20/umolPerMol)
	near(t, "GapMinutes", h.a.Snapshot().GapMinutes, 10/60.0)
}

// An out-of-order sample credits nothing, and — the part that is easy to get
// wrong — must not rewind prev either, or the seconds between the rewound prev
// and the next forward sample get integrated twice.
func TestOutOfOrderSamplesWithinADayCreditNothingAndDoNotRewindPrev(t *testing.T) {
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	t.Run("a step back credits nothing and no second is counted twice", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC}, start)

		h.a.Add(start, 500)
		h.a.Add(start.Add(10*time.Second), 500) // 5000 µmol·s
		h.a.Add(start.Add(4*time.Second), 500)  // backwards: nothing
		h.a.Add(start.Add(4*time.Second), 500)  // duplicate: nothing
		h.a.Add(start.Add(14*time.Second), 500) // only +4 s, from the un-rewound prev

		// The real span is 12:00:00 -> 12:00:14 at a steady 500: 7000 µmol·s.
		// Rewinding prev to +4 s would credit 5000 for [4,14] on top of the
		// 5000 already booked for [0,10] and report 10 000.
		near(t, "DLI", h.a.Snapshot().DLI, 7000/umolPerMol)
		exact(t, "GapMinutes", h.a.Snapshot().GapMinutes, 0)
	})

	// Stated as an invariant rather than a number: whatever the sample order,
	// the day cannot exceed a constant PPFD times the real elapsed span. This
	// is the package's first property — never fabricate light — reduced to one
	// comparison, and it is what the rewind violated.
	t.Run("a repeated 30 s stretch is not credited twice", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC}, start)

		h.a.Add(start, 1000)
		h.a.Add(start.Add(30*time.Second), 1000)
		h.a.Add(start.Add(1*time.Second), 1000) // NTP steps back 29 s
		h.a.Add(start.Add(31*time.Second), 1000)

		const span = 31.0 // 12:00:00 -> 12:00:31, the whole real interval
		got := h.a.Snapshot().DLI * umolPerMol
		if got > 1000*span {
			t.Errorf("DLI = %v µmol·s over a real %v s span at a steady 1000 µmol; "+
				"anything above %v means the overlap was integrated twice", got, span, 1000*span)
		}
		near(t, "DLI", got, 1000*span)
	})

	// Erring the other way is the accepted cost, and it is not silent: prev
	// stays put, so nothing is credited until the clock passes it again, and
	// when that takes longer than maxGap the ordinary guard books the
	// remainder as gap where an operator can see it.
	t.Run("the shortfall after a step back shows up in GapMinutes", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC, MaxGap: 60 * time.Second}, start)

		h.a.Add(start, 1000)
		h.a.Add(start.Add(30*time.Second), 1000) // 30 s trapezoid: 30 000
		h.a.Add(start, 1000)                     // NTP steps back 30 s: nothing
		h.a.Add(start.Add(100*time.Second), 1000)

		// The last interval is 70 s measured from the un-rewound prev at +30 s:
		// a 60 s bounded hold, with the leftover 10 s booked as gap.
		near(t, "DLI", h.a.Snapshot().DLI, (30*1000+60*1000)/umolPerMol)
		near(t, "GapMinutes", h.a.Snapshot().GapMinutes, 10/60.0)
	})
}

// ---------------------------------------------------------------------------
// midnight rollover
// ---------------------------------------------------------------------------

// The straddle split, by hand.
//
//	prev  23:59:55  100 µmol
//	cur   00:00:07  220 µmol      (12 s apart, well inside maxGap)
//
// The boundary is 5 s in, so the interpolated PPFD there is
// 100 + 120*(5/12) = 150.
//
//	old day  (100+150)/2 * 5 =  625 µmol·s
//	new day  (150+220)/2 * 7 = 1295 µmol·s
func TestStraddlingIntervalIsSplitAtTheBoundary(t *testing.T) {
	loc := time.UTC
	prev := time.Date(2026, 7, 28, 23, 59, 55, 0, loc)
	cur := time.Date(2026, 7, 29, 0, 0, 7, 0, loc)

	h := newHarness(t, Config{Location: loc}, prev)
	h.a.Add(prev, 100)
	h.a.Add(cur, 220)

	final := h.only(t, RolloverFinal)
	near(t, "old day DLI", final.Reading.DLI, 625/umolPerMol)
	near(t, "old day PhotoperiodHours", final.Reading.PhotoperiodHours, 5/3600.0)
	exact(t, "old day GapMinutes", final.Reading.GapMinutes, 0)

	got := h.a.Snapshot()
	near(t, "new day DLI", got.DLI, 1295/umolPerMol)
	near(t, "new day PhotoperiodHours", got.PhotoperiodHours, 7/3600.0)
	exact(t, "new day GapMinutes", got.GapMinutes, 0)

	// Nothing is lost or duplicated: the two halves are the whole interval.
	near(t, "old + new", final.Reading.DLI+got.DLI, (625+1295)/umolPerMol)
}

// The interpolated boundary sample is synthetic, so it must not set a peak.
// Here it would: the interval rises from 100 to 900 across midnight, and the
// value at the boundary (500) is higher than any PPFD actually measured before
// midnight.
func TestPeakIgnoresTheInterpolatedBoundarySample(t *testing.T) {
	loc := time.UTC
	prev := time.Date(2026, 7, 28, 23, 59, 55, 0, loc)
	cur := time.Date(2026, 7, 29, 0, 0, 5, 0, loc)

	h := newHarness(t, Config{Location: loc}, prev)
	h.a.Add(prev, 100)
	h.a.Add(cur, 900)

	if mid := lerp(prev, 100, cur, 900, cur.Add(-5*time.Second)); mid != 500 {
		t.Fatalf("test setup drifted: boundary PPFD = %v, want 500", mid)
	}
	exact(t, "old day PeakPPFD", h.only(t, RolloverFinal).Reading.PeakPPFD, 100)
	exact(t, "new day PeakPPFD", h.a.Snapshot().PeakPPFD, 900)
}

// The five steps in order, with the state file inspected from inside the hook
// to prove the persist lands between step 3 and step 5.
func TestRolloverOrderIsFinalYesterdayZeroPersistReset(t *testing.T) {
	loc := time.UTC
	prev := time.Date(2026, 7, 28, 23, 59, 55, 0, loc)
	cur := time.Date(2026, 7, 29, 0, 0, 7, 0, loc)

	h := newHarness(t, Config{Location: loc, StatePath: tempState(t)}, prev)

	type observed struct {
		kind           EventKind
		fileDay        string
		fileUmol       float64
		fileYesterday  float64
		fileYesterdayD string
		fileLastSample int64
	}
	var seen []observed
	h.onEvent = func(e Event) {
		s, err := loadState(h.path)
		if err != nil {
			t.Errorf("state file unreadable during %v: %v", e.Kind, err)
			return
		}
		seen = append(seen, observed{e.Kind, s.Day, s.UmolSeconds, s.YesterdayDLI, s.YesterdayDay, s.LastSampleUnixNano})
	}

	h.a.Add(prev, 100) // seeds the day and writes the first state file
	h.a.Add(cur, 220)

	wantKinds := []EventKind{RolloverFinal, RolloverYesterday, RolloverReset}
	if got := h.kinds(); len(got) != 3 || got[0] != wantKinds[0] || got[1] != wantKinds[1] || got[2] != wantKinds[2] {
		t.Fatalf("event order = %v, want %v", got, wantKinds)
	}

	// Step 1: the completed day's full total, with yesterday not yet promoted.
	near(t, "final DLI", h.events[0].Reading.DLI, 625/umolPerMol)
	exact(t, "final Yesterday", h.events[0].Reading.Yesterday, 0)
	if h.events[0].Day != "2026-07-28" {
		t.Errorf("final event day = %q, want 2026-07-28", h.events[0].Day)
	}

	// Step 2: same total, now also as yesterday.
	near(t, "yesterday event DLI", h.events[1].Reading.DLI, 625/umolPerMol)
	near(t, "yesterday event Yesterday", h.events[1].Reading.Yesterday, 625/umolPerMol)
	if h.events[1].Day != "2026-07-28" {
		t.Errorf("yesterday event day = %q, want 2026-07-28", h.events[1].Day)
	}

	// Step 5: the new day's zeroes, labelled with the new day.
	exact(t, "reset DLI", h.events[2].Reading.DLI, 0)
	exact(t, "reset PeakPPFD", h.events[2].Reading.PeakPPFD, 0)
	exact(t, "reset GapMinutes", h.events[2].Reading.GapMinutes, 0)
	near(t, "reset Yesterday", h.events[2].Reading.Yesterday, 625/umolPerMol)
	if h.events[2].Day != "2026-07-29" {
		t.Errorf("reset event day = %q, want 2026-07-29", h.events[2].Day)
	}

	// Steps 3 and 4 are invisible in the event stream, so they are read off
	// the file: during steps 1 and 2 it still holds the old day, and by step 5
	// it holds the zeroed new day with yesterday committed.
	if len(seen) != 3 {
		t.Fatalf("observed %d states, want 3", len(seen))
	}
	for i, k := range wantKinds[:2] {
		if seen[i].kind != k || seen[i].fileDay != "2026-07-28" {
			t.Errorf("at %v the file said day=%q, want the old day still persisted", k, seen[i].fileDay)
		}
	}
	if seen[2].fileDay != "2026-07-29" {
		t.Errorf("at %v the file said day=%q, want the new day already persisted", RolloverReset, seen[2].fileDay)
	}
	exact(t, "persisted umolSeconds at reset", seen[2].fileUmol, 0)
	near(t, "persisted yesterdayDli at reset", seen[2].fileYesterday, 625/umolPerMol)
	if seen[2].fileYesterdayD != "2026-07-28" {
		t.Errorf("persisted yesterdayDay = %q, want the completed day so the total is dated",
			seen[2].fileYesterdayD)
	}

	// Step 3 before step 4, which is the one ordering in rollover that nothing
	// else pins. This is the caller that reaches rollover holding a live prev
	// — the interpolated boundary sample — so if rollover stopped dropping
	// prev, or persisted before dropping it, the file written mid-rollover
	// would name that sample here instead of 0. A restart from such a file
	// resumes a prev under the wrong day and credits it a bounded hold.
	if seen[2].fileLastSample != 0 {
		t.Errorf("at %v the file carried a prev at %s; rollover must drop prev *before* it writes",
			RolloverReset, time.Unix(0, seen[2].fileLastSample).UTC().Format(time.RFC3339))
	}
}

// The bounded hold counts prev as lit when it sits exactly on the threshold,
// matching litFraction's own inclusive comparison. The two branches implement
// the guard separately — one inside a day, one across midnight — so both are
// driven here; a `>` in either would silently shorten the photoperiod of every
// day that goes dark at exactly the threshold.
func TestBoundedHoldCountsPrevAtExactlyTheThresholdAsLit(t *testing.T) {
	const threshold = 20.0

	t.Run("inside a day", func(t *testing.T) {
		start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		h := newHarness(t, Config{Location: time.UTC, LitThresholdPPFD: threshold}, start)

		h.a.Add(start, threshold)
		h.a.Add(start.Add(2*time.Hour), 0) // over maxGap: bounded hold on prev

		near(t, "PhotoperiodHours", h.a.Snapshot().PhotoperiodHours, 60/3600.0)
		near(t, "DLI", h.a.Snapshot().DLI, threshold*60/umolPerMol)
	})

	t.Run("across midnight", func(t *testing.T) {
		loc := time.UTC
		late := time.Date(2026, 7, 28, 23, 59, 0, 0, loc)
		morning := time.Date(2026, 7, 29, 6, 0, 0, 0, loc)

		h := newHarness(t, Config{Location: loc, LitThresholdPPFD: threshold}, late)
		h.a.Add(late, threshold)
		h.a.Add(morning, 0)

		final := h.only(t, RolloverFinal)
		near(t, "completed day PhotoperiodHours", final.Reading.PhotoperiodHours, 60/3600.0)
		near(t, "completed day DLI", final.Reading.DLI, threshold*60/umolPerMol)
	})

	// The other side of the comparison, so the assertions above cannot pass by
	// counting everything: one µmol below threshold credits no photoperiod.
	t.Run("one µmol below the threshold is dark", func(t *testing.T) {
		start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
		h := newHarness(t, Config{Location: time.UTC, LitThresholdPPFD: threshold}, start)

		h.a.Add(start, threshold-1)
		h.a.Add(start.Add(2*time.Hour), 0)

		exact(t, "PhotoperiodHours", h.a.Snapshot().PhotoperiodHours, 0)
	})
}

func TestRolloverIsIdempotentAcrossManySamples(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 7, 28, 23, 59, 0, 0, loc)
	h := newHarness(t, Config{Location: loc}, start)

	for ts := start; ts.Before(start.Add(2 * time.Minute)); ts = ts.Add(10 * time.Second) {
		h.a.Add(ts, 400)
	}

	if got := len(h.events); got != 3 {
		t.Fatalf("want exactly one rollover (3 events) across a single midnight, got %d: %v", got, h.kinds())
	}
}

// A multi-day silence produces exactly one rollover, not one per elapsed day.
func TestMultiDayGapRollsOverOnce(t *testing.T) {
	loc := time.UTC
	first := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)
	later := time.Date(2026, 7, 31, 12, 0, 0, 0, loc)

	h := newHarness(t, Config{Location: loc}, first)
	h.a.Add(first, 900)
	h.a.Add(later, 900)

	if got := len(h.events); got != 3 {
		t.Fatalf("want one rollover (3 events), got %d: %v", got, h.kinds())
	}
	if h.a.Day() != "2026-07-31" {
		t.Errorf("Day() = %q, want 2026-07-31", h.a.Day())
	}
	// The bounded hold lands on the day that was open; the new day carries the
	// portion of the silence that fell after its own midnight.
	near(t, "completed day DLI", h.events[0].Reading.DLI, 900*60/umolPerMol)
	near(t, "new day GapMinutes", h.a.Snapshot().GapMinutes, 12*60)
	exact(t, "new day DLI", h.a.Snapshot().DLI, 0)

	// 2026-07-28 is not the day before 2026-07-31, so there is no yesterday to
	// publish. Promoting it anyway would put a three-day-old partial — one
	// that stops at the first minute of an outage, no less — under a label
	// that means "the day before today".
	exact(t, "Yesterday after a multi-day gap", h.a.Snapshot().Yesterday, 0)
	exact(t, "Yesterday on the rollover event", h.events[1].Reading.Yesterday, 0)
}

// The same physical situation reached three ways has to give one answer. It is
// the same state either way — a day open at 2026-07-28 with 0.054 mol on it,
// and the next sample three days later — so a live rollover, a cold start and
// a restart from the live run's own file must agree, or the number an operator
// sees depends on whether the daemon happened to be running.
func TestLiveAndStartupAgreeAboutYesterdayAfterAMultiDayGap(t *testing.T) {
	loc := time.UTC
	first := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)
	later := time.Date(2026, 7, 31, 12, 0, 0, 0, loc)

	livePath := tempState(t)
	live := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: livePath}, first)
	live.a.Add(first, 900)
	live.a.Add(later, 900)

	coldPath := tempState(t)
	writeState(t, coldPath, state{
		Version: stateVersion, Zone: "UTC", Day: "2026-07-28", UmolSeconds: 900 * 60,
	})
	cold := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: coldPath}, later)

	restart := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: livePath}, later.Add(time.Minute))

	for _, tc := range []struct {
		name string
		h    *harness
	}{
		{"live rollover", live},
		{"cold start onto the same state", cold},
		{"restart from the live run's own file", restart},
	} {
		if got := tc.h.a.Day(); got != "2026-07-31" {
			t.Errorf("%s: Day() = %q, want 2026-07-31", tc.name, got)
		}
		if got := tc.h.a.Snapshot().Yesterday; got != 0 {
			t.Errorf("%s: Yesterday = %v, want 0 — 2026-07-28 is not the day "+
				"before 2026-07-31", tc.name, got)
		}
	}
}

// Yesterday is cleared, not merely left unwritten: a real yesterday already on
// the books describes a day further back still once a multi-day gap intervenes.
func TestAMultiDayGapClearsAYesterdayThatWasAlreadyThere(t *testing.T) {
	loc := time.UTC
	path := tempState(t)

	// A file from 2026-07-28 carrying a legitimate yesterday for 07-27.
	writeState(t, path, state{
		Version: stateVersion, Zone: "UTC", Day: "2026-07-28",
		UmolSeconds: 20_000_000, YesterdayDay: "2026-07-27", YesterdayDLI: 33.5,
	})

	noon := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)
	h := newHarness(t, Config{Location: loc, Zone: "UTC", StatePath: path}, noon)
	near(t, "Yesterday on resume", h.a.Snapshot().Yesterday, 33.5)

	// Then a four-day silence.
	h.a.Add(noon, 900)
	h.a.Add(time.Date(2026, 8, 1, 12, 0, 0, 0, loc), 900)

	exact(t, "Yesterday after the gap", h.a.Snapshot().Yesterday, 0)
	if got := readState(t, path).YesterdayDay; got != "" {
		t.Errorf("persisted yesterdayDay = %q, want it cleared alongside the total", got)
	}
}

// A clock that steps backwards across a date rolls over and zeroes, loudly.
func TestBackwardsClockStepAcrossADateZeroesAndWarns(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 7, 29, 12, 0, 0, 0, loc)
	h := newHarness(t, Config{Location: loc}, start)

	h.a.Add(start, 1000)
	h.a.Add(start.Add(10*time.Second), 1000) // 10 000 µmol·s on 2026-07-29

	// NTP steps the clock back a day.
	h.a.Add(time.Date(2026, 7, 28, 12, 0, 0, 0, loc), 1000)

	if got := len(h.events); got != 3 {
		t.Fatalf("want one rollover, got %d: %v", got, h.kinds())
	}
	near(t, "abandoned day DLI", h.events[0].Reading.DLI, 10000/umolPerMol)
	exact(t, "DLI after the step", h.a.Snapshot().DLI, 0)
	if h.a.Day() != "2026-07-28" {
		t.Errorf("Day() = %q, want 2026-07-28", h.a.Day())
	}
	// And the abandoned partial is not promoted: 2026-07-29 is now *tomorrow*,
	// so publishing its 0.01 mol as "yesterday" would be a day that has not
	// happened yet.
	exact(t, "Yesterday after the step", h.a.Snapshot().Yesterday, 0)

	logs := h.log()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "moved backwards") {
		t.Errorf("want a WARN about the clock moving backwards, got:\n%s", logs)
	}
}

// After a backwards step across a date, the new day integrates from the sample
// that opened it. The rollover has to be told that explicitly: accrue declines
// to advance prev over a backwards interval (it would double-count; see
// TestOutOfOrderSamples...), so what survives into the new day would otherwise
// be a timestamp from the abandoned day — in its own future — and nothing
// would credit until the clock caught back up to it.
func TestBackwardsClockStepAcrossADateSeedsTheNewDayFromTheCurrentSample(t *testing.T) {
	loc := time.UTC
	start := time.Date(2026, 7, 29, 12, 0, 0, 0, loc)
	h := newHarness(t, Config{Location: loc}, start)

	h.a.Add(start, 1000)
	h.a.Add(start.Add(10*time.Second), 1000)

	stepped := time.Date(2026, 7, 28, 12, 0, 0, 0, loc)
	h.a.Add(stepped, 1000)
	h.a.Add(stepped.Add(10*time.Second), 1000)

	// Ten real seconds at 1000 µmol, measured after the step and filed under
	// the day the clock now says it is.
	near(t, "new day DLI", h.a.Snapshot().DLI, 10000/umolPerMol)
	exact(t, "new day GapMinutes", h.a.Snapshot().GapMinutes, 0)
	if h.a.Day() != "2026-07-28" {
		t.Errorf("Day() = %q, want 2026-07-28", h.a.Day())
	}
}

// ---------------------------------------------------------------------------
// DST
// ---------------------------------------------------------------------------

// constantDay drives a constant PPFD across a span at a fixed cadence and
// returns the recorded events, so a DST day can be checked against the number
// of seconds it really had.
func constantDay(t *testing.T, loc *time.Location, from, to time.Time, step time.Duration, ppfd float64) *harness {
	t.Helper()
	h := newHarness(t, Config{Location: loc}, from)
	for ts := from; !ts.After(to); ts = ts.Add(step) {
		h.a.Add(ts, ppfd)
	}
	return h
}

// A spring-forward day is 23 hours long, and a constant 1000 µmol must
// integrate to 23 h of it — not 24. Europe/London loses an hour on
// 2026-03-29 at 01:00 GMT.
func TestSpringForwardDayIsTwentyThreeHours(t *testing.T) {
	lon := mustLoad(t, "Europe/London")
	from := time.Date(2026, 3, 28, 23, 0, 0, 0, lon)
	to := time.Date(2026, 3, 30, 1, 0, 0, 0, lon)
	if h := to.Sub(from).Hours(); h != 25 {
		t.Fatalf("test setup drifted: span is %vh, want 25h (1 + 23 + 1)", h)
	}

	h := constantDay(t, lon, from, to, 30*time.Second, 1000)

	shortDay := findFinal(t, h, "2026-03-29")
	near(t, "23h day DLI", shortDay.DLI, 1000*23*3600/umolPerMol)
	near(t, "23h day PhotoperiodHours", shortDay.PhotoperiodHours, 23)
	exact(t, "23h day GapMinutes", shortDay.GapMinutes, 0)
}

// An autumn day is 25 hours long. Europe/London gains an hour on 2026-10-25
// at 02:00 BST.
func TestAutumnFallBackDayIsTwentyFiveHours(t *testing.T) {
	lon := mustLoad(t, "Europe/London")
	from := time.Date(2026, 10, 24, 23, 0, 0, 0, lon)
	to := time.Date(2026, 10, 26, 1, 0, 0, 0, lon)
	if h := to.Sub(from).Hours(); h != 27 {
		t.Fatalf("test setup drifted: span is %vh, want 27h (1 + 25 + 1)", h)
	}

	h := constantDay(t, lon, from, to, 30*time.Second, 1000)

	longDay := findFinal(t, h, "2026-10-25")
	near(t, "25h day DLI", longDay.DLI, 1000*25*3600/umolPerMol)
	near(t, "25h day PhotoperiodHours", longDay.PhotoperiodHours, 25)
	exact(t, "25h day GapMinutes", longDay.GapMinutes, 0)
}

func findFinal(t *testing.T, h *harness, day string) Reading {
	t.Helper()
	for _, e := range h.events {
		if e.Kind == RolloverFinal && e.Day == day {
			return e.Reading
		}
	}
	t.Fatalf("no final event for %s; saw %v", day, h.events)
	return Reading{}
}

// The reason the boundary is found by search rather than by time.Date.
//
// America/Havana skips local midnight on 2026-03-08: 23:59:59-05:00 is
// followed by 01:00:00-04:00. time.Date is documented as returning "a time
// that is correct in one of the two zones involved in the transition, but it
// does not guarantee which" (go1.25 src/time/time.go:1722-1727), and here it
// picks the wrong side hard enough to land on the *previous date*.
func TestBoundarySearchSurvivesASkippedMidnight(t *testing.T) {
	hav := mustLoad(t, "America/Havana")

	byDate := time.Date(2026, 3, 8, 0, 0, 0, 0, hav)
	if got := byDate.Format(dayLayout); got == "2026-03-08" {
		t.Skip("tzdata no longer skips midnight in Havana on 2026-03-08; pick another zone")
	} else if got != "2026-03-07" {
		t.Fatalf("test setup drifted: time.Date landed on %s", got)
	}

	lo := time.Date(2026, 3, 7, 23, 30, 0, 0, hav)
	hi := lo.Add(2 * time.Hour)
	if hi.Format(dayLayout) != "2026-03-08" {
		t.Fatalf("test setup drifted: hi is %s", hi.Format(time.RFC3339))
	}

	got, ok := firstInstantOf("2026-03-08", lo, hi, hav)
	if !ok {
		t.Fatal("firstInstantOf found no boundary")
	}
	// The first instant that is 2026-03-08 locally is 01:00:00-04:00.
	want := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("boundary = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Format(dayLayout) != "2026-03-08" {
		t.Errorf("boundary labels as %s, want 2026-03-08", got.Format(dayLayout))
	}
	// And the instant one second earlier must still be the old day, which is
	// what makes it the *first*.
	if before := got.Add(-time.Second).Format(dayLayout); before != "2026-03-07" {
		t.Errorf("the second before the boundary labels as %s, want 2026-03-07", before)
	}
}

// The mirror case: Havana repeats local midnight on 2026-11-01, so there are
// two instants labelled 2026-11-01T00:00:00. The boundary is the earlier one.
func TestBoundarySearchSurvivesARepeatedMidnight(t *testing.T) {
	hav := mustLoad(t, "America/Havana")

	lo := time.Date(2026, 10, 31, 23, 30, 0, 0, hav)
	hi := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)
	if lo.Format(dayLayout) != "2026-10-31" || hi.In(hav).Format(dayLayout) != "2026-11-01" {
		t.Fatalf("test setup drifted: lo=%s hi=%s", lo.Format(time.RFC3339), hi.In(hav).Format(time.RFC3339))
	}

	got, ok := firstInstantOf("2026-11-01", lo, hi, hav)
	if !ok {
		t.Fatal("firstInstantOf found no boundary")
	}
	want := time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC) // the first of the two midnights
	if !got.Equal(want) {
		t.Errorf("boundary = %s (%s), want %s",
			got.Format(time.RFC3339), got.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if before := got.Add(-time.Second).In(hav).Format(dayLayout); before != "2026-10-31" {
		t.Errorf("the second before the boundary labels as %s, want 2026-10-31", before)
	}
}

// An interval that straddles a skipped midnight still splits correctly, which
// is the property the search exists to give the integrator.
func TestStraddleAcrossASkippedMidnight(t *testing.T) {
	hav := mustLoad(t, "America/Havana")

	// 20 s before the jump to 40 s after it, in absolute time.
	boundary := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	prev := boundary.Add(-20 * time.Second)
	cur := boundary.Add(40 * time.Second)
	if prev.In(hav).Format(dayLayout) != "2026-03-07" || cur.In(hav).Format(dayLayout) != "2026-03-08" {
		t.Fatalf("test setup drifted: %s / %s", prev.In(hav), cur.In(hav))
	}

	h := newHarness(t, Config{Location: hav}, prev)
	h.a.Add(prev, 300)
	h.a.Add(cur, 300)

	// Constant 300 µmol, so each side gets 300 * its own seconds.
	near(t, "old day DLI", h.only(t, RolloverFinal).Reading.DLI, 300*20/umolPerMol)
	near(t, "new day DLI", h.a.Snapshot().DLI, 300*40/umolPerMol)
}

// firstInstantOf bisects on "does this instant label as the new day", which
// needs the predicate to go false then true and never flip back. That is an
// empirical property of modern tzdata, not a theorem about local dates, and
// the comment on firstInstantOf says so — so both halves of the claim are
// pinned here rather than left to be re-derived.
func TestBoundarySearchMonotonicityIsEmpiricalNotAxiomatic(t *testing.T) {
	// Half one: the counterexample is real, and is still in tzdata to point
	// at. America/Goose_Bay ended DST at 00:01 local, so on 1987-10-25 the
	// date begins, un-begins a minute later, and begins again an hour after
	// that. If this ever stops reproducing, the citation in firstInstantOf's
	// comment has gone stale and needs a new zone, not deleting.
	t.Run("a pre-2011 Atlantic-Canada rule breaks it", func(t *testing.T) {
		loc := mustLoad(t, "America/Goose_Bay")

		firstMidnight := time.Date(1987, 10, 25, 3, 0, 0, 0, time.UTC)  // 00:00 -03
		reverted := time.Date(1987, 10, 25, 3, 1, 0, 0, time.UTC)       // 23:01 -04
		secondMidnight := time.Date(1987, 10, 25, 4, 0, 0, 0, time.UTC) // 00:00 -04

		for _, tc := range []struct {
			at   time.Time
			want string
		}{
			{firstMidnight.Add(-time.Second), "1987-10-24"},
			{firstMidnight, "1987-10-25"},
			{reverted, "1987-10-24"}, // the flip back that breaks the bisection
			{secondMidnight, "1987-10-25"},
		} {
			if got := dayLabel(tc.at, loc); got != tc.want {
				t.Fatalf("tzdata no longer reproduces the counterexample: %s labels as %s, want %s",
					tc.at.Format(time.RFC3339), got, tc.want)
			}
		}

		// And the consequence, so the limitation is on the record rather than
		// merely described: the search lands on the *second* midnight.
		got, ok := firstInstantOf("1987-10-25", firstMidnight.Add(-time.Hour), secondMidnight.Add(time.Hour), loc)
		if !ok {
			t.Fatal("firstInstantOf found no boundary")
		}
		if !got.Equal(secondMidnight) {
			t.Errorf("boundary = %s, want the second midnight %s — if the search now finds "+
				"the first, firstInstantOf became robust to this and its comment should say so",
				got.UTC().Format(time.RFC3339), secondMidnight.Format(time.RFC3339))
		}
	})

	// Half two: no zone does this any more. Every zone that ever violated is
	// re-scanned transition by transition over the era the daemon can actually
	// be handed, which is what makes relying on the predicate legitimate.
	t.Run("no violation survives into 2011-2050", func(t *testing.T) {
		from := time.Date(2011, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
		to := time.Date(2051, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

		for _, name := range []string{
			// The four zones a 1970-2050 scan of all 498 zones in tzdata 2026b
			// flags; there are no others. The first three ended DST at 00:01
			// local; Casey made a one-off three-hour step on 2010-03-04.
			"America/Goose_Bay", "America/Moncton", "America/St_Johns", "Antarctica/Casey",
			// Plus the zones this package already leans on elsewhere, and the
			// usual suspects for unusual rules.
			"Europe/London", "America/Havana", "Australia/Lord_Howe",
			"Pacific/Apia", "America/Santiago", "Africa/Casablanca", "Asia/Tehran",
		} {
			loc := mustLoad(t, name)
			for _, at := range transitions(loc, from, to) {
				before, after := dayLabel(at.Add(-time.Second), loc), dayLabel(at, loc)
				if after < before {
					t.Errorf("%s: the local date goes backwards at %s (%s -> %s); "+
						"firstInstantOf can land on the wrong midnight here",
						name, at.Format(time.RFC3339), before, after)
				}
			}
		}
	})
}

// transitions returns every instant in [from, to) at which loc's UTC offset
// changes. Candidate days are probed one at a time and each change is then
// bisected to the second, which finds every transition unless two fall inside
// the same day and return to the same offset — nothing in tzdata does that.
func transitions(loc *time.Location, from, to int64) []time.Time {
	const day = 24 * 60 * 60
	var out []time.Time

	_, off := time.Unix(from, 0).In(loc).Zone()
	lo := from
	for lo < to {
		hi := min(lo+day, to)
		_, next := time.Unix(hi, 0).In(loc).Zone()
		if next != off {
			a, b := lo, hi
			for b-a > 1 {
				mid := a + (b-a)/2
				if _, m := time.Unix(mid, 0).In(loc).Zone(); m == off {
					a = mid
				} else {
					b = mid
				}
			}
			out = append(out, time.Unix(b, 0).In(loc))
			off = next
		}
		lo = hi
	}
	return out
}

// ---------------------------------------------------------------------------
// zone changes
// ---------------------------------------------------------------------------

func TestSetLocationRollsOverOnlyWhenTheLabelChanges(t *testing.T) {
	start := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)

	t.Run("same label: no rollover, nothing re-filed", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC, Zone: "UTC0"}, start)
		h.a.Add(start, 600)
		h.a.Add(start.Add(10*time.Second), 600) // 6000 µmol·s

		// One hour west: 19:00 on the same date.
		h.a.SetLocation(time.FixedZone("WEST1", -3600), "WEST1")
		h.a.Add(start.Add(20*time.Second), 600)

		if len(h.events) != 0 {
			t.Errorf("no rollover expected, got %v", h.kinds())
		}
		near(t, "DLI", h.a.Snapshot().DLI, 12000/umolPerMol)
		if !strings.Contains(h.log(), "local zone changed") {
			t.Errorf("the zone change should be logged, got:\n%s", h.log())
		}
	})

	t.Run("label changes: rollover, still nothing re-filed", func(t *testing.T) {
		h := newHarness(t, Config{Location: time.UTC, Zone: "UTC0"}, start)
		h.a.Add(start, 600)
		h.a.Add(start.Add(10*time.Second), 600) // 6000 µmol·s on 2026-07-28

		// Twelve hours east: 20:00 UTC is already 08:00 the next day.
		h.a.SetLocation(time.FixedZone("EAST12", 12*3600), "EAST12")
		h.a.Add(start.Add(20*time.Second), 600)

		final := h.only(t, RolloverFinal)
		if final.Day != "2026-07-28" {
			t.Errorf("final event day = %q, want 2026-07-28", final.Day)
		}
		// Retro-adjustment would have re-filed or rescaled these seconds.
		near(t, "completed day DLI", final.Reading.DLI, 12000/umolPerMol)
		if h.a.Day() != "2026-07-29" {
			t.Errorf("Day() = %q, want 2026-07-29", h.a.Day())
		}
		exact(t, "new day DLI", h.a.Snapshot().DLI, 0)
	})
}

func TestSetLocationIgnoresNilAndNoOps(t *testing.T) {
	start := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Location: time.UTC, Zone: "UTC0"}, start)

	h.a.SetLocation(nil, "whatever")
	h.a.SetLocation(time.UTC, "UTC0")

	if strings.Contains(h.log(), "local zone changed") {
		t.Errorf("neither call changes anything, so neither should log:\n%s", h.log())
	}
}

// An empty zone falls back to the location's own name, so the state file never
// records an anonymous basis that a later run could not compare against.
func TestSetLocationDefaultsTheZoneToTheLocationName(t *testing.T) {
	start := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC)
	path := tempState(t)
	h := newHarness(t, Config{Location: time.UTC, Zone: "UTC0", StatePath: path}, start)

	lon := mustLoad(t, "Europe/London")
	h.a.SetLocation(lon, "")

	h.a.Add(start, 500)
	if got := readState(t, path).Zone; got != lon.String() {
		t.Errorf("persisted zone = %q, want the location name %q", got, lon.String())
	}
}

// ---------------------------------------------------------------------------
// small pieces
// ---------------------------------------------------------------------------

func TestPreviousDay(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2026-07-28", "2026-07-27"},
		{"2026-01-01", "2025-12-31"},
		{"2026-03-01", "2026-02-28"},
		{"2024-03-01", "2024-02-29"}, // leap year
		// The two DST days in Europe/London. Arithmetic in the local zone
		// would land 23 or 25 hours away; in UTC it is always one date.
		{"2026-03-29", "2026-03-28"},
		{"2026-10-25", "2026-10-24"},
		{"", ""},
		{"not-a-date", ""},
		{"2026-13-40", ""},
	} {
		if got := previousDay(tc.in); got != tc.want {
			t.Errorf("previousDay(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstInstantOfRejectsNonStraddlingIntervals(t *testing.T) {
	loc := time.UTC
	mid := time.Date(2026, 7, 29, 0, 0, 0, 0, loc)

	for _, tc := range []struct {
		name   string
		want   string
		lo, hi time.Time
	}{
		{"hi is not the wanted day", "2026-07-29", mid.Add(-2 * time.Hour), mid.Add(-time.Hour)},
		{"lo is already the wanted day", "2026-07-29", mid.Add(time.Hour), mid.Add(2 * time.Hour)},
		{"hi precedes lo", "2026-07-29", mid.Add(time.Hour), mid.Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := firstInstantOf(tc.want, tc.lo, tc.hi, loc); ok {
				t.Errorf("firstInstantOf = %s, ok, want !ok", got.Format(time.RFC3339))
			}
		})
	}
}

func TestFirstInstantOfIsExactOnASimpleZone(t *testing.T) {
	loc := time.UTC
	mid := time.Date(2026, 7, 29, 0, 0, 0, 0, loc)

	// Sub-second endpoints on both sides, so truncation to whole seconds has
	// to be handled rather than assumed away.
	lo := mid.Add(-5*time.Second - 700*time.Millisecond)
	hi := mid.Add(7*time.Second + 300*time.Millisecond)

	got, ok := firstInstantOf("2026-07-29", lo, hi, loc)
	if !ok {
		t.Fatal("firstInstantOf found no boundary")
	}
	if !got.Equal(mid) {
		t.Errorf("boundary = %s, want %s", got.Format(time.RFC3339Nano), mid.Format(time.RFC3339Nano))
	}
}

func TestLerp(t *testing.T) {
	t0 := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(12 * time.Second)

	near(t, "midpoint", lerp(t0, 100, t1, 220, t0.Add(6*time.Second)), 160)
	near(t, "five twelfths", lerp(t0, 100, t1, 220, t0.Add(5*time.Second)), 150)
	exact(t, "at t0", lerp(t0, 100, t1, 220, t0), 100)
	exact(t, "at t1", lerp(t0, 100, t1, 220, t1), 220)
	exact(t, "zero span falls back to p1", lerp(t0, 100, t0, 220, t0), 220)
}

func TestNewRequiresALocation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New with no Location should fail rather than silently assume UTC")
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	a, err := New(Config{Location: time.UTC, Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.maxGap != DefaultMaxGap {
		t.Errorf("maxGap = %v, want %v", a.maxGap, DefaultMaxGap)
	}
	if a.litThreshold != DefaultLitThresholdPPFD {
		t.Errorf("litThreshold = %v, want %v", a.litThreshold, DefaultLitThresholdPPFD)
	}
	if a.zone != time.UTC.String() {
		t.Errorf("zone = %q, want %q", a.zone, time.UTC.String())
	}
	if a.log == nil {
		t.Error("a nil Logger should be replaced, not left to panic")
	}
	// Nothing panics without an OnRollover hook.
	a.Add(time.Unix(0, 0).Add(24*time.Hour), 100)
}

func TestNegativeLitThresholdCountsEveryMoment(t *testing.T) {
	start := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Location: time.UTC, LitThresholdPPFD: -1}, start)

	h.a.Add(start, 0)
	h.a.Add(start.Add(30*time.Second), 0)

	near(t, "PhotoperiodHours", h.a.Snapshot().PhotoperiodHours, 30/3600.0)
}

func TestEventKindString(t *testing.T) {
	for _, tc := range []struct {
		k    EventKind
		want string
	}{
		{RolloverFinal, "final"},
		{RolloverYesterday, "yesterday"},
		{RolloverReset, "reset"},
		{EventKind(0), "EventKind(0)"},
	} {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("EventKind(%d).String() = %q, want %q", int(tc.k), got, tc.want)
		}
	}
}

// The package claims Add and Snapshot are safe together; -race has to agree.
func TestConcurrentAddAndSnapshot(t *testing.T) {
	start := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	h := newHarness(t, Config{Location: time.UTC}, start)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := range 500 {
			h.a.Add(start.Add(time.Duration(i)*10*time.Second), float64(i%900))
		}
	}()
	for range 2 {
		go func() {
			defer wg.Done()
			for range 500 {
				_ = h.a.Snapshot()
				_ = h.a.Day()
			}
		}()
	}
	wg.Wait()
}
