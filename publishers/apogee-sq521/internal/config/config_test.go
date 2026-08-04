package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeEnv builds a getenv func backed by a map, so no test ever reads the real
// process environment (which would make results depend on the developer's
// shell and on test ordering).
func fakeEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

// validEnv is the smallest environment that loads cleanly. Only the password is
// set: the default username is non-empty, so an otherwise-bare environment is
// (correctly) a validation failure — see TestLoadDefaultsWithoutPassword.
func validEnv() map[string]string {
	return map[string]string{envMQTTPassword: "hunter2-correct-horse"}
}

// withEnv returns validEnv() overlaid with the given key/value pairs.
func withEnv(overrides map[string]string) map[string]string {
	env := validEnv()
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

func TestLoadDefaults(t *testing.T) {
	got, err := Load(fakeEnv(validEnv()))
	if err != nil {
		t.Fatalf("Load() with defaults: unexpected error: %v", err)
	}

	want := Config{
		NodeID:          "quantum-sensor",
		MQTTHost:        "192.168.8.3",
		MQTTPort:        1883,
		MQTTUsername:    "edge-daniel-home",
		MQTTPassword:    "hunter2-correct-horse",
		TopicPrefix:     "grow/daniel-home",
		SDI12Port:       "/dev/serial/by-id/usb-FTDI_FT231X_USB_UART_D30GQUR5-if00-port0",
		SDI12Address:    "0",
		PollInterval:    10 * time.Second,
		PPFDUnit:        "µmol/s/m²",
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    time.Second,
		OfflineAfter:    3,
		ShutdownTimeout: 3 * time.Second,
		StateDir:        "",
		SubstrateEvery:  15,
	}
	if got != want {
		t.Errorf("Load() with defaults =\n%+v\nwant\n%+v", got, want)
	}
	if got.PersistenceEnabled() {
		t.Error("PersistenceEnabled() = true with no STATE_DIRECTORY, want false")
	}
}

func TestLoadOverrides(t *testing.T) {
	env := map[string]string{
		envNodeID:          "quantum-sensor-2",
		envMQTTHost:        "mqtt.home.arpa",
		envMQTTPort:        "8883",
		envMQTTUsername:    "edge-test",
		envMQTTPassword:    "s3cret",
		envTopicPrefix:     "grow/test-site",
		envSDI12Port:       "/dev/ttyUSB1",
		envSDI12Address:    "Z",
		envPollSeconds:     "30",
		envPPFDUnit:        "umol/s/m2",
		envReadTimeout:     "5",
		envWriteTimeout:    "0.25",
		envOfflineAfter:    "10",
		envShutdownTimeout: "1.5",
		envStateDir:        "/var/lib/apogee-sq521",
	}

	got, err := Load(fakeEnv(env))
	if err != nil {
		t.Fatalf("Load() with overrides: unexpected error: %v", err)
	}

	want := Config{
		NodeID:          "quantum-sensor-2",
		MQTTHost:        "mqtt.home.arpa",
		MQTTPort:        8883,
		MQTTUsername:    "edge-test",
		MQTTPassword:    "s3cret",
		TopicPrefix:     "grow/test-site",
		SDI12Port:       "/dev/ttyUSB1",
		SDI12Address:    "Z",
		PollInterval:    30 * time.Second,
		PPFDUnit:        "umol/s/m2",
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    250 * time.Millisecond,
		OfflineAfter:    10,
		ShutdownTimeout: 1500 * time.Millisecond,
		StateDir:        "/var/lib/apogee-sq521",
		SubstrateEvery:  15,
	}
	if got != want {
		t.Errorf("Load() with overrides =\n%+v\nwant\n%+v", got, want)
	}
	if !got.PersistenceEnabled() {
		t.Error("PersistenceEnabled() = false with STATE_DIRECTORY set, want true")
	}
}

// TestLoadDefaultsWithoutPassword pins the defect this daemon exists to fix:
// a bare environment inherits the default username, and a username without a
// password must be fatal rather than producing a client that never connects.
func TestLoadDefaultsWithoutPassword(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{}))
	if err == nil {
		t.Fatal("Load() with no password: got nil error, want a validation failure")
	}
	if !strings.Contains(err.Error(), envMQTTPassword) {
		t.Errorf("Load() error = %q, want it to name %s", err, envMQTTPassword)
	}
}

