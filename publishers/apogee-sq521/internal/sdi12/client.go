package sdi12

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	// defaultReadTimeout matches the Python's serial timeout=2. SDI-12 requires
	// a sensor to begin its reply within 15 ms, so 2 s is pure slack for the
	// USB bridge.
	defaultReadTimeout = 2 * time.Second

	// defaultWriteTimeout bounds a 4-byte command at 9600 baud (~4 ms of wire
	// time). Anything approaching a second means the FTDI is not draining.
	defaultWriteTimeout = time.Second

	// serviceRequestSlack is added to the sensor's own ttt estimate when waiting
	// for the service request, mirroring the Python's `ttt + 1`.
	serviceRequestSlack = time.Second

	// maxDataCommands caps the D0!/D1!/D2! continuation walk. The SQ-521 fits
	// every command's payload in one response; the cap keeps a lying value count
	// from turning into an unbounded loop.
	maxDataCommands = 3

	// drainQuiet is how long the port must stay silent during Resync before we
	// call it realigned, and drainReads bounds the work if it never does.
	drainQuiet = 250 * time.Millisecond
	drainReads = 32
)

// Client speaks SDI-12 to a single sensor address over a [Transport].
//
// Not safe for concurrent use: SDI-12 is a half-duplex request/response bus, so
// one goroutine must own a Client (and hence the port) for the duration of a
// transaction.
type Client struct {
	t            Transport
	addr         byte
	readTimeout  time.Duration
	writeTimeout time.Duration
	lr           *LineReader

	missedServiceRequests uint64
	drainBuf              []byte
}

// New returns a Client for the sensor at addr (the ASCII character, e.g. '0',
// not the number 0). Non-positive timeouts fall back to the package defaults.
func New(t Transport, addr byte, readTimeout, writeTimeout time.Duration) *Client {
	if addr == 0 {
		addr = '0' // the SQ-521's factory address
	}
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	if writeTimeout <= 0 {
		writeTimeout = defaultWriteTimeout
	}
	return &Client{
		t:            t,
		addr:         addr,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
		lr:           NewLineReader(t),
		drainBuf:     make([]byte, readChunk),
	}
}

// Address returns the sensor address this client talks to.
func (c *Client) Address() byte { return c.addr }

// MissedServiceRequests counts measurements where the sensor never sent its
// bare-address "data is ready" notification and we collected the data anyway.
// A steadily climbing count means the adapter is swallowing traffic; a constant
// one is harmless.
func (c *Client) MissedServiceRequests() uint64 { return c.missedServiceRequests }

// Identify issues aI! and decodes the fixed-width identification string. The
// daemon uses the firmware for the device's sw_version and the serial for the
// sensor_serial diagnostic entity.
func (c *Client) Identify(ctx context.Context) (Identity, error) {
	if err := c.begin(ctx); err != nil {
		return Identity{}, err
	}
	cmd := c.command("I!")
	if err := c.write(ctx, cmd); err != nil {
		return Identity{}, err
	}
	line, err := c.readLine(ctx, c.readTimeout)
	if err != nil {
		// The framer raises ErrProtocol of its own for an over-long or
		// terminator-less line. That means we are mid-frame just as surely as a
		// parse failure does, so it drains too.
		c.resyncOnProtocol(ctx, err)
		return Identity{}, fmt.Errorf("sdi12: %s: %w", cmd, err)
	}
	id, err := ParseIdentity(line)
	if err != nil {
		c.resyncOnProtocol(ctx, err)
		return Identity{}, fmt.Errorf("sdi12: %s: %w", cmd, err)
	}
	return id, nil
}

// Measure runs a full aM<sub>! measurement transaction and returns every value
// the sensor reports. sub selects the measurement: "" for PPFD (0M!), "1" for
// detector millivolts (0M1!), "4" for tilt (0M4!).
//
// ErrNoValues means this unit does not implement the measurement — SQ-521s below
// serial 3033 answer 0M4! with a zero value count — and the caller should stop
// polling it rather than log a fault every cycle.
func (c *Client) Measure(ctx context.Context, sub string) ([]float64, error) {
	if err := validSub(sub); err != nil {
		return nil, err
	}
	return c.transaction(ctx, c.command("M"+sub+"!"))
}

