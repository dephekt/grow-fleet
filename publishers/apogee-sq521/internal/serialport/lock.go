//go:build linux

package serialport

import (
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/sys/unix"
)

// instanceSocketPrefix namespaces the abstract socket. The node ID is appended
// so two publishers on one host (a staging unit alongside the live one, say)
// do not lock each other out, while two copies of the *same* unit do.
const instanceSocketPrefix = "@apogee-sq521-"

// maxInstanceSocketName is sizeof(struct sockaddr_un.sun_path). An abstract name
// may use all 108 bytes because its leading NUL is the '@' the Go net package
// translates, rather than an extra terminator; a filesystem path could not.
// syscall rejects anything longer with a bare EINVAL, which tells an operator
// with a long NODE_ID nothing at all.
const maxInstanceSocketName = 108

// AcquireInstance takes the primary single-instance lock for nodeID and returns
// the handle that holds it. Closing the handle releases it.
//
// The lock is an abstract-namespace unix socket, and it is the *primary* guard
// rather than one of the two device-level locks in [Open] for two reasons:
//
//   - It is device-independent, so it covers the boot window. USB enumeration
//     routinely lags a Pi reboot by seconds; during that window there is no
//     device to flock, and two units racing to be first would both sail past a
//     device-level guard and then fight over the adapter as it appears.
//   - The kernel reclaims it when the process dies, including under SIGKILL and
//     including an OOM kill. There is no stale state to reap, which a PID file
//     would have needed — and reaping a PID file is how single-instance guards
//     usually acquire their own race.
//
// A second caller gets an error wrapping [ErrInUse]. The daemon should exit
// non-zero rather than continue: two publishers on one sensor produce
// interleaved SDI-12 transactions, which is the desync this codebase spends
// most of its comments preventing.
func AcquireInstance(nodeID string) (io.Closer, error) {
	name := InstanceSocketName(nodeID)
	if len(name) > maxInstanceSocketName {
		return nil, fmt.Errorf("serialport: instance socket name %q is %d bytes, over the %d-byte sun_path limit; shorten NODE_ID",
			name, len(name), maxInstanceSocketName)
	}

	// net.Listen recognises the leading '@' as the abstract namespace and, on
	// Close, skips the unlink it would perform for a filesystem path — there is
	// no filesystem entry to remove, which is the whole point.
	l, err := net.Listen("unix", name)
	if err != nil {
		if errors.Is(err, unix.EADDRINUSE) {
			return nil, fmt.Errorf("serialport: %s: %w", name, ErrInUse)
		}
		return nil, fmt.Errorf("serialport: bind %s: %w", name, err)
	}
	// The listener is never accepted on. It exists to hold the name; a caller
	// that connected to it would just sit in the backlog, which is fine, and
	// nothing in this system connects.
	return l, nil
}

// InstanceSocketName returns the abstract socket name [AcquireInstance] binds
// for nodeID. Exported so a diagnostic can print what to look for in
// `ss -x | grep apogee`.
func InstanceSocketName(nodeID string) string { return instanceSocketPrefix + nodeID }
