package timezone

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestManager returns a manager over a fresh state directory, defaulting to
// a zone that is deliberately not UTC so "reverted to the default" cannot be
// confused with "fell back to UTC because something failed".
func newTestManager(t *testing.T) (*Manager, *Store) {
	t.Helper()
	def := time.FixedZone("TESTDEF", 5*3600)
	s := NewStore(t.TempDir())
	return NewManager(s, def, nil), s
}

// statOrNil returns the file's FileInfo, or nil when it does not exist, for
// os.SameFile comparisons that detect whether a write happened.
func statOrNil(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	return fi
}

func TestManagerDefaultsBeforeStart(t *testing.T) {
	m, _ := newTestManager(t)
	if got := m.Location().String(); got != "TESTDEF" {
		t.Errorf("Location before Start = %q, want the configured default", got)
	}
	if got := m.State(); got != "" {
		t.Errorf("State before Start = %q, want an empty string", got)
	}
}

func TestNewManagerNilDefault(t *testing.T) {
	m := NewManager(NewStore(t.TempDir()), nil, nil)
	if got := m.Location(); got != time.UTC {
		t.Errorf("Location with a nil default = %v, want UTC", got)
	}
}

func TestManagerStartNoOverride(t *testing.T) {
	m, _ := newTestManager(t)
	m.Start()

	if got := m.Location().String(); got != "TESTDEF" {
		t.Errorf("Location = %q, want the configured default", got)
	}
	if got := m.State(); got != "" {
		t.Errorf("State = %q, want an empty string", got)
	}
}

func TestManagerStartRestoresOverride(t *testing.T) {
	m, s := newTestManager(t)
	if err := s.Save(londonTZ); err != nil {
		t.Fatalf("setup: %v", err)
	}

	m.Start()

	if got := m.State(); got != londonTZ {
		t.Errorf("State = %q, want %q", got, londonTZ)
	}
	// The restored zone must actually be in effect, not merely echoed.
	if name, off := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(m.Location()).Zone(); name != "BST" || off != 3600 {
		t.Errorf("restored zone gives %s/%d in July, want BST/3600", name, off)
	}
}

// TestManagerStartDiscardsInvalidOverride covers timezone_text.cpp:66-70. The
// file is deleted so the daemon does not log the same complaint on every boot,
// and the configured default takes over.
func TestManagerStartDiscardsInvalidOverride(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"unparsable", "not a timezone"},
		{"DST rules dropped", "GMT0,M3.5.0/1,M10.5.0"},
		{"trailing newline from a hand edit", londonTZ + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, s := newTestManager(t)
			if err := s.Save(tt.value); err != nil {
				t.Fatalf("setup: %v", err)
			}

			m.Start()

			if got := m.Location().String(); got != "TESTDEF" {
				t.Errorf("Location = %q, want the configured default", got)
			}
			if got := m.State(); got != "" {
				t.Errorf("State = %q, want an empty string", got)
			}
			if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
				t.Errorf("invalid override was not deleted (stat error = %v)", err)
			}
		})
	}
}

// TestManagerStartSurvivesUnreadableState covers the state directory being
// unusable. The daemon has to come up regardless: refusing to boot over a
// timezone override would take the sensor offline for a cosmetic setting.
func TestManagerStartSurvivesUnreadableState(t *testing.T) {
	dir := t.TempDir()
	// A directory where the state file belongs makes ReadFile fail with
	// something other than ErrNotExist.
	if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	m := NewManager(NewStore(dir), time.FixedZone("TESTDEF", 5*3600), nil)

	m.Start()

	if got := m.Location().String(); got != "TESTDEF" {
		t.Errorf("Location = %q, want the configured default", got)
	}
	if got := m.State(); got != "" {
		t.Errorf("State = %q, want an empty string", got)
	}
}

