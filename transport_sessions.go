package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// How long platform transport sessions live and why they end.
//
// Every new session is a new connection to the platform, and the platform runs
// latency and speed tests on each one: about 1.75 MB of traffic per connect. So a
// node's byte cost is set by how often its sessions end, and nothing recorded
// why they end. These counters give each ended session a lifetime bucket and a
// coarse reason.

type sessionEndReason int

const (
	// our own teardown: a reload, the reaper, shutdown
	sessionEndLocal sessionEndReason = iota
	// the platform enforcing the client limit
	sessionEndClientLimit
	// the platform or the network ended it: a remote close, a read or write
	// error, a missed keepalive
	sessionEndRemote
	sessionEndReasonCount
)

var sessionEndReasonNames = [sessionEndReasonCount]string{"local", "client_limit", "remote"}

// upper bounds of the session lifetime histogram; the last bucket is longer
var sessionLifetimeBounds = []time.Duration{
	10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute,
	15 * time.Minute, 30 * time.Minute, time.Hour, 6 * time.Hour,
}

var sessionLifetimeCounts [sessionEndReasonCount][9]atomic.Int64

func recordSessionEnd(ctx context.Context, lived time.Duration, clientLimitClosed bool) {
	reason := sessionEndRemote
	switch {
	case ctx.Err() != nil:
		reason = sessionEndLocal
	case clientLimitClosed:
		reason = sessionEndClientLimit
	}
	if reason == sessionEndRemote {
		remoteCloseBursts.note(time.Now())
	}
	for i, bound := range sessionLifetimeBounds {
		if lived <= bound {
			sessionLifetimeCounts[reason][i].Add(1)
			return
		}
	}
	sessionLifetimeCounts[reason][len(sessionLifetimeBounds)].Add(1)
}

// SessionEndStats is, per end reason, the cumulative count of ended sessions by
// lifetime bucket (bounds in seconds, +Inf last).
type SessionEndStats struct {
	BoundsSeconds []float64
	// Cumulative is keyed by SessionEndReasons.
	Cumulative map[string][]int64
}

func SessionEndStatsNow() SessionEndStats {
	stats := SessionEndStats{Cumulative: map[string][]int64{}}
	for _, bound := range sessionLifetimeBounds {
		stats.BoundsSeconds = append(stats.BoundsSeconds, bound.Seconds())
	}
	for reason, name := range sessionEndReasonNames {
		total := int64(0)
		cumulative := make([]int64, 0, len(sessionLifetimeCounts[reason]))
		for i := range sessionLifetimeCounts[reason] {
			total += sessionLifetimeCounts[reason][i].Load()
			cumulative = append(cumulative, total)
		}
		stats.Cumulative[name] = cumulative
	}
	return stats
}

func SessionEndReasons() []string { return append([]string(nil), sessionEndReasonNames[:]...) }

// Bursts of simultaneous remote closes.
//
// When the platform cycles its connection servers (a rollout) every provider
// transport is dropped at once and redials, and each redial earns a speed test.
// That shows as many sessions ending "remote" within seconds. A burst is a
// window of remoteCloseBurstWindow with at least remoteCloseBurstMin remote
// closes; the worst one seen is kept and each is logged, at most once a minute.

const (
	remoteCloseBurstWindow = 10 * time.Second
	remoteCloseBurstMin    = 50
	remoteCloseLogEvery    = time.Minute
)

type remoteCloseBurstDetector struct {
	mutex     sync.Mutex
	window    []time.Time
	bursts    int64
	maxBurst  int64
	lastLogAt time.Time
	emit      func(string)
}

var remoteCloseBursts = &remoteCloseBurstDetector{emit: emitGovernLine}

func (self *remoteCloseBurstDetector) note(now time.Time) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	cutoff := now.Add(-remoteCloseBurstWindow)
	keep := 0
	for keep < len(self.window) && self.window[keep].Before(cutoff) {
		keep++
	}
	self.window = append(self.window[keep:], now)
	count := int64(len(self.window))
	if count < remoteCloseBurstMin {
		return
	}
	if count == remoteCloseBurstMin {
		self.bursts += 1
	}
	if self.maxBurst < count {
		self.maxBurst = count
	}
	if self.lastLogAt.IsZero() || remoteCloseLogEvery <= now.Sub(self.lastLogAt) {
		self.lastLogAt = now
		self.emit(fmt.Sprintf(
			"[t]mass disconnect: %d transports closed by the platform or network within %s; each redial costs about 1.75 MB of platform speed tests",
			count, remoteCloseBurstWindow,
		))
	}
}

// RemoteCloseBurstsNow returns how many bursts were seen and the largest one.
func RemoteCloseBurstsNow() (bursts int64, largest int64) {
	remoteCloseBursts.mutex.Lock()
	defer remoteCloseBursts.mutex.Unlock()
	return remoteCloseBursts.bursts, remoteCloseBursts.maxBurst
}

// Why transports close.
//
// The lifetime histogram says how long sessions live; this says what ended them,
// from the error the read or write loop saw: a websocket close code from the
// platform, a read timeout, a reset, an EOF, or our own teardown. The set of
// causes is small and fixed so the metric cannot grow without bound.

var transportCloseCauseNames = []string{
	"ws_normal", "ws_going_away", "ws_abnormal", "ws_policy", "ws_internal", "ws_app", "ws_other",
	"timeout", "reset", "eof", "closed_local", "other",
}

var transportCloseCauseCounts [2][2][12]atomic.Int64 // [h1=0,h3=1][read=0,write=1][cause]

func classifyTransportCloseCause(err error) int {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch {
		case closeErr.Code == websocket.CloseNormalClosure:
			return 0
		case closeErr.Code == websocket.CloseGoingAway:
			return 1
		case closeErr.Code == websocket.CloseAbnormalClosure:
			return 2
		case closeErr.Code == websocket.ClosePolicyViolation:
			return 3
		case closeErr.Code == websocket.CloseInternalServerErr:
			return 4
		case 4000 <= closeErr.Code:
			return 5
		}
		return 6
	}
	var netErr net.Error
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return 7
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return 8
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return 9
	case errors.Is(err, net.ErrClosed), errors.Is(err, context.Canceled):
		return 10
	}
	return 11
}

func recordTransportCloseCause(mode string, side string, err error) {
	if err == nil {
		return
	}
	modeIndex, sideIndex := 0, 0
	if mode == "h3" {
		modeIndex = 1
	}
	if side == "write" {
		sideIndex = 1
	}
	transportCloseCauseCounts[modeIndex][sideIndex][classifyTransportCloseCause(err)].Add(1)
}

// TransportCloseCause is the count of transport read or write errors of one kind.
type TransportCloseCause struct {
	Mode  string
	Side  string
	Cause string
	Count int64
}

func TransportCloseCausesNow() []TransportCloseCause {
	var causes []TransportCloseCause
	for mode, modeName := range []string{"h1", "h3"} {
		for side, sideName := range []string{"read", "write"} {
			for cause, causeName := range transportCloseCauseNames {
				causes = append(causes, TransportCloseCause{
					Mode: modeName, Side: sideName, Cause: causeName,
					Count: transportCloseCauseCounts[mode][side][cause].Load(),
				})
			}
		}
	}
	return causes
}
