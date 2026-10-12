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
	// releasedAtNano is when this destination was last released (or, in shadow,
	// would have been); zero once it has acknowledged since. releasedShadow tells
	// the two apart.
	releasedAtNano atomic.Int64
	releasedShadow atomic.Bool
}

func (self *destinationYield) noteSend(byteCount int, nowNano int64) {
	self.sentBytes.Add(int64(byteCount))
	self.lastSendNano.Store(nowNano)
	self.silentSinceNano.CompareAndSwap(0, nowNano)
}

func (self *destinationYield) noteAck(byteCount int, nowNano int64) {
	self.ackedBytes.Add(int64(byteCount))
	self.noteLive(nowNano)
}

// noteLive records proof the destination is receiving without counting bytes,
// for acknowledgements that are counted when the item is later acknowledged in
// order (a selective ack). A silence that ends here is a recovered silence, and
// its length is the data the release thresholds should be chosen from.
func (self *destinationYield) noteLive(nowNano int64) {
	self.lastAckNano.Store(nowNano)
	if released := self.releasedAtNano.Swap(0); released != 0 && released < nowNano {
		recordReturnAfterRelease(self.releasedShadow.Load(), time.Duration(nowNano-released))
	}
	if since := self.silentSinceNano.Swap(0); since != 0 && since < nowNano {
		recordRecoveredSilence(time.Duration(nowNano - since))
	}
}

// reset forgets the silence after a release, so a client that comes back is
// judged afresh rather than released again at once.
func (self *destinationYield) reset() {
	self.silentSinceNano.Store(0)
}

// markReleased remembers that this destination was released (or would have been,
// in shadow), so that if it acknowledges again the time it took to come back is
// recorded: a release followed by a quick return was a false release.
func (self *destinationYield) markReleased(nowNano int64, shadow bool) {
	self.releasedShadow.Store(shadow)
	self.releasedAtNano.Store(nowNano)
	self.reset()
}

