package connect

import (
	"context"
	"math"
	mathrand "math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The reconnect attempt budget.
//
// While the backend is failing, every disconnected proxy retries on its own
// backoff and nothing adds the attempts up, so N proxies produce N/60 full
// connect attempts per second. The budget is the missing sum. It is a single
// process-wide rate that platform-transport connects must be admitted under,
// and it only exists while the governor says attempts are failing:
//
//   - healthy backend: the rate sits at the ceiling with an empty queue, and
//     admit is a lock and two compares. Nothing waits, so startup, hot restarts
//     and ordinary reconnects run at full speed.
//   - failing backend: the rate is cut by budgetDecreaseFactor every control
//     interval down to a floor, so a few scouts keep probing and the rest of the
//     proxies wait without holding a socket.
//   - recovery: as soon as recent attempts connect, the rate doubles every
//     interval back to the ceiling, and the queue drains first-in first-out, so
//     thousands of parked proxies return in about a minute without a herd.
//
// The direct path is exempt, and connected transports never pass through here:
// only the start of a connect does.

const (
	// budgetFloorRate is the attempts per second let through while the backend
	// is failing: the scouts.
	budgetFloorRate = 2.0
	// budgetCeilingRate is where the budget opens again. It matches the ceiling
	// the proxy auth limiter already runs at, so recovery is no harsher on the
	// backend than ordinary startup.
	budgetCeilingRate = 200.0

	budgetDecreaseFactor = 4.0
	budgetIncreaseFactor = 2.0

	budgetControlInterval = 5 * time.Second
	budgetTick            = 50 * time.Millisecond

	// budgetRecentWindow is the span the controller judges. Short, so recovery
	// is seen in seconds.
	budgetRecentWindow = 10 * time.Second
	// fewer attempts than this in the window say nothing, in either direction
	budgetMinDecreaseAttempts = 10
	budgetMinIncreaseAttempts = 3
	// share of good attempts at which the budget starts opening
	budgetOpenRatio = 0.5
)

// A waiter that finds the dispatcher has not ticked for budgetStallGrace is let
// through: a dead dispatcher must never park proxies forever. Variables so tests
// can shorten them.
var (
	budgetStallCheckIntervalNs atomic.Int64
	budgetStallGraceNs         atomic.Int64
)

func init() {
	budgetStallCheckIntervalNs.Store(int64(5 * time.Second))
	budgetStallGraceNs.Store(int64(5 * time.Second))
}

// setBudgetStallForTest shortens the stall detector and returns a restore func.
// Atomic because waiter goroutines read them while a test restores them.
func setBudgetStallForTest(interval, grace time.Duration) func() {
	prevI, prevG := budgetStallCheckIntervalNs.Swap(int64(interval)), budgetStallGraceNs.Swap(int64(grace))
	return func() {
		budgetStallCheckIntervalNs.Store(prevI)
		budgetStallGraceNs.Store(prevG)
	}
}

type governMode int32

const (
	// governOff: the governor and budget do nothing
	governOff governMode = iota
	// governShadow: observe and report only (the default)
	governShadow
	// governEnforce: the budget gates connects and a failing backend gets one
	// dial round per attempt
	governEnforce
)

var governModeValue atomic.Int32

func init() { governModeValue.Store(int32(parseGovernMode(os.Getenv("URNETWORK_GOVERN")))) }

func parseGovernMode(raw string) governMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "0", "false", "no":
		return governOff
	case "enforce", "on", "1", "true", "yes":
		return governEnforce
	}
	return governShadow
}

func currentGovernMode() governMode { return governMode(governModeValue.Load()) }

// setGovernModeForTest swaps the mode and returns a restore func.
func setGovernModeForTest(mode governMode) func() {
	prev := governModeValue.Swap(int32(mode))
	return func() { governModeValue.Store(prev) }
}

// scoutRandomEvery makes every Nth released token go to a random waiter and the
// rest to the waiter with the best record. Best-first brings the proxies that
// earned before the outage back soonest and probes with the likeliest to work;
// the random slot means a stale favourite, or a proxy with no history yet, can
// never hold everyone else out.
const scoutRandomEvery = 3

type admitWaiter struct {
	ch       chan struct{}
	score    uint64
	canceled atomic.Bool
}

