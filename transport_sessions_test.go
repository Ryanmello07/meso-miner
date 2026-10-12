package connect

import (
	"context"
	"testing"
	"time"
)

func sessionTotals() map[string]int64 {
	stats := SessionEndStatsNow()
	totals := map[string]int64{}
	for reason, cumulative := range stats.Cumulative {
		totals[reason] = cumulative[len(cumulative)-1]
	}
	return totals
}

// A session that ends is counted under why it ended and how long it lived.
func TestSessionEnd_CountsByReasonAndLifetime(t *testing.T) {
	before := sessionTotals()
	beforeStats := SessionEndStatsNow()

	alive := context.Background()
	recordSessionEnd(alive, 20*time.Second, false) // remote, <=30s
	recordSessionEnd(alive, 20*time.Minute, false) // remote, <=30m
	recordSessionEnd(alive, 5*time.Second, true)   // client limit, <=10s
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	recordSessionEnd(cancelled, 2*time.Hour, false) // local, <=6h, even if it was also young

	after := sessionTotals()
	if after["remote"]-before["remote"] != 2 || after["client_limit"]-before["client_limit"] != 1 || after["local"]-before["local"] != 1 {
		t.Fatalf("totals %v -> %v", before, after)
	}
	afterStats := SessionEndStatsNow()
	bound := func(seconds float64) int {
		for i, b := range afterStats.BoundsSeconds {
			if b == seconds {
				return i
			}
		}
		t.Fatalf("no bucket for %gs", seconds)
		return -1
	}
	remote30s, remote30m := bound(30), bound(1800)
	if afterStats.Cumulative["remote"][remote30s]-beforeStats.Cumulative["remote"][remote30s] != 1 {
		t.Fatal("the 20s session must be in the <=30s bucket")
	}
	if afterStats.Cumulative["remote"][remote30m]-beforeStats.Cumulative["remote"][remote30m] != 2 {
		t.Fatal("both remote sessions must be in the <=30m bucket (cumulative)")
	}
}

// Only the main transport is counted, so an auxiliary H3 beside H1 does not
// double count one node's churn.
func TestSessionEnd_AuxiliaryTransportIsNotCounted(t *testing.T) {
	before := sessionTotals()
	flaps := newFlapGuard(time.Minute, time.Second)
	sessionEndWait(context.Background(), flaps, 5*time.Second, false, "aux", NewNoopLogger(), false)
	if after := sessionTotals(); after["remote"] != before["remote"] {
		t.Fatalf("an auxiliary session was counted: %v -> %v", before, after)
	}
	sessionEndWait(context.Background(), flaps, 5*time.Second, false, "main", NewNoopLogger(), true)
	if after := sessionTotals(); after["remote"]-before["remote"] != 1 {
		t.Fatalf("the main transport's session was not counted: %v -> %v", before, after)
	}
}
