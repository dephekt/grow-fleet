package sdnotify

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// env builds an injectable getenv over a fixed map. Tests never touch the real
// process environment (except TestNew, which needs to prove New() reads it).
func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// listenAt binds a unixgram socket at addr, which may be a filesystem path or
// an '@'-prefixed abstract-namespace name.
func listenAt(t *testing.T, addr string) *net.UnixConn {
	t.Helper()
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram %q: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// socketPath returns a filesystem path for a test socket.
//
// sockaddr_un.sun_path is 108 bytes on Linux, and t.TempDir() derives its path
// from the (sub)test name, so a long table-driven name can silently push the
// socket past the limit. Fall back to a short os.MkdirTemp when that happens
// rather than failing with an opaque "invalid argument" from bind(2).
func socketPath(t *testing.T) string {
	t.Helper()
	const sunPathMax = 100 // 108 minus NUL and a little headroom
	p := filepath.Join(t.TempDir(), "n.sock")
	if len(p) > sunPathMax {
		dir, err := os.MkdirTemp("", "sdn")
		if err != nil {
			t.Fatalf("mkdir temp: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		p = filepath.Join(dir, "n.sock")
	}
	return p
}

// recv reads exactly one datagram, failing the test if none arrives promptly.
// The send side is synchronous and local, so a short deadline is plenty and
// keeps a broken implementation from hanging the suite.
func recv(t *testing.T, conn *net.UnixConn) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read datagram: %v", err)
	}
	return string(buf[:n])
}

// expectNoDatagram asserts nothing was sent, with a deadline short enough to
// keep the suite fast.
func expectNoDatagram(t *testing.T, conn *net.UnixConn) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 4096)
	n, _, err := conn.ReadFromUnix(buf)
	if err == nil {
		t.Fatalf("unexpected datagram %q", buf[:n])
	}
	// Only a deadline expiry proves "nothing was sent"; any other error means
	// the assertion itself broke.
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("want read timeout, got %v", err)
	}
}

