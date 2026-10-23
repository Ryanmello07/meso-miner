package connect

import (
	"testing"
	"time"
)

// The provider guard shares one flow table across all authenticated sources.
// When the table is full, eviction must spare established secure flows:
// evicting one would reset a validated TLS session, because the recreated
// state would re-inspect opaque TLS data as a fresh negotiation and reject
// it. The eviction must prefer rejected or still-negotiating flows.
func TestSmtpGuardEvictionSparesSecureFlows(t *testing.T) {
	var guard smtpEgressGuard

	// Fill the table to the cap with established secure flows.
	for index := 0; index < smtpMaxFlowCount-1; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(20000+index, smtpImplicitTlsPort, uint32(index+1)),
			smtpTestClientHello,
		))
	}
	// One still-negotiating flow: SYN only, no ClientHello yet.
	negotiating := smtpTestSyn(30000, smtpImplicitTlsPort, 1)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(negotiating, nil))

	// The table is full; a new flow triggers eviction. The deterministic
	// non-secure pass must remove the negotiating flow and spare the secure
	// ones.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(31000, smtpImplicitTlsPort, 1), nil,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("flow table size = %d, want %d", len(guard.flows), smtpMaxFlowCount)
	}
	negotiatingKey, ok := smtpFlowKeyForOwnerPath(Id{}, negotiating)
	if !ok {
		t.Fatal("could not build the negotiating flow key")
	}
	if _, exists := guard.flows[negotiatingKey]; exists {
		t.Fatal("eviction did not remove the negotiating flow")
	}
	firstSecure, ok := smtpFlowKeyForOwnerPath(Id{}, smtpTestPath(20000, smtpImplicitTlsPort, 1))
	if !ok {
		t.Fatal("could not build the first secure flow key")
	}
	if _, exists := guard.flows[firstSecure]; !exists {
		t.Fatal("eviction removed an established secure flow")
	}
}

// A secure flow that has been latched rejected is expendable: it is already
// dead, so evicting it cannot reset a valid session. Eviction must prefer it
// over a still-valid secure flow.
func TestSmtpGuardEvictionPrefersRejectedSecureFlow(t *testing.T) {
	var guard smtpEgressGuard

	// A secure flow that is still valid.
	valid := smtpTestPath(20000, smtpImplicitTlsPort, 1)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(valid, smtpTestClientHello))

	// A secure flow latched rejected by a conflicting retransmission of the
	// validated ClientHello prefix.
	rejected := smtpTestPath(20001, smtpImplicitTlsPort, 1)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(rejected, smtpTestClientHello))
	conflict := append([]byte(nil), smtpTestClientHello...)
	conflict[5] = 0x02
	requireSmtpVerdict(t, smtpEgressReject, guard.inspect(rejected, conflict))

	// Fill the rest of the table with valid secure flows.
	for index := 2; index < smtpMaxFlowCount; index++ {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
			smtpTestPath(20000+index, smtpImplicitTlsPort, uint32(index+1)),
			smtpTestClientHello,
		))
	}

	// The table is full; a new flow triggers eviction. The rejected secure
	// flow must be evicted and the valid secure flow must survive.
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspect(
		smtpTestSyn(31000, smtpImplicitTlsPort, 1), nil,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("flow table size = %d, want %d", len(guard.flows), smtpMaxFlowCount)
	}
	rejectedKey, ok := smtpFlowKeyForOwnerPath(Id{}, rejected)
	if !ok {
		t.Fatal("could not build the rejected flow key")
	}
	if _, exists := guard.flows[rejectedKey]; exists {
		t.Fatal("eviction did not remove the rejected secure flow")
	}
	validKey, ok := smtpFlowKeyForOwnerPath(Id{}, valid)
	if !ok {
		t.Fatal("could not build the valid flow key")
	}
	if _, exists := guard.flows[validKey]; !exists {
		t.Fatal("eviction removed a valid secure flow")
	}
}

