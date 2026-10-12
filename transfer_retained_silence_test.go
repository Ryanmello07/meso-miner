package connect

import (
	"context"
	"testing"
	"time"
)

func newRetainedSilenceSequence(t *testing.T, retainedAckTimeout time.Duration) (*SendSequence, func()) {
	t.Helper()
	clientSettings := DefaultClientSettings()
	clientSettings.EncryptionSettings.Mode = EncryptionModeOff
	clientSettings.SendBufferSettings.MinResendInterval = 5 * time.Millisecond
	clientSettings.SendBufferSettings.MaxResendInterval = 10 * time.Millisecond
	clientSettings.SendBufferSettings.AckTimeout = time.Hour
	clientSettings.SendBufferSettings.IdleTimeout = time.Hour
	clientSettings.SendBufferSettings.RetainedAckTimeout = retainedAckTimeout

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	sendBuffer := NewSendBuffer(ctx, client, clientSettings.SendBufferSettings)
	seq := NewSendSequence(
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
	return seq, cancel
}

func addRetainedItem(seq *SendSequence, sequenceNumber uint64, ackCallback func(error)) *sendItem {
	item := &sendItem{
		transferItem: transferItem{
			messageId:        NewId(),
			sequenceNumber:   sequenceNumber,
			messageByteCount: 100,
		},
		sendTime:              time.Now(),
		resendTime:            time.Now().Add(time.Hour),
		sendCount:             1,
		head:                  sequenceNumber == 1,
		transferFrameBytes:    []byte("retained"),
		ackCallback:           ackCallback,
		retainAfterAckTimeout: true,
		backstopDeadline:      time.Now().Add(time.Hour),
	}
	seq.retainedByteCount += item.MessageByteCount()
	seq.sendItems = append(seq.sendItems, item)
	seq.resendQueue.Add(item)
	return item
}

// A destination that acknowledges nothing ends the sequence at
// RetainedAckTimeout, far before the per-item backstop, and fails the owner.
func TestRetainedSilenceExitsSequenceAndFailsOwner(t *testing.T) {
	seq, cancel := newRetainedSilenceSequence(t, 100*time.Millisecond)
	defer cancel()

	failed := make(chan error, 1)
	addRetainedItem(seq, 1, func(err error) { failed <- err })
	seq.retainedSilenceStart = time.Now()

	exited := make(chan struct{})
	go HandleError(func() {
		seq.Run()
		close(exited)
	})

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("retained owner failed with nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("silent destination did not end the retained sequence")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("sequence did not exit")
	}
}

// Zero keeps retention bound only by the per-item backstop.
func TestRetainedSilenceZeroKeepsRetention(t *testing.T) {
	seq, cancel := newRetainedSilenceSequence(t, 0)
	defer cancel()

	failed := make(chan error, 1)
	addRetainedItem(seq, 1, func(err error) { failed <- err })
	seq.retainedSilenceStart = time.Now().Add(-time.Hour)

	if seq.retainedDestinationSilent(time.Now()) {
		t.Fatal("zero RetainedAckTimeout must never report silence")
	}
	if _, ok := seq.retainedSilenceDeadline(); ok {
		t.Fatal("zero RetainedAckTimeout must have no deadline")
	}
}

// An acknowledgement restarts the clock, a gap shorter than the bound keeps
// the bytes, and an emptied queue forgets earlier silence.
func TestRetainedSilenceClockRules(t *testing.T) {
	seq, cancel := newRetainedSilenceSequence(t, time.Minute)
	defer cancel()

	item := addRetainedItem(seq, 1, func(error) {})
	start := time.Now()
	seq.retainedSilenceStart = start

	if seq.retainedDestinationSilent(start.Add(30 * time.Second)) {
		t.Fatal("a gap shorter than the bound must keep retained bytes")
	}
	if !seq.retainedDestinationSilent(start.Add(time.Minute)) {
		t.Fatal("silence at the bound must end the sequence")
	}

	// an acknowledgement of an outstanding item restarts the clock
	seq.receiveAck(item.messageId, false, nil)
	if !seq.retainedSilenceStart.IsZero() {
		t.Fatal("acknowledgement must clear the silence start")
	}

	// a new write after the acknowledgement starts a fresh clock
	addRetainedItem(seq, 2, func(error) {})
	seq.observeCarrierWrite(seq.sendItems[len(seq.sendItems)-1], transferWriteDisposition{})
	if seq.retainedSilenceStart.IsZero() {
		t.Fatal("first write after an acknowledgement must start the clock")
	}
	if seq.retainedDestinationSilent(time.Now().Add(30 * time.Second)) {
		t.Fatal("fresh clock must not be silent early")
	}

	// nothing outstanding: earlier silence does not carry over
	for _, queued := range seq.resendQueue.Clear() {
		_ = queued
	}
	seq.retainedSilenceStart = start
	if seq.retainedDestinationSilent(start.Add(time.Hour)) {
		t.Fatal("an empty queue cannot be silent")
	}
	if !seq.retainedSilenceStart.IsZero() {
		t.Fatal("an empty queue must forget earlier silence")
	}
}

// An acknowledgement that leaves other retained bytes unanswered restarts the
// silence window at the acknowledgement, so the bound does not wait for the next
// carrier write (which a parked resend can delay); once nothing retained is left
// the clock is cleared.
func TestRetainedSilenceRestartsAtTheAckWhileRetainedBytesRemain(t *testing.T) {
	seq, cancel := newRetainedSilenceSequence(t, time.Minute)
	defer cancel()

	first := addRetainedItem(seq, 1, func(error) {})
	second := addRetainedItem(seq, 2, func(error) {})
	seq.retainedSilenceStart = time.Now().Add(-50 * time.Second)

	before := time.Now()
	seq.receiveAck(first.messageId, false, nil)
	if seq.retainedSilenceStart.Before(before) {
		t.Fatal("an ack with retained bytes still outstanding must restart the silence window at the ack")
	}
	if _, ok := seq.retainedSilenceDeadline(); !ok {
		t.Fatal("the deadline must stay armed while retained bytes are outstanding")
	}

	seq.receiveAck(second.messageId, false, nil)
	if !seq.retainedSilenceStart.IsZero() {
		t.Fatal("with no retained bytes left the clock must be cleared")
	}
}
