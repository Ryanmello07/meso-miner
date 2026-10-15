package connect

// icmp echo acceptance gate. icmp echo is ON by default (the v33 decision),
// with an operator kill switch and a platform self-disable:
//
//   - `URNETWORK_ICMP=off` restores the historical drop (read once and
//     cached; a restart applies a change).
//   - a platform whose kernel refuses unprivileged icmp sockets
//     (`net.ipv4.ping_group_range` excluding the service gid) self-disables
//     the first time a flow cannot open its egress socket, with ONE logged
//     line. After that icmp simply reverts to unsupported - exactly as if
//     the feature were never enabled - and no other traffic is affected.
//
// The gate is consulted by the ParseIpPathWithPayload icmp tail, so a
// disabled icmp is rejected at the parse, never reaches the NAT, and cannot
// open an egress socket at all.

import (
	"os"
	"strings"
	"sync/atomic"
)

// icmpEchoSelfDisabled is set once by the platform refusal path.
var icmpEchoSelfDisabled atomic.Bool

// icmpEnvState caches the URNETWORK_ICMP read. The zero value must mean
// "unread" so that a fresh process reads the env on the first gate consult:
// the value zero is what a booted process actually has.
const (
	icmpEnvUnread = 0
	icmpEnvOn     = 1
	icmpEnvOff    = 2
)

var icmpEnvState atomic.Int32

func icmpEnvDisabled() bool {
	state := icmpEnvState.Load()
	if state == icmpEnvUnread {
		state = icmpEnvOn
		if strings.ToLower(strings.TrimSpace(os.Getenv("URNETWORK_ICMP"))) == "off" {
			state = icmpEnvOff
		}
		icmpEnvState.Store(state)
	}
	return state == icmpEnvOff
}

// icmpEchoParsingEnabled reports whether an icmp echo packet is accepted by
// the parse surfaces. nil cost when enabled (the default).
func icmpEchoParsingEnabled() bool {
	if icmpEchoSelfDisabled.Load() {
		return false
	}
	return !icmpEnvDisabled()
}

// icmpEgressRefused marks this process as unable to open unprivileged icmp
// sockets and logs exactly one line, naming the platform check that decides
// it. The affected flow has already failed soft; every later icmp packet is
// rejected at the parse.
func icmpEgressRefused(log Logger, err error) {
	if icmpEchoSelfDisabled.CompareAndSwap(false, true) {
		log.Errorf("[icmp] unprivileged icmp sockets are not available (check net.ipv4.ping_group_range against the service gid); icmp echo is disabled for this process: %s\n", err)
	}
}
