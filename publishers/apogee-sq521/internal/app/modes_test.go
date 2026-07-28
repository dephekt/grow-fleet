package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/config"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

// --check-config is the answer to "did I get the environment right" without
// touching the bus, and its stability is what makes it usable as a CI golden
// file.
func TestCheckConfigOutput(t *testing.T) {
	cfg := testConfig()
	var b bytes.Buffer
	if err := CheckConfig(&b, cfg); err != nil {
		t.Fatalf("CheckConfig: %v", err)
	}
	got := b.String()

	// The credential must never appear. This output is read in terminals and
	// pasted into chat.
	if strings.Contains(got, cfg.MQTTPassword) {
		t.Fatal("the password appears in --check-config output")
	}
	if !strings.Contains(got, "MQTTPassword") || !strings.Contains(got, "<set>") {
		t.Error("the password field is missing or not reported as <set>")
	}

	// Every entity, from both tables, with its exact topic.
	topics := entities.Topics{Prefix: cfg.TopicPrefix, NodeID: cfg.NodeID}
	for _, e := range entities.Build(entities.Params{PPFDUnit: cfg.PPFDUnit}) {
		if !strings.Contains(got, topics.Discovery(e.Component, e.ObjectID)) {
			t.Errorf("%s discovery topic missing from the report", e.ObjectID)
		}
	}
	for _, x := range auxTable(cfg.PPFDUnit) {
		if !strings.Contains(got, topics.Discovery(x.Component, x.ObjectID)) {
			t.Errorf("%s discovery topic missing from the report", x.ObjectID)
		}
	}
	for _, want := range []string{
		topics.Status(),
		topics.Command(timeZoneComponent, timeZoneObjectID),
		topics.State(timeZoneComponent, timeZoneObjectID),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("topic %q missing from the report", want)
		}
	}

	// Every payload it prints must be the JSON it would really publish.
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Errorf("a printed discovery payload is not valid JSON: %v\n%s", err, line)
		}
	}

	// Byte-stable across calls, or it is worthless as a golden file.
	var again bytes.Buffer
	if err := CheckConfig(&again, cfg); err != nil {
		t.Fatalf("CheckConfig: %v", err)
	}
	if again.String() != got {
		t.Error("--check-config output is not stable across calls")
	}
}

// The report is printed even for an invalid config, because the operator needs
// to see which variables were read in order to fix the ones that were not — and
// a Config returned alongside an error is only partially populated, with zero
// values where parsing failed.
func TestCheckConfigRendersAPartialConfig(t *testing.T) {
	env := map[string]string{
		"MQTT_PORT":    "not a number",
		"POLL_SECONDS": "-1",
		// No MQTT_PASSWORD: the headline validation failure.
	}
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("the broken environment was accepted; this test needs rewriting")
	}

	var b bytes.Buffer
	if err := CheckConfig(&b, cfg); err != nil {
		t.Fatalf("CheckConfig on a partial config: %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "<empty>") {
		t.Error("the report does not show the empty password that made the config invalid")
	}
	// The zero-valued fields still render, so the operator can see them.
	if !strings.Contains(got, "MQTTPort") {
		t.Error("the report omits the field that failed to parse")
	}
}

// --once is the README's smoke test. It has to reach the sensor, print what it
// found, and publish nothing.
func TestOncePrintsAProbeAndACycleWithoutPublishing(t *testing.T) {
	session := newHealthySession()
	dialer := &fakeDialer{fn: func(int) (Session, error) { return session, nil }}

	var b bytes.Buffer
	if err := Once(t.Context(), testConfig(), dialer, nil, &b); err != nil {
		t.Fatalf("Once: %v", err)
	}
	got := b.String()

	for _, want := range []string{
		"104",     // the firmware from 0I!
		"3043",    // the serial
		"1.23457", // the cal factor, %.6g
		"156.9",   // PPFD, at the table's payload precision
		"grow/daniel-home/quantum-sensor/sensor/ppfd/state",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--once output does not mention %q:\n%s", want, got)
		}
	}
	// One session, opened and closed.
	if session.closeCount() != 1 {
		t.Errorf("the port was closed %d times, want 1", session.closeCount())
	}
	if dialer.dials() != 1 {
		t.Errorf("dials = %d, want 1", dialer.dials())
	}
}

