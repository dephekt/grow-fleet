// Command apogee-sq521 publishes an Apogee SQ-521 quantum (PAR) sensor to the
// grow MQTT bus.
//
// This file is deliberately thin: flags, the signal context, the config load,
// the two locks, and exit codes. Everything else is internal/app, so that the
// orchestration can be tested without a process.
//
// Exit codes follow sysexits(3):
//
//	0   clean shutdown, and a successful --help or --version
//	1   an unexpected failure
//	69  EX_UNAVAILABLE — --once could not reach the sensor
//	75  EX_TEMPFAIL    — another instance already holds this node's lock
//	78  EX_CONFIG      — the environment is wrong; fix it and restart
//
// The split matters to the unit file. A systemd Restart=on-failure will loop on
// 78 forever without ever succeeding, so an operator wants that code to be
// unmistakable in `systemctl status`; 75 says the same about a second copy.
//
// 69 matters to the README's smoke step rather than to systemd: --once is run
// before anything has been established, so "the adapter is not plugged in yet"
// and "this binary is broken" have to be distinguishable in a script. They both
// used to be 1.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/app"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/config"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/mqttpub"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/sdnotify"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/serialport"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/timezone"
)

// sysexits(3) codes. See the package doc for why these three are called out.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUnavailable = 69 // EX_UNAVAILABLE
	exitTempFail    = 75 // EX_TEMPFAIL
	exitConfig      = 78 // EX_CONFIG
)

// version is stamped at link time (-ldflags "-X main.version=…").
var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apogee-sq521", flag.ContinueOnError)
	// flag writes the usage to its own single output for both a help request and
	// a parse error, and those two belong on different streams: `--help` is a
	// question that was asked, so `apogee-sq521 --help | less` has to work, while
	// a malformed invocation is a diagnostic and must stay out of a pipeline's
	// data. Discarding here and reprinting below is what separates them; it also
	// means the error text is ours to print rather than flag's to interleave.
	fs.SetOutput(io.Discard)
	var (
		checkConfig = fs.Bool("check-config", false,
			"validate the environment, print the resolved config and the discovery messages, and exit")
		once = fs.Bool("once", false,
			"take both locks, run one probe and one measurement cycle to stdout, and exit")
		showVersion = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		// --help and -h are a successful request for information, not a
		// malformed invocation: the usage goes to stdout, like --version's
		// answer, and the code is 0. Returning 78 for them breaks
		// `apogee-sq521 --help && …` and makes the binary look broken to
		// anything that checks.
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return exitOK
		}
		// Anything else is a diagnostic: the reason and the usage both go to
		// stderr. EX_CONFIG rather than EX_USAGE because the operator-visible
		// cause is the same — the unit file is wrong — and it keeps the set of
		// codes this binary can return to the ones the package doc lists.
		fmt.Fprintln(stderr, err)
		fs.SetOutput(stderr)
		fs.Usage()
		return exitConfig
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return exitOK
	}

	log := newLogger(stderr)

	cfg, cfgErr := config.Load(os.Getenv)
	if *checkConfig {
		// The resolved config is printed even when it is invalid: a partially
		// populated Config still shows which variables were read, which is what
		// an operator needs in order to fix the ones that were not.
		if err := app.CheckConfig(stdout, cfg); err != nil {
			fmt.Fprintln(stderr, err)
			return exitFailure
		}
		if cfgErr != nil {
			fmt.Fprintf(stderr, "\nconfiguration is invalid:\n%v\n", cfgErr)
			return exitConfig
		}
		return exitOK
	}
	if cfgErr != nil {
		// Every problem at once. The feedback loop here is edit, restart,
		// journalctl, and fixing one typo per restart is miserable.
		log.Error("invalid configuration", "problems", cfgErr)
		return exitConfig
	}

	// Layer 1 of the instance guard, and the only one that covers the boot
	// window: USB enumeration lags a Pi reboot by seconds, and during that gap
	// there is no device for two copies to fight over yet.
	guard, err := serialport.AcquireInstance(cfg.NodeID)
	if err != nil {
		if errors.Is(err, serialport.ErrInUse) {
			log.Error("another instance is already publishing this node", "node_id", cfg.NodeID, "error", err)
			return exitTempFail
		}
		log.Error("could not take the instance lock", "node_id", cfg.NodeID, "error", err)
		return exitFailure
	}
	defer func() { _ = guard.Close() }()

	// SIGINT and SIGTERM cancel rootCtx; every wait in the daemon selects on
	// it, so a stop is bounded by the deadline of whatever serial read happens
	// to be in flight rather than by a retry loop.
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dialer := app.NewSerialDialer(cfg, log)
	if *once {
		if err := app.Once(rootCtx, cfg, dialer, log, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			// The hardware and this binary get different codes. --once is run
			// before either has been established, so a script that cannot tell
			// "the adapter has not enumerated yet" from "the daemon is broken"
			// cannot be used to establish the first one.
			if errors.Is(err, app.ErrDeviceUnavailable) {
				return exitUnavailable
			}
			return exitFailure
		}
		return exitOK
	}

	if err := serve(rootCtx, cfg, dialer, log); err != nil {
		log.Error("could not start", "error", err)
		return exitFailure
	}
	return exitOK
}

