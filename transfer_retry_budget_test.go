package connect

import (
	"context"
	"testing"
	"time"
)

func newTestRetryBudget() (*transferRetryBudget, *time.Time) {
	now := time.Unix(3_000_000, 0)
	b := newTransferRetryBudget(func() time.Time { return now })
	return b, &now
}

func TestRetryBudget_StartsWithAGrantSoAFreshProcessCanRecover(t *testing.T) {
	b, _ := newTestRetryBudget()
	if !b.allow(1000) {
		t.Fatal("a fresh budget must allow a resend")
	}
}

func TestRetryBudget_ResendsAreChargedAgainstWhatWasFirstSent(t *testing.T) {
	b, _ := newTestRetryBudget()
	b.reset(0)

	if b.allow(1) {
		t.Fatal("an empty budget must refuse")
	}
	b.earn(10_000) // 10 KB first-sent earns 25%
	if !b.allow(2500) {
		t.Fatal("2.5 KB of resends is exactly the 25% earned")
	}
	if b.allow(1) {
		t.Fatal("that spent it all")
	}
}

func TestRetryBudget_FloorRefillsOverTimeSoQuietFlowsStillRecover(t *testing.T) {
	b, now := newTestRetryBudget()
	b.reset(0)
	*now = now.Add(time.Second)
	if !b.allow(retryBudgetFloorBytesPerSecond) {
		t.Fatal("one second must refill the floor rate")
	}
	if b.allow(1000) {
		t.Fatal("but not more than the floor rate")
	}
}

func TestRetryBudget_BurstIsCapped(t *testing.T) {
	b, now := newTestRetryBudget()
	b.reset(0)
	b.earn(1 << 40)
	*now = now.Add(24 * time.Hour)
	if !b.allow(retryBudgetBurstBytes) {
		t.Fatal("a full bucket must pay out its burst")
	}
	if b.allow(1) {
		t.Fatal("the bucket never holds more than the burst, however long it sits")
	}
}

func TestRetryBudget_RefusalsAreNotCharged(t *testing.T) {
	b, _ := newTestRetryBudget()
	b.reset(100)
	if b.allow(1000) {
		t.Fatal("too big")
	}
	if !b.allow(100) {
		t.Fatal("a refused resend must leave the tokens alone")
	}
}

// The incident: a peer that never acks. Every unacked frame used to be resent up
// to 16 times, so a node sent many times what it delivered. With the budget the
// resends are bounded by what was first sent plus a small floor.
func TestSendSequence_ResendsToANonAckingPeerAreBudgeted(t *testing.T) {
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 2 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 4 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = time.Minute
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	// a route that swallows everything and never acks: the slow far end
	sink := make(chan []byte, 1<<16)
	client.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sink})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sink:
			}
		}
	}()
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, clientSettings.SendBufferSettings)

	const items = 200
	const frameBytes = 1000
	for i := 0; i < items; i++ {
		item := &sendItem{
			transferItem: transferItem{
				messageId:        NewId(),
				sequenceNumber:   uint64(i + 1),
				messageByteCount: frameBytes,
			},
			sendTime:           time.Now(),
			resendTime:         time.Now().Add(-time.Second),
			sendCount:          1,
			head:               true,
			transferFrameBytes: make([]byte, frameBytes),
			ackCallback:        func(err error) {},
		}
		seq.sendItems = append(seq.sendItems, item)
		seq.resendQueue.Add(item)
	}

	// 20 KB to spend, nothing first-sent: only the grant and the floor may pay
	transferResendBudget.reset(20_000)
	before := TransferSendStatsNow()

	go HandleError(func() { seq.Run() })
	time.Sleep(600 * time.Millisecond)

	after := TransferSendStatsNow()
	resent := after.ResendBytes - before.ResendBytes
	// 20 KB grant + 64 KiB/s floor for 0.6 s (about 39 KB), with slack for the loop
	floorRate := float64(retryBudgetFloorBytesPerSecond)
	limit := int64(20_000 + 0.6*floorRate + 3*frameBytes)
	if resent > limit {
		t.Fatalf("resent %d bytes, the budget allows about %d (unbudgeted this would be ~%d)", resent, limit, items*15*frameBytes)
	}
	if resent == 0 {
		t.Fatal("the budget must still let some resends through")
	}
	if after.ResendDeferredFrames-before.ResendDeferredFrames == 0 {
		t.Fatal("refused resends must be counted as deferred")
	}

	// earning from first sends releases more
	transferResendBudget.earn(400_000) // +100 KB
	time.Sleep(400 * time.Millisecond)
	if got := TransferSendStatsNow().ResendBytes - after.ResendBytes; got < 40_000 {
		t.Fatalf("earned budget should have released more resends, only %d bytes went", got)
	}
}

// shadow and off never delay a resend
func TestSendSequence_ResendBudgetIsOnlyEnforcedInEnforceMode(t *testing.T) {
	defer setGovernModeForTest(governShadow)()
	transferResendBudget.reset(0)
	if !retryAllowed(1000) {
		t.Fatal("shadow mode must never refuse a resend")
	}
	defer setGovernModeForTest(governOff)()
	if !retryAllowed(1000) {
		t.Fatal("off mode must never refuse a resend")
	}
	restore := setGovernModeForTest(governEnforce)
	defer restore()
	if retryAllowed(1000) {
		t.Fatal("enforce mode refuses when the budget is empty")
	}
}
