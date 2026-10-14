package connect

// Extracted verbatim from upstream transport_family.go at 20b29f79 for the
// udp-candidates leaf (normalizeIpFamily through udpAddrFamily, and
// resolveControlUDPAddrs through orderControlIPAddrs). Merge back when the
// L3 transport_family core lands; these symbols are otherwise inert.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// normalizeIpFamily is 4 or 6, else 0.
func normalizeIpFamily(ipFamily int) int {
	switch ipFamily {
	case 4, 6:
		return ipFamily
	default:
		return 0
	}
}

// pinnedDialNetwork narrows a family-agnostic network string to the pinned
// family. A network already of that family passes unchanged; a network of the
// OTHER family is an error, because a pinned transport must never dial the
// family it does not prove. Networks that carry no family (unix sockets) pass
// unchanged.
func pinnedDialNetwork(network string, ipFamily int) (string, error) {
	ipFamily = normalizeIpFamily(ipFamily)
	if ipFamily == 0 {
		return network, nil
	}
	switch network {
	case "tcp", "udp":
		return fmt.Sprintf("%s%d", network, ipFamily), nil
	case "tcp4", "udp4":
		if ipFamily == 4 {
			return network, nil
		}
	case "tcp6", "udp6":
		if ipFamily == 6 {
			return network, nil
		}
	default:
		return network, nil
	}
	return "", fmt.Errorf("network %s contradicts the ipv%d pin", network, ipFamily)
}

// pinnedFamilyPolicyConflict reports whether the developer's control family
// policy forbids the pinned family outright. Unlike a demotion, a Force is an
// explicit override and a pinned transport obeys it by idling (A6).
func pinnedFamilyPolicyConflict(ipFamily int) bool {
	switch ControlIpFamilyPolicy() {
	case IpFamilyForce4:
		return ipFamily == 6
	case IpFamilyForce6:
		return ipFamily == 4
	default:
		return false
	}
}

// ipLiteralFamily is 4 or 6 for a dial address that names an ip literal (with
// or without a port), else 0.
func ipLiteralFamily(addr string) int {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return 0
	}
	if ip.Unmap().Is4() {
		return 4
	}
	return 6
}

// pinnedFamilySupported reports whether this device currently has a path of
// the family that a pinned transport could use: a global address of the
// family off this product's own tunnel, and, when the process is bound to an
// egress interface that carries exactly one family, that family.
func pinnedFamilySupported(ipFamily int) bool {
	if !controlFamilyProbe(ipFamily) {
		return false
	}
	index4, index6 := EgressInterfaceIndex()
	if (index4 == 0) == (index6 == 0) {
		// unbound, or bound to an interface that carries both families:
		// the bind constrains nothing about family
		return true
	}
	if ipFamily == 4 {
		return index4 != 0
	}
	return index6 != 0
}

// udpWildcardForFamily is the family-specific wildcard a QUIC socket binds to,
// so a v6 dial never leaves an AF_INET socket and a v4 dial never depends on
// the platform's dual-stack socket behavior.
func udpWildcardForFamily(ipFamily int) (string, *net.UDPAddr) {
	if ipFamily == 6 {
		return "udp6", &net.UDPAddr{IP: net.IPv6zero, Port: 0}
	}
	return "udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0}
}

// udpAddrFamily is 4 or 6 for a resolved udp address.
func udpAddrFamily(udpAddr *net.UDPAddr) int {
	if udpAddr != nil && udpAddr.IP.To4() != nil {
		return 4
	}
	return 6
}

// NewDirectClientStrategy builds the client strategy a family-pinned platform
// transport dials with: the caller's settings without extenders, VLESS
// servers or a proxy, so every dial is direct and the observed remote family
// is the family the provider actually has (A4), and with the pin enforced
// below name resolution. Each pinned transport owns one, which also gives it
// connect pacing independent of the process-wide strategy (A5).
//
// The caller's settings are not mutated. An injected DialContextSettings
// (headless hosts) is kept as the inner dial so a source-identity seam still
// applies; a configured ProxySettings is dropped, because a pinned dial through
// a proxy would prove the proxy's family, not the provider's.

