package sdi12

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Timeouts are generous in wall-clock terms but never actually elapse: the fake
// transport reports a deadline expiry the moment it has nothing queued.
const (
	testRead  = 2 * time.Second
	testWrite = time.Second
)

func newTestClient(t *testing.T, responses ...string) (*Client, *fakeTransport) {
	t.Helper()
	tr := &fakeTransport{responses: responses}
	return New(tr, '0', testRead, testWrite), tr
}

// Golden transactions against the SQ-521's documented command map. Each response
// string is what the sensor puts on the wire in reply to the preceding command.
func TestClientMeasureGolden(t *testing.T) {
	tests := []struct {
		name      string
		sub       string
		responses []string
		wantCmds  []string
		want      []float64
	}{
		{
			name: "0M! canopy ppfd",
			sub:  "",
			// "00011" = ready in 1 s, 1 value; then the bare-address service
			// request; then the data once we ask for it.
			responses: []string{"00011\r\n0\r\n", "0+156.9289\r\n"},
			wantCmds:  []string{"0M!", "0D0!"},
			want:      []float64{156.9289},
		},
		{
			name:      "0M1! detector millivolts",
			sub:       "1",
			responses: []string{"00011\r\n0\r\n", "0+15.9124\r\n"},
			wantCmds:  []string{"0M1!", "0D0!"},
			want:      []float64{15.9124},
		},
		{
			name:      "0M4! tilt from vertical",
			sub:       "4",
			responses: []string{"00011\r\n0\r\n", "0+1.2\r\n"},
			wantCmds:  []string{"0M4!", "0D0!"},
			want:      []float64{1.2},
		},
		{
			// ttt = 000: data is ready immediately and there is no service
			// request to wait for.
			name:      "immediate data, no service request",
			sub:       "",
			responses: []string{"00001\r\n", "0+156.9289\r\n"},
			wantCmds:  []string{"0M!", "0D0!"},
			want:      []float64{156.9289},
		},
		{
			// Whether 0M! reports more than PPFD is unknown precisely because
			// the Python discarded everything past the first value.
			name:      "multi-value D0",
			sub:       "",
			responses: []string{"00013\r\n0\r\n", "0+156.9289+15.9124+1.2\r\n"},
			wantCmds:  []string{"0M!", "0D0!"},
			want:      []float64{156.9289, 15.9124, 1.2},
		},
		{
			name:      "negative reading in darkness",
			sub:       "",
			responses: []string{"00011\r\n0\r\n", "0-0.1147\r\n"},
			wantCmds:  []string{"0M!", "0D0!"},
			want:      []float64{-0.1147},
		},
		{
			name:      "bare LF terminators",
			sub:       "",
			responses: []string{"00011\n0\n", "0+156.9289\n"},
			wantCmds:  []string{"0M!", "0D0!"},
			want:      []float64{156.9289},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, tr := newTestClient(t, tc.responses...)
			got, err := c.Measure(t.Context(), tc.sub)
			if err != nil {
				t.Fatalf("Measure(%q): %v", tc.sub, err)
			}
			assertFloats(t, got, tc.want)
			assertCommands(t, tr, tc.wantCmds)
			if tr.flushes == 0 {
				t.Error("transaction did not flush the input buffer before writing")
			}
			if c.MissedServiceRequests() != 0 {
				t.Errorf("MissedServiceRequests() = %d, want 0", c.MissedServiceRequests())
			}
		})
	}
}

func TestClientVerifyGolden(t *testing.T) {
	// 0V! returns the calibration coefficients; the first is the multiplier the
	// daemon publishes as cal_factor.
	c, tr := newTestClient(t, "00013\r\n0\r\n", "0+0.05387+0.0+0.0\r\n")
	got, err := c.Verify(t.Context())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertFloats(t, got, []float64{0.05387, 0, 0})
	assertCommands(t, tr, []string{"0V!", "0D0!"})
}

func TestClientIdentifyGolden(t *testing.T) {
	c, tr := newTestClient(t, "013APOGEE  SQ-5211043043\r\n")
	got, err := c.Identify(t.Context())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	want := Identity{
		Address: '0', Version: "13", Vendor: "APOGEE",
		Model: "SQ-521", Firmware: "104", Serial: "3043",
	}
	if got != want {
		t.Fatalf("Identify() = %+v, want %+v", got, want)
	}
	assertCommands(t, tr, []string{"0I!"})
}

func TestClientIdentifyShortLine(t *testing.T) {
	c, tr := newTestClient(t, "013APOGEE\r\n")
	_, err := c.Identify(t.Context())
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Identify error = %v, want ErrProtocol", err)
	}
	if tr.flushes < 2 {
		t.Errorf("flushes = %d, want a resync flush after the malformed identity", tr.flushes)
	}
}