func TestManagerApplyValid(t *testing.T) {
	m, s := newTestManager(t)
	m.Start()

	if echo := m.Apply(londonTZ); echo != londonTZ {
		t.Errorf("Apply echoed %q, want the received bytes %q", echo, londonTZ)
	}
	if got := m.State(); got != londonTZ {
		t.Errorf("State = %q, want %q", got, londonTZ)
	}
	if name, off := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(m.Location()).Zone(); name != "BST" || off != 3600 {
		t.Errorf("zone gives %s/%d in July, want BST/3600", name, off)
	}
	stored, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored != londonTZ {
		t.Errorf("persisted %q, want %q", stored, londonTZ)
	}
}

// TestManagerApplyNoOpDoesNotWrite covers the short-circuit at
// timezone_text.cpp:80-85. The reconciler re-pushes the desired value on every
// reconnect; without this, each one would rewrite the SD card.
//
// A write is detected by inode identity rather than mtime, because the atomic
// write renames a freshly created file into place every time. That makes the
// check exact and free of any sleep waiting for a timestamp to tick.
func TestManagerApplyNoOpDoesNotWrite(t *testing.T) {
	m, s := newTestManager(t)
	m.Start()

	m.Apply(londonTZ)
	before := statOrNil(t, s.Path())
	if before == nil {
		t.Fatal("setup: the first Apply did not persist anything")
	}

	for i := 0; i < 3; i++ {
		if echo := m.Apply(londonTZ); echo != londonTZ {
			t.Fatalf("re-push %d echoed %q, want %q", i, echo, londonTZ)
		}
	}

	after := statOrNil(t, s.Path())
	if after == nil {
		t.Fatal("the state file disappeared")
	}
	if !os.SameFile(before, after) {
		t.Error("a repeated Apply of the value already in force rewrote the " +
			"state file; the no-op short-circuit is not working")
	}
}

// TestManagerApplyNoOpWhenUnset is the same short-circuit on the other side:
// an empty push while already unset must not touch the disk either. Without
// the short-circuit this would call Clear, which is harmless here but would
// mask a missing guard.
//
// A stat cannot see that on its own, because Clear on a file that is not there
// succeeds without creating anything — so the log is the only evidence. A
// manager that started life with persisted set to false would take the retry
// path here and announce it; asserting the log stays quiet is what pins the
// initial value to true, i.e. "nothing applied, nothing owed to the disk".
func TestManagerApplyNoOpWhenUnset(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	var buf bytes.Buffer
	m := NewManager(s, time.FixedZone("TESTDEF", 5*3600), logCapture(&buf))
	m.Start()
	buf.Reset()

	if echo := m.Apply(""); echo != "" {
		t.Errorf("Apply(\"\") echoed %q, want an empty string", echo)
	}
	if fi := statOrNil(t, s.Path()); fi != nil {
		t.Error("an empty push while already unset created a state file")
	}
	if got := buf.String(); strings.Contains(got, "retry") {
		t.Errorf("an empty push while already unset went down the retry path, so the "+
			"manager began life believing it owed the disk a write; got: %s", got)
	}
}

// TestManagerApplyRejectedReEchoesCurrentState covers timezone_text.cpp:105,
// 111 and 120. Echoing the rejected value back would tell the reconciler its
// write landed and stop it from ever retrying.
func TestManagerApplyRejectedReEchoesCurrentState(t *testing.T) {
	bad := []string{
		"not a timezone",
		"GMT0,M3.5.0/1,M10.5.0", // DST rules silently dropped
		londonTZ + "\n",         // control byte
		string(make([]byte, MaxLen+1)),
	}

	t.Run("with an override in force", func(t *testing.T) {
		m, s := newTestManager(t)
		m.Start()
		m.Apply(londonTZ)
		before := statOrNil(t, s.Path())

		for _, v := range bad {
			if echo := m.Apply(v); echo != londonTZ {
				t.Errorf("Apply(%q) echoed %q, want the value still in force %q", v, echo, londonTZ)
			}
		}

		if got := m.State(); got != londonTZ {
			t.Errorf("State = %q, want %q", got, londonTZ)
		}
		if name, _ := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(m.Location()).Zone(); name != "BST" {
			t.Errorf("zone changed to %s despite every write being rejected", name)
		}
		if after := statOrNil(t, s.Path()); after == nil || !os.SameFile(before, after) {
			t.Error("a rejected value rewrote the state file")
		}
	})

	t.Run("with no override in force", func(t *testing.T) {
		m, s := newTestManager(t)
		m.Start()

		for _, v := range bad {
			if echo := m.Apply(v); echo != "" {
				t.Errorf("Apply(%q) echoed %q, want an empty string", v, echo)
			}
		}

		if got := m.Location().String(); got != "TESTDEF" {
			t.Errorf("Location = %q, want the configured default", got)
		}
		if fi := statOrNil(t, s.Path()); fi != nil {
			t.Error("a rejected value created a state file")
		}
	})
}