// TestLoadCredentialsAreMandatory pins the headline check from both sides.
//
// Anonymous access is deliberately unsupported (see the package doc), so the
// username and the password are independently required and each reports its own
// error. The first case is the one that matters: MQTT_USERNAME=" " is a single
// stray space in an EnvironmentFile, it trims to "", and under a guard spelled
// `username != "" && password == ""` it disables the credential check entirely —
// reproducing, through a different door, the exact Python defect this daemon
// exists to eliminate.
//
// The error counts are load-bearing. Demanding two errors for the blank/blank
// case pins the guard against over-firing as well: re-coupling the password
// check to a non-empty username collapses that case back to one error, and
// dropping the username check collapses it to one the other way.
func TestLoadCredentialsAreMandatory(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantErrs int
		wantIn   []string
	}{
		{
			name:     "whitespace username and no password",
			env:      map[string]string{envMQTTUsername: " ", envMQTTPassword: ""},
			wantErrs: 2,
			wantIn:   []string{envMQTTUsername, `" "`, envMQTTPassword},
		},
		{
			name:     "whitespace username with a valid password",
			env:      map[string]string{envMQTTUsername: "   ", envMQTTPassword: "hunter2"},
			wantErrs: 1,
			wantIn:   []string{envMQTTUsername, `"   "`, "must not be empty"},
		},
		{
			name:     "explicit username with no password",
			env:      map[string]string{envMQTTUsername: "edge-test", envMQTTPassword: ""},
			wantErrs: 1,
			wantIn:   []string{envMQTTPassword, envMQTTUsername, "edge-test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(fakeEnv(tt.env))
			if err == nil {
				t.Fatalf("Load(%v) = nil error, want a credential failure (username %q, password empty = %t)",
					tt.env, cfg.MQTTUsername, cfg.MQTTPassword == "")
			}
			if n := len(joined(err)); n != tt.wantErrs {
				t.Errorf("Load() reported %d errors, want %d:\n%v", n, tt.wantErrs, err)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestLoadEmptyUsernameKeepsTheDefault documents the anonymous-access decision.
//
// An unset variable and one set to the empty string are indistinguishable
// through getenv, so MQTT_USERNAME= resurrects the default rather than selecting
// anonymous access. That is the intended behaviour: there is deliberately no
// environment in which this daemon starts without credentials.
func TestLoadEmptyUsernameKeepsTheDefault(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
		envMQTTUsername: "",
		envMQTTPassword: "hunter2-correct-horse",
	}))
	if err != nil {
		t.Fatalf("Load(MQTT_USERNAME=): unexpected error: %v", err)
	}
	if cfg.MQTTUsername != defaultMQTTUsername {
		t.Errorf("MQTTUsername = %q, want the default %q", cfg.MQTTUsername, defaultMQTTUsername)
	}
}

// TestLoadRejectsOutOfRangeSeconds covers the float-to-Duration conversion.
//
// time.Duration is an int64 of nanoseconds, and the Go spec leaves a
// float-to-integer conversion undefined when the value does not fit. On amd64
// it yields INT64_MIN, which is negative and trips the floor check by accident;
// on arm64 — the Pi this daemon is deployed to — FCVTZS saturates to INT64_MAX,
// which passes every check as a ~292-year interval and produces a daemon that
// silently never polls. The range check therefore has to happen before the
// conversion, and the error has to say so rather than complaining about a floor.
func TestLoadRejectsOutOfRangeSeconds(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		get   func(Config) time.Duration
	}{
		{
			name:  "poll seconds one second past the int64 nanosecond ceiling",
			key:   envPollSeconds,
			value: "9223372037",
			get:   func(c Config) time.Duration { return c.PollInterval },
		},
		{
			name:  "poll seconds far past the ceiling",
			key:   envPollSeconds,
			value: "9300000000",
			get:   func(c Config) time.Duration { return c.PollInterval },
		},
		{
			name:  "read timeout past float64 itself",
			key:   envReadTimeout,
			value: strings.Repeat("9", 400),
			get:   func(c Config) time.Duration { return c.ReadTimeout },
		},
		{
			name:  "shutdown timeout past the negative bound",
			key:   envShutdownTimeout,
			value: "-9300000000",
			get:   func(c Config) time.Duration { return c.ShutdownTimeout },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(fakeEnv(withEnv(map[string]string{tt.key: tt.value})))
			if err == nil {
				t.Fatalf("Load(%s=%s) = nil error, want an out-of-range failure", tt.key, tt.value)
			}
			if n := len(joined(err)); n != 1 {
				t.Errorf("Load() reported %d errors, want exactly 1:\n%v", n, err)
			}
			for _, want := range []string{tt.key, "out of range"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err, want)
				}
			}
			// The rejected value must not survive as a duration at all: a
			// saturated or wrapped one is worse than none, because a caller
			// that ignores the error gets a plausible-looking interval.
			if got := tt.get(cfg); got != 0 {
				t.Errorf("%s = %v, want 0 for a rejected value", tt.key, got)
			}
		})
	}

	// The largest value that does fit must still load, so the range check
	// cannot be satisfied by rejecting everything large.
	cfg, err := Load(fakeEnv(withEnv(map[string]string{envPollSeconds: "9223372036"})))
	if err != nil {
		t.Fatalf("Load(%s=9223372036): unexpected error: %v", envPollSeconds, err)
	}
	if want := 9223372036 * time.Second; cfg.PollInterval != want {
		t.Errorf("PollInterval = %v, want %v", cfg.PollInterval, want)
	}
}