// Verify runs aV!, whose values are the sensor's calibration coefficients. The
// first is the multiplier the daemon publishes as cal_factor.
func (c *Client) Verify(ctx context.Context) ([]float64, error) {
	return c.transaction(ctx, c.command("V!"))
}

// Resync realigns the port after garbage: it drops everything buffered on both
// sides of the OS boundary, then reads until the sensor has been quiet for
// drainQuiet. Silence is the success condition, so a deadline expiry here is not
// an error.
//
// The Python had no equivalent, which is why a single desync was terminal.
func (c *Client) Resync(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sdi12: resync: %w", err)
	}
	c.lr.Reset()
	if err := c.t.FlushInput(); err != nil {
		return fmt.Errorf("sdi12: resync: flush input: %w: %w", ErrPortDead, err)
	}
	for i := 0; i < drainReads; i++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("sdi12: resync: %w", err)
		}
		if err := c.t.SetDeadline(c.deadline(ctx, drainQuiet)); err != nil {
			return fmt.Errorf("sdi12: resync: set deadline: %w: %w", ErrPortDead, err)
		}
		n, err := c.t.Read(c.drainBuf)
		switch {
		case err == nil && n > 0:
			continue // still talking; keep discarding
		case err == nil, isTimeout(err):
			return nil // quiet
		default:
			// io.EOF has no case of its own: on a serial fd it says exactly what
			// EIO and ENODEV say — the descriptor is gone and only a reopen
			// recovers — so a separate branch could only ever produce the same
			// error, which is what it did.
			return fmt.Errorf("sdi12: resync: %w: %w", ErrPortDead, err)
		}
	}
	// Chattering past the cap is not itself fatal — the next transaction starts
	// with its own flush — so report success and let the caller's protocol
	// errors escalate if it really is stuck.
	return nil
}

// transaction is the ported _transaction: command -> "atttn" -> service request
// -> D0!… Every defect called out in the doc comments below was a silent failure
// mode in the Python.
func (c *Client) transaction(ctx context.Context, cmd string) ([]float64, error) {
	// 1. Start from a known-clean port: stale bytes from an abandoned poll are
	//    indistinguishable from this poll's reply once they are in the buffer.
	if err := c.begin(ctx); err != nil {
		return nil, err
	}

	// 2. The write carries a deadline. Its absence is what let a wedged FTDI
	//    block the whole daemon forever: pyserial's write() has no timeout by
	//    default, so the poll loop simply stopped, taking the MQTT keepalive and
	//    the systemd watchdog down with it.
	if err := c.write(ctx, cmd); err != nil {
		return nil, err
	}

	// 3. "atttn".
	line, err := c.readLine(ctx, c.readTimeout)
	if err != nil {
		// A framer-level ErrProtocol (over-long or terminator-less line) is the
		// same "we are mid-frame" condition as a parse failure, and the more
		// urgent one: it means the adapter is still streaming, so next poll's
		// flush would race the tail of the garbage.
		c.resyncOnProtocol(ctx, err)
		return nil, fmt.Errorf("sdi12: %s: %w", cmd, err)
	}
	ready, count, err := ParseMeasureHeader(line)
	if err != nil {
		// 4. Garbage here means we are reading mid-frame. Drain before
		//    returning so the next poll starts aligned instead of inheriting
		//    the desync.
		c.resyncOnProtocol(ctx, err)
		return nil, fmt.Errorf("sdi12: %s: %w", cmd, err)
	}

	// 5. A zero value count is the sensor declining the command, not a fault.
	if count == 0 {
		return nil, fmt.Errorf("sdi12: %s: measurement reports 0 values: %w", cmd, ErrNoValues)
	}

	// 6. ttt > 0 means "call back later"; the sensor announces readiness by
	//    transmitting its bare address.
	var missedSR bool
	if ready > 0 {
		var err error
		if missedSR, err = c.awaitServiceRequest(ctx, cmd, ready); err != nil {
			return nil, err
		}
	}

	// 7. Collect. The value count from the header finally gets used: if D0!
	//    under-delivers we continue with D1!, D2!.
	return c.collect(ctx, cmd, count, missedSR)
}

