package connect

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// shippedRetainedAckTimeout is the default silence bound for retained returns. It
// is a first guess that has not been validated against fleet ack-silence data, so
// it is configurable.
const shippedRetainedAckTimeout = 240 * time.Second

// retainedAckTimeoutDefault is the RetainedAckTimeout a fresh settings value
// carries. URNETWORK_RETAINED_ACK_TIMEOUT overrides it in whole seconds; 0 turns
// the bound off, leaving retained items to their per-item backstop. An empty,
// negative or unparseable value keeps the shipped default, so a typo cannot
// silently disable the protection.
func retainedAckTimeoutDefault() time.Duration {
	return parseRetainedAckTimeout(os.Getenv("URNETWORK_RETAINED_ACK_TIMEOUT"))
}

func parseRetainedAckTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return shippedRetainedAckTimeout
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return shippedRetainedAckTimeout
	}
	return time.Duration(seconds) * time.Second
}
