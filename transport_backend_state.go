package connect

import (
	"sync/atomic"
	"time"
)

// Fork-only error throttles. The auth and write throttles, and the backend
// degraded state (isBackendDegraded, noteBackendFailure/Success), are owned by
// transport.go now (ported from upstream).
var (
	selectErrThrottle = newLogThrottle(time.Minute)
	flapErrThrottle   = newLogThrottle(time.Minute)
)

// activeProxyConnections counts proxy transports that are currently registered
// with the route manager (i.e. authenticated and live on the platform). It is
// incremented when a H1 transport registers (setRegistered) and decremented when
// it is torn down. ActiveProxyConnections() exposes it for the [health]
// heartbeat as proxies=N.
var activeProxyConnections int64

func ActiveProxyConnections() int64 {
	return atomic.LoadInt64(&activeProxyConnections)
}

// shouldLogSelectErr reports whether a `[net][s]select` error line may be
// emitted now, with the count of lines suppressed since the previous allowed
// one. The second value is only meaningful when the first is true, and reading
// it resets the counter.
func shouldLogSelectErr() (bool, int64) { return selectErrThrottle.Allow(time.Now()) }

// IsBackendDegraded is the exported form for use by the provider binary.
func IsBackendDegraded() bool {
	return isBackendDegraded()
}
