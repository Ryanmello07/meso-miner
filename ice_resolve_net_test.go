//go:build !js

// This file pins native ICE name resolution to the lifetime of the exact peer
// generation that initiated it.
//
// Ported to urnetwork-3.23-fix from the v2026 engine,
// github.com/full-bars/connect@v0.0.0-20261017075543-c8d8cdf536a8
// (ice_resolve_net_test.go). Two adaptations, both test scaffolding only:
//
//   - PIN's dual-stack helpers (forEachIpVersion, testUdpNetwork,
//     testTcpNetwork in test_dualstack_test.go, including its IPv6 loopback
//     precondition) do not exist in this generation. The lookups exercised
//     here are synthetic and resolve through a blocked Dial, so the two
//     families are driven by the local table below with no loopback
//     precondition.
//   - The peer-lifecycle test used PIN's webRtcPeerConnectionFactory,
//     WebRtcSettings.UseLoopbackOnlyIceInterfaces / EnableDatagramFastPath and
//     peerConn.teardown() / teardownDone / workers. This generation has no
//     factory and no separate teardown step: it retires a peer by canceling
//     its context and Run returns after Pion's local description unblocks, so
//     the test drives peer.Cancel() and waits on a local run-done channel.
package connect

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/transport/v4/stdnet"

	"github.com/urnetwork/connect/protocol"
)

// blockingIceResolver exposes entry into a real net.Resolver lookup and holds
// its wire dial until the lookup context is canceled.
type blockingIceResolver struct {
	resolver *net.Resolver
	entered  chan struct{}
	exited   chan struct{}
}

// newBlockingIceResolver creates an explicit cancellation barrier without a
// reachable DNS server or scheduler-dependent packet timeout.
func newBlockingIceResolver() *blockingIceResolver {
	entered := make(chan struct{})
	exited := make(chan struct{})
	var enteredOnce sync.Once
	var exitedOnce sync.Once
	resolver := &net.Resolver{
		PreferGo:     true,
		StrictErrors: true,
		Dial: func(ctx context.Context, network string, address string) (
			net.Conn,
			error,
		) {
			enteredOnce.Do(func() { close(entered) })
			<-ctx.Done()
			exitedOnce.Do(func() { close(exited) })
			return nil, context.Cause(ctx)
		},
	}
	return &blockingIceResolver{
		resolver: resolver,
		entered:  entered,
		exited:   exited,
	}
}

// waitForIceResolveTestSignal bounds a positive synchronization edge; elapsed
// time is not part of the behavior being asserted.
func waitForIceResolveTestSignal(
	t *testing.T,
	signal <-chan struct{},
	description string,
) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

// iceResolveTestFamilies are the transport network families the resolver is
// exercised on, standing in for PIN's testIpVersions plus testUdpNetwork /
// testTcpNetwork.
var iceResolveTestFamilies = []struct {
	name string
	udp  string
	tcp  string
}{
	{name: "v4", udp: "udp4", tcp: "tcp4"},
	{name: "v6", udp: "udp6", tcp: "tcp6"},
}

// iceResolveTestSignalSender discards the peer's signals: the lifecycle test
// only needs Pion's candidate gathering to start, not a negotiated peer.
type iceResolveTestSignalSender struct{}

func (iceResolveTestSignalSender) SendSignal(TransferPath, *protocol.Frame) {}

// restoreStunTallyWindowForTest leaves the process-global STUN aggregate as
// this test found it.
//
// The real peer connection below fails Pion's "Failed to resolve STUN host"
// line, which pionLeveledLogger counts into stunTally whether or not logging is
// enabled (see stun_tally.go). That one synthetic pulse starts the aggregate's
// emit window, so a later test that asserts on a before/after delta
// (stun_tally_test.go) would find its own pulse emitting and resetting the
// counters instead of incrementing them. No production behavior is involved:
// the counters are log-only, and the test's pulse describes a hostname that
// cannot resolve by construction.
func restoreStunTallyWindowForTest() {
	stunTally.mu.Lock()
	defer stunTally.mu.Unlock()
	stunTally.ok = 0
	stunTally.fail = 0
	stunTally.lastLogAt = time.Now()
	stunTally.nextLogAt = time.Time{}
}

