package sdi12

import (
	"bytes"
	"os"
	"time"
)

// fakeTransport is a scripted stand-in for the serial port.
//
// It models the bus faithfully in the one way that matters: SDI-12 sensors are
// silent until spoken to, so a queued response only becomes readable when a
// command is written. That is what lets FlushInput behave like the real thing
// (it drops whatever has not been read yet) without invalidating the script.
//
// An empty read buffer yields os.ErrDeadlineExceeded immediately rather than
// blocking, so "the sensor said nothing for two seconds" costs no wall time.
type fakeTransport struct {
	// responses are delivered one per Write, in order. A response may contain
	// several lines ("00011\r\n0\r\n") to model the header and the service
	// request landing in one read.
	responses []string

	writes    []string
	deadlines []time.Time
	flushes   int

	pending bytes.Buffer

	// maxChunk truncates each Read to force the LineReader's accumulate path.
	// Zero means "whatever fits".
	maxChunk int

	// injected failures
	writeErr       error
	readErr        error // returned instead of a deadline once pending drains
	flushErr       error
	setDeadlineErr error

	// onWrite fires after the n-th write (1-based) is recorded, before the
	// response is queued. Used to cancel a context mid-transaction.
	onWrite func(n int)
}

func (f *fakeTransport) Read(p []byte) (int, error) {
	if f.pending.Len() == 0 {
		if f.readErr != nil {
			return 0, f.readErr
		}
		return 0, os.ErrDeadlineExceeded
	}
	n := len(p)
	if f.maxChunk > 0 && f.maxChunk < n {
		n = f.maxChunk
	}
	return f.pending.Read(p[:n])
}

func (f *fakeTransport) Write(p []byte) (int, error) {
	f.writes = append(f.writes, string(p))
	if f.onWrite != nil {
		f.onWrite(len(f.writes))
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if len(f.responses) > 0 {
		f.pending.WriteString(f.responses[0])
		f.responses = f.responses[1:]
	}
	return len(p), nil
}

func (f *fakeTransport) SetDeadline(t time.Time) error {
	f.deadlines = append(f.deadlines, t)
	return f.setDeadlineErr
}

func (f *fakeTransport) FlushInput() error {
	f.flushes++
	f.pending.Reset()
	return f.flushErr
}

// lastDeadline returns the most recently set deadline.
func (f *fakeTransport) lastDeadline() time.Time {
	if len(f.deadlines) == 0 {
		return time.Time{}
	}
	return f.deadlines[len(f.deadlines)-1]
}

// staticTransport serves a fixed byte string and then reports silence. Used for
// LineReader tests, which have no command/response pairing.
type staticTransport struct {
	data     bytes.Buffer
	maxChunk int
	err      error // returned once data is exhausted
	flushes  int
}

func newStaticTransport(s string) *staticTransport {
	t := &staticTransport{}
	t.data.WriteString(s)
	return t
}

func (t *staticTransport) Read(p []byte) (int, error) {
	if t.data.Len() == 0 {
		if t.err != nil {
			return 0, t.err
		}
		return 0, os.ErrDeadlineExceeded
	}
	n := len(p)
	if t.maxChunk > 0 && t.maxChunk < n {
		n = t.maxChunk
	}
	return t.data.Read(p[:n])
}

func (t *staticTransport) Write(p []byte) (int, error)   { return len(p), nil }
func (t *staticTransport) SetDeadline(_ time.Time) error { return nil }
func (t *staticTransport) FlushInput() error {
	t.flushes++
	t.data.Reset()
	return nil
}

// chatterTransport never goes quiet: every Read yields bytes. Models an adapter
// spewing continuously, which Resync must survive rather than spin on.
type chatterTransport struct {
	reads int
}

func (c *chatterTransport) Read(p []byte) (int, error) {
	c.reads++
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func (c *chatterTransport) Write(p []byte) (int, error) { return len(p), nil }
func (c *chatterTransport) SetDeadline(_ time.Time) error {
	return nil
}
func (c *chatterTransport) FlushInput() error { return nil }

var (
	_ Transport = (*fakeTransport)(nil)
	_ Transport = (*staticTransport)(nil)
	_ Transport = (*chatterTransport)(nil)
)