// The header defect: garbage must surface as ErrProtocol and trigger a resync,
// never as a fabricated (1 s, 1 value) guess that sends D0! into the middle of
// somebody else's response.
func TestClientMeasureMalformedHeader(t *testing.T) {
	tests := []struct {
		name      string
		responses []string
	}{
		{name: "noise", responses: []string{"?????\r\n", "0+156.9289\r\n"}},
		{name: "truncated header", responses: []string{"001\r\n", "0+156.9289\r\n"}},
		{name: "data line where the header belongs", responses: []string{"0+156.9289\r\n", "0+1.1\r\n"}},
		{name: "adapter banner", responses: []string{"SDI-12 USB Adapter v1.0\r\n", "0+1.1\r\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, tr := newTestClient(t, tc.responses...)
			got, err := c.Measure(t.Context(), "")
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Measure error = %v, want ErrProtocol", err)
			}
			if got != nil {
				t.Fatalf("Measure returned %v alongside its error; a malformed header must not be guessed at", got)
			}
			// The fabricated-header bug was only observable through its
			// consequence: a D0! sent on the strength of the guess.
			assertCommands(t, tr, []string{"0M!"})
			if tr.flushes < 2 {
				t.Errorf("flushes = %d, want a resync flush after the protocol error", tr.flushes)
			}
		})
	}
}

func TestClientMeasureSilence(t *testing.T) {
	c, tr := newTestClient(t) // nothing queued: the sensor never answers
	_, err := c.Measure(t.Context(), "")
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Measure error = %v, want ErrNoResponse", err)
	}
	// Silence is not a desync, so there is nothing to drain.
	if tr.flushes != 1 {
		t.Errorf("flushes = %d, want only the transaction's opening flush", tr.flushes)
	}
	assertCommands(t, tr, []string{"0M!"})
}

// An SQ-521 below serial 3033 has no tilt sensor and answers 0M4! with a zero
// value count. That is a capability answer, not a fault.
func TestClientMeasureZeroValues(t *testing.T) {
	c, tr := newTestClient(t, "00000\r\n")
	_, err := c.Measure(t.Context(), "4")
	if !errors.Is(err, ErrNoValues) {
		t.Fatalf("Measure error = %v, want ErrNoValues", err)
	}
	// No D0! may follow: there is nothing to collect and reading anyway would
	// consume the next command's response.
	assertCommands(t, tr, []string{"0M4!"})
	if tr.flushes != 1 {
		t.Errorf("flushes = %d, want no resync for a well-formed zero-value reply", tr.flushes)
	}
}

func TestClientMeasureDataContinuation(t *testing.T) {
	tests := []struct {
		name      string
		responses []string
		wantCmds  []string
		want      []float64
	}{
		{
			name: "D0 then D1",
			responses: []string{
				"00014\r\n0\r\n",
				"0+1.1+2.2+3.3\r\n",
				"0+4.4\r\n",
			},
			wantCmds: []string{"0M!", "0D0!", "0D1!"},
			want:     []float64{1.1, 2.2, 3.3, 4.4},
		},
		{
			// A bare address means "no more data"; keep what we already have
			// instead of failing the whole poll.
			name: "sensor runs out early",
			responses: []string{
				"00014\r\n0\r\n",
				"0+1.1\r\n",
				"0\r\n",
			},
			wantCmds: []string{"0M!", "0D0!", "0D1!"},
			want:     []float64{1.1},
		},
		{
			// A lying value count must not turn into an unbounded walk.
			name: "capped at three D commands",
			responses: []string{
				"00019\r\n0\r\n",
				"0+1.1\r\n", "0+2.2\r\n", "0+3.3\r\n", "0+4.4\r\n",
			},
			wantCmds: []string{"0M!", "0D0!", "0D1!", "0D2!"},
			want:     []float64{1.1, 2.2, 3.3},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, tr := newTestClient(t, tc.responses...)
			got, err := c.Measure(t.Context(), "")
			if err != nil {
				t.Fatalf("Measure: %v", err)
			}
			assertFloats(t, got, tc.want)
			assertCommands(t, tr, tc.wantCmds)
		})
	}
}

