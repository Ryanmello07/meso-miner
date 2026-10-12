package connect

import (
	"testing"
	"time"
)

func TestParseRetainedAckTimeout(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":        shippedRetainedAckTimeout,
		"  ":      shippedRetainedAckTimeout,
		"garbage": shippedRetainedAckTimeout,
		"-5":      shippedRetainedAckTimeout,
		"1.5":     shippedRetainedAckTimeout,
		"0":       0,
		"60":      60 * time.Second,
		" 420 ":   420 * time.Second,
	} {
		if got := parseRetainedAckTimeout(raw); got != want {
			t.Fatalf("parseRetainedAckTimeout(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestDefaultSendBufferSettingsCarryTheRetainedAckTimeout(t *testing.T) {
	t.Setenv("URNETWORK_RETAINED_ACK_TIMEOUT", "90")
	if got := DefaultSendBufferSettings().RetainedAckTimeout; got != 90*time.Second {
		t.Fatalf("settings carry %s, want the 90s override", got)
	}
	t.Setenv("URNETWORK_RETAINED_ACK_TIMEOUT", "")
	if got := DefaultSendBufferSettings().RetainedAckTimeout; got != shippedRetainedAckTimeout {
		t.Fatalf("settings carry %s, want the shipped %s", got, shippedRetainedAckTimeout)
	}
}