// Eviction is scoped to the INSERTING owner. Clients that together fill the
// shared table with secure flows must not cause another client's established
// secure flow to be evicted (which would reset a live TLS session). A new
// owner with no flow of its own to give up is refused, and nothing is evicted.
func TestSmtpGuardEvictionScopedToOwner(t *testing.T) {
	var guard smtpEgressGuard
	owners := smtpMaxFlowCount / smtpMaxOwnerFlowCount
	ownerA := Id{1}
	ownerB := Id{0xff}

	// Owners fill the table to the cap, each with its full share. Owner A
	// holds secure flows and one still-negotiating flow (SYN only).
	for owner := range owners {
		ownerId := Id{byte(owner + 1)}
		for index := range smtpMaxOwnerFlowCount {
			if ownerId == ownerA && index == smtpMaxOwnerFlowCount-1 {
				requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
					ownerA, smtpTestSyn(30000, smtpImplicitTlsPort, 1), nil,
				))
				continue
			}
			requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
				ownerId,
				smtpTestPath(20000+index, smtpImplicitTlsPort, uint32(index+1)),
				smtpTestClientHello,
			))
		}
	}

	// Owner B has no flow of its own to give up: refused, nothing evicted.
	requireSmtpVerdict(t, smtpEgressReject, guard.inspectForOwner(
		ownerB,
		smtpTestSyn(31000, smtpImplicitTlsPort, 1), nil,
	))

	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != smtpMaxFlowCount {
		t.Fatalf("flow table size = %d, want %d (cap must stay bounded)", len(guard.flows), smtpMaxFlowCount)
	}
	firstSecureKey, ok := smtpFlowKeyForOwnerPath(ownerA, smtpTestPath(20000, smtpImplicitTlsPort, 1))
	if !ok {
		t.Fatal("could not build owner A secure flow key")
	}
	if _, exists := guard.flows[firstSecureKey]; !exists {
		t.Fatal("owner A's secure flow was evicted by owner B's insert")
	}
	negotiatingKey, ok := smtpFlowKeyForOwnerPath(ownerA, smtpTestSyn(30000, smtpImplicitTlsPort, 1))
	if !ok {
		t.Fatal("could not build owner A negotiating flow key")
	}
	if _, exists := guard.flows[negotiatingKey]; !exists {
		t.Fatal("owner A's negotiating flow was evicted by owner B's insert")
	}
	ownerBKey, ok := smtpFlowKeyForOwnerPath(ownerB, smtpTestSyn(31000, smtpImplicitTlsPort, 1))
	if !ok {
		t.Fatal("could not build owner B flow key")
	}
	if _, exists := guard.flows[ownerBKey]; exists {
		t.Fatal("owner B's flow was stored past the cap")
	}
}

// One client cannot hold more than its share: past smtpMaxOwnerFlowCount its
// own oldest expendable flow makes room, so its count never grows, while the
// rest of the table stays free for other clients.
func TestSmtpGuardCapsFlowsPerOwner(t *testing.T) {
	var guard smtpEgressGuard
	owner := Id{7}
	for index := range smtpMaxOwnerFlowCount + 10 {
		requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
			owner, smtpTestSyn(40000+index, smtpImplicitTlsPort, 1), nil,
		))
	}
	other := Id{8}
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		other, smtpTestSyn(50000, smtpImplicitTlsPort, 1), nil,
	))
	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if got := guard.ownerFlowCountWithLock(owner); got != smtpMaxOwnerFlowCount {
		t.Fatalf("owner holds %d flows, want %d", got, smtpMaxOwnerFlowCount)
	}
	if got := guard.ownerFlowCountWithLock(other); got != 1 {
		t.Fatalf("other owner holds %d flows, want 1", got)
	}
}

// Tuples that end without an RST are retired once idle, and the reap runs
// only once per interval.
func TestSmtpGuardReapsIdleFlows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	guard := smtpEgressGuard{timeNow: func() time.Time { return now }}
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		Id{1}, smtpTestPath(41000, smtpImplicitTlsPort, 1), smtpTestClientHello,
	))
	now = now.Add(smtpFlowIdleTimeout / 2)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		Id{2}, smtpTestSyn(41001, smtpImplicitTlsPort, 1), nil,
	))
	// the first flow is now idle past the timeout; the second is not
	now = now.Add(smtpFlowIdleTimeout/2 + time.Second)
	requireSmtpVerdict(t, smtpEgressAllow, guard.inspectForOwner(
		Id{3}, smtpTestSyn(41002, smtpImplicitTlsPort, 1), nil,
	))
	guard.stateLock.Lock()
	defer guard.stateLock.Unlock()
	if len(guard.flows) != 2 {
		t.Fatalf("%d flows after the reap, want 2", len(guard.flows))
	}
	if guard.ownerFlowCountWithLock(Id{1}) != 0 {
		t.Fatal("the idle flow was not reaped")
	}
}
