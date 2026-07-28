// Package sdi12 implements a hardware-free SDI-12 client for the Apogee SQ-521
// quantum sensor as reached through a Dr. Liu SDI-12->USB bridge.
//
// Nothing in this package opens a port, sleeps on a clock, or touches the OS:
// all I/O goes through the [Transport] seam, so the entire framing and parsing
// surface is exercised in tests against a scripted buffer. The daemon layer owns
// the serial port, the retry policy, and the MQTT contract.
//
// It is a port of the Python publisher's Sdi12 class, with the framing defects
// that made a wedged adapter unrecoverable fixed — see the individual doc
// comments on [ParseMeasureHeader], [ParseValues] and [Client.Measure].
package sdi12

import "errors"

// Callers classify failures with errors.Is against these four sentinels. The
// split exists because the daemon's recovery action differs per class, and the
// Python conflated all of them into "None", which is why a desynced port stayed
// desynced forever.
//
// Every error produced by an I/O path wraps exactly one sentinel. Two kinds of
// failure deliberately wrap none, because neither is a statement about the
// sensor and neither has a recovery action on this list:
//
//   - The caller's context ended. These wrap context.Canceled or
//     context.DeadlineExceeded. Cancellation is the most common non-nil error
//     during shutdown, and attaching a sentinel to it would be a lie with
//     consequences: ErrPortDead would provoke a pointless reopen mid-shutdown,
//     ErrNoResponse would claim the sensor was silent when we never finished
//     listening.
//   - Argument validation, i.e. [Client.Measure] rejecting a sub-command. It
//     fails before any I/O, so there is no wire outcome to classify and no port
//     state to recover; it is a caller bug that no retry policy can fix.
//
// So the order to classify in is:
//
//	ctx.Err() != nil          -> shutting down; discard
//	errors.Is(ErrPortDead)    -> close and reopen the port
//	errors.Is(ErrProtocol)    -> already drained for you; retry next poll
//	errors.Is(ErrNoValues)    -> stop issuing this command
//	errors.Is(ErrNoResponse)  -> skip this poll, keep the port
//	default                   -> a usage bug; fix the call
var (
	// ErrNoResponse means the sensor said nothing within the budget. Normal and
	// transient (SDI-12 is a silent bus between transactions): skip this poll
	// and try again — do NOT reopen the port.
	ErrNoResponse = errors.New("sdi12: no response within budget")

	// ErrProtocol means we heard bytes but they did not fit the frame — either a
	// parser rejected the line or [LineReader] could not frame one at all. The
	// port is presumed desynced (we are reading mid-response, or the adapter
	// injected something), so every [Client] path that can raise it drains the
	// port before returning; the caller does not need to call [Client.Resync].
	ErrProtocol = errors.New("sdi12: malformed response")

	// ErrNoValues means no value was obtained even though the sensor was
	// answering: either it declared zero values up front, or it declared some
	// and then delivered none. Callers should stop asking only in the first
	// case — see [ErrHeaderNoValues].
	ErrNoValues = errors.New("sdi12: sensor reports zero values")

	// ErrHeaderNoValues narrows ErrNoValues to the one case that is a
	// definitive statement about this unit's capabilities: the "atttn" header
	// of an aM!/aV! reply declared a value count of zero. The SQ-521 does this
	// for 0M4! (tilt) on serial numbers below 3033.
	//
	// Every error wrapping it also wraps ErrNoValues, so a caller classifying
	// only by the four sentinels above is unaffected and the order above still
	// holds. The distinction exists because the other ErrNoValues path —
	// [Client.collect] finding nothing after a header that promised n values —
	// is an adapter that swallowed the data response, which is transient. A
	// caller that retires an entity permanently (the daemon does exactly that
	// for tilt) must key on the header and never on a lost data line, or one
	// glitch disables the channel until the next restart.
	ErrHeaderNoValues = errors.New("sdi12: measurement header declares zero values")

	// ErrPortDead means the file descriptor itself is unusable — EIO/ENODEV
	// after a USB re-enumeration, EOF after a close, or a write that could not
	// drain. Only a close-and-reopen recovers; retrying reads will spin.
	ErrPortDead = errors.New("sdi12: port unusable")
)