// serve builds the collaborators, hands them to the app, and blocks until the
// daemon stops.
//
// A returned error means the daemon could not *start*, which is a fault worth a
// non-zero exit. A teardown that did not complete is not: mqttpub asks the
// broker to publish the will whenever it cannot confirm the retained "offline"
// itself, so the availability still ends up correct, and a failed DLI flush
// loses at most one persist interval of integration. Exiting non-zero for
// either would mark the unit failed on every `systemctl stop` taken while the
// broker happened to be down.
//
// The construction order is load-bearing in one place: the timezone override is
// restored and validated synchronously, before MQTT can connect, so the
// retained state published on the first connection is always the zone actually
// in force. Doing it later is what forces the ESPHome component to carry a
// "wrote before setup" flag; here the file is read to completion before
// anything can subscribe.
func serve(ctx context.Context, cfg config.Config, dialer app.Dialer, log *slog.Logger) error {
	zones := timezone.NewManager(timezone.NewStore(cfg.StateDir), localZone(), log)
	zones.Start()

	if !cfg.PersistenceEnabled() {
		log.Info("no STATE_DIRECTORY granted; the timezone override and the day's DLI will not survive a restart")
	}

	rollovers := &app.RolloverSink{}
	integrator, err := dli.New(dli.Config{
		Location:   zones.Location(),
		Zone:       zones.State(),
		StatePath:  dli.StatePath(cfg.StateDir),
		Logger:     log,
		OnRollover: rollovers.Handle,
	})
	if err != nil {
		return fmt.Errorf("dli: %w", err)
	}

	topics := struct{ Prefix, NodeID string }{cfg.TopicPrefix, cfg.NodeID}
	pub, err := mqttpub.New(mqttpub.Config{
		Host:     cfg.MQTTHost,
		Port:     cfg.MQTTPort,
		Username: cfg.MQTTUsername,
		Password: cfg.MQTTPassword,
		NodeID:   cfg.NodeID,
		// The will topic has to be known before the first CONNECT, which is
		// why it is built here rather than asked of the app later.
		StatusTopic:     topics.Prefix + "/" + topics.NodeID + "/status",
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          log,
	})
	if err != nil {
		return err
	}

	a, err := app.New(cfg, app.Deps{
		Dialer:     dialer,
		Publisher:  pub,
		Notifier:   sdnotify.New(),
		Zones:      zones,
		Integrator: integrator,
		Rollovers:  rollovers,
		Logger:     log,
	})
	if err != nil {
		return err
	}

	if err := a.Run(ctx); err != nil {
		log.Warn("teardown did not complete cleanly; the broker's will covers the retained availability",
			"error", err)
	}
	return nil
}

// localZone is the fallback the timezone manager uses until grow-app pushes an
// override.
//
// time.Local reflects /etc/localtime, which on this Pi is whatever the image
// was built with. It is only a default: the reconciler's push takes precedence
// the moment it arrives, and dli records which zone a day was measured under so
// a change cannot silently mix two bases.
func localZone() *time.Location { return time.Local }

// newLogger writes structured text to stderr, which systemd captures into the
// journal.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
