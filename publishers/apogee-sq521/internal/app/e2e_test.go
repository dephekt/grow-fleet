package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/dli"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/mqttpub"
	"github.com/dephekt/grow-fleet/publishers/apogee-sq521/internal/timezone"
)

// The end-to-end check: a real broker, the real mqttpub publisher, the real
// timezone manager and the real DLI accumulator — everything except the serial
// port, which is the one thing that needs hardware.
//
// It exists because the fakes cannot prove the two properties that only appear
// once autopaho, paho's session state and a retained store are in the loop:
// that the whole birth sequence really lands in the broker's retained store
// (which is what grow-app reads on startup), and that the retained availability
// ends at "offline" after a clean stop.
func TestEndToEndAgainstARealBroker(t *testing.T) {
	broker := startBroker(t)
	host, port := broker.host()

	cfg := testConfig()
	cfg.MQTTHost, cfg.MQTTPort = host, port
	cfg.StateDir = t.TempDir()
	cfg.PollInterval = 20 * time.Millisecond
	base := cfg.TopicPrefix + "/" + cfg.NodeID
	broker.watch(cfg.TopicPrefix + "/#")

	log := testLogger(t)
	pub, err := mqttpub.New(mqttpub.Config{
		Host:            cfg.MQTTHost,
		Port:            cfg.MQTTPort,
		Username:        cfg.MQTTUsername,
		Password:        cfg.MQTTPassword,
		NodeID:          cfg.NodeID,
		StatusTopic:     base + "/status",
		ShutdownTimeout: cfg.ShutdownTimeout,
		Logger:          log,
	})
	if err != nil {
		t.Fatalf("mqttpub.New: %v", err)
	}

	zones := timezone.NewManager(timezone.NewStore(cfg.StateDir), time.UTC, log)
	zones.Start()
	sink := &RolloverSink{}
	acc, err := dli.New(dli.Config{
		Location:   zones.Location(),
		Zone:       zones.State(),
		StatePath:  dli.StatePath(cfg.StateDir),
		Logger:     log,
		OnRollover: sink.Handle,
	})
	if err != nil {
		t.Fatalf("dli.New: %v", err)
	}

	// A pre-3033 unit, so the run also proves that a probed-away entity never
	// reaches the retained store in the first place.
	session := newHealthySession()
	session.script("4", step{err: errHeaderZero})

	a, err := New(cfg, Deps{
		Dialer:     &fakeDialer{fn: func(int) (Session, error) { return session, nil }},
		Publisher:  pub,
		Notifier:   &fakeNotifier{},
		Zones:      zones,
		Integrator: acc,
		Rollovers:  sink,
		Logger:     log,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(ctx) }()

	broker.waitFor("the birth sequence and a first reading", 10*time.Second, func(msgs []brokerMessage) bool {
		var online, ppfd bool
		for _, m := range msgs {
			switch {
			case m.topic == base+"/status" && m.payload == "online":
				online = true
			case m.topic == base+"/sensor/ppfd/state" && m.payload != "":
				ppfd = true
			}
		}
		return online && ppfd
	})

	// What grow-app actually reads on startup: the retained store.
	for _, tc := range []struct{ topic, want string }{
		{base + "/status", "online"},
		{base + "/sensor/ppfd/state", "156.9"},
		{base + "/sensor/sensor_serial/state", "3043"},
		{base + "/sensor/cal_factor/state", "1.23457"},
	} {
		got, ok := broker.retained(tc.topic)
		if !ok {
			t.Errorf("%s holds no retained message", tc.topic)
			continue
		}
		if got != tc.want {
			t.Errorf("retained %s = %q, want %q", tc.topic, got, tc.want)
		}
	}

	// The discovery config carries the firmware the probe read, which is the
	// end of the chain that starts at 0I!.
	cfgTopic := cfg.TopicPrefix + "/_discovery/sensor/" + cfg.NodeID + "/ppfd/config"
	raw, ok := broker.retained(cfgTopic)
	if !ok {
		t.Fatalf("%s holds no retained discovery config", cfgTopic)
	}
	var decoded struct {
		StateTopic string `json:"state_topic"`
		Device     struct {
			SWVersion string `json:"sw_version"`
		} `json:"device"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("discovery config is not JSON: %v", err)
	}
	if decoded.Device.SWVersion != "104" {
		t.Errorf("sw_version = %q, want the firmware from 0I!", decoded.Device.SWVersion)
	}

	// tilt was probed away, so nothing about it is *retained*. The daemon does
	// publish an empty payload to its two topics, which is the retraction that
	// clears whatever a previous process may have left there — and the point of
	// checking the retained store rather than the traffic is that an empty
	// retained payload leaves the store with no entry at all.
	for _, m := range broker.observed() {
		if strings.Contains(m.topic, "/tilt/") && m.payload != "" {
			t.Errorf("a probed-away entity reached the bus: %s", m)
		}
	}
	for _, topic := range []string{
		cfg.TopicPrefix + "/_discovery/sensor/" + cfg.NodeID + "/tilt/config",
		base + "/sensor/tilt/state",
	} {
		if got, ok := broker.retained(topic); ok {
			t.Errorf("retained %s = %q, want nothing; the entity was probed away", topic, got)
		}
	}

	// The timezone round trip, over a real subscription.
	const posix = "GMT0BST,M3.5.0/1,M10.5.0"
	broker.publish(base+"/text/time_zone/command", []byte(posix))
	broker.waitFor("the timezone echo", 5*time.Second, func(msgs []brokerMessage) bool {
		for _, m := range msgs {
			if m.topic == base+"/text/time_zone/state" && m.payload == posix {
				return true
			}
		}
		return false
	})
	if got := zones.Location().String(); got != posix {
		t.Errorf("effective zone = %q, want %q", got, posix)
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The whole reason mqttpub exists: a stopped process must never leave
	// "online" retained, because grow-app has no staleness and would serve the
	// last reading as live indefinitely.
	if got, ok := broker.retained(base + "/status"); !ok || got != "offline" {
		t.Errorf("retained status after a clean stop = %q (present=%v), want offline", got, ok)
	}
}
