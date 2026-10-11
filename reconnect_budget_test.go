package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type budgetHarness struct {
	gov    *connectGovernor
	now    *time.Time
	budget *reconnectBudget
}

func newBudgetHarness() *budgetHarness {
	start := time.Unix(2_000_000, 0)
	now := start
	clock := func() time.Time { return now }
	gov := newConnectGovernor(clock)
	return &budgetHarness{gov: gov, now: &now, budget: newReconnectBudget(gov, clock)}
}

func (h *budgetHarness) advance(d time.Duration) {
	*h.now = h.now.Add(d)
}

// step advances the fake clock in the dispatcher's own tick size
func (h *budgetHarness) step(total time.Duration) {
	for elapsed := time.Duration(0); elapsed < total; elapsed += budgetTick {
		h.advance(budgetTick)
		h.budget.tick(*h.now)
	}
}

func (h *budgetHarness) attempts(class connectClass, n int) {
	for i := 0; i < n; i++ {
		h.gov.recordAttempt(class, nil)
	}
}

// waiter starts an admit in the background and reports when it is released
func waiter(ctx context.Context, b *reconnectBudget, direct bool) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- b.admit(ctx, direct) }()
	return out
}

func waitQueued(t *testing.T, b *reconnectBudget, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.queued() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d waiters queued", b.queued(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBudget_HealthyPathNeverQueues(t *testing.T) {
	h := newBudgetHarness()
	for i := 0; i < 1000; i++ {
		if !h.budget.admit(context.Background(), false) {
			t.Fatal("an open budget must admit immediately")
		}
	}
	if h.budget.queued() != 0 || h.budget.throttled() {
		t.Fatal("a healthy budget must hold nothing")
	}
}

func TestBudget_DirectPathIsExempt(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	if !h.budget.admit(context.Background(), true) {
		t.Fatal("the direct path must never wait on the budget")
	}
	if h.budget.queued() != 0 {
		t.Fatal("the direct path must not occupy the queue")
	}
}

func TestBudget_ThrottledReleasesAtTheRateInOrder(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	ctx := context.Background()
	var released []<-chan bool
	for i := 0; i < 10; i++ {
		released = append(released, waiter(ctx, h.budget, false))
		waitQueued(t, h.budget, i+1) // enqueue strictly in order
	}

	h.step(time.Second) // 2 per second
	time.Sleep(50 * time.Millisecond)

	got := 0
	for i, ch := range released {
		select {
		case ok := <-ch:
			if !ok {
				t.Fatalf("waiter %d was refused", i)
			}
			if i >= 2 {
				t.Fatalf("waiter %d released out of order", i)
			}
			got++
		default:
		}
	}
	if got != 2 {
		t.Fatalf("released %d in one second at 2/s, want 2", got)
	}
}

func TestBudget_CanceledWaiterConsumesNoToken(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	cctx, cancel := context.WithCancel(context.Background())
	first := waiter(cctx, h.budget, false)
	waitQueued(t, h.budget, 1)
	second := waiter(context.Background(), h.budget, false)
	waitQueued(t, h.budget, 2)

	cancel()
	if ok := <-first; ok {
		t.Fatal("a canceled waiter must be refused")
	}

	h.step(500 * time.Millisecond) // one token at 2/s
	select {
	case ok := <-second:
		if !ok {
			t.Fatal("the live waiter must be admitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the canceled waiter ate the token")
	}
}

func TestBudget_BackendFailingCutsTheRate(t *testing.T) {
	h := newBudgetHarness()
	h.attempts(connectTimeoutPlatform, 200)
	h.budget.control()
	if got := h.budget.currentRate(); got != budgetCeilingRate/budgetDecreaseFactor {
		t.Fatalf("rate after one bad interval = %v, want %v", got, budgetCeilingRate/budgetDecreaseFactor)
	}
	for i := 0; i < 10; i++ {
		h.attempts(connectTimeoutPlatform, 50)
		h.budget.control()
	}
	if got := h.budget.currentRate(); got != budgetFloorRate {
		t.Fatalf("rate never goes below the floor, got %v", got)
	}
}

func TestBudget_FewAttemptsDoNotCutTheRate(t *testing.T) {
	h := newBudgetHarness()
	h.attempts(connectTimeoutPlatform, budgetMinDecreaseAttempts-1)
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("a handful of failures is noise and must not throttle")
	}
}

