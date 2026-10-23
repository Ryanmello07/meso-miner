//go:build linux

package urnettools

import (
	"strings"
	"testing"
)

func TestRewriteTimerCalendarSpreadsTheFleet(t *testing.T) {
	// The weekly timer the installer used to write, with no delay.
	legacy := "[Unit]\nDescription=Run URnetwork Update\n\n[Timer]\nOnCalendar=Sun *-*-* 00:00:00 UTC\nPersistent=true\n\n[Install]\nWantedBy=default.target\n"
	out, ok := rewriteTimerCalendar(legacy, "daily")
	if !ok {
		t.Fatal("OnCalendar not found")
	}
	if !strings.Contains(out, "OnCalendar=daily\nRandomizedDelaySec=6h\n") {
		t.Fatalf("rewritten timer = %q, want daily with a 6h randomized delay", out)
	}
	if strings.Contains(out, "Sun *-*-*") {
		t.Fatalf("old calendar survived: %q", out)
	}

	// Rewriting again replaces the delay instead of stacking a second one.
	out, _ = rewriteTimerCalendar(out, "hourly")
	if strings.Count(out, "RandomizedDelaySec=") != 1 || !strings.Contains(out, "OnCalendar=hourly\nRandomizedDelaySec=15min\n") {
		t.Fatalf("hourly rewrite = %q, want one 15min delay", out)
	}

	if _, ok := rewriteTimerCalendar("[Timer]\nPersistent=true\n", "daily"); ok {
		t.Fatal("a unit with no OnCalendar must be reported")
	}
}
