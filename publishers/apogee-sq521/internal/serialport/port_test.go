//go:build linux

package serialport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdi12"
)

// A *Port is what the daemon hands to sdi12.New, so the interface has to fit.
// The assertion lives in the test rather than in port.go so the production
// import graph keeps pointing one way: the OS layer knows nothing about the
// protocol layer, but breaking the contract still fails the build.
var _ sdi12.Transport = (*Port)(nil)

// ---------------------------------------------------------------------------
// pty harness
//
// Every test here runs against a pty rather than the FTDI adapter, which is a
// real but bounded compromise. What a pty reproduces faithfully (verified, not
// assumed — see TestTermiosRoundTripsOnAPty): TCSETS accepts the full cflag
// including the baud bits and reads back byte for byte, raw mode really is raw
// in both directions, TCFLSH drops queued input, TIOCEXCL rejects a second
// open, flock behaves, and — the point of the exercise — the descriptor is
// pollable, so deadlines fire on a blocked write.
//
// What it does not reproduce: there is no wire, so the baud rate has no
// observable effect (a pty moves bytes at memory speed), and no modem control
// lines, so CLOCAL and the DTR toggle that resets the Dr. Liu adapter are
// inert. Those are exactly the settings that cannot be tested without hardware;
// they are asserted structurally by reading the termios back instead.
//
// One behaviour differs outright and the package accommodates it: TIOCEXCL
// survives our close on a pty, because the master keeps the tty_struct alive,
// whereas on a USB serial device our close is the last one and destroys it. See
// Port.Close and TestCloseReleasesExclusiveModeOnAPty.
// ---------------------------------------------------------------------------

// newPTY returns the master side of a fresh pty and the path of its slave. The
// slave stands in for the serial device; the master stands in for the sensor.
func newPTY(t *testing.T) (master *os.File, slavePath string) {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("/dev/ptmx is unavailable (%v); these tests need a pty to stand in for the serial device", err)
	}
	// TIOCSPTLCK 0 unlocks the slave; without it the open below fails EIO.
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(mfd)
		t.Skipf("TIOCSPTLCK on /dev/ptmx failed (%v); no usable pty here", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(mfd)
		t.Skipf("TIOCGPTN on /dev/ptmx failed (%v); no usable pty here", err)
	}
	// The master is opened O_NONBLOCK too, so it is poller-managed and the test
	// can put deadlines on its own reads instead of hanging the suite.
	m := os.NewFile(uintptr(mfd), "/dev/ptmx")
	t.Cleanup(func() { _ = m.Close() })
	return m, fmt.Sprintf("/dev/pts/%d", n)
}

// testLogger keeps Open's one info line out of the test output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// swap installs v in *p for the duration of the test and puts the old value
// back afterwards. Its only use is the seam vars in port.go — the failure paths
// inside Open that a pty cannot be made to produce. Nothing here runs in
// parallel, so a package-level swap is safe.
func swap[T any](t *testing.T, p *T, v T) {
	t.Helper()
	prev := *p
	*p = v
	t.Cleanup(func() { *p = prev })
}

// openPort opens path with the settle delay disabled — 2.5 s per test would
// dominate the suite, and nothing here is testing the adapter's MCU reset.
func openPort(t *testing.T, path string) *Port {
	t.Helper()
	p, err := Open(context.Background(), Options{Path: path, SettleDelay: -1, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// inputQueued reports TIOCINQ, the number of bytes the tty layer is holding for
// us. It makes the flush tests deterministic instead of sleep-and-hope.
func inputQueued(t *testing.T, p *Port) int {
	t.Helper()
	var n int
	if err := p.control(func(fd int) error {
		var err error
		n, err = unix.IoctlGetInt(fd, unix.TIOCINQ)
		return err
	}); err != nil {
		t.Fatalf("TIOCINQ: %v", err)
	}
	return n
}

// waitForInput blocks until the tty layer has queued at least one byte. Data
// crosses a pty through a workqueue, so a write on the master is not instantly
// visible as TIOCINQ on the slave.
func waitForInput(t *testing.T, p *Port) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inputQueued(t, p) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no bytes reached the port's receive queue within 5s")
}

// ---------------------------------------------------------------------------
// The headline test
// ---------------------------------------------------------------------------

// TestWriteDeadlineFiresOnABlockedWrite is the reason this package exists.
//
// The pty's slave-to-master buffer is a few tens of kilobytes; with nothing
// reading the master it fills, and the next write has to park. Under the defect
// this rewrite fixes it parks forever. Here it must come back with
// os.ErrDeadlineExceeded, and it must come back *late* — an immediate failure
// would prove only that a stale deadline is honoured, not that a genuinely
// blocked write can be interrupted.
func TestWriteDeadlineFiresOnABlockedWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a pty buffer and waits out a deadline")
	}
	master, slave := newPTY(t)
	_ = master // deliberately never read from: that is what wedges the write

	p := openPort(t, slave)

	const budget = 750 * time.Millisecond
	if err := p.SetDeadline(time.Now().Add(budget)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	type outcome struct {
		total   int
		elapsed time.Duration
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		buf := make([]byte, 32*1024)
		start := time.Now()
		var total int
		for {
			n, err := p.Write(buf)
			total += n
			if err != nil {
				done <- outcome{total, time.Since(start), err}
				return
			}
			// A pty that swallowed this much without ever blocking would mean
			// the buffer is unbounded and the test proves nothing; bail rather
			// than spin.
			if total > 64<<20 {
				done <- outcome{total, time.Since(start), nil}
				return
			}
		}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatalf("wrote %d bytes without ever blocking; the pty buffer is not bounded, so this test cannot prove anything", got.total)
		}
		if !errors.Is(got.err, os.ErrDeadlineExceeded) {
			t.Fatalf("write stopped after %d bytes with %v; want os.ErrDeadlineExceeded", got.total, got.err)
		}
		// Half the budget is a generous floor: the buffer fills in microseconds,
		// so the overwhelming majority of the elapsed time is the parked write.
		if got.elapsed < budget/2 {
			t.Errorf("write returned the deadline error after only %v (budget %v) having written %d bytes; "+
				"it never actually blocked, so the poller path is untested", got.elapsed, budget, got.total)
		}
		t.Logf("blocked write interrupted after %v having written %d bytes", got.elapsed.Round(time.Millisecond), got.total)
	case <-time.After(budget + 15*time.Second):
		// This is the failure the package exists to prevent: the descriptor is
		// not registered with the netpoller, so the deadline is inert and the
		// write is unbounded.
		t.Fatal("write blocked past its deadline: the fd is not poller-managed and serial writes are unbounded")
	}
}