// Waiting for the service request: absence is survivable, but a *different*
// line means somebody else is on the wire.
func TestClientServiceRequestHandling(t *testing.T) {
	t.Run("missing service request proceeds anyway", func(t *testing.T) {
		// We have already waited at least ttt, so D0! is legitimate. Some
		// adapters simply swallow the notification.
		c, tr := newTestClient(t, "00011\r\n", "0+156.9289\r\n")
		got, err := c.Measure(t.Context(), "")
		if err != nil {
			t.Fatalf("Measure: %v", err)
		}
		assertFloats(t, got, []float64{156.9289})
		assertCommands(t, tr, []string{"0M!", "0D0!"})
		if c.MissedServiceRequests() != 1 {
			t.Errorf("MissedServiceRequests() = %d, want 1", c.MissedServiceRequests())
		}
	})

	t.Run("stray blank line counts as missing", func(t *testing.T) {
		c, _ := newTestClient(t, "00011\r\n\r\n", "0+156.9289\r\n")
		got, err := c.Measure(t.Context(), "")
		if err != nil {
			t.Fatalf("Measure: %v", err)
		}
		assertFloats(t, got, []float64{156.9289})
		if c.MissedServiceRequests() != 1 {
			t.Errorf("MissedServiceRequests() = %d, want 1", c.MissedServiceRequests())
		}
	})

	t.Run("wrong line is a desync", func(t *testing.T) {
		tests := []struct {
			name string
			line string
		}{
			{name: "data line", line: "0+9.9"},
			{name: "other address", line: "1"},
			{name: "another header", line: "00011"},
			{name: "noise", line: "@@@"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				c, tr := newTestClient(t, "00011\r\n"+tc.line+"\r\n", "0+156.9289\r\n")
				got, err := c.Measure(t.Context(), "")
				if !errors.Is(err, ErrProtocol) {
					t.Fatalf("Measure error = %v, want ErrProtocol", err)
				}
				if got != nil {
					t.Fatalf("Measure returned %v alongside its error", got)
				}
				// D0! must not be sent into a conversation we do not understand.
				assertCommands(t, tr, []string{"0M!"})
				if tr.flushes < 2 {
					t.Errorf("flushes = %d, want a resync flush", tr.flushes)
				}
			})
		}
	})

	t.Run("late service request in front of the data is skipped", func(t *testing.T) {
		// The notification lost the race with our D0! write and lands ahead of
		// the data line. Skipping it beats flushing, which would have dropped
		// the data instead.
		c, tr := newTestClient(t, "00011\r\n", "0\r\n0+156.9289\r\n")
		got, err := c.Measure(t.Context(), "")
		if err != nil {
			t.Fatalf("Measure: %v", err)
		}
		assertFloats(t, got, []float64{156.9289})
		assertCommands(t, tr, []string{"0M!", "0D0!"})
	})
}

func TestClientMeasureMalformedData(t *testing.T) {
	c, tr := newTestClient(t, "00011\r\n0\r\n", "garbage\r\n")
	_, err := c.Measure(t.Context(), "")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Measure error = %v, want ErrProtocol", err)
	}
	if tr.flushes < 2 {
		t.Errorf("flushes = %d, want a resync flush after malformed data", tr.flushes)
	}
}

func TestClientMeasureNoDataAtAll(t *testing.T) {
	// The header promised a value but D0! returned a bare address.
	c, _ := newTestClient(t, "00011\r\n0\r\n", "0\r\n")
	_, err := c.Measure(t.Context(), "")
	if !errors.Is(err, ErrNoValues) {
		t.Fatalf("Measure error = %v, want ErrNoValues", err)
	}
}

// ErrHeaderNoValues must separate "this unit cannot measure that" from "the
// adapter ate the data response". The daemon retires an optional entity
// permanently on the first, and merely skips a poll on the second, so a
// classification that could not tell them apart would disable tilt for the life
// of the process after one swallowed line.
func TestHeaderNoValuesIsNarrowerThanNoValues(t *testing.T) {
	tests := []struct {
		name       string
		responses  []string
		wantHeader bool
	}{
		{
			// A pre-3033 unit answering 0M4!: the header itself says zero.
			name:       "header declares zero",
			responses:  []string{"00000\r\n"},
			wantHeader: true,
		},
		{
			// The header promised one value; D0! answered with a bare address.
			name:       "header promised, data did not arrive",
			responses:  []string{"00011\r\n0\r\n", "0\r\n"},
			wantHeader: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.responses...)
			_, err := c.Measure(t.Context(), "4")
			if !errors.Is(err, ErrNoValues) {
				t.Fatalf("Measure error = %v, want it to wrap ErrNoValues", err)
			}
			if got := errors.Is(err, ErrHeaderNoValues); got != tc.wantHeader {
				t.Errorf("errors.Is(%v, ErrHeaderNoValues) = %v, want %v", err, got, tc.wantHeader)
			}
		})
	}
}