// A sensor that answers nothing is a failure, not a quiet success: the whole
// point of the mode is to fail loudly.
func TestOnceFailsWhenNothingAnswers(t *testing.T) {
	session := newHealthySession()
	for _, sub := range []string{"", "1", "4"} {
		session.script(sub, step{err: errNoResponse})
	}
	dialer := &fakeDialer{fn: func(int) (Session, error) { return session, nil }}

	var b bytes.Buffer
	err := Once(t.Context(), testConfig(), dialer, nil, &b)
	if err == nil {
		t.Fatalf("Once returned nil for a sensor that answered nothing:\n%s", b.String())
	}
	if !errors.Is(err, ErrDeviceUnavailable) {
		t.Errorf("Once = %v, want it classified as unreachable hardware", err)
	}
}

// Both hardware failures are marked as such. --once is the README's smoke step
// and is run before anything has been established, so "the adapter has not
// enumerated yet" and "this binary is broken" have to be distinguishable by a
// script; cmd/apogee-sq521 turns this classification into exit code 69.
func TestOnceClassifiesAnUnreachableDevice(t *testing.T) {
	tests := map[string]func() Dialer{
		"the adapter is not there": func() Dialer {
			return &fakeDialer{fn: func(int) (Session, error) {
				return nil, errors.New("resolve /dev/serial/by-id/…: no such file or directory")
			}}
		},
		"the adapter is there and the sensor is not": func() Dialer {
			session := newHealthySession()
			for _, sub := range []string{"", "1", "4"} {
				session.script(sub, step{err: errNoResponse})
			}
			return &fakeDialer{fn: func(int) (Session, error) { return session, nil }}
		},
	}

	for name, dialer := range tests {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			err := Once(t.Context(), testConfig(), dialer(), nil, &b)
			if !errors.Is(err, ErrDeviceUnavailable) {
				t.Errorf("Once = %v, want ErrDeviceUnavailable", err)
			}
		})
	}
}

// The one arithmetic relationship an operator gets wrong, stated where the
// numbers are read. OFFLINE_AFTER counts cycles and a silent cycle spends the
// whole cycle budget, so the wait before the device card turns grey is
// OFFLINE_AFTER x max(POLL_SECONDS, cycle budget) — 99 s at the defaults, not
// the 30 s that two numbers in a config dump suggest.
func TestCheckConfigStatesTheOutageTiming(t *testing.T) {
	cfg := testConfig()
	var b bytes.Buffer
	if err := CheckConfig(&b, cfg); err != nil {
		t.Fatalf("CheckConfig: %v", err)
	}
	got := b.String()

	table := entities.Build(entities.Params{PPFDUnit: cfg.PPFDUnit})
	cycle := cycleBudget(table, cfg.ReadTimeout)
	want := time.Duration(cfg.OfflineAfter) * max(cfg.PollInterval, cycle)
	if want <= time.Duration(cfg.OfflineAfter)*cfg.PollInterval {
		t.Fatal("at these defaults the two computations agree, so this test proves nothing")
	}

	for _, phrase := range []string{cycle.String(), want.String(), "NOT OFFLINE_AFTER"} {
		if !strings.Contains(got, phrase) {
			t.Errorf("the report does not state %q:\n%s", phrase, got)
		}
	}
}

// The report is advertised as a byte-stable CI golden, and a column that is one
// character too narrow for the longest variable name pushes exactly one row's
// value out of line.
func TestCheckConfigColumnsAlign(t *testing.T) {
	var b bytes.Buffer
	if err := CheckConfig(&b, testConfig()); err != nil {
		t.Fatalf("CheckConfig: %v", err)
	}

	// The resolved-configuration section only: the discovery payloads below it
	// are single-field JSON lines with parentheses of their own.
	report := b.String()
	if at := strings.Index(report, discoveryHeading); at > 0 {
		report = report[:at]
	}

	column := -1
	for _, line := range strings.Split(report, "\n") {
		open := strings.Index(line, " (")
		close := strings.Index(line, ")")
		if open < 0 || close < open {
			continue
		}
		open++ // past the space, onto the "("
		if env := line[open+1 : close]; env != strings.ToUpper(env) {
			continue // not a "Name (ENV_VAR) value" row
		}
		// Where the value begins: the first non-space rune after the ")".
		at := close + 1
		for at < len(line) && line[at] == ' ' {
			at++
		}
		if at >= len(line) {
			continue
		}
		if column < 0 {
			column = at
			continue
		}
		if at != column {
			t.Errorf("the value column moves to %d (want %d) on:\n  %s", at, column, line)
		}
	}
	if column < 0 {
		t.Fatal("no configuration rows were found; this test is looking at the wrong output")
	}
}

