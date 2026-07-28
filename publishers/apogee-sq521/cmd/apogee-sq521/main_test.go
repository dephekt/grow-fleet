package main

import (
	"bytes"
	"strings"
	"testing"
)

// The exit codes are the only thing about this binary a script can see, and the
// two the daemon reports its own faults with are already covered by the unit
// file. These are the ones a human or a smoke test reaches first.

// --version and --help are successful requests for information, not malformed
// invocations — so they exit 0 and answer on stdout.
//
// flag.ErrHelp used to fall through to the same EX_CONFIG as a genuine parse
// error, which breaks `apogee-sq521 --help && …` and makes the binary look
// broken to anything that checks a status. The usage then stayed on stderr even
// once the code was fixed, because flag writes both the help and the parse error
// to one output: `apogee-sq521 --help | less` showed an empty pager.
func TestInformationalFlagsExitZeroOnStdout(t *testing.T) {
	tests := map[string]struct {
		args      []string
		mustMatch string
	}{
		"--version": {args: []string{"--version"}, mustMatch: "dev"},
		"--help":    {args: []string{"--help"}, mustMatch: "-once"},
		"-h":        {args: []string{"-h"}, mustMatch: "-check-config"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tc.args, &stdout, &stderr); got != exitOK {
				t.Errorf("run(%v) = %d, want %d", tc.args, got, exitOK)
			}
			if got := stdout.String(); !strings.Contains(got, tc.mustMatch) {
				t.Errorf("run(%v) stdout = %q, want it to mention %q", tc.args, got, tc.mustMatch)
			}
			// Nothing on stderr: an answer that was asked for is not a
			// diagnostic, and a caller redirecting one stream must not have to
			// merge the other to read it.
			if got := stderr.String(); got != "" {
				t.Errorf("run(%v) wrote %q to stderr; the answer belongs on stdout alone", tc.args, got)
			}
		})
	}
}

// A flag that does not exist is still a configuration fault: the unit file is
// wrong, and systemd Restart=on-failure would loop on it forever without ever
// succeeding, which is exactly what EX_CONFIG is for.
//
// And it is a diagnostic, so it stays on stderr — the other half of the split
// above. A pipeline reading stdout must not receive a usage message as data.
func TestAnUnknownFlagIsAConfigurationFaultOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := run([]string{"--not-a-flag"}, &stdout, &stderr); got != exitConfig {
		t.Errorf("run with an unknown flag = %d, want %d (EX_CONFIG)", got, exitConfig)
	}
	for _, want := range []string{"not-a-flag", "-check-config"} {
		if got := stderr.String(); !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want it to mention %q", got, want)
		}
	}
	if got := stdout.String(); got != "" {
		t.Errorf("a malformed invocation wrote %q to stdout, want nothing", got)
	}
}

// The README's smoke step, run on a Pi where the adapter has not been plugged
// in yet — which is when it is run.
//
// "The device is not there" and "this binary is broken" both used to exit 1, so
// the step could not establish the thing it exists to establish. The instance
// lock is an abstract unix socket, so taking it here has no filesystem side
// effects and releases with the process's listener.
func TestOnceWithNoDeviceExitsUnavailable(t *testing.T) {
	for k, v := range map[string]string{
		"NODE_ID":       "apogee-sq521-main-test",
		"MQTT_HOST":     "127.0.0.1",
		"MQTT_USERNAME": "test",
		"MQTT_PASSWORD": "test",
		"TOPIC_PREFIX":  "grow/test",
		"SDI12_PORT":    "/nonexistent/apogee-sq521-main-test",
	} {
		t.Setenv(k, v)
	}

	var stdout, stderr bytes.Buffer
	got := run([]string{"--once"}, &stdout, &stderr)
	if got != exitUnavailable {
		t.Errorf("--once with no adapter = %d, want %d (EX_UNAVAILABLE); stderr:\n%s",
			got, exitUnavailable, stderr.String())
	}
	if got == exitFailure {
		t.Error("the hardware fault is indistinguishable from an internal one")
	}
}

// The exit codes are distinct values, which is the whole point of having five
// of them. A duplicate would make two different faults indistinguishable in
// `systemctl status` and in the README's smoke step.
func TestTheExitCodesAreDistinct(t *testing.T) {
	seen := map[int]string{}
	for name, code := range map[string]int{
		"exitOK":          exitOK,
		"exitFailure":     exitFailure,
		"exitUnavailable": exitUnavailable,
		"exitTempFail":    exitTempFail,
		"exitConfig":      exitConfig,
	} {
		if other, dup := seen[code]; dup {
			t.Errorf("%s and %s are both %d", name, other, code)
		}
		seen[code] = name
	}
}
