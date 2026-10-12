package connect

import (
	"sync/atomic"
	"time"
)

// Ack yield per destination.
//
// A provider answers a client by sending it frames and waiting for the client to
// acknowledge them. Per-item timers cannot tell a client that is gone from a slow
// one, and the transfer sequence for a destination comes and goes every minute,
// so nothing remembered "this client has not acknowledged anything for minutes".
// This record does. It lives with the send buffer, keyed by destination, and
// outlives the sequences.
//
// silentSince is the first write after the destination last acknowledged
// anything, and is cleared by any acknowledgement. A destination that is written
// to and never answers keeps its silentSince for as long as it stays silent, so
// the silence is measured in wall time whether or not the backend is degraded.

type destinationYield struct {
	destinationId   Id
	sentBytes       atomic.Int64
	ackedBytes      atomic.Int64
	silentSinceNano atomic.Int64
	lastAckNano     atomic.Int64
	lastSendNano    atomic.Int64
}

func (self *destinationYield) noteSend(byteCount int, nowNano int64) {
	self.sentBytes.Add(int64(byteCount))
	self.lastSendNano.Store(nowNano)
	self.silentSinceNano.CompareAndSwap(0, nowNano)
}

func (self *destinationYield) noteAck(byteCount int, nowNano int64) {
	self.ackedBytes.Add(int64(byteCount))
	self.lastAckNano.Store(nowNano)
	self.silentSinceNano.Store(0)
}

// reset forgets the silence after a release, so a client that comes back is
// judged afresh rather than released again at once.
func (self *destinationYield) reset() {
	self.silentSinceNano.Store(0)
}

// destinationYieldView is a point-in-time reading of one destination. Durations
// are zero when the event never happened.
type destinationYieldView struct {
	DestinationId Id
	// SilentFor is how long the destination has been written to without
	// acknowledging anything. Zero when it is not silent.
	SilentFor time.Duration
	// SinceLastAck and SinceLastSend are zero when it never happened.
	SinceLastAck  time.Duration
	SinceLastSend time.Duration
	SentBytes     int64
	AckedBytes    int64
}

func (self *destinationYield) view(now time.Time) destinationYieldView {
	age := func(nano int64) time.Duration {
		if nano == 0 {
			return 0
		}
		return max(0, now.Sub(time.Unix(0, nano)))
	}
	return destinationYieldView{
		DestinationId: self.destinationId,
		SilentFor:     age(self.silentSinceNano.Load()),
		SinceLastAck:  age(self.lastAckNano.Load()),
		SinceLastSend: age(self.lastSendNano.Load()),
		SentBytes:     self.sentBytes.Load(),
		AckedBytes:    self.ackedBytes.Load(),
	}
}

// yieldFor returns the yield record for a destination, creating it on first use.
func (self *SendBuffer) yieldFor(destinationId Id) *destinationYield {
	if existing, ok := self.yields.Load(destinationId); ok {
		return existing.(*destinationYield)
	}
	created, _ := self.yields.LoadOrStore(destinationId, &destinationYield{destinationId: destinationId})
	return created.(*destinationYield)
}

// DestinationYields reads every destination this send buffer has written to. A
// record idle for yieldRetention is dropped.
func (self *SendBuffer) DestinationYields(now time.Time) []destinationYieldView {
	var views []destinationYieldView
	self.yields.Range(func(key, value any) bool {
		yield := value.(*destinationYield)
		view := yield.view(now)
		if yieldRetention < max(view.SinceLastSend, view.SinceLastAck) && view.SilentFor == 0 {
			self.yields.Delete(key)
			return true
		}
		views = append(views, view)
		return true
	})
	return views
}

// yieldRetention is how long a destination with nothing outstanding is remembered.
const yieldRetention = 15 * time.Minute

// ResetDestinationYield forgets a destination's silence after releasing it.
func (self *SendBuffer) ResetDestinationYield(destinationId Id) {
	if existing, ok := self.yields.Load(destinationId); ok {
		existing.(*destinationYield).reset()
	}
}

// CancelDestination cancels every send sequence to one destination and returns
// how many. Cancelling fails each sequence's undelivered items through its normal
// teardown, so held bytes are released and counted as undelivered.
func (self *SendBuffer) CancelDestination(destinationId Id) int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	count := 0
	for id, sendSequence := range self.sendSequences {
		if id.Destination.DestinationId == destinationId {
			sendSequence.Cancel()
			count += 1
		}
	}
	return count
}

// The dead-client policy.
//
// A destination is released when it has been silent for Silence while the fleet
// looks healthy, meaning other destinations are acknowledging: then the problem
// is that client, not the backend, and its held bytes are not coming back. When
// almost nothing is being acknowledged anywhere the backend or relay is the
// problem and releasing every client would tear down connections that recover
// with it, so release waits, but never past HardSilence.
type deadClientPolicy struct {
	Silence     time.Duration
	HardSilence time.Duration
	// ActiveWindow is how recently a destination must have sent or been
	// acknowledged to count as active.
	ActiveWindow time.Duration
	// AckWindow is how recently an active destination must have acknowledged to
	// count as healthy.
	AckWindow       time.Duration
	HealthyFraction float64
	// MinActive is the fewest active destinations at which a fleet-wide collapse
	// can be declared; with fewer, there is no basis to call it the backend.
	MinActive int
}

func defaultDeadClientPolicy() deadClientPolicy {
	return deadClientPolicy{
		Silence:         180 * time.Second,
		HardSilence:     600 * time.Second,
		ActiveWindow:    120 * time.Second,
		AckWindow:       60 * time.Second,
		HealthyFraction: 0.2,
		MinActive:       5,
	}
}

type deadClientDecision struct {
	Release []destinationYieldView
	// Silent counts destinations past Silence, released or held.
	Silent         int
	FleetCollapsed bool
	Active         int
	Acking         int
}

func decideDeadClients(views []destinationYieldView, policy deadClientPolicy) deadClientDecision {
	decision := deadClientDecision{}
	for _, view := range views {
		recentSend := view.SinceLastSend != 0 && view.SinceLastSend <= policy.ActiveWindow
		recentAck := view.SinceLastAck != 0 && view.SinceLastAck <= policy.ActiveWindow
		if !recentSend && !recentAck {
			continue
		}
		decision.Active += 1
		if view.SinceLastAck != 0 && view.SinceLastAck <= policy.AckWindow {
			decision.Acking += 1
		}
	}
	decision.FleetCollapsed = policy.MinActive <= decision.Active &&
		float64(decision.Acking) < policy.HealthyFraction*float64(decision.Active)
	for _, view := range views {
		if view.SilentFor < policy.Silence {
			continue
		}
		decision.Silent += 1
		if decision.FleetCollapsed && view.SilentFor < policy.HardSilence {
			continue
		}
		decision.Release = append(decision.Release, view)
	}
	return decision
}
