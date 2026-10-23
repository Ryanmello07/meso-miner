package urnettools

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"
)

// Auto-update policy.
//
// The platform retires old provider versions, so a node that updates weekly
// (the old default) can be refused before its next run. Auto-update is on by
// default everywhere and checks daily, at a per-box time so a fleet does not
// query the release API together; hourly is available for operators who want
// it. A check that finds the node current costs one release API call and
// touches nothing (cmdUpdate skips a provider already on the tag).
//
// The operator can still choose: `urnet-tools auto-update off` (or any other
// interval) is recorded in the provider's state dir, and that recorded choice
// is what every later install, update and migration honors. An install with
// no recorded choice is on the default.

// defaultAutoUpdateInterval is the schedule a provider gets unless its
// operator chose another one.
const defaultAutoUpdateInterval = "daily"

// autoUpdateChoiceFile records the operator's explicit auto-update choice in
// the provider's state dir. Absent means "never chose": the default applies
// and a legacy schedule may be migrated to it.
const autoUpdateChoiceFile = "auto_update"

// autoUpdateIntervals are the accepted schedule names, most frequent first.
var autoUpdateIntervals = []string{"hourly", "daily", "weekly", "monthly"}

// normalizeAutoUpdateInterval maps the CLI argument to a schedule name. "on"
// and "default" select the default interval.
func normalizeAutoUpdateInterval(arg string) (string, error) {
	switch arg {
	case "on", "default":
		return defaultAutoUpdateInterval, nil
	case "off":
		return "off", nil
	}
	for _, interval := range autoUpdateIntervals {
		if arg == interval {
			return arg, nil
		}
	}
	return "", fmt.Errorf("invalid interval %q: on|hourly|daily|weekly|monthly|off", arg)
}

// recordAutoUpdateChoice stores the operator's explicit choice. It is best
// effort for the caller: a provider without a resolvable state dir still gets
// its schedule changed, it just cannot remember the choice.
func recordAutoUpdateChoice(p Provider, interval string) error {
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	h, err := openProviderStateDir(p)
	if err != nil {
		return err
	}
	defer h.Close()
	return h.writeOwned(autoUpdateChoiceFile, []byte(interval+"\n"), 0o644)
}

// readAutoUpdateChoice returns the recorded choice, or "" when the operator
// never made one (or it cannot be read, which is treated the same way).
func readAutoUpdateChoice(p Provider) string {
	if p.StateDir == "" {
		return ""
	}
	h, err := openProviderStateDir(p)
	if err != nil {
		return ""
	}
	defer h.Close()
	data, err := h.readFile(autoUpdateChoiceFile, 64)
	if err != nil {
		return ""
	}
	choice, err := normalizeAutoUpdateInterval(strings.TrimSpace(string(data)))
	if err != nil {
		return ""
	}
	return choice
}

// effectiveAutoUpdateInterval is the schedule a provider should have: its
// recorded choice, else the default.
func effectiveAutoUpdateInterval(p Provider) string {
	if choice := readAutoUpdateChoice(p); choice != "" {
		return choice
	}
	return defaultAutoUpdateInterval
}

// autoUpdateScheduleExistsFn reports whether a provider currently has an
// auto-update schedule installed. Package var so tests can stub the platform
// probe.
var autoUpdateScheduleExistsFn = autoUpdateScheduleExists

// setAutoUpdateScheduleFn is the platform schedule writer, as a seam for
// tests.
var setAutoUpdateScheduleFn = setAutoUpdateSchedule

// migrateAutoUpdateSchedule moves a provider that never chose a schedule onto
// the default. It runs after an update, from the tool that update just
// installed, so the fleet reaches the default schedule without anyone
// re-running an installer.
//
// It only rewrites a schedule that exists: a provider with no schedule and no
// recorded choice was turned off by an older tool (which removed the
// schedule without recording anything), and that operator choice stands.
func migrateAutoUpdateSchedule(p Provider) (bool, error) {
	if readAutoUpdateChoice(p) != "" {
		return false, nil
	}
	if !autoUpdateScheduleExistsFn(p) {
		return false, nil
	}
	if err := setAutoUpdateScheduleFn(p, autoUpdateLabel(p), defaultAutoUpdateInterval); err != nil {
		return false, err
	}
	// Record the migration as the default so it runs once; the operator can
	// still pick any other interval, which overwrites this.
	_ = recordAutoUpdateChoice(p, defaultAutoUpdateInterval)
	return true, nil
}

// autoUpdateSpread picks a stable time of day (hour 0-23, minute 0-59) for
// a schedule so a fleet does not hit the release API at the same moment. It
// is derived from the host name and the schedule's label, so it is the same
// across rewrites on one box and different between boxes. Hourly schedules
// use only the minute.
func autoUpdateSpread(label string) (hour, minute int) {
	host, _ := os.Hostname()
	h := fnv.New32a()
	h.Write([]byte(host))
	h.Write([]byte{0})
	h.Write([]byte(label))
	v := h.Sum32() % (24 * 60)
	return int(v / 60), int(v % 60)
}
