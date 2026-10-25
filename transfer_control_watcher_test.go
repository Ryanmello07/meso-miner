package connect

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// controlSyncWatcherCount counts goroutines parked in a ControlSync supersede
// watcher.
func controlSyncWatcherCount() int {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	count := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "(*ControlSync).Send.func") && strings.Contains(g, "transfer_control.go") &&
			!strings.Contains(g, "controlSync") {
			count += 1
		}
	}
	return count
}

// An acked send must not leave its supersede watcher parked: a bulk provider
// holds several long-lived scopes per proxy identity, and each used to keep a
// watcher until the scope's next Send.
func TestControlSyncReleasesWatcherAfterAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientA := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	controlClient := NewClientWithDefaults(ctx, ControlId, NewNoContractClientOob())
	controlClient.ContractManager().AddNoContractPeer(clientA.ClientId())

	aSend := make(chan []byte)
	aReceive := make(chan []byte)
	clientA.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{aSend})
	clientA.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{aReceive})
	controlClient.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{aReceive})
	controlClient.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{aSend})

	base := controlSyncWatcherCount()

	n := 20
	acked := make(chan struct{}, n)
	syncs := make([]*ControlSync, n)
	for i := range n {
		syncs[i] = NewControlSync(ctx, clientA, fmt.Sprintf("scope-%d", i))
		frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: uint32(i)}, DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		syncs[i].Send(frame, nil, func(err error) {
			if err == nil {
				acked <- struct{}{}
			}
		})
	}
	for range n {
		select {
		case <-acked:
		case <-time.After(30 * time.Second):
			t.Fatal("control sends were not acked")
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		c := controlSyncWatcherCount()
		if c <= base {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d supersede watchers still parked after %d acked sends (base %d)", c-base, n, base)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// the scopes stay usable after the release
	frame, err := ToFrame(&protocol.SimpleMessage{MessageIndex: 99}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	syncs[0].Send(frame, nil, func(err error) {
		if err == nil {
			acked <- struct{}{}
		}
	})
	select {
	case <-acked:
	case <-time.After(30 * time.Second):
		t.Fatal("a second send on a released scope was not acked")
	}
}