func TestBudget_DeadProxiesDoNotCutTheRate(t *testing.T) {
	h := newBudgetHarness()
	h.attempts(connectSocksFail, 500)
	h.attempts(connectOK, 100)
	h.budget.control()
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatal("dead socks proxies are a proxy-list problem and must not throttle the fleet")
	}
}

func TestBudget_RecoveryRampsBackToOpen(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	for i := 0; i < 20 && h.budget.currentRate() < budgetCeilingRate; i++ {
		h.advance(budgetControlInterval)
		h.attempts(connectOK, 20)
		h.budget.control()
	}
	if h.budget.currentRate() != budgetCeilingRate {
		t.Fatalf("a healthy backend must ramp back to the ceiling, stuck at %v", h.budget.currentRate())
	}
	if h.budget.throttled() {
		t.Fatal("at the ceiling with an empty queue the budget is open again")
	}
}

// The reason the budget exists: when the backend heals, thousands of parked
// proxies must come back fast but never as one herd.
func TestBudget_ThousandsOfParkedProxiesComeBackWithinASecondOrTwoOfTheRamp(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	const parked = 4000
	ctx := context.Background()
	released := make(chan struct{}, parked)
	for i := 0; i < parked; i++ {
		go func() {
			if h.budget.admit(ctx, false) {
				released <- struct{}{}
			}
		}()
	}
	waitQueued(t, h.budget, parked)

	// the backend is healthy from now on
	perSecond := map[int64]int{}
	count := 0
	start := *h.now
	for count < parked && h.now.Sub(start) < 5*time.Minute {
		h.advance(budgetTick)
		if h.now.Sub(start)%budgetControlInterval == 0 {
			h.attempts(connectOK, 20)
			h.budget.control()
		}
		h.budget.tick(*h.now)
		time.Sleep(200 * time.Microsecond) // let released goroutines report
		for drained := false; !drained; {
			select {
			case <-released:
				count++
				perSecond[int64(h.now.Sub(start)/time.Second)]++
			default:
				drained = true
			}
		}
	}
	elapsed := h.now.Sub(start)
	if count != parked {
		t.Fatalf("only %d of %d parked proxies were released in %s", count, parked, elapsed)
	}
	if elapsed > 90*time.Second {
		t.Fatalf("4000 proxies took %s to come back, recovery must be about a minute", elapsed)
	}
	for sec, n := range perSecond {
		if float64(n) > budgetCeilingRate*1.2 {
			t.Fatalf("second %d released %d proxies, above the %v/s ceiling", sec, n, budgetCeilingRate)
		}
	}
}

func TestBudget_ThrottledSinceRateOrQueue(t *testing.T) {
	h := newBudgetHarness()
	if h.budget.throttled() {
		t.Fatal("fresh budget is open")
	}
	h.budget.setRate(budgetFloorRate)
	if !h.budget.throttled() {
		t.Fatal("a reduced rate is throttled")
	}
}

func newProxiedFlapTransport(t *testing.T, ctx context.Context, serverUrl string, settings *PlatformTransportSettings) *PlatformTransport {
	t.Helper()
	strategySettings := DefaultClientStrategySettings()
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	strategySettings.RequestTimeout = 300 * time.Millisecond
	// a proxy that does not exist: the test is about the gate in front of the
	// connect, so the connect itself may fail once it is admitted
	strategySettings.ProxySettings = &ProxySettings{Index: 5, Network: "tcp", Address: "127.0.0.1:1"}
	strategy := NewClientStrategy(ctx, strategySettings)
	routeManager := NewRouteManager(ctx, "test-budget")
	auth := &ClientAuth{ByJwt: "jwt", InstanceId: NewId(), AppVersion: "1.0.0", ProvideIntent: true}
	return NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "ws"+serverUrl[len("http"):], auth, TransportModeH1, settings)
}

