package connect

// control_family_test.go — the tests for the ported control-plane address
// family policy (control_family.go). Ported from connect
// v0.0.0-20261017075543-c8d8cdf536a8 (control_family_test.go), with the cases
// that cover the parts NOT ported here removed (the pinned-transport policy
// conflict, the per-dial evidence line, pickControlIPAddr) and the
// network-change case rewritten: this tree has no NetworkChanged signal, so the
// ledger's invalidation is exercised through controlFamilyClear directly.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func TestControlDialNetworkForce(t *testing.T) {
	tests := []struct {
		name    string
		policy  IpFamilyPolicy
		network string
		want    string
		wantErr bool
	}{
		{"auto leaves tcp alone", IpFamilyAuto, "tcp", "tcp", false},
		{"auto leaves udp alone", IpFamilyAuto, "udp", "udp", false},
		{"force4 narrows tcp", IpFamilyForce4, "tcp", "tcp4", false},
		{"force6 narrows tcp", IpFamilyForce6, "tcp", "tcp6", false},
		{"force4 narrows udp", IpFamilyForce4, "udp", "udp4", false},
		{"force6 narrows udp", IpFamilyForce6, "udp", "udp6", false},
		{"force4 passes matching explicit", IpFamilyForce4, "tcp4", "tcp4", false},
		{"force4 rejects conflicting explicit", IpFamilyForce4, "tcp6", "", true},
		{"force6 rejects conflicting explicit", IpFamilyForce6, "udp4", "", true},
		{"auto passes explicit through", IpFamilyAuto, "tcp6", "tcp6", false},
		{"unknown network is untouched", IpFamilyForce4, "unix", "unix", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			SetControlIpFamilyPolicy(test.policy)
			defer SetControlIpFamilyPolicy(IpFamilyAuto)
			got, err := controlDialNetwork(test.network, "api.example:443")
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %s under %d", test.network, test.policy)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

// A dial whose target is ALREADY AN IP LITERAL has no family choice left in
// it, so narrowing it can never change which family is dialed -- it can only
// turn a working dial into an instant "no suitable address found". Measured:
// `dial tcp6 1.1.1.1:443` fails immediately.
//
// This is the whole fallback layer, not a corner case. The extender dialers
// dial a configured ip and the remote plain-DNS resolver dials a configured
// resolver address (net_http_doh.go), both IP literals, both through
// ConnectSettings.DialContext. A demotion learned on the api path used to take
// every one of them down with it -- and because they fail at CONNECT, none of
// them recorded anything that could undo the demotion.
func TestControlDialNetworkNeverNarrowsAnIPLiteral(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()

	tests := []struct {
		name    string
		policy  IpFamilyPolicy
		demote  int
		network string
		addr    string
	}{
		{"force6 leaves an ipv4 extender literal alone", IpFamilyForce6, 0, "tcp", "192.0.2.7:443"},
		{"force4 leaves an ipv6 extender literal alone", IpFamilyForce4, 0, "tcp", "[2001:db8::7]:443"},
		{"a demotion of 6 leaves an ipv4 literal alone", IpFamilyAuto, 6, "tcp", "1.1.1.1:443"},
		{"a demotion of 4 leaves an ipv6 literal alone", IpFamilyAuto, 4, "tcp", "[2606:4700:4700::1111]:53"},
		{"a demotion leaves a bare literal alone", IpFamilyAuto, 6, "udp", "1.1.1.1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controlFamilyClear()
			defer controlFamilyClear()
			SetControlIpFamilyPolicy(test.policy)
			defer SetControlIpFamilyPolicy(IpFamilyAuto)
			if test.demote != 0 {
				SetControlIpFamilyPolicy(IpFamilyAuto)
				if !controlFamilyDemote(test.demote) {
					t.Fatal("expected the demotion to take")
				}
				SetControlIpFamilyPolicy(test.policy)
			}

			got, err := controlDialNetwork(test.network, test.addr)
			if err != nil {
				t.Fatalf("%v -- a literal dial must never be refused", err)
			}
			if got != test.network {
				t.Fatalf(
					"got %q for %s, want %q unchanged -- narrowing a literal can only break it",
					got, test.addr, test.network)
			}
		})
	}
}

func TestSetControlIpFamilyPolicyClampsUnknown(t *testing.T) {
	defer SetControlIpFamilyPolicy(IpFamilyAuto)
	SetControlIpFamilyPolicy(IpFamilyPolicy(99))
	if got := ControlIpFamilyPolicy(); got != IpFamilyAuto {
		t.Fatalf("got %d, want IpFamilyAuto", got)
	}
	SetControlIpFamilyPolicy(IpFamilyPolicy(-3))
	if got := ControlIpFamilyPolicy(); got != IpFamilyAuto {
		t.Fatalf("got %d, want IpFamilyAuto", got)
	}
}

// Only a post-connect TIMEOUT proves a path is blackholed. Everything else is
// a server or configuration fault, and demoting a family for one would steer
// every user off a healthy path.
func TestIsPathTimeoutIsNarrow(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"context deadline", context.DeadlineExceeded, true},
		{"os deadline", os.ErrDeadlineExceeded, true},
		{"wrapped deadline", fmt.Errorf("tls: %w", context.DeadlineExceeded), true},
		{"net timeout", &net.OpError{Err: &timeoutError{}}, true},
		{"certificate", &tls.CertificateVerificationError{}, false},
		{"connection refused", &net.OpError{Err: errors.New("connection refused")}, false},
		{"reset", errors.New("read: connection reset by peer"), false},
		{"alpn", errors.New("tls: no application protocol"), false},
		{"nil", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isPathTimeout(test.err); got != test.want {
				t.Fatalf("isPathTimeout(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

type timeoutError struct{}

func (self *timeoutError) Error() string   { return "i/o timeout" }
func (self *timeoutError) Timeout() bool   { return true }
func (self *timeoutError) Temporary() bool { return true }

// A demotion must never take the user offline. With no IPv4 on the device,
// demoting IPv6 is refused -- and with no IPv6, demoting IPv4 is refused.
//
// BOTH directions, and each with a probe that answers true only for the family
// being demoted. That is what pins `other`: with the guard's
// `other := 4; if family == 4 { other = 6 }` mutated so `other` is always 4,
// the demote(4) row probes 4, gets true, and the demotion is wrongly accepted.
// A single-direction test, or a row whose probe answers true for everything,
// cannot tell the mutant from the original.
func TestControlFamilyDemoteRefusedWhenOtherFamilyUnusable(t *testing.T) {
	tests := []struct {
		name     string
		usable   int
		demote   int
		wantNoop string
	}{
		{"no ipv4, demoting ipv6 refused", 6, 6, "tcp"},
		{"no ipv6, demoting ipv4 refused", 4, 4, "tcp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restore := swapControlFamilyProbe(func(family int) bool {
				return family == test.usable
			})
			defer restore()
			controlFamilyClear()
			defer controlFamilyClear()

			if controlFamilyDemote(test.demote) {
				t.Fatalf(
					"demoted ipv%d with no ipv%d available -- the only family "+
						"left is the one being demoted",
					test.demote, 10-test.demote)
			}
			network, err := controlDialNetwork("tcp", "api.example:443")
			if err != nil {
				t.Fatal(err)
			}
			if network != test.wantNoop {
				t.Fatalf("got %q, want %s -- a refused demotion must not narrow", network, test.wantNoop)
			}
		})
	}
}

// The IPv6-only guard is only as good as the probe behind it.
//
// The device this pins is the one the guard exists for: an IPv6-only handset
// (ios has no CLAT; NAT64/DNS64 is the App Store-required configuration) with
// this product's tunnel up. The tun carries RandomLocalIpv4's 10.a.b.h, which
// is IsGlobalUnicast, so a probe that accepts any global-unicast address
// answers "IPv4 is available" on a device that has no IPv4 whatsoever -- and
// the demotion of IPv6 that follows takes the control plane offline for five
// minutes, doubling to six hours.
func TestProbeFamilySupportIgnoresOurOwnTunnel(t *testing.T) {
	loopback := controlFamilyInterface{
		name:  "lo0",
		flags: net.FlagUp | net.FlagLoopback,
		addrs: []net.Addr{ipNet("127.0.0.1/8"), ipNet("::1/128")},
	}
	// the ios/darwin shape: PacketTunnelProvider installs RandomLocalIpv4
	utun := controlFamilyInterface{
		name:  "utun4",
		flags: net.FlagUp | net.FlagPointToPoint,
		addrs: []net.Addr{ipNet("10.7.3.42/32")},
	}
	// the android shape, including escape mode's 192.0.2.1
	androidTun := controlFamilyInterface{
		name:  "tun0",
		flags: net.FlagUp | net.FlagPointToPoint,
		addrs: []net.Addr{ipNet("10.0.0.19/24")},
	}
	androidEscapeTun := controlFamilyInterface{
		name:  "tun0",
		flags: net.FlagUp | net.FlagPointToPoint,
		addrs: []net.Addr{ipNet("192.0.2.1/24")},
	}
	cellularV6Only := controlFamilyInterface{
		name:  "pdp_ip0",
		flags: net.FlagUp,
		addrs: []net.Addr{ipNet("2600:1700:1234:5678::1/64"), ipNet("fe80::1/64")},
	}
	// an ordinary home-lan lease. RFC1918 by range, exactly like the tun's
	// address, and it MUST still count: rejecting private ranges outright
	// would answer "no IPv4" for most of the userbase.
	wifi := controlFamilyInterface{
		name:  "en0",
		flags: net.FlagUp,
		addrs: []net.Addr{ipNet("192.168.1.20/24")},
	}
	downEthernet := controlFamilyInterface{
		name:  "en1",
		flags: 0,
		addrs: []net.Addr{ipNet("192.168.5.7/24")},
	}

	tests := []struct {
		name   string
		ifaces []controlFamilyInterface
		want4  bool
		want6  bool
	}{
		{
			"ipv6-only iphone with our tunnel up",
			[]controlFamilyInterface{loopback, cellularV6Only, utun},
			false, true,
		},
		{
			"ipv6-only android with our tunnel up",
			[]controlFamilyInterface{loopback, cellularV6Only, androidTun},
			false, true,
		},
		{
			"ipv6-only android in escape mode",
			[]controlFamilyInterface{loopback, cellularV6Only, androidEscapeTun},
			false, true,
		},
		{
			"dual-stack wifi with our tunnel up",
			[]controlFamilyInterface{loopback, wifi, cellularV6Only, utun},
			true, true,
		},
		{
			"ipv4-only wifi",
			[]controlFamilyInterface{loopback, wifi},
			true, false,
		},
		{
			"loopback alone is not connectivity",
			[]controlFamilyInterface{loopback},
			false, false,
		},
		{
			"an interface that is down carries no path",
			[]controlFamilyInterface{loopback, downEthernet},
			false, false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restore := swapControlFamilyInterfaces(
				func() ([]controlFamilyInterface, error) { return test.ifaces, nil })
			defer restore()

			if got := probeFamilySupport(4); got != test.want4 {
				t.Fatalf("probeFamilySupport(4) = %v, want %v", got, test.want4)
			}
			if got := probeFamilySupport(6); got != test.want6 {
				t.Fatalf("probeFamilySupport(6) = %v, want %v", got, test.want6)
			}
		})
	}
}