// TestManagerApplyEmptyClears covers timezone_text.cpp:73 and 87-96: an empty
// command drops the override and reverts to the configured default in the
// running process, not just at the next restart.
func TestManagerApplyEmptyClears(t *testing.T) {
	m, s := newTestManager(t)
	m.Start()
	m.Apply(londonTZ)

	if echo := m.Apply(""); echo != "" {
		t.Errorf("Apply(\"\") echoed %q, want an empty string", echo)
	}
	if got := m.State(); got != "" {
		t.Errorf("State = %q, want an empty string", got)
	}
	if got := m.Location().String(); got != "TESTDEF" {
		t.Errorf("Location = %q, want the configured default restored immediately", got)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("state file survived the clear (stat error = %v)", err)
	}
}

// TestManagerRestartAfterClear is the property the clear path exists for: a
// cleared override must stay cleared across a restart.
func TestManagerRestartAfterClear(t *testing.T) {
	dir := t.TempDir()
	def := time.FixedZone("TESTDEF", 5*3600)

	first := NewManager(NewStore(dir), def, nil)
	first.Start()
	first.Apply(londonTZ)
	first.Apply("")

	second := NewManager(NewStore(dir), def, nil)
	second.Start()
	if got := second.State(); got != "" {
		t.Errorf("State after restart = %q, want an empty string", got)
	}
	if got := second.Location().String(); got != "TESTDEF" {
		t.Errorf("Location after restart = %q, want the configured default", got)
	}
}

// TestManagerRestartRestoresOverride is the same property for the applied path:
// what Apply persisted is what the next process comes up with, which is what
// makes the pre-connect Start meaningful.
func TestManagerRestartRestoresOverride(t *testing.T) {
	dir := t.TempDir()
	def := time.FixedZone("TESTDEF", 5*3600)

	first := NewManager(NewStore(dir), def, nil)
	first.Start()
	first.Apply(londonTZ)

	second := NewManager(NewStore(dir), def, nil)
	second.Start()
	if got := second.State(); got != londonTZ {
		t.Errorf("State after restart = %q, want %q", got, londonTZ)
	}
	if name, off := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(second.Location()).Zone(); name != "BST" || off != 3600 {
		t.Errorf("zone after restart gives %s/%d, want BST/3600", name, off)
	}
}

func TestManagerApplySwitchesBetweenOverrides(t *testing.T) {
	m, _ := newTestManager(t)
	m.Start()

	m.Apply(londonTZ)
	const sydney = "AEST-10AEDT,M10.1.0,M4.1.0/3"
	if echo := m.Apply(sydney); echo != sydney {
		t.Errorf("Apply echoed %q, want %q", echo, sydney)
	}
	// July is standard time in Sydney; if the old London zone were still in
	// force this would report BST/3600.
	if name, off := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(m.Location()).Zone(); name != "AEST" || off != 36000 {
		t.Errorf("zone gives %s/%d in July, want AEST/36000", name, off)
	}
}

