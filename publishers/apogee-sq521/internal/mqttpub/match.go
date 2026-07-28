package mqttpub

import "strings"

// topicMatches reports whether an MQTT topic filter matches a topic name, per
// MQTT v5 §4.7.
//
// The broker has already done this once — it only sends us PUBLISHes for
// filters we subscribed to — but a Publisher may hold several subscriptions at
// once and paho hands every inbound message to every callback. Without matching
// here, adding a second Subscribe would silently start feeding each callback
// the other's traffic.
//
// Nothing on the SQ-521 is writable today, so in practice the filters are exact
// command topics. Wildcards are supported anyway because the alternative —
// exact string comparison — turns a filter containing '+' into a subscription
// that receives nothing, which is a much quieter failure than it deserves.
func topicMatches(filter, topic string) bool {
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")

	// A leading wildcard never matches a $-prefixed topic: $SYS and friends are
	// the broker's own, and a subscriber asking for "#" is not asking for those.
	if strings.HasPrefix(topic, "$") && (f[0] == "#" || f[0] == "+") {
		return false
	}

	for i, level := range f {
		if level == "#" {
			// "#" is only a wildcard as the final level, and it matches the
			// parent level too: "a/#" matches "a" as well as "a/b/c".
			return i == len(f)-1 && i <= len(t)
		}
		if i >= len(t) {
			return false
		}
		if level != "+" && level != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}
