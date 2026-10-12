package connect

import (
	"os"
	"strings"
	"sync/atomic"
)

// The protection mode shared by the outage protections that ship on this line.
// URNETWORK_GOVERN selects it; unset, empty and unrecognised values all mean
// enforce, so a typo can never silently disable a protection. Only an explicit
// off or shadow turns it down.

type governMode int32

const (
	// governOff: the protections do nothing. An emergency override only.
	governOff governMode = iota
	// governShadow: observe and report only, never refuse or delay.
	governShadow
	// governEnforce: the protections act. The default.
	governEnforce
)

var governModeValue atomic.Int32

func init() { governModeValue.Store(int32(parseGovernMode(os.Getenv("URNETWORK_GOVERN")))) }

func parseGovernMode(raw string) governMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "0", "false", "no":
		return governOff
	case "shadow":
		return governShadow
	}
	return governEnforce
}

func currentGovernMode() governMode { return governMode(governModeValue.Load()) }

// setGovernModeForTest swaps the mode and returns a restore func.
func setGovernModeForTest(mode governMode) func() {
	prev := governModeValue.Swap(int32(mode))
	return func() { governModeValue.Store(prev) }
}
