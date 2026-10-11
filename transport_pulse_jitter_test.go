package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitPulseJitter_StaysWithinTheSpread(t *testing.T) {
	prev := pulseSpread
	pulseSpread = 40 * time.Millisecond
	defer func() { pulseSpread = prev }()
	for i := 0; i < 20; i++ {
		start := time.Now()
		if !waitPulseJitter(context.Background()) {
			t.Fatal("an uncancelled wait must complete")
		}
		if d := time.Since(start); d > 200*time.Millisecond {
			t.Fatalf("waited %s, the spread is 40ms", d)
		}
	}
}

func TestWaitPulseJitter_CancelsPromptly(t *testing.T) {
	prevS, prevJ := pulseSpread, pulseJitter
	pulseSpread = time.Hour
	pulseJitter = func(max time.Duration) time.Duration { return time.Hour }
	defer func() { pulseSpread, pulseJitter = prevS, prevJ }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- waitPulseJitter(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a cancelled wait must report it was cancelled")
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled transport waited out the jitter")
	}
}

// The hourly pulse wakes every transport that is backing off. Waking them all in
// the same instant is a herd, so each waits a random slice of the spread first.
func TestRunH1_PulseWakesATransportAfterAJitter(t *testing.T) {
	prevS, prevJ := pulseSpread, pulseJitter
	pulseSpread = time.Second
	pulseJitter = func(max time.Duration) time.Duration { return 800 * time.Millisecond }
	defer func() { pulseSpread, pulseJitter = prevS, prevJ }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close() // refuses every dial

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultPlatformTransportSettings()
	settings.ReconnectTimeout = 30 * time.Second // a long backoff, so only the pulse can wake it
	transport := newFlapTestTransport(t, ctx, url, settings)
	defer transport.Close()

	// wait for the first attempt to fail and the transport to start its backoff
	first := connectGov.snapshot().Attempts
	deadline := time.After(10 * time.Second)
	for connectGov.snapshot().Attempts == first {
		select {
		case <-deadline:
			t.Fatal("the first attempt never finished")
		case <-time.After(20 * time.Millisecond):
		}
	}
	time.Sleep(200 * time.Millisecond) // into the backoff select
	after := connectGov.snapshot().Attempts

	pulsedAt := time.Now()
	TriggerPulse()
	for connectGov.snapshot().Attempts == after {
		select {
		case <-time.After(5 * time.Second):
			t.Fatal("the pulse never woke the transport")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 800ms jitter plus the 500ms failed dial that finally records the attempt
	if d := time.Since(pulsedAt); d < 900*time.Millisecond {
		t.Fatalf("retried %s after the pulse; the jitter was skipped", d)
	}
}
