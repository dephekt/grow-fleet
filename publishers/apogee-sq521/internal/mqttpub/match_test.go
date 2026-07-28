package mqttpub

import "testing"

func TestTopicMatches(t *testing.T) {
	tests := []struct {
		filter string
		topic  string
		want   bool
	}{
		// Exact filters, which is all this daemon uses today.
		{"grow/x/status", "grow/x/status", true},
		{"grow/x/status", "grow/x/state", false},
		{"grow/x/status", "grow/x/status/extra", false},
		{"grow/x/status", "grow/x", false},

		// Single-level wildcard.
		{"grow/+/status", "grow/x/status", true},
		{"grow/+/status", "grow/x/y/status", false},
		{"grow/+", "grow/x", true},
		{"grow/+", "grow/", true}, // an empty level is still a level
		{"+/x", "grow/x", true},
		{"+", "grow", true},
		{"+", "grow/x", false},

		// Multi-level wildcard, including its match against the parent level.
		{"grow/#", "grow/x/y/z", true},
		{"grow/#", "grow/x", true},
		{"grow/#", "grow", true},
		{"grow/#", "growth/x", false},
		{"#", "anything/at/all", true},
		{"grow/x/#", "grow", false},

		// A leading wildcard never matches the broker's own topics.
		{"#", "$SYS/broker/uptime", false},
		{"+/broker/uptime", "$SYS/broker/uptime", false},
		{"$SYS/#", "$SYS/broker/uptime", true},

		// '#' is only a wildcard as the final level; an invalid filter matches
		// nothing rather than matching loosely.
		{"grow/#/status", "grow/x/status", false},
	}

	for _, tt := range tests {
		if got := topicMatches(tt.filter, tt.topic); got != tt.want {
			t.Errorf("topicMatches(%q, %q) = %v, want %v", tt.filter, tt.topic, got, tt.want)
		}
	}
}
