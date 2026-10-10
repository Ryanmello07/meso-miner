package connect

import (
	"context"
	"math"
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

type admitWaiter struct {
	ch       chan struct{}
	canceled atomic.Bool
}

type reconnectBudget struct {
	gov *connectGovernor
	now func() time.Time

	mu       sync.Mutex
	rate     float64
	carry    float64
	lastTick time.Time
	queue    []*admitWaiter
}

func newReconnectBudget(gov *connectGovernor, now func() time.Time) *reconnectBudget {
	return &reconnectBudget{gov: gov, now: now, rate: budgetCeilingRate, lastTick: now()}
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
	if direct {
		return true
	}
	self.mu.Lock()
	if self.rate >= budgetCeilingRate && len(self.queue) == 0 {
		self.mu.Unlock()
		return true
	}
	w := &admitWaiter{ch: make(chan struct{})}
	self.queue = append(self.queue, w)
	self.mu.Unlock()

	select {
	case <-w.ch:
		return true
	case <-ctx.Done():
		w.canceled.Store(true)
		return false
	}
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

	released := 0
	for released < n && 0 < len(self.queue) {
		w := self.queue[0]
		self.queue[0] = nil
		self.queue = self.queue[1:]
		if w.canceled.Load() {
			continue
		}
		close(w.ch)
		released += 1
	}
	if len(self.queue) == 0 {
		// unspent time is not banked: an idle budget must not release a burst
		self.carry = 0
	}
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
	case self.rate < budgetCeilingRate && recent.PlatformAttempts == 0 && len(self.queue) == 0:
		// nothing is trying: stop holding a reduced rate for no reason
		self.rate = min(budgetCeilingRate, self.rate*budgetIncreaseFactor)
	}
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
func admitConnect(ctx context.Context, direct bool) bool {
	if currentGovernMode() != governEnforce {
		return true
	}
	budgetLoopOnce.Do(func() { go connectBudget.run(context.Background()) })
	return connectBudget.admit(ctx, direct)
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