// --once must never perturb retained state, which is why its Publisher refuses
// every publish rather than silently discarding it.
func TestOnceCannotPublish(t *testing.T) {
	if err := (discardPublisher{}).PublishRetained(context.Background(), "any/topic", nil); err == nil {
		t.Error("the --once publisher accepted a publish; a diagnostic that writes to the bus defeats its purpose")
	}
}

// A dead adapter is reported rather than retried: --once is a one-shot, and the
// operator wants the error, not a backoff loop.
func TestOnceReportsADialFailure(t *testing.T) {
	dialer := &fakeDialer{fn: func(int) (Session, error) { return nil, errPortDead }}
	var b bytes.Buffer
	if err := Once(t.Context(), testConfig(), dialer, nil, &b); err == nil {
		t.Error("Once returned nil when the port could not be opened")
	}
}

// THE INVARIANT: no time.Sleep in the binary.
//
// Every wait has to be a select on ctx.Done() or it cannot be interrupted, and
// an uninterruptible wait is how the Python's 36-second serial-open retry loop
// held SIGTERM for half a minute. The check is a parse rather than a grep so a
// mention inside a comment or a string does not fail it — and so that a
// `time.Sleep` smuggled in under an alias does.
func TestNoTimeSleepInTheBinary(t *testing.T) {
	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		// Resolve whatever name "time" was imported under, so an alias cannot
		// hide the call.
		timeName := ""
		for _, imp := range file.Imports {
			if imp.Path.Value != `"time"` {
				continue
			}
			timeName = "time"
			if imp.Name != nil {
				timeName = imp.Name.Name
			}
		}
		if timeName == "" {
			return nil
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Sleep" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != timeName {
				return true
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s:%d calls time.Sleep; every wait must select on ctx.Done() or it cannot be interrupted",
				rel, fset.Position(call.Pos()).Line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
}

// moduleRoot finds the directory holding go.mod, starting from this file.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this test")
		}
		dir = parent
	}
}

// The --once collaborators are production code, not test doubles: they are what
// keeps a diagnostic run from touching the bus, the service manager or the
// state directory. They must be inert rather than merely unused.
func TestOnceCollaboratorsAreInert(t *testing.T) {
	ctx := context.Background()

	pub := discardPublisher{}
	if err := pub.Start(ctx); err != nil {
		t.Errorf("Start = %v, want nil", err)
	}
	if err := pub.Subscribe(ctx, "any", func(string, []byte) {}); err != nil {
		t.Errorf("Subscribe = %v, want nil", err)
	}
	pub.OnConnectionUp(func() { t.Error("a connection-up callback ran in --once") })
	if err := pub.Shutdown(ctx, "any"); err != nil {
		t.Errorf("Shutdown = %v, want nil", err)
	}

	n := discardNotifier{}
	for name, call := range map[string]func() error{
		"Ready":    n.Ready,
		"Stopping": n.Stopping,
		"Watchdog": n.Watchdog,
		"Status":   func() error { return n.Status("x") },
	} {
		if err := call(); err != nil {
			t.Errorf("%s = %v, want nil", name, err)
		}
	}
	if _, armed := n.WatchdogInterval(); armed {
		t.Error("--once reported an armed watchdog; nothing supervises it")
	}

	z := utcZones{}
	if got := z.Apply("GMT0BST,M3.5.0/1,M10.5.0"); got != "" {
		t.Errorf("Apply = %q, want \"\"; --once must not adopt a pushed zone", got)
	}
	if z.State() != "" {
		t.Errorf("State = %q, want \"\"", z.State())
	}
	if z.Location() != time.UTC {
		t.Errorf("Location = %v, want UTC", z.Location())
	}

	acc := discardIntegrator{}
	acc.Add(time.Now(), 500)
	acc.SetLocation(time.UTC, "UTC0")
	if got := acc.Snapshot(); got != (dli.Reading{}) {
		t.Errorf("Snapshot = %+v, want the zero reading; --once must not resume a day", got)
	}
	if err := acc.Flush(); err != nil {
		t.Errorf("Flush = %v, want nil; --once must never write the state file", err)
	}
}