// An unreadable interface table is NO EVIDENCE, and the only job this probe
// has is to refuse a dangerous demotion. Failing open answers "yes, the other
// family works" on a device nothing is known about, which is the one answer
// that can take the control plane down.
func TestProbeFamilySupportFailsClosedOnEnumerationError(t *testing.T) {
	restore := swapControlFamilyInterfaces(
		func() ([]controlFamilyInterface, error) {
			return nil, errors.New("operation not permitted")
		})
	defer restore()

	if probeFamilySupport(4) {
		t.Fatal("probeFamilySupport(4) said yes with no readable interface table")
	}
	if probeFamilySupport(6) {
		t.Fatal("probeFamilySupport(6) said yes with no readable interface table")
	}

	// and the guard it feeds must therefore refuse the demotion
	controlFamilyClear()
	defer controlFamilyClear()
	if controlFamilyDemote(6) {
		t.Fatal("demoted ipv6 on a device whose interfaces could not be read")
	}
}

// FamilySupported is the exported probe for the callers outside this package.
// It answers from the same interface table the probe does -- loopback alone is
// not connectivity -- and refuses anything that is not an ip version.
func TestFamilySupportedReadsTheInterfaceTable(t *testing.T) {
	loopback := controlFamilyInterface{
		name:  "lo0",
		flags: net.FlagUp | net.FlagLoopback,
		addrs: []net.Addr{ipNet("127.0.0.1/8"), ipNet("::1/128")},
	}
	// 198.18.0.0/15 (RFC 2544) and 3fff::/20 (RFC 9637) are global unicast and
	// are not among controlFamilyReservedPrefixes, which the documentation
	// ranges deliberately are -- so these read as connectivity while naming no
	// real network.
	dualStack := controlFamilyInterface{
		name:  "en0",
		flags: net.FlagUp,
		addrs: []net.Addr{ipNet("198.18.7.20/24"), ipNet("3fff:db8::1/64")},
	}

	restore := swapControlFamilyInterfaces(
		func() ([]controlFamilyInterface, error) {
			return []controlFamilyInterface{loopback}, nil
		})
	if FamilySupported(4) || FamilySupported(6) {
		restore()
		t.Fatal("a loopback-only host reported a usable family")
	}
	restore()

	restore = swapControlFamilyInterfaces(
		func() ([]controlFamilyInterface, error) {
			return []controlFamilyInterface{loopback, dualStack}, nil
		})
	supported4, supported6 := FamilySupported(4), FamilySupported(6)
	restore()
	if !supported4 || !supported6 {
		t.Fatalf("dual-stack host reported v4 = %v, v6 = %v", supported4, supported6)
	}

	for _, ipVersion := range []int{0, 5, -1, 46} {
		if FamilySupported(ipVersion) {
			t.Fatalf("FamilySupported(%d) = true, expected false", ipVersion)
		}
	}
}

