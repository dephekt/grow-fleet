//go:build linux

// Package serialport owns the file descriptor for the Dr. Liu SDI-12->USB
// bridge. It is the only OS-specific package in this daemon, and deliberately
// the only one that issues a syscall of its own.
//
// # Why not a serial library
//
// Neither go.bug.st/serial nor tarm/serial exposes a *write* deadline, and an
// unbounded serial write is the headline defect this rewrite exists to fix. The
// Python publisher called pyserial's write() with no write_timeout, so a wedged
// FTDI parked the poll loop forever — taking the MQTT keepalive and the systemd
// watchdog down with it, which is how a sensor that had stopped reporting still
// looked like a healthy unit. A port whose Write cannot time out is not a port
// this daemon can use, so the descriptor is opened and configured here by hand.
//
// # How the deadline is made to work
//
// [os.NewFile] on a descriptor that is *already* O_NONBLOCK registers it with
// the runtime netpoller: newFileFromNewFile reads F_GETFL and passes
// unix.HasNonblockFlag(flags) to newFile as its nonBlocking argument, which is
// what makes the descriptor pollable (os/file_unix.go:87-155, Go 1.25). A write
// that cannot drain then fails with EAGAIN, and internal/poll converts that into
// a waitWrite the deadline can interrupt (internal/poll/fd_unix.go:389-390). A
// descriptor the poller refused would instead fail SetDeadline outright with
// os.ErrNoDeadline (internal/poll/fd_poll_runtime.go:160) and degrade silently
// to blocking I/O — which is exactly the regression we cannot afford to ship
// unnoticed, so [Open] asserts that a deadline can be set before it returns.
//
// That whole chain hinges on the read path producing EAGAIN, which in turn
// hinges on VMIN; see the comment on [configure] for the one termios setting
// that would otherwise disable every deadline in the package.
//
// # Instance guard
//
// Three layers, because none alone is sufficient. [AcquireInstance] takes an
// abstract unix socket, [Open] takes flock and TIOCEXCL. See lock.go.
package serialport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultSettleDelay is how long [Open] waits before handing the port back.
//
// Opening the tty asserts DTR, which resets the Dr. Liu adapter's MCU; commands
// written during the reset are simply lost, and the Python publisher's first
// poll after every restart failed for exactly this reason. 2.5 s is the Python's
// measured value carried over unchanged.
const DefaultSettleDelay = 2500 * time.Millisecond

var (
	// ErrDeviceGone reports that the descriptor is no longer usable: the USB
	// device re-enumerated (EIO/ENODEV/ENXIO), the peer hung up (io.EOF), or the
	// Port has been closed. Only a close-and-reopen recovers.
	//
	// Callers classify it as sdi12.ErrPortDead. This package deliberately does
	// not import sdi12 to say so — the OS layer should not depend on the
	// protocol layer — but the two sentinels mean the same thing, and
	// [sdi12.LineReader] already maps every non-timeout read failure onto
	// ErrPortDead, so a Port error reaches the daemon correctly classified
	// whether it travels through the client or around it.
	ErrDeviceGone = errors.New("serialport: device gone")

	// ErrInUse reports that another instance of this daemon already holds the
	// resource: the abstract socket that [AcquireInstance] binds, or — once the
	// adapter has enumerated — the tty itself.
	//
	// On the device the usual source is EBUSY from our own open(2). A rival copy
	// of this daemon holds the tty in exclusive mode, TIOCEXCL is enforced inside
	// open(2), and so our open fails before we have a descriptor to flock. The
	// flock branch in [Open] reports the same sentinel but only fires for a
	// competitor that took flock *without* also taking TIOCEXCL, which no copy of
	// this daemon does; it is a backstop against a stranger, not the common case.
	ErrInUse = errors.New("serialport: already held by another instance")

	// ErrNoDeadlines reports that the kernel refused to register the descriptor
	// with the runtime netpoller, so reads and writes on it cannot time out.
	//
	// This is fatal at startup by design. A port without deadlines is a port
	// that can wedge the daemon indefinitely, which is the failure this whole
	// package exists to eliminate; failing the open converts it into a visible
	// unit that will not start, rather than an invisible one that has stopped.
	ErrNoDeadlines = errors.New("serialport: file descriptor does not support I/O deadlines")
)

