package connect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The reconnect governor, in SHADOW mode.
//
// During a backend episode where the API answers but slowly, thousands of
// proxies each retry the platform transport on their own backoff and nothing
// adds the attempts up. The governor is the process-wide view that does: it
// classifies every connect attempt, attributes real dials and bytes to it, and
// decides over a sliding window whether the backend is degraded.
//
// Shadow mode means it only OBSERVES. It reports what a shared attempt budget
// would have held back (`would_hold`) and logs a `[govern]` line, so the
// thresholds and the true cost of a failed attempt can be calibrated on the
// fleet before anything is allowed to delay a connect. Nothing here gates,
// delays or cancels an attempt.

// govWindow is how far back the verdict looks.
const govWindow = 60 * time.Second

// govMinAttempts is the fewest platform-class attempts in the window that can
// produce a degraded verdict, so a handful of failures is never an outage.
const govMinAttempts = 20

// govDegradedRatio: below this share of good platform attempts the window is
// degraded. A connect that dies young is not good.
const govDegradedRatio = 0.2

// govShadowBudgetPerSecond is the attempt rate the future budget would allow
// while degraded. Shadow mode only uses it to report what would be held.
const govShadowBudgetPerSecond = 2

// platformReachedBytes separates a timeout after hearing the platform from a
// timeout in silence. A TLS server hello with its certificate chain is several
// KB, so more than one packet back means the TLS peer answered. It is a
// heuristic, validated by the shadow data itself.
const platformReachedBytes = 1500

// govHeartbeatTicks is how many quiet one-minute ticks pass between healthy
// `[govern]` heartbeat lines.
const govHeartbeatTicks = 5

type connectClass uint8

const (
	// connectOK: the platform transport connected and authenticated
	connectOK connectClass = iota
	// connectSocksFail: the proxy path failed before the platform answered. A
	// property of the proxy, not the backend; excluded from the verdict.
	connectSocksFail
	connectTlsFail
	connectUpgradeFail
	// connectTimeoutPlatform: gave up after the platform answered
	connectTimeoutPlatform
	// connectTimeoutNoReply: gave up without hearing the platform
	connectTimeoutNoReply
	connectOtherFail
	// connectCanceled: local teardown, not an attempt outcome
	connectCanceled

	connectClassCount
)

func (self connectClass) String() string {
	switch self {
	case connectOK:
		return "ok"
	case connectSocksFail:
		return "socks"
	case connectTlsFail:
		return "tls"
	case connectUpgradeFail:
		return "upgrade"
	case connectTimeoutPlatform:
		return "timeout_platform"
	case connectTimeoutNoReply:
		return "timeout_silent"
	case connectOtherFail:
		return "other"
	case connectCanceled:
		return "canceled"
	}
	return fmt.Sprintf("class(%d)", uint8(self))
}

// attemptStats counts what ONE connect attempt cost: how many proxy dials it
// opened (the dial fan-out) and how many bytes crossed those connections,
// handshakes of attempts that fail included. It travels in the context, so the
// dial path needs no signature change.
type attemptStats struct {
	dials atomic.Int64
	rx    atomic.Int64
	tx    atomic.Int64
}

type attemptStatsKey struct{}

func withAttemptStats(ctx context.Context, stats *attemptStats) context.Context {
	return context.WithValue(ctx, attemptStatsKey{}, stats)
}

func attemptStatsFrom(ctx context.Context) *attemptStats {
	stats, _ := ctx.Value(attemptStatsKey{}).(*attemptStats)
	return stats
}

// classifyConnectError buckets a connect error. stats may be nil.
func classifyConnectError(err error, stats *attemptStats) connectClass {
	if err == nil {
		return connectOK
	}
	if errors.Is(err, context.Canceled) {
		return connectCanceled
	}
	// a dial that gave up carries the real dial error; classify that, not the
	// timeout it ended as
	var dialErr *dialTimeoutError
	if errors.As(err, &dialErr) && dialErr.last != nil {
		return classifyConnectError(dialErr.last, stats)
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "bad handshake"), strings.Contains(msg, "websocket:"):
		return connectUpgradeFail
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"):
		return connectTlsFail
	case strings.Contains(msg, "timeout"), isTimeoutError(err):
		if stats != nil && platformReachedBytes <= stats.rx.Load() {
			return connectTimeoutPlatform
		}
		return connectTimeoutNoReply
	case strings.Contains(msg, "socks"), strings.Contains(msg, "dial tcp"),
		strings.Contains(msg, "connection refused"), strings.Contains(msg, "no route"),
		strings.Contains(msg, "proxy"), strings.Contains(msg, "unreachable"),
		strings.Contains(msg, "no such host"):
		return connectSocksFail
	}
	return connectOtherFail
}

type govBucket struct {
	sec         int64
	attempts    int64
	ok          int64
	early       int64
	classes     [connectClassCount]int64
	dials       int64
	bytes       int64
	failedBytes int64
}

// govSnapshot is the governor's view of the last govWindow.
type govSnapshot struct {
	Attempts         int64
	OK               int64
	Early            int64
	Classes          [connectClassCount]int64
	Dials            int64
	Bytes            int64
	FailedBytes      int64
	PlatformAttempts int64
	PlatformGood     int64
	Ratio            float64
	Degraded         bool
	WouldHold        int64
}

type connectGovernor struct {
	mu   sync.Mutex
	now  func() time.Time
	ring [int(govWindow / time.Second)]govBucket
}

func newConnectGovernor(now func() time.Time) *connectGovernor {
	return &connectGovernor{now: now}
}