// TestManagerAppliesWhenPersistenceFails mirrors the ESPHome ordering at
// timezone_text.cpp:123-127, where set_timezone runs before persist_ and
// persist_ only warns. A read-only state directory should not stop the daemon
// honouring the zone it was just told to use.
func TestManagerAppliesWhenPersistenceFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions are not enforced")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	m := NewManager(NewStore(dir), time.FixedZone("TESTDEF", 5*3600), nil)
	m.Start()

	if echo := m.Apply(londonTZ); echo != londonTZ {
		t.Errorf("Apply echoed %q, want %q", echo, londonTZ)
	}
	if got := m.State(); got != londonTZ {
		t.Errorf("State = %q, want %q", got, londonTZ)
	}
	if name, off := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC).In(m.Location()).Zone(); name != "BST" || off != 3600 {
		t.Errorf("zone gives %s/%d, want BST/3600 despite the failed write", name, off)
	}
}

// TestManagerRetriesFailedPersist is the property the persisted flag exists
// for. A write that failed while the state directory was read-only must be
// retried once the directory is writable again, on the reconciler's next push
// of the same value.
//
// Without the flag this cannot happen: Apply short-circuits on the value
// matching m.state, so every re-push after the failure returns without touching
// the disk and the override stays missing for the life of the process — a
// timezone that silently reverts at the next reboot, which is the failure mode
// this package exists to avoid.
func TestManagerRetriesFailedPersist(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions are not enforced")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	s := NewStore(dir)
	m := NewManager(s, time.FixedZone("TESTDEF", 5*3600), nil)
	m.Start()

	if echo := m.Apply(londonTZ); echo != londonTZ {
		t.Fatalf("setup: Apply echoed %q, want %q", echo, londonTZ)
	}
	if fi := statOrNil(t, s.Path()); fi != nil {
		t.Fatal("setup: the write was expected to fail against a read-only directory")
	}

	// The card comes back, or the operator fixes the permissions.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if echo := m.Apply(londonTZ); echo != londonTZ {
		t.Errorf("re-push echoed %q, want %q", echo, londonTZ)
	}

	stored, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored != londonTZ {
		t.Errorf("after the directory became writable the override was not retried: "+
			"stored %q, want %q", stored, londonTZ)
	}
	// And a restart must now come up with it, which is the whole point.
	second := NewManager(NewStore(dir), time.FixedZone("TESTDEF", 5*3600), nil)
	second.Start()
	if got := second.State(); got != londonTZ {
		t.Errorf("State after restart = %q, want %q", got, londonTZ)
	}
}

// TestManagerStopsRetryingOncePersisted keeps the retry from undoing the SD-card
// protection the no-op short-circuit exists for: once a retry succeeds, further
// re-pushes of the same value must go back to touching nothing.
func TestManagerStopsRetryingOncePersisted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions are not enforced")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	s := NewStore(dir)
	m := NewManager(s, time.FixedZone("TESTDEF", 5*3600), nil)
	m.Start()
	m.Apply(londonTZ)

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	m.Apply(londonTZ) // the retry that succeeds

	before := statOrNil(t, s.Path())
	if before == nil {
		t.Fatal("setup: the retry did not persist anything")
	}
	for i := 0; i < 3; i++ {
		m.Apply(londonTZ)
	}
	after := statOrNil(t, s.Path())
	if after == nil {
		t.Fatal("the state file disappeared")
	}
	if !os.SameFile(before, after) {
		t.Error("re-pushes kept rewriting the state file after the retry succeeded; " +
			"the no-op short-circuit is no longer protecting the card")
	}
}