// CONSTRAINT 3: entities.Build is the only source of an entities.Entity.
//
// entities.Format panics on an unset FormatKind, by design — it makes a
// forgotten field a property the compiler almost catches, and the alternative
// (falling through to fixed-point) writes wrong-but-plausible numbers into
// InfluxDB where nobody ever notices. But that design only holds while the
// table is the sole producer: an Entity assembled at a call site can omit
// PayloadFormat, and the panic then lands in the poll goroutine and takes the
// process down.
//
// The auxiliary table is the sanctioned way to add an entity this daemon
// publishes that the sensor does not measure; see aux.go for why those two rows
// cannot be expressed as entities.Entity at all.
//
// # What this guard does and does not cover
//
// It is a syntactic check without type information, so it catches the three
// spellings that actually produce a fresh Entity a caller can then fill in:
// a composite literal, a var declaration of that type, and new(). It does not
// and cannot catch, say, a value returned from a helper in another package.
// entities.Format's panic is the real protection; this is the part that can be
// enforced before the process runs, and the doc comment is narrowed to what it
// really checks rather than to the ambition.
func TestNoEntityIsConstructedOutsideTheTable(t *testing.T) {
	root := moduleRoot(t)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The table's own package is where entities are supposed to be built.
		if filepath.Base(filepath.Dir(path)) == "entities" {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		pkgName := ""
		for _, imp := range file.Imports {
			if !strings.HasSuffix(imp.Path.Value, `/internal/entities"`) {
				continue
			}
			pkgName = "entities"
			if imp.Name != nil {
				pkgName = imp.Name.Name
			}
		}
		if pkgName == "" {
			return nil
		}

		// isEntityType reports whether an expression names entities.Entity
		// itself — not a slice, pointer or map of it, which every function
		// signature in this package is full of.
		isEntityType := func(expr ast.Expr) bool {
			sel, ok := expr.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Entity" {
				return false
			}
			pkg, ok := sel.X.(*ast.Ident)
			return ok && pkg.Name == pkgName
		}

		report := func(pos token.Pos, how string) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s:%d %s an entities.Entity outside entities.Build; a row with no PayloadFormat panics in Format",
				rel, fset.Position(pos).Line, how)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if !isEntityType(node.Type) {
					return true
				}
				// The zero value returned on a lookup miss is the one
				// exception: it is never formatted, only reported alongside
				// ok == false.
				if len(node.Elts) == 0 {
					return true
				}
				report(node.Pos(), "builds")

			case *ast.ValueSpec:
				// `var e entities.Entity`, which the composite-literal check
				// misses entirely — the fields are assigned afterwards, and an
				// init() doing that passed the guard while the doc claimed it
				// could not.
				if node.Type != nil && isEntityType(node.Type) {
					report(node.Pos(), "declares")
				}

			case *ast.CallExpr:
				fn, ok := node.Fun.(*ast.Ident)
				if !ok || fn.Name != "new" || len(node.Args) != 1 {
					return true
				}
				if isEntityType(node.Args[0]) {
					report(node.Pos(), "allocates")
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the module: %v", err)
	}
}

// The guard above has to fail on all three spellings, or its doc comment is a
// claim about code nobody checked. Parsing a source string keeps the fixtures
// out of the package's own build.
func TestTheEntityConstructionGuardCatchesEachSpelling(t *testing.T) {
	const preamble = `package sample

import "github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"

`
	tests := map[string]struct {
		body      string
		wantCount int
	}{
		"composite literal": {body: "var _ = entities.Entity{ObjectID: \"x\"}\n", wantCount: 1},
		"var declaration":   {body: "var leaked entities.Entity\n", wantCount: 1},
		"new":               {body: "var _ = new(entities.Entity)\n", wantCount: 1},
		"empty literal is the sanctioned lookup miss": {
			body: "func f() (entities.Entity, bool) { return entities.Entity{}, false }\n",
		},
		"a slice parameter is not a construction": {
			body: "func f(table []entities.Entity) int { return len(table) }\n",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := countEntityConstructions(t, preamble+tc.body)
			if got != tc.wantCount {
				t.Errorf("the guard found %d constructions, want %d, in:\n%s", got, tc.wantCount, tc.body)
			}
		})
	}
}

// countEntityConstructions is the guard's matcher, run over one source string.
// It is deliberately a copy rather than a shared helper: the check above walks
// real files and reports through *testing.T, and threading a counter through it
// would complicate the thing being protected in order to test it.
func countEntityConstructions(t *testing.T, src string) int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}

	isEntityType := func(expr ast.Expr) bool {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Entity" {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "entities"
	}

	n := 0
	ast.Inspect(file, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.CompositeLit:
			if isEntityType(v.Type) && len(v.Elts) > 0 {
				n++
			}
		case *ast.ValueSpec:
			if v.Type != nil && isEntityType(v.Type) {
				n++
			}
		case *ast.CallExpr:
			if fn, ok := v.Fun.(*ast.Ident); ok && fn.Name == "new" && len(v.Args) == 1 && isEntityType(v.Args[0]) {
				n++
			}
		}
		return true
	})
	return n
}
