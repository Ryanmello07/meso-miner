package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type speedEchoHarness struct {
	mu  sync.Mutex
	now time.Time
	b   *speedEchoBudget
}

func newSpeedEchoHarness() *speedEchoHarness {
	h := &speedEchoHarness{now: time.Unix(3_000_000, 0)}
	h.b = newSpeedEchoBudget(func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	})
	h.b.degraded = func() bool { return false }
	return h
}

func (h *speedEchoHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

// The platform runs a speed test on a fresh connection: it floods the websocket
// and the provider echoes every byte back. That is real bandwidth on the paid
// proxy for zero billable payload, and after a mass reconnect every connection
// does it at once (2.4 GiB in five minutes on one host). A single test must fit,
// the aggregate must not.
func TestSpeedEcho_OneNormalTestIsNeverCut(t *testing.T) {
	h := newSpeedEchoHarness()
	s := h.b.newSession()
	var sent int
	for sent < 6<<20 { // 6 MiB in 64 KiB frames, spread over 10 seconds
		if !s.allow(64 << 10) {
			t.Fatalf("a single speed test was cut after %d bytes", sent)
		}
		sent += 64 << 10
		h.advance(10 * time.Second / 96)
	}
}

func TestSpeedEcho_AStuckTestEndsAtThePerSessionCap(t *testing.T) {
	h := newSpeedEchoHarness()
	s := h.b.newSession()
	var echoed int
	for i := 0; i < 1000; i++ {
		if s.allow(64 << 10) {
			echoed += 64 << 10
		}
		h.advance(time.Second) // slowly enough that the global bucket never binds
	}
	if echoed != speedEchoSessionMaxBytes {
		t.Fatalf("a never-ending test echoed %d bytes, want exactly the cap %d", echoed, speedEchoSessionMaxBytes)
	}
}

func TestSpeedEcho_ManyConcurrentTestsShareTheGlobalRate(t *testing.T) {
	h := newSpeedEchoHarness()
	sessions := make([]*speedEchoSession, 700)
	for i := range sessions {
		sessions[i] = h.b.newSession()
	}
	var echoed int
	// 30 seconds in which every one of 700 connections tries to echo 90 KB/s
	for sec := 0; sec < 30; sec++ {
		for _, s := range sessions {
			for f := 0; f < 2; f++ {
				if s.allow(45 << 10) {
					echoed += 45 << 10
				}
			}
		}
		h.advance(time.Second)
	}
	offered := 700 * 30 * 90 << 10
	ceiling := int(speedEchoRate*30) + speedEchoBurst
	if ceiling < echoed {
		t.Fatalf("echoed %d bytes in 30s, above the global ceiling %d", echoed, ceiling)
	}
	if offered/4 < echoed {
		t.Fatalf("echoed %d of %d offered: the storm was not cut enough", echoed, offered)
	}
}

func TestSpeedEcho_DuringAnOutageTheGlobalRateDrops(t *testing.T) {
	h := newSpeedEchoHarness()
	sessions := make([]*speedEchoSession, 200)
	for i := range sessions {
		sessions[i] = h.b.newSession()
	}
	run := func() int {
		echoed := 0
		for sec := 0; sec < 30; sec++ {
			for _, s := range sessions {
				if s.allow(64 << 10) {
					echoed += 64 << 10
				}
			}
			h.advance(time.Second)
		}
		return echoed
	}
	healthy := run()
	h.b.degraded = func() bool { return true }
	for i := range sessions {
		sessions[i] = h.b.newSession()
	}
	h.advance(time.Minute) // refill; the burst is the same, the sustained rate is what changes
	outage := run()
	if outage*3 > healthy {
		t.Fatalf("outage echo %d should be well under healthy echo %d", outage, healthy)
	}
}

func TestSpeedEcho_OnlyEnforceModeDrops(t *testing.T) {
	for _, mode := range []governMode{governOff, governShadow} {
		restore := setGovernModeForTest(mode)
		h := newSpeedEchoHarness()
		s := h.b.newSession()
		for i := 0; i < 1000; i++ {
			if !s.allowInMode(64 << 10) {
				restore()
				t.Fatalf("mode %v dropped an echo", mode)
			}
		}
		restore()
	}
}

func TestSpeedEcho_CountersSplitEchoedFromDropped(t *testing.T) {
	before := SpeedEchoStatsNow()
	h := newSpeedEchoHarness()
	s := h.b.newSession()
	restore := setGovernModeForTest(governEnforce)
	defer restore()
	for i := 0; i < 400; i++ {
		s.allowInMode(64 << 10)
	}
	after := SpeedEchoStatsNow()
	if after.EchoedBytes-before.EchoedBytes != speedEchoSessionMaxBytes {
		t.Fatalf("echoed = %d, want the session cap", after.EchoedBytes-before.EchoedBytes)
	}
	if after.DroppedBytes-before.DroppedBytes != int64(400*(64<<10)-speedEchoSessionMaxBytes) {
		t.Fatalf("dropped = %d", after.DroppedBytes-before.DroppedBytes)
	}
}

// End to end: the platform starts a speed test and floods the websocket; a
// provider that echoed it all would send back everything. Only the session cap
// comes back.
func TestRunH1_SpeedTestFloodIsEchoedOnlyUpToTheCap(t *testing.T) {
	restore := setGovernModeForTest(governEnforce)
	defer restore()
	speedEcho.mu.Lock()
	speedEcho.tokens = speedEchoBurst
	speedEcho.last = time.Now()
	speedEcho.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const frame = 64 << 10
	const flood = 24 << 20 // three times the cap
	var echoedBack atomic.Int64
	done := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		go func() {
			for {
				_, msg, err := ws.ReadMessage()
				if err != nil {
					return
				}
				if frame <= len(msg) {
					echoedBack.Add(int64(len(msg)))
				}
			}
		}()
		_ = ws.WriteMessage(websocket.BinaryMessage, []byte{TransportControlSpeedStart, 0, 0, 0, 0})
		buf := make([]byte, frame)
		for sent := 0; sent < flood; sent += frame {
			if ws.WriteMessage(websocket.BinaryMessage, buf) != nil {
				break
			}
		}
		time.Sleep(1500 * time.Millisecond)
		close(done)
	}))
	defer server.Close()

	settings := DefaultPlatformTransportSettings()
	transport := newFlapTestTransport(t, ctx, server.URL, settings)
	defer transport.Close()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the flood never finished")
	}
	got := echoedBack.Load()
	if got == 0 {
		t.Fatal("setup: nothing was echoed at all")
	}
	if speedEchoSessionMaxBytes < got {
		t.Fatalf("echoed %d bytes of a %d byte flood; the cap is %d", got, flood, speedEchoSessionMaxBytes)
	}
}