type reconnectBudget struct {
	gov *connectGovernor
	now func() time.Time

	// lastOutage is the unix nanos of the last control tick that found the
	// budget throttled
	lastOutage atomic.Int64

	// pick chooses the random slot's waiter; replaceable so tests are exact
	pick func(n int) int

	mu         sync.Mutex
	rate       float64
	carry      float64
	lastTick   time.Time
	queue      []*admitWaiter
	dispatched int
}

func newReconnectBudget(gov *connectGovernor, now func() time.Time) *reconnectBudget {
	return &reconnectBudget{gov: gov, now: now, rate: budgetCeilingRate, lastTick: now(), pick: mathrand.Intn}
}

func (self *reconnectBudget) currentRate() float64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.rate
}

func (self *reconnectBudget) setRate(rate float64) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.rate = min(max(rate, budgetFloorRate), budgetCeilingRate)
}

func (self *reconnectBudget) queued() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.queue)
}

// throttled reports whether the budget is holding anything back: a reduced rate,
// or waiters still draining after the rate recovered.
func (self *reconnectBudget) throttled() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.rate < budgetCeilingRate || 0 < len(self.queue)
}

// admit blocks until a connect may start. It returns false only when ctx ends
// first. The direct path never waits.
func (self *reconnectBudget) admit(ctx context.Context, direct bool) bool {
	return self.admitParked(ctx, direct, nil, nil)
}

// admitParked is admit that ranks the caller by score (evaluated only if it has
// to wait, so the healthy path never pays for it) and reports to onPark when it
// starts and stops waiting, so the proxy can be shown as parked rather than
// broken. Both may be nil.
func (self *reconnectBudget) admitParked(ctx context.Context, direct bool, score func() uint64, onPark func(parked bool)) bool {
	if direct {
		return true
	}
	self.mu.Lock()
	if self.rate >= budgetCeilingRate && len(self.queue) == 0 {
		self.mu.Unlock()
		return true
	}
	self.mu.Unlock()

	// Scored only now that we know we must wait, so the healthy path never pays
	// for it, and BEFORE joining the queue: the dispatcher reads the score under
	// the lock, so it must never be written after the waiter is visible. The
	// scorer takes its own locks, so it cannot run under ours.
	var waiterScore uint64
	if score != nil {
		waiterScore = score()
	}

	self.mu.Lock()
	if self.rate >= budgetCeilingRate && len(self.queue) == 0 {
		// the budget reopened while we were scoring
		self.mu.Unlock()
		return true
	}
	w := &admitWaiter{ch: make(chan struct{}), score: waiterScore}
	self.queue = append(self.queue, w)
	self.mu.Unlock()

	if onPark != nil {
		onPark(true)
		defer onPark(false)
	}

	stall := time.NewTicker(time.Duration(budgetStallCheckIntervalNs.Load()))
	defer stall.Stop()
	for {
		select {
		case <-w.ch:
			return true
		case <-ctx.Done():
			w.canceled.Store(true)
			return false
		case <-stall.C:
			if self.stalled() {
				// the dispatcher is not running: fail open, and drop the
				// queue entry so it does not take a later token
				w.canceled.Store(true)
				return true
			}
		}
	}
}

// stalled reports whether the dispatcher has missed its ticks.
func (self *reconnectBudget) stalled() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return time.Duration(budgetStallGraceNs.Load()) < self.now().Sub(self.lastTick)
}

// tick releases the waiters the elapsed time has earned, oldest first. A
// canceled waiter earns nothing and is dropped without using a token.
func (self *reconnectBudget) tick(now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()

	dt := now.Sub(self.lastTick)
	self.lastTick = now
	if dt <= 0 {
		return
	}
	// a long gap (the loop was not running, the host slept) must not mint a
	// burst of tokens
	dt = min(dt, time.Second)

	self.carry += self.rate * dt.Seconds()
	// the epsilon absorbs float drift: twenty ticks of 0.1 token sum to
	// 1.9999999999999998, which must still release two
	n := int(math.Floor(self.carry + 1e-9))
	self.carry -= float64(n)

	// canceled waiters earn nothing and are dropped without using a token
	live := self.queue[:0]
	for _, w := range self.queue {
		if !w.canceled.Load() {
			live = append(live, w)
		}
	}
	clear(self.queue[len(live):])
	self.queue = live

	released := 0
	for released < n && 0 < len(self.queue) {
		i := self.nextLocked()
		w := self.queue[i]
		self.queue = append(self.queue[:i], self.queue[i+1:]...)
		close(w.ch)
		released += 1
		self.dispatched += 1
	}
	if len(self.queue) == 0 {
		// unspent time is not banked: an idle budget must not release a burst
		self.carry = 0
	}
}

