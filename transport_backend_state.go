package connect

import (
	"sync"
	"sync/atomic"
	"time"
)

// One throttle per high-volume error class. Each emits at most one line per
// minute and counts suppressions in between (see logThrottle).
var (
	authErrThrottle   = newLogThrottle(time.Minute)
	selectErrThrottle = newLogThrottle(time.Minute)
	writeErrThrottle  = newLogThrottle(time.Minute)
	flapErrThrottle   = newLogThrottle(time.Minute)
)

// backendFailState is the backend health signal: a count of consecutive
// failures and the time of the most recent one. The two are one value, not two.
// isBackendDegraded needs them from the SAME transition — read separately, a
// stale-streak reset landing between the two loads pairs the old count with the
// new timestamp, and the backend reads as degraded on the strength of a single
// recent failure. Storing an immutable snapshot behind atomic.Pointer makes
// that pairing structural, and keeps the read lock-free: isBackendDegraded runs
// per contract creation and per window resize, so a mutex on the read path
// would put process-wide contention on a hot read to close a race that only
// writers can create.
type backendFailState struct {
	// fails counts backend failures (auth or OOB) since the last success. Any
	// successful connect or OOB result resets it to 0. A real platform outage
	// drives this up fast because every attempt fails with nothing to reset it;
	// isolated transient timeouts never accumulate because an interleaved
	// success clears the count. isBackendDegraded() requires this to cross a
	// threshold so one or two stray failures are not mistaken for an outage.
	fails int64
	// lastNano is the time of the most recent failure, in unix nanos. Updated
	// on every failure, not rate-limited. Used by isBackendDegraded() as the
	// recency guard. Zero means no outstanding failures.
	lastNano int64
}

// backendFail holds the current backend health signal. Never nil after init,
// so readers do not have to nil-check.
//
// This is process-wide rather than per-Client on purpose: "is the control API
// reachable from this host" is a property of the host, not of any one client.
// A host running many proxies gets a stronger signal from sharing the state,
// because a success on any one clears it — so a single misbehaving proxy
// cannot trip the threshold on its own.
//
// The known limit of that framing: a process talking to MULTIPLE platform urls
// (separate network spaces) shares one signal across them, so a dead custom
// endpoint can gate a healthy one. Accepted for now — the fleet runs one
// platform per process — and keying this state by platform url is the upgrade
// path if that changes.
var backendFail atomic.Pointer[backendFailState]

// backendFailMu serializes writers. The pointer store is atomic, but building
// the next state reads the current one first, and that read-modify-write has to
// be exclusive or two concurrent failures both read the same count and one
// increment is lost.
var backendFailMu sync.Mutex

func init() {
	backendFail.Store(&backendFailState{})
}

// activeProxyConnections counts proxy transports that are currently registered
// with the route manager (i.e. authenticated and live on the platform). It is
// incremented after UpdateTransport in runH1/runH3 and decremented when the
// transport is torn down. ActiveProxyConnections() exposes it for the [health]
// heartbeat as proxies=N.
var activeProxyConnections int64

func ActiveProxyConnections() int64 {
	return atomic.LoadInt64(&activeProxyConnections)
}

// backendDegradedFailThreshold is the number of consecutive backend failures
// (with no intervening success) required before the backend is considered
// degraded. Set above the level of normal transient churn.
const backendDegradedFailThreshold = 3

// backendDegradedWindow is how recent the last failure must be for the counter
// to be trusted. Comfortably larger than the 60s reconnect-backoff cap so a real
// outage's retry attempts always read as recent, while a stale count left by an
// old blip on an idle provider does not.
const backendDegradedWindow = 2 * time.Minute

// shouldLogAuthErr reports whether a `[t]auth error` line may be emitted now.
//
// The second return is the number of lines SUPPRESSED since the previous
// allowed one, not a throttle state or an interval: it is meant to be printed
// as the "(N suppressed)" tail so a single surviving line still carries the
// volume behind it. It is only meaningful when the first return is true, and
// reading it resets the counter — so each suppressed line is attributed to
// exactly one emitted line.
func shouldLogAuthErr() (bool, int64) { return authErrThrottle.Allow(time.Now()) }

// shouldLogSelectErr reports whether a `[net][s]select` error line may be
// emitted now, with the count of lines suppressed since the previous allowed
// one. See shouldLogAuthErr for the contract on that second value.
func shouldLogSelectErr() (bool, int64) { return selectErrThrottle.Allow(time.Now()) }

// shouldLogWriteErr reports whether a transport write error line may be emitted
// now, with the count of lines suppressed since the previous allowed one. See
// shouldLogAuthErr for the contract on that second value.
func shouldLogWriteErr() (bool, int64) { return writeErrThrottle.Allow(time.Now()) }

// isBackendDegraded returns true when backend failures have accumulated past
// the threshold with no intervening success and the last failure is recent.
// This distinguishes a sustained, broad outage (every attempt failing) from the
// isolated single-connection timeouts that are normal churn on a busy provider.
//
// During an outage where the transport stays connected (the control API down,
// the websocket alive), auth never re-runs and the gated CreateContract is the
// only other success source — so nothing can SUCCEED to clear the state.
// Recovery then rides the recency window instead: after backendDegradedWindow
// without failures this reads false, the sequences that tick before three
// fresh failures land probe the backend, and either one succeeds (clearing the
// state) or the gate re-trips. The steady state of a long OOB-only outage is
// therefore a bounded probe burst every ~backendDegradedWindow, not a latched
// stop — which is also what makes recovery need no timer of its own.
func isBackendDegraded() bool {
	// one load: the count and the timestamp are guaranteed to come from the
	// same transition, so no interleaving writer can pair a stale count with a
	// fresh timestamp under the reader.
	state := backendFail.Load()
	if state.fails < backendDegradedFailThreshold {
		return false
	}
	return time.Now().UnixNano()-state.lastNano < int64(backendDegradedWindow)
}

// IsBackendDegraded is the exported form for use by the provider binary.
func IsBackendDegraded() bool {
	return isBackendDegraded()
}

// noteBackendFailure records a failed backend round-trip (auth or contract OOB).
//
// A streak older than backendDegradedWindow is discarded rather than extended.
// Without that, an idle provider that saw a few failures long ago and simply
// stopped retrying would carry the old count forward: the next single failure
// would push the total past the threshold with a fresh timestamp, and the
// backend would read as degraded on the strength of one recent failure. The
// threshold means "consecutive failures within the window", so a gap that
// invalidates the streak for isBackendDegraded must also reset it here.
func noteBackendFailure() {
	now := time.Now().UnixNano()

	backendFailMu.Lock()
	defer backendFailMu.Unlock()

	state := backendFail.Load()
	fails := int64(1)
	if state.lastNano != 0 && now-state.lastNano < int64(backendDegradedWindow) {
		fails = state.fails + 1
	}
	backendFail.Store(&backendFailState{fails: fails, lastNano: now})
}

// noteBackendSuccess clears the recorded backend failure state after a
// successful auth or OOB round-trip.
//
// It takes backendFailMu so the clear cannot land in the middle of a
// concurrent noteBackendFailure's read-modify-write, which would otherwise
// resurrect the count it just cleared.
func noteBackendSuccess() {
	backendFailMu.Lock()
	defer backendFailMu.Unlock()

	backendFail.Store(&backendFailState{})
}
