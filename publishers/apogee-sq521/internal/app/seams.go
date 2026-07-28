package app

import (
	"context"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
)

// The seams. Everything this package needs from the outside world is one of the
// six interfaces below, so the whole orchestration — probe ordering, the failure
// ladder, absolute-boundary scheduling, watchdog staleness — is exercised
// without a serial port, a broker or a wall clock.
//
// Each interface is satisfied as-is by the production type it names, with no
// adapter: *sdi12.Client, *mqttpub.Publisher, *sdnotify.Notifier,
// *timezone.Manager and *dli.Accumulator. Session is the exception, because a
// Client does not own the fd it talks through; see serialDialer.

// Clock is the time source. Every wait in this package goes through it, which
// is what lets a test drive a day of polling in microseconds — and is why there
// is no time.Sleep anywhere in the binary.
type Clock interface {
	Now() time.Time

	// After behaves like [time.After]: it returns a channel that receives once
	// d has elapsed. A non-positive d must deliver immediately.
	//
	// The abandoned-timer case is normal here — every wait races After against
	// ctx.Done() — so an implementation must not block on delivery. The
	// production one is time.After, whose channel has capacity 1.
	After(d time.Duration) <-chan time.Time
}

// Session is one port session: an SDI-12 conversation bound to an open device.
//
// It exists because the *sdi12.Client and the *serialport.Port are two objects
// with one lifetime. ErrPortDead means the fd is beyond saving, and recovery is
// close-and-reopen — which produces a new Client as well, since its LineReader
// holds bytes framed off the old descriptor. Pairing them in one handle is what
// makes "the poll goroutine is the sole owner of the serial port" enforceable:
// there is exactly one value to own.
type Session interface {
	Identify(ctx context.Context) (sdi12.Identity, error)
	Measure(ctx context.Context, sub string) ([]float64, error)
	Verify(ctx context.Context) ([]float64, error)

	// MissedServiceRequests reports how often the adapter swallowed the
	// sensor's data-ready notification. A rising count is the signal that the
	// USB bridge, not the sensor, is dropping traffic; it is logged once per
	// port session rather than acted on.
	MissedServiceRequests() uint64

	// Close releases the device. Called by the poll goroutine and by nobody
	// else.
	Close() error

	// String names the device for the journal, e.g. "<by-id link> -> /dev/ttyUSB0".
	String() string
}

// Dialer opens a Session. It is called for the first open and again after every
// ErrPortDead, behind the poller's context-aware backoff.
type Dialer interface {
	Dial(ctx context.Context) (Session, error)
}

// Publisher is the MQTT surface, satisfied by *mqttpub.Publisher.
//
// Note what is *not* here: there is no "publish discovery on connect" hook that
// takes a payload. OnConnectionUp takes a bare func precisely so this package
// decides, at the moment the connection comes up, whether a discovery set
// exists yet — see App.onConnectionUp and the ordering constraint on
// entities.Snapshot.
type Publisher interface {
	Start(ctx context.Context) error
	PublishRetained(ctx context.Context, topic string, payload []byte) error
	Subscribe(ctx context.Context, topic string, cb func(topic string, payload []byte)) error
	OnConnectionUp(fn func())
	Shutdown(ctx context.Context, statusTopic string) error
}

// Notifier is the sd_notify(3) surface, satisfied by *sdnotify.Notifier. Every
// method is a no-op outside systemd, so nothing here is conditional.
type Notifier interface {
	Ready() error
	Stopping() error
	Watchdog() error
	Status(s string) error
	WatchdogInterval() (time.Duration, bool)
}

// Zones is the timezone surface, satisfied by *timezone.Manager.
type Zones interface {
	// Apply handles one reconciler write and returns the exact bytes to echo to
	// the retained state topic — including on rejection, where it re-echoes the
	// zone still in force.
	Apply(value string) string
	// State is the bytes that belong on the retained state topic right now.
	State() string
	// Location is the zone currently in effect.
	Location() *time.Location
}

// Integrator is the DLI surface, satisfied by *dli.Accumulator.
type Integrator interface {
	Add(t time.Time, ppfd float64)
	SetLocation(loc *time.Location, zone string)
	Snapshot() dli.Reading
	Flush() error
}

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) <-chan time.Time {
	if d <= 0 {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	return time.After(d)
}