// awaitServiceRequest waits out the sensor's own conversion estimate. It reports
// whether the notification failed to turn up, which changes how a bare address
// is read during collection.
//
// The budget is the sensor's ttt plus a second of slack — never c.readTimeout.
// On a long-conversion sensor those differ by minutes, and a read timeout would
// put aD0! on the bus while the measurement is still running.
func (c *Client) awaitServiceRequest(ctx context.Context, cmd string, ready time.Duration) (missed bool, err error) {
	budget := ready + serviceRequestSlack
	// What a full wait would cost, computed before spending any of it, so that a
	// timeout can be attributed either to the sensor or to our own deadline.
	full := time.Now().Add(budget)

	line, err := c.readLine(ctx, budget)
	switch {
	case err != nil:
		if !errors.Is(err, ErrNoResponse) {
			// Includes the framer's own ErrProtocol for an over-long or
			// terminator-less line; drain before giving up.
			c.resyncOnProtocol(ctx, err)
			return false, fmt.Errorf("sdi12: %s: awaiting service request: %w", cmd, err)
		}
		if cd, ok := ctx.Deadline(); ok && cd.Before(full) {
			// The wait did not elapse, it was cut short: [Client.deadline]
			// clamps every read to the caller's deadline, so a poll cycle
			// tighter than ttt expires here with the conversion still running.
			// Proceeding would send aD0! mid-measurement — the desync this
			// package exists to eliminate — and return a value that on real
			// hardware is stale. Charging missedServiceRequests would be worse
			// than useless: it is the one diagnostic that tells a lossy adapter
			// from a healthy one, and ordinary deadline pressure would make it
			// blame the adapter.
			return false, fmt.Errorf("sdi12: %s: context deadline left %v of the sensor's %v conversion unwaited: %w",
				cmd, full.Sub(cd), budget, context.DeadlineExceeded)
		}
		// Timing out after the full budget is survivable: we have really waited
		// ttt, so the data is due and D0! is legitimate. Some adapters simply
		// eat the notification. Record it — a rising count is the signal that
		// the bridge, not the sensor, is dropping traffic.
		c.missedServiceRequests++
		return true, nil
	case line == "":
		// A stray bare CRLF carries no information; treat it as silence rather
		// than as a framing violation.
		c.missedServiceRequests++
		return true, nil
	case !IsServiceRequest(line, c.addr):
		// Something else is on the wire. This was the Python's second silent
		// desync path: it read one line, ignored what it said, and then sent
		// D0! into whatever conversation was actually in progress.
		c.resyncOnProtocol(ctx, ErrProtocol)
		return false, fmt.Errorf("sdi12: %s: expected service request %q, got %q: %w",
			cmd, string(c.addr), line, ErrProtocol)
	}
	return false, nil
}

// collect walks aD0!, aD1!, … until it has count values or runs out of road.
func (c *Client) collect(ctx context.Context, cmd string, count int, lateSR bool) ([]float64, error) {
	// The reservation is clamped independently of the codec's own range check.
	// count is a length field that arrived from a USB device, and a length field
	// off the wire must never size an allocation on trust alone — one bound is a
	// validation, two is the invariant that the process survives a bad line.
	values := make([]float64, 0, min(max(count, 0), maxValueCount))
	for d := 0; d < maxDataCommands && len(values) < count; d++ {
		dcmd := fmt.Sprintf("%cD%d!", c.addr, d)
		if err := c.write(ctx, dcmd); err != nil {
			return nil, err
		}
		line, err := c.readDataLine(ctx, lateSR)
		lateSR = false // at most one notification can still be in flight
		if err != nil {
			// Framer-level ErrProtocol drains here too; see Identify.
			c.resyncOnProtocol(ctx, err)
			return nil, fmt.Errorf("sdi12: %s: %s: %w", cmd, dcmd, err)
		}
		vals, err := ParseValues(line)
		if err != nil {
			// A bare address means "no more data". Legitimate when the sensor's
			// value count was optimistic; only an error if we have nothing.
			if errors.Is(err, ErrNoValues) && len(values) > 0 {
				break
			}
			c.resyncOnProtocol(ctx, err)
			return nil, fmt.Errorf("sdi12: %s: %s: %w", cmd, dcmd, err)
		}
		values = append(values, vals...)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("sdi12: %s: no values collected: %w", cmd, ErrNoValues)
	}
	// Fewer than promised is returned as-is rather than failed: the daemon only
	// needs the leading value, and a short read is not evidence of corruption.
	return values, nil
}

