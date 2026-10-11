package connect

import (
	"sync"
	"sync/atomic"
	"time"
)

// The platform speed-tests a transport right after it connects: it sends a
// control frame, then floods the websocket with data, and the provider echoes
// every byte back. That is real bandwidth on the proxy (the paid ones are metered)
// for zero billable payload. One test is small. But every connect gets one, so a
// mass reconnect, which is exactly what a backend outage produces, turns into a
// bandwidth storm: on one host 700 reconnects in a minute echoed 2.4 GiB, and the
// connect attempt budget could not see it because the cost is not in the attempt.
//
// So the echo is bounded three ways:
//
//   - a test stops being echoed after speedEchoSessionMaxBytes, so a test that is
//     never stopped cannot run for ever;
//   - all tests together share a token bucket, so N simultaneous tests cost the
//     host a bounded rate however large N is;
//   - the shared rate drops while the backend is degraded, when reconnect storms
//     are what is generating the tests.
//
// A refused echo is dropped. The tester then sees a slower link on this one
// connection, which is the truth about a connection that is being rationed.
const (
	// one test measured on a live host moved about 1 MiB per 12 s each way; this
	// leaves an order of magnitude of headroom
	speedEchoSessionMaxBytes = 8 << 20

	// shared sustained rate (bytes/s) and burst (bytes) across all connections
	speedEchoRate  = 2 << 20
	speedEchoBurst = 16 << 20

	// shared sustained rate while the backend is degraded
	speedEchoDegradedRate = 256 << 10
)

var speedEchoCounters struct {
	tests   atomic.Int64
	echoed  atomic.Int64
	dropped atomic.Int64
}

// SpeedEchoStats is a snapshot of the process-wide speed test counters.
type SpeedEchoStats struct {
	Tests        int64
	EchoedBytes  int64
	DroppedBytes int64
}

func SpeedEchoStatsNow() SpeedEchoStats {
	return SpeedEchoStats{
		Tests:        speedEchoCounters.tests.Load(),
		EchoedBytes:  speedEchoCounters.echoed.Load(),
		DroppedBytes: speedEchoCounters.dropped.Load(),
	}
}

// speedEchoBudget is the token bucket all speed tests in the process share.
type speedEchoBudget struct {
	now      func() time.Time
	degraded func() bool

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newSpeedEchoBudget(now func() time.Time) *speedEchoBudget {
	return &speedEchoBudget{now: now, degraded: BackendDegraded, tokens: speedEchoBurst, last: now()}
}

var speedEcho = newSpeedEchoBudget(time.Now)

func (self *speedEchoBudget) take(byteCount int) bool {
	rate := float64(speedEchoRate)
	if self.degraded != nil && self.degraded() {
		rate = speedEchoDegradedRate
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	now := self.now()
	if elapsed := now.Sub(self.last); 0 < elapsed {
		self.tokens = min(float64(speedEchoBurst), self.tokens+rate*elapsed.Seconds())
	}
	self.last = now
	if float64(byteCount) <= self.tokens {
		self.tokens -= float64(byteCount)
		return true
	}
	return false
}

// speedEchoSession is one connection's speed test.
type speedEchoSession struct {
	budget *speedEchoBudget
	echoed int
}

func (self *speedEchoBudget) newSession() *speedEchoSession {
	speedEchoCounters.tests.Add(1)
	return &speedEchoSession{budget: self}
}

// allow reports whether to echo a frame of this size, charging both the session
// cap and the shared bucket.
func (self *speedEchoSession) allow(byteCount int) bool {
	if speedEchoSessionMaxBytes < self.echoed+byteCount {
		return false
	}
	if !self.budget.take(byteCount) {
		return false
	}
	self.echoed += byteCount
	return true
}

// allowInMode is allow, plus the accounting and the mode: only enforce mode
// refuses, so shadow and off behave as before while still counting.
func (self *speedEchoSession) allowInMode(byteCount int) bool {
	if self.allow(byteCount) {
		speedEchoCounters.echoed.Add(int64(byteCount))
		return true
	}
	speedEchoCounters.dropped.Add(int64(byteCount))
	if currentGovernMode() != governEnforce {
		return true
	}
	return false
}
