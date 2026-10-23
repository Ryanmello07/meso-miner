package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeAutoUpdateInterval(t *testing.T) {
	for arg, want := range map[string]string{
		"on":      defaultAutoUpdateInterval,
		"default": defaultAutoUpdateInterval,
		"off":     "off",
		"hourly":  "hourly",
		"daily":   "daily",
		"weekly":  "weekly",
		"monthly": "monthly",
	} {
		got, err := normalizeAutoUpdateInterval(arg)
		if err != nil || got != want {
			t.Fatalf("normalize(%q) = %q, %v; want %q", arg, got, err, want)
		}
	}
	for _, bad := range []string{"", "15min", "yearly", "ON"} {
		if _, err := normalizeAutoUpdateInterval(bad); err == nil {
			t.Fatalf("normalize(%q) accepted, want an error", bad)
		}
	}
	if defaultAutoUpdateInterval != "daily" {
		t.Fatalf("default interval = %q, want daily", defaultAutoUpdateInterval)
	}
}

// stubAutoUpdatePlatform replaces the platform probe and writer for one test
// and returns a pointer to the intervals the writer was asked for.
func stubAutoUpdatePlatform(t *testing.T, exists bool, setErr error) *[]string {
	t.Helper()
	var calls []string
	oldExists, oldSet := autoUpdateScheduleExistsFn, setAutoUpdateScheduleFn
	autoUpdateScheduleExistsFn = func(Provider) bool { return exists }
	setAutoUpdateScheduleFn = func(_ Provider, _ string, interval string) error {
		calls = append(calls, interval)
		return setErr
	}
	t.Cleanup(func() {
		autoUpdateScheduleExistsFn, setAutoUpdateScheduleFn = oldExists, oldSet
	})
	return &calls
}

func TestMigrateAutoUpdateScheduleMovesLegacyScheduleToDefault(t *testing.T) {
	p := Provider{StateDir: t.TempDir(), Unit: "urnetwork.service"}
	calls := stubAutoUpdatePlatform(t, true, nil)

	migrated, err := migrateAutoUpdateSchedule(p)
	if err != nil || !migrated {
		t.Fatalf("migrate = %v, %v; want migrated", migrated, err)
	}
	if len(*calls) != 1 || (*calls)[0] != defaultAutoUpdateInterval {
		t.Fatalf("schedule writes = %v, want [%s]", *calls, defaultAutoUpdateInterval)
	}
	if got := readAutoUpdateChoice(p); got != defaultAutoUpdateInterval {
		t.Fatalf("recorded choice = %q, want %q", got, defaultAutoUpdateInterval)
	}

	// The recorded default makes the migration one-shot.
	migrated, err = migrateAutoUpdateSchedule(p)
	if err != nil || migrated || len(*calls) != 1 {
		t.Fatalf("second migrate = %v, %v, writes %v; want a no-op", migrated, err, *calls)
	}
}

func TestMigrateAutoUpdateScheduleHonorsRecordedChoice(t *testing.T) {
	for _, choice := range []string{"off", "weekly", "hourly"} {
		p := Provider{StateDir: t.TempDir(), Unit: "urnetwork.service"}
		if err := recordAutoUpdateChoice(p, choice); err != nil {
			t.Fatal(err)
		}
		calls := stubAutoUpdatePlatform(t, true, nil)
		migrated, err := migrateAutoUpdateSchedule(p)
		if err != nil || migrated || len(*calls) != 0 {
			t.Fatalf("choice %s: migrate = %v, %v, writes %v; want untouched", choice, migrated, err, *calls)
		}
	}
}

func TestMigrateAutoUpdateScheduleLeavesMissingScheduleOff(t *testing.T) {
	// An older tool's `auto-update off` removed the schedule without recording
	// anything; that choice must stand.
	p := Provider{StateDir: t.TempDir(), Unit: "urnetwork.service"}
	calls := stubAutoUpdatePlatform(t, false, nil)
	migrated, err := migrateAutoUpdateSchedule(p)
	if err != nil || migrated || len(*calls) != 0 {
		t.Fatalf("migrate = %v, %v, writes %v; want untouched", migrated, err, *calls)
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, autoUpdateChoiceFile)); !os.IsNotExist(err) {
		t.Fatalf("choice file written for an untouched provider (err=%v)", err)
	}
}

func TestMigrateAutoUpdateScheduleWriteFailureIsRetried(t *testing.T) {
	p := Provider{StateDir: t.TempDir(), Unit: "urnetwork.service"}
	stubAutoUpdatePlatform(t, true, errors.New("systemctl failed"))
	if _, err := migrateAutoUpdateSchedule(p); err == nil {
		t.Fatal("migrate hid the schedule write failure")
	}
	// Nothing recorded, so the next update tries again.
	if got := readAutoUpdateChoice(p); got != "" {
		t.Fatalf("recorded choice %q after a failed migration, want none", got)
	}
}

func TestAutoUpdateSpreadIsStableAndInRange(t *testing.T) {
	h1, m1 := autoUpdateSpread("urnetwork-update")
	h2, m2 := autoUpdateSpread("urnetwork-update")
	if h1 != h2 || m1 != m2 {
		t.Fatalf("spread not stable: %d:%d vs %d:%d", h1, m1, h2, m2)
	}
	seen := map[int]bool{}
	for _, label := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		h, m := autoUpdateSpread(label)
		if h < 0 || h > 23 || m < 0 || m > 59 {
			t.Fatalf("spread(%q) = %d:%d out of range", label, h, m)
		}
		seen[h*60+m] = true
	}
	if len(seen) < 4 {
		t.Fatalf("spread collapses labels onto %d slots, want them spread", len(seen))
	}
}

func TestEffectiveAutoUpdateInterval(t *testing.T) {
	p := Provider{StateDir: t.TempDir()}
	if got := effectiveAutoUpdateInterval(p); got != defaultAutoUpdateInterval {
		t.Fatalf("no choice: %q, want the default", got)
	}
	if err := recordAutoUpdateChoice(p, "off"); err != nil {
		t.Fatal(err)
	}
	if got := effectiveAutoUpdateInterval(p); got != "off" {
		t.Fatalf("recorded off: %q, want off (an install must not turn it back on)", got)
	}
	if got := effectiveAutoUpdateInterval(Provider{}); got != defaultAutoUpdateInterval {
		t.Fatalf("no state dir: %q, want the default", got)
	}
}