// A wedged FTDI is the failure the write deadline exists for: without one the
// Python's poll loop blocked forever, taking the MQTT keepalive with it.
func TestClientPortDead(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeTransport)
	}{
		{
			name:  "write fails",
			setup: func(f *fakeTransport) { f.writeErr = errors.New("write /dev/ttyUSB0: input/output error") },
		},
		{
			// A four-byte command at 9600 baud takes microseconds, so a write
			// that cannot drain is a wedged adapter, never a quiet bus.
			name:  "write deadline expires",
			setup: func(f *fakeTransport) { f.writeErr = os.ErrDeadlineExceeded },
		},
		{
			name: "fd closed under us",
			setup: func(f *fakeTransport) {
				f.responses = nil
				f.readErr = io.EOF
			},
		},
		{
			name: "device unplugged mid-poll",
			setup: func(f *fakeTransport) {
				// The header arrives, then the USB device disappears before the
				// data can be collected.
				f.responses = []string{"00011\r\n0\r\n"}
				f.readErr = errors.New("read /dev/ttyUSB0: no such device")
			},
		},
		{
			name:  "flush fails",
			setup: func(f *fakeTransport) { f.flushErr = errors.New("ioctl: bad file descriptor") },
		},
		{
			name:  "set deadline fails",
			setup: func(f *fakeTransport) { f.setDeadlineErr = os.ErrClosed },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTransport{responses: []string{"00011\r\n0\r\n", "0+156.9289\r\n"}}
			tc.setup(tr)
			c := New(tr, '0', testRead, testWrite)
			if _, err := c.Measure(t.Context(), ""); !errors.Is(err, ErrPortDead) {
				t.Fatalf("Measure error = %v, want ErrPortDead", err)
			}
		})
	}
}

// The USB device can vanish at any point in a transaction; each stage must
// surface it as ErrPortDead rather than as a soft "no reading this poll".
func TestClientPortDiesMidTransaction(t *testing.T) {
	tests := []struct {
		name      string
		responses []string
		wantCmds  []string
	}{
		{
			name:      "while awaiting the service request",
			responses: []string{"00011\r\n"},
			wantCmds:  []string{"0M!"},
		},
		{
			name:      "while collecting a continuation",
			responses: []string{"00014\r\n0\r\n", "0+1.1\r\n"},
			wantCmds:  []string{"0M!", "0D0!", "0D1!"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTransport{
				responses: tc.responses,
				readErr:   errors.New("read /dev/ttyUSB0: input/output error"),
			}
			c := New(tr, '0', testRead, testWrite)
			_, err := c.Measure(t.Context(), "")
			if !errors.Is(err, ErrPortDead) {
				t.Fatalf("Measure error = %v, want ErrPortDead", err)
			}
			assertCommands(t, tr, tc.wantCmds)
		})
	}
}

func TestClientContextCancellation(t *testing.T) {
	t.Run("cancelled before the transaction", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		c, tr := newTestClient(t, "00011\r\n0\r\n", "0+156.9289\r\n")
		_, err := c.Measure(ctx, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Measure error = %v, want context.Canceled", err)
		}
		if len(tr.writes) != 0 {
			t.Fatalf("wrote %q on a cancelled context", tr.writes)
		}
	})

	t.Run("cancelled mid-transaction", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		tr := &fakeTransport{responses: []string{"00011\r\n0\r\n", "0+156.9289\r\n"}}
		// Cancel the moment the measurement command is on the wire; the header
		// read must not proceed.
		tr.onWrite = func(n int) {
			if n == 1 {
				cancel()
			}
		}
		c := New(tr, '0', testRead, testWrite)
		_, err := c.Measure(ctx, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Measure error = %v, want context.Canceled", err)
		}
		assertCommands(t, tr, []string{"0M!"})
	})

	t.Run("cancelled before the data command", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		tr := &fakeTransport{responses: []string{"00011\r\n0\r\n", "0+156.9289\r\n"}}
		tr.onWrite = func(n int) {
			if n == 2 { // the D0!
				cancel()
			}
		}
		c := New(tr, '0', testRead, testWrite)
		_, err := c.Measure(ctx, "")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Measure error = %v, want context.Canceled", err)
		}
	})

	t.Run("identify honours cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		c, tr := newTestClient(t, "013APOGEE  SQ-5211043043\r\n")
		if _, err := c.Identify(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Identify error = %v, want context.Canceled", err)
		}
		if len(tr.writes) != 0 {
			t.Fatalf("wrote %q on a cancelled context", tr.writes)
		}
	})
}

// A context deadline tighter than the per-read budget must win, or a poll cycle
// could overrun by the full read timeout on every step.
func TestClientContextDeadlineClampsBudget(t *testing.T) {
	tr := &fakeTransport{responses: []string{"00011\r\n0\r\n", "0+156.9289\r\n"}}
	c := New(tr, '0', time.Hour, time.Hour)

	ctxDeadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(t.Context(), ctxDeadline)
	defer cancel()

	if _, err := c.Measure(ctx, ""); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if len(tr.deadlines) == 0 {
		t.Fatal("no deadline was ever set on the transport")
	}
	for i, d := range tr.deadlines {
		if d.After(ctxDeadline) {
			t.Fatalf("deadline %d is %v, past the context deadline %v", i, d, ctxDeadline)
		}
	}
}

