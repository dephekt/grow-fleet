// Package sdnotify implements the small slice of systemd's sd_notify(3)
// protocol this daemon actually uses: READY=1, STOPPING=1, WATCHDOG=1 and
// STATUS=<text>.
//
// This is deliberately hand-rolled rather than pulling in
// github.com/coreos/go-systemd. The protocol is a single datagram written to
// the AF_UNIX socket named by $NOTIFY_SOCKET — roughly fifty lines of net, os
// and strconv — and the whole premise of this binary is a minimal static
// artifact with a supply-chain surface small enough to audit by reading it.
//
// Everything degrades to a no-op when $NOTIFY_SOCKET is unset, so the same
// binary runs identically under systemd, under a bare shell, and in the
// smoke-test modes (--once, --check-config) that nobody supervises.
package sdnotify

import (
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// writeTimeout bounds the datagram write.
//
// A notify socket is a bounded receive queue owned by another process. If
// systemd is wedged, stopped, or merely slow to drain, an unbounded write parks
// the calling goroutine forever — and this code path runs on the shutdown path
// (Stopping) and in the watchdog-ping goroutine, where an indefinite park is
// precisely the failure the watchdog exists to catch. Notifications are a
// courtesy to the service manager, so we would rather miss one than stall.
const writeTimeout = 100 * time.Millisecond

// ErrNotifyTimeout reports that a notification could not be handed to the
// service manager within writeTimeout.
//
// It is a *missed notification*, not a daemon fault: the socket was there and
// the daemon is healthy, the manager just was not draining. Callers should log
// it and carry on —
//
//	if err := n.Watchdog(); err != nil && !errors.Is(err, sdnotify.ErrNotifyTimeout) {
//	        return err
//	}
//
// — rather than treat it as fatal. It is returned rather than swallowed so the
// caller can log the miss; this package deliberately owns no logger.
var ErrNotifyTimeout = errors.New("sdnotify: notification write timed out")

// maxStatusLen caps STATUS= payloads. systemd truncates long status strings
// anyway, and an unbounded string (say, a wrapped serial-port error carrying a
// whole device path plus errno text) risks exceeding the socket's datagram
// size limit, which would fail the send outright and lose the notification.
const maxStatusLen = 1024

// maxWatchdogUsec is the largest WATCHDOG_USEC we will convert. Above this the
// microsecond-to-nanosecond multiply overflows time.Duration's int64 and would
// silently yield a negative interval.
//
// The divisor is time.Microsecond because WATCHDOG_USEC is microseconds and a
// Duration counts nanoseconds: the ceiling is MaxInt64/1000 µs, i.e. 9223372036854775
// µs — about 292 years. Scaling it by anything else (time.Second, say, which
// would put the ceiling at ~2.5 hours) does not overflow, it just quietly
// disarms the watchdog for ordinary WatchdogSec values, which is why
// TestMaxWatchdogUsecValue pins the number to a literal.
//
// systemd never sets anything remotely near 292 years, so a value above the
// ceiling is garbage rather than a real timeout.
const maxWatchdogUsec = int64(math.MaxInt64) / int64(time.Microsecond)

// Notifier sends readiness/status notifications to the service manager.
//
// The zero value is not usable; construct one with New or NewWithEnv. A
// Notifier holds no connection: each notification dials, writes one datagram
// and closes, which is what sd_notify(3) itself does and keeps the type safe
// to share across goroutines.
type Notifier struct {
	// getenv reads the environment. Injected so tests can drive the notifier
	// without mutating the real process environment (and so a future --env-file
	// style override could feed it something else).
	getenv func(string) string
	// pid is captured at construction for the WATCHDOG_PID check.
	pid int
}

// New returns a Notifier reading the real process environment.
func New() *Notifier { return NewWithEnv(os.Getenv) }

// NewWithEnv returns a Notifier that resolves NOTIFY_SOCKET, WATCHDOG_USEC and
// WATCHDOG_PID through getenv. A nil getenv behaves like a completely empty
// environment, i.e. every method becomes a no-op.
func NewWithEnv(getenv func(string) string) *Notifier {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return &Notifier{getenv: getenv, pid: os.Getpid()}
}

// Enabled reports whether a service manager is listening, i.e. whether
// NOTIFY_SOCKET is set. Callers use it to decide whether to arm a watchdog
// ticker at all, not to guard the Notify calls — those are already no-ops.
func (n *Notifier) Enabled() bool { return n.socket() != "" }

// Ready signals that startup is complete (Type=notify units block on this).
func (n *Notifier) Ready() error { return n.send("READY=1") }

// Stopping signals that shutdown has begun, so systemd stops counting the unit
// as live and does not trip the watchdog while we drain.
func (n *Notifier) Stopping() error { return n.send("STOPPING=1") }

// Watchdog pets WatchdogSec. Send at roughly half WatchdogInterval.
func (n *Notifier) Watchdog() error { return n.send("WATCHDOG=1") }

// Status sets the one-line description shown by `systemctl status`.
func (n *Notifier) Status(s string) error { return n.send("STATUS=" + sanitizeStatus(s)) }

// WatchdogInterval reports the watchdog timeout systemd configured for this
// unit, and whether the watchdog is armed for *us*.
//
// systemd exports the timeout as WATCHDOG_USEC (microseconds) and, when
// WatchdogSec is set, WATCHDOG_PID as the pid it expects pings from. Because
// both variables are inherited by anything we exec, a mismatched WATCHDOG_PID
// means the watchdog belongs to an ancestor process and pinging it from here
// would be wrong — so report it as disarmed.
func (n *Notifier) WatchdogInterval() (time.Duration, bool) {
	if raw := n.getenv("WATCHDOG_PID"); raw != "" {
		pid, err := strconv.Atoi(raw)
		if err != nil || pid != n.pid {
			return 0, false
		}
	}
	usec, err := strconv.ParseInt(n.getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 || usec > maxWatchdogUsec {
		return 0, false
	}
	return time.Duration(usec) * time.Microsecond, true
}

// socket returns the raw NOTIFY_SOCKET value, unmodified.
func (n *Notifier) socket() string { return n.getenv("NOTIFY_SOCKET") }

// addr builds the address to dial, or nil when NOTIFY_SOCKET is unset.
//
// The name is $NOTIFY_SOCKET byte for byte. A value beginning with '@' denotes
// a Linux abstract-namespace address, and net.UnixAddr already performs the
// leading '@' -> NUL translation when it builds the sockaddr_un — translating
// it here as well would produce a doubly-escaped address that binds to nothing.
//
// This is split out of send purely so a test can assert on the name: the
// kernel-facing spellings are indistinguishable at the far end, because
// syscall.SockaddrUnix treats a leading '@' and a leading NUL as the same
// abstract address, so a delivery-based test cannot tell a verbatim name from a
// hand-translated one. See TestAddrPassesSocketNameVerbatim.
func (n *Notifier) addr() *net.UnixAddr {
	sock := n.socket()
	if sock == "" {
		return nil
	}
	return &net.UnixAddr{Name: sock, Net: "unixgram"}
}

// send writes one notification datagram, under writeTimeout.
//
// A write that runs out of time returns an error wrapping ErrNotifyTimeout and
// os.ErrDeadlineExceeded; see ErrNotifyTimeout for why that is a miss to log
// rather than a failure to propagate.
func (n *Notifier) send(msg string) error {
	addr := n.addr()
	if addr == nil {
		// Not running under a service manager. Silently succeed so callers can
		// notify unconditionally.
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return fmt.Errorf("sdnotify: dial %q: %w", addr.Name, err)
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("sdnotify: set write deadline: %w", err)
	}
	if _, err := conn.Write([]byte(msg)); err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return fmt.Errorf("sdnotify: send %q: %w: %w", msg, ErrNotifyTimeout, err)
		}
		return fmt.Errorf("sdnotify: send %q: %w", msg, err)
	}
	return nil
}