// ---------------------------------------------------------------------------
// Deadlines, reads and framing
// ---------------------------------------------------------------------------

// TestReadDeadlineFires covers the ordinary case: the sensor said nothing.
func TestReadDeadlineFires(t *testing.T) {
	_, slave := newPTY(t)
	p := openPort(t, slave)

	const budget = 200 * time.Millisecond
	if err := p.SetDeadline(time.Now().Add(budget)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	start := time.Now()
	n, err := p.Read(make([]byte, 32))
	elapsed := time.Since(start)

	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read on a quiet port: n=%d err=%v; want os.ErrDeadlineExceeded", n, err)
	}
	if elapsed < budget/2 {
		t.Errorf("Read returned after %v, well before the %v budget", elapsed, budget)
	}
}

// TestIdleReadTimesOutRatherThanReportingEOF pins the VMIN=1 decision.
//
// With VMIN=0/VTIME=0 — the setting a "raw mode" recipe hands you — N_TTY makes
// read(2) return zero bytes on an empty queue instead of EAGAIN, os.NewFile's
// ZeroReadIsEOF turns that into io.EOF, and this package would report every
// quiet moment on a silent bus as a dead port. The daemon would then reopen the
// adapter (2.5 s of MCU reset) on every poll where the sensor had nothing to
// say. Guard the distinction explicitly, because both settings "work" right up
// until the bus goes quiet.
func TestIdleReadTimesOutRatherThanReportingEOF(t *testing.T) {
	_, slave := newPTY(t) // master stays open: this is a live port with no traffic
	p := openPort(t, slave)

	if err := p.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	_, err := p.Read(make([]byte, 32))
	if errors.Is(err, io.EOF) || errors.Is(err, ErrDeviceGone) {
		t.Fatalf("idle read reported %v; a quiet bus must not look like a hangup (VMIN is probably 0)", err)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("idle read: %v; want os.ErrDeadlineExceeded", err)
	}
}

// TestRawModeDoesNotTranslateBytes asserts the tty layer is out of the way in
// both directions: no ICRNL/INLCR on the way in, no OPOST/ONLCR on the way out,
// no echo looping our own commands back onto a half-duplex bus. SDI-12 framing
// is CR/LF-terminated, so any translation would corrupt every response.
func TestRawModeDoesNotTranslateBytes(t *testing.T) {
	master, slave := newPTY(t)
	p := openPort(t, slave)

	// The bytes that would move under the settings we turned off, plus a
	// high-bit byte that PARMRK/ISTRIP would mangle.
	payload := []byte("0+123.45\r\n\xff")

	if _, err := master.Write(payload); err != nil {
		t.Fatalf("master write: %v", err)
	}
	if got := readN(t, p, len(payload)); !bytes.Equal(got, payload) {
		t.Errorf("sensor -> port: got %q, want %q", got, payload)
	}

	if _, err := p.Write(payload); err != nil {
		t.Fatalf("port write: %v", err)
	}
	if got := readNFile(t, master, len(payload)); !bytes.Equal(got, payload) {
		t.Errorf("port -> sensor: got %q, want %q", got, payload)
	}

	// Nothing should have come back: ECHO off means our own write did not
	// reappear in the receive queue.
	if err := p.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if n, err := p.Read(make([]byte, 32)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("after writing, port read returned n=%d err=%v; the tty is echoing our commands", n, err)
	}
}

// TestTermiosRoundTripsOnAPty reads the line settings back so the pieces that
// have no observable effect without real hardware — the baud bits, CLOCAL — are
// still asserted rather than assumed.
func TestTermiosRoundTripsOnAPty(t *testing.T) {
	_, slave := newPTY(t)
	p := openPort(t, slave)

	var got *unix.Termios
	if err := p.control(func(fd int) error {
		var err error
		got, err = unix.IoctlGetTermios(fd, unix.TCGETS)
		return err
	}); err != nil {
		t.Fatalf("TCGETS: %v", err)
	}

	wantCflag := uint32(unix.B9600 | unix.CS8 | unix.CREAD | unix.CLOCAL)
	if got.Cflag != wantCflag {
		t.Errorf("Cflag = %#x, want %#x", got.Cflag, wantCflag)
	}
	if got.Iflag != uint32(unix.IGNPAR) {
		t.Errorf("Iflag = %#x, want %#x (IGNPAR only)", got.Iflag, unix.IGNPAR)
	}
	if got.Oflag != 0 {
		t.Errorf("Oflag = %#x, want 0 (no output post-processing)", got.Oflag)
	}
	if got.Lflag != 0 {
		t.Errorf("Lflag = %#x, want 0 (no ICANON/ECHO/ISIG)", got.Lflag)
	}
	if got.Cc[unix.VMIN] != 1 {
		t.Errorf("VMIN = %d, want 1; see configure() — VMIN=0 disables every deadline in this package", got.Cc[unix.VMIN])
	}
	if got.Cc[unix.VTIME] != 0 {
		t.Errorf("VTIME = %d, want 0", got.Cc[unix.VTIME])
	}
}

// ---------------------------------------------------------------------------
// FlushInput
// ---------------------------------------------------------------------------

