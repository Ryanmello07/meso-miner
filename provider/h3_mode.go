package main

import (
	"fmt"
	"strings"

	"github.com/urnetwork/connect"
)

// h3Mode is the `h3` control key: which transports an identity may use beyond H1.
//
//	off     H1 only. No QUIC and no dns carrier is ever dialed.
//	direct  the default. The direct (native) identity runs every upstream carrier
//	        as an automatic fallback: H1 is the primary, and H3, H3 over dns and
//	        the dns pump are started in that order only while nothing better is
//	        connected, and stand down once something better is. Idle they cost
//	        nothing. Proxied identities stay on H1.
//	on      direct, plus the opt-in auxiliary: H3 runs beside H1 on the direct
//	        identity instead of waiting behind it. This is what `h3 on` has always
//	        meant, so persisted values keep their behavior.
//
// The mode is read when an identity builds its platform transport (see
// platformTransportSettingsFor), so a change reaches the direct identity at its
// next (re)start.
type h3Mode int

const (
	h3ModeOff h3Mode = iota
	h3ModeDirect
	h3ModeOn
)

const (
	h3ModeOffName    = "off"
	h3ModeDirectName = "direct"
	h3ModeOnName     = "on"
)

func (m h3Mode) String() string {
	switch m {
	case h3ModeOff:
		return h3ModeOffName
	case h3ModeOn:
		return h3ModeOnName
	default:
		return h3ModeDirectName
	}
}

// parseH3Mode parses a control value. An empty value is the default.
func parseH3Mode(value string) (h3Mode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return h3ModeDirect, nil
	case h3ModeOffName:
		return h3ModeOff, nil
	case h3ModeDirectName:
		return h3ModeDirect, nil
	case h3ModeOnName:
		return h3ModeOn, nil
	}
	return h3ModeDirect, fmt.Errorf("h3: must be off, direct or on (got %q)", value)
}

// resolveH3Mode is the mode in force: the h3 control key when it is set and valid,
// otherwise the startup default. The startup default keeps the existing opt-ins:
// URNETWORK_H3 and the direct-only default (see defaultH3Setting) select the
// beside-H1 auxiliary, and everything else gets the automatic fallbacks.
func resolveH3Mode(state *controlState, resolution int32) h3Mode {
	if state != nil {
		if v, ok := state.get("h3"); ok {
			if mode, err := parseH3Mode(v); err == nil {
				return mode
			}
		}
	}
	if defaultH3Setting(state, resolution) {
		return h3ModeOn
	}
	return h3ModeDirect
}

// h3ModePreferences is the transport mode policy of one identity under a mode.
// Lower is better; a mode runs only while nothing better is connected, except
// that equal tiers run side by side.
func h3ModePreferences(mode h3Mode, direct bool) map[connect.TransportMode]int {
	if !direct || mode == h3ModeOff {
		return map[connect.TransportMode]int{connect.TransportModeH1: 1}
	}
	preferences := connect.DefaultTransportModePreferences()
	if mode == h3ModeOn {
		// the auxiliary: H3 shares H1's tier
		preferences[connect.TransportModeH3] = preferences[connect.TransportModeH1]
	}
	return preferences
}
