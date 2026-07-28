//go:build linux

package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/config"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/serialport"
)

// This is the only file in the package that touches the OS, which is why it
// carries the build tag rather than the package doing so: everything else —
// the probe ordering, the failure ladder, the scheduler, the birth sequence —
// compiles and is tested against the seams in seams.go with no hardware in
// sight.

// SerialDialer opens the Dr. Liu SDI-12->USB adapter and binds an SDI-12 client
// to it. It is the production [Dialer].
type SerialDialer struct {
	// Path is normally the /dev/serial/by-id symlink, so a reboot that reorders
	// ttyUSB numbering still finds the right adapter.
	Path string
	// Address is the sensor's SDI-12 bus address as an ASCII byte ('0'), not a
	// number.
	Address byte

	// ReadTimeout and WriteTimeout bound individual serial operations.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// SettleDelay overrides serialport's post-open wait. Zero selects the
	// package default (2.5 s, the adapter's measured MCU reset); negative skips
	// it, which is only ever right in a test.
	SettleDelay time.Duration

	Logger *slog.Logger
}

// NewSerialDialer builds the production dialer from the daemon's configuration.
func NewSerialDialer(cfg config.Config, log *slog.Logger) *SerialDialer {
	// config validates SDI12Address as exactly one character from [0-9a-zA-Z],
	// so this only guards against being handed an unvalidated Config. sdi12.New
	// would substitute the factory address for a zero byte anyway, but doing it
	// here keeps the substitution visible.
	addr := byte('0')
	if len(cfg.SDI12Address) == 1 {
		addr = cfg.SDI12Address[0]
	}
	return &SerialDialer{
		Path:         cfg.SDI12Port,
		Address:      addr,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		Logger:       log,
	}
}

// Dial opens the port and returns a Session bound to it.
//
// Failure is ordinary rather than fatal: USB enumeration lags a Pi reboot by
// seconds and systemd may well start us first, so the caller backs off and
// tries again. serialport.Open bounds its own settle on ctx, which is what
// keeps a SIGTERM arriving during those 2.5 s from having to wait them out.
//
// ctx also governs the session for the whole of its life, not just the open;
// see serialSession.watch.
func (d *SerialDialer) Dial(ctx context.Context) (Session, error) {
	port, err := serialport.Open(ctx, serialport.Options{
		Path:        d.Path,
		SettleDelay: d.SettleDelay,
		Logger:      d.Logger,
	})
	if err != nil {
		return nil, fmt.Errorf("app: dial %s: %w", d.Path, err)
	}
	s := &serialSession{
		Client: sdi12.New(port, d.Address, d.ReadTimeout, d.WriteTimeout),
		port:   port,
		closed: make(chan struct{}),
	}
	go s.watch(ctx)
	return s, nil
}

// serialSession pairs the client with the descriptor it frames bytes out of.
//
// They share one lifetime and must be replaced together: sdi12.LineReader
// retains bytes it pulled off the old fd, so a client kept across a reopen
// would start the new session mid-frame — the desync the whole package exists
// to prevent, reintroduced through the recovery path.
type serialSession struct {
	*sdi12.Client
	port *serialport.Port

	// closed stops the watcher when the session is released, so the goroutine's
	// lifetime is bounded by the shorter of "this session" and "this process".
	closed    chan struct{}
	closeOnce sync.Once
}

// watch shortens the port's deadline the moment ctx is cancelled.
//
// THE TRAP this closes: nothing else shortens a deadline that has already been
// set. sdi12 clamps each read to the caller's ctx.Deadline() but never selects
// on ctx.Done(), and the goroutine that would notice the cancellation is the
// one blocked in the read — it is the sole owner of the port, by design. So the
// stop latency is whatever deadline was in force when SIGTERM arrived, and that
// is set from the sensor's *declared* conversion time: ParseMeasureHeader
// accepts a ttt up to 999 s, and line noise parsing as <addr><3 digits><digit>
// declares one just as effectively as a real sensor does.
//
// On the shipping SQ-521 (ttt=001) the in-flight read is at most a couple of
// seconds, so this is robustness rather than a live defect — but it is the same
// class of fault as the Python's 36-second serial-open retry holding SIGTERM,
// which is the thing this daemon's every wait is shaped to avoid.
//
// Setting the deadline to now unblocks the pending read with a timeout, which
// serialport deliberately does not classify as a dead descriptor; the poll
// goroutine then sees ctx.Err() != nil at its next check and returns without
// publishing anything about it.
func (s *serialSession) watch(ctx context.Context) {
	select {
	case <-s.closed:
	case <-ctx.Done():
		// Best effort: a port closed in the same instant refuses this, which is
		// the outcome we wanted anyway.
		_ = s.port.SetDeadline(time.Now())
	}
}

func (s *serialSession) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return s.port.Close()
}

func (s *serialSession) String() string { return s.port.String() }
