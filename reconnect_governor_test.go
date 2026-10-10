package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func newTestGovernor() (*connectGovernor, *time.Time) {
	now := time.Unix(1_000_000, 0)
	g := newConnectGovernor(func() time.Time { return now })
	return g, &now
}

func record(g *connectGovernor, class connectClass, n int) {
	for i := 0; i < n; i++ {
		g.recordAttempt(class, nil)
	}
}

func TestClassifyConnectError(t *testing.T) {
	reached := &attemptStats{}
	reached.rx.Store(platformReachedBytes + 1)
	silent := &attemptStats{}

	cases := []struct {
		name string
		err  error
		st   *attemptStats
		want connectClass
	}{
		{"nil error is a connect", nil, nil, connectOK},
		{"local teardown is ignored", context.Canceled, nil, connectCanceled},
		{"giving up after hearing the platform", errors.New("Timeout."), reached, connectTimeoutPlatform},
		{"giving up without hearing anything", errors.New("Timeout."), silent, connectTimeoutNoReply},
		{"giving up with no stats at all", errors.New("Timeout."), nil, connectTimeoutNoReply},
		{"deadline exceeded after hearing the platform", context.DeadlineExceeded, reached, connectTimeoutPlatform},
		{"socks failure", errors.New("socks connect tcp 1.2.3.4:1081->api: connection refused"), silent, connectSocksFail},
		{"dial failure", errors.New("dial tcp 1.2.3.4:1081: connect: no route to host"), silent, connectSocksFail},
		{"tls failure", errors.New("tls: handshake failure"), reached, connectTlsFail},
		{"certificate failure", errors.New("x509: certificate signed by unknown authority"), reached, connectTlsFail},
		{"upgrade rejected", errors.New("websocket: bad handshake"), reached, connectUpgradeFail},
		{"something unforeseen", errors.New("kaboom"), silent, connectOtherFail},
	}
	for _, c := range cases {
		if got := classifyConnectError(c.err, c.st); got != c.want {
			t.Errorf("%s: classified %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGovernor_CountsAndExpires(t *testing.T) {
	g, now := newTestGovernor()
	record(g, connectOK, 3)
	record(g, connectTimeoutPlatform, 7)
	if s := g.snapshot(); s.Attempts != 10 || s.OK != 3 {
		t.Fatalf("got attempts=%d ok=%d, want 10 and 3", s.Attempts, s.OK)
	}
	*now = now.Add(govWindow + time.Second)
	if s := g.snapshot(); s.Attempts != 0 {
		t.Fatalf("attempts older than the window must expire, got %d", s.Attempts)
	}
}

func TestGovernor_HealthyBackendIsNotDegraded(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectOK, 95)
	record(g, connectTimeoutPlatform, 5)
	if g.snapshot().Degraded {
		t.Fatal("95% of attempts connecting is a healthy backend")
	}
}

func TestGovernor_AllFailuresAreDegraded(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectTimeoutPlatform, 100)
	if !g.snapshot().Degraded {
		t.Fatal("100 straight platform timeouts must read as degraded")
	}
}

// The incident: the backend accepts the websocket and the session dies seconds
// later. Every connect "succeeds", so a plain success ratio never trips.
func TestGovernor_ConnectsThatDieYoungCountAsFailures(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectOK, 100)
	for i := 0; i < 100; i++ {
		g.recordEarlyDeath()
	}
	s := g.snapshot()
	if !s.Degraded {
		t.Fatalf("100 connects that all died young must read as degraded: %+v", s)
	}
	if s.Early != 100 {
		t.Fatalf("early deaths = %d, want 100", s.Early)
	}
}

// Junk proxies fail forever whatever the backend does. They must not push a
// healthy node over the line.
func TestGovernor_DeadSocksProxiesDoNotTrip(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectSocksFail, 400)
	record(g, connectOK, 600)
	if g.snapshot().Degraded {
		t.Fatal("40% dead socks proxies is a proxy-list problem, not a backend outage")
	}
}

func TestGovernor_CanceledAttemptsAreIgnored(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectCanceled, 500)
	s := g.snapshot()
	if s.Attempts != 0 || s.Degraded {
		t.Fatalf("local teardown must not count as an attempt: %+v", s)
	}
}

