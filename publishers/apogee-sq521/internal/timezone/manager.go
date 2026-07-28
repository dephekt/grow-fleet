package timezone

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Manager owns the effective timezone: it restores the persisted override at
// startup, applies reconciler writes, and publishes the live *time.Location to
// consumers.
//
// The zone is exposed through an atomic.Pointer rather than a getter behind the
// mutex so a consumer — the DLI accumulator deciding when local midnight falls
// — picks up a change on its next read, with no restart, no lock contention
// against the MQTT callback, and no chance of tearing.
type Manager struct {
	store *Store
	def   *time.Location
	log   *slog.Logger

	// loc is the effective zone: the override when one is in force, otherwise
	// def. Read on the hot path by consumers, written only by Apply and Start.
	loc atomic.Pointer[time.Location]

	// mu serialises Apply against itself and Start. The reconciler is a single
	// MQTT subscription so writes are already serial in practice, but state and
	// the disk write have to move together or a concurrent caller could echo a
	// value that never reached the file.
	mu sync.Mutex

	// state is the exact byte string last published to the retained state
	// topic: the override, or "" when the configured default is in force. It
	// records what is *applied*, which is not the same as what is on disk —
	// see persisted.
	state string

	// persisted reports whether the store agrees with state. It is false only
	// between a write that failed and a later one that succeeds.
	//
	// The two have to be tracked apart. Collapsing them into state alone means
	// "applied" and "applied and persisted" are indistinguishable, and since
	// Apply short-circuits on state matching, a write that failed once would
	// never be retried: every subsequent re-push of the same value would take
	// the short-circuit and return without touching the disk, leaving the
	// override missing for the life of the process even after the state
	// directory became writable again.
	persisted bool
}

