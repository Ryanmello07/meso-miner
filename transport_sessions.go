package connect

import (
	"context"
	"sync/atomic"
	"time"
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
