package sdi12

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLineReaderFraming(t *testing.T) {
	tests := []struct {
		name  string
		wire  string
		want  []string
		chunk int // 0 = deliver as much as fits
	}{
		{
			name: "crlf terminated",
			wire: "00011\r\n0\r\n0+156.9289\r\n",
			want: []string{"00011", "0", "0+156.9289"},
		},
		{
			name: "bare lf terminated",
			wire: "00011\n0\n",
			want: []string{"00011", "0"},
		},
		{
			name: "empty lines preserved",
			wire: "\r\n0\r\n",
			want: []string{"", "0"},
		},
		{
			// One byte at a time forces the accumulate path across many reads.
			name:  "reassembled from single bytes",
			wire:  "013APOGEE  SQ-5211043043\r\n",
			want:  []string{"013APOGEE  SQ-5211043043"},
			chunk: 1,
		},
		{
			name:  "split across chunks mid-terminator",
			wire:  "00011\r\n0+1.1\r\n",
			want:  []string{"00011", "0+1.1"},
			chunk: 6, // cuts between the CR and the LF
		},
		{
			name: "maximum length payload accepted",
			wire: strings.Repeat("x", MaxLineLen) + "\r\n",
			want: []string{strings.Repeat("x", MaxLineLen)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newStaticTransport(tc.wire)
			tr.maxChunk = tc.chunk
			r := NewLineReader(tr)
			for i, want := range tc.want {
				got, err := r.ReadLine()
				if err != nil {
					t.Fatalf("ReadLine %d: unexpected error: %v", i, err)
				}
				if got != want {
					t.Fatalf("ReadLine %d = %q, want %q", i, got, want)
				}
			}
			if _, err := r.ReadLine(); !errors.Is(err, ErrNoResponse) {
				t.Fatalf("ReadLine past end: error = %v, want ErrNoResponse", err)
			}
		})
	}
}

// The header and the service request routinely arrive in a single read; the
// bytes after the first '\n' must survive for the next call. Losing them is what
// made the Python consume the service request as if it were the data line.
func TestLineReaderRetainsLeftover(t *testing.T) {
	tr := newStaticTransport("00011\r\n0\r\n")
	tr.maxChunk = 0 // one read delivers everything
	r := NewLineReader(tr)

	first, err := r.ReadLine()
	if err != nil {
		t.Fatalf("first ReadLine: %v", err)
	}
	if first != "00011" {
		t.Fatalf("first ReadLine = %q, want %q", first, "00011")
	}
	if r.Buffered() == 0 {
		t.Fatal("no bytes retained after the first line; the second line was dropped")
	}

	// Draining the transport proves the second line comes from the retained
	// bytes and not from another read.
	tr.data.Reset()

	second, err := r.ReadLine()
	if err != nil {
		t.Fatalf("second ReadLine: %v", err)
	}
	if second != "0" {
		t.Fatalf("second ReadLine = %q, want the service request %q", second, "0")
	}
	if r.Buffered() != 0 {
		t.Fatalf("Buffered() = %d after consuming both lines, want 0", r.Buffered())
	}
}

func TestLineReaderReset(t *testing.T) {
	tr := newStaticTransport("00011\r\n0\r\n")
	r := NewLineReader(tr)
	if _, err := r.ReadLine(); err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if r.Buffered() == 0 {
		t.Fatal("expected retained bytes before Reset")
	}

	r.Reset()
	if r.Buffered() != 0 {
		t.Fatalf("Buffered() = %d after Reset, want 0", r.Buffered())
	}
	tr.data.Reset()
	if _, err := r.ReadLine(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("after Reset the retained line was still returned: err = %v", err)
	}
}

func TestLineReaderLengthCap(t *testing.T) {
	tests := []struct {
		name string
		wire string
	}{
		{
			name: "over-long terminated line",
			wire: strings.Repeat("x", MaxLineLen+1) + "\r\n",
		},
		{
			// A stuck-high line never sends a terminator; without the cap the
			// buffer would grow until the process died.
			name: "no terminator at all",
			wire: strings.Repeat("x", MaxLineLen*4),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewLineReader(newStaticTransport(tc.wire))
			line, err := r.ReadLine()
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("ReadLine error = %v, want ErrProtocol", err)
			}
			if line != "" {
				t.Fatalf("ReadLine returned %q alongside its error", line)
			}
			if r.Buffered() != 0 {
				t.Fatalf("Buffered() = %d after an over-long line, want the buffer dropped", r.Buffered())
			}
		})
	}
}

func TestLineReaderErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		// Silence on a request/response bus is ordinary; reopening the port for
		// it would cost a 2.5 s adapter reset on every unsupported command.
		{name: "deadline is silence", err: os.ErrDeadlineExceeded, want: ErrNoResponse},
		{name: "timeout interface is silence", err: timeoutErr{}, want: ErrNoResponse},
		{name: "eof is fatal", err: io.EOF, want: ErrPortDead},
		{name: "closed fd is fatal", err: os.ErrClosed, want: ErrPortDead},
		{name: "unplugged usb is fatal", err: errors.New("read /dev/ttyUSB0: no such device"), want: ErrPortDead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newStaticTransport("")
			tr.err = tc.err
			r := NewLineReader(tr)
			_, err := r.ReadLine()
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReadLine error = %v, want %v", err, tc.want)
			}
			if !errors.Is(err, tc.err) && tc.want == ErrPortDead {
				t.Fatalf("ReadLine error %v lost the underlying cause %v", err, tc.err)
			}
		})
	}
}

// A partial line followed by silence keeps its bytes: the rest may still arrive
// under a fresh deadline, and the client explicitly Resets when it wants them
// gone.
func TestLineReaderRetainsPartialLineAfterTimeout(t *testing.T) {
	tr := newStaticTransport("0+156.")
	r := NewLineReader(tr)
	if _, err := r.ReadLine(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("ReadLine error = %v, want ErrNoResponse", err)
	}
	if r.Buffered() != len("0+156.") {
		t.Fatalf("Buffered() = %d, want the partial line retained", r.Buffered())
	}

	tr.data.WriteString("9289\r\n")
	got, err := r.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine after the rest arrived: %v", err)
	}
	if got != "0+156.9289" {
		t.Fatalf("ReadLine = %q, want %q", got, "0+156.9289")
	}
}

// A Transport that returns (0, nil) instead of blocking until its deadline must
// not spin the reader in a tight loop.
func TestLineReaderRejectsEmptyReads(t *testing.T) {
	r := NewLineReader(emptyReadTransport{})
	if _, err := r.ReadLine(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("ReadLine error = %v, want ErrNoResponse", err)
	}
}

type emptyReadTransport struct{}

func (emptyReadTransport) Read(_ []byte) (int, error)    { return 0, nil }
func (emptyReadTransport) Write(p []byte) (int, error)   { return len(p), nil }
func (emptyReadTransport) SetDeadline(_ time.Time) error { return nil }
func (emptyReadTransport) FlushInput() error             { return nil }

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }
