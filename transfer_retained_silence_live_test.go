package connect

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// Live-loop rows for RetainedAckTimeout. They run SendSequence.Run inside a
// synctest bubble against a drained gateway route, so every resend, ack and
// deadline is on the virtual clock and nothing sleeps for real.

type retainedLiveRig struct {
	seq     *SendSequence
	wires   atomic.Int64
	exited  chan struct{}
	mu      sync.Mutex
	results map[uint64][]error
	items   map[uint64]*sendItem
}

func (self *retainedLiveRig) failures(sequenceNumber uint64) []error {
	self.mu.Lock()
	defer self.mu.Unlock()
	return append([]error(nil), self.results[sequenceNumber]...)
}

func (self *retainedLiveRig) hasExited() bool {
	select {
	case <-self.exited:
		return true
	default:
		return false
	}
}

// Starts a sequence holding `count` retained items numbered 1..count. When
// `withRoute` is false the client has no carrier, so nothing can be written.
func startRetainedLiveRig(
	t *testing.T,
	count int,
	withRoute bool,
	backstop time.Duration,
	configure func(settings *SendBufferSettings),
) *retainedLiveRig {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.ControlPingTimeout = 0
	clientSettings.Log = NewNoopLogger()
	if configure != nil {
		configure(clientSettings.SendBufferSettings)
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	t.Cleanup(client.Close)

	rig := &retainedLiveRig{
		exited:  make(chan struct{}),
		results: map[uint64][]error{},
		items:   map[uint64]*sendItem{},
	}

	if withRoute {
		route := make(chan []byte, 4096)
		transport := NewSendGatewayTransport()
		client.RouteManager().UpdateTransport(transport, []Route{route})
		t.Cleanup(func() { client.RouteManager().RemoveTransport(transport) })
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-route:
					rig.wires.Add(1)
				}
			}
		}()
	}

	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	rig.seq = NewSendSequence(
		ctx,
		client,
		sendBuffer,
		DestinationId(NewId()),
		MultiHopId{},
		false,
		true,
		sequenceTlsRoleClient,
		false,
		clientSettings.SendBufferSettings,
	)

	now := time.Now()
	for i := 1; i <= count; i++ {
		sequenceNumber := uint64(i)
		item := &sendItem{
			transferItem: transferItem{
				messageId:        NewId(),
				sequenceNumber:   sequenceNumber,
				messageByteCount: 100,
			},
			sendTime:   now,
			resendTime: now.Add(clientSettings.SendBufferSettings.MinResendInterval),
			sendCount:  1,
			// every item is marked head so a later head needs no frame rewrite
			head:               true,
			transferFrameBytes: []byte("retained"),
			ackCallback: func(err error) {
				rig.mu.Lock()
				rig.results[sequenceNumber] = append(rig.results[sequenceNumber], err)
				rig.mu.Unlock()
			},
			retainAfterAckTimeout: true,
			backstopDeadline:      now.Add(backstop),
		}
		rig.items[sequenceNumber] = item
		rig.seq.retainedByteCount += item.MessageByteCount()
		rig.seq.sendItems = append(rig.seq.sendItems, item)
		rig.seq.resendQueue.Add(item)
	}

	go HandleError(func() {
		defer close(rig.exited)
		rig.seq.Run()
	})
	return rig
}

func (self *retainedLiveRig) ack(sequenceNumber uint64, selective bool) {
	self.seq.acks <- &protocol.Ack{
		MessageId: self.items[sequenceNumber].messageId.Bytes(),
		Selective: selective,
	}
}

// Real resends keep a silent destination's clock running and the sequence
// ends near the bound, long before the item backstop, failing every owner.
func TestRetainedSilenceLiveSilentDestinationExitsNearBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig := startRetainedLiveRig(t, 3, true, time.Hour, nil)
		bound := rig.seq.sendBufferSettings.RetainedAckTimeout

		time.Sleep(bound / 2)
		synctest.Wait()
		if rig.hasExited() {
			t.Fatal("sequence exited at half the bound")
		}
		if rig.wires.Load() == 0 {
			t.Fatal("no resend reached the carrier; the clock cannot have started")
		}

		time.Sleep(bound/2 + rig.seq.sendBufferSettings.AckTimeout + rig.seq.sendBufferSettings.MaxResendInterval)
		synctest.Wait()
		if !rig.hasExited() {
			t.Fatal("silent destination did not end the sequence by bound + one resend interval")
		}
		for sequenceNumber := uint64(1); sequenceNumber <= 3; sequenceNumber++ {
			failures := rig.failures(sequenceNumber)
			if len(failures) != 1 || failures[0] == nil {
				t.Fatalf("owner %d failures = %v, want exactly one error", sequenceNumber, failures)
			}
			if strings.Contains(failures[0].Error(), "backstop") {
				t.Fatalf("owner %d ended by the backstop, not the silence bound: %v", sequenceNumber, failures[0])
			}
		}
		if rig.seq.retainedByteCount != 0 {
			t.Fatalf("retainedByteCount = %d after exit, want 0", rig.seq.retainedByteCount)
		}

		after := rig.wires.Load()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if rig.wires.Load() != after {
			t.Fatal("writes continued after the sequence exited")
		}
	})
}