// TestNumericParsersAgree pins the two numeric parsers to one definition of a
// number. strconv.ParseFloat takes Go float-literal syntax — underscore
// separators, hex-float mantissas, exponents — and the words "NaN" and "Inf",
// none of which strconv.Atoi accepts. That left POLL_SECONDS=1_0 meaning 10s
// while MQTT_PORT=1_8_8_3 was rejected, and the doc comment claiming a
// strictness the code did not enforce.
func TestNumericParsersAgree(t *testing.T) {
	rejected := []string{"1_0", "1__0", "0x1p4", "0x10", "1e3", "1E3", "0b11", "١٠", "1 0", "١"}
	for _, tok := range rejected {
		t.Run("rejected/"+tok, func(t *testing.T) {
			if cfg, err := Load(fakeEnv(withEnv(map[string]string{envMQTTPort: tok}))); err == nil {
				t.Errorf("Load(%s=%s) = nil error (port %d), want a rejection", envMQTTPort, tok, cfg.MQTTPort)
			}
			if cfg, err := Load(fakeEnv(withEnv(map[string]string{envPollSeconds: tok}))); err == nil {
				t.Errorf("Load(%s=%s) = nil error (interval %v), want a rejection", envPollSeconds, tok, cfg.PollInterval)
			}
		})
	}

	accepted := []struct {
		tok      string
		wantPort int
		wantPoll time.Duration
	}{
		{"10", 10, 10 * time.Second},
		{"+10", 10, 10 * time.Second},
		{"0010", 10, 10 * time.Second},
	}
	for _, tt := range accepted {
		t.Run("accepted/"+tt.tok, func(t *testing.T) {
			cfg, err := Load(fakeEnv(withEnv(map[string]string{envMQTTPort: tt.tok})))
			if err != nil {
				t.Errorf("Load(%s=%s): unexpected error: %v", envMQTTPort, tt.tok, err)
			} else if cfg.MQTTPort != tt.wantPort {
				t.Errorf("MQTTPort = %d, want %d", cfg.MQTTPort, tt.wantPort)
			}
			cfg, err = Load(fakeEnv(withEnv(map[string]string{envPollSeconds: tt.tok})))
			if err != nil {
				t.Errorf("Load(%s=%s): unexpected error: %v", envPollSeconds, tt.tok, err)
			} else if cfg.PollInterval != tt.wantPoll {
				t.Errorf("PollInterval = %v, want %v", cfg.PollInterval, tt.wantPoll)
			}
		})
	}
}