// bucket returns the current second's bucket, zeroing it when it still holds an
// older lap of the ring. Caller holds mu.
func (self *connectGovernor) bucket() *govBucket {
	sec := self.now().Unix()
	b := &self.ring[int(sec%int64(len(self.ring)))]
	if b.sec != sec {
		*b = govBucket{sec: sec}
	}
	return b
}

// recordAttempt records one finished connect attempt. Local teardown is not an
// outcome and is dropped.
func (self *connectGovernor) recordAttempt(class connectClass, stats *attemptStats) {
	if class == connectCanceled {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	b := self.bucket()
	b.attempts += 1
	b.classes[class] += 1
	if class == connectOK {
		b.ok += 1
	}
	if stats != nil {
		bytes := stats.rx.Load() + stats.tx.Load()
		b.dials += stats.dials.Load()
		b.bytes += bytes
		if class != connectOK {
			b.failedBytes += bytes
		}
	}
}

// recordEarlyDeath records that a connect which counted as OK lost its session
// young. Such a connect is a failure in all but name: it resets the backoff and
// the shared failure streak, and it is what the incident was made of.
func (self *connectGovernor) recordEarlyDeath() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.bucket().early += 1
}

func (self *connectGovernor) snapshot() govSnapshot {
	self.mu.Lock()
	defer self.mu.Unlock()

	var s govSnapshot
	cutoff := self.now().Unix() - int64(len(self.ring))
	for i := range self.ring {
		b := &self.ring[i]
		if b.sec <= cutoff {
			continue
		}
		s.Attempts += b.attempts
		s.OK += b.ok
		s.Early += b.early
		for c := range b.classes {
			s.Classes[c] += b.classes[c]
		}
		s.Dials += b.dials
		s.Bytes += b.bytes
		s.FailedBytes += b.failedBytes
	}

	// a dead proxy fails whatever the backend does, so socks failures say
	// nothing about it; connects that died young are not good
	s.PlatformAttempts = s.Attempts - s.Classes[connectSocksFail]
	s.PlatformGood = max(0, s.OK-s.Early)
	if 0 < s.PlatformAttempts {
		s.Ratio = float64(s.PlatformGood) / float64(s.PlatformAttempts)
	}
	s.Degraded = govMinAttempts <= s.PlatformAttempts && s.Ratio < govDegradedRatio
	if s.Degraded {
		s.WouldHold = max(0, s.Attempts-govShadowBudgetPerSecond*int64(govWindow/time.Second))
	}
	return s
}

// govReporter turns the per-minute snapshots into `[govern]` lines: every
// transition and every degraded tick, plus a slow heartbeat while healthy.
type govReporter struct {
	degraded bool
	quiet    int
}

func (self *govReporter) next(s govSnapshot) (string, bool) {
	if s.Degraded {
		self.degraded = true
		self.quiet = 0
		return govLine("DEGRADED (shadow: observing only)", s), true
	}
	if self.degraded {
		self.degraded = false
		self.quiet = 0
		return govLine("recovered", s), true
	}
	if s.Attempts == 0 {
		return "", false
	}
	self.quiet += 1
	if govHeartbeatTicks <= self.quiet {
		self.quiet = 0
		return govLine("ok", s), true
	}
	return "", false
}

func govLine(state string, s govSnapshot) string {
	perAttemptDials, perAttemptKB := 0.0, 0.0
	if 0 < s.Attempts {
		perAttemptDials = float64(s.Dials) / float64(s.Attempts)
		perAttemptKB = float64(s.Bytes) / float64(s.Attempts) / 1024
	}
	return fmt.Sprintf(
		"[govern] %s window=%ds attempts=%d (%.1f/s) connected=%d died_young=%d good=%.0f%% socks=%d tls=%d upgrade=%d timeout_platform=%d timeout_silent=%d other=%d dials/attempt=%.1f KB/attempt=%.1f failed_KB=%d would_hold=%d",
		state, int(govWindow/time.Second), s.Attempts, float64(s.Attempts)/govWindow.Seconds(),
		s.OK, s.Early, 100*s.Ratio,
		s.Classes[connectSocksFail], s.Classes[connectTlsFail], s.Classes[connectUpgradeFail],
		s.Classes[connectTimeoutPlatform], s.Classes[connectTimeoutNoReply], s.Classes[connectOtherFail],
		perAttemptDials, perAttemptKB, s.FailedBytes/1024, s.WouldHold,
	)
}

// connectGov is the process-wide governor. "Is the platform reachable from this
// host" is a property of the host, not of one proxy, so every transport feeds
// the same one.
var connectGov = newConnectGovernor(time.Now)

var govLoopOnce sync.Once

// startGovernorLoop starts the once-a-minute reporter on first use.
func startGovernorLoop() {
	govLoopOnce.Do(func() {
		go func() {
			reporter := &govReporter{}
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				if line, ok := reporter.next(connectGov.snapshot()); ok {
					DefaultLogger().Infof("%s\n", line)
				}
			}
		}()
	})
}

// GovernorStatus is the exported view of the governor for the health heartbeat.
type GovernorStatus struct {
	Degraded  bool
	Attempts  int64
	Connected int64
	DiedYoung int64
	WouldHold int64
}

func GovernorStatusNow() GovernorStatus {
	s := connectGov.snapshot()
	return GovernorStatus{
		Degraded:  s.Degraded,
		Attempts:  s.Attempts,
		Connected: s.OK,
		DiedYoung: s.Early,
		WouldHold: s.WouldHold,
	}
}
