//go:build linux

package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The production dialer, exercised against a pty standing in for the Dr. Liu
// adapter — the same substitution internal/serialport's own tests make. The
// master side plays the sensor.
//
// What this can prove: that Dial produces a Session whose SDI-12 client is
// bound to the descriptor it opened, and that Close releases that descriptor.
// What it cannot: baud rate (a pty moves bytes at memory speed) or the DTR
// toggle that resets the adapter's MCU. Those are asserted structurally in
// serialport.

func newPTY(t *testing.T) (master *os.File, slavePath string) {
	t.Helper()
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("/dev/ptmx is unavailable (%v); this test needs a pty to stand in for the adapter", err)
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(mfd)
		t.Skipf("TIOCSPTLCK on /dev/ptmx failed (%v); no usable pty here", err)
	}
	n, err := unix.IoctlGetInt(mfd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(mfd)
		t.Skipf("TIOCGPTN on /dev/ptmx failed (%v); no usable pty here", err)
	}
	m := os.NewFile(uintptr(mfd), "/dev/ptmx")
	t.Cleanup(func() { _ = m.Close() })
	return m, fmt.Sprintf("/dev/pts/%d", n)
}

func TestSerialDialerProducesAWorkingSession(t *testing.T) {
	master, slave := newPTY(t)

	dialer := &SerialDialer{
		Path:         slave,
		Address:      '0',
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		// Negative skips serialport's 2.5 s post-open settle, which is the
		// adapter's MCU reset and has nothing to reset here.
		SettleDelay: -1,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	session, err := dialer.Dial(t.Context())
	if err != nil {
		t.Fatalf("Dial(%s): %v", slave, err)
	}
	defer func() { _ = session.Close() }()

	if session.String() != slave {
		t.Errorf("session names %q, want the device it opened (%s)", session, slave)
	}

	// Play the sensor: answer 0I! with the golden identification string.
	answered := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		if err := master.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			answered <- err
			return
		}
		n, err := master.Read(buf)
		if err != nil {
			answered <- err
			return
		}
		if got := string(buf[:n]); got != "0I!" {
			answered <- fmt.Errorf("adapter received %q, want %q", got, "0I!")
			return
		}
		_, err = master.Write([]byte("013APOGEE  SQ-5211043043\r\n"))
		answered <- err
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	id, err := session.Identify(ctx)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("the stand-in sensor: %v", err)
	}
	if id.Vendor != "APOGEE" || id.Model != "SQ-521" {
		t.Errorf("identity = %+v, want the SQ-521 golden string decoded", id)
	}

	// Close must release the descriptor, and a second use must say so rather
	// than reaching a file descriptor number the kernel has reused.
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := session.Identify(ctx); err == nil {
		t.Error("Identify succeeded after Close")
	}
}

// M1. A stop must not wait out a deadline the sensor chose.
//
// sdi12 clamps each read to the caller's ctx.Deadline() and never selects on
// ctx.Done(), and the goroutine that could notice a cancellation is the one
// blocked in the read — it owns the port, by design. So without a watcher the
// stop latency is whatever deadline was in force, and that comes from the
// sensor's *declared* conversion time: ParseMeasureHeader accepts ttt up to
// 999 s, and line noise parsing as <addr><3 digits><digit> declares one just as
// convincingly as a real sensor.
//
// The pty answers nothing at all, so the read below would otherwise sit for the
// full deadline.
func TestCancellationUnblocksAnInFlightRead(t *testing.T) {
	_, slave := newPTY(t) // the master is never written to: a silent sensor

	// Far longer than the test's patience, which is the point: nothing but the
	// cancellation can end the read.
	const declaredByTheSensor = 30 * time.Second
	dialer := &SerialDialer{
		Path:         slave,
		Address:      '0',
		ReadTimeout:  declaredByTheSensor,
		WriteTimeout: time.Second,
		SettleDelay:  -1,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	ctx, cancel := context.WithCancel(t.Context())
	session, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial(%s): %v", slave, err)
	}
	defer func() { _ = session.Close() }()

	returned := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		// The deadline is the sensor's, not the caller's: a context with no
		// deadline of its own is exactly the shape sdi12 clamps nothing from.
		_, err := session.Identify(ctx)
		if err == nil {
			t.Error("Identify succeeded against a pty that answered nothing")
		}
		returned <- time.Since(start)
	}()

	// Let the read get in flight, then stop the daemon.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case took := <-returned:
		if took > 5*time.Second {
			t.Errorf("the in-flight read took %s to return after cancellation, want promptly", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the in-flight read did not return within 5s of cancellation; a stop is bounded by the sensor's declared %s",
			declaredByTheSensor)
	}
}