func TestFlushInputDiscardsPendingBytes(t *testing.T) {
	master, slave := newPTY(t)
	p := openPort(t, slave)

	// Stand-in for a response we abandoned last poll and must not mistake for
	// this poll's answer.
	if _, err := master.Write([]byte("0+999.99\r\n")); err != nil {
		t.Fatalf("master write: %v", err)
	}
	waitForInput(t, p)

	if err := p.FlushInput(); err != nil {
		t.Fatalf("FlushInput: %v", err)
	}
	if n := inputQueued(t, p); n != 0 {
		t.Errorf("TIOCINQ after flush = %d, want 0", n)
	}

	if err := p.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	buf := make([]byte, 32)
	if n, err := p.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read after flush returned n=%d %q err=%v; the discarded bytes are still queued", n, buf[:n], err)
	}
}

// TestFlushInputLeavesLaterBytesAlone: the flush must drop what is queued now,
// not put the port into some state where subsequent traffic is lost too.
func TestFlushInputLeavesLaterBytesAlone(t *testing.T) {
	master, slave := newPTY(t)
	p := openPort(t, slave)

	if _, err := master.Write([]byte("stale\r\n")); err != nil {
		t.Fatalf("master write: %v", err)
	}
	waitForInput(t, p)
	if err := p.FlushInput(); err != nil {
		t.Fatalf("FlushInput: %v", err)
	}

	want := []byte("0+123.45\r\n")
	if _, err := master.Write(want); err != nil {
		t.Fatalf("master write: %v", err)
	}
	if got := readN(t, p, len(want)); !bytes.Equal(got, want) {
		t.Errorf("after flush, read %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Open: resolution, exclusion, settle
// ---------------------------------------------------------------------------

func TestOpenResolvesSymlink(t *testing.T) {
	_, slave := newPTY(t)
	// Named like the real by-id entry so the failure message reads the way the
	// production one would.
	link := filepath.Join(t.TempDir(), "usb-FTDI_FT231X_USB_UART_TEST-if00-port0")
	if err := os.Symlink(slave, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p := openPort(t, link)
	if p.Link() != link {
		t.Errorf("Link() = %q, want %q", p.Link(), link)
	}
	if p.Path() != slave {
		t.Errorf("Path() = %q, want the resolved %q", p.Path(), slave)
	}
	if want := link + " -> " + slave; p.String() != want {
		t.Errorf("String() = %q, want %q", p.String(), want)
	}
}

// TestOpenLogsBothTheLinkAndTheDevice: a swapped or re-enumerated adapter keeps
// the by-id path identical and changes only the target, so a log line carrying
// just one of the two makes that invisible in journalctl.
//
// It is at Debug, and that half is asserted too. Open is called once per port
// session, and the daemon reopens on failure — every few cycles, for as long as
// a mute sensor stays mute. At Info that was eleven lines in a 180 s run and ten
// of the fourteen after the onset, which buries the line that says when the
// trouble started. Announcing a session is the caller's call; only the caller
// knows whether this open is the first or the four hundredth.
func TestOpenLogsBothTheLinkAndTheDevice(t *testing.T) {
	_, slave := newPTY(t)
	link := filepath.Join(t.TempDir(), "usb-FTDI_FT231X_USB_UART_TEST-if00-port0")
	if err := os.Symlink(slave, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	open := func(t *testing.T, level slog.Level) string {
		t.Helper()
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))
		p, err := Open(context.Background(), Options{Path: link, SettleDelay: -1, Logger: log})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer p.Close()
		return buf.String()
	}

	out := open(t, slog.LevelDebug)
	for _, want := range []string{link, slave} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not mention %q", out, want)
		}
	}

	// The level a daemon actually runs at. Nothing may come out here, or the
	// reopen loop is a line per open again.
	if got := open(t, slog.LevelInfo); got != "" {
		t.Errorf("Open wrote %q at Info; a caller that reopens every few cycles gets one of these per open", got)
	}
}

// TestNilLoggerFallsBackToDefault: Options is often built from config with no
// logger at all, and a nil dereference on the startup path would be a poor way
// to find out.
func TestNilLoggerFallsBackToDefault(t *testing.T) {
	if got := (Options{}).logger(); got != slog.Default() {
		t.Errorf("logger() = %v, want slog.Default()", got)
	}
	custom := testLogger()
	if got := (Options{Logger: custom}).logger(); got != custom {
		t.Error("logger() ignored the configured logger")
	}
}

// TestStringOnANonSymlinkPath: SDI12_PORT may legitimately name /dev/ttyUSB0
// directly, and "X -> X" would read like a resolution that went nowhere.
func TestStringOnANonSymlinkPath(t *testing.T) {
	_, slave := newPTY(t)
	p := openPort(t, slave)
	if p.String() != slave {
		t.Errorf("String() = %q, want the bare device %q", p.String(), slave)
	}
}

// TestPerDirectionDeadlines: sdi12.Client sets a combined deadline, but the
// split setters exist and a caller that reached for one must not find it inert.
func TestPerDirectionDeadlines(t *testing.T) {
	master, slave := newPTY(t)
	p := openPort(t, slave)

	// A read deadline must not bound writes.
	if err := p.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := p.Write([]byte("0M!")); err != nil {
		t.Errorf("write after the read deadline expired: %v", err)
	}
	if _, err := p.Read(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read: %v; want os.ErrDeadlineExceeded", err)
	}

	// And a write deadline must not bound reads.
	if err := p.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	if err := p.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := master.Write([]byte("0+1.0\r\n")); err != nil {
		t.Fatalf("master write: %v", err)
	}
	if _, err := p.Read(make([]byte, 32)); err != nil {
		t.Errorf("read after the write deadline expired: %v", err)
	}
}

func TestOpenRejectsMissingDevice(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "usb-FTDI-not-plugged-in-if00-port0")
	p, err := Open(context.Background(), Options{Path: missing, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p.Close()
		t.Fatal("Open on a nonexistent device succeeded")
	}
	// A device that has not enumerated yet is an ordinary runtime condition the
	// daemon retries through, not a dead port and not a config error.
	if errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrInUse) {
		t.Errorf("Open on a missing device reported %v; want a plain resolve failure", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Open on a missing device: %v; want os.ErrNotExist", err)
	}
}

// TestOpenRejectsANonTTY covers SDI12_PORT pointing at something that is not a
// serial device — a stale regular file left where the by-id symlink used to be,
// say. config deliberately does not stat the path (USB enumeration lags a
// reboot), so this is where that mistake surfaces, and it must surface as a
// plain failure rather than as ErrDeviceGone, which would send the daemon into
// a reopen loop against a file that will never become a tty.
func TestOpenRejectsANonTTY(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-serial-device")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	p, err := Open(context.Background(), Options{Path: path, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p.Close()
		t.Fatal("Open accepted a regular file as a serial port")
	}
	if !errors.Is(err, unix.ENOTTY) {
		t.Errorf("Open on a regular file: %v; want ENOTTY", err)
	}
	if errors.Is(err, ErrDeviceGone) {
		t.Errorf("Open on a regular file reported a dead device: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path", err)
	}
}