func TestClientAppliesWriteDeadline(t *testing.T) {
	tr := &fakeTransport{responses: []string{"013APOGEE  SQ-5211043043\r\n"}}
	c := New(tr, '0', time.Hour, 25*time.Millisecond)

	before := time.Now()
	if _, err := c.Identify(t.Context()); err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if len(tr.deadlines) == 0 {
		t.Fatal("no deadline set before the write")
	}
	// The first deadline belongs to the write and must reflect the short write
	// budget, not the hour-long read budget.
	if got := tr.deadlines[0]; got.After(before.Add(time.Second)) {
		t.Fatalf("write deadline %v is far past the 25ms write timeout (now was %v)", got, before)
	}
}

func TestClientResync(t *testing.T) {
	t.Run("drains until quiet", func(t *testing.T) {
		tr := &fakeTransport{}
		tr.pending.WriteString("leftover garbage from an abandoned poll\r\n")
		c := New(tr, '0', testRead, testWrite)
		if err := c.Resync(t.Context()); err != nil {
			t.Fatalf("Resync: %v", err)
		}
		if tr.flushes != 1 {
			t.Errorf("flushes = %d, want 1", tr.flushes)
		}
		if tr.pending.Len() != 0 {
			t.Errorf("%d bytes still buffered after Resync", tr.pending.Len())
		}
	})

	t.Run("drops retained bytes", func(t *testing.T) {
		tr := &fakeTransport{responses: []string{"00011\r\n0\r\n"}}
		c := New(tr, '0', testRead, testWrite)
		if err := c.write(t.Context(), "0M!"); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := c.readLine(t.Context(), testRead); err != nil {
			t.Fatalf("readLine: %v", err)
		}
		if c.lr.Buffered() == 0 {
			t.Fatal("expected the service request to be retained")
		}
		if err := c.Resync(t.Context()); err != nil {
			t.Fatalf("Resync: %v", err)
		}
		if c.lr.Buffered() != 0 {
			t.Errorf("Buffered() = %d after Resync; FlushInput cannot reach these bytes, "+
				"so Resync must Reset the line reader", c.lr.Buffered())
		}
	})

	t.Run("dead port reported", func(t *testing.T) {
		tr := &fakeTransport{readErr: io.EOF}
		c := New(tr, '0', testRead, testWrite)
		if err := c.Resync(t.Context()); !errors.Is(err, ErrPortDead) {
			t.Fatalf("Resync error = %v, want ErrPortDead", err)
		}
	})

	t.Run("endless chatter gives up without erroring", func(t *testing.T) {
		// A port that never goes quiet must not hang Resync. Giving up is not
		// an error: the next transaction opens with its own flush, and a real
		// fault will resurface as a protocol error there.
		tr := &chatterTransport{}
		c := New(tr, '0', testRead, testWrite)
		if err := c.Resync(t.Context()); err != nil {
			t.Fatalf("Resync: %v", err)
		}
		if tr.reads > drainReads {
			t.Fatalf("Resync performed %d reads, want at most %d", tr.reads, drainReads)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		tr := &fakeTransport{}
		c := New(tr, '0', testRead, testWrite)
		if err := c.Resync(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Resync error = %v, want context.Canceled", err)
		}
	})
}

// After a desync the very next transaction must work: this is the regression the
// Python could not pass, since nothing anywhere realigned the port.
func TestClientRecoversAfterDesync(t *testing.T) {
	tr := &fakeTransport{responses: []string{
		"noise\r\n",      // breaks the first measurement
		"00011\r\n0\r\n", // second measurement's header
		"0+156.9289\r\n", // its data
	}}
	c := New(tr, '0', testRead, testWrite)

	if _, err := c.Measure(t.Context(), ""); !errors.Is(err, ErrProtocol) {
		t.Fatalf("first Measure error = %v, want ErrProtocol", err)
	}
	got, err := c.Measure(t.Context(), "")
	if err != nil {
		t.Fatalf("second Measure: %v", err)
	}
	assertFloats(t, got, []float64{156.9289})
}

func TestClientDefaultsAndValidation(t *testing.T) {
	t.Run("zero address defaults to the factory address", func(t *testing.T) {
		c := New(&fakeTransport{}, 0, 0, 0)
		if c.Address() != '0' {
			t.Errorf("Address() = %q, want '0'", string(c.Address()))
		}
		if c.readTimeout != defaultReadTimeout || c.writeTimeout != defaultWriteTimeout {
			t.Errorf("timeouts = (%v, %v), want the package defaults", c.readTimeout, c.writeTimeout)
		}
	})

	t.Run("non-default address is used throughout", func(t *testing.T) {
		// Address 3: the commands, the service request and the data echo all
		// carry it.
		tr := &fakeTransport{responses: []string{"30011\r\n3\r\n", "3+1.1\r\n"}}
		c := New(tr, '3', testRead, testWrite)
		got, err := c.Measure(t.Context(), "")
		if err != nil {
			t.Fatalf("Measure: %v", err)
		}
		assertFloats(t, got, []float64{1.1})
		assertCommands(t, tr, []string{"3M!", "3D0!"})
	})

	t.Run("invalid sub-command rejected before any I/O", func(t *testing.T) {
		for _, sub := range []string{"0", "10", "x", "!", " ", "-1"} {
			tr := &fakeTransport{}
			c := New(tr, '0', testRead, testWrite)
			if _, err := c.Measure(t.Context(), sub); err == nil {
				t.Errorf("Measure(%q) accepted an invalid sub-command", sub)
			}
			if len(tr.writes) != 0 {
				t.Errorf("Measure(%q) wrote %q despite the invalid sub-command", sub, tr.writes)
			}
		}
	})
}

// The header's value count is a length field read straight off the wire and
// handed to make(). Unbounded, a 14-byte noise line shaped
// <byte><3 digits><10+ digits> takes the daemon down rather than merely
// confusing it: ten nines reserves 80 GB and dies with a *fatal* runtime
// out-of-memory that no recover() can catch, twenty overflows int to a negative
// capacity and panics in makeslice, and nine reserves 8 GB — an OOM-kill on a
// Pi. That is strictly worse than the fabricated-header defect this rewrite
// exists to fix: the Python invented a value, this kills the process.
func TestClientMeasureRejectsUnboundedValueCount(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "one hundred values", header: "0001100"},
		{name: "nine digits reserves 8GB", header: "0001" + strings.Repeat("9", 9)},
		{name: "ten digits is a fatal oom", header: "0001" + strings.Repeat("9", 10)},
		{name: "twenty digits overflows int", header: "0001" + strings.Repeat("9", 20)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, tr := newTestClient(t, tc.header+"\r\n", "0+1.1\r\n")
			got, err := c.Measure(t.Context(), "")
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("Measure error = %v, want ErrProtocol", err)
			}
			if got != nil {
				t.Fatalf("Measure returned %v alongside its error", got)
			}
			assertCommands(t, tr, []string{"0M!"})
			if tr.flushes < 2 {
				t.Errorf("flushes = %d, want a resync flush after the protocol error", tr.flushes)
			}
		})
	}
}