func ipNet(cidr string) *net.IPNet {
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	network.IP = ip
	return network
}

// The POLICY accessor must never reflect a learned demotion. A ui row that
// read back "Force IPv4" because the heuristic fired could not be set back to
// Auto -- it would already appear not to be Auto. The demotion is visible
// through controlFamilyStatus instead, which is what the ui shows beside the
// policy.
func TestControlIpFamilyPolicyIgnoresDemotion(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	controlFamilyClear()
	defer controlFamilyClear()
	SetControlIpFamilyPolicy(IpFamilyAuto)
	defer SetControlIpFamilyPolicy(IpFamilyAuto)

	if !controlFamilyDemote(6) {
		t.Fatal("expected the demotion to take")
	}
	if got := ControlIpFamilyPolicy(); got != IpFamilyAuto {
		t.Fatalf("policy reads %d after a demotion, want IpFamilyAuto -- "+
			"a demotion must never be reported as a policy the user set", got)
	}
	if controlFamilyStatus() == "" {
		t.Fatal("expected a non-empty status while a demotion is live -- " +
			"the ui has no other way to tell auto-with-a-demotion from plain auto")
	}
	if ControlFamilyStatus() == "" {
		t.Fatal("the exported status must read the same live demotion")
	}
}

func TestControlFamilyDemoteNarrowsToTheOtherFamily(t *testing.T) {
	tests := []struct {
		name   string
		demote int
		want   string
	}{
		{"demote 6 narrows to tcp4", 6, "tcp4"},
		{"demote 4 narrows to tcp6", 4, "tcp6"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restore := swapControlFamilyProbe(func(int) bool { return true })
			defer restore()
			controlFamilyClear()
			defer controlFamilyClear()

			if !controlFamilyDemote(test.demote) {
				t.Fatal("expected the demotion to take")
			}
			network, err := controlDialNetwork("tcp", "api.example:443")
			if err != nil {
				t.Fatal(err)
			}
			if network != test.want {
				t.Fatalf("got %q, want %s", network, test.want)
			}
			if controlFamilyStatus() == "" {
				t.Fatal("expected a non-empty status while a demotion is live")
			}
		})
	}
}