// Seams for the three failure paths inside [Open] that no pty can produce.
//
// Each guards a property that only appears once something has already gone
// wrong: the kernel refusing to register the descriptor with the poller, TCSETS
// failing on a tty that had just accepted TIOCEXCL, or a sibling instance
// racing us between its flock and its own TIOCEXCL. None is reachable by
// pointing [Open] at a real path — a descriptor unpollable enough to matter
// (a regular file) dies at TIOCEXCL with ENOTTY a dozen lines earlier — so
// called directly they are guards no test can exercise, and a guard no test can
// exercise is one the next refactor deletes with CI staying green. Substituting
// these is how the tests reach them; nothing in production assigns to them.
var (
	setWriteDeadline = (*os.File).SetWriteDeadline
	configureFD      = configure
	flockFD          = unix.Flock
)

// Options configures [Open].
type Options struct {
	// Path is the device to open, normally the /dev/serial/by-id symlink so a
	// reboot that reorders ttyUSB numbering still finds the right adapter.
	Path string

	// SettleDelay overrides the post-open wait. Zero selects
	// [DefaultSettleDelay]; a negative value skips the wait entirely, which is
	// only useful in tests — on real hardware the adapter is still resetting.
	SettleDelay time.Duration

	// Logger receives the one line that records which symlink resolved to which
	// device node. Nil uses [slog.Default].
	Logger *slog.Logger
}