// TestManagerRetriesFailedClearFromStart covers the other direction, and the
// one path in Start that records a failed write: an invalid stored override
// that could not be deleted because the directory was read-only. The bad value
// is still on disk while the daemon runs with the default, so a later empty
// push has to retry the removal rather than short-circuit on the state already
// being empty.
func TestManagerRetriesFailedClearFromStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions are not enforced")
	}
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Save("not a timezone"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Readable so Load and Parse still run, but not writable, so Clear fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	m := NewManager(s, time.FixedZone("TESTDEF", 5*3600), nil)
	m.Start()

	if got := m.Location().String(); got != "TESTDEF" {
		t.Errorf("Location = %q, want the configured default", got)
	}
	if fi := statOrNil(t, s.Path()); fi == nil {
		t.Fatal("setup: the invalid override was expected to survive the failed clear")
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if echo := m.Apply(""); echo != "" {
		t.Errorf("Apply(\"\") echoed %q, want an empty string", echo)
	}
	if fi := statOrNil(t, s.Path()); fi != nil {
		t.Error("the invalid override was never removed: the failed clear at " +
			"startup was not retried on a later empty push")
	}
}

// logCapture returns a logger writing into buf, for the cases where a log line
// is the only externally visible difference.
func logCapture(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestManagerLogsDistinguishUnreadableFromUnset is the one place the difference
// is observable at all: both cases leave the configured default in force and an
// empty state, so without the log an operator cannot tell a broken state
// directory from a fresh install, and a silently dropped override looks exactly
// like never having had one.
func TestManagerLogsDistinguishUnreadableFromUnset(t *testing.T) {
	t.Run("unreadable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, FileName), 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
		var buf bytes.Buffer
		NewManager(NewStore(dir), time.UTC, logCapture(&buf)).Start()

		got := buf.String()
		if !strings.Contains(got, "unreadable") {
			t.Errorf("log did not report the state file as unreadable; got: %s", got)
		}
		if !strings.Contains(got, "level=WARN") {
			t.Errorf("an unreadable state directory was not logged at WARN; got: %s", got)
		}
	})

	t.Run("unset", func(t *testing.T) {
		var buf bytes.Buffer
		NewManager(NewStore(t.TempDir()), time.UTC, logCapture(&buf)).Start()

		got := buf.String()
		if strings.Contains(got, "level=WARN") {
			t.Errorf("a fresh state directory produced a warning; got: %s", got)
		}
	})
}

// TestManagerLogsRejection covers the other silent-by-design path: a rejected
// write changes nothing observable except the echoed value, so the warning is
// what tells an operator why the reconciler keeps retrying.
func TestManagerLogsRejection(t *testing.T) {
	var buf bytes.Buffer
	m := NewManager(NewStore(t.TempDir()), time.UTC, logCapture(&buf))
	m.Start()
	buf.Reset()

	m.Apply("GMT0,M3.5.0/1,M10.5.0")

	got := buf.String()
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("a rejected override was not logged at WARN; got: %s", got)
	}
	if !strings.Contains(got, "rejecting") {
		t.Errorf("log does not say the value was rejected; got: %s", got)
	}
	// The reason has to reach the log too, or the operator sees a rejection
	// with no way to tell which guard fired.
	if !strings.Contains(got, "DST") {
		t.Errorf("log does not carry the reason for the rejection; got: %s", got)
	}
}

// TestManagerLogsUnpersistedApply covers the case where the zone is honoured
// but will not survive a restart. Silence here would let a read-only state
// directory go unnoticed until a reboot quietly reverted the timezone.
func TestManagerLogsUnpersistedApply(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; directory permissions are not enforced")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("setup: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	var buf bytes.Buffer
	m := NewManager(NewStore(dir), time.UTC, logCapture(&buf))
	m.Start()
	buf.Reset()

	m.Apply(londonTZ)

	got := buf.String()
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("a failed persist was not logged at WARN; got: %s", got)
	}
	if !strings.Contains(got, "not persisted") {
		t.Errorf("log does not say the value was not persisted; got: %s", got)
	}
}

// TestManagerConcurrentReads exercises the atomic.Pointer under -race: a
// consumer reading the live zone while the reconciler swaps it must never see a
// torn or nil value.
func TestManagerConcurrentReads(t *testing.T) {
	m, _ := newTestManager(t)
	m.Start()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if loc := m.Location(); loc == nil {
					t.Error("Location returned nil")
					return
				}
				_ = m.State()
			}
		}()
	}

	// Each Apply fsyncs twice, so keep the loop short: the point is to give
	// the race detector overlapping reads and writes, not to do volume.
	for i := 0; i < 10; i++ {
		m.Apply(londonTZ)
		m.Apply("AEST-10AEDT,M10.1.0,M4.1.0/3")
		m.Apply("")
	}
	close(stop)
	wg.Wait()
}