// collect's reservation is clamped independently of the codec's range check.
// That second bound is unreachable through Measure by construction — which is
// precisely why it needs driving directly, since a guard no test can reach is a
// guard that quietly stops working. Without it a count the parser can no longer
// produce still reserves 8 TB and dies where no recover() helps.
func TestClientCollectClampsReservation(t *testing.T) {
	tr := &fakeTransport{responses: []string{"0+1.1\r\n", "0\r\n"}}
	c := New(tr, '0', testRead, testWrite)

	got, err := c.collect(t.Context(), "0M!", 1<<40, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	assertFloats(t, got, []float64{1.1})
}

// Spec step 4 is "Malformed => Resync + ErrProtocol", and ErrProtocol's own doc
// comment promises the transaction drains the port before returning. That held
// only for ErrProtocol raised by the codec. [LineReader] raises its own for an
// over-long or terminator-less line, and four sites returned it raw. Those are
// precisely the case Resync exists for — an adapter that is *still streaming* —
// where next poll's flush-then-write races the tail of the garbage.
func TestClientResyncsOnFramerProtocolError(t *testing.T) {
	// No terminator and past MaxLineLen, so the framer gives up on its own; no
	// parser is ever reached.
	noise := strings.Repeat("x", 200)

	tests := []struct {
		name      string
		responses []string
		run       func(context.Context, *Client) error
	}{
		{
			name:      "measurement header",
			responses: []string{noise},
			run:       func(ctx context.Context, c *Client) error { _, err := c.Measure(ctx, ""); return err },
		},
		{
			name:      "service request wait",
			responses: []string{"00011\r\n" + noise},
			run:       func(ctx context.Context, c *Client) error { _, err := c.Measure(ctx, ""); return err },
		},
		{
			name:      "data line",
			responses: []string{"00011\r\n0\r\n", noise},
			run:       func(ctx context.Context, c *Client) error { _, err := c.Measure(ctx, ""); return err },
		},
		{
			name:      "identify",
			responses: []string{noise},
			run:       func(ctx context.Context, c *Client) error { _, err := c.Identify(ctx); return err },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTransport{responses: tc.responses}
			c := New(tr, '0', testRead, testWrite)
			if err := tc.run(t.Context(), c); !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v, want ErrProtocol", err)
			}
			if tr.flushes < 2 {
				t.Errorf("flushes = %d, want the transaction's opening flush plus a resync drain; "+
					"a framer-level ErrProtocol means we are mid-frame just as much as a codec-level one",
					tr.flushes)
			}
		})
	}
}

