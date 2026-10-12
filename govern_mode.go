package connect

import (
	"os"
	"strings"
	"sync/atomic"
)

// The protection mode shared by the outage protections that ship on this line.
// URNETWORK_GOVERN selects it. The shipped default is shadow: unset, empty and
// unrecognised values observe and log what enforce would do without acting, so a
// protection can be watched before it is relied on. Only an explicit enforce
// acts, and an explicit off disables it.

type governMode int32

const (
	// governOff: the protections do nothing. An emergency override only.
	governOff governMode = iota
	// governShadow: observe and report only, never refuse or delay.
	governShadow
	// governEnforce: the protections act. An explicit opt-in on this line.
	governEnforce
)

var governModeValue atomic.Int32

func init() { governModeValue.Store(int32(parseGovernMode(os.Getenv("URNETWORK_GOVERN")))) }

func parseGovernMode(raw string) governMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "enforce", "on", "1", "true", "yes":
		return governEnforce
	case "off", "0", "false", "no":
		return governOff
	}
	return governShadow
}

func currentGovernMode() governMode { return governMode(governModeValue.Load()) }

// setGovernModeForTest swaps the mode and returns a restore func.
func setGovernModeForTest(mode governMode) func() {
	prev := governModeValue.Swap(int32(mode))
	return func() { governModeValue.Store(prev) }
}

// emitGovernLine writes one operator-facing [govern] line to the default log.
func emitGovernLine(line string) { DefaultLogger().Infof("%s\n", line) }