// settleDelay resolves the documented zero/negative conventions.
func (o Options) settleDelay() time.Duration {
	switch {
	case o.SettleDelay == 0:
		return DefaultSettleDelay
	case o.SettleDelay < 0:
		return 0
	default:
		return o.SettleDelay
	}
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// Port is an open serial device with working I/O deadlines.
//
// It satisfies sdi12.Transport. Like the client that drives it, a Port is not
// safe for concurrent use by multiple goroutines: SDI-12 is a half-duplex
// request/response bus, so exactly one goroutine owns the port for the duration
// of a transaction. [Port.Close] is the exception — it is idempotent and may be
// called from a shutdown path.
type Port struct {
	f *os.File

	// link is Options.Path as configured; path is what it resolved to. They
	// differ whenever the configured path is a by-id symlink, which is the
	// normal case.
	link string
	path string

	// closed is the package's own answer to "has this been released", rather
	// than an inference from whatever error the standard library happens to
	// return. That inference does not hold: os.File.Read and Write route their
	// failures through wrapErr, which maps internal/poll's ErrFileClosing onto
	// os.ErrClosed, but SetDeadline and SyscallConn().Control return
	// ErrFileClosing unconverted — same message, different error value, not
	// matched by errors.Is(err, os.ErrClosed). Relying on that split would make
	// half the methods classify a closed port as ErrDeviceGone and the other
	// half not, on the strength of an unexported stdlib detail. Checking a flag
	// we own makes every entry point agree.
	closed atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

// Open opens and configures the serial device named by opts.Path.
//
// The sequence is: resolve the symlink, open non-blocking, take the two
// kernel-level exclusion locks, apply the line settings, hand the descriptor to
// os.NewFile, assert that deadlines work, then wait out the adapter's reset.
// Every failure after the descriptor exists closes it, so a failed Open never
// leaks an fd or leaves the tty locked against the next attempt.
//
// ctx bounds the settle delay and is checked before any work begins; it does not
// bound the syscalls themselves, which are non-blocking or near-instant.
func Open(ctx context.Context, opts Options) (*Port, error) {
	if opts.Path == "" {
		return nil, errors.New("serialport: open: no device path configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("serialport: open %s: %w", opts.Path, err)
	}

	// Resolve before opening so the journal records which physical adapter this
	// run is talking to. A swapped or re-enumerated device changes the target
	// while the by-id link stays the same, and that difference is otherwise
	// invisible in the logs.
	resolved, err := filepath.EvalSymlinks(opts.Path)
	if err != nil {
		// Includes the ordinary "USB has not enumerated yet" case a few seconds
		// after boot. The caller retries; this is not a configuration error.
		return nil, fmt.Errorf("serialport: resolve %s: %w", opts.Path, err)
	}

	// O_NONBLOCK is load-bearing twice over: it stops open(2) itself from
	// blocking on a modem-control line that never asserts, and it is what makes
	// os.NewFile register the descriptor with the netpoller further down.
	// O_NOCTTY keeps the tty from becoming this process's controlling terminal,
	// which would route the sensor's line noise into our signal disposition.
	fd, err := unix.Open(resolved, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		// EBUSY here is TIOCEXCL, enforced by the tty layer inside open(2), and
		// it is where a second instance of this daemon actually gets turned
		// away: the other copy set the flag, so our open fails before we have a
		// descriptor to flock and the flock branch below never runs. Say so with
		// the sentinel — an operator seeing a bare "device or resource busy"
		// learns nothing about which of the three guards fired, and the daemon
		// cannot tell "someone else has it" from "the adapter is broken".
		if errors.Is(err, unix.EBUSY) {
			return nil, fmt.Errorf("serialport: open %s (%s): %w: %w", resolved, opts.Path, ErrInUse, err)
		}
		return nil, fmt.Errorf("serialport: open %s (%s): %w", resolved, opts.Path, err)
	}

	// From here until os.NewFile takes ownership, every error path closes fd.
	//
	// exclusive tracks whether TIOCEXCL has been applied, because past that
	// point closing is not enough: exclusive mode is a property of the tty, not
	// of our descriptor, and on any device whose tty outlives our close (see
	// Close) a bare close would leave it locked against the next attempt.
	//
	// It is deliberately conditional rather than unconditional, because TIOCEXCL
	// is one flag on the shared tty and not a property of our descriptor: an
	// unconditional clear would release whoever's flag happens to be set, not
	// ours. The flock failure path below can race a *sibling* instance that has
	// just taken flock and not yet reached its own TIOCEXCL, and clearing on the
	// way past would knock out that instance's layer-3 guard — the one that
	// keeps minicom out. TestFailedFlockLeavesASiblingsExclusiveModeAlone drives
	// exactly that interleaving.
	var exclusive bool
	fail := func(format string, args ...any) (*Port, error) {
		if exclusive {
			_ = unix.IoctlSetInt(fd, unix.TIOCNXCL, 0)
		}
		_ = unix.Close(fd)
		return nil, fmt.Errorf("serialport: "+format, args...)
	}

	// Layer 2 of the instance guard. flock keys on the inode, so it holds even
	// if the other instance opened /dev/ttyUSB0 while we opened the by-id link.
	// It is taken before TIOCEXCL on purpose: if another instance already owns
	// the port we must not set the exclusive-mode flag on the way out, because
	// TIOCEXCL lives on the tty rather than on our descriptor and clearing it is
	// our job (see Close). TestFlockIsTakenBeforeExclusiveMode pins the order.
	if err := flockFD(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return fail("flock %s: %w", resolved, ErrInUse)
		}
		return fail("flock %s: %w", resolved, err)
	}

	// Layer 3. flock is advisory and nothing else on this Pi takes it, so
	// TIOCEXCL is what actually stops minicom, screen, picocom or a forgotten
	// copy of the old Python publisher: their plain open(2) fails with EBUSY.
	if err := unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); err != nil {
		return fail("TIOCEXCL %s: %w", resolved, err)
	}
	exclusive = true

	if err := configureFD(fd); err != nil {
		return fail("configure %s as raw 9600 8N1: %w", resolved, err)
	}

	// The descriptor is already O_NONBLOCK, so this registers it with the
	// runtime netpoller and deadlines become possible. Ownership of fd passes to
	// f here; do not close fd directly past this point.
	f := os.NewFile(uintptr(fd), resolved)
	if f == nil {
		return fail("os.NewFile rejected fd %d for %s", fd, resolved)
	}

	p := &Port{f: f, link: opts.Path, path: resolved}

	if err := p.assertDeadlines(); err != nil {
		_ = p.Close()
		return nil, err
	}

	opts.logger().LogAttrs(ctx, slog.LevelInfo, "serial port opened",
		slog.String("link", opts.Path),
		slog.String("device", resolved),
	)

	if err := settle(ctx, opts.settleDelay()); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("serialport: %s: settling after open: %w", resolved, err)
	}

	// The DTR-triggered MCU reset can leave a few bytes of adapter banner in the
	// receive queue. The client flushes at the head of every transaction anyway,
	// so this is belt and braces — but it means the first thing in the buffer
	// after Open returns is the sensor, not the bridge.
	if err := p.FlushInput(); err != nil {
		_ = p.Close()
		return nil, err
	}

	return p, nil
}

