//go:build linux

package urnettools

import (
	"os"
	"path/filepath"
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

// A timer the old installer disabled (`auto-update off` left the file) is not
// a schedule to migrate; only an enabled one is.
func TestTimerEnabledNeedsWantsLink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urnetwork-update.timer")
	if timerEnabled(path) {
		t.Fatal("a missing timer counted as enabled")
	}
	if err := os.WriteFile(path, []byte("[Timer]\nOnCalendar=weekly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if timerEnabled(path) {
		t.Fatal("a disabled timer (file, no wants link) counted as enabled")
	}
	wants := filepath.Join(dir, "default.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(wants, "urnetwork-update.timer")); err != nil {
		t.Fatal(err)
	}
	if !timerEnabled(path) {
		t.Fatal("an enabled timer was not seen")
	}
}