// nextLocked picks the queue index to release next: the best score, earliest
// arrival on ties (so equal scores stay first-in first-out), except that every
// scoutRandomEvery-th token goes to a random waiter. Caller holds mu and the
// queue is not empty.
func (self *reconnectBudget) nextLocked() int {
	if self.dispatched%scoutRandomEvery == scoutRandomEvery-1 {
		return min(max(self.pick(len(self.queue)), 0), len(self.queue)-1)
	}
	best := 0
	for i, w := range self.queue {
		if self.queue[best].score < w.score {
			best = i
		}
	}
	return best
}

// control adjusts the rate from what recent attempts did. Cut hard on evidence
// of failure, open fast on evidence of success, hold in between.
func (self *reconnectBudget) control() {
	recent := self.gov.snapshotWindow(budgetRecentWindow)

	self.mu.Lock()
	defer self.mu.Unlock()

	switch {
	case budgetMinDecreaseAttempts <= recent.PlatformAttempts && recent.Ratio < govDegradedRatio:
		self.rate = max(budgetFloorRate, self.rate/budgetDecreaseFactor)
	case self.rate < budgetCeilingRate &&
		(budgetMinIncreaseAttempts <= recent.PlatformAttempts && budgetOpenRatio <= recent.Ratio):
		self.rate = min(budgetCeilingRate, self.rate*budgetIncreaseFactor)
	case self.rate < budgetCeilingRate && recent.PlatformAttempts < budgetMinIncreaseAttempts:
		// Too few attempts reached the platform to say anything. Either nothing
		// is trying, or the scouts the budget released landed on dead proxies.
		// A reduced rate must not be held on no evidence, so probe harder:
		// an uninformative attempt is cheap, it never reached the platform.
		self.rate = min(budgetCeilingRate, self.rate*budgetIncreaseFactor)
	}

	if self.rate < budgetCeilingRate {
		self.lastOutage.Store(self.now().UnixNano())
	}
}

// outageRecent reports whether the budget is throttled now or was within d. A
// proxy that has been down since an outage may only be called dead after a
// quiet period, not on the day the backend comes back.
func (self *reconnectBudget) outageRecent(d time.Duration) bool {
	if self.throttled() {
		return true
	}
	last := self.lastOutage.Load()
	return last != 0 && self.now().Sub(time.Unix(0, last)) < d
}

func (self *reconnectBudget) run(ctx context.Context) {
	tick := time.NewTicker(budgetTick)
	defer tick.Stop()
	control := time.NewTicker(budgetControlInterval)
	defer control.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			self.tick(now)
		case <-control.C:
			self.control()
		}
	}
}

// connectBudget is the process-wide budget.
var connectBudget = newReconnectBudget(connectGov, time.Now)

var budgetLoopOnce sync.Once

// admitConnect gates the start of a platform-transport connect. In any mode but
// enforce it admits immediately.
func admitConnect(ctx context.Context, direct bool, proxyIndex int) bool {
	if currentGovernMode() != governEnforce {
		return true
	}
	budgetLoopOnce.Do(func() { go connectBudget.run(context.Background()) })
	return connectBudget.admitParked(
		ctx,
		direct,
		func() uint64 { return proxyScoutScore(proxyIndex) },
		func(parked bool) { markProxyParked(proxyIndex, parked) },
	)
}

// OutageRecent reports whether a backend outage is under way or ended within d,
// as seen by the reconnect attempt budget. The provider's reapers use it to hold
// off: a proxy that has been down since the outage is not dead until a long
// quiet period says so. Always false unless the governor is enforcing.
func OutageRecent(d time.Duration) bool {
	if currentGovernMode() != governEnforce {
		return false
	}
	return connectBudget.outageRecent(d)
}

// singleRoundCheck is replaceable so tests can force either answer.
var singleRoundCheck = singleRoundWhenDegraded

// singleRoundWhenDegraded reports whether a failed dial should give up after
// one pass over the dialers instead of retrying them until the request deadline.
// Retrying is what makes one failed attempt cost many dials; while the backend
// is failing the next round fails too.
func singleRoundWhenDegraded() bool {
	return currentGovernMode() == governEnforce && connectGov.snapshot().Degraded
}