// A force beats the ledger in both directions -- it is an explicit override,
// which is the whole point of the setting.
func TestForceBeatsDemotion(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	controlFamilyClear()
	defer controlFamilyClear()
	controlFamilyDemote(6)

	SetControlIpFamilyPolicy(IpFamilyForce6)
	defer SetControlIpFamilyPolicy(IpFamilyAuto)
	network, err := controlDialNetwork("tcp", "api.example:443")
	if err != nil {
		t.Fatal(err)
	}
	if network != "tcp6" {
		t.Fatalf("got %q, want tcp6 -- an explicit force outranks a demotion", network)
	}
}

func TestControlFamilyBackoffDoublesAndCaps(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	base := time.Unix(1750000000, 0)
	now := base
	restoreClock := swapControlFamilyClock(func() time.Time { return now })
	defer restoreClock()
	controlFamilyClear()
	defer controlFamilyClear()

	controlFamilyDemote(6)
	if got := controlFamilyDemotedUntil(6).Sub(base); got != controlFamilyDemotionBase {
		t.Fatalf("first demotion lasts %s, want %s", got, controlFamilyDemotionBase)
	}
	controlFamilyDemote(6)
	if got := controlFamilyDemotedUntil(6).Sub(base); got != 2*controlFamilyDemotionBase {
		t.Fatalf("second demotion lasts %s, want %s", got, 2*controlFamilyDemotionBase)
	}
	for i := 0; i < 20; i += 1 {
		controlFamilyDemote(6)
	}
	if got := controlFamilyDemotedUntil(6).Sub(base); got != controlFamilyDemotionMax {
		t.Fatalf("demotion lasts %s, want the %s cap", got, controlFamilyDemotionMax)
	}
}

