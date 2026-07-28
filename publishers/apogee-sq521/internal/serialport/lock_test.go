//go:build linux

package serialport

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// nodeID returns an ID unique to this test and this process, so a parallel
// package run — or a real daemon on the developer's machine — cannot collide
// with the abstract name under test.
func nodeID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%d-%s", os.Getpid(), strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()))
}

func TestInstanceSocketName(t *testing.T) {
	if got, want := InstanceSocketName("quantum-sensor"), "@apogee-sq521-quantum-sensor"; got != want {
		t.Errorf("InstanceSocketName = %q, want %q", got, want)
	}
	// The leading '@' is what puts the name in the abstract namespace, which is
	// the whole reason there is no stale file to reap after a SIGKILL.
	if !strings.HasPrefix(InstanceSocketName("x"), "@") {
		t.Error("instance socket name is not abstract")
	}
}

// TestAcquireInstanceExcludesASecondHolder is the guard's reason for existing:
// two publishers interleaving SDI-12 transactions on one bus is the desync this
// codebase spends most of its effort preventing.
func TestAcquireInstanceExcludesASecondHolder(t *testing.T) {
	id := nodeID(t)

	first, err := AcquireInstance(id)
	if err != nil {
		t.Fatalf("first AcquireInstance: %v", err)
	}

	second, err := AcquireInstance(id)
	if err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("a second AcquireInstance succeeded")
	}
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("second AcquireInstance: %v; want ErrInUse", err)
	}
	// The message has to name the socket: an operator debugging a unit that
	// will not start needs something to look for in `ss -x`.
	if !strings.Contains(err.Error(), InstanceSocketName(id)) {
		t.Errorf("error %q does not name the socket %q", err, InstanceSocketName(id))
	}

	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Released on close: a restart must not have to wait out a lingering name,
	// which is exactly what a filesystem socket or a PID file would impose.
	third, err := AcquireInstance(id)
	if err != nil {
		t.Fatalf("AcquireInstance after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestAcquireInstanceIsPerNode: a staging unit and the live one on the same host
// have different NODE_IDs and must not lock each other out.
func TestAcquireInstanceIsPerNode(t *testing.T) {
	a, err := AcquireInstance(nodeID(t) + "-a")
	if err != nil {
		t.Fatalf("AcquireInstance a: %v", err)
	}
	defer a.Close()

	b, err := AcquireInstance(nodeID(t) + "-b")
	if err != nil {
		t.Fatalf("AcquireInstance b: %v; distinct node IDs must not collide", err)
	}
	defer b.Close()
}

// TestAcquireInstanceLeavesNoFilesystemEntry: an abstract socket has no path, so
// nothing accumulates in the working directory or in /run across restarts.
func TestAcquireInstanceLeavesNoFilesystemEntry(t *testing.T) {
	id := nodeID(t)
	h, err := AcquireInstance(id)
	if err != nil {
		t.Fatalf("AcquireInstance: %v", err)
	}
	defer h.Close()

	name := InstanceSocketName(id)
	for _, p := range []string{name, strings.TrimPrefix(name, "@")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%q exists on disk; the socket is not abstract", p)
		}
	}
}

// TestAcquireInstanceRejectsAnOverlongName turns a bare EINVAL from bind(2) into
// something an operator can act on.
func TestAcquireInstanceRejectsAnOverlongName(t *testing.T) {
	long := strings.Repeat("n", maxInstanceSocketName)
	h, err := AcquireInstance(long)
	if err == nil {
		_ = h.Close()
		t.Fatal("AcquireInstance accepted a name longer than sun_path")
	}
	if errors.Is(err, ErrInUse) {
		t.Errorf("an over-long name reported ErrInUse: %v", err)
	}
	if !strings.Contains(err.Error(), "NODE_ID") {
		t.Errorf("error %q does not tell the operator which variable to shorten", err)
	}
}

// TestAcquireInstanceAcceptsTheLongestLegalName pins the boundary, so the
// length check cannot drift off by one and start rejecting valid IDs.
func TestAcquireInstanceAcceptsTheLongestLegalName(t *testing.T) {
	pad := maxInstanceSocketName - len(instanceSocketPrefix) - len(nodeID(t))
	if pad < 0 {
		t.Skipf("test name already exceeds the socket budget")
	}
	id := nodeID(t) + strings.Repeat("x", pad)
	if len(InstanceSocketName(id)) != maxInstanceSocketName {
		t.Fatalf("built a name of %d bytes, want %d", len(InstanceSocketName(id)), maxInstanceSocketName)
	}
	h, err := AcquireInstance(id)
	if err != nil {
		t.Fatalf("AcquireInstance with a full-length name: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
