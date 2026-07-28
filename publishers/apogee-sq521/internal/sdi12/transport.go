package sdi12

import (
	"io"
	"time"
)

// Transport is the byte pipe to the sensor: in production a serial port opened
// at 9600 8N1, in tests a scripted buffer.
//
// It is deliberately the *only* thing in this package that touches the outside
// world. Keeping SetDeadline and FlushInput in the interface (rather than, say,
// accepting an io.ReadWriter and layering timeouts on top with goroutines) means
// the client never leaks a goroutine blocked on a wedged FTDI read, and a test
// can make "the port went quiet" happen instantly instead of by waiting.
type Transport interface {
	io.ReadWriter

	// SetDeadline bounds both subsequent reads and writes. A zero t clears the
	// deadline. Implementations must return an error satisfying
	// errors.Is(err, os.ErrDeadlineExceeded) — or a net.Error reporting
	// Timeout() == true — from the I/O call that expires, so the client can tell
	// "the bus was silent" (ErrNoResponse) apart from "the fd is gone"
	// (ErrPortDead).
	SetDeadline(t time.Time) error

	// FlushInput discards bytes sitting in the OS receive buffer. Every
	// transaction starts with one so a response we abandoned last poll (say, a
	// service request that arrived after we gave up) is not mistaken for this
	// poll's answer.
	//
	// Note that FlushInput cannot see bytes already pulled into [LineReader]'s
	// retained buffer; the client always pairs it with LineReader.Reset.
	FlushInput() error
}