func TestControlFamilyDemotionExpires(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	now := time.Unix(1750000000, 0)
	restoreClock := swapControlFamilyClock(func() time.Time { return now })
	defer restoreClock()
	controlFamilyClear()
	defer controlFamilyClear()

	controlFamilyDemote(6)
	now = now.Add(controlFamilyDemotionBase + time.Second)
	network, err := controlDialNetwork("tcp", "api.example:443")
	if err != nil {
		t.Fatal(err)
	}
	if network != "tcp" {
		t.Fatalf("got %q, want tcp once the demotion expired", network)
	}
	if controlFamilyStatus() != "" {
		t.Fatal("expected an empty status once the demotion expired")
	}
}

// A network change invalidates everything learned about the old path. This
// tree has no NetworkChanged signal, so the invalidation is exercised through
// controlFamilyClear -- the function a host that learns one must call.
func TestControlFamilyClearDropsTheLedger(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()
	controlFamilyClear()
	defer controlFamilyClear()

	controlFamilyDemote(6)
	controlFamilyClear()
	network, err := controlDialNetwork("tcp", "api.example:443")
	if err != nil {
		t.Fatal(err)
	}
	if network != "tcp" {
		t.Fatalf("got %q, want tcp after a network change", network)
	}
}

// A learned demotion must not be applied unchecked on the next path.
//
// This tree has no network-change invalidation wired (controlFamilyClear is
// called by the tests and by whatever host learns such a signal), so an entry
// can stand until its backoff expires -- including across a wifi/cellular
// switch that removed the family it would steer onto. The self-inflicted-
// outage guard is therefore re-evaluated on every READ, not only when the
// demotion was recorded: a demotion can never be applied on a path where
// recording it would have been refused.
func TestADemotionIsRevalidatedOnUseNotOnlyWhenRecorded(t *testing.T) {
	controlFamilyClear()
	defer controlFamilyClear()

	// learned on a dual-stack wifi with a broken HE ipv6 path
	restoreDualStack := swapControlFamilyProbe(func(int) bool { return true })
	if !controlFamilyDemote(6) {
		t.Fatal("expected the demotion to take on a dual-stack path")
	}
	if got, _ := controlDialNetwork("tcp", "api.example:443"); got != "tcp4" {
		t.Fatalf("precondition: controlDialNetwork = %q, want tcp4", got)
	}
	restoreDualStack()

	// the user joins an ipv6-only cellular / NAT64 network. Nothing in this
	// process can report the change, so the entry is still in the ledger.
	restore := swapControlFamilyProbe(func(family int) bool { return family == 6 })
	defer restore()

	if got := controlFamilyDemotedFamily(); got != 0 {
		t.Fatalf("ipv%d is still demoted on a path with no ipv4 at all", got)
	}
	network, err := controlDialNetwork("tcp", "api.example:443")
	if err != nil {
		t.Fatal(err)
	}
	if network != "tcp" {
		t.Fatalf("narrowed to %q on a path with no ipv4 -- every control dial "+
			"would fail with no route until the backoff expired", network)
	}
	if got := controlFamilyStatus(); got != "" {
		t.Fatalf("status %q describes a demotion the dialer is no longer acting on", got)
	}
}