// destinationYieldView is a point-in-time reading of one destination. Durations
// are zero when the event never happened.
type destinationYieldView struct {
	DestinationId Id
	// owner is the send buffer holding the record, set when views from several
	// send buffers are judged together.
	owner *SendBuffer
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
		// a destination nothing has been sent to or acknowledged by for a while is
		// forgotten whether or not it was silent: silence only means something
		// while we are still writing to it
		if yieldRetention < max(view.SinceLastSend, view.SinceLastAck) {
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

// MarkDestinationReleased forgets a destination's silence after releasing it and
// starts timing how long it takes to come back.
func (self *SendBuffer) MarkDestinationReleased(destinationId Id, now time.Time, shadow bool) {
	if existing, ok := self.yields.Load(destinationId); ok {
		existing.(*destinationYield).markReleased(now.UnixNano(), shadow)
	}
}

// HeldBytes is the undelivered bytes queued for resend across every send
// sequence to one destination.
func (self *SendBuffer) HeldBytes(destinationId Id) int64 {
	self.mutex.Lock()
	sequences := []*SendSequence{}
	for id, sendSequence := range self.sendSequences {
		if id.Destination.DestinationId == destinationId {
			sequences = append(sequences, sendSequence)
		}
	}
	self.mutex.Unlock()
	total := int64(0)
	for _, sendSequence := range sequences {
		_, byteCount, _, _ := sendSequence.ResendQueueSizeAndMessageTypes()
		total += int64(byteCount)
	}
	return total
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
// A destination that is still being written to is released when it has been silent for Silence while the fleet
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

// Why a destination was released.
const (
	// the fleet is acknowledging, so this client is the problem
	deadClientReasonHealthy = "healthy_fleet"
	// the fleet is collapsed, but the client has been silent past the hard limit
	deadClientReasonHard = "hard_silence"
	// too few active destinations to judge the fleet, silent past the hard limit
	deadClientReasonSmall = "small_fleet"
)

type deadClientRelease struct {
	View   destinationYieldView
	Reason string
}

type deadClientDecision struct {
	Release []deadClientRelease
	// Silent counts destinations still being written to that are past Silence,
	// released or held.
	Silent         int
	FleetCollapsed bool
	Active         int
	Acking         int
}

// decideDeadClients judges the views together. A destination is only a
// candidate while we are still writing to it: silence with nothing outstanding
// is an idle client, not a gone one.
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
	judged := policy.MinActive <= decision.Active
	decision.FleetCollapsed = judged &&
		float64(decision.Acking) < policy.HealthyFraction*float64(decision.Active)
	for _, view := range views {
		writing := view.SinceLastSend != 0 && view.SinceLastSend <= policy.ActiveWindow
		if !writing || view.SilentFor < policy.Silence {
			continue
		}
		decision.Silent += 1
		switch {
		case !judged:
			// with few active destinations there is no basis to blame the client
			// rather than the platform, so only the hard limit releases
			if policy.HardSilence <= view.SilentFor {
				decision.Release = append(decision.Release, deadClientRelease{view, deadClientReasonSmall})
			}
		case decision.FleetCollapsed:
			if policy.HardSilence <= view.SilentFor {
				decision.Release = append(decision.Release, deadClientRelease{view, deadClientReasonHard})
			}
		default:
			decision.Release = append(decision.Release, deadClientRelease{view, deadClientReasonHealthy})
		}
	}
	return decision
}

// recoveredSilenceBounds are the upper bounds of the recovered-silence
// histogram; the last bucket is everything longer.
var recoveredSilenceBounds = []time.Duration{
	10 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second,
	180 * time.Second, 300 * time.Second, 600 * time.Second,
}

var recoveredSilenceCounts [8]atomic.Int64

func recordRecoveredSilence(duration time.Duration) {
	for i, bound := range recoveredSilenceBounds {
		if duration <= bound {
			recoveredSilenceCounts[i].Add(1)
			return
		}
	}
	recoveredSilenceCounts[len(recoveredSilenceBounds)].Add(1)
}

// RecoveredSilenceHistogram is the count of silences that ended in an
// acknowledgement, per bucket (cumulative, Prometheus style), with the bucket
// upper bounds in seconds and +Inf last.
func RecoveredSilenceHistogram() (boundsSeconds []float64, cumulative []int64) {
	total := int64(0)
	for i := range recoveredSilenceCounts {
		total += recoveredSilenceCounts[i].Load()
		cumulative = append(cumulative, total)
		if i < len(recoveredSilenceBounds) {
			boundsSeconds = append(boundsSeconds, recoveredSilenceBounds[i].Seconds())
		}
	}
	return boundsSeconds, cumulative
}

// returnAfterReleaseBounds are the upper bounds of the histogram of how long a
// released destination took to acknowledge again; the last bucket is everything
// longer. One histogram for enforce, one for shadow.
var returnAfterReleaseBounds = []time.Duration{
	30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
}

var returnAfterReleaseCounts [2][7]atomic.Int64

func recordReturnAfterRelease(shadow bool, duration time.Duration) {
	mode := 0
	if shadow {
		mode = 1
	}
	for i, bound := range returnAfterReleaseBounds {
		if duration <= bound {
			returnAfterReleaseCounts[mode][i].Add(1)
			return
		}
	}
	returnAfterReleaseCounts[mode][len(returnAfterReleaseBounds)].Add(1)
}

// ReturnAfterReleaseHistogram is, for enforce or shadow, how many released
// destinations acknowledged again and how long they took (cumulative buckets,
// bounds in seconds). Every count is a release that was wrong, or in shadow a
// release that would have been.
func ReturnAfterReleaseHistogram(shadow bool) (boundsSeconds []float64, cumulative []int64) {
	mode := 0
	if shadow {
		mode = 1
	}
	total := int64(0)
	for i := range returnAfterReleaseCounts[mode] {
		total += returnAfterReleaseCounts[mode][i].Load()
		cumulative = append(cumulative, total)
		if i < len(returnAfterReleaseBounds) {
			boundsSeconds = append(boundsSeconds, returnAfterReleaseBounds[i].Seconds())
		}
	}
	return boundsSeconds, cumulative
}