// NewManager returns a Manager that falls back to def when no override is in
// force. A nil def means UTC; a nil logger discards.
//
// The returned Manager is usable immediately — Location reports def — but Start
// must run before the first MQTT connect so the reconciler never sees a state
// topic that contradicts the zone actually in effect.
func NewManager(store *Store, def *time.Location, log *slog.Logger) *Manager {
	if def == nil {
		def = time.UTC
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{store: store, def: def, log: log, persisted: true}
	m.loc.Store(def)
	return m
}

// Start restores and validates the persisted override.
//
// Call this synchronously, before the first MQTT connect. Doing so is what lets
// this implementation drop the wrote_before_setup_ flag the ESPHome component
// has to carry (timezone_text.cpp:51-55, 94-95, 129-130): there, a controller
// write can land before setup() runs and has to be prevented from being
// clobbered by stale flash contents. Here the file is read to completion before
// anything can subscribe, so no write can arrive first and the state published
// on connect is always the state in force.
//
// Start does not return an error, because there is no failure here worth
// refusing to boot over: a missing, unreadable, or invalid override all mean
// "run in the configured default zone", which is a zone the operator chose. An
// invalid stored value is deleted so the daemon does not re-log the same
// complaint on every restart (timezone_text.cpp:66-70). Everything is logged.
func (m *Manager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, err := m.store.Load()
	if err != nil {
		// persisted deliberately stays true here. Something is at the override
		// path that we cannot read, so we know the store does not agree with
		// the empty state we are about to run with — but the only write that
		// would reconcile them is a Clear, and speculatively deleting a path we
		// could not even read is not a repair this function should be making on
		// its own. The operator has the warning; an explicit empty push from
		// the reconciler will still clear it.
		m.log.Warn("timezone override unreadable; using default",
			"path", m.store.Path(), "default", m.def.String(), "error", err)
		return
	}
	if stored == "" {
		m.log.Debug("no timezone override stored; using default", "default", m.def.String())
		return
	}

	loc, err := Parse(stored)
	if err != nil {
		m.log.Warn("clearing invalid persisted timezone override",
			"value", stored, "default", m.def.String(), "error", err)
		if err := m.store.Clear(); err != nil {
			// The bad value is still on disk while we run with the default, so
			// the store does not agree with state. Recording that lets a later
			// empty push retry the removal instead of short-circuiting.
			m.persisted = false
			m.log.Warn("could not clear invalid timezone override", "error", err)
		}
		return
	}

	m.loc.Store(loc)
	m.state = stored
	m.log.Info("restored timezone override", "value", stored)
}

// Location returns the zone currently in effect. It is safe to call from any
// goroutine and cheap enough to call per use rather than caching.
func (m *Manager) Location() *time.Location {
	return m.loc.Load()
}

// State returns the exact bytes that belong on the retained state topic: the
// override in force, or "" when the configured default is.
func (m *Manager) State() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Apply handles one write from the reconciler and returns the exact bytes to
// publish to the retained state topic.
//
// The reconciler compares our retained state against the value it wants, so the
// return value must always be echoed, including when the write was rejected.
// Three cases, mirroring timezone_text.cpp:
//
//   - Unchanged value (timezone_text.cpp:80-85): acknowledge without parsing,
//     applying, or touching the disk. The reconciler re-pushes on every
//     reconnect and on its own schedule; without this short-circuit each one
//     would rewrite the SD card for no reason. The one exception is a value
//     whose earlier write failed, which is retried — see persisted.
//
//   - Empty value (timezone_text.cpp:73, 87-96): clear the override and revert
//     to the configured default, echoing "". This is what a fresh ESPHome node
//     publishes when it has no override, so a daemon and a node look identical
//     to the reconciler. Note that "" never equals a desired POSIX string, so
//     the reconciler pushes once more and its own loop guard holds until the
//     next reset. That is intended.
//
//   - Anything else: parse, and on success persist and apply. On failure re-echo
//     the currently effective bytes (timezone_text.cpp:105, 111, 120) so the
//     reconciler sees its write did not take rather than seeing its own value
//     reflected back and concluding we are in sync.
func (m *Manager) Apply(value string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if value == m.state {
		if m.persisted {
			return value
		}
		// Same value, already in force, but an earlier write did not land. The
		// conditions that cause that — a read-only remount, a full card — are
		// transient, and the reconciler hands us a free retry on every push, so
		// take it. Only the write is redone: there is nothing to re-parse and
		// nothing to re-apply, which is what keeps this off the Info log and
		// out of the way of the no-op short-circuit above.
		if err := m.persistLocked(value); err != nil {
			m.log.Warn("timezone state still not persisted; will retry on the next push",
				"value", value, "error", err)
		} else {
			m.log.Info("timezone state persisted on retry", "value", value)
		}
		return value
	}

	if value == "" {
		if err := m.persistLocked(""); err != nil {
			m.log.Warn("could not clear timezone override; will retry on the next push",
				"error", err)
		}
		m.loc.Store(m.def)
		m.state = ""
		m.log.Info("cleared timezone override; default restored", "default", m.def.String())
		return ""
	}

	loc, err := Parse(value)
	if err != nil {
		m.log.Warn("rejecting timezone override", "value", value, "error", err)
		return m.state
	}

	// Apply in memory even if the write fails, matching the ESPHome ordering
	// (timezone_text.cpp:123-127, where set_timezone precedes persist_ and
	// persist_ only warns). A read-only or full state directory should not stop
	// the daemon from honouring the zone it was just told to use.
	if err := m.persistLocked(value); err != nil {
		m.log.Warn("timezone override applied but not persisted; it will not survive a restart unless a later push retries the write successfully",
			"value", value, "error", err)
	}

	m.loc.Store(loc)
	m.state = value
	m.log.Info("applied timezone override", "value", value)
	return value
}

// persistLocked brings the store into line with value — Save for an override,
// Clear for the empty string — and records in persisted whether it got there.
// m.mu must be held.
//
// Routing both directions through one place is what keeps persisted honest: it
// is set on every path that touches the store, so it cannot drift out of step
// with what Apply believes about the disk.
func (m *Manager) persistLocked(value string) error {
	var err error
	if value == "" {
		err = m.store.Clear()
	} else {
		err = m.store.Save(value)
	}
	m.persisted = err == nil
	return err
}
