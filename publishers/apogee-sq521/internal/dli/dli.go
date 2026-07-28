// Package dli measures Daily Light Integral: the photosynthetic photon flux
// density a canopy actually received over one local day, integrated into
// mol·m⁻²·d⁻¹.
//
// Nothing else in the fleet measures this. grow-app's "DLI now" is a
// *projection* — dliFor(ppfd, onHours) = (ppfd*onHours*3600)/1e6, i.e. the
// instantaneous PPFD extrapolated across the *planned* photoperiod. It answers
// "what would today reach if the light held steady until lights-out". This
// package answers "what did today actually accumulate", which is a different
// number and the one a grower compares against a cultivar's DLI target.
//
// Three properties define the package:
//
//  1. It never fabricates light. Every joule credited comes from an interval
//     bounded by two real samples, or from an explicitly bounded hold when a
//     sample is missing (see the max-gap guard on Add). A publisher that dies
//     at dusk and returns at dawn must not credit the night.
//
//  2. It survives restarts without zeroing. A `systemctl restart` at 14:00
//     resumes the half-finished day from $STATE_DIRECTORY/dli-state.json.
//     Zeroing would produce a saw-tooth that any downstream last()/max() reads
//     as a real drop in light.
//
//  3. Days are decided by comparing date labels derived from the clock, never
//     by arming a timer at a computed midnight. See rollover for why that
//     matters in zones whose DST transition lands on midnight.
//
// # Integration rule
//
// Trapezoid, accumulating µmol·s in a float64 and dividing by 1e6 only at
// publish time.
//
// Accuracy is not the reason. At ~10-16 s sampling over a 12-20 h photoperiod
// the trapezoid-versus-rectangle difference telescopes to the two end samples
// and comes to roughly 0.03% of the day. The reason is that grow-app already
// has a trapezoidal integrator (integratePower, in JS) that may later audit
// this number from recorded samples. If the two used different rules, every
// comparison would show a spurious delta and the audit would be worthless.
// Same rule means they agree to float noise, so a real disagreement is a real
// signal.
//
// # Concurrency
//
// An Accumulator is safe for concurrent use. Config.OnRollover is called
// synchronously from Add with the internal lock held, so it must not call back
// into the Accumulator — it is handed a complete Reading precisely so it never
// needs to — and it should not block, because Add runs on the poll path.
package dli