// The service-request budget is the sensor's own ttt plus a second of slack.
// Every golden uses ttt=001 against an instant fake, so `ready` never influences
// an assertion and mutating the budget to c.readTimeout leaves the whole suite
// green — while on a long-conversion sensor that mutant puts aD0! on the bus
// ~118 s early, every poll.
func TestClientServiceRequestBudgetTracksTTT(t *testing.T) {
	const ttt = 120 * time.Second
	tr := &fakeTransport{responses: []string{"01201\r\n0\r\n", "0+156.9289\r\n"}}
	c := New(tr, '0', testRead, testWrite)

	// A context with no deadline of its own, so nothing clamps the budget.
	before := time.Now()
	if _, err := c.Measure(context.Background(), ""); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	after := time.Now()

	// Deadlines in order: [0] the 0M! write, [1] the header read, [2] the
	// service-request wait, [3..] the collection.
	if len(tr.deadlines) < 3 {
		t.Fatalf("deadlines = %v, want at least 3", tr.deadlines)
	}
	got := tr.deadlines[2]
	want := ttt + serviceRequestSlack
	if got.Before(before.Add(want)) || got.After(after.Add(want)) {
		t.Fatalf("service-request deadline is %v from the start of the call, want %v (ttt+slack). "+
			"%v would mean the budget is c.readTimeout and aD0! goes out %v before the sensor is done",
			got.Sub(before), want, testRead, want-testRead)
	}
}

// A context tighter than the conversion clamps the service-request wait, so it
// expires long before the sensor could possibly be finished. Proceeding to aD0!
// anyway does the two things this package exists to prevent: it puts a command
// on the bus mid-conversion, and it charges missedServiceRequests for what is
// ordinary poll-deadline pressure — so the counter stops meaning "the adapter is
// eating traffic" and starts meaning "your poll interval is tight".
func TestClientContextTruncatedConversionWait(t *testing.T) {
	tr := &fakeTransport{responses: []string{"01201\r\n", "0+156.9289\r\n"}}
	c := New(tr, '0', testRead, testWrite)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	got, err := c.Measure(ctx, "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Measure error = %v, want context.DeadlineExceeded", err)
	}
	if got != nil {
		t.Fatalf("Measure returned %v alongside its error; on real hardware that value is stale", got)
	}
	assertCommands(t, tr, []string{"0M!"})
	if c.MissedServiceRequests() != 0 {
		t.Errorf("MissedServiceRequests() = %d, want 0: the sensor did not miss the notification, "+
			"we stopped listening 119 s early", c.MissedServiceRequests())
	}
}

// A sensor answering aD0! with a bare address is saying "no data" — ErrNoValues,
// meaning "stop asking". Combined with a service request the adapter swallowed,
// the client skipped that bare address as a presumed late notification, re-read
// into silence, and reported ErrNoResponse: "transient, retry". The daemon then
// retries a sensor that answered perfectly well, burning a full readTimeout
// every poll on exactly the adapters that habitually eat notifications.
func TestClientMissedServiceRequestThenNoData(t *testing.T) {
	c, tr := newTestClient(t, "00011\r\n", "0\r\n")
	_, err := c.Measure(t.Context(), "")
	if !errors.Is(err, ErrNoValues) {
		t.Fatalf("Measure error = %v, want ErrNoValues", err)
	}
	if errors.Is(err, ErrNoResponse) {
		t.Fatal("Measure reported silence for a sensor that answered with a well-formed bare address")
	}
	assertCommands(t, tr, []string{"0M!", "0D0!"})
	if c.MissedServiceRequests() != 1 {
		t.Errorf("MissedServiceRequests() = %d, want 1", c.MissedServiceRequests())
	}
}

// Resync had a dedicated io.EOF case producing an error byte-identical to the
// default's. Nothing could tell the two apart, so it was dead code wearing the
// costume of a distinction. Pin that every read fault classifies alike.
func TestResyncClassifiesEveryReadFaultAlike(t *testing.T) {
	faults := []struct {
		name string
		err  error
	}{
		{name: "eof", err: io.EOF},
		{name: "unplugged usb", err: errors.New("read /dev/ttyUSB0: no such device")},
	}
	for _, tc := range faults {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTransport{readErr: tc.err}
			c := New(tr, '0', testRead, testWrite)
			err := c.Resync(t.Context())
			if !errors.Is(err, ErrPortDead) {
				t.Fatalf("Resync error = %v, want ErrPortDead", err)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("Resync error %v lost the underlying cause %v", err, tc.err)
			}
			if got, want := err.Error(), "sdi12: resync: "+ErrPortDead.Error()+": "+tc.err.Error(); got != want {
				t.Fatalf("Resync error = %q, want %q", got, want)
			}
		})
	}
}

// sentinelsOf reports which of the package's four sentinels err wraps.
func sentinelsOf(err error) []error {
	var out []error
	for _, s := range []error{ErrNoResponse, ErrProtocol, ErrNoValues, ErrPortDead} {
		if errors.Is(err, s) {
			out = append(out, s)
		}
	}
	return out
}

