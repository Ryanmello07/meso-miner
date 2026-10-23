package connect

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The platform floods a fresh connection with a speed test and the provider echoes
// every probe back. It must echo ALL of them: a test that never comes back is retried
// by the server (a provider that dropped echoes got ten times the tests and ten
// times the inbound bytes), and the score feeds provider ranking. The platform
// interleaves real Transfer frames with the probes; those belong to receive and
// must not be echoed (an echoed frame reflects its source back and is dropped as
// forged, so the user's data is lost). A probe from another test is not echoed.
func TestRunH1_SpeedTestEchoesOnlyItsProbesInFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const frame = TransportSpeedTestProbeByteCount
	const flood = 4 << 20
	const testId = 7
	var echoedBack atomic.Int64
	var reflected atomic.Int64
	var interleavedSent atomic.Int64
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
				if IsTransportSpeedTestProbe(msg, testId) {
					echoedBack.Add(int64(len(msg)))
				} else if frame <= len(msg) {
					reflected.Add(1)
				}
			}
		}()
		_ = ws.WriteMessage(websocket.BinaryMessage, []byte{TransportControlSpeedStart, 0, 0, 0, testId})
		probe := make([]byte, frame)
		SetTransportSpeedTestProbeHeader(probe, testId)
		// a probe-sized message that is not a probe of this test (a Transfer
		// frame never starts with a zero tag, so 0x0a stands in for one)
		transferLike := bytes.Repeat([]byte{0x0a}, frame)
		staleProbe := make([]byte, frame)
		SetTransportSpeedTestProbeHeader(staleProbe, testId+1)
		for sent := 0; sent < flood; sent += frame {
			if ws.WriteMessage(websocket.BinaryMessage, probe) != nil {
				break
			}
			if sent%(64*frame) == 0 {
				if ws.WriteMessage(websocket.BinaryMessage, transferLike) != nil ||
					ws.WriteMessage(websocket.BinaryMessage, staleProbe) != nil {
					break
				}
				interleavedSent.Add(2)
			}
		}
		time.Sleep(1500 * time.Millisecond)
		close(done)
	}))
	defer server.Close()

	before := SpeedEchoStatsNow()
	transport := newFlapTestTransport(t, ctx, server.URL, DefaultPlatformTransportSettings())
	defer transport.Close()
	// the interleaved frames are delivered to receive, so drain it
	var delivered atomic.Int64
	reader := transport.routeManager.OpenMultiRouteReader(DestinationId(NewId()))
	defer transport.routeManager.CloseMultiRouteReader(reader)
	go func() {
		for ctx.Err() == nil {
			if message, err := reader.Read(ctx, 100*time.Millisecond); err == nil && message != nil {
				delivered.Add(1)
				MessagePoolReturn(message)
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the flood never finished")
	}
	if got := echoedBack.Load(); got != flood {
		t.Fatalf("echoed %d of %d bytes", got, flood)
	}
	if got := reflected.Load(); got != 0 {
		t.Fatalf("echoed %d messages that were not this test's probes", got)
	}
	if got, want := delivered.Load(), interleavedSent.Load(); got != want {
		t.Fatalf("delivered %d of %d interleaved frames", got, want)
	}
	after := SpeedEchoStatsNow()
	if after.Tests-before.Tests != 1 {
		t.Fatalf("tests counted = %d, want 1", after.Tests-before.Tests)
	}
	if after.EchoedBytes-before.EchoedBytes != flood {
		t.Fatalf("echoed bytes counted = %d, want %d", after.EchoedBytes-before.EchoedBytes, flood)
	}
}
