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
	// the backend also visibly failed some attempts, as it did in the incident
	record(g, connectUpgradeFail, govMinHardFailures)
	s := g.snapshot()
	if !s.Degraded {
		t.Fatalf("100 connects that all died young, beside visible platform failures, must read as degraded: %+v", s)
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

// A proxy that accepts the connection and never answers fails the same way a
// dead proxy always has. It says nothing about the backend, so a poor proxy
// list on a healthy backend must not read as an outage.
func TestGovernor_SilentTimeoutsAreAmbiguous(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectTimeoutNoReply, 900)
	record(g, connectOK, 100)
	if g.snapshot().Degraded {
		t.Fatal("silent timeouts are proxy trouble, not evidence against the platform")
	}
	only, _ := newTestGovernor()
	record(only, connectTimeoutNoReply, 900)
	if only.snapshot().Degraded {
		t.Fatal("nothing but silent timeouts is no evidence at all")
	}
}

func TestGovernor_TlsAndOtherFailuresAreAmbiguous(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectTlsFail, 400)
	record(g, connectOtherFail, 400)
	record(g, connectOK, 100)
	if g.snapshot().Degraded {
		t.Fatal("tls and unclassified failures can be a proxy tampering with the connection")
	}
}

// The platform answered and said no: that is the backend talking.
func TestGovernor_RejectedUpgradesCount(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectUpgradeFail, 100)
	if !g.snapshot().Degraded {
		t.Fatal("a flood of rejected upgrades is the platform failing")
	}
}

// A poor proxy list mixed with a backend that is working: the connects that do
// happen are good, everything else is ambiguous, so the verdict is healthy.
func TestGovernor_PoorProxyListOnAHealthyBackendIsHealthy(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectSocksFail, 500)
	record(g, connectTimeoutNoReply, 300)
	record(g, connectTlsFail, 50)
	record(g, connectOK, 150)
	record(g, connectTimeoutPlatform, 10)
	if g.snapshot().Degraded {
		t.Fatalf("a mostly dead proxy list must not look like an outage: %+v", g.snapshot())
	}
}

// The label must say what the governor is actually doing. In enforce mode the
// line used to claim "observing only", which sent an operator looking for a
// shadow-mode setting that was not on.
func TestGovernorReport_NamesTheModeHonestly(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectTimeoutPlatform, 100)

	enforcing := &govReporter{mode: func() governMode { return governEnforce }, rate: func() float64 { return 8 }, waiting: func() int { return 321 }}
	line, ok := enforcing.next(g.snapshot())
	if !ok || !strings.Contains(line, "enforcing") || strings.Contains(line, "observing only") {
		t.Fatalf("enforce mode must say it is enforcing, got %q", line)
	}
	for _, want := range []string{"mode=enforce", "rate=8.0/s", "waiting=321"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}

	shadow := &govReporter{mode: func() governMode { return governShadow }}
	line, _ = shadow.next(g.snapshot())
	if !strings.Contains(line, "observing only") || !strings.Contains(line, "mode=shadow") {
		t.Fatalf("shadow mode must say it only observes, got %q", line)
	}
}

func TestGovernorStatusNow_ReportsModeRateAndWaiting(t *testing.T) {
	defer setGovernModeForTest(governEnforce)()
	connectBudget.setRate(25)
	defer connectBudget.setRate(budgetCeilingRate)

	s := GovernorStatusNow()
	if s.Mode != "enforce" {
		t.Fatalf("mode = %q, want enforce", s.Mode)
	}
	if s.BudgetRate != 25 {
		t.Fatalf("budget rate = %v, want 25", s.BudgetRate)
	}
	if s.Verdict != "ok" && s.Verdict != "degraded" {
		t.Fatalf("verdict = %q", s.Verdict)
	}
	restore := setGovernModeForTest(governOff)
	defer restore()
	if got := GovernorStatusNow().Mode; got != "off" {
		t.Fatalf("mode = %q, want off", got)
	}
}

// Sessions that die young with NO visible platform failure beside them are as
// likely to be proxies resetting connections (rotating residential IPs, a flaky
// list) as a struggling backend. The flap guard already slows such proxies one by
// one; the fleet-wide verdict, which throttles every reconnect, needs the
// platform itself to have visibly failed some attempts.
func TestGovernor_FlappingProxiesAloneAreNotABackendOutage(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectOK, 200)
	for i := 0; i < 200; i++ {
		g.recordEarlyDeath()
	}
	if g.snapshot().Degraded {
		t.Fatal("proxy-side flapping with no platform failure must not throttle the fleet")
	}
}

func TestGovernor_HardFailuresAreTimeoutsAfterTheAnswerAndRejectedUpgrades(t *testing.T) {
	g, _ := newTestGovernor()
	record(g, connectTimeoutPlatform, 2)
	record(g, connectUpgradeFail, 1)
	record(g, connectTimeoutNoReply, 50)
	record(g, connectSocksFail, 50)
	if got := g.snapshot().HardFailures; got != 3 {
		t.Fatalf("hard failures = %d, want 3 (silent timeouts and socks failures are not hard)", got)
	}
}

// A SOCKS handshake that timed out is the proxy's failure, however many bytes the
// attempt's earlier dials moved. These are the real lines from a 1,100-proxy host
// whose backend was healthy: the "last:" cause of every flagged timeout.
func TestClassify_SocksHandshakeTimeoutsAreProxyFailuresNotPlatformOnes(t *testing.T) {
	stats := &attemptStats{}
	stats.rx.Store(6000) // earlier dials in the same attempt moved plenty
	for _, msg := range []string{
		"socks connect tcp 45.74.31.41:10062->65.49.70.70:443: read tcp 66.187.7.83:4806->45.74.31.41:10062: i/o timeout",
		"socks connect tcp 156.225.20.83:1080->65.49.70.70:443: read tcp 66.187.7.83:4806->156.225.20.83:1080: i/o timeout",
		"read tcp 66.187.7.83:4806->156.225.20.83:1080: i/o timeout (proxy 156.225.20.83:1080 unreachable)",
	} {
		err := &dialTimeoutError{failures: 2, rounds: 1, last: errors.New(msg)}
		if got := classifyConnectError(err, stats); got != connectSocksFail {
			t.Errorf("%q classified %v, want socks", msg, got)
		}
	}
	// a timeout waiting on the platform itself, through an established tunnel, still counts
	err := &dialTimeoutError{failures: 1, rounds: 1, last: errors.New("read tcp 66.187.7.83:4806->45.74.31.41:10062: i/o timeout")}
	if got := classifyConnectError(err, stats); got != connectTimeoutPlatform {
		t.Errorf("a post-handshake timeout classified %v, want timeout_platform", got)
	}
}