// TestLoadValidationFailures walks one bad variable at a time. wantIn lists
// substrings the (single) resulting error must contain — always the env var
// name and the offending value, per the package contract.
func TestLoadValidationFailures(t *testing.T) {
	tests := []struct {
		name   string
		env    map[string]string
		wantIn []string
	}{
		{
			name:   "password empty with explicit username",
			env:    map[string]string{envMQTTUsername: "edge-test", envMQTTPassword: ""},
			wantIn: []string{envMQTTUsername, "edge-test", envMQTTPassword},
		},
		{
			name:   "host blank",
			env:    withEnv(map[string]string{envMQTTHost: "   "}),
			wantIn: []string{envMQTTHost, "must not be empty"},
		},
		{
			name:   "port zero",
			env:    withEnv(map[string]string{envMQTTPort: "0"}),
			wantIn: []string{envMQTTPort, `"0"`, "1..65535"},
		},
		{
			name:   "port above range",
			env:    withEnv(map[string]string{envMQTTPort: "65536"}),
			wantIn: []string{envMQTTPort, `"65536"`},
		},
		{
			name:   "port negative",
			env:    withEnv(map[string]string{envMQTTPort: "-1"}),
			wantIn: []string{envMQTTPort, `"-1"`},
		},
		{
			name:   "port not a number",
			env:    withEnv(map[string]string{envMQTTPort: "mqtt"}),
			wantIn: []string{envMQTTPort, `"mqtt"`, "whole number"},
		},
		{
			name:   "node id blank",
			env:    withEnv(map[string]string{envNodeID: " "}),
			wantIn: []string{envNodeID, "must not be empty"},
		},
		{
			name:   "node id with slash",
			env:    withEnv(map[string]string{envNodeID: "grow/sensor"}),
			wantIn: []string{envNodeID, "grow/sensor"},
		},
		{
			name:   "node id with plus",
			env:    withEnv(map[string]string{envNodeID: "sensor+1"}),
			wantIn: []string{envNodeID, "sensor+1"},
		},
		{
			name:   "node id with hash",
			env:    withEnv(map[string]string{envNodeID: "sensor#1"}),
			wantIn: []string{envNodeID, "sensor#1"},
		},
		{
			// Surrounding whitespace is trimmed, so an interior newline is the
			// one that survives into a topic segment — and into String(), whose
			// one-line-per-field shape TestStringListsEveryField asserts.
			name:   "node id with an interior newline",
			env:    withEnv(map[string]string{envNodeID: "quantum\nsensor"}),
			wantIn: []string{envNodeID, "control character"},
		},
		{
			name:   "node id with an interior NUL",
			env:    withEnv(map[string]string{envNodeID: "quantum\x00sensor"}),
			wantIn: []string{envNodeID, "control character"},
		},
		{
			name:   "topic prefix blank",
			env:    withEnv(map[string]string{envTopicPrefix: "  "}),
			wantIn: []string{envTopicPrefix, "must not be empty"},
		},
		{
			name:   "topic prefix with an interior newline",
			env:    withEnv(map[string]string{envTopicPrefix: "grow/daniel\nhome"}),
			wantIn: []string{envTopicPrefix, "control character"},
		},
		{
			name:   "topic prefix leading slash",
			env:    withEnv(map[string]string{envTopicPrefix: "/grow/daniel-home"}),
			wantIn: []string{envTopicPrefix, "/grow/daniel-home", "'/'"},
		},
		{
			name:   "topic prefix trailing slash",
			env:    withEnv(map[string]string{envTopicPrefix: "grow/daniel-home/"}),
			wantIn: []string{envTopicPrefix, "grow/daniel-home/", "'/'"},
		},
		{
			name:   "topic prefix with plus wildcard",
			env:    withEnv(map[string]string{envTopicPrefix: "grow/+"}),
			wantIn: []string{envTopicPrefix, "grow/+", "wildcard"},
		},
		{
			name:   "topic prefix with hash wildcard",
			env:    withEnv(map[string]string{envTopicPrefix: "grow/#"}),
			wantIn: []string{envTopicPrefix, "grow/#", "wildcard"},
		},
		{
			name:   "poll interval not a number",
			env:    withEnv(map[string]string{envPollSeconds: "ten"}),
			wantIn: []string{envPollSeconds, `"ten"`, "number of seconds"},
		},
		{
			name:   "poll interval NaN",
			env:    withEnv(map[string]string{envPollSeconds: "NaN"}),
			wantIn: []string{envPollSeconds, `"NaN"`, "number of seconds"},
		},
		{
			name:   "poll interval infinite",
			env:    withEnv(map[string]string{envPollSeconds: "Inf"}),
			wantIn: []string{envPollSeconds, `"Inf"`, "number of seconds"},
		},
		{
			name:   "poll interval below one second",
			env:    withEnv(map[string]string{envPollSeconds: "0.5"}),
			wantIn: []string{envPollSeconds, `"0.5"`, "at least 1s"},
		},
		{
			name:   "poll interval zero",
			env:    withEnv(map[string]string{envPollSeconds: "0"}),
			wantIn: []string{envPollSeconds, `"0"`, "at least 1s"},
		},
		{
			name:   "poll interval negative",
			env:    withEnv(map[string]string{envPollSeconds: "-10"}),
			wantIn: []string{envPollSeconds, `"-10"`, "at least 1s"},
		},
		{
			// The reported value is the raw one: every other emptiness message
			// shows what the operator actually typed, and " " reported as ""
			// reads like the variable was never set.
			name:   "sdi-12 address blank",
			env:    withEnv(map[string]string{envSDI12Address: " "}),
			wantIn: []string{envSDI12Address, `" "`, "[0-9a-zA-Z]"},
		},
		{
			name:   "sdi-12 address two characters",
			env:    withEnv(map[string]string{envSDI12Address: "01"}),
			wantIn: []string{envSDI12Address, `"01"`},
		},
		{
			name:   "sdi-12 address punctuation",
			env:    withEnv(map[string]string{envSDI12Address: "!"}),
			wantIn: []string{envSDI12Address, `"!"`},
		},
		{
			name:   "sdi-12 address multi-byte rune",
			env:    withEnv(map[string]string{envSDI12Address: "µ"}),
			wantIn: []string{envSDI12Address, "µ"},
		},
		{
			name:   "ppfd unit blank",
			env:    withEnv(map[string]string{envPPFDUnit: " "}),
			wantIn: []string{envPPFDUnit, "must not be empty"},
		},
		{
			// The same guard the node id and the topic prefix get, and for the
			// same reasons: the unit is stored as an InfluxDB tag value and is
			// interpolated into the sd_notify STATUS= line, whose payload is
			// newline-separated KEY=VALUE. sdnotify folds CR/LF on the way out,
			// so this is defence in depth rather than the only guard — but a
			// value that survives only because a consumer sanitises it is one
			// consumer away from being a problem.
			name:   "ppfd unit with a newline",
			env:    withEnv(map[string]string{envPPFDUnit: "µmol/s/m²\nWATCHDOG=1"}),
			wantIn: []string{envPPFDUnit, "control characters"},
		},
		{
			name:   "sdi-12 port blank",
			env:    withEnv(map[string]string{envSDI12Port: "  "}),
			wantIn: []string{envSDI12Port, "must not be empty"},
		},
		{
			name:   "read timeout zero",
			env:    withEnv(map[string]string{envReadTimeout: "0"}),
			wantIn: []string{envReadTimeout, `"0"`, "greater than zero"},
		},
		{
			name:   "read timeout negative",
			env:    withEnv(map[string]string{envReadTimeout: "-2"}),
			wantIn: []string{envReadTimeout, `"-2"`, "greater than zero"},
		},
		{
			name:   "read timeout not a number",
			env:    withEnv(map[string]string{envReadTimeout: "2s"}),
			wantIn: []string{envReadTimeout, `"2s"`, "number of seconds"},
		},
		{
			name:   "write timeout zero",
			env:    withEnv(map[string]string{envWriteTimeout: "0"}),
			wantIn: []string{envWriteTimeout, `"0"`, "greater than zero"},
		},
		{
			name:   "shutdown timeout zero",
			env:    withEnv(map[string]string{envShutdownTimeout: "0"}),
			wantIn: []string{envShutdownTimeout, `"0"`, "greater than zero"},
		},
		{
			name:   "shutdown timeout negative",
			env:    withEnv(map[string]string{envShutdownTimeout: "-0.5"}),
			wantIn: []string{envShutdownTimeout, `"-0.5"`, "greater than zero"},
		},
		{
			name:   "offline after zero",
			env:    withEnv(map[string]string{envOfflineAfter: "0"}),
			wantIn: []string{envOfflineAfter, `"0"`, "at least 1"},
		},
		{
			name:   "offline after negative",
			env:    withEnv(map[string]string{envOfflineAfter: "-3"}),
			wantIn: []string{envOfflineAfter, `"-3"`, "at least 1"},
		},
		{
			name:   "offline after not a number",
			env:    withEnv(map[string]string{envOfflineAfter: "three"}),
			wantIn: []string{envOfflineAfter, `"three"`, "whole number"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(fakeEnv(tt.env))
			if err == nil {
				t.Fatal("Load() = nil error, want a validation failure")
			}
			if n := len(joined(err)); n != 1 {
				t.Errorf("Load() reported %d errors, want exactly 1:\n%v", n, err)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load() error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestLoadAggregatesErrors is the reason validation uses errors.Join: an
// operator editing a unit file should learn about every problem in one restart,
// not one problem per restart.
func TestLoadAggregatesErrors(t *testing.T) {
	env := map[string]string{
		envMQTTUsername:    "edge-test",
		envMQTTPassword:    "", // 1: credentials
		envMQTTHost:        " ",
		envMQTTPort:        "70000",
		envNodeID:          "quantum/sensor",
		envTopicPrefix:     "/grow/daniel-home",
		envPollSeconds:     "0.1",
		envSDI12Address:    "??",
		envPPFDUnit:        " ",
		envSDI12Port:       " ",
		envReadTimeout:     "0",
		envWriteTimeout:    "-1",
		envShutdownTimeout: "nope",
		envOfflineAfter:    "0",
	}

	_, err := Load(fakeEnv(env))
	if err == nil {
		t.Fatal("Load() = nil error, want an aggregate validation failure")
	}

	// One error value, carrying one entry per broken variable.
	wantVars := []string{
		envMQTTPassword, envMQTTHost, envMQTTPort, envNodeID, envTopicPrefix,
		envPollSeconds, envSDI12Address, envPPFDUnit, envSDI12Port,
		envReadTimeout, envWriteTimeout, envShutdownTimeout, envOfflineAfter,
	}
	msg := err.Error()
	for _, v := range wantVars {
		if !strings.Contains(msg, v) {
			t.Errorf("aggregate error does not mention %s:\n%s", v, msg)
		}
	}
	if got, want := len(joined(err)), len(wantVars); got != want {
		t.Errorf("aggregate carries %d errors, want %d:\n%s", got, want, msg)
	}
}

// TestLoadFractionalPollSeconds covers the "may be fractional" half of
// POLL_SECONDS: values at or above the 1s floor are kept at sub-second
// resolution rather than truncated to whole seconds.
func TestLoadFractionalPollSeconds(t *testing.T) {
	tests := []struct {
		raw  string
		want time.Duration
	}{
		{"1", time.Second},
		{"1.5", 1500 * time.Millisecond},
		{"2.25", 2250 * time.Millisecond},
		{"10.001", 10001 * time.Millisecond},
		{"0.999", 0}, // just under the floor: rejected below, not rounded up
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			cfg, err := Load(fakeEnv(withEnv(map[string]string{envPollSeconds: tt.raw})))
			if tt.want == 0 {
				if err == nil {
					t.Fatalf("Load(POLL_SECONDS=%s) = nil error, want the sub-1s rejection", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(POLL_SECONDS=%s): unexpected error: %v", tt.raw, err)
			}
			if cfg.PollInterval != tt.want {
				t.Errorf("PollInterval = %v, want %v", cfg.PollInterval, tt.want)
			}
		})
	}
}

// TestLoadTrimsWhitespace documents the "set but padded" case: an
// EnvironmentFile line like `MQTT_HOST = 192.168.8.3` leaves a leading space,
// which should be tolerated everywhere except the password.
func TestLoadTrimsWhitespace(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
		envMQTTHost:     "  mqtt.home.arpa  ",
		envNodeID:       "\tquantum-sensor\n",
		envMQTTPassword: "  padded-password  ",
	}))
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}
	if cfg.MQTTHost != "mqtt.home.arpa" {
		t.Errorf("MQTTHost = %q, want %q", cfg.MQTTHost, "mqtt.home.arpa")
	}
	if cfg.NodeID != "quantum-sensor" {
		t.Errorf("NodeID = %q, want %q", cfg.NodeID, "quantum-sensor")
	}
	// Whitespace is legal in a password; stripping it would turn a working
	// credential into an auth failure that looks like a wrong password.
	if cfg.MQTTPassword != "  padded-password  " {
		t.Errorf("MQTTPassword = %q, want it preserved verbatim", cfg.MQTTPassword)
	}
}

// TestLoadDoesNotStatSerialPort guards the deliberate omission: USB enumeration
// can lag a Pi reboot, so a device path that does not exist yet must load fine
// and be retried at runtime.
func TestLoadDoesNotStatSerialPort(t *testing.T) {
	cfg, err := Load(fakeEnv(withEnv(map[string]string{
		envSDI12Port: "/dev/definitely-not-enumerated-yet",
	})))
	if err != nil {
		t.Fatalf("Load() with a non-existent device: unexpected error: %v", err)
	}
	if cfg.SDI12Port != "/dev/definitely-not-enumerated-yet" {
		t.Errorf("SDI12Port = %q, want it passed through verbatim", cfg.SDI12Port)
	}
}

func TestValidAddress(t *testing.T) {
	valid := []string{"0", "9", "a", "z", "A", "Z", "5"}
	for _, s := range valid {
		if !validAddress(s) {
			t.Errorf("validAddress(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "00", "-", "!", " ", "µ", "0 "}
	for _, s := range invalid {
		if validAddress(s) {
			t.Errorf("validAddress(%q) = true, want false", s)
		}
	}
}

// TestStringRedactsPassword is the one test that must never be deleted:
// --check-config output lands in terminals and pastes.
func TestStringRedactsPassword(t *testing.T) {
	const secret = "sup3r-s3cret-broker-password"

	cfg, err := Load(fakeEnv(withEnv(map[string]string{
		envMQTTPassword: secret,
		envStateDir:     "/var/lib/apogee-sq521",
	})))
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}

	out := cfg.String()
	if strings.Contains(out, secret) {
		t.Fatalf("String() leaked the password:\n%s", out)
	}
	if !strings.Contains(out, "<set>") {
		t.Errorf("String() = %q, want the password rendered as <set>", out)
	}

	// Empty password renders as <empty>, still never the value.
	empty := cfg
	empty.MQTTPassword = ""
	if !strings.Contains(empty.String(), "<empty>") {
		t.Errorf("String() with no password = %q, want it rendered as <empty>", empty.String())
	}
}

// TestStringListsEveryField keeps String() honest as fields are added: one line
// per field, each naming its env var and its value.
func TestStringListsEveryField(t *testing.T) {
	cfg, err := Load(fakeEnv(withEnv(map[string]string{envStateDir: "/var/lib/apogee-sq521"})))
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}

	out := cfg.String()
	if got, want := len(strings.Split(out, "\n")), 17; got != want {
		t.Errorf("String() rendered %d lines, want %d (one per field):\n%s", got, want, out)
	}

	wantSubstrings := []string{
		"NodeID", envNodeID, "quantum-sensor",
		"MQTTHost", envMQTTHost, "192.168.8.3",
		"MQTTPort", envMQTTPort, "1883",
		"MQTTUsername", envMQTTUsername, "edge-daniel-home",
		"MQTTPassword", envMQTTPassword,
		"TopicPrefix", envTopicPrefix, "grow/daniel-home",
		"SDI12Port", envSDI12Port, defaultSDI12Port,
		"SDI12Address", envSDI12Address,
		"PollInterval", envPollSeconds, "10s",
		"PPFDUnit", envPPFDUnit, "µmol/s/m²",
		"ReadTimeout", envReadTimeout, "2s",
		"WriteTimeout", envWriteTimeout, "1s",
		"OfflineAfter", envOfflineAfter, "3",
		"ShutdownTimeout", envShutdownTimeout, "3s",
		"StateDir", envStateDir, "/var/lib/apogee-sq521",
		"SubstrateSpec", envSubstrate, "no substrate probes",
		"SubstrateEvery", envSubstrateEvery, "15",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(out, want) {
			t.Errorf("String() output missing %q:\n%s", want, out)
		}
	}
}

// TestStringUnsetStateDir checks the disabled-persistence rendering, which is
// the one field whose empty value is normal rather than a misconfiguration.
func TestStringUnsetStateDir(t *testing.T) {
	cfg, err := Load(fakeEnv(validEnv()))
	if err != nil {
		t.Fatalf("Load(): unexpected error: %v", err)
	}
	if !strings.Contains(cfg.String(), "persistence disabled") {
		t.Errorf("String() with no STATE_DIRECTORY = %q, want it to flag persistence as disabled", cfg.String())
	}
}

// joined unwraps an errors.Join result into its constituent errors so tests can
// assert on how many distinct problems were reported. A non-joined error counts
// as one.
func joined(err error) []error {
	if err == nil {
		return nil
	}
	var multi interface{ Unwrap() []error }
	if errors.As(err, &multi) {
		return multi.Unwrap()
	}
	return []error{err}
}