// configure puts the descriptor into raw 9600 8N1, the SQ-521's only supported
// line setting.
//
// Oflag and Lflag are left at zero deliberately: no OPOST (which would translate
// our '\n' on the way out), no ICANON (which would buffer by line and let the
// sensor's CR/LF framing be reinterpreted by the tty layer), no ECHO (which on a
// half-duplex bus would put our own commands back on the wire), no ISIG.
//
// CLOCAL ignores DCD, which the adapter does not drive; without it, reads block
// waiting for a carrier that never appears. IGNPAR discards characters with
// parity or framing errors rather than letting the tty layer inject a marker
// byte into the middle of a response.
//
// VMIN is 1, not 0 — the one place this deviates from a naive "raw mode" recipe,
// and the deviation is mandatory. In N_TTY, VMIN==0 && VTIME==0 selects a
// polling read: n_tty_read sets its timeout to zero and returns *0 bytes* when
// the queue is empty, before it ever consults O_NONBLOCK. os.NewFile sets
// ZeroReadIsEOF, so every read of a quiet bus would come back as io.EOF —
// indistinguishable from a USB unplug, immediately classified as ErrPortDead,
// and the daemon would reopen the adapter (a 2.5 s reset) on every silent poll.
// The deadline machinery would never engage at all, because internal/poll only
// parks on EAGAIN. With VMIN=1 the same read returns EAGAIN, the poller waits,
// and the deadline fires exactly as intended.
//
// This is not a stylistic preference and it is not safe to "correct" back to
// the recipe. Setting VMIN=0 here and running the suite fails eight tests —
// TestReadDeadlineFires, TestIdleReadTimesOutRatherThanReportingEOF,
// TestRawModeDoesNotTranslateBytes, TestTermiosRoundTripsOnAPty,
// TestFlushInputDiscardsPendingBytes, TestOpenDiscardsWhatWasQueuedBeforeIt,
// TestDeadlineSelfTestLeavesNoDeadlineBehind and TestPortDrivesSDI12Client —
// measured on Linux 7.0 against a pty. Re-measure this list if you add a test
// that reads: it is here so a skeptic can reproduce the claim, and a count that
// does not match is a reason to doubt the whole paragraph. If a
// specification, a review comment or a serial-port tutorial tells you VMIN
// should be 0, it is wrong about this descriptor; read this paragraph again
// before believing it.
//
// VTIME stays 0: it is an inter-character timer measured in tenths of a second
// and only consulted once VMIN characters are pending, which is a coarser and
// redundant version of the deadline we already own.
func configure(fd int) error {
	t := unix.Termios{
		Cflag: unix.B9600 | unix.CS8 | unix.CREAD | unix.CLOCAL,
		Iflag: unix.IGNPAR,
		Oflag: 0,
		Lflag: 0,
	}
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	// TCSETS rather than TCSETSW/TCSETSF: there is nothing queued to drain on a
	// descriptor we opened microseconds ago, and TCSETS applies immediately.
	// The baud lives in the CBAUD bits of Cflag; Ispeed/Ospeed are ignored by
	// this ioctl and stay zero.
	return unix.IoctlSetTermios(fd, unix.TCSETS, &t)
}

// assertDeadlines is the startup self-test, and the single most important call
// in this package.
//
// Everything above only *arranges* for the descriptor to be pollable. Whether
// epoll actually accepted this particular tty is a property of the driver, and
// the failure mode if it did not is silent: SetDeadline returns ErrNoDeadline,
// callers that ignore it get blocking I/O back, and the daemon regresses to the
// exact hang the rewrite was for. Asking the question once, loudly, at startup
// turns that into a unit that refuses to start.
//
// The call site in [Open] is the load-bearing part, and it is reachable only
// through the setWriteDeadline seam — see TestOpenFailsWhenNoDeadlineCanBeSet.
func (p *Port) assertDeadlines() error {
	// The write side, because the write deadline is the one with no alternative
	// implementation.
	//
	// selfTestInstant is deliberately in the past. Pollability is decided before
	// the value is looked at — internal/poll's setDeadlineImpl returns
	// ErrNoDeadline on a zero runtimeCtx whatever t is
	// (internal/poll/fd_poll_runtime.go:146-165, Go 1.25) — so an expired
	// deadline detects a refused descriptor exactly as well as a distant one,
	// and it makes the clear below load-bearing in a way an hour could never be.
	// Drop the clear with an hour on the descriptor and the port carries a
	// silent 60-minute write budget that no test can tell from none; drop it
	// with this and the very first write fails instantly and visibly, at
	// startup. TestDeadlineSelfTestLeavesNoDeadlineBehind is that test.
	if err := setWriteDeadline(p.f, selfTestInstant); err != nil {
		if errors.Is(err, os.ErrNoDeadline) {
			return fmt.Errorf("serialport: %s: %w: the kernel would not add it to the poller, so a wedged write could never be interrupted",
				p.path, ErrNoDeadlines)
		}
		return fmt.Errorf("serialport: %s: deadline self-test: %w", p.path, err)
	}
	if err := p.f.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("serialport: %s: clearing self-test deadline: %w", p.path, err)
	}
	return nil
}