// TestFlushInputReportsIoctlFailure pins the wrapping on the ioctl error path,
// which no real tty produces. ENOTTY specifically must not become ErrDeviceGone:
// see TestOpenRejectsANonTTY for why that distinction has consequences.
func TestFlushInputReportsIoctlFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-tty")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()

	p := &Port{f: f, link: path, path: path}
	err = p.FlushInput()
	if err == nil {
		t.Fatal("FlushInput succeeded on a descriptor with no tty behind it")
	}
	if !errors.Is(err, unix.ENOTTY) {
		t.Errorf("FlushInput: %v; want ENOTTY", err)
	}
	if errors.Is(err, ErrDeviceGone) {
		t.Errorf("FlushInput turned ENOTTY into a dead device: %v", err)
	}
	if !strings.Contains(err.Error(), "flush input") {
		t.Errorf("error %q does not say which operation failed", err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Fatal("Open with no path succeeded")
	}
}

// TestOpenRejectsAnAlreadyCancelledContext pins the precheck at the head of
// Open, as distinct from the ctx-aware settle that
// TestSettleDelayIsInterruptedByContext covers.
//
// SettleDelay is negative here, so settle returns without consulting ctx at all
// and the precheck is the only thing left that can notice. It matters on the
// reopen path: the daemon reopens the adapter on every port-dead error, and
// during shutdown that means asserting DTR and resetting the sensor's MCU for a
// port nobody is going to read.
func TestOpenRejectsAnAlreadyCancelledContext(t *testing.T) {
	_, slave := newPTY(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p, err := Open(ctx, Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p.Close()
		t.Fatal("Open ignored an already-cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Open with a cancelled context: %v; want context.Canceled", err)
	}

	// And it must have bailed before touching the device at all.
	fd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("a cancelled Open still went as far as locking the device: %v", err)
	}
	_ = unix.Close(fd)
}

// TestOpenDiscardsWhatWasQueuedBeforeIt pins Open's trailing FlushInput.
//
// Opening the tty asserts DTR, the Dr. Liu adapter's MCU resets, and it puts a
// banner in the receive queue. The sdi12 client flushes at the head of every
// transaction so this is belt and braces — but "belt and braces" is how a line
// gets deleted, and the property it holds up is that the first bytes a caller
// sees after Open are the sensor's rather than the bridge's.
func TestOpenDiscardsWhatWasQueuedBeforeIt(t *testing.T) {
	master, slave := newPTY(t)

	if _, err := master.Write([]byte("Dr.Liu SDI-12 bridge ready\r\n")); err != nil {
		t.Fatalf("master write: %v", err)
	}

	// A pty moves bytes across a workqueue, so having written them is not the
	// same as their having arrived. Wait on a descriptor of our own, and keep it
	// open across the Open below so the queue cannot be torn down underneath the
	// test. It takes no lock, so it does not compete with Open.
	watcher, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open watcher: %v", err)
	}
	defer unix.Close(watcher)
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := unix.IoctlGetInt(watcher, unix.TIOCINQ)
		if err != nil {
			t.Fatalf("TIOCINQ: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the banner never reached the receive queue")
		}
		time.Sleep(time.Millisecond)
	}

	p := openPort(t, slave)

	if n := inputQueued(t, p); n != 0 {
		t.Errorf("TIOCINQ straight after Open = %d, want 0; the adapter's banner is still queued", n)
	}
	if err := p.SetDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	buf := make([]byte, 64)
	if n, err := p.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("first read after Open returned n=%d %q err=%v; it should have been the sensor's, not the bridge's", n, buf[:n], err)
	}
}

// TestFlockRejectsASecondHolder exercises guard layer 2 in isolation. The
// competing holder takes flock on a plain descriptor without setting TIOCEXCL,
// so Open's own open(2) succeeds and the flock is what turns it away.
func TestFlockRejectsASecondHolder(t *testing.T) {
	_, slave := newPTY(t)

	other, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open competing fd: %v", err)
	}
	defer unix.Close(other)
	if err := unix.Flock(other, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock competing fd: %v", err)
	}

	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p.Close()
		t.Fatal("Open succeeded while another descriptor held the flock")
	}
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("Open against a held flock: %v; want ErrInUse", err)
	}

	// And the losing attempt must not have left the tty in exclusive mode: the
	// flock holder, and the next attempt after it goes away, still need to open
	// the device.
	probe, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("a failed Open left the tty locked against everyone: %v", err)
	}
	_ = unix.Close(probe)
}