func TestGovernor_TooFewAttemptsGiveNoVerdict(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectTimeoutPlatform, govMinAttempts-1)
	if g.snapshot().Degraded {
		t.Fatal("a handful of failures is noise, not an outage")
	}
}

func TestGovernor_WouldHoldIsEverythingOverTheBudget(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectTimeoutPlatform, 4000)
	s := g.snapshot()
	if want := int64(4000) - govShadowBudgetPerSecond*int64(govWindow/time.Second); s.WouldHold != want {
		t.Fatalf("would hold %d, want %d", s.WouldHold, want)
	}
	healthy, _ := newTestGovernor()
	record(healthy, connectOK, 4000)
	if healthy.snapshot().WouldHold != 0 {
		t.Fatal("a healthy backend must never report held attempts")
	}
}

func TestGovernor_AttributesBytesAndDialsToFailedAttempts(t *testing.T) {
	g, _ := newTestGovernor()
	failed := &attemptStats{}
	failed.dials.Store(4)
	failed.rx.Store(30_000)
	failed.tx.Store(10_000)
	ok := &attemptStats{}
	ok.dials.Store(1)
	ok.rx.Store(5_000)
	ok.tx.Store(2_000)

	g.recordAttempt(connectTimeoutPlatform, failed)
	g.recordAttempt(connectOK, ok)

	s := g.snapshot()
	if s.Dials != 5 || s.Bytes != 47_000 {
		t.Fatalf("dials=%d bytes=%d, want 5 and 47000", s.Dials, s.Bytes)
	}
	if s.FailedBytes != 40_000 {
		t.Fatalf("failed bytes = %d, want 40000", s.FailedBytes)
	}
}

func TestAttemptStats_ThroughContextAndConn(t *testing.T) {
	st := &attemptStats{}
	ctx := withAttemptStats(context.Background(), st)
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if attemptStatsFrom(child) != st {
		t.Fatal("stats must survive the derived contexts the dial path creates")
	}
	if attemptStatsFrom(context.Background()) != nil {
		t.Fatal("no stats on a plain context")
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	tc := &trackedConn{Conn: a, attempt: st}
	go func() {
		buf := make([]byte, 100)
		n, _ := b.Read(buf)
		b.Write(buf[:n/2])
	}()
	if _, err := tc.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Read(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if st.tx.Load() != 100 || st.rx.Load() != 50 {
		t.Fatalf("tx=%d rx=%d, want 100 and 50", st.tx.Load(), st.rx.Load())
	}
}

func TestGovernorReport_LogsTransitionsAndHeartbeat(t *testing.T) {
	g, _ := newTestGovernor()
	r := &govReporter{}

	record(g, connectOK, 100)
	if line, ok := r.next(g.snapshot()); ok {
		t.Fatalf("a healthy first tick should stay quiet, got %q", line)
	}

	record(g, connectTimeoutPlatform, 900)
	line, ok := r.next(g.snapshot())
	if !ok || !strings.Contains(line, "DEGRADED") {
		t.Fatalf("entering degraded must log, got %q ok=%v", line, ok)
	}
	if line, ok := r.next(g.snapshot()); !ok || !strings.Contains(line, "DEGRADED") {
		t.Fatalf("a degraded tick keeps logging, got %q ok=%v", line, ok)
	}

	healthy, _ := newTestGovernor()
	record(healthy, connectOK, 100)
	line, ok = r.next(healthy.snapshot())
	if !ok || !strings.Contains(line, "recovered") {
		t.Fatalf("leaving degraded must log, got %q ok=%v", line, ok)
	}
	for i := 0; i < govHeartbeatTicks-1; i++ {
		if _, ok := r.next(healthy.snapshot()); ok {
			t.Fatalf("healthy ticks stay quiet until the heartbeat (tick %d)", i)
		}
	}
	if line, ok := r.next(healthy.snapshot()); !ok || !strings.Contains(line, "ok") {
		t.Fatalf("healthy heartbeat missing, got %q ok=%v", line, ok)
	}
	_ = fmt.Sprint
}