// selfTestInstant is the already-expired deadline [Port.assertDeadlines] sets.
// Any instant comfortably in the past does; this one is recognisable in a strace
// and cannot be mistaken for a budget somebody meant.
var selfTestInstant = time.Unix(1, 0)

// settle waits d, or returns as soon as ctx ends.
//
// time.Sleep would be wrong here: this is 2.5 s on the startup path and on every
// reopen, and a SIGTERM arriving during it must not be held up until it expires.
func settle(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Link returns the path as configured, which is normally a by-id symlink.
func (p *Port) Link() string { return p.link }

// Path returns the device node the configured path resolved to.
func (p *Port) Path() string { return p.path }

// String renders the port as "<link> -> <device>", or just the device when the
// configured path was not a symlink.
func (p *Port) String() string {
	if p.link == p.path {
		return p.path
	}
	return p.link + " -> " + p.path
}

// Read implements io.Reader. It returns after the first available bytes; short
// reads are normal and the caller frames lines out of the stream.
func (p *Port) Read(b []byte) (int, error) {
	if err := p.check("read"); err != nil {
		return 0, err
	}
	n, err := p.f.Read(b)
	return n, p.classify(err)
}

// Write implements io.Writer.
//
// os.File.Write already loops over partial writes, so a returned error means the
// remaining bytes could not be handed to the kernel at all. A deadline expiry
// passes through as os.ErrDeadlineExceeded rather than becoming ErrDeviceGone:
// the two are different diagnoses (a full FTDI transmit buffer versus a missing
// device), and the sdi12 client already treats every write failure as fatal to
// the port, so nothing is lost by keeping the distinction visible in the log.
func (p *Port) Write(b []byte) (int, error) {
	if err := p.check("write"); err != nil {
		return 0, err
	}
	n, err := p.f.Write(b)
	return n, p.classify(err)
}

// SetDeadline bounds both subsequent reads and writes. A zero t clears it.
func (p *Port) SetDeadline(t time.Time) error {
	if err := p.check("set deadline"); err != nil {
		return err
	}
	return p.classify(p.f.SetDeadline(t))
}

// SetReadDeadline bounds subsequent reads only.
func (p *Port) SetReadDeadline(t time.Time) error {
	if err := p.check("set read deadline"); err != nil {
		return err
	}
	return p.classify(p.f.SetReadDeadline(t))
}

// SetWriteDeadline bounds subsequent writes only.
func (p *Port) SetWriteDeadline(t time.Time) error {
	if err := p.check("set write deadline"); err != nil {
		return err
	}
	return p.classify(p.f.SetWriteDeadline(t))
}

// FlushInput discards bytes sitting in the kernel receive queue.
//
// The ioctl goes through SyscallConn().Control rather than f.Fd() for one
// reason: Control increfs the descriptor and holds the reference for the
// duration of the call (internal/poll/fd_posix.go:56-63), so the ioctl cannot
// race a concurrent Close into a descriptor number the kernel has already
// handed to something else. Fd() hands out a bare int with no such guarantee.
//
// Not, despite what os.File.Fd's own documentation warns, because Fd() would
// revert the descriptor to blocking and undo every deadline in this package.
// That applies only when Go itself made the descriptor non-blocking: File.fd()
// reverts iff the f.nonblock flag is set (os/file_unix.go:69-84), and newFile
// sets it only for a descriptor it called SetNonblock on, or for kindSock. This
// one was already O_NONBLOCK before os.NewFile ever saw it, so the flag stays
// false and Fd() leaves it alone — which the standard library states outright:
// "passing a nonblocking descriptor to NewFile should remain nonblocking if you
// get it back using Fd" (os/file_unix.go:102-106). Measured on Go 1.25:
// O_NONBLOCK reads back true either side of the call and read deadlines still
// fire. The incref is the whole argument; the blocking-mode hazard is one this
// package does not have.
func (p *Port) FlushInput() error {
	if err := p.check("flush input"); err != nil {
		return err
	}
	err := p.control(func(fd int) error {
		return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
	})
	if err != nil {
		return fmt.Errorf("serialport: %s: flush input: %w", p.path, err)
	}
	return nil
}

// Close releases the descriptor. It is safe to call more than once and safe to
// call from a different goroutine than the one doing I/O; repeat calls return
// the first call's result rather than "file already closed".
//
// The flock is released by close(2) itself, because it is held by the open file
// description. TIOCEXCL is cleared explicitly, which is a small deviation from
// "the kernel does it for you": the tty layer only resets the flag when the
// tty_struct is destroyed, which for a USB serial device coincides with our
// close but for a pty (the shape of every test in this package) does not, since
// the master keeps the structure alive. Rather than have Close mean two
// different things depending on the device behind it, ask for the release.
func (p *Port) Close() error {
	p.closeOnce.Do(func() {
		// Published before the descriptor goes away, so a racing Read or Write
		// is turned back by check — with this package's own message naming the
		// operation — rather than by os.File a layer lower.
		//
		// This buys a consistent diagnosis, not memory safety: pfd.incref fails
		// once Close has run, so neither ordering can let a caller reach a
		// descriptor number the kernel has reused, and both end up wrapping
		// ErrDeviceGone (os.ErrClosed is in deviceGone's set). Deliberately
		// untested for that reason — the two orderings differ only in the
		// message, and the window between them is too narrow to steer a test
		// into. Do not read the ordering as load-bearing; it is hygiene.
		p.closed.Store(true)
		// Best effort, and deliberately not reported: we are on the way out, the
		// flag dies with the tty on real hardware anyway, and a failure here
		// must not mask a close(2) error. This calls control directly because
		// check would now (correctly) refuse.
		_ = p.control(func(fd int) error {
			return unix.IoctlSetInt(fd, unix.TIOCNXCL, 0)
		})
		p.closeErr = p.f.Close()
	})
	return p.closeErr
}

// check turns use-after-Close into ErrDeviceGone. See the closed field for why
// this is not left to the standard library's error values.
func (p *Port) check(op string) error {
	if p.closed.Load() {
		return fmt.Errorf("serialport: %s: %s after Close: %w", p.path, op, ErrDeviceGone)
	}
	return nil
}

// control runs fn on the raw descriptor while the runtime holds a reference to
// it. See FlushInput for why f.Fd() is never used instead.
func (p *Port) control(fn func(fd int) error) error {
	rc, err := p.f.SyscallConn()
	if err != nil {
		return p.classify(err)
	}
	var inner error
	if err := rc.Control(func(fd uintptr) { inner = fn(int(fd)) }); err != nil {
		return p.classify(err)
	}
	return p.classify(inner)
}

// classify maps a raw os/syscall failure onto this package's sentinels.
//
// The one distinction that matters is deadline-versus-dead. On a silent bus a
// read timeout is the ordinary way to learn that nobody answered, and the client
// turns it into a skipped poll; calling that a dead port would make the daemon
// reopen the adapter — a 2.5 s reset — every time the sensor was merely busy.
// So a timeout is passed through untouched and only genuine descriptor failures
// become ErrDeviceGone.
func (p *Port) classify(err error) error {
	switch {
	case err == nil:
		return nil
	// Redundant with default, and kept anyway: deviceGone already answers false
	// for a deadline, so deleting this case changes no behaviour and no test.
	// It stays because it states the package's most consequential rule where the
	// rule is applied, and because it pins the answer here rather than leaving
	// it as a property of deviceGone's errno list that a later addition to that
	// list could quietly overturn.
	case errors.Is(err, os.ErrDeadlineExceeded):
		return err
	case deviceGone(err):
		return fmt.Errorf("serialport: %s: %w: %w", p.path, ErrDeviceGone, err)
	default:
		return err
	}
}

// deviceGoneErrnos are the errno values a yanked or re-enumerated USB serial
// adapter produces. EBADF is included for the descriptor this package could not
// itself have closed — a double close elsewhere, or a runtime bug — because the
// recovery is identical either way.
var deviceGoneErrnos = []error{unix.EIO, unix.ENODEV, unix.ENXIO, unix.EBADF}

// deviceGone reports whether err says the descriptor is beyond saving.
//
// io.EOF counts. os.NewFile sets ZeroReadIsEOF, and a zero-length read on a tty
// configured with VMIN=1 can only mean a hangup — the peer went away — never
// "nothing to say right now", which is EAGAIN and is handled by the poller.
// os.ErrClosed counts for the obvious reason: someone already closed the Port.
func deviceGone(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
		return true
	}
	for _, errno := range deviceGoneErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