// TestExclusiveModeBlocksOtherOpeners exercises guard layer 3. flock is
// advisory and minicom/screen/picocom do not take it; TIOCEXCL is what stops
// them, and stopped the old Python publisher from being run alongside this one.
func TestExclusiveModeBlocksOtherOpeners(t *testing.T) {
	_, slave := newPTY(t)
	p := openPort(t, slave)

	fd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err == nil {
		_ = unix.Close(fd)
		t.Fatal("a plain open of the device succeeded while the port was held")
	}
	if !errors.Is(err, unix.EBUSY) {
		t.Errorf("plain open of a held device: %v; want EBUSY from TIOCEXCL", err)
	}

	// And a second Open through this package is refused for the same reason — it
	// never even reaches its own flock. This is the realistic competitor: not a
	// stranger holding an flock, but another copy of this daemon, whose TIOCEXCL
	// stops our open(2) before there is a descriptor to lock. It has to arrive
	// as ErrInUse, because that is the sentinel the caller distinguishes "a
	// second unit is running" from "the adapter is broken" with, and the two
	// want opposite responses: exit non-zero versus retry the open.
	p2, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p2.Close()
		t.Fatal("a second Open of the same device succeeded")
	}
	if !errors.Is(err, ErrInUse) {
		t.Errorf("a second Open: %v; want ErrInUse — a bare EBUSY tells the daemon nothing about which guard fired", err)
	}
	// The errno stays reachable so the journal still says what the kernel said.
	if !errors.Is(err, unix.EBUSY) {
		t.Errorf("a second Open: %v; the underlying EBUSY was lost", err)
	}
	_ = p
}

// TestFlockIsTakenBeforeExclusiveMode pins the order of guard layers 2 and 3.
//
// flock first is not arbitrary. TIOCEXCL is a single flag on the shared tty
// rather than a property of our descriptor, so an Open that sets it and then
// discovers it has lost the race has to take it back off — and cannot tell its
// own flag from a sibling's. Losing at flock, before the flag is ever set,
// leaves nothing to undo. Reverse the two and this test sees exclusive mode
// already in force at the moment flock runs.
func TestFlockIsTakenBeforeExclusiveMode(t *testing.T) {
	_, slave := newPTY(t)

	var exclusiveAtFlock bool
	swap(t, &flockFD, func(fd, how int) error {
		// Whether an unrelated open of the tty is refused right now is exactly
		// the question "has TIOCEXCL been set yet".
		probe, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
		if err == nil {
			_ = unix.Close(probe)
		} else {
			exclusiveAtFlock = errors.Is(err, unix.EBUSY)
		}
		return unix.Flock(fd, how)
	})

	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	if exclusiveAtFlock {
		t.Error("TIOCEXCL was already set when flock ran: the guards are in the wrong order, so an Open that loses the flock has a flag to clear that may not be its own")
	}
}

// TestFailedFlockLeavesASiblingsExclusiveModeAlone drives the interleaving the
// conditional in Open's fail() exists for, which no amount of running the suite
// normally will produce.
//
// Two instances start together. The sibling wins the flock and reaches its own
// TIOCEXCL while we are still between our open(2) and our flock. We lose. If
// the failure path cleared TIOCEXCL unconditionally instead of only when we set
// it ourselves, we would walk out having disarmed the running instance's
// layer-3 guard — the one that keeps minicom, screen and a stray copy of the
// old Python publisher off the bus — for an instance that is doing nothing
// wrong.
func TestFailedFlockLeavesASiblingsExclusiveModeAlone(t *testing.T) {
	_, slave := newPTY(t)

	sibling, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open the sibling instance's fd: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.IoctlSetInt(sibling, unix.TIOCNXCL, 0)
		_ = unix.Close(sibling)
	})

	var siblingErr error
	swap(t, &flockFD, func(int, int) error {
		siblingErr = unix.IoctlSetInt(sibling, unix.TIOCEXCL, 0)
		return unix.EWOULDBLOCK
	})

	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p.Close()
		t.Fatal("Open succeeded despite losing the flock")
	}
	if siblingErr != nil {
		t.Fatalf("setting up the sibling's TIOCEXCL: %v", siblingErr)
	}
	if !errors.Is(err, ErrInUse) {
		t.Errorf("Open against a held flock: %v; want ErrInUse", err)
	}

	fd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err == nil {
		_ = unix.Close(fd)
		t.Error("a losing Open cleared the sibling instance's TIOCEXCL; the device is now open to anyone while that instance is still using it")
	} else if !errors.Is(err, unix.EBUSY) {
		t.Errorf("probing the device after a losing Open: %v; want EBUSY from the sibling's TIOCEXCL", err)
	}
}

// TestOpenClearsExclusiveModeWhenConfigureFails covers the other half of that
// conditional — the half where we did set the flag.
//
// configure is the only step between TIOCEXCL and os.NewFile, so a TCSETS
// failure is the one real way to reach fail() with exclusive already true; it
// cannot be provoked from a pty, because a descriptor that would reject TCSETS
// would have rejected TIOCEXCL first. Without the clear, the tty stays locked
// on every device whose tty_struct outlives our close — every pty, and a USB
// adapter that some other process also has open — against us and everyone else.
func TestOpenClearsExclusiveModeWhenConfigureFails(t *testing.T) {
	_, slave := newPTY(t)

	boom := errors.New("TCSETS refused the line settings")
	swap(t, &configureFD, func(int) error { return boom })

	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p.Close()
		t.Fatal("Open succeeded despite configure failing")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Open with a failing configure: %v; want the configure error", err)
	}

	fd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("after a failed configure the device is still locked (%v); Open's failure path left TIOCEXCL set", err)
	}
	_ = unix.Close(fd)
}