// The error contract, pinned. Callers switch on the sentinels to choose between
// "skip this poll", "drain and realign", "stop asking" and "reopen the port", so
// a path wrapping nothing recognisable silently lands in their default case —
// and cancellation, the most common non-nil error during shutdown, is exactly
// such a path.
func TestErrorClassificationContract(t *testing.T) {
	cancelled := func(t *testing.T) context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}

	tests := []struct {
		name string
		// exactly one of these is non-nil; both nil means the error is expected
		// to wrap neither a sentinel nor a context cause.
		wantSentinel error
		wantCtx      error
		run          func(*testing.T) error
	}{
		{
			name:         "silence",
			wantSentinel: ErrNoResponse,
			run: func(t *testing.T) error {
				c, _ := newTestClient(t)
				_, err := c.Measure(t.Context(), "")
				return err
			},
		},
		{
			name:         "malformed header",
			wantSentinel: ErrProtocol,
			run: func(t *testing.T) error {
				c, _ := newTestClient(t, "?????\r\n")
				_, err := c.Measure(t.Context(), "")
				return err
			},
		},
		{
			name:         "unsupported measurement",
			wantSentinel: ErrNoValues,
			run: func(t *testing.T) error {
				c, _ := newTestClient(t, "00000\r\n")
				_, err := c.Measure(t.Context(), "4")
				return err
			},
		},
		{
			name:         "port dead",
			wantSentinel: ErrPortDead,
			run: func(t *testing.T) error {
				tr := &fakeTransport{writeErr: errors.New("write /dev/ttyUSB0: input/output error")}
				c := New(tr, '0', testRead, testWrite)
				_, err := c.Measure(t.Context(), "")
				return err
			},
		},
		{
			name:         "identify on a dead port",
			wantSentinel: ErrPortDead,
			run: func(t *testing.T) error {
				tr := &fakeTransport{flushErr: errors.New("ioctl: bad file descriptor")}
				c := New(tr, '0', testRead, testWrite)
				_, err := c.Identify(t.Context())
				return err
			},
		},
		{
			name:    "cancelled before the transaction",
			wantCtx: context.Canceled,
			run: func(t *testing.T) error {
				c, _ := newTestClient(t, "00011\r\n0\r\n", "0+1.1\r\n")
				_, err := c.Measure(cancelled(t), "")
				return err
			},
		},
		{
			name:    "cancelled identify",
			wantCtx: context.Canceled,
			run: func(t *testing.T) error {
				c, _ := newTestClient(t, "013APOGEE  SQ-5211043043\r\n")
				_, err := c.Identify(cancelled(t))
				return err
			},
		},
		{
			name:    "cancelled resync",
			wantCtx: context.Canceled,
			run:     func(t *testing.T) error { return New(&fakeTransport{}, '0', testRead, testWrite).Resync(cancelled(t)) },
		},
		{
			name:    "context truncated the conversion wait",
			wantCtx: context.DeadlineExceeded,
			run: func(t *testing.T) error {
				tr := &fakeTransport{responses: []string{"01201\r\n", "0+1.1\r\n"}}
				c := New(tr, '0', testRead, testWrite)
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				_, err := c.Measure(ctx, "")
				return err
			},
		},
		{
			// The documented exception: argument validation fails before any I/O,
			// so there is no wire outcome to classify and no port state to
			// recover. Wrapping a sentinel here would tell the daemon to drain,
			// reopen or back off over a caller bug that no retry can fix.
			name: "invalid sub-command",
			run: func(t *testing.T) error {
				c, _ := newTestClient(t)
				_, err := c.Measure(t.Context(), "x")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			got := sentinelsOf(err)
			switch {
			case tc.wantSentinel != nil:
				if len(got) != 1 || got[0] != tc.wantSentinel {
					t.Fatalf("%v wraps sentinels %v, want exactly [%v]", err, got, tc.wantSentinel)
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%v wraps a context cause as well as %v", err, tc.wantSentinel)
				}
			case tc.wantCtx != nil:
				if !errors.Is(err, tc.wantCtx) {
					t.Fatalf("%v does not wrap %v", err, tc.wantCtx)
				}
				// A context error must not masquerade as a wire outcome: a
				// caller that reopened the port or backed off on shutdown would
				// be reacting to its own cancellation.
				if len(got) != 0 {
					t.Fatalf("%v wraps sentinels %v; cancellation is not a wire outcome", err, got)
				}
			default:
				if len(got) != 0 {
					t.Fatalf("%v wraps sentinels %v, want none: it fails before any I/O", err, got)
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("%v wraps a context cause", err)
				}
			}
		})
	}
}

func assertCommands(t *testing.T, tr *fakeTransport, want []string) {
	t.Helper()
	if len(tr.writes) != len(want) {
		t.Fatalf("commands = %q, want %q", tr.writes, want)
	}
	for i := range want {
		if tr.writes[i] != want[i] {
			t.Fatalf("command %d = %q, want %q (all: %q)", i, tr.writes[i], want[i], tr.writes)
		}
	}
}
