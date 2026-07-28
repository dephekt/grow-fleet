package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/config"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/entities"
)

// The two diagnostic modes. Both exist so that "did I get this right?" can be
// answered without a running service and without a broker — and, in Once's
// case, so that answering it cannot silently interleave with the service that
// is already running.

// Placeholders used by CheckConfig where the real value is only known once the
// sensor has been asked. They are deliberately not plausible values: the output
// doubles as a CI golden file, so it must be byte-stable, and a reader must
// never mistake one for something the daemon actually read.
//
// Parentheses rather than angle brackets because encoding/json escapes < and >
// to \u003c and \u003e by default, which would make the placeholder harder to
// read than the thing it stands in for.
const (
	placeholderSWVersion = "(0I! firmware)"
	placeholderSerial    = "(0I! serial)"
	placeholderCalFactor = "(0V! multiplier)"
)

// CheckConfig writes the resolved configuration and every retained message the
// daemon would announce, without touching the serial port or the broker.
//
// It answers "did I get the environment right" — the question that otherwise
// costs an edit, a systemctl restart and a journalctl — and its output is
// stable enough to diff in CI, which is what turns the discovery payloads into
// a golden test of the wire contract.
//
// The password is never rendered; config.Config.String reports it as <set> or
// <empty>. That matters because this output is read in terminals and pasted.
func CheckConfig(w io.Writer, cfg config.Config) error {
	topics := entities.Topics{Prefix: cfg.TopicPrefix, NodeID: cfg.NodeID}
	device := entities.Device{
		Identifiers:  []string{cfg.NodeID},
		Name:         deviceName,
		Manufacturer: deviceManufacturer,
		Model:        deviceModel,
		SWVersion:    placeholderSWVersion,
	}

	// A snapshot in which everything is available, so every row is rendered.
	// The real snapshot is built from one probe and may retire tilt or omit a
	// static; those cases are called out in the notes rather than guessed at
	// here, because guessing would make the output depend on hardware nobody
	// has attached.
	snap := entities.Snapshot{
		SWVersion: placeholderSWVersion,
		Statics: map[string]string{
			"sensor_serial":   placeholderSerial,
			calFactorObjectID: placeholderCalFactor,
		},
	}

	var b strings.Builder

	b.WriteString(configHeading + "\n")
	b.WriteString(rule(configHeading))
	b.WriteString(cfg.String())
	b.WriteString("\n\n")

	b.WriteString(discoveryHeading + "\n")
	b.WriteString(rule(discoveryHeading))
	b.WriteString("sw_version and the static payloads below are placeholders; the real values\n")
	b.WriteString("come from 0I! and 0V! at startup. Optional entities are announced only if\n")
	b.WriteString("the capability probe finds the sensor implements them.\n\n")

	table := entities.Build(entities.Params{PPFDUnit: cfg.PPFDUnit})
	for _, e := range entities.EligibleSet(table, snap) {
		payload, err := entities.DiscoveryPayload(e, topics, device)
		if err != nil {
			return fmt.Errorf("app: render discovery for %s: %w", e.ObjectID, err)
		}
		fmt.Fprintf(&b, "%s%s\n  %s\n", topics.Discovery(e.Component, e.ObjectID), optionalNote(e), payload)
	}
	for _, x := range auxTable(cfg.PPFDUnit) {
		payload, err := x.discoveryPayload(topics, device)
		if err != nil {
			return fmt.Errorf("app: render discovery for %s: %w", x.ObjectID, err)
		}
		fmt.Fprintf(&b, "%s\n  %s\n", topics.Discovery(x.Component, x.ObjectID), payload)
	}

	b.WriteString("\n" + otherHeading + "\n")
	b.WriteString(rule(otherHeading))
	fmt.Fprintf(&b, "availability  %s\n", topics.Status())
	fmt.Fprintf(&b, "              retained \"online\"/\"offline\"; also registered as the MQTT will\n")
	fmt.Fprintf(&b, "timezone cmd  %s\n", topics.Command(timeZoneComponent, timeZoneObjectID))
	fmt.Fprintf(&b, "              bare POSIX TZ string from grow-app's reconciler; echoed to\n")
	fmt.Fprintf(&b, "              %s\n", topics.State(timeZoneComponent, timeZoneObjectID))

	// The one arithmetic relationship an operator gets wrong. OFFLINE_AFTER
	// counts cycles, and a failing cycle spends the whole per-cycle budget
	// rather than finishing promptly, so the wait before the device card turns
	// grey is not OFFLINE_AFTER × POLL_SECONDS. Nobody computes this correctly
	// from two numbers in a config dump, so it is spelled out where the numbers
	// are read.
	cycle := cycleBudget(table, cfg.ReadTimeout)
	offlineAfter := time.Duration(cfg.OfflineAfter) * max(cfg.PollInterval, cycle)
	b.WriteString("\n" + timingHeading + "\n")
	b.WriteString(rule(timingHeading))
	fmt.Fprintf(&b, "cycle budget  %s\n", cycle)
	fmt.Fprintf(&b, "              one SDI-12 transaction budget per measurement sub-command; it\n")
	fmt.Fprintf(&b, "              follows the sensor's declared conversion time, not POLL_SECONDS\n")
	fmt.Fprintf(&b, "offline after %s (%d failed cycles)\n", offlineAfter, cfg.OfflineAfter)
	fmt.Fprintf(&b, "              NOT OFFLINE_AFTER x POLL_SECONDS: a silent cycle spends the whole\n")
	fmt.Fprintf(&b, "              cycle budget before it counts as failed, so the worst case is\n")
	fmt.Fprintf(&b, "              OFFLINE_AFTER x max(POLL_SECONDS, cycle budget)\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// The section headings of the --check-config report, and the underline that
// follows each. Derived from the heading rather than written out, so the two
// cannot drift by a character when a heading is reworded.
const (
	configHeading    = "resolved configuration"
	discoveryHeading = "discovery configs (retained, QoS 1)"
	otherHeading     = "other topics"
	timingHeading    = "timing"
)

func rule(heading string) string {
	return strings.Repeat("-", utf8.RuneCountInString(heading)) + "\n"
}

// optionalNote annotates a row whose presence depends on the probe.
func optionalNote(e entities.Entity) string {
	if e.Optional {
		return "   [probed; omitted when the sensor declares zero values for " + e.SubCmd + "]"
	}
	return ""
}

// Once runs one probe and one measurement cycle and writes the result to w.
//
// It takes the same two locks the daemon does — the caller holds the instance
// lock, and Dial takes flock plus TIOCEXCL on the tty — so running it while the
// service is up fails loudly instead of interleaving SDI-12 transactions with
// the running poller, which is the desync this codebase spends most of its
// comments preventing. That is the whole reason the README's smoke test became
// this rather than a mosquitto_sub one-liner.
//
// Nothing is published. The collaborators an App needs for the bus are
// substituted with no-ops here, so --once cannot perturb retained state.
func Once(ctx context.Context, cfg config.Config, dialer Dialer, log *slog.Logger, w io.Writer) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	a, err := New(cfg, Deps{
		Dialer:     dialer,
		Publisher:  discardPublisher{},
		Notifier:   discardNotifier{},
		Zones:      utcZones{},
		Integrator: discardIntegrator{},
		Rollovers:  &RolloverSink{},
		Logger:     log,
	})
	if err != nil {
		return err
	}
	return a.once(ctx, w)
}

// ErrDeviceUnavailable marks a --once failure that is about the hardware rather
// than about this binary: the adapter is not plugged in, or the sensor on the
// far end of it answered nothing usable.
//
// It exists so the README's smoke step has an exit code to test. Both outcomes
// used to exit 1, the same as an internal fault, so "not plugged in yet" and
// "the binary is broken" were indistinguishable in a script — and the smoke
// test is run precisely when neither has been established yet.
var ErrDeviceUnavailable = errors.New("app: the sensor could not be reached")

func (a *App) once(ctx context.Context, w io.Writer) error {
	session, err := a.deps.Dialer.Dial(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDeviceUnavailable, err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			a.log.Warn("closing the serial port", "error", err)
		}
	}()

	fmt.Fprintf(w, "device        %s\n", session)

	res := a.probe(ctx, session, &pollState{})
	a.adopt(res)
	snap := res.snap

	fmt.Fprintf(w, "sw_version    %s\n", orNone(snap.SWVersion))
	for _, objectID := range []string{serialObjectID, calFactorObjectID} {
		fmt.Fprintf(w, "%-13s %s\n", objectID, orNone(snap.Statics[objectID]))
	}
	if retired := unsupportedIDs(snap); len(retired) > 0 {
		fmt.Fprintf(w, "unsupported   %s\n", strings.Join(retired, ", "))
	}

	working := a.workingSet()
	cctx, cancel := context.WithTimeout(ctx, a.cycleBudget)
	defer cancel()
	readings, mr := a.measure(cctx, session, groupBySubCmd(working), &pollState{seen: map[string]int{}})

	fmt.Fprintln(w)
	for _, r := range readings {
		topic := a.topics.State(r.entity.Component, r.entity.ObjectID)
		if !r.ok {
			fmt.Fprintf(w, "%-13s %-56s (no reading)\n", r.entity.ObjectID, topic)
			continue
		}
		fmt.Fprintf(w, "%-13s %-56s %s %s\n",
			r.entity.ObjectID, topic, entities.Format(r.entity, r.value), r.entity.Unit)
	}
	if n := session.MissedServiceRequests(); n > 0 {
		fmt.Fprintf(w, "\nmissed data-ready notifications: %d (the USB bridge is dropping traffic)\n", n)
	}

	if mr.ok == 0 {
		if mr.first == nil {
			// ok == 0 with no error means no transaction was attempted, which
			// takes an empty working set. Unreachable today and deliberately
			// not tested: ppfd is not Optional, so entities.Eligible keeps it in
			// the set whatever the probe finds. The guard is here because the
			// only thing making it unreachable is a property of the table, and a
			// table edit that made every polled row Optional would otherwise
			// print "%!w(<nil>)" — %w's rendering of a nil error — in the first
			// output an operator ever reads from this binary.
			return fmt.Errorf("%w: the probe left no measurement to run", ErrDeviceUnavailable)
		}
		return fmt.Errorf("%w: no measurement produced a value: %w", ErrDeviceUnavailable, mr.first)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// unsupportedIDs lists the entities the probe definitively retired, sorted so
// the output is stable.
func unsupportedIDs(snap entities.Snapshot) []string {
	out := make([]string, 0, len(snap.Unsupported))
	for id, off := range snap.Unsupported {
		if off {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// The no-op collaborators Once substitutes. They exist here rather than in the
// test files because --once is a production mode: a diagnostic that published
// would defeat its own purpose.

type discardPublisher struct{}

func (discardPublisher) Start(context.Context) error { return nil }
func (discardPublisher) PublishRetained(context.Context, string, []byte) error {
	return errors.New("app: --once does not publish")
}
func (discardPublisher) Subscribe(context.Context, string, func(string, []byte)) error { return nil }
func (discardPublisher) OnConnectionUp(func())                                         {}
func (discardPublisher) Shutdown(context.Context, string) error                        { return nil }

type discardNotifier struct{}

func (discardNotifier) Ready() error                            { return nil }
func (discardNotifier) Stopping() error                         { return nil }
func (discardNotifier) Watchdog() error                         { return nil }
func (discardNotifier) Status(string) error                     { return nil }
func (discardNotifier) WatchdogInterval() (time.Duration, bool) { return 0, false }

type utcZones struct{}

func (utcZones) Apply(string) string      { return "" }
func (utcZones) State() string            { return "" }
func (utcZones) Location() *time.Location { return time.UTC }

type discardIntegrator struct{}

func (discardIntegrator) Add(time.Time, float64)             {}
func (discardIntegrator) SetLocation(*time.Location, string) {}
func (discardIntegrator) Snapshot() dli.Reading              { return dli.Reading{} }
func (discardIntegrator) Flush() error                       { return nil }