// TestCloseReleasesExclusiveModeOnAPty documents the one place a pty and a USB
// serial device genuinely diverge, and pins the workaround.
//
// On real hardware our close is the last one, the tty_struct is destroyed and
// TTY_EXCLUSIVE dies with it. On a pty the master keeps the structure alive, so
// the flag would survive and the device would be permanently unopenable — which
// is why Close issues TIOCNXCL explicitly instead of trusting the close.
func TestCloseReleasesExclusiveModeOnAPty(t *testing.T) {
	_, slave := newPTY(t)

	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	p2, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err != nil {
		t.Fatalf("reopen after Close: %v; exclusive mode outlived the descriptor", err)
	}
	if err := p2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestDoubleCloseIsSafe(t *testing.T) {
	_, slave := newPTY(t)
	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Not merely "does not panic": the shutdown path closes the port and so does
	// the reopen path, and a second close must not report a failure the daemon
	// would then log as a fault.
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v; want nil", err)
	}
}

// TestSettleDelayIsInterruptedByContext: 2.5 s of settle runs on every open and
// every reopen, and a SIGTERM arriving during it must not be held up.
func TestSettleDelayIsInterruptedByContext(t *testing.T) {
	_, slave := newPTY(t)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	p, err := Open(ctx, Options{Path: slave, SettleDelay: time.Hour, Logger: testLogger()})
	elapsed := time.Since(start)

	if err == nil {
		_ = p.Close()
		t.Fatal("Open returned successfully despite a cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open during cancellation: %v; want context.DeadlineExceeded", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Open took %v to notice the cancellation; the settle delay is not ctx-aware", elapsed)
	}

	// The abandoned attempt must have released the device, or the next reopen
	// after a restart would find its own adapter locked.
	p2, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err != nil {
		t.Fatalf("reopen after a cancelled settle: %v; the abandoned Open leaked the descriptor", err)
	}
	_ = p2.Close()
}

func TestSettleDelayResolution(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero means the default", 0, DefaultSettleDelay},
		{"negative means none", -1, 0},
		{"positive is honoured", 40 * time.Millisecond, 40 * time.Millisecond},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Options{SettleDelay: tc.in}).settleDelay(); got != tc.want {
				t.Errorf("settleDelay(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestSettleDelayIsActuallyWaited keeps the default from being quietly reduced
// to nothing: the wait exists because the adapter's MCU is resetting.
func TestSettleDelayIsActuallyWaited(t *testing.T) {
	_, slave := newPTY(t)
	const delay = 150 * time.Millisecond

	start := time.Now()
	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: delay, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()

	if elapsed := time.Since(start); elapsed < delay {
		t.Errorf("Open returned after %v, less than the %v settle delay", elapsed, delay)
	}
}

// TestDefaultSettleDelayValue pins the constant. It is the Python's measured
// value, and shrinking it makes the first poll after every restart fail.
func TestDefaultSettleDelayValue(t *testing.T) {
	if DefaultSettleDelay != 2500*time.Millisecond {
		t.Errorf("DefaultSettleDelay = %v, want 2.5s", DefaultSettleDelay)
	}
}

// ---------------------------------------------------------------------------
// The startup self-test
// ---------------------------------------------------------------------------

// TestDeadlineSelfTestRejectsANonPollableFD proves the assertion in Open is not
// merely decorative.
//
// A regular file is the readiest descriptor epoll refuses — internal/poll's Init
// fails with EPERM and quietly leaves the descriptor unregistered — so it stands
// in for the residual uncertainty the self-test exists to remove: whether the
// kernel will accept *this* tty. If it would not, deadlines silently become
// no-ops and every read and write is unbounded again, which is the whole defect
// back. The self-test has to notice.
func TestDeadlineSelfTestRejectsANonPollableFD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-tty")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0o600)
	if err != nil {
		t.Fatalf("open regular file: %v", err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()

	// Sanity: if the runtime ever starts polling regular files this test is
	// testing nothing, and should say so rather than pass vacuously.
	if err := f.SetWriteDeadline(time.Now().Add(time.Hour)); !errors.Is(err, os.ErrNoDeadline) {
		t.Skipf("a regular file now accepts deadlines (%v); no stand-in for a non-pollable fd", err)
	}

	p := &Port{f: f, link: path, path: path}
	err = p.assertDeadlines()
	if err == nil {
		t.Fatal("the self-test accepted a descriptor with no deadline support")
	}
	if !errors.Is(err, ErrNoDeadlines) {
		t.Fatalf("self-test failure: %v; want ErrNoDeadlines", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the device", err)
	}
}

// TestOpenFailsWhenNoDeadlineCanBeSet pins the *call site* at the end of Open,
// which TestDeadlineSelfTestRejectsANonPollableFD does not.
//
// That test proves assertDeadlines answers correctly; this one proves Open
// listens. The two are worth separating because the call site is a three-line
// block that no realistic input can reach — Open cannot be pointed at a
// non-pollable descriptor, since a regular file fails TIOCEXCL with ENOTTY well
// before the self-test — so without the seam, deleting the block leaves the
// whole suite green while the daemon silently regains unbounded writes. That is
// the exact regression this package exists to prevent, and it must not be the
// one mutation nothing catches.
func TestOpenFailsWhenNoDeadlineCanBeSet(t *testing.T) {
	_, slave := newPTY(t)

	// Stand in for the kernel refusing to poll this particular tty: the
	// descriptor opens and configures fine, and only the registration fails.
	swap(t, &setWriteDeadline, func(*os.File, time.Time) error { return os.ErrNoDeadline })

	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err == nil {
		_ = p.Close()
		t.Fatal("Open returned a port whose deadlines do not work; the self-test's answer is being discarded")
	}
	if !errors.Is(err, ErrNoDeadlines) {
		t.Fatalf("Open with a refused deadline: %v; want ErrNoDeadlines", err)
	}
	if !strings.Contains(err.Error(), slave) {
		t.Errorf("error %q does not name the device", err)
	}

	// The refusal must go out through Close, not a bare descriptor close: a unit
	// that fails this check would otherwise leave the tty in exclusive mode and
	// lock out its own next attempt as well as everyone else's.
	fd, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("a failed self-test left the device locked: %v", err)
	}
	_ = unix.Close(fd)
}

// TestDeadlineSelfTestLeavesNoDeadlineBehind: the self-test sets a write
// deadline and must clear it before handing the port back.
//
// The write comes first and is the real assertion. assertDeadlines sets a write
// deadline that is deliberately already expired (see selfTestInstant), so a
// missing clear does not leave a merely inappropriate budget on the descriptor
// — it leaves an expired one, and every subsequent write fails instantly with
// os.ErrDeadlineExceeded without a byte reaching the wire. A far-future value
// would make this test impossible to write: nothing observable within a test's
// lifetime distinguishes an hour-long write budget from none at all, which is
// how the original version of this test came to pass for a reason it had not
// actually checked.
func TestDeadlineSelfTestLeavesNoDeadlineBehind(t *testing.T) {
	// The rest of this test only bites while selfTestInstant is in the past. Move
	// it to the future and both "clear deleted" and "clear kept" pass identically,
	// silently returning the package to the un-guardable state this test exists to
	// prevent — with the doc comment above still explaining why it works. Pin it.
	if !selfTestInstant.Before(time.Now()) {
		t.Fatalf("selfTestInstant is %v, not in the past; this test can no longer tell a leaked write deadline from a cleared one",
			selfTestInstant)
	}

	master, slave := newPTY(t)
	p := openPort(t, slave)

	// No SetDeadline call of our own; the descriptor must come back clean.
	cmd := []byte("0M!")
	if n, err := p.Write(cmd); err != nil {
		t.Fatalf("write with no caller deadline: wrote %d of %d bytes, %v; Open left its self-test deadline on the descriptor",
			n, len(cmd), err)
	}
	// Drain it so the read below is answering the master's reply and not an echo.
	_ = readNFile(t, master, len(cmd))

	// And the read side. The self-test does not set a read deadline today, but
	// it clears with SetDeadline, and a future edit that set one too would be
	// caught here rather than in production.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = master.Write([]byte("0+1.0\r\n"))
	}()

	done := make(chan error, 1)
	go func() {
		_, err := p.Read(make([]byte, 32))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read with no caller deadline: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read never returned; Open left a deadline on the descriptor")
	}
}

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

// TestClosedPortIsDeviceGone: every entry point must agree, because the daemon
// classifies on the sentinel and not on which call produced it.
func TestClosedPortIsDeviceGone(t *testing.T) {
	_, slave := newPTY(t)
	p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ops := []struct {
		name string
		call func() error
	}{
		{"Read", func() error { _, err := p.Read(make([]byte, 8)); return err }},
		{"Write", func() error { _, err := p.Write([]byte("0M!")); return err }},
		{"SetDeadline", func() error { return p.SetDeadline(time.Now().Add(time.Second)) }},
		{"SetReadDeadline", func() error { return p.SetReadDeadline(time.Now().Add(time.Second)) }},
		{"SetWriteDeadline", func() error { return p.SetWriteDeadline(time.Now().Add(time.Second)) }},
		{"FlushInput", p.FlushInput},
	}
	for _, op := range ops {
		t.Run(op.name, func(t *testing.T) {
			err := op.call()
			if err == nil {
				t.Fatalf("%s on a closed port succeeded", op.name)
			}
			if !errors.Is(err, ErrDeviceGone) {
				t.Errorf("%s on a closed port: %v; want ErrDeviceGone", op.name, err)
			}
		})
	}
}

// TestHangupIsDeviceGone simulates the USB unplug. Closing the pty master is the
// closest a test can get to yanking the adapter: the peer disappears and the
// read comes back with a zero-length result, which os.NewFile's ZeroReadIsEOF
// surfaces as io.EOF.
func TestHangupIsDeviceGone(t *testing.T) {
	master, slave := newPTY(t)
	p := openPort(t, slave)

	if err := master.Close(); err != nil {
		t.Fatalf("close master: %v", err)
	}

	if err := p.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	n, err := p.Read(make([]byte, 32))
	if err == nil {
		t.Fatalf("read after hangup returned %d bytes and no error", n)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read after hangup timed out instead of reporting the hangup: %v", err)
	}
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("read after hangup: %v; want ErrDeviceGone", err)
	}
}

// TestDeviceGoneClassification covers the errno set directly, since the errnos a
// real USB re-enumeration produces cannot all be provoked from a pty.
func TestDeviceGoneClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"EIO, the classic yanked-adapter errno", unix.EIO, true},
		{"ENODEV, device unbound mid-read", unix.ENODEV, true},
		{"ENXIO, node survives but the device is gone", unix.ENXIO, true},
		{"EBADF", unix.EBADF, true},
		{"io.EOF, a hangup under ZeroReadIsEOF", io.EOF, true},
		{"os.ErrClosed", os.ErrClosed, true},
		{"wrapped EIO", fmt.Errorf("read: %w", unix.EIO), true},
		{"PathError around EIO", &os.PathError{Op: "read", Path: "/dev/ttyUSB0", Err: unix.EIO}, true},
		{"EAGAIN is not death, it is the poller's normal path", unix.EAGAIN, false},
		{"deadline expiry is not death", os.ErrDeadlineExceeded, false},
		{"ENOTTY, a config mistake rather than a dead device", unix.ENOTTY, false},
		{"EBUSY, someone else holds it", unix.EBUSY, false},
		{"context.Canceled", context.Canceled, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceGone(tc.err); got != tc.want {
				t.Errorf("deviceGone(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyPreservesTheDeadlineDiagnosis: a read timeout must survive
// classification unchanged, because sdi12 maps it to ErrNoResponse ("skip this
// poll") while anything else means ErrPortDead ("reopen the adapter"). Getting
// this backwards costs a 2.5 s adapter reset on every quiet poll.
func TestClassifyPreservesTheDeadlineDiagnosis(t *testing.T) {
	p := &Port{path: "/dev/ttyUSB0"}
	err := p.classify(&os.PathError{Op: "read", Path: "/dev/ttyUSB0", Err: os.ErrDeadlineExceeded})
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("classify lost the deadline: %v", err)
	}
	if errors.Is(err, ErrDeviceGone) {
		t.Fatal("classify turned a read timeout into a dead port")
	}
}

// TestClassifyPassesThroughUnknownErrors: a wrapper that swallowed the original
// would leave an operator with "device gone" for an error that says something
// else entirely.
func TestClassifyPassesThroughUnknownErrors(t *testing.T) {
	p := &Port{path: "/dev/ttyUSB0"}
	sentinel := errors.New("something else")
	if got := p.classify(sentinel); !errors.Is(got, sentinel) {
		t.Errorf("classify(%v) = %v; want the original error", sentinel, got)
	}
	if got := p.classify(nil); got != nil {
		t.Errorf("classify(nil) = %v, want nil", got)
	}
}

// TestDeviceGoneErrorKeepsTheUnderlyingCause: ErrDeviceGone tells the daemon
// what to do, the errno tells the operator what happened, and the log line needs
// both.
func TestDeviceGoneErrorKeepsTheUnderlyingCause(t *testing.T) {
	p := &Port{path: "/dev/ttyUSB0"}
	err := p.classify(&os.PathError{Op: "read", Path: "/dev/ttyUSB0", Err: unix.EIO})
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("classify(EIO) = %v; want ErrDeviceGone", err)
	}
	if !errors.Is(err, unix.EIO) {
		t.Errorf("classify(EIO) = %v; the errno was lost", err)
	}
}

// ---------------------------------------------------------------------------
// The port drives the real protocol client
// ---------------------------------------------------------------------------

// TestPortDrivesSDI12Client wires a Port into the actual sdi12.Client with the
// pty master playing the sensor. It is the seam test: everything either side has
// its own coverage, but only this proves the Transport contract holds when the
// implementation is a real file descriptor.
func TestPortDrivesSDI12Client(t *testing.T) {
	master, slave := newPTY(t)
	p := openPort(t, slave)

	c := sdi12.New(p, '0', time.Second, time.Second)

	sensor := make(chan error, 1)
	go func() {
		// Play out one 0M! transaction: read the command, answer the header,
		// send the service request, then the data.
		script := []struct {
			expect string
			reply  string
		}{
			{"0M!", "00011\r\n"},
			{"", "0\r\n"}, // service request, unprompted
			{"0D0!", "0+1234.5\r\n"},
		}
		buf := make([]byte, 64)
		for _, step := range script {
			if step.expect != "" {
				if err := master.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					sensor <- err
					return
				}
				n, err := master.Read(buf)
				if err != nil {
					sensor <- fmt.Errorf("reading %q: %w", step.expect, err)
					return
				}
				if got := string(buf[:n]); got != step.expect {
					sensor <- fmt.Errorf("sensor got %q, want %q", got, step.expect)
					return
				}
			}
			if _, err := master.Write([]byte(step.reply)); err != nil {
				sensor <- err
				return
			}
		}
		sensor <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	values, err := c.Measure(ctx, "")
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(values) != 1 || values[0] != 1234.5 {
		t.Errorf("Measure = %v, want [1234.5]", values)
	}
	if err := <-sensor; err != nil {
		t.Errorf("sensor side: %v", err)
	}
}

// TestFailedOpenDoesNotLeakDescriptors: the daemon reopens the port on every
// port-dead error, so a leak on the failure path would be a slow march to EMFILE
// on a unit that is already having a bad day.
func TestFailedOpenDoesNotLeakDescriptors(t *testing.T) {
	_, slave := newPTY(t)

	// A competing flock holder makes Open fail after it has already opened the
	// descriptor — the only failure path where there is something to leak.
	other, err := unix.Open(slave, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open competing fd: %v", err)
	}
	defer unix.Close(other)
	if err := unix.Flock(other, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("flock competing fd: %v", err)
	}

	missing := filepath.Join(t.TempDir(), "absent")
	before := openDescriptors(t)
	for range 25 {
		if p, err := Open(context.Background(), Options{Path: slave, SettleDelay: -1, Logger: testLogger()}); err == nil {
			_ = p.Close()
			t.Fatal("Open succeeded against a held flock")
		}
		if p, err := Open(context.Background(), Options{Path: missing, SettleDelay: -1, Logger: testLogger()}); err == nil {
			_ = p.Close()
			t.Fatal("Open succeeded on a missing device")
		}
	}
	if after := openDescriptors(t); after > before {
		t.Errorf("descriptor count went from %d to %d across 50 failed opens", before, after)
	}
}

// openDescriptors counts this process's open file descriptors.
func openDescriptors(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unavailable (%v); cannot count descriptors", err)
	}
	// Minus one for the directory handle ReadDir itself held.
	return len(entries) - 1
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// readN reads exactly n bytes from p under a deadline, failing rather than
// hanging.
func readN(t *testing.T, p *Port, n int) []byte {
	t.Helper()
	if err := p.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		k, err := p.Read(buf)
		out = append(out, buf[:k]...)
		if err != nil {
			t.Fatalf("read (%d/%d bytes so far, %q): %v", len(out), n, out, err)
		}
	}
	return out
}

// readNFile is readN for the pty master.
func readNFile(t *testing.T, f *os.File, n int) []byte {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	out := make([]byte, 0, n)
	buf := make([]byte, n)
	for len(out) < n {
		k, err := f.Read(buf)
		out = append(out, buf[:k]...)
		if err != nil {
			t.Fatalf("read (%d/%d bytes so far, %q): %v", len(out), n, out, err)
		}
	}
	return out
}
