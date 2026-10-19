package connect

// net_http_doh_dualstack_test.go — the DoH resolver's address-family policy
// and its round-robin server choice.
//
// Ported from connect v0.0.0-20261017075543-c8d8cdf536a8
// (net_http_doh_dualstack_test.go), adapted to this tree's DohServer type (a
// url plus the query format it speaks, not a bare url string).
//
// The test this replaces, TestDnsResolverAddrsOmitIpv6WithoutHostIpv6, pinned
// the older `if !hostHasGlobalIpv6() { ipv6 = nil }` filter: a v6 resolver was
// never offered on a host whose one-shot v6 probe failed. That filter is gone
// (see dnsResolverAddrs): the family a lookup needs is decided per query by the
// record type (dnsLookupNetwork), and a family the path lacks costs one
// attempt out of an operator-interleaved list rather than suppressing the other
// family for the life of the process.
//
// The PIN's launch-wave grouping (dohServerSiblings/dohLaunchWaves) is NOT
// ported: this tree's fan-out is the stagger in dohQueryWithClientResult, which
// has no per-operator wave concept. The operator pairing it consumes is ported
// (familySiblings), so a later port can build on it.

import (
	"net"
	"net/url"
	"strings"
	"testing"
)

func withHostIpv6(t *testing.T, has bool) {
	old := hostHasGlobalIpv6
	hostHasGlobalIpv6 = func() bool { return has }
	t.Cleanup(func() { hostHasGlobalIpv6 = old })
}

func TestDohIpVersionFollowsHostIpv6(t *testing.T) {
	withHostIpv6(t, true)
	if v := DefaultDohSettings().IpVersion; v != 0 {
		t.Fatalf("dual-stack host: IpVersion %d, want 0", v)
	}
	withHostIpv6(t, false)
	if v := DefaultDohSettings().IpVersion; v != 4 {
		t.Fatalf("IPv4-only host must keep IpVersion 4, got %d", v)
	}
}

func TestDefaultIpv6DohServersAreWellFormed(t *testing.T) {
	rs := DefaultDnsResolverSettings()
	if len(rs.RemoteDohServersIpv6) != len(rs.RemoteDohServersIpv4) {
		t.Fatalf("each operator needs a v6 endpoint: %d v4, %d v6", len(rs.RemoteDohServersIpv4), len(rs.RemoteDohServersIpv6))
	}
	for _, s := range rs.RemoteDohServersIpv6 {
		u, err := url.Parse(s.Url)
		if err != nil || u.Scheme != "https" {
			t.Fatalf("bad v6 doh url %q: %v", s.Url, err)
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || ip.To4() != nil {
			t.Fatalf("v6 doh server %q must be an IPv6 literal", s.Url)
		}
		if s.Format != DohFormatJson && s.Format != DohFormatWire {
			t.Fatalf("v6 doh server %q has no format", s.Url)
		}
	}
	for _, a := range rs.LocalDnsIpv6 {
		if ip := net.ParseIP(a); ip == nil || ip.To4() != nil {
			t.Fatalf("local v6 dns %q is not IPv6", a)
		}
	}
}

func TestDualStackSettingsConsultBothFamilies(t *testing.T) {
	withHostIpv6(t, true)
	st := DefaultDohSettings()
	servers := remoteDohServers(st, st.IpVersion)
	var v4, v6 int
	for _, s := range servers {
		if strings.Contains(s.Url, "[") {
			v6++
		} else {
			v4++
		}
	}
	if v4 == 0 || v6 == 0 {
		t.Fatalf("dual-stack must use both families, got v4=%d v6=%d", v4, v6)
	}
	addrs := dnsResolverAddrs(st, false, "")
	var has6 bool
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() == nil {
			has6 = true
		}
	}
	if !has6 {
		t.Fatalf("local dns addrs missing IPv6: %v", addrs)
	}
	// a v4-only host must not see any v6 server
	withHostIpv6(t, false)
	st = DefaultDohSettings()
	for _, s := range remoteDohServers(st, st.IpVersion) {
		if strings.Contains(s.Url, "[") {
			t.Fatalf("IPv4-only host would consult a v6 doh server: %s", s.Url)
		}
	}
}

func TestDetectGlobalIpv6DoesNotPanicOrBlock(t *testing.T) {
	_ = detectGlobalIpv6() // result depends on the host; it must return promptly
}

// The host probe no longer filters resolver families. It answered a question
// about the host from a one-shot dial that does not re-run across a network
// switch, and it could only ever suppress v6, so a host that gained v6 stayed on
// v4 resolvers for the life of the process. Both families are offered now, and
// the family a lookup needs is decided per query by record type.
func TestDnsResolverAddrsKeepBothFamiliesOnAnIpv4OnlyHost(t *testing.T) {
	family := func(addrs []string, v6 bool) bool {
		for _, addr := range addrs {
			if ip := net.ParseIP(addr); ip != nil && (ip.To4() == nil) == v6 {
				return true
			}
		}
		return false
	}

	withHostIpv6(t, false)
	settings := DefaultDohSettings()
	settings.DnsResolverSettings = DefaultDnsResolverSettings()
	settings.IpVersion = 0 // the dual-stack default, which the probe no longer narrows
	// the plain-dns remote lists are not part of the shipped defaults; give them
	// one known operator pair so both surfaces carry both families
	settings.DnsResolverSettings.RemoteDnsIpv4 = []string{"9.9.9.9"}
	settings.DnsResolverSettings.RemoteDnsIpv6 = []string{"2620:fe::fe"}

	for _, remote := range []bool{false, true} {
		for _, network := range []string{"", "udp", "tcp"} {
			if !family(dnsResolverAddrs(settings, remote, network), true) {
				t.Fatalf("remote=%v network=%q dropped the v6 resolver on an IPv4-only host", remote, network)
			}
			if !family(dnsResolverAddrs(settings, remote, network), false) {
				t.Fatalf("remote=%v network=%q dropped the v4 resolver", remote, network)
			}
		}
		for _, network := range []string{"udp6", "tcp6", "ip6"} {
			if !family(dnsResolverAddrs(settings, remote, network), true) {
				t.Fatalf("remote=%v network=%q dropped the configured v6 resolver", remote, network)
			}
		}
		for _, network := range []string{"udp4", "tcp4", "ip4"} {
			if family(dnsResolverAddrs(settings, remote, network), true) {
				t.Fatalf("remote=%v network=%q carried a v6 resolver into a v4-only lookup", remote, network)
			}
		}
	}

	// IpVersion 6 is the same explicit request, made in configuration
	settings.IpVersion = 6
	if !family(dnsResolverAddrs(settings, false, "udp"), true) {
		t.Fatal("an explicitly configured IpVersion 6 must be honoured on an IPv4-only host")
	}

	withHostIpv6(t, true)
	settings = DefaultDohSettings() // IpVersion 0 on a dual-stack host
	if !family(dnsResolverAddrs(settings, false, ""), true) {
		t.Fatal("a dual-stack host must be offered IPv6 resolvers")
	}
}