// readDataLine reads a D-command reply.
//
// A bare address is ambiguous on the wire: it is both the "no more data" reply
// and the service request. The disambiguation is context, not content — if we
// already consumed the notification in step 6, a bare address here can only mean
// "no more data". Only when the wait timed out (lateSR) might the notification
// still be in flight and land in front of the data, so only then do we skip one.
//
// Skipping rather than flushing is deliberate: a flush between the measurement
// and the collection would race the same notification and could discard the data
// line instead of the duplicate.
func (c *Client) readDataLine(ctx context.Context, lateSR bool) (string, error) {
	line, err := c.readLine(ctx, c.readTimeout)
	if err != nil {
		return "", err
	}
	// A blank line never carries data, so re-reading past it is always safe.
	skippedSR := lateSR && IsServiceRequest(line, c.addr)
	if line != "" && !skippedSR {
		return line, nil
	}
	next, err := c.readLine(ctx, c.readTimeout)
	if err != nil && skippedSR && errors.Is(err, ErrNoResponse) {
		// Nothing followed the bare address, so it was never the late
		// notification we guessed it was — it was the sensor's well-formed "I
		// have no more data" reply. Returning the silence instead told the
		// daemon ErrNoResponse, "transient, retry", about a sensor that answered
		// correctly and will answer identically forever; the caller turns this
		// line into ErrNoValues, "stop asking". The re-read costs a readTimeout
		// either way — that is the price of the ambiguity, not of the
		// classification — but only the classification was wrong.
		return line, nil
	}
	return next, err
}

// begin puts both buffers — the OS receive queue and our retained bytes — into a
// known-empty state.
func (c *Client) begin(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sdi12: %w", err)
	}
	c.lr.Reset()
	if err := c.t.FlushInput(); err != nil {
		return fmt.Errorf("sdi12: flush input: %w: %w", ErrPortDead, err)
	}
	return nil
}

func (c *Client) write(ctx context.Context, cmd string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("sdi12: %s: %w", cmd, err)
	}
	if err := c.t.SetDeadline(c.deadline(ctx, c.writeTimeout)); err != nil {
		return fmt.Errorf("sdi12: %s: set write deadline: %w: %w", cmd, ErrPortDead, err)
	}
	// Unlike a read, a write that cannot drain is never "the bus was quiet" —
	// four bytes at 9600 baud take microseconds — so every write failure,
	// deadline included, is ErrPortDead and the caller should reopen.
	if _, err := io.WriteString(c.t, cmd); err != nil {
		return fmt.Errorf("sdi12: %s: write: %w: %w", cmd, ErrPortDead, err)
	}
	return nil
}

func (c *Client) readLine(ctx context.Context, budget time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("sdi12: %w", err)
	}
	if err := c.t.SetDeadline(c.deadline(ctx, budget)); err != nil {
		return "", fmt.Errorf("sdi12: set read deadline: %w: %w", ErrPortDead, err)
	}
	return c.lr.ReadLine()
}

// deadline is now+budget, clamped to the context's own deadline so a cancelled
// poll cycle cannot be extended by a generous per-read budget.
func (c *Client) deadline(ctx context.Context, budget time.Duration) time.Time {
	d := time.Now().Add(budget)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		return cd
	}
	return d
}

// resyncOnProtocol drains the port when err says we heard garbage. Best effort:
// the caller is already returning a failure, and a resync error would only
// replace a precise diagnosis with a vaguer one.
func (c *Client) resyncOnProtocol(ctx context.Context, err error) {
	if errors.Is(err, ErrProtocol) {
		_ = c.Resync(ctx)
	}
}

func (c *Client) command(tail string) string {
	return string(c.addr) + tail
}

// validSub guards the caller against building a command the sensor will not
// recognise. SDI-12 allows aM! and aM1!..aM9! only.
func validSub(sub string) error {
	switch {
	case sub == "":
		return nil
	case len(sub) == 1 && sub[0] >= '1' && sub[0] <= '9':
		return nil
	default:
		return fmt.Errorf("sdi12: invalid measurement sub-command %q: want %q or a digit 1-9", sub, "")
	}
}