func TestNotifyMethodsSendExactBytes(t *testing.T) {
	tests := []struct {
		name string
		call func(*Notifier) error
		want string
	}{
		{"ready", (*Notifier).Ready, "READY=1"},
		{"stopping", (*Notifier).Stopping, "STOPPING=1"},
		{"watchdog", (*Notifier).Watchdog, "WATCHDOG=1"},
		{"status", func(n *Notifier) error { return n.Status("polling SDI-12") }, "STATUS=polling SDI-12"},
		{"status empty", func(n *Notifier) error { return n.Status("") }, "STATUS="},
		{"status utf8", func(n *Notifier) error { return n.Status("PPFD 812 µmol/s/m²") }, "STATUS=PPFD 812 µmol/s/m²"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sock := socketPath(t)
			conn := listenAt(t, sock)
			n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": sock}))

			if err := tc.call(n); err != nil {
				t.Fatalf("notify: %v", err)
			}
			if got := recv(t, conn); got != tc.want {
				t.Errorf("datagram = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNotifyIsNoOpWithoutSocket(t *testing.T) {
	// A listener exists but the notifier is never told about it: proves the
	// no-op path really sends nothing rather than guessing a default address.
	sock := socketPath(t)
	conn := listenAt(t, sock)

	notifiers := map[string]*Notifier{
		"unset":      NewWithEnv(env(map[string]string{})),
		"empty":      NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": ""})),
		"nil getenv": NewWithEnv(nil),
	}

	for name, n := range notifiers {
		t.Run(name, func(t *testing.T) {
			if n.Enabled() {
				t.Error("Enabled() = true, want false")
			}
			for method, call := range map[string]func() error{
				"Ready":    n.Ready,
				"Stopping": n.Stopping,
				"Watchdog": n.Watchdog,
				"Status":   func() error { return n.Status("ignored") },
			} {
				if err := call(); err != nil {
					t.Errorf("%s() = %v, want nil", method, err)
				}
			}
			expectNoDatagram(t, conn)
		})
	}
}

func TestEnabled(t *testing.T) {
	tests := []struct {
		name string
		sock string
		want bool
	}{
		{"unset", "", false},
		{"path", "/run/systemd/notify", true},
		{"abstract", "@/run/systemd/notify", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": tc.sock}))
			if got := n.Enabled(); got != tc.want {
				t.Errorf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAddrPassesSocketNameVerbatim is the real guard on the '@' pass-through.
//
// It asserts on the address *object*, not on delivery, because delivery cannot
// distinguish the two spellings: syscall.SockaddrUnix treats a leading '@' and
// a leading NUL identically ("isAbstract := n > 0 && (name[0] == '@' ||
// name[0] == '\x00')" in syscall/syscall_linux.go), so a notifier that
// hand-translated '@' -> NUL before dialing would still reach the listener and
// an end-to-end test would stay green. What actually matters is that the name
// handed to the dialer is byte-for-byte $NOTIFY_SOCKET, so that a future change
// of dialer, or a systemd that grows a third spelling, cannot be broken by a
// translation we were never asked to perform.
func TestAddrPassesSocketNameVerbatim(t *testing.T) {
	raws := []string{
		"/run/systemd/notify",     // filesystem path
		"@/run/systemd/notify",    // abstract, '@' spelling (what systemd exports)
		"\x00/run/systemd/notify", // abstract, NUL spelling
		"@",                       // degenerate: '@' with nothing after it
	}

	for _, raw := range raws {
		t.Run(strconv.Quote(raw), func(t *testing.T) {
			n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": raw}))
			addr := n.addr()
			if addr == nil {
				t.Fatalf("addr() = nil, want an address for %q", raw)
			}
			if addr.Name != raw {
				t.Errorf("addr().Name = %q, want %q verbatim", addr.Name, raw)
			}
			if addr.Net != "unixgram" {
				t.Errorf("addr().Net = %q, want %q", addr.Net, "unixgram")
			}
		})
	}

	t.Run("unset", func(t *testing.T) {
		n := NewWithEnv(env(map[string]string{}))
		if addr := n.addr(); addr != nil {
			t.Errorf("addr() = %+v, want nil when NOTIFY_SOCKET is unset", addr)
		}
	})
}

// TestAbstractSocket proves abstract-namespace sockets work end to end. It does
// NOT guard the '@' pass-through — see TestAddrPassesSocketNameVerbatim for why
// no delivery-based test can.
func TestAbstractSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract unix sockets are Linux-only")
	}
	addr := fmt.Sprintf("@sdnotify-test-%d", os.Getpid())
	conn := listenAt(t, addr)

	n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": addr}))
	if err := n.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := recv(t, conn); got != "READY=1" {
		t.Errorf("datagram = %q, want %q", got, "READY=1")
	}
}

func TestSendErrorsWhenSocketMissing(t *testing.T) {
	// Nothing is bound at this path. A configured-but-broken NOTIFY_SOCKET is
	// a real failure and must surface, unlike the unset case.
	sock := filepath.Join(t.TempDir(), "absent.sock")
	n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": sock}))
	if err := n.Ready(); err == nil {
		t.Fatal("Ready() = nil, want dial error")
	}
}

func TestStatusSanitisation(t *testing.T) {
	long := strings.Repeat("a", maxStatusLen+50)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// A newline would be read by systemd as the start of another
			// KEY=VALUE assignment.
			name: "newline folded",
			in:   "serial error\nreopening adapter",
			want: "STATUS=serial error reopening adapter",
		},
		{
			name: "crlf folded to single space",
			in:   "a\r\nb",
			want: "STATUS=a b",
		},
		{
			name: "carriage return folded",
			in:   "a\rb",
			want: "STATUS=a b",
		},
		{
			name: "truncated",
			in:   long,
			want: "STATUS=" + strings.Repeat("a", maxStatusLen),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sock := socketPath(t)
			conn := listenAt(t, sock)
			n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": sock}))

			if err := n.Status(tc.in); err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got := recv(t, conn); got != tc.want {
				t.Errorf("datagram = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStatusTruncationKeepsValidUTF8 guards the rune-boundary cut: the status
// text carries units like "µmol/s/m²", and slicing mid-sequence would put an
// invalid byte on the wire.
func TestStatusTruncationKeepsValidUTF8(t *testing.T) {
	// "µ" is two bytes; repeating it means a naive byte cut at maxStatusLen+1
	// odd offsets would land mid-rune for at least one of these lengths.
	for _, pad := range []int{0, 1} {
		in := strings.Repeat("x", pad) + strings.Repeat("µ", maxStatusLen)
		got := sanitizeStatus(in)
		if !utf8.ValidString(got) {
			t.Errorf("pad=%d: truncated status is not valid UTF-8: %q", pad, got)
		}
		if len(got) > maxStatusLen {
			t.Errorf("pad=%d: len = %d, want <= %d", pad, len(got), maxStatusLen)
		}
		if len(got) < maxStatusLen-utf8.UTFMax {
			t.Errorf("pad=%d: truncated too aggressively: len = %d", pad, len(got))
		}
	}
}

// TestStatusTruncationNeverClearsStatus is the invalid-UTF-8 trap.
//
// "STATUS=" with an empty value is systemd's *clear the status line* command,
// so a truncation that walks all the way back to zero does not degrade the
// status, it erases it — precisely when an operator most needs `systemctl
// status` to say something, since the realistic source of raw bytes in status
// text is an SDI-12 adapter echoing garbage during a fault.
func TestStatusTruncationNeverClearsStatus(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{
			// Nothing in the string is a rune start, so an unbounded walk-back
			// from maxStatusLen runs to 0 and returns "".
			name: "all continuation bytes",
			in:   strings.Repeat("\x80", maxStatusLen+10),
		},
		{
			// Valid text, then a long run of continuation bytes covering the
			// whole walk-back window and far beyond it.
			name: "valid prefix then continuation run",
			in:   "PPFD 812" + strings.Repeat("\x80", maxStatusLen),
		},
		{
			// A truncated 4-byte lead followed by continuation filler: the only
			// rune start within the walk-back window is the 0xF0 lead itself,
			// two bytes before the cut.
			name: "truncated lead byte before the cut",
			in:   strings.Repeat("a", maxStatusLen-2) + "\xf0" + strings.Repeat("\x80", 64),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeStatus(tc.in)
			if got == "" {
				t.Fatal("sanitizeStatus() = \"\": on the wire this is a bare STATUS=, which clears the status line")
			}
			if len(got) > maxStatusLen {
				t.Errorf("len = %d, want <= %d", len(got), maxStatusLen)
			}
			// A valid UTF-8 prefix can never end more than utf8.UTFMax-1 bytes
			// before the cut point, so anything shorter means the walk-back ran
			// past its bound.
			if len(got) < maxStatusLen-(utf8.UTFMax-1) {
				t.Errorf("len = %d, want >= %d: walk-back exceeded utf8.UTFMax-1 bytes",
					len(got), maxStatusLen-(utf8.UTFMax-1))
			}
		})
	}
}

// TestStatusTruncationCutsAtRuneBoundary pins the ordinary mid-rune case with
// exact byte counts, so a fix for the walk-back bound cannot regress into
// "always cut at max".
func TestStatusTruncationCutsAtRuneBoundary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// The byte at maxStatusLen is the second byte of a 2-byte "µ"
			// starting at maxStatusLen-1: cut back one byte.
			name: "two byte rune straddles the cut",
			in:   strings.Repeat("a", maxStatusLen-1) + strings.Repeat("µ", 8),
			want: strings.Repeat("a", maxStatusLen-1),
		},
		{
			// A 4-byte rune starting at maxStatusLen-3: the walk-back must
			// travel the full utf8.UTFMax-1 bytes.
			name: "four byte rune straddles the cut",
			in:   strings.Repeat("a", maxStatusLen-3) + strings.Repeat("😀", 8),
			want: strings.Repeat("a", maxStatusLen-3),
		},
		{
			// A rune starts exactly at the cut: keep the full budget, do not
			// give a byte back.
			name: "rune starts at the cut",
			in:   strings.Repeat("a", maxStatusLen) + strings.Repeat("µ", 8),
			want: strings.Repeat("a", maxStatusLen),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeStatus(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeStatus() len = %d, want %d (bytes differ)", len(got), len(tc.want))
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncated status is not valid UTF-8: %q", got)
			}
		})
	}
}

func TestWatchdogInterval(t *testing.T) {
	self := strconv.Itoa(os.Getpid())
	other := strconv.Itoa(os.Getpid() + 1)

	tests := []struct {
		name    string
		vars    map[string]string
		wantDur time.Duration
		wantOK  bool
	}{
		{
			name:    "valid",
			vars:    map[string]string{"WATCHDOG_USEC": "30000000"},
			wantDur: 30 * time.Second,
			wantOK:  true,
		},
		{
			name:    "valid with matching pid",
			vars:    map[string]string{"WATCHDOG_USEC": "1500000", "WATCHDOG_PID": self},
			wantDur: 1500 * time.Millisecond,
			wantOK:  true,
		},
		{
			// Inherited from an ancestor: the watchdog is not ours to pet.
			name:   "pid mismatch",
			vars:   map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": other},
			wantOK: false,
		},
		{
			name:   "unparseable pid",
			vars:   map[string]string{"WATCHDOG_USEC": "30000000", "WATCHDOG_PID": "not-a-pid"},
			wantOK: false,
		},
		{
			name:   "absent",
			vars:   map[string]string{},
			wantOK: false,
		},
		{
			name:   "empty",
			vars:   map[string]string{"WATCHDOG_USEC": ""},
			wantOK: false,
		},
		{
			name:   "zero",
			vars:   map[string]string{"WATCHDOG_USEC": "0"},
			wantOK: false,
		},
		{
			name:   "negative",
			vars:   map[string]string{"WATCHDOG_USEC": "-1000"},
			wantOK: false,
		},
		{
			name:   "garbage",
			vars:   map[string]string{"WATCHDOG_USEC": "30s"},
			wantOK: false,
		},
		{
			name:   "float",
			vars:   map[string]string{"WATCHDOG_USEC": "30000000.0"},
			wantOK: false,
		},
		{
			name:   "whitespace padded",
			vars:   map[string]string{"WATCHDOG_USEC": " 30000000 "},
			wantOK: false,
		},
		{
			// Would overflow the usec -> nsec multiply into a negative Duration.
			name:   "absurdly large",
			vars:   map[string]string{"WATCHDOG_USEC": "9223372036854775807"},
			wantOK: false,
		},
		{
			name:    "one microsecond",
			vars:    map[string]string{"WATCHDOG_USEC": "1"},
			wantDur: time.Microsecond,
			wantOK:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := NewWithEnv(env(tc.vars))
			gotDur, gotOK := n.WatchdogInterval()
			if gotOK != tc.wantOK {
				t.Fatalf("WatchdogInterval() ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotDur != tc.wantDur {
				t.Errorf("WatchdogInterval() = %v, want %v", gotDur, tc.wantDur)
			}
		})
	}
}

// TestMaxWatchdogUsecValue pins the overflow ceiling to a literal.
//
// Every other test derives its input from the constant, so a mistake in the
// constant's own arithmetic — dividing MaxInt64 by time.Second instead of
// time.Microsecond, say, which tightens the ceiling from ~292 years to ~2.5
// hours and silently disarms the watchdog for any realistic WatchdogSec above
// that — would move the tests along with it and stay invisible.
//
// math.MaxInt64 microseconds is the largest value that survives the usec ->
// nsec multiply inside time.Duration.
func TestMaxWatchdogUsecValue(t *testing.T) {
	const want = 9223372036854775 // math.MaxInt64 / 1000
	if maxWatchdogUsec != want {
		t.Fatalf("maxWatchdogUsec = %d, want %d (%v)", maxWatchdogUsec, want,
			time.Duration(want)*time.Microsecond)
	}
	// And the whole point of the ceiling: the boundary value must still convert
	// to a positive Duration.
	if d := time.Duration(want) * time.Microsecond; d <= 0 {
		t.Fatalf("boundary converts to %v, want positive", d)
	}
}

// TestWatchdogIntervalOverflowBoundary exercises the guard at its exact edge.
// Literal inputs, deliberately: see TestMaxWatchdogUsecValue.
func TestWatchdogIntervalOverflowBoundary(t *testing.T) {
	tests := []struct {
		name    string
		usec    string
		wantDur time.Duration
		wantOK  bool
	}{
		{
			// Exactly at the ceiling: still convertible, so still accepted. A
			// guard written as ">=" rejects this.
			name:    "at ceiling",
			usec:    "9223372036854775",
			wantDur: time.Duration(9223372036854775000),
			wantOK:  true,
		},
		{
			name:    "one below ceiling",
			usec:    "9223372036854774",
			wantDur: time.Duration(9223372036854774000),
			wantOK:  true,
		},
		{
			// One microsecond past: the multiply would wrap negative.
			name:   "one above ceiling",
			usec:   "9223372036854776",
			wantOK: false,
		},
		{
			// A plausible-but-huge WatchdogSec well inside the ceiling. A guard
			// mistakenly scaled by time.Second would cut off at ~9223 seconds
			// and reject this.
			name:    "one day",
			usec:    "86400000000",
			wantDur: 24 * time.Hour,
			wantOK:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := NewWithEnv(env(map[string]string{"WATCHDOG_USEC": tc.usec}))
			gotDur, gotOK := n.WatchdogInterval()
			if gotOK != tc.wantOK {
				t.Fatalf("WatchdogInterval(%s) ok = %v, want %v", tc.usec, gotOK, tc.wantOK)
			}
			if gotDur != tc.wantDur {
				t.Errorf("WatchdogInterval(%s) = %v (%d ns), want %v (%d ns)",
					tc.usec, gotDur, gotDur, tc.wantDur, tc.wantDur)
			}
		})
	}
}

// TestWatchdogIntervalIndependentOfNotifySocket documents that the watchdog
// variables are read on their own: a unit can be Type=simple with WatchdogSec
// set, and conversely NOTIFY_SOCKET alone arms nothing.
func TestWatchdogIntervalIndependentOfNotifySocket(t *testing.T) {
	n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": "/run/systemd/notify"}))
	if d, ok := n.WatchdogInterval(); ok {
		t.Errorf("WatchdogInterval() = (%v, true), want disarmed", d)
	}
}

// TestNew proves New() reads the real process environment. t.Setenv restores
// the previous value and blocks t.Parallel, so this stays contained.
func TestNew(t *testing.T) {
	sock := socketPath(t)
	conn := listenAt(t, sock)
	t.Setenv("NOTIFY_SOCKET", sock)
	t.Setenv("WATCHDOG_USEC", "20000000")
	t.Setenv("WATCHDOG_PID", strconv.Itoa(os.Getpid()))

	n := New()
	if !n.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}
	if err := n.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := recv(t, conn); got != "READY=1" {
		t.Errorf("datagram = %q, want %q", got, "READY=1")
	}
	if d, ok := n.WatchdogInterval(); !ok || d != 20*time.Second {
		t.Errorf("WatchdogInterval() = (%v, %v), want (20s, true)", d, ok)
	}
}

// TestSendIsBoundedWhenReceiveQueueIsFull is the unbounded-block trap.
//
// A bound-but-never-drained notify socket is exactly what a wedged or stopped
// systemd looks like from in here. Without a write deadline the send parks
// forever on a full receive queue — and this same code path sits on the
// Stopping() shutdown path and in the watchdog-ping goroutine, so an indefinite
// park there is the one thing this daemon promises never to do.
//
// The listener is never read from, so ~500 KB of datagrams fills the peer's
// SO_RCVBUF and the next write has nowhere to go.
func TestSendIsBoundedWhenReceiveQueueIsFull(t *testing.T) {
	sock := socketPath(t)
	listenAt(t, sock) // deliberately never drained
	n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": sock}))

	// Each Status() is ~1 KB, so the queue fills in a few hundred iterations;
	// the cap is only there to stop a runaway loop if a kernel somewhere never
	// applies back pressure.
	filler := strings.Repeat("x", maxStatusLen)
	const maxSends = 100000

	errc := make(chan error, 1)
	go func() {
		for i := 0; i < maxSends; i++ {
			if err := n.Status(filler); err != nil {
				errc <- err
				return
			}
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatalf("%d sends never blocked: the socket applied no back pressure, so this test proves nothing", maxSends)
		}
		if !errors.Is(err, ErrNotifyTimeout) {
			t.Fatalf("send error = %v, want one wrapping ErrNotifyTimeout", err)
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("send error = %v, want it to preserve os.ErrDeadlineExceeded", err)
		}
	case <-time.After(10 * time.Second):
		// The send goroutine is parked in write(2) and will stay parked; the
		// test binary exits regardless.
		t.Fatalf("send blocked for >10s on a full receive queue: no write deadline is set (writeTimeout = %v)", writeTimeout)
	}
}

// TestWriteTimeoutIsShort guards the deadline's magnitude, not just its
// existence: a notification is a courtesy to the service manager, and the
// shutdown path must not stall on one. Anything approaching a second is long
// enough to be felt.
func TestWriteTimeoutIsShort(t *testing.T) {
	if writeTimeout <= 0 {
		t.Fatalf("writeTimeout = %v, want a positive deadline", writeTimeout)
	}
	if writeTimeout > 500*time.Millisecond {
		t.Errorf("writeTimeout = %v, want <= 500ms: this blocks shutdown", writeTimeout)
	}
}

// TestSequentialNotifications covers the common runtime pattern — ready, then
// periodic status/watchdog, then stopping — and confirms each call dials
// afresh without leaking or reordering datagrams.
func TestSequentialNotifications(t *testing.T) {
	sock := socketPath(t)
	conn := listenAt(t, sock)
	n := NewWithEnv(env(map[string]string{"NOTIFY_SOCKET": sock}))

	steps := []struct {
		call func() error
		want string
	}{
		{n.Ready, "READY=1"},
		{func() error { return n.Status("PPFD 812") }, "STATUS=PPFD 812"},
		{n.Watchdog, "WATCHDOG=1"},
		{n.Watchdog, "WATCHDOG=1"},
		{n.Stopping, "STOPPING=1"},
	}
	for i, step := range steps {
		if err := step.call(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if got := recv(t, conn); got != step.want {
			t.Errorf("step %d: datagram = %q, want %q", i, got, step.want)
		}
	}
}
