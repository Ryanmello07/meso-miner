package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

func TestParseH3Mode(t *testing.T) {
	for value, want := range map[string]h3Mode{
		"": h3ModeDirect, "direct": h3ModeDirect, "DIRECT": h3ModeDirect,
		"off": h3ModeOff, "on": h3ModeOn, " On ": h3ModeOn,
	} {
		got, err := parseH3Mode(value)
		if err != nil || got != want {
			t.Fatalf("parseH3Mode(%q) = %v, %v; want %v", value, got, err, want)
		}
	}
	for _, bad := range []string{"maybe", "1", "true", "all", "16"} {
		if _, err := parseH3Mode(bad); err == nil {
			t.Fatalf("parseH3Mode(%q) must be rejected", bad)
		}
	}
}

// The default: the direct identity runs every upstream carrier as an automatic
// fallback behind H1, and no identity is the beside-H1 auxiliary.
func TestDirectIdentityDefaultsToAutomaticFallbacks(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")
	settings := platformTransportSettingsFor(nil, true)
	if settings.EnableH3 {
		t.Fatal("the beside-H1 auxiliary must stay opt-in")
	}
	want := connect.DefaultTransportModePreferences()
	if len(settings.ModePreferences) != len(want) {
		t.Fatalf("direct modes = %v, want upstream's %v", settings.ModePreferences, want)
	}
	for mode, priority := range want {
		if settings.ModePreferences[mode] != priority {
			t.Fatalf("mode %s priority %d, want %d", mode, settings.ModePreferences[mode], priority)
		}
	}
	if !(settings.ModePreferences[connect.TransportModeH1] < settings.ModePreferences[connect.TransportModeH3]) {
		t.Fatal("H1 must be the primary: H3 is a fallback unless the auxiliary is on")
	}
}

func TestH3ModePreferencesPerMode(t *testing.T) {
	h1Only := map[connect.TransportMode]int{connect.TransportModeH1: 1}
	for _, mode := range []h3Mode{h3ModeOff, h3ModeDirect, h3ModeOn} {
		got := h3ModePreferences(mode, false)
		if len(got) != 1 || got[connect.TransportModeH1] != 1 {
			t.Fatalf("mode %s: a non-direct identity must be H1 only, got %v (want %v)", mode, got, h1Only)
		}
	}
	if got := h3ModePreferences(h3ModeOff, true); len(got) != 1 || got[connect.TransportModeH1] != 1 {
		t.Fatalf("off: direct identity must be H1 only, got %v", got)
	}
	on := h3ModePreferences(h3ModeOn, true)
	if on[connect.TransportModeH3] != on[connect.TransportModeH1] {
		t.Fatalf("on: H3 must share H1's tier (auxiliary), got %v", on)
	}
	for _, mode := range []connect.TransportMode{connect.TransportModeH3Dns, connect.TransportModeH3DnsPump} {
		if on[mode] <= on[connect.TransportModeH1] {
			t.Fatalf("on: %s must stay a fallback, got %v", mode, on)
		}
	}
}

func TestResolveH3Mode(t *testing.T) {
	t.Setenv("URNETWORK_H3", "")
	if got := resolveH3Mode(nil, proxyResolutionNoSource); got != h3ModeDirect {
		t.Fatalf("unset on a pooled node = %v, want direct", got)
	}
	if got := resolveH3Mode(nil, proxyResolutionZeroValid); got != h3ModeOn {
		t.Fatalf("unset on a settled direct-only node = %v, want on (the existing default)", got)
	}
	t.Setenv("URNETWORK_H3", "on")
	if got := resolveH3Mode(nil, proxyResolutionNoSource); got != h3ModeOn {
		t.Fatalf("URNETWORK_H3=on = %v, want on", got)
	}
	state := newControlState()
	for _, c := range []struct {
		value string
		want  h3Mode
	}{{"off", h3ModeOff}, {"direct", h3ModeDirect}, {"on", h3ModeOn}} {
		if err := state.set("h3", c.value); err != nil {
			t.Fatal(err)
		}
		if got := resolveH3Mode(state, proxyResolutionZeroValid); got != c.want {
			t.Fatalf("explicit %q = %v, want %v", c.value, got, c.want)
		}
	}
}

// A mode change relaunches the direct identity; the startup replay does not.
func TestApplyH3ModeRelaunchesOnlyOnAChange(t *testing.T) {
	restoreH3Gate(t)
	h3AppliedMode.Store(-1)
	t.Cleanup(func() { h3AppliedMode.Store(-1) })
	if _, relaunch := applyH3Mode(h3ModeDirect); relaunch {
		t.Fatal("the first application is the startup replay and must not relaunch")
	}
	if _, relaunch := applyH3Mode(h3ModeDirect); relaunch {
		t.Fatal("an unchanged mode must not relaunch")
	}
	if previous, relaunch := applyH3Mode(h3ModeOn); !relaunch || previous != h3ModeDirect {
		t.Fatalf("direct -> on: previous=%v relaunch=%v", previous, relaunch)
	}
	if !connect.H3Enabled() {
		t.Fatal("on must open the auxiliary gate")
	}
	if _, relaunch := applyH3Mode(h3ModeOff); !relaunch || connect.H3Enabled() {
		t.Fatalf("on -> off: relaunch=%v gate=%v", relaunch, connect.H3Enabled())
	}
}