func (self *ClientStrategy) resolveControlUDPAddrs(ctx context.Context, address string, ipFamily int) ([]*net.UDPAddr, error) {
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, fmt.Errorf("resolve %s: empty host", address)
	}
	port, err := parseControlUDPPort(address, portStr)
	if err != nil {
		return nil, err
	}
	ipFamily = normalizeIpFamily(ipFamily)
	network, err := pinnedDialNetwork("udp", ipFamily)
	if err != nil {
		return nil, err
	}
	network, err = controlDialNetwork(network, address)
	if err != nil {
		return nil, err
	}
	if ipFamily != 0 && pinnedFamilyPolicyConflict(ipFamily) {
		return nil, fmt.Errorf("ipv%d is disabled by the control family policy", ipFamily)
	}

	// an ip literal needs no resolution; it must only agree with the pin
	if ip, ipErr := netip.ParseAddr(host); ipErr == nil {
		if literalFamily := ipLiteralFamily(host); ipFamily != 0 && literalFamily != ipFamily {
			return nil, fmt.Errorf("resolve %s: address is not ipv%d", address, ipFamily)
		}
		return []*net.UDPAddr{{IP: net.IP(ip.AsSlice()), Port: port, Zone: ip.Zone()}}, nil
	}

	var addrs []net.IPAddr
	if self != nil && self.internalDohResolver != nil && self.internalDohResolver.matches(host) {
		resolved, err := self.internalDohResolver.resolve(ctx, network, host)
		if err != nil {
			return nil, err
		}
		for _, addr := range resolved {
			addrs = append(addrs, net.IPAddr{IP: net.IP(addr.AsSlice()), Zone: addr.Zone()})
		}
	} else {
		// the same resolver selection as resolveEgressUDPAddr (egress_dial.go):
		// the platform's egress-bound resolver while bound, else the OS
		// resolver. A nil *net.Resolver IS the default resolver (net docs:
		// "A nil *Resolver is equivalent to the zero Resolver"), which keeps
		// this the multi-address twin of that audited no-egress path.
		var resolver *net.Resolver
		boundFamilyOnly := 0
		if self != nil && self.settings != nil && self.settings.ConnectSettings.Resolver != nil {
			resolver = self.settings.ConnectSettings.Resolver
		} else if platformResolver := egressResolver(); platformResolver != nil && egressBound() {
			resolver = platformResolver
			// the same single-family hard constraint egressBoundIPAddr
			// applies to a one-address pick
			index4, index6 := EgressInterfaceIndex()
			if (index4 == 0) != (index6 == 0) {
				boundFamilyOnly = 4
				if index6 != 0 {
					boundFamilyOnly = 6
				}
			}
		}
		resolved, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, addr := range resolved {
			if boundFamilyOnly != 0 && ipAddrFamily(addr) != boundFamilyOnly {
				continue
			}
			addrs = append(addrs, addr)
		}
	}

	// the network may have been narrowed by the pin, a Force or a demotion
	wantFamily := 0
	switch network {
	case "udp4":
		wantFamily = 4
	case "udp6":
		wantFamily = 6
	}
	udpAddrs := []*net.UDPAddr{}
	for _, addr := range orderControlIPAddrs(addrs) {
		if wantFamily != 0 && ipAddrFamily(addr) != wantFamily {
			continue
		}
		udpAddrs = append(udpAddrs, &net.UDPAddr{IP: addr.IP, Port: port, Zone: addr.Zone})
	}
	if len(udpAddrs) == 0 {
		if wantFamily != 0 {
			return nil, fmt.Errorf("resolve %s: no ipv%d address", address, wantFamily)
		}
		return nil, fmt.Errorf("resolve %s: no addresses", address)
	}
	return udpAddrs, nil
}

func ipAddrFamily(addr net.IPAddr) int {
	if addr.IP.To4() != nil {
		return 4
	}
	return 6
}

// orderControlIPAddrs dedupes and interleaves the families v6 first, so a
// dead family costs one stagger and never the whole attempt.
func orderControlIPAddrs(addrs []net.IPAddr) []net.IPAddr {
	ipv4 := []net.IPAddr{}
	ipv6 := []net.IPAddr{}
	seen := map[string]bool{}
	for _, addr := range addrs {
		key := addr.String()
		if addr.IP == nil || seen[key] {
			continue
		}
		seen[key] = true
		if addr.IP.To4() != nil {
			ipv4 = append(ipv4, addr)
		} else {
			ipv6 = append(ipv6, addr)
		}
	}
	ordered := make([]net.IPAddr, 0, len(ipv4)+len(ipv6))
	for i := 0; i < max(len(ipv4), len(ipv6)); i += 1 {
		if i < len(ipv6) {
			ordered = append(ordered, ipv6[i])
		}
		if i < len(ipv4) {
			ordered = append(ordered, ipv4[i])
		}
	}
	return ordered
}