// The use-time guard has its own copy of "which family is the OTHER one", and
// that copy needs the same discriminating table that pinned the copy in
// controlFamilyDemote (TestControlFamilyDemoteRefusedWhenOtherFamilyUnusable
// above). Same defect class, reintroduced in new code.
//
// With controlFamilyLiveDemotion's `other := 4; if live == 4 { other = 6 }`
// mutated so `other` is always 4, the ipv6 row cannot tell the difference --
// it probes 4 either way. Only the ipv4 row can: the mutant re-validates a
// demotion of IPv4 against probe(4), which is the family it just demoted, gets
// true, and keeps steering every control dial in the process onto IPv6 on a
// device that has no IPv6 at all. That is the self-inflicted outage the guard
// exists to prevent, arrived at through the guard itself.
//
// Both rows demote on a dual-stack path first, because recording a demotion is
// itself guarded: the ledger cannot be seeded into the state under test any
// other way.
func TestALiveDemotionIsRevalidatedAgainstTheOtherFamily(t *testing.T) {
	tests := []struct {
		name   string
		demote int
		// the only family with a path once the device has moved
		usable int
	}{
		{"ipv6 demoted, then the path loses ipv4", 6, 6},
		{"ipv4 demoted, then the path loses ipv6", 4, 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controlFamilyClear()
			defer controlFamilyClear()

			// learned where the guard permits it: both families present
			restoreDualStack := swapControlFamilyProbe(func(int) bool { return true })
			if !controlFamilyDemote(test.demote) {
				t.Fatalf("precondition: demoting ipv%d on a dual-stack path was refused", test.demote)
			}
			restoreDualStack()

			// the device moves to a path carrying ONLY the demoted family
			restore := swapControlFamilyProbe(func(family int) bool {
				return family == test.usable
			})
			defer restore()

			if got := controlFamilyDemotedFamily(); got != 0 {
				t.Fatalf(
					"ipv%d is still demoted on a path with no ipv%d -- the guard "+
						"was re-checked against the demoted family instead of the other one",
					got, 10-test.usable)
			}
			network, err := controlDialNetwork("tcp", "api.example:443")
			if err != nil {
				t.Fatal(err)
			}
			if network != "tcp" {
				t.Fatalf(
					"narrowed to %q on a path that has no such family -- every "+
						"control dial would fail with no route until the backoff expired",
					network)
			}
			if got := controlFamilyStatus(); got != "" {
				t.Fatalf("status %q describes a demotion the dialer is no longer acting on", got)
			}
		})
	}
}

func TestConnFamily(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		want int
	}{
		{"ipv4", &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}, 4},
		{"ipv4 in ipv6 form", &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.1"), Port: 443}, 4},
		{"ipv6", &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}, 6},
		{"nil", nil, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := connFamily(&stubConn{remote: test.addr}); got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
		})
	}
}

type stubConn struct {
	net.Conn
	remote net.Addr
}

func (self *stubConn) RemoteAddr() net.Addr { return self.remote }

func (self *stubConn) Close() error { return nil }