// sanitizeStatus makes an arbitrary string safe to embed after "STATUS=".
//
// The notify payload is newline-separated KEY=VALUE assignments, so a newline
// inside the status text would be parsed by systemd as the start of another
// assignment — an error string containing a newline could otherwise inject a
// bogus READY=1. Fold vertical whitespace into spaces, then truncate.
func sanitizeStatus(s string) string {
	if strings.ContainsAny(s, "\r\n") {
		s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
	}
	return truncate(s, maxStatusLen)
}

// truncate clips s to at most max bytes without splitting a UTF-8 sequence —
// status text carries units like "µmol/s/m²", and half a rune on the wire
// shows up as a replacement character in `systemctl status`.
//
// The walk-back is bounded at utf8.UTFMax-1, the longest a valid encoded rune
// can trail its own start byte. Beyond that bound the bytes are not a split
// rune, they are garbage — plausibly an SDI-12 adapter echoing line noise into
// an error string — and walking further can only find a rune start that is not
// the one we are inside. An unbounded walk on such input runs all the way to 0
// and returns "", which is not a degraded status: "STATUS=" with an empty value
// is systemd's command to *clear* the status line, so the fault that produced
// the garbage would also blank the one place an operator looks for it. When no
// rune start is in range, cut at max and let the garbage through truncated.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	floor := max - (utf8.UTFMax - 1)
	if floor < 0 {
		floor = 0
	}
	for cut := max; cut >= floor; cut-- {
		if utf8.RuneStart(s[cut]) {
			return s[:cut]
		}
	}
	return s[:max]
}
