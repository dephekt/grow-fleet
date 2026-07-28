package sdi12

import (
	"bytes"
	"errors"
	"fmt"
	"os"
)

// MaxLineLen caps the payload of one SDI-12 response line, excluding the CR/LF
// terminator. The longest thing the SQ-521 ever sends is an identification
// string (~35 bytes); 128 leaves generous headroom while still bounding the
// damage when the adapter starts emitting noise with no newline in it — without
// a cap, a stuck-high line would grow the buffer until the process died.
const MaxLineLen = 128

// readChunk is the per-Read syscall size. SDI-12 responses are tens of bytes, so
// one chunk almost always holds a whole line (and often two: the "atttn" header
// and the service request that follows it a second later).
const readChunk = 256

// LineReader frames newline-terminated ASCII lines out of a [Transport].
//
// The important property — and the reason this is not just a bufio.Scanner — is
// that bytes arriving *after* the newline are retained for the next ReadLine.
// A measurement transaction reads "00011\r\n" and then, a second later, the bare
// service request "0\r\n"; those two frequently land in a single read. The
// Python threw the remainder away by calling readline() on a freshly flushed
// port each time, which is one of the ways it desynced: the service request
// would be consumed as if it were the D0! data line.
//
// A LineReader is not safe for concurrent use.
type LineReader struct {
	t     Transport
	buf   []byte // bytes read from t but not yet returned; may span past a '\n'
	chunk []byte
}

// NewLineReader returns a LineReader framing lines out of t.
func NewLineReader(t Transport) *LineReader {
	return &LineReader{t: t, chunk: make([]byte, readChunk)}
}

// Reset drops any retained bytes. The client calls this alongside
// Transport.FlushInput so that "start a clean transaction" clears both the OS
// buffer and the bytes we already pulled out of it.
func (r *LineReader) Reset() {
	r.buf = r.buf[:0]
}

// Buffered reports how many retained bytes are waiting. Only useful for tests
// and diagnostics.
func (r *LineReader) Buffered() int { return len(r.buf) }

// ReadLine returns the next line with its terminator removed, reading from the
// transport only when the retained bytes do not already contain one.
//
// Deadlines are the caller's job: set one on the Transport before calling.
// A line longer than [MaxLineLen] yields ErrProtocol and the buffer is dropped
// (we are clearly not aligned to the sensor's framing, so keeping the bytes
// would only propagate the desync).
func (r *LineReader) ReadLine() (string, error) {
	for {
		if i := bytes.IndexByte(r.buf, '\n'); i >= 0 {
			line := trimCR(r.buf[:i])
			if len(line) > MaxLineLen {
				r.Reset()
				return "", fmt.Errorf("sdi12: line of %d bytes exceeds %d: %w", len(line), MaxLineLen, ErrProtocol)
			}
			// Materialise the string before shifting: it aliases r.buf.
			s := string(line)
			rest := r.buf[i+1:]
			copy(r.buf, rest)
			r.buf = r.buf[:len(rest)]
			return s, nil
		}

		// No terminator yet. +1 tolerates a pending CR of an otherwise
		// maximum-length payload.
		if len(r.buf) > MaxLineLen+1 {
			n := len(r.buf)
			r.Reset()
			return "", fmt.Errorf("sdi12: %d bytes without a terminator, exceeds %d: %w", n, MaxLineLen, ErrProtocol)
		}

		n, err := r.t.Read(r.chunk)
		if n > 0 {
			r.buf = append(r.buf, r.chunk[:n]...)
			if err == nil {
				continue
			}
		}
		if err == nil {
			// A well-behaved Transport blocks until the deadline rather than
			// spinning on (0, nil); treat it as silence so a broken one cannot
			// wedge us in a busy loop.
			return "", fmt.Errorf("sdi12: transport returned no bytes: %w", ErrNoResponse)
		}
		// Bytes may have been salvaged above; they stay retained in case the
		// rest of the line follows on a later call with a fresh deadline.
		return "", classifyReadErr(err)
	}
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

// timeouter matches net.Error (and anything else reporting a soft timeout)
// without importing net.
type timeouter interface{ Timeout() bool }

// isTimeout reports whether err is a deadline expiry rather than a real fault.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var t timeouter
	return errors.As(err, &t) && t.Timeout()
}

// classifyReadErr maps a transport read failure onto the package sentinels.
//
// A deadline expiry becomes ErrNoResponse, not ErrPortDead: on a silent bus like
// SDI-12 "nobody answered in 2 s" is the ordinary way to learn a command is
// unsupported or the sensor is busy, and reopening the port for that would churn
// the adapter (each open toggles DTR and costs a ~2.5 s MCU reset). Everything
// else — EIO/ENODEV from a yanked USB device, EOF from a closed fd, EBADF — is
// unrecoverable without a reopen, so it becomes ErrPortDead. Write deadlines are
// treated as fatal instead; see Client.write.
func classifyReadErr(err error) error {
	if err == nil {
		return nil
	}
	if isTimeout(err) {
		return fmt.Errorf("sdi12: read timed out: %w", ErrNoResponse)
	}
	return fmt.Errorf("sdi12: read failed: %w: %w", ErrPortDead, err)
}