// The watcher must not outlive the session that owns it. One goroutine per port
// session, released by whichever of Close and cancellation comes first.
func TestTheCancellationWatcherDoesNotLeak(t *testing.T) {
	_, slave := newPTY(t)
	dialer := &SerialDialer{
		Path: slave, Address: '0',
		ReadTimeout: time.Second, WriteTimeout: time.Second,
		SettleDelay: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	before := runtime.NumGoroutine()
	for range 20 {
		session, err := dialer.Dial(t.Context())
		if err != nil {
			t.Fatalf("Dial(%s): %v", slave, err)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Close twice: the poll loop closes exactly once, but a watcher that
		// panicked on a second close would take the process down.
		_ = session.Close()
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+2 {
		t.Errorf("goroutines went from %d to %d across twenty opened-and-closed sessions", before, got)
	}
}

// HIGH 5, one layer down. A reopen loop must not write a line per open.
//
// App.runPoller already demotes its own "serial session open" to Debug once the
// sensor has stopped answering (poll.go), because a mute-but-enumerated sensor
// forces a reopen every reopenAfterSoftFailures cycles for as long as it stays
// mute. serialport.Open's line sat directly underneath it and was not demoted,
// so the flood simply relocated: measured over a 180 s mute-sensor run, eleven
// "serial port opened" lines — ~5,240 a day — and ten of the fourteen lines
// after the onset were that one. `journalctl -n 200` after an hour of outage
// returned nothing else and never reached "sensor readings failing", which is
// the only line that says when the trouble started.
//
// The assertion is on the whole stack rather than on serialport alone, because
// that is where the property lives: however the two layers divide the work
// between them, a run of reopens is allowed one line about it.
func TestAReopenLoopDoesNotWriteALinePerOpen(t *testing.T) {
	_, slave := newPTY(t)

	// The level the process actually runs at; a line demoted to Debug is a line
	// an operator never sees, which is the whole point of demoting it.
	journal := newLogSink(t)
	dialer := &SerialDialer{
		Path: slave, Address: '0',
		ReadTimeout: time.Second, WriteTimeout: time.Second,
		SettleDelay: -1, Logger: journal.logger(),
	}

	const reopens = 11 // what the 180 s mute-sensor run churned through
	for range reopens {
		session, err := dialer.Dial(t.Context())
		if err != nil {
			t.Fatalf("Dial(%s): %v", slave, err)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	if n := journal.count("serial port opened"); n > 1 {
		t.Errorf("%q appears %d times across %d opens, want at most 1; at a reopen every ~16 s that is ~%d a day:\n%s",
			"serial port opened", n, reopens, n*24*60*60/180, journal.dump())
	}
	if n := len(journal.matching("")); n > 1 {
		t.Errorf("%d journal lines across %d opens; the flood buries the line that says when the outage started:\n%s",
			n, reopens, journal.dump())
	}
}

// The address is a single ASCII byte off the configured string, not a number.
// config validates the shape; this pins the conversion, because sdi12.New
// silently substitutes the factory address for a zero byte and a transposition
// here would be invisible on a sensor that happens to live at '0'.
func TestNewSerialDialerMapsTheConfiguration(t *testing.T) {
	cfg := testConfig()
	cfg.SDI12Address = "7"
	cfg.SDI12Port = "/dev/serial/by-id/some-adapter"

	d := NewSerialDialer(cfg, slog.New(slog.DiscardHandler))
	if d.Address != '7' {
		t.Errorf("Address = %q, want the ASCII byte '7'", d.Address)
	}
	if d.Path != cfg.SDI12Port {
		t.Errorf("Path = %q, want %q", d.Path, cfg.SDI12Port)
	}
	if d.ReadTimeout != cfg.ReadTimeout || d.WriteTimeout != cfg.WriteTimeout {
		t.Errorf("timeouts = %s/%s, want %s/%s", d.ReadTimeout, d.WriteTimeout, cfg.ReadTimeout, cfg.WriteTimeout)
	}
	// SettleDelay is deliberately left zero, which selects serialport's own
	// default — the adapter's measured 2.5 s reset. A negative here would skip
	// it and lose the first poll after every restart.
	if d.SettleDelay != 0 {
		t.Errorf("SettleDelay = %s, want the zero that selects serialport.DefaultSettleDelay", d.SettleDelay)
	}

	// An unvalidated Config cannot silently produce a wrong address either.
	cfg.SDI12Address = ""
	if got := NewSerialDialer(cfg, nil).Address; got != '0' {
		t.Errorf("Address for an empty configured address = %q, want the factory '0'", got)
	}
}
