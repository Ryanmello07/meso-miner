package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// The retained-byte budget is charged and released with exactly the same amount,
// through real admission, a cumulative ack and a drop. It used to be charged with
// the payload size and released with the (larger) wire frame size, drifting
// negative by the framing overhead of every retained item and loosening the cap.
func TestRetainedBudget_ChargeAndReleaseBalanceThroughRealAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultClientSettings()
	settings.EncryptionSettings.Mode = EncryptionModeOff
	settings.ControlPingTimeout = 0
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer client.Close()
	sink := make(chan []byte, 1<<12)
	transport := NewSendGatewayTransport()
	client.RouteManager().UpdateTransport(transport, []Route{sink})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sink:
			}
		}
	}()
	sendBuffer := NewSendBuffer(ctx, client, settings.SendBufferSettings)
	seq := NewSendSequence(ctx, client, sendBuffer, DestinationId(NewId()), MultiHopId{}, false, true, sequenceTlsRoleClient, false, settings.SendBufferSettings)

	const count = 6
	for i := 0; i < count; i++ {
		frame := RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "retained return payload"})
		seq.send(frame, func(error) {}, true, false, true)
	}
	if len(seq.sendItems) != count {
		t.Fatalf("admitted %d items, want %d", len(seq.sendItems), count)
	}
	charged := ByteCount(0)
	for _, item := range seq.sendItems {
		if !item.retainAfterAckTimeout || item.retainedCharge == 0 {
			t.Fatalf("an admitted retained item carries no charge: %+v", item)
		}
		charged += item.retainedCharge
	}
	if seq.retainedByteCount != charged {
		t.Fatalf("budget %d, want exactly what items were charged %d", seq.retainedByteCount, charged)
	}

	// cumulative ack of the first half, then drop one of the rest, then ack the rest
	seq.receiveAck(seq.sendItems[2].messageId, false, nil)
	if len(seq.sendItems) != count-3 {
		t.Fatalf("cumulative ack left %d items", len(seq.sendItems))
	}
	seq.dropItem(seq.sendItems[0], context.Canceled)
	seq.receiveAck(seq.sendItems[len(seq.sendItems)-1].messageId, false, nil)

	if seq.retainedByteCount != 0 {
		t.Fatalf("retained budget after releasing everything = %d, want exactly 0", seq.retainedByteCount)
	}
}