// With the budget throttled and enforcing, a proxied transport waits for its
// turn before it starts a connect. Nothing is dialed until it is released.
func TestRunH1_ThrottledBudgetHoldsAProxiedConnect(t *testing.T) {
	defer setGovernModeForTest(governEnforce)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectBudget.setRate(budgetFloorRate)
	defer connectBudget.setRate(budgetCeilingRate)
	// every third token is random; pin it to the head of the line (a blocker) so
	// the test does not lose about 5% of the time to its own waiter being drawn
	defer connectBudget.setPick(func(n int) int { return 0 })()

	// twenty waiters ahead of us at 2/s: our turn is about ten seconds away
	blockers, releaseBlockers := context.WithCancel(context.Background())
	for i := 0; i < 20; i++ {
		go connectBudget.admit(blockers, false)
	}
	waitQueued(t, connectBudget, 20)

	before := connectGov.snapshot()
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := newProxiedFlapTransport(t, ctx, "http://127.0.0.1:9", settings)
	defer transport.Close()

	time.Sleep(1500 * time.Millisecond)
	if n := connectGov.snapshot().Attempts - before.Attempts; n != 0 {
		t.Fatalf("a throttled budget let %d connect attempts through", n)
	}

	// release the line: the transport must now get its turn and dial
	connectBudget.setRate(budgetCeilingRate)
	releaseBlockers()
	deadline := time.After(10 * time.Second)
	for connectGov.snapshot().Attempts-before.Attempts == 0 {
		select {
		case <-deadline:
			t.Fatal("transport never got its turn after the budget opened")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The same throttle, but not enforcing: shadow mode must never delay a connect.
func TestRunH1_ShadowModeNeverHoldsAConnect(t *testing.T) {
	defer setGovernModeForTest(governShadow)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectBudget.setRate(budgetFloorRate)
	defer connectBudget.setRate(budgetCeilingRate)

	before := connectGov.snapshot()
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := newProxiedFlapTransport(t, ctx, "http://127.0.0.1:9", settings)
	defer transport.Close()

	deadline := time.After(5 * time.Second)
	for connectGov.snapshot().Attempts-before.Attempts == 0 {
		select {
		case <-deadline:
			t.Fatal("shadow mode held a connect")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// The direct path is the scout of last resort and never waits.
func TestRunH1_DirectPathIgnoresAThrottledBudget(t *testing.T) {
	defer setGovernModeForTest(governEnforce)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectBudget.setRate(budgetFloorRate)
	defer connectBudget.setRate(budgetCeilingRate)
	blockers, releaseBlockers := context.WithCancel(context.Background())
	defer releaseBlockers()
	for i := 0; i < 20; i++ {
		go connectBudget.admit(blockers, false)
	}
	waitQueued(t, connectBudget, 20)

	var accepts atomic.Int64
	var stamps flapStamps
	server := flapServer(time.Hour, &accepts, &stamps)
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := newFlapTestTransport(t, ctx, server.URL, settings) // no ProxySettings: the direct path
	defer transport.Close()

	deadline := time.After(5 * time.Second)
	for accepts.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the direct path waited on the budget")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// While the backend is failing, one failed attempt must not turn into many
// dial rounds over the full request timeout.
func TestWsDialContext_DegradedBackendGetsOneRound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "database is on fire", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := newFastFailStrategy(ctx, 800*time.Millisecond)

	prev := singleRoundCheck
	defer func() { singleRoundCheck = prev }()

	singleRoundCheck = func() bool { return true }
	start := time.Now()
	_, _, err := strategy.WsDialContext(ctx, url, http.Header{})
	short := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "1 round(s)") {
		t.Fatalf("a degraded backend should get exactly one round, got %v", err)
	}
	if short > 400*time.Millisecond {
		t.Fatalf("one round took %s, it must not wait out the request timeout", short)
	}

	singleRoundCheck = func() bool { return false }
	start = time.Now()
	_, _, err = strategy.WsDialContext(ctx, url, http.Header{})
	if long := time.Since(start); long < 700*time.Millisecond {
		t.Fatalf("a healthy backend keeps retrying for the request timeout, returned after %s (%v)", long, err)
	}
}

// Scouts that land on dead proxies tell the controller nothing. A throttle that
// only ever sees such attempts must not sit at the floor forever.
func TestBudget_UninformativeAttemptsLiftTheThrottle(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter(ctx, h.budget, false)
	waitQueued(t, h.budget, 1)

	h.attempts(connectSocksFail, 30)
	h.attempts(connectTimeoutNoReply, 30)
	h.budget.control()
	if got := h.budget.currentRate(); got != budgetFloorRate*budgetIncreaseFactor {
		t.Fatalf("rate after an uninformative interval = %v, want %v", got, budgetFloorRate*budgetIncreaseFactor)
	}
}

// If the dispatcher dies, waiters must not park forever.
func TestBudget_FailsOpenWhenTheDispatcherStalls(t *testing.T) {
	prevInterval, prevGrace := budgetStallCheckInterval, budgetStallGrace
	budgetStallCheckInterval, budgetStallGrace = 10*time.Millisecond, 50*time.Millisecond
	defer func() { budgetStallCheckInterval, budgetStallGrace = prevInterval, prevGrace }()

	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	out := waiter(context.Background(), h.budget, false)
	waitQueued(t, h.budget, 1)

	h.advance(time.Minute) // time passes and nothing ticks
	select {
	case ok := <-out:
		if !ok {
			t.Fatal("a stalled budget must fail open, not refuse")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a waiter stayed parked behind a dead dispatcher")
	}
}

func TestBudget_HealthyDispatcherDoesNotFailOpen(t *testing.T) {
	prevInterval, prevGrace := budgetStallCheckInterval, budgetStallGrace
	budgetStallCheckInterval, budgetStallGrace = 10*time.Millisecond, 50*time.Millisecond
	defer func() { budgetStallCheckInterval, budgetStallGrace = prevInterval, prevGrace }()

	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := waiter(ctx, h.budget, false)
	waitQueued(t, h.budget, 1)

	for i := 0; i < 5; i++ {
		h.advance(10 * time.Millisecond)
		h.budget.tick(*h.now) // the dispatcher is alive, just slow at 2/s
		time.Sleep(15 * time.Millisecond)
	}
	select {
	case <-out:
		t.Fatal("a live dispatcher at its rate must not be bypassed")
	default:
	}
}

// The reaper must stay out of the way for as long as an outage could still be
// the reason a proxy is down, including after the budget has reopened.
func TestBudget_OutageSignalOutlivesTheThrottle(t *testing.T) {
	h := newBudgetHarness()
	if h.budget.outageRecent(time.Hour) {
		t.Fatal("a healthy budget has never seen an outage")
	}

	h.attempts(connectTimeoutPlatform, 200)
	h.budget.control()
	if !h.budget.outageRecent(time.Minute) {
		t.Fatal("a throttled budget is an outage in progress")
	}

	// the backend heals and the budget ramps back to open
	for i := 0; i < 20 && h.budget.throttled(); i++ {
		h.advance(budgetControlInterval)
		h.attempts(connectOK, 20)
		h.budget.control()
	}
	if h.budget.throttled() {
		t.Fatal("setup: the budget should have reopened")
	}
	if !h.budget.outageRecent(30 * time.Minute) {
		t.Fatal("a proxy that has been down since the outage may only be dead after a long quiet period")
	}
	h.advance(31 * time.Minute)
	if h.budget.outageRecent(30 * time.Minute) {
		t.Fatal("thirty quiet minutes later the outage is over")
	}
}

func scored(score uint64) func() uint64 { return func() uint64 { return score } }

func waiterScored(ctx context.Context, b *reconnectBudget, score uint64) <-chan bool {
	out := make(chan bool, 1)
	go func() { out <- b.admitParked(ctx, false, scored(score), nil) }()
	return out
}

// When tokens are scarce the proxies with the best record go first: the ones
// that earned before the outage are the likeliest to work, and they are the ones
// worth having back soonest.
func TestBudget_BestHistoryScoutsGoFirst(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	h.budget.pick = func(n int) int { return 0 }

	scores := []uint64{5, 1, 9, 3, 7, 2}
	outs := make([]<-chan bool, len(scores))
	for i, sc := range scores {
		outs[i] = waiterScored(context.Background(), h.budget, sc)
		waitQueued(t, h.budget, i+1)
	}

	h.step(time.Second) // two tokens
	time.Sleep(50 * time.Millisecond)

	released := map[uint64]bool{}
	for i, ch := range outs {
		select {
		case <-ch:
			released[scores[i]] = true
		default:
		}
	}
	if len(released) != 2 || !released[9] || !released[7] {
		t.Fatalf("the two best (9 and 7) should have gone first, released %v", released)
	}
}

// A third of the slots are random so one stale favourite, or a proxy with no
// history yet, cannot hold everyone else out.
func TestBudget_EveryThirdScoutIsRandom(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(3) // 3 tokens in a second
	h.budget.pick = func(n int) int { return 0 }

	scores := []uint64{1, 9, 8, 7, 6}
	outs := make([]<-chan bool, len(scores))
	for i, sc := range scores {
		outs[i] = waiterScored(context.Background(), h.budget, sc)
		waitQueued(t, h.budget, i+1)
	}

	h.step(time.Second)
	time.Sleep(50 * time.Millisecond)

	released := map[uint64]bool{}
	for i, ch := range outs {
		select {
		case <-ch:
			released[scores[i]] = true
		default:
		}
	}
	// two best-first (9, 8) then one random slot; the injected pick(0) takes the
	// head of what is left, which is the lowest-scored arrival (1)
	if len(released) != 3 || !released[9] || !released[8] || !released[1] {
		t.Fatalf("expected the two best then a random pick, released %v", released)
	}
}

func TestBudget_EqualScoresStayFirstInFirstOut(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)
	outs := make([]<-chan bool, 5)
	for i := range outs {
		outs[i] = waiterScored(context.Background(), h.budget, 4)
		waitQueued(t, h.budget, i+1)
	}
	h.step(time.Second)
	time.Sleep(50 * time.Millisecond)
	for i, ch := range outs {
		select {
		case <-ch:
			if 2 <= i {
				t.Fatalf("waiter %d released before earlier equal-score arrivals", i)
			}
		default:
			if i < 2 {
				t.Fatalf("waiter %d should have been released first", i)
			}
		}
	}
}

// Priority must never become starvation: with wildly different scores every
// parked proxy still comes back within the ramp.
func TestBudget_LowScoresStillComeBackAtTheRamp(t *testing.T) {
	h := newBudgetHarness()
	h.budget.setRate(budgetFloorRate)

	const parked = 2000
	released := make(chan struct{}, parked)
	for i := 0; i < parked; i++ {
		score := uint64(i % 97)
		go func() {
			if h.budget.admitParked(context.Background(), false, scored(score), nil) {
				released <- struct{}{}
			}
		}()
	}
	waitQueued(t, h.budget, parked)

	count := 0
	start := *h.now
	for count < parked && h.now.Sub(start) < 5*time.Minute {
		h.advance(budgetTick)
		if h.now.Sub(start)%budgetControlInterval == 0 {
			h.attempts(connectOK, 20)
			h.budget.control()
		}
		h.budget.tick(*h.now)
		time.Sleep(100 * time.Microsecond)
		for drained := false; !drained; {
			select {
			case <-released:
				count++
			default:
				drained = true
			}
		}
	}
	if count != parked || h.now.Sub(start) > 90*time.Second {
		t.Fatalf("released %d of %d in %s", count, parked, h.now.Sub(start))
	}
}
