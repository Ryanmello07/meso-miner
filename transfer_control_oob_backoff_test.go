package connect

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Retries spread over [delay/2, delay), so identities that failed together
// do not retry together.
func TestOobRetryJitterStaysInItsBand(t *testing.T) {
	for _, delay := range []time.Duration{time.Second, 8 * time.Second, 60 * time.Second} {
		for range 1000 {
			if got := oobRetryJitter(delay); got < delay/2 || delay <= got {
				t.Fatalf("jitter(%s) = %s, want [%s, %s)", delay, got, delay/2, delay)
			}
		}
	}
	if got := oobRetryJitter(1); got != 1 {
		t.Fatalf("jitter(1ns) = %s", got)
	}
}

// failingTimedOob fails every control and records when each attempt came.
type failingTimedOob struct {
	attempts chan time.Time
}

func (self *failingTimedOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	self.attempts <- time.Now()
	callback(nil, errors.New("api unreachable"))
}

// Against an API that keeps failing, the retries of one Send back off: each
// gap is at least half the previous delay doubled, up to the cap.
func TestControlSyncOobRetriesBackOff(t *testing.T) {
	oob := &failingTimedOob{attempts: make(chan time.Time, 64)}
	client := NewClient(context.Background(), NewId(), oob, DefaultClientSettings())
	defer client.Cancel()
	control := NewControlSyncOob(context.Background(), client, "test")
	defer control.Close()
	control.retryTimeout = 20 * time.Millisecond
	control.maxRetryTimeout = 80 * time.Millisecond

	control.Send(&protocol.Frame{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: MessagePoolCopy([]byte("x"))}, nil)
	var times []time.Time
	for len(times) < 6 {
		select {
		case at := <-oob.attempts:
			times = append(times, at)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d attempts arrived", len(times))
		}
	}
	// expected delays: 20, 40, 80, 80, 80 ms, each jittered into [d/2, d)
	want := []time.Duration{20, 40, 80, 80, 80}
	for index := 1; index < len(times); index++ {
		gap := times[index].Sub(times[index-1])
		floor := want[index-1] * time.Millisecond / 2
		if gap < floor {
			t.Fatalf("retry %d came after %s, want at least %s", index, gap, floor)
		}
	}
	if total := times[len(times)-1].Sub(times[0]); total < 150*time.Millisecond {
		t.Fatalf("5 retries took %s; a fixed 20ms retry would take 100ms", total)
	}
}
