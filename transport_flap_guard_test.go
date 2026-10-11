package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func identityJitter(d time.Duration) time.Duration { return d }

func newTestFlapGuard(stableAfter time.Duration, base time.Duration) *flapGuard {
	g := newFlapGuard(stableAfter, base)
	g.jitter = identityJitter
	return g
}

func TestFlapGuard_StableSessionNeedsNoExtraWait(t *testing.T) {
	g := newTestFlapGuard(60*time.Second, 5*time.Second)
	if got := g.sessionEnded(2 * time.Hour); got != 0 {
		t.Fatalf("a session that lived 2h must reconnect without extra wait, got %s", got)
	}
}

func TestFlapGuard_YoungSessionsEscalate(t *testing.T) {
	g := newTestFlapGuard(60*time.Second, 5*time.Second)
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}
	for i, w := range want {
		if got := g.sessionEnded(3 * time.Second); got != w {
			t.Fatalf("young session %d: wait %s, want %s", i, got, w)
		}
	}
}

func TestFlapGuard_StableSessionClearsStreak(t *testing.T) {
	g := newTestFlapGuard(60*time.Second, 5*time.Second)
	g.sessionEnded(time.Second)
	g.sessionEnded(time.Second)
	if got := g.sessionEnded(61 * time.Second); got != 0 {
		t.Fatalf("stable session must not wait, got %s", got)
	}
	if got := g.sessionEnded(time.Second); got != 5*time.Second {
		t.Fatalf("after a stable session the streak starts over at the base, got %s", got)
	}
}

func TestFlapGuard_SessionJustUnderThresholdCounts(t *testing.T) {
	g := newTestFlapGuard(60*time.Second, 5*time.Second)
	if got := g.sessionEnded(60*time.Second - time.Nanosecond); got == 0 {
		t.Fatal("a session shorter than the stable threshold is a flap")
	}
	if got := g.sessionEnded(60 * time.Second); got != 0 {
		t.Fatalf("a session exactly at the threshold is stable, got %s", got)
	}
}

func TestFlapGuard_UnsetBaseFallsBackToOneSecond(t *testing.T) {
	g := newTestFlapGuard(60*time.Second, 0)
	if got := g.sessionEnded(time.Second); got != time.Second {
		t.Fatalf("unset base must not make a tight loop, got %s", got)
	}
}

func TestFlapGuard_NegativeStableAfterDisables(t *testing.T) {
	g := newTestFlapGuard(-1, 5*time.Second)
	for i := 0; i < 5; i++ {
		if got := g.sessionEnded(time.Millisecond); got != 0 {
			t.Fatalf("disabled guard must never wait, got %s", got)
		}
	}
}

func TestFlapGuard_JitterStaysWithinBand(t *testing.T) {
	g := newFlapGuard(60*time.Second, 10*time.Second)
	for i := 0; i < 200; i++ {
		g2 := newFlapGuard(60*time.Second, 10*time.Second)
		got := g2.sessionEnded(time.Second)
		if got < 8*time.Second || got > 12*time.Second {
			t.Fatalf("first wait %s outside the +-20%% band around 10s", got)
		}
	}
	_ = g
}

func TestFlapGuard_CountsEngagedBackoffs(t *testing.T) {
	before := TransportFlapBackoffs()
	g := newTestFlapGuard(60*time.Second, time.Second)
	g.sessionEnded(time.Hour)
	g.sessionEnded(time.Second)
	g.sessionEnded(time.Second)
	if got := TransportFlapBackoffs() - before; got != 2 {
		t.Fatalf("expected 2 engaged backoffs counted, got %d", got)
	}
}

// flapServer upgrades every websocket and holds it for hold before closing, so
// a client sees a connect that succeeds and then dies.
func flapServer(hold time.Duration, accepts *atomic.Int64, stamps *flapStamps) *httptest.Server {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepts.Add(1)
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		stamps.add(time.Now())
		if 0 < hold {
			time.Sleep(hold)
		}
		ws.Close()
	}))
}

type flapStamps struct {
	mu sync.Mutex
	at []time.Time
}

func (s *flapStamps) add(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = append(s.at, t)
}

func (s *flapStamps) snapshot() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.at...)
}

func newFlapTestTransport(t *testing.T, ctx context.Context, serverUrl string, settings *PlatformTransportSettings) *PlatformTransport {
	t.Helper()
	// the default strategy spaces every connect 100-1000ms apart, which would
	// mask the loop's own behavior; these tests are about the loop, not that pacing
	strategySettings := DefaultClientStrategySettings()
	strategySettings.MinNextConnectDelay = 0
	strategySettings.MaxNextConnectDelay = 0
	strategy := NewClientStrategy(ctx, strategySettings)
	routeManager := NewRouteManager(ctx, "test-flap")
	auth := &ClientAuth{
		ByJwt:         "jwt",
		InstanceId:    NewId(),
		AppVersion:    "1.0.0",
		ProvideIntent: true,
	}
	return NewPlatformTransportWithTargetMode(ctx, strategy, routeManager, "ws"+strings.TrimPrefix(serverUrl, "http"), auth, TransportModeH1, settings)
}

// The incident shape: the backend accepts the websocket and the session dies
// seconds later. Before the guard each death reconnected as fast as the
// ReconnectTimeout jitter allowed, forever.
func TestRunH1_FlappingSessionBacksOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var accepts atomic.Int64
	var stamps flapStamps
	server := flapServer(0, &accepts, &stamps)
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	settings.FlapStableAfter = 10 * time.Second

	transport := newFlapTestTransport(t, ctx, server.URL, settings)
	defer transport.Close()

	time.Sleep(2 * time.Second)

	// waits of ~20, 40, 80, 160, 320, 640, 1280 ms: at most 8 connects in 2s.
	// an unguarded loop reconnects every few ms and lands in the hundreds.
	if n := accepts.Load(); n > 12 {
		t.Fatalf("flapping sessions were not backed off: %d connects in 2s", n)
	}
	if n := accepts.Load(); n < 3 {
		t.Fatalf("guard over-throttled, only %d connects in 2s", n)
	}
}

// A healthy session ends rarely and long after it began. The guard must not add
// any wait there: this is the path every hot restart and platform deploy takes.
func TestRunH1_StableSessionReconnectsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var accepts atomic.Int64
	var stamps flapStamps
	server := flapServer(300*time.Millisecond, &accepts, &stamps)
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 20 * time.Millisecond
	// every session below lives 300ms, which is stable by this threshold
	settings.FlapStableAfter = 100 * time.Millisecond

	transport := newFlapTestTransport(t, ctx, server.URL, settings)
	defer transport.Close()

	deadline := time.After(10 * time.Second)
	for len(stamps.snapshot()) < 4 {
		select {
		case <-deadline:
			t.Fatalf("only %d connects, stable sessions are being delayed", len(stamps.snapshot()))
		case <-time.After(10 * time.Millisecond):
		}
	}
	at := stamps.snapshot()
	for i := 1; i < 4; i++ {
		// 300ms hold plus a prompt reconnect; a guard delay would add 20ms, 40ms,
		// 80ms on top, so the third gap is the discriminating one
		gap := at[i].Sub(at[i-1])
		if gap > 300*time.Millisecond+60*time.Millisecond {
			t.Fatalf("gap %d between connects was %s, stable sessions must reconnect promptly", i, gap)
		}
	}
}