// The legacy DisableIpv4/DisableIpv6 mapping is public API in this tree and
// must keep working exactly as it did; the control-plane policy is applied on
// TOP of it, and both reach the dialer through the same ConnectSettings
// entry point.
func TestDialContextKeepsLegacyDisableAndAppliesTheControlFamilyPolicy(t *testing.T) {
	restore := swapControlFamilyProbe(func(int) bool { return true })
	defer restore()

	newSettings := func() (*ConnectSettings, *[]string) {
		settings := DefaultConnectSettings()
		dialed := []string{}
		settings.DialContextSettings = &DialContextSettings{
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				dialed = append(dialed, network)
				return &stubConn{remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 443}}, nil
			},
		}
		return settings, &dialed
	}

	t.Run("no policy is the identity dial", func(t *testing.T) {
		controlFamilyClear()
		defer controlFamilyClear()
		SetControlIpFamilyPolicy(IpFamilyAuto)
		defer SetControlIpFamilyPolicy(IpFamilyAuto)

		settings, dialed := newSettings()
		if _, err := settings.DialContext(context.Background(), "tcp", "api.example:443"); err != nil {
			t.Fatal(err)
		}
		if len(*dialed) != 1 || (*dialed)[0] != "tcp" {
			t.Fatalf("dialed %v, want [tcp]", *dialed)
		}
	})

	t.Run("a learned demotion narrows the control dial", func(t *testing.T) {
		controlFamilyClear()
		defer controlFamilyClear()
		SetControlIpFamilyPolicy(IpFamilyAuto)
		defer SetControlIpFamilyPolicy(IpFamilyAuto)
		if !controlFamilyDemote(6) {
			t.Fatal("expected the demotion to take")
		}

		settings, dialed := newSettings()
		if _, err := settings.DialContext(context.Background(), "tcp", "api.example:443"); err != nil {
			t.Fatal(err)
		}
		if len(*dialed) != 1 || (*dialed)[0] != "tcp4" {
			t.Fatalf("dialed %v, want [tcp4]", *dialed)
		}
	})

	t.Run("a demotion leaves a literal alone", func(t *testing.T) {
		controlFamilyClear()
		defer controlFamilyClear()
		SetControlIpFamilyPolicy(IpFamilyAuto)
		defer SetControlIpFamilyPolicy(IpFamilyAuto)
		if !controlFamilyDemote(6) {
			t.Fatal("expected the demotion to take")
		}

		settings, dialed := newSettings()
		if _, err := settings.DialContext(context.Background(), "tcp", "1.1.1.1:443"); err != nil {
			t.Fatal(err)
		}
		if len(*dialed) != 1 || (*dialed)[0] != "tcp" {
			t.Fatalf("dialed %v, want [tcp] -- an IP literal has no family to steer", *dialed)
		}
	})

	t.Run("the legacy disable pair is unchanged", func(t *testing.T) {
		controlFamilyClear()
		defer controlFamilyClear()
		SetControlIpFamilyPolicy(IpFamilyAuto)
		defer SetControlIpFamilyPolicy(IpFamilyAuto)

		settings, dialed := newSettings()
		settings.DisableIpv6 = true
		if _, err := settings.DialContext(context.Background(), "tcp", "api.example:443"); err != nil {
			t.Fatal(err)
		}
		if _, err := settings.DialContext(context.Background(), "tcp6", "api.example:443"); err == nil {
			t.Fatal("an explicitly disabled family must still be rejected")
		}
		if len(*dialed) != 1 || (*dialed)[0] != "tcp4" {
			t.Fatalf("dialed %v, want [tcp4]", *dialed)
		}
	})

	t.Run("the legacy disable pair and a contradicting force conflict", func(t *testing.T) {
		controlFamilyClear()
		defer controlFamilyClear()
		SetControlIpFamilyPolicy(IpFamilyForce6)
		defer SetControlIpFamilyPolicy(IpFamilyAuto)

		settings, _ := newSettings()
		settings.DisableIpv6 = true
		if _, err := settings.DialContext(context.Background(), "tcp", "api.example:443"); err == nil {
			t.Fatal("a force that contradicts the legacy disable pair must not silently dial")
		}
	})
}
