package connect

import (
	"testing"
	"time"
)

func resetFramedUpgradeCache(t *testing.T) {
	t.Helper()
	framedUpgradeCache.Lock()
	clear(framedUpgradeCache.until)
	framedUpgradeCache.Unlock()
	t.Cleanup(func() {
		framedUpgradeCache.Lock()
		clear(framedUpgradeCache.until)
		framedUpgradeCache.Unlock()
	})
}

var rejectedUpgrade = &HTTPUpgradeError{Reason: "rejected", StatusCode: 426}

const upgradeTestAddress = "wss://platform.example.invalid/"

// A rejection seen through one route (a proxy or middlebox that does not pass the
// framed upgrade) must not turn H1+ off on any other route.
func TestFramedUpgradeRejectionIsScopedToItsRoute(t *testing.T) {
	if !H1PlusAvailable() {
		t.Skip("h1+ is switched off in this build")
	}
	resetFramedUpgradeCache(t)
	proxyA, proxyB := "route-a", "route-b"

	recordFramedUpgradeFailureFor(proxyA, upgradeTestAddress, H1FramerProtocol, rejectedUpgrade)

	if framedUpgradePermittedFor(proxyA, upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("the route that saw the rejection must skip the upgrade")
	}
	if !framedUpgradePermittedFor(proxyB, upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("another route must still try the upgrade")
	}
	if !framedUpgradePermittedFor("", upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("the shared (direct) route must still try the upgrade")
	}
}

// The shared route keeps the behavior of a single path: a rejection there blocks
// that route only, not the scoped ones.
func TestFramedUpgradeSharedRouteIsItsOwnScope(t *testing.T) {
	if !H1PlusAvailable() {
		t.Skip("h1+ is switched off in this build")
	}
	resetFramedUpgradeCache(t)

	recordFramedUpgradeFailureFor("", upgradeTestAddress, H1FramerProtocol, rejectedUpgrade)

	if framedUpgradePermittedFor("", upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("the shared route must skip the upgrade after its own rejection")
	}
	if !framedUpgradePermittedFor("route-a", upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("a scoped route must not inherit the shared route's rejection")
	}
	// the exported wrappers are the shared route
	if FramedUpgradePermitted(upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("FramedUpgradePermitted is the shared route")
	}
}

// Only unsupported-protocol evidence is cached, per route, and it expires.
func TestFramedUpgradeOtherFailuresAreNotCached(t *testing.T) {
	if !H1PlusAvailable() {
		t.Skip("h1+ is switched off in this build")
	}
	resetFramedUpgradeCache(t)
	for _, status := range []int{401, 403, 500, 503} {
		recordFramedUpgradeFailureFor("route-a", upgradeTestAddress, H1FramerProtocol, &HTTPUpgradeError{Reason: "rejected", StatusCode: status})
	}
	if !framedUpgradePermittedFor("route-a", upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("a failure that is not evidence of an unsupported protocol must not be cached")
	}
	// expiry
	recordFramedUpgradeFailureFor("route-a", upgradeTestAddress, H1FramerProtocol, rejectedUpgrade)
	key := framedUpgradeCacheKey("route-a", upgradeTestAddress, H1FramerProtocol)
	framedUpgradeCache.Lock()
	framedUpgradeCache.until[key] = time.Now().Add(-time.Second)
	framedUpgradeCache.Unlock()
	if !framedUpgradePermittedFor("route-a", upgradeTestAddress, H1FramerProtocol) {
		t.Fatal("an expired rejection must permit the upgrade again")
	}
}

func TestUpgradeRouteScopeDiffersPerDialer(t *testing.T) {
	a, b := &clientDialer{}, &clientDialer{}
	if upgradeRouteScope(a) == upgradeRouteScope(b) {
		t.Fatal("two dialers must not share a scope")
	}
	if upgradeRouteScope(a) != upgradeRouteScope(a) {
		t.Fatal("a dialer's scope must be stable")
	}
}