// A destination that acknowledges less often than the bound is never cut off,
// and goes silent only once the acknowledgements stop.
func TestRetainedSilenceLiveSlowAcksKeepSequenceThenSilenceEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig := startRetainedLiveRig(t, 6, true, 2*time.Hour, nil)
		bound := rig.seq.sendBufferSettings.RetainedAckTimeout
		gap := bound - 40*time.Second

		for sequenceNumber := uint64(1); sequenceNumber <= 4; sequenceNumber++ {
			time.Sleep(gap)
			synctest.Wait()
			if rig.hasExited() {
				t.Fatalf("sequence exited before cumulative ack %d with gaps shorter than the bound", sequenceNumber)
			}
			rig.ack(sequenceNumber, false)
			synctest.Wait()
		}
		if elapsed := 4 * gap; elapsed <= bound {
			t.Fatalf("test did not outlast the bound: %s <= %s", elapsed, bound)
		}
		if rig.hasExited() {
			t.Fatal("sequence ended while the destination was acknowledging")
		}
		if failures := rig.failures(4); len(failures) != 1 || failures[0] != nil {
			t.Fatalf("acked owner saw %v, want one nil", failures)
		}

		time.Sleep(bound + rig.seq.sendBufferSettings.AckTimeout + rig.seq.sendBufferSettings.MaxResendInterval)
		synctest.Wait()
		if !rig.hasExited() {
			t.Fatal("silence after the last ack did not end the sequence")
		}
		if failures := rig.failures(5); len(failures) != 1 || failures[0] == nil {
			t.Fatalf("unacked owner saw %v, want one error", failures)
		}
	})
}

// A selective ack proves the destination is live, restarting the clock.
func TestRetainedSilenceLiveSelectiveAckRestartsClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig := startRetainedLiveRig(t, 4, true, 2*time.Hour, nil)
		bound := rig.seq.sendBufferSettings.RetainedAckTimeout

		time.Sleep(bound - 30*time.Second)
		synctest.Wait()
		rig.ack(3, true)
		synctest.Wait()

		time.Sleep(bound / 2)
		synctest.Wait()
		if rig.hasExited() {
			t.Fatal("selective ack did not restart the silence clock")
		}

		time.Sleep(bound + rig.seq.sendBufferSettings.AckTimeout + rig.seq.sendBufferSettings.MaxResendInterval)
		synctest.Wait()
		if !rig.hasExited() {
			t.Fatal("silence after the selective ack did not end the sequence")
		}
	})
}

// With no carrier nothing reaches the wire, so the clock never starts and the
// per-item backstop remains the only bound.
func TestRetainedSilenceLiveNoCarrierLeavesBackstopAsOnlyBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backstop := 20 * time.Minute
		rig := startRetainedLiveRig(t, 1, false, backstop, nil)
		bound := rig.seq.sendBufferSettings.RetainedAckTimeout

		time.Sleep(3 * bound)
		synctest.Wait()
		if rig.hasExited() {
			t.Fatal("silence bound fired with nothing ever written")
		}
		if rig.seq.retainedSilenceStart.IsZero() == false {
			t.Fatal("clock started without a carrier write")
		}

		time.Sleep(backstop)
		synctest.Wait()
		failures := rig.failures(1)
		if len(failures) != 1 || failures[0] == nil || !strings.Contains(failures[0].Error(), "backstop") {
			t.Fatalf("owner saw %v, want the backstop error", failures)
		}
	})
}

// Zero disables the bound; only the per-item backstop ends the item.
func TestRetainedSilenceLiveZeroDisablesBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backstop := 30 * time.Minute
		rig := startRetainedLiveRig(t, 2, true, backstop, func(settings *SendBufferSettings) {
			settings.RetainedAckTimeout = 0
		})

		time.Sleep(backstop - time.Minute)
		synctest.Wait()
		if rig.hasExited() {
			t.Fatal("sequence exited with the bound disabled")
		}
		for sequenceNumber := uint64(1); sequenceNumber <= 2; sequenceNumber++ {
			if failures := rig.failures(sequenceNumber); len(failures) != 0 {
				t.Fatalf("owner %d resolved early with the bound disabled: %v", sequenceNumber, failures)
			}
		}

		time.Sleep(2 * time.Minute)
		synctest.Wait()
		failures := rig.failures(1)
		if len(failures) != 1 || failures[0] == nil || !strings.Contains(failures[0].Error(), "backstop") {
			t.Fatalf("owner saw %v, want the backstop error", failures)
		}
	})
}

// The resend cap already throttles a silent item: backed-off resends up to
// MaxResendCount, then one per AckTimeout. The carrier's total write count over
// the whole silent lifetime therefore stays small, and stops at the bound.
func TestRetainedSilenceLiveWireWritesStayBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rig := startRetainedLiveRig(t, 1, true, time.Hour, nil)
		settings := rig.seq.sendBufferSettings

		time.Sleep(settings.RetainedAckTimeout + settings.AckTimeout + settings.MaxResendInterval)
		synctest.Wait()
		if !rig.hasExited() {
			t.Fatal("sequence did not end")
		}
		// the carrier is written about twice per resend while backing off to
		// the cap, then at most one resend per AckTimeout until the bound
		limit := 2*int64(settings.MaxResendCount) + int64(settings.RetainedAckTimeout/settings.AckTimeout) + 4
		if writes := rig.wires.Load(); writes > limit {
			t.Fatalf("carrier saw %d writes for one silent item, want <= %d", writes, limit)
		}
		after := rig.wires.Load()
		time.Sleep(time.Hour)
		synctest.Wait()
		if rig.wires.Load() != after {
			t.Fatal("carrier written after the sequence ended")
		}
	})
}