import (
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

// dayLayout is the local date label. Labels are compared as strings, and
// because this layout is fixed-width and big-endian, string order is also
// chronological order — which is how rollover detects a clock that went
// backwards.
const dayLayout = "2006-01-02"

// umolPerMol converts the µmol·s accumulator to mol·m⁻²·d⁻¹ at publish time.
const umolPerMol = 1e6

// DefaultMaxGap is the longest interval between samples that is integrated
// normally. It is about four times the 13-16 s this sensor actually shows
// between samples (the poll is nominally 10 s, but each cycle is three SDI-12
// measurement transactions and each has its own settling time), which absorbs
// that jitter plus a dropped poll or two with no special case, while staying
// far shorter than any outage worth noticing.
const DefaultMaxGap = 60 * time.Second

// DefaultLitThresholdPPFD is the PPFD at or above which a moment counts toward
// PhotoperiodHours. It sits above the quantum sensor's dark offset and above
// the light that leaks into a closed tent, and far below any real photoperiod
// intensity.
//
// TODO: this number is a guess. Validate it against a recorded night — log raw
// PPFD through a full dark period with the room light on and off — and adjust
// before release. Too low and stray room light inflates the photoperiod; too
// high and the dawn and dusk ramps get clipped.
const DefaultLitThresholdPPFD = 20.0

// persistInterval is the coalescing window for state writes. Persisting every
// poll would be 8640 writes a day onto an SD card; at one write a minute the
// worst case lost to a hard power cut is one minute of integration: 0.015 mol
// at an indoor canopy's 250 µmol, 0.12 mol under full sun. Against the ±5%
// calibration on a 30 mol day — 1.5 mol — neither is worth another 7000 writes.
const persistInterval = 60 * time.Second

// Reading is the published view of the accumulator.
type Reading struct {
	// DLI is today's integral so far, in mol·m⁻²·d⁻¹.
	DLI float64
	// Yesterday is the completed integral for the previous day, or 0 when
	// there is no trustworthy previous day (first run, or a gap long enough
	// that the stored day was not literally yesterday).
	Yesterday float64
	// GapMinutes is how much of today went uncredited because samples were
	// missing — a data-quality signal, not a measurement. A day with a
	// non-zero gap under-reports DLI by an unknown amount.
	GapMinutes float64
	// PhotoperiodHours is how long today spent at or above the lit threshold,
	// integrated under the same interval and max-gap rules as DLI so the two
	// never disagree about whether a stretch of time was measured.
	PhotoperiodHours float64
	// PeakPPFD is the highest single sample seen today. Only real samples
	// count; the synthetic sample interpolated at a midnight boundary does
	// not, because it was never measured.
	PeakPPFD float64
}

// EventKind identifies which step of the midnight rollover produced an Event.
type EventKind int

const (
	// RolloverFinal carries the completed day's full total. It is emitted
	// first so that the last point recorded against that date is the whole
	// day, which is what a downstream last() query returns.
	RolloverFinal EventKind = iota + 1
	// RolloverYesterday carries the completed day's total again, now with the
	// yesterday promotion applied — or with Reading.Yesterday cleared, when
	// the day that closed was not literally the day before the one opening.
	// See rollover.
	RolloverYesterday
	// RolloverReset carries the new day's zeroes. It is emitted last, and
	// only after the zeroed state has been persisted, so a crash can never
	// leave a published zero that the file would contradict on resume.
	RolloverReset
)

func (k EventKind) String() string {
	switch k {
	case RolloverFinal:
		return "final"
	case RolloverYesterday:
		return "yesterday"
	case RolloverReset:
		return "reset"
	default:
		return fmt.Sprintf("EventKind(%d)", int(k))
	}
}

// Event is one step of a rollover, handed to Config.OnRollover.
type Event struct {
	Kind EventKind
	// Day is the local date label the Reading belongs to: the completed day
	// for RolloverFinal and RolloverYesterday, the new day for RolloverReset.
	Day string
	// Reading is the accumulator's state at that step.
	Reading Reading
}

// Config constructs an Accumulator.
type Config struct {
	// Location is the local zone that defines a "day". Required.
	Location *time.Location

	// Zone is the identity recorded alongside the persisted day label —
	// normally the POSIX TZ string the location was built from, e.g.
	// "GMT0BST,M3.5.0/1,M10.5.0". A restart whose zone differs from the file's
	// refuses to resume rather than silently mixing two bases. Empty means
	// Location.String().
	Zone string

	// MaxGap is the longest interval integrated normally; see Add. Zero
	// selects DefaultMaxGap.
	MaxGap time.Duration

	// LitThresholdPPFD is the PPFD at or above which a moment counts toward
	// PhotoperiodHours. Zero selects DefaultLitThresholdPPFD; a negative value
	// counts every moment, which is the way to disable the threshold.
	LitThresholdPPFD float64

	// StatePath is the JSON state file. Empty disables persistence entirely,
	// which is the correct behaviour when systemd granted no StateDirectory:
	// the sensor is still useful, it just forgets across restarts. Use
	// StatePath(dir) to build it.
	StatePath string

	// Now supplies the current instant. It is consulted exactly once, by New,
	// to decide whether the state file describes today; every later decision
	// uses the timestamp passed to Add. Nil means time.Now.
	Now func() time.Time

	// Logger receives the handful of lines that explain a surprising day:
	// a refused resume, a clock that went backwards, a zone change. Nil
	// discards them.
	Logger *slog.Logger

	// OnRollover observes the midnight sequence. See the package doc for the
	// re-entrancy constraint. Nil ignores rollovers, which is what tests that
	// only assert on Snapshot do.
	OnRollover func(Event)
}

// sample is one point of the integrand.
type sample struct {
	t    time.Time
	ppfd float64
}

// Accumulator integrates PPFD over a local day.
//
// The zero value is not usable; construct one with New.
type Accumulator struct {
	mu sync.Mutex

	loc          *time.Location
	zone         string
	maxGap       time.Duration
	litThreshold float64
	statePath    string
	log          *slog.Logger
	onRollover   func(Event)

	// st is both the live accumulator and the on-disk format; see state.go.
	st state
	// prev is the last point integrated, nil when there is nothing to
	// integrate from. It is held apart from st because it is a *time.Time*
	// rather than the int64 the file carries, and it is folded back into st
	// only when marshalling.
	prev *sample
	// lastPersist is the sample timestamp of the last write, which is what
	// makes the persist cadence coalesce; see maybePersist.
	lastPersist time.Time
}

// New returns an Accumulator, resuming from cfg.StatePath when the file
// describes today under the same zone.
//
// Startup never fails on a bad state file: a missing, truncated, corrupt or
// future-versioned file is logged and the day starts from zero, because a
// sensor that refuses to publish is worse than one that lost this morning. The
// only error returned is a nil Location, which is a programming mistake — a
// silent fallback to UTC would compute the wrong days everywhere but Britain
// in winter.
func New(cfg Config) (*Accumulator, error) {
	if cfg.Location == nil {
		return nil, fmt.Errorf("dli: Config.Location is required")
	}

	a := &Accumulator{
		loc:          cfg.Location,
		zone:         cfg.Zone,
		maxGap:       cfg.MaxGap,
		litThreshold: cfg.LitThresholdPPFD,
		statePath:    cfg.StatePath,
		log:          cfg.Logger,
		onRollover:   cfg.OnRollover,
	}
	if a.zone == "" {
		a.zone = cfg.Location.String()
	}
	if a.maxGap <= 0 {
		a.maxGap = DefaultMaxGap
	}
	if a.litThreshold == 0 {
		a.litThreshold = DefaultLitThresholdPPFD
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}

	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	today := dayLabel(now(), a.loc)

	a.st = state{Version: stateVersion, Zone: a.zone, Day: today}
	a.resume(today)
	return a, nil
}

// resume applies the loaded state to a freshly zeroed accumulator.
//
// The four cases are deliberately distinct, because they carry different
// amounts of trust:
//
//   - No usable file. Normal on first run, so INFO.
//   - A different zone. The stored day label was computed under a basis we no
//     longer use, so neither it nor the yesterday it implies means anything
//     here. Zero, and say both strings out loud — this is the line an operator
//     needs when a day looks wrong after a TZ edit.
//   - Same day, same zone. Resume everything, including the last sample. This
//     is the case the file exists for.
//   - A different day. Today starts from zero either way; the only question is
//     whether the stored day can be published as "yesterday".
func (a *Accumulator) resume(today string) {
	if a.statePath == "" {
		return
	}

	s, err := loadState(a.statePath)
	if err != nil {
		a.log.Info("dli: starting from zero, no usable state file",
			"path", a.statePath, "day", today, "reason", err)
		return
	}

	if s.Zone != a.zone {
		a.log.Warn("dli: refusing to resume across a zone change; starting from zero",
			"path", a.statePath, "stored_zone", s.Zone, "effective_zone", a.zone,
			"stored_day", s.Day, "day", today)
		return
	}

	if s.Day == today {
		a.st = s
		// yesterdayDay is what makes the stored yesterdayDli checkable. Both
		// are written together and only ever together, so a file where they
		// disagree was not written by this code — truncated, hand-edited,
		// merged from another host — and a total under the wrong date is
		// precisely what Reading.Yesterday promises not to publish.
		if a.st.YesterdayDLI != 0 && a.st.YesterdayDay != previousDay(today) {
			a.log.Warn("dli: state file's yesterday is not dated the day before it; dropping it",
				"path", a.statePath, "day", today,
				"stored_yesterday_day", a.st.YesterdayDay, "stored_yesterday_dli", a.st.YesterdayDLI)
			a.st.YesterdayDay = ""
			a.st.YesterdayDLI = 0
		}
		if s.LastSampleUnixNano != 0 {
			// The resumed prev feeds the ordinary max-gap guard with no
			// restart-specific branch. Restart plus serial settle plus the
			// first poll is essentially always more than maxGap, so the guard
			// credits lastPpfd × maxGap and restarts — which is exactly the
			// right answer, and it is the same answer any other over-long
			// interval gets.
			a.prev = &sample{t: time.Unix(0, s.LastSampleUnixNano).In(a.loc), ppfd: s.LastPPFD}
		}
		a.log.Info("dli: resumed partial day",
			"day", today, "dli", a.st.UmolSeconds/umolPerMol, "gap_minutes", a.st.GapSeconds/60)
		return
	}

	// A different day. The stored day's own total is what would become
	// "yesterday" — not the stored yesterdayDli, which by then describes the
	// day before that. Promote it only when the stored day is literally the
	// label before today; after a longer gap there is no yesterday to speak
	// of and publishing a stale total would be worse than publishing nothing.
	switch prior := previousDay(today); {
	case s.Day == prior:
		a.st.YesterdayDay = s.Day
		a.st.YesterdayDLI = s.UmolSeconds / umolPerMol
		a.log.Info("dli: new day; carried yesterday forward from state",
			"day", today, "yesterday_day", s.Day, "yesterday_dli", a.st.YesterdayDLI)
	case s.Day > today:
		// The file is dated ahead of the clock: either the RTC lost time or
		// NTP stepped backwards across a date. Either way the stored day is
		// not a past day and cannot be yesterday.
		a.log.Warn("dli: state file is dated ahead of the clock; starting from zero",
			"path", a.statePath, "stored_day", s.Day, "day", today)
	default:
		a.log.Info("dli: state file is older than yesterday; starting from zero with no yesterday",
			"path", a.statePath, "stored_day", s.Day, "day", today)
	}
}

// Add integrates one PPFD sample taken at t.
//
// Samples must arrive in time order; see below for what happens when they do
// not. Non-finite readings are dropped outright, and negative readings are
// clamped to zero — quantum sensors read slightly negative in darkness, and
// grow-app clamps identically, so both agree that darkness is exactly 0 rather
// than a small debt.
//
// # The max-gap guard
//
// The failure this exists for: 1000 µmol at 19:59:50, the publisher dies, and
// it returns at 06:00 the next morning reading 0. A naive trapezoid bridges
// the outage as (1000+0)/2 × 36010 s = 1.8e7 µmol·s = 18 mol — roughly a 40%
// inflation, credited to the wrong day.
//
// So an interval longer than maxGap is not integrated. Instead:
//
//	umolSeconds += prev.ppfd * maxGap   // a rectangle on the earlier sample
//	prev = cur                          // then restart; credit nothing more
//	gapSeconds += dt - maxGap
//
// A rectangle on prev rather than a trapezoid because we have no evidence
// about the interior, and prev is the last thing that was actually measured.
//
// Alternatives, and why not:
//
//   - Naive trapezoid across the gap: over-counts as shown above.
//   - min(prev, cur) × dt: still fabricates hours of light nobody measured; it
//     merely fabricates less.
//   - Credit 0 for the whole interval: under-counts by a full interval's worth
//     at every gap, so frequent short outages bias the day low, and it falls
//     off a cliff at the threshold — an interval of exactly maxGap credits
//     (p0+p1)/2·maxGap and one nanosecond more credits nothing. The bounded
//     hold's step at the threshold is from (p0+p1)/2·maxGap to p0·maxGap,
//     i.e. (p1−p0)/2·maxGap, which is zero for steady light and bounded by
//     half a sample-to-sample change otherwise. It costs one line.
//
// The comparison is dt <= maxGap, so an interval of exactly maxGap is
// integrated normally.
//
// # Out-of-order samples
//
// An interval of zero or negative length credits nothing *and leaves prev
// alone*, so prev stays at the latest sample seen rather than rewinding to the
// out-of-order one. Rewinding would re-integrate the overlap on the next
// forward interval:
//
//	12:00:00 @1000, 12:00:30 @1000   -> 30 000 µmol·s
//	12:00:01 @1000                   -> nothing, but prev rewinds to +1 s
//	12:00:31 @1000                   -> 30 000 µmol·s again
//
// 60 000 µmol·s for a real span of 31 s at 1000 — fabricated light, which the
// package's first property forbids, so the rewind is the one thing this branch
// must not do. Holding the later prev errs the other way instead: the stretch
// the clock repeated credits nothing until it passes prev again, and if that
// takes longer than maxGap the ordinary guard books the remainder as gap, so
// the shortfall is visible in GapMinutes rather than silent.
//
// Within a day this is a duplicate poll or a small NTP correction; across a
// day it is handled by the rollover, which zeroes and logs.
func (a *Accumulator) Add(t time.Time, ppfd float64) {
	if math.IsNaN(ppfd) || math.IsInf(ppfd, 0) {
		return
	}
	if ppfd < 0 {
		ppfd = 0
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	day := dayLabel(t, a.loc)
	if day != a.st.Day {
		a.crossMidnight(day, t, ppfd)
	} else {
		a.accrue(t, ppfd)
	}

	if ppfd > a.st.PeakPPFD {
		a.st.PeakPPFD = ppfd
	}
	a.maybePersist(t)
}

// crossMidnight handles a sample whose local date differs from the accumulated
// day. It runs with the lock held and leaves prev pointing at the new sample.
func (a *Accumulator) crossMidnight(day string, t time.Time, ppfd float64) {
	prev := a.prev

	// Nothing spans the boundary yet: this is the first sample the process has
	// seen, so the rollover is the whole story and the sample seeds the new
	// day.
	if prev == nil {
		a.rollover(day, t)
		a.accrue(t, ppfd)
		return
	}

	boundary, ok := firstInstantOf(day, prev.t, t, a.loc)
	if !ok {
		// The label changed without any instant in the interval crossing a
		// local midnight. Time moving forward under a stable zone cannot do
		// that, so exactly two things reach here, and both want the same
		// treatment:
		//
		//   - The clock stepped backwards over a date. There is no forward
		//     interval at all, and accrue credits nothing for one — the same
		//     answer it gives inside a day.
		//   - The zone moved rather than the clock: SetLocation re-labelled an
		//     interval that never passed a local midnight. That is a real
		//     interval, and accrue integrates it by the ordinary rules.
		//
		// Either way it belongs to the day that was open, because
		// already-integrated seconds are never re-filed, and the new day
		// starts from the current sample.
		//
		// The re-seed is spelled out rather than carried over from accrue,
		// because accrue only advances prev when the interval moved forward:
		// in the backwards-clock case it deliberately leaves prev alone. That
		// stale prev cannot survive rollover, which drops it unconditionally —
		// so without this line the new day would start with no prev at all and
		// silently lose its first interval (DLI 0 rather than the first
		// sample's contribution), which is what deleting it actually produces.
		a.accrue(t, ppfd)
		a.rollover(day, t)
		a.prev = &sample{t: t, ppfd: ppfd}
		return
	}

	// The interval is short enough to trust, so split it at the boundary: the
	// pre-boundary trapezoid belongs to the day that ended and the
	// post-boundary trapezoid to the day that began. Anything else would file
	// up to a full interval of real light under the wrong date.
	if t.Sub(prev.t) <= a.maxGap {
		mid := lerp(prev.t, prev.ppfd, t, ppfd, boundary)
		a.accrue(boundary, mid)
		a.rollover(day, t)
		// rollover nils prev; what must not survive it is a prev from the old
		// day. Re-seeding at the boundary instant is not that — it is the
		// first point of the new day, and without it the post-boundary
		// trapezoid would be lost.
		a.prev = &sample{t: boundary, ppfd: mid}
		a.accrue(t, ppfd)
		return
	}

	// Over-gap across midnight. The max-gap guard applies exactly as it does
	// mid-day — bounded hold on prev, then restart — because a long silence
	// tells us nothing about the interior no matter which dates it spans. The
	// hold is credited whole to prev's day: it is bounded by maxGap, so the
	// most it can misfile is a minute of light, and it is prev's own
	// measurement.
	//
	// The uncredited remainder is divided at the boundary, though, because
	// gapSeconds is a per-day quality counter: a day that begins in the middle
	// of an outage should say so rather than inheriting a zero.
	a.st.UmolSeconds += prev.ppfd * a.maxGap.Seconds()
	if prev.ppfd >= a.litThreshold {
		a.st.LitSeconds += a.maxGap.Seconds()
	}
	resume := prev.t.Add(a.maxGap)
	if d := boundary.Sub(resume); d > 0 {
		a.st.GapSeconds += d.Seconds()
	}
	carry := t.Sub(maxTime(boundary, resume))

	a.rollover(day, t)
	if carry > 0 {
		a.st.GapSeconds += carry.Seconds()
	}
	a.prev = &sample{t: t, ppfd: ppfd}
}

// accrue integrates the interval from prev to (t, ppfd) and makes it the new
// prev. See Add for the rules it implements.
func (a *Accumulator) accrue(t time.Time, ppfd float64) {
	prev := a.prev
	if prev == nil {
		a.prev = &sample{t: t, ppfd: ppfd}
		return
	}

	// Out of order: credit nothing, and leave prev where it is. Advancing it
	// to the earlier sample would make the *next* forward interval re-integrate
	// seconds already counted; see Add.
	dt := t.Sub(prev.t)
	if dt <= 0 {
		return
	}

	a.prev = &sample{t: t, ppfd: ppfd}
	switch {
	case dt > a.maxGap:
		a.st.UmolSeconds += prev.ppfd * a.maxGap.Seconds()
		if prev.ppfd >= a.litThreshold {
			a.st.LitSeconds += a.maxGap.Seconds()
		}
		a.st.GapSeconds += dt.Seconds() - a.maxGap.Seconds()
	default:
		// Plain float64 addition, deliberately. No Kahan summation: a full day
		// peaks around 5e7 µmol·s, each increment is at most ~1e5, and
		// float64 carries 15-16 significant digits, so the relative error per
		// increment is ~1e-16 and the accumulated error over 8640 samples is
		// far below the sensor's own ±5% calibration. Compensated summation
		// here would buy nothing and would be one more thing to get wrong.
		secs := dt.Seconds()
		a.st.UmolSeconds += (prev.ppfd + ppfd) / 2 * secs
		a.st.LitSeconds += litFraction(prev.ppfd, ppfd, a.litThreshold) * secs
	}
}

// rollover closes out a.st.Day and opens newDay.
//
// The order is load-bearing and is the reason this is not three lines inline:
//
//  1. Emit the completed day's final total, so the last point recorded against
//     that date is the whole day.
//  2. Promote that total to yesterday — or clear yesterday, when the day that
//     closed was not literally the day before newDay — and emit it.
//  3. Zero the accumulators and drop prev. Every caller relies on the drop:
//     prev was measured under the day that just closed, so all four re-seed
//     *after* this returns rather than leaving it in place — three with an
//     explicit sample, and the first-sample path via accrue.
//  4. Persist. Step 3 has to precede this, not follow it: the file is what a
//     restart resumes from, and a file that carried the old day's prev under
//     the new day's label would credit that sample a bounded hold against a
//     day it never touched. The straddle caller is what makes the ordering
//     observable — it reaches rollover with a live prev.
//  5. Only then emit the new day's zeroes.
func (a *Accumulator) rollover(newDay string, t time.Time) {
	old := a.st.Day
	total := a.st.UmolSeconds / umolPerMol

	if newDay < old {
		// Labels sort chronologically, so this is a clock that moved
		// backwards across a date — an NTP step on a Pi with no RTC is the
		// usual cause. The day is zeroed regardless, because carrying a
		// partial total into a date that already happened is worse than
		// losing it, but it gets a line in the journal.
		a.log.Warn("dli: local date moved backwards; zeroing the day",
			"from", old, "to", newDay, "sample_time", t, "dli", total)
	}

	a.emit(Event{Kind: RolloverFinal, Day: old, Reading: a.reading()})

	// Promote only when the day that closed is literally the label before the
	// one opening. A multi-day silence and a clock that stepped backwards over
	// a date both land here with a total that is not yesterday's, and
	// Reading.Yesterday promises not to publish one of those.
	//
	// The else branch is not redundant: whatever yesterday held described the
	// day before old, which is further from newDay still, so leaving it would
	// publish an even staler number.
	//
	// resume applies the identical test to the state file at startup. That is
	// the point of doing it here — the same physical situation reached live
	// and reached through a restart has to give the same answer, or the number
	// changes under an operator for no reason they can see.
	if old == previousDay(newDay) {
		a.st.YesterdayDay = old
		a.st.YesterdayDLI = total
	} else {
		a.st.YesterdayDay = ""
		a.st.YesterdayDLI = 0
	}
	a.emit(Event{Kind: RolloverYesterday, Day: old, Reading: a.reading()})

	a.st.Day = newDay
	a.st.UmolSeconds = 0
	a.st.PeakPPFD = 0
	a.st.LitSeconds = 0
	a.st.GapSeconds = 0
	a.prev = nil

	a.persistAt(t)

	a.emit(Event{Kind: RolloverReset, Day: newDay, Reading: a.reading()})
}

// SetLocation adopts a new local zone mid-run, as a TZ reload would.
//
// Nothing is retro-adjusted: seconds already integrated stay where they were
// filed. The zone simply changes how the *next* sample's date label is
// computed, and the ordinary label comparison in Add then decides whether that
// amounts to a rollover. A shift that keeps today's label — most of them —
// costs nothing at all.
func (a *Accumulator) SetLocation(loc *time.Location, zone string) {
	if loc == nil {
		return
	}
	if zone == "" {
		zone = loc.String()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.loc == loc && a.zone == zone {
		return
	}
	a.log.Info("dli: local zone changed",
		"from", a.zone, "to", zone, "day", a.st.Day,
		"note", "already-integrated seconds are not re-filed")
	a.loc = loc
	a.zone = zone
}

// Snapshot returns the current published view.
func (a *Accumulator) Snapshot() Reading {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reading()
}

// Day returns the local date label the accumulator is currently filing under.
func (a *Accumulator) Day() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.Day
}

// Flush writes the state file now. Call it on shutdown; the coalescing cadence
// otherwise leaves up to persistInterval unwritten. It is a no-op when
// persistence is disabled.
func (a *Accumulator) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.write()
}

// reading projects the raw accumulators into published units. It must be
// called with the lock held.
func (a *Accumulator) reading() Reading {
	return Reading{
		DLI:              a.st.UmolSeconds / umolPerMol,
		Yesterday:        a.st.YesterdayDLI,
		GapMinutes:       a.st.GapSeconds / 60,
		PhotoperiodHours: a.st.LitSeconds / 3600,
		PeakPPFD:         a.st.PeakPPFD,
	}
}

func (a *Accumulator) emit(e Event) {
	if a.onRollover != nil {
		a.onRollover(e)
	}
}

// maybePersist writes at most once per persistInterval of sample time.
//
// The cadence is driven by sample timestamps rather than a background ticker
// on purpose. Nothing in the accumulator changes except in Add, so a ticker
// could only ever rewrite an unchanged file; keying off the samples themselves
// gives the same steady-state rate (one write per six polls at a 10 s
// interval) with no goroutine, no second lock holder, and a cadence that a
// test driving a simulated day can predict exactly.
func (a *Accumulator) maybePersist(t time.Time) {
	if a.lastPersist.IsZero() || t.Sub(a.lastPersist) >= persistInterval || t.Before(a.lastPersist) {
		a.persistAt(t)
	}
}

// persistAt writes and records the cadence mark. The mark advances even when
// the write fails, so a full or read-only filesystem produces one warning a
// minute rather than one per sample.
func (a *Accumulator) persistAt(t time.Time) {
	a.lastPersist = t
	if err := a.write(); err != nil {
		a.log.Warn("dli: could not persist state", "path", a.statePath, "err", err)
	}
}

// dayLabel renders t's local date in loc.
func dayLabel(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(dayLayout)
}

// previousDay returns the label of the calendar day before the given label.
//
// It takes a label rather than a time.Time and a location, which is what makes
// it immune to DST: time.Parse with no zone in the layout yields a UTC
// midnight, and UTC has no transitions, so stepping back a day is unambiguous
// for a 23-hour local day as much as a 25-hour one. Resist adding a
// *time.Location parameter — the label is a civil date and does not need one.
//
// An unparseable label yields "", which matches no stored day and so declines
// to publish a yesterday. That is the safe direction.
func previousDay(label string) string {
	d, err := time.Parse(dayLayout, label)
	if err != nil {
		return ""
	}
	return d.AddDate(0, 0, -1).Format(dayLayout)
}

// firstInstantOf returns the earliest instant in (lo, hi] whose local date in
// loc is want, and reports whether it found one.
//
// This is a binary search on whole seconds rather than a call to time.Date,
// and that is the whole point of the design. time.Date's own documentation
// (go1.25 src/time/time.go:1722-1727) says that when a DST transition skips or
// repeats a local time, "the choice of time zone, and therefore the time, is
// not well-defined. Date returns a time that is correct in one of the two
// zones involved in the transition, but it does not guarantee which." Midnight
// is exactly such a time in several zones. In America/Havana,
// time.Date(2026, 3, 8, 0, 0, 0, 0, loc) returns 2026-03-07T23:00:00-05:00 —
// an instant whose date label is 2026-03-07, the day *before* the one asked
// for, because local midnight that night never happened.
//
// Searching for the first instant that *labels* as the new day has no such
// problem: it is the definition of the boundary, and it is exact whether
// midnight was skipped (Havana finds 01:00:00-04:00) or repeated (Havana on
// 2026-11-01 finds the earlier of the two midnights, which is the correct one).
//
// The search needs "label == want" to be false and then true across the
// interval, never flipping back. That is *not* a theorem about local dates,
// and an earlier version of this comment claimed it was. A zone that moves its
// clock backwards during the first minutes of a local day makes the predicate
// go true, false, true — and the bisection then lands on the second midnight
// rather than the first. Real tzdata contains such rules:
//
//   - America/Goose_Bay, America/Moncton and America/St_Johns ended DST at
//     00:01 local until 2010. In Goose Bay on 1987-10-25 the label runs
//     1987-10-25 (03:00:00Z), 1987-10-24 (03:01:00Z), 1987-10-25 (04:00:00Z).
//   - Antarctica/Casey stepped from +11 to +08 at 2010-03-04T15:00:00Z, which
//     is 02:00 on 2010-03-05 local, doing the same thing three hours wide.
//
// The narrower claim the code actually rests on is empirical: a scan of every
// UTC-offset transition in all 498 zones of tzdata 2026b, 1970 through 2050,
// finds 63 backwards label steps and every one of them is before 2011 — the
// latest are Goose Bay and St John's on 2010-11-07, under rules that no longer
// exist. Nothing this daemon can be handed is affected, and the two failing
// shapes are named above so the next reader can re-check the claim instead of
// re-deriving it. TestBoundarySearchMonotonicityIsEmpiricalNotAxiomatic pins
// both halves: that the historic counterexample is still there to point at,
// and that no zone which ever violated does so in the modern era.
//
// Second granularity is enough because every zone offset in tzdata is a whole
// number of seconds, so a day boundary always falls on one.
func firstInstantOf(want string, lo, hi time.Time, loc *time.Location) (time.Time, bool) {
	if dayLabel(hi, loc) != want {
		return time.Time{}, false
	}
	loSec, hiSec := lo.Unix(), hi.Unix()
	// Truncation moves lo no later, so it cannot cross forward into want's
	// day. If it already labels as want the interval does not straddle the
	// boundary at all — which happens when the clock steps backwards, since
	// then hi precedes lo.
	if hiSec <= loSec || dayLabel(time.Unix(loSec, 0), loc) == want {
		return time.Time{}, false
	}
	for hiSec-loSec > 1 {
		mid := loSec + (hiSec-loSec)/2
		if dayLabel(time.Unix(mid, 0), loc) == want {
			hiSec = mid
		} else {
			loSec = mid
		}
	}
	// The result is always inside (lo, hi]: hiSec ends at least one second
	// above lo's truncation, so it is strictly after lo, and it only ever
	// descends from hi's truncation, so it is never after hi.
	return time.Unix(hiSec, 0).In(loc), true
}

// lerp interpolates PPFD linearly to at, which is the same model of the
// interior that the trapezoid rule assumes.
func lerp(t0 time.Time, p0 float64, t1 time.Time, p1 float64, at time.Time) float64 {
	span := t1.Sub(t0)
	if span <= 0 {
		return p1
	}
	return p0 + (p1-p0)*(float64(at.Sub(t0))/float64(span))
}

// litFraction returns the fraction of an interval spent at or above threshold,
// assuming PPFD moves linearly between the endpoints.
//
// Using the same linear interior as the trapezoid is what keeps
// PhotoperiodHours consistent with DLI: a dawn ramp from 0 to 800 over one
// poll contributes the part of that poll spent above threshold, not all of it
// and not none of it.
func litFraction(p0, p1, threshold float64) float64 {
	lit0, lit1 := p0 >= threshold, p1 >= threshold
	switch {
	case lit0 && lit1:
		return 1
	case !lit0 && !lit1:
		return 0
	case lit1:
		// Rising through the threshold; p1 > p0 because they straddle it.
		return (p1 - threshold) / (p1 - p0)
	default:
		// Falling through it.
		return (p0 - threshold) / (p0 - p1)
	}
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
