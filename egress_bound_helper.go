package connect

// Temporary home for egressBoundIPAddr, moved verbatim from upstream
// egress_dial.go at 20b29f79 so the ported control_family test compiles ahead
// of the L1 egress port. REMOVE this file when egress_dial.go lands, or the
// symbol will redeclare (recorded as a fork-only conflict in the handback).

import "net"

// egressBoundIPAddr applies an interface-index bind's family constraint on
// top of pickControlIPAddr's preference, when the bind actually expresses
// one. Only a SINGLE family's index being set is a family constraint. With
// both indexes set -- the normal shape while the Windows service holds the
// tunnel up, per EgressInterfaceIndex's doc comment -- the bind constrains
// nothing about family: a naive "first address whose family has a nonzero
// index" loop would match addrs[0] on its very first iteration regardless of
// family, silently discarding pick (and with it any Force4/Force6 or
// demotion). With neither index set there is no bind at all. So the override
// only runs when exactly one of index4/index6 is nonzero; otherwise pick
// stands unchanged.
func egressBoundIPAddr(addrs []net.IPAddr, index4 uint32, index6 uint32, pick net.IPAddr) net.IPAddr {
	if (index4 == 0) == (index6 == 0) {
		return pick
	}
	for _, a := range addrs {
		is4 := a.IP.To4() != nil
		if (is4 && index4 != 0) || (!is4 && index6 != 0) {
			return a
		}
	}
	return pick
}