// TestPeerConnectionResolveNetCancelsStunAndTurnLookups covers both resolver
// entry points used by Pion gathering. The old transport.Net calls had no
// cancellation input and remained parked in DNS after their peer was gone.
func TestPeerConnectionResolveNetCancelsStunAndTurnLookups(t *testing.T) {
	for _, family := range iceResolveTestFamilies {
		t.Run(family.name, func(t *testing.T) {
			base, err := stdnet.NewNet()
			if err != nil {
				t.Fatal(err)
			}
			cases := []struct {
				network string
				address string
				resolve func(*peerConnectionResolveNet) error
			}{
				{
					network: family.udp,
					address: "stun.blocked.invalid:3478",
					resolve: func(network *peerConnectionResolveNet) error {
						_, resolveErr := network.ResolveUDPAddr(family.udp, "stun.blocked.invalid:3478")
						return resolveErr
					},
				},
				{
					network: family.tcp,
					address: "turn.blocked.invalid:3478",
					resolve: func(network *peerConnectionResolveNet) error {
						_, resolveErr := network.ResolveTCPAddr(family.tcp, "turn.blocked.invalid:3478")
						return resolveErr
					},
				},
			}
			for _, testCase := range cases {
				blockedResolver := newBlockingIceResolver()
				network, cancel := newPeerConnectionResolveNet(
					context.Background(),
					base,
					blockedResolver.resolver,
					time.Hour,
				)
				if network.resolver == blockedResolver.resolver {
					t.Fatal("peer resolver aliases the shared resolver")
				}
				if !network.resolver.PreferGo || !network.resolver.StrictErrors {
					t.Fatal("peer resolver did not preserve resolver policy")
				}
				resolveResult := make(chan error, 1)
				go func() {
					resolveResult <- testCase.resolve(network)
				}()
				waitForIceResolveTestSignal(
					t,
					blockedResolver.entered,
					testCase.network+" resolver entry for "+testCase.address,
				)
				cancel()
				select {
				case resolveErr := <-resolveResult:
					if !errors.Is(resolveErr, context.Canceled) {
						t.Fatalf(
							"%s resolve error = %v, want context cancellation",
							testCase.network,
							resolveErr,
						)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s resolve ignored peer cancellation", testCase.network)
				}
				waitForIceResolveTestSignal(
					t,
					blockedResolver.exited,
					testCase.network+" resolver exit",
				)
			}
		})
	}
}

// TestWebRtcPeerTeardownCancelsBlockedStunResolution reproduces the production
// shutdown ordering: SetLocalDescription starts a STUN hostname lookup, then
// the peer retires while that lookup is blocked. Teardown must cancel DNS
// before PeerConnection.Close joins the ICE gatherer's WaitGroup.
func TestWebRtcPeerTeardownCancelsBlockedStunResolution(t *testing.T) {
	defer restoreStunTallyWindowForTest()

	blockedResolver := newBlockingIceResolver()
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.StunGatherTimeout = time.Hour
	settings.IceServerUrls = []string{"stun:stun.blocked.invalid:3478"}
	settings.iceResolverForTest = blockedResolver.resolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer, err := newPeerConn(
		ctx,
		peerConnKey{PeerId: NewId(), StreamId: NewId()},
		NewId(),
		true,
		iceResolveTestSignalSender{},
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	if peer.cancelIceResolve == nil {
		t.Fatal("peer connection has no cancelable ICE resolver")
	}

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		peer.Run()
	}()
	waitForIceResolveTestSignal(
		t,
		blockedResolver.entered,
		"Pion STUN resolver entry",
	)

	// Retirement is exactly what the manager does to a replaced connection:
	// cancel the generation's context. Pion resolves the STUN host in a
	// goroutine detached from SetLocalDescription, so a resolver that is not
	// retired with its generation leaves that lookup parked -- the exit and
	// run-done waits below time out.
	peer.Cancel()
	waitForIceResolveTestSignal(t, blockedResolver.exited, "STUN resolver exit")
	waitForIceResolveTestSignal(t, runDone, "peer run exit")
}
