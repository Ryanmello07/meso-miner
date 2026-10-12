package connect

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// The dead-client sweeper.
//
// Nothing tells a provider that a client it is answering has gone away. Returns
// to that client are retried and held until their own timers expire, a new
// sequence opens for the next packet, and the client's NAT flows (a TCP window
// that is never read, a UDP flow that nobody drains) stay up. This sweeper reads
// the per-destination ack yield (transfer_dest_yield.go) and, when a destination
// has been written to for deadClientPolicy.Silence without acknowledging
// anything while the rest of the fleet is acknowledging, releases it: its send
// sequences are cancelled, which fails the bytes held for it, and its NAT flows
// are cancelled. A client that was only slow and comes back simply opens fresh
// flows. When almost nothing is acknowledged anywhere it is the backend, not the
// clients, and the sweeper waits (see decideDeadClients).
//
// URNETWORK_GOVERN decides what happens: enforce releases, shadow counts and
// logs what it would release, off does nothing.

const deadClientSweepInterval = 15 * time.Second

// deadClientShadowLogEvery limits how often one destination is named in shadow.
const deadClientShadowLogEvery = 5 * time.Minute

var deadClientCounters struct {
	released          atomic.Int64
	releasedFlows     atomic.Int64
	releasedSequences atomic.Int64
	wouldRelease      atomic.Int64
	sweeps            atomic.Int64
	silent            atomic.Int64
	fleetCollapsed    atomic.Int64
}

// DeadClientStats is a snapshot of the dead-client counters.
type DeadClientStats struct {
	// Released is destinations released, with the NAT flows and send sequences
	// cancelled for them. WouldRelease is shadow-mode destinations.
	Released          int64
	ReleasedFlows     int64
	ReleasedSequences int64
	WouldRelease      int64
	Sweeps            int64
	// Silent is the silent destinations seen by the latest sweep and
	// FleetCollapsed is 1 when that sweep judged the backend, not the clients, at fault.
	Silent         int64
	FleetCollapsed int64
}

func DeadClientStatsNow() DeadClientStats {
	return DeadClientStats{
		Released:          deadClientCounters.released.Load(),
		ReleasedFlows:     deadClientCounters.releasedFlows.Load(),
		ReleasedSequences: deadClientCounters.releasedSequences.Load(),
		WouldRelease:      deadClientCounters.wouldRelease.Load(),
		Sweeps:            deadClientCounters.sweeps.Load(),
		Silent:            deadClientCounters.silent.Load(),
		FleetCollapsed:    deadClientCounters.fleetCollapsed.Load(),
	}
}

type deadClientSweeper struct {
	provider *RemoteUserNatProvider
	policy   deadClientPolicy
	now      func() time.Time
	emit     func(string)

	mutex       sync.Mutex
	shadowLogAt map[Id]time.Time
}

func newDeadClientSweeper(provider *RemoteUserNatProvider) *deadClientSweeper {
	return &deadClientSweeper{
		provider:    provider,
		policy:      defaultDeadClientPolicy(),
		now:         time.Now,
		emit:        emitGovernLine,
		shadowLogAt: map[Id]time.Time{},
	}
}

func (self *deadClientSweeper) run() {
	ticker := time.NewTicker(deadClientSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-self.provider.client.ctx.Done():
			return
		case <-ticker.C:
			self.sweep()
		}
	}
}

// sweep reads the yields once and acts on the decision. It reports how many
// destinations it released.
func (self *deadClientSweeper) sweep() int {
	mode := currentGovernMode()
	deadClientCounters.sweeps.Add(1)
	now := self.now()
	decision := decideDeadClients(self.provider.client.sendBuffer.DestinationYields(now), self.policy)

	deadClientCounters.silent.Store(int64(decision.Silent))
	if decision.FleetCollapsed {
		deadClientCounters.fleetCollapsed.Store(1)
	} else {
		deadClientCounters.fleetCollapsed.Store(0)
	}
	if mode == governOff {
		return 0
	}

	released := 0
	for _, view := range decision.Release {
		if mode == governShadow {
			deadClientCounters.wouldRelease.Add(1)
			if self.shouldLogShadow(view.DestinationId, now) {
				self.emit(fmt.Sprintf(
					"[govern] dead client %s silent %s (sent %.1f KiB, acked %.1f KiB; %d of %d active clients acking); shadow, not released",
					view.DestinationId, view.SilentFor.Round(time.Second),
					float64(view.SentBytes)/1024, float64(view.AckedBytes)/1024, decision.Acking, decision.Active,
				))
			}
			continue
		}
		sequences := self.provider.client.sendBuffer.CancelDestination(view.DestinationId)
		flows := self.provider.localUserNat.CancelSource(view.DestinationId)
		self.provider.client.sendBuffer.ResetDestinationYield(view.DestinationId)
		deadClientCounters.released.Add(1)
		deadClientCounters.releasedFlows.Add(int64(flows))
		deadClientCounters.releasedSequences.Add(int64(sequences))
		released += 1
		self.emit(fmt.Sprintf(
			"[govern] released dead client %s: silent %s (sent %.1f KiB, acked %.1f KiB; %d of %d active clients acking), cancelled %d send sequences and %d NAT flows",
			view.DestinationId, view.SilentFor.Round(time.Second),
			float64(view.SentBytes)/1024, float64(view.AckedBytes)/1024, decision.Acking, decision.Active,
			sequences, flows,
		))
	}
	return released
}

func (self *deadClientSweeper) shouldLogShadow(destinationId Id, now time.Time) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if last, ok := self.shadowLogAt[destinationId]; ok && now.Sub(last) < deadClientShadowLogEvery {
		return false
	}
	self.shadowLogAt[destinationId] = now
	// forget old names so the map cannot grow without bound
	for id, at := range self.shadowLogAt {
		if 4*deadClientShadowLogEvery < now.Sub(at) {
			delete(self.shadowLogAt, id)
		}
	}
	return true
}
