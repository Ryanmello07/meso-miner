package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// Tests for the stream lease port. The first three mirror upstream's
// transfer_stream_authorization_test.go; the stream lifecycle is observed
// through the meso buffer's fake-transport seam instead of upstream's tracker.

const testServerClock = int64(1700000000000)

// newAuthorizationTestClient returns a client whose stream sequences use fake
// transports, so opening a stream needs no WebRTC.
func newAuthorizationTestClient(t *testing.T, oob OutOfBandControl) *Client {
	t.Helper()
	settings := DefaultClientSettings()
	settings.StreamManagerSettings = &StreamManagerSettings{
		StreamBufferSettings: &StreamBufferSettings{
			ReadTimeout:  -1,
			WriteTimeout: -1,
			P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
				transport, _ := fakeChildTransport(sequence)
				return []*P2pTransport{transport}
			},
		},
	}
	client := NewClient(t.Context(), NewId(), oob, settings)
	t.Cleanup(client.Close)
	return client
}

// anchorClock installs a server clock anchor and a controllable now.
func anchorClock(manager *StreamManager, now *time.Time) {
	state := manager.authorization
	state.Lock()
	defer state.Unlock()
	state.now = func() time.Time { return *now }
	state.clockLocal = *now
	state.clockServer = testServerClock
}

func receiveControl(manager *StreamManager, message proto.Message) {
	frame := RequireToFrameWithDefaultProtocolVersion(message)
	defer MessagePoolReturn(frame.MessageBytes)
	manager.Receive(SourceId(ControlId), []*protocol.Frame{frame}, Peer{})
}

// awaitStreamClosed waits for the asynchronous reap that follows a cancel.
func awaitStreamClosed(t *testing.T, manager *StreamManager, streamId Id, message string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for manager.IsStreamOpen(streamId) {
		if time.Now().After(deadline) {
			t.Fatal(message)
		}
		time.Sleep(time.Millisecond)
	}
}

func leasedOpen(streamId Id, generation Id, peer Id) *protocol.StreamOpen {
	return &protocol.StreamOpen{
		StreamId:                        streamId.Bytes(),
		DestinationId:                   peer.Bytes(),
		AuthorizationGeneration:         generation.Bytes(),
		AuthorizationLeaseMillis:        90000,
		AuthorizationDeadlineUnixMillis: testServerClock + 90000,
	}
}

func TestStreamAuthorizationExpiryClosesStreamAndRejectsReplay(t *testing.T) {
	client := newAuthorizationTestClient(t, NewNoContractClientOob())
	manager := client.streamManager
	now := time.Now()
	anchorClock(manager, &now)
	state := manager.authorization

	sid, generation, peer := NewId(), NewId(), NewId()
	open := leasedOpen(sid, generation, peer)
	receiveControl(manager, open)
	if !manager.IsStreamOpen(sid) {
		t.Fatal("leased stream did not open")
	}

	state.Lock()
	record := state.records[sid]
	firstDeadline := record.deadline
	now = now.Add(20 * time.Second)
	state.Unlock()
	// An identical delayed control keeps its original absolute deadline.
	receiveControl(manager, open)
	state.Lock()
	if !state.records[sid].deadline.Equal(firstDeadline) {
		state.Unlock()
		t.Fatal("replay restarted the lease")
	}
	now = firstDeadline
	state.Unlock()

	manager.expireStreamAuthorization(sid, record)
	awaitStreamClosed(t, manager, sid, "expired lease left the stream open")

	// A delayed renewal cannot revive the retired generation, nor can a reset.
	open.AuthorizationDeadlineUnixMillis = testServerClock + 180000
	receiveControl(manager, open)
	receiveControl(manager, &protocol.StreamReset{Streams: []*protocol.StreamOpen{open}})
	if manager.IsStreamOpen(sid) {
		t.Fatal("renewal or reset resurrected a retired stream")
	}

	// An ordinary peer can never submit a new authoritative generation.
	peerOpen := proto.Clone(open).(*protocol.StreamOpen)
	peerOpen.StreamId = NewId().Bytes()
	frame := RequireToFrameWithDefaultProtocolVersion(peerOpen)
	manager.Receive(SourceId(peer), []*protocol.Frame{frame}, Peer{})
	MessagePoolReturn(frame.MessageBytes)
	state.Lock()
	defer state.Unlock()
	if len(state.records) != 1 {
		t.Fatal("peer supplied platform authority")
	}
}

func TestStreamAuthorizationClockDelayAndRetirementGc(t *testing.T) {
	client := newAuthorizationTestClient(t, NewNoContractClientOob())
	manager := client.streamManager
	state := manager.authorization
	now := time.Now()
	state.Lock()
	state.now = func() time.Time { return now }
	state.clockLocal = now.Add(-time.Second)
	state.clockServer = testServerClock
	state.Unlock()

	opens := 0
	run := func() error { opens++; return nil }
	first := &protocol.StreamOpen{StreamId: NewId().Bytes(), AuthorizationGeneration: NewId().Bytes(), AuthorizationLeaseMillis: 90000, AuthorizationDeadlineUnixMillis: testServerClock + 90000}
	for i := 0; i < maxStreamAuthorizationRecords; i++ {
		open := proto.Clone(first).(*protocol.StreamOpen)
		open.StreamId = NewId().Bytes()
		if err := manager.authorizeStreamOpen(open, run); err != nil {
			t.Fatal(err)
		}
	}
	if opens != maxStreamAuthorizationRecords {
		t.Fatal("unexpected admission count", opens)
	}
	// A full table refuses new admission without dropping records that could
	// authorize a delayed frame.
	_ = manager.authorizeStreamOpen(first, run)
	if opens != maxStreamAuthorizationRecords {
		t.Fatal("cap discarded a live tombstone")
	}
	// After every server binding lifetime ends, storage is reclaimed.
	state.Lock()
	now = now.Add(8*time.Hour + MaxStreamAuthorizationLease + StreamAuthorizationClockMargin + time.Second)
	state.Unlock()
	_ = manager.authorizeStreamOpen(first, run)
	if opens != maxStreamAuthorizationRecords {
		t.Fatal("expired delayed grant reopened after gc")
	}
	fresh := proto.Clone(first).(*protocol.StreamOpen)
	fresh.StreamId = NewId().Bytes()
	fresh.AuthorizationDeadlineUnixMillis = testServerClock + int64((8*time.Hour+5*time.Minute)/time.Millisecond)
	_ = manager.authorizeStreamOpen(fresh, run)
	if opens != maxStreamAuthorizationRecords+1 {
		t.Fatal("bounded gc permanently disabled future streams")
	}
}

// clockOob answers a StreamAuthorization clock request. Like the real OOB
// boundary it lends the response frames only until the callback returns.
type clockOob struct {
	serverMillis int64
	requests     chan struct{}
}

func (self *clockOob) SendControl(frames []*protocol.Frame, callback OobResultFunction) {
	request, err := FromFrame(frames[0])
	for _, frame := range frames {
		MessagePoolReturn(frame.MessageBytes)
	}
	if self.requests != nil {
		self.requests <- struct{}{}
	}
	if err != nil {
		callback(nil, err)
		return
	}
	clock, ok := request.(*protocol.StreamAuthorization)
	if !ok {
		callback(nil, nil)
		return
	}
	response := RequireToFrameWithDefaultProtocolVersion(&protocol.StreamAuthorization{ClockId: clock.ClockId, ClockUnixMillis: self.serverMillis})
	callback([]*protocol.Frame{response}, nil)
	clear(response.MessageBytes)
	MessagePoolReturn(response.MessageBytes)
}

func TestStreamAuthorizationClockBorrowsCallbackResponse(t *testing.T) {
	oob := &clockOob{serverMillis: testServerClock}
	client := newAuthorizationTestClient(t, oob)
	manager := client.streamManager
	manager.requestAuthorizationClock()
	state := manager.authorization
	state.Lock()
	defer state.Unlock()
	if state.clockServer != oob.serverMillis || state.clockLocal.IsZero() {
		t.Fatal("borrowed OOB response was retained beyond the callback")
	}
}

// A leased StreamOpen before any clock anchor waits (fail closed), asks the
// platform for its clock once, and opens when the anchor arrives.
func TestStreamAuthorizationPendingOpensAfterClock(t *testing.T) {
	oob := &clockOob{serverMillis: testServerClock, requests: make(chan struct{}, 4)}
	client := newAuthorizationTestClient(t, oob)
	manager := client.streamManager

	sid := NewId()
	receiveControl(manager, leasedOpen(sid, NewId(), NewId()))
	select {
	case <-oob.requests:
	case <-time.After(10 * time.Second):
		t.Fatal("no clock request for a pending grant")
	}
	deadline := time.Now().Add(10 * time.Second)
	for !manager.IsStreamOpen(sid) {
		if time.Now().After(deadline) {
			t.Fatal("pending grant never opened after the clock anchor")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An older platform sends StreamOpen without a generation; it opens at once
// and a StreamClose closes it, exactly as before the port.
func TestStreamAuthorizationLegacyOpenIsUnchanged(t *testing.T) {
	client := newAuthorizationTestClient(t, NewNoContractClientOob())
	manager := client.streamManager

	sid, peer := NewId(), NewId()
	receiveControl(manager, &protocol.StreamOpen{StreamId: sid.Bytes(), DestinationId: peer.Bytes()})
	if !manager.IsStreamOpen(sid) {
		t.Fatal("legacy StreamOpen did not open")
	}
	receiveControl(manager, &protocol.StreamClose{StreamId: sid.Bytes()})
	awaitStreamClosed(t, manager, sid, "legacy StreamClose did not close")
	// No lease state is retained for legacy streams.
	state := manager.authorization
	state.Lock()
	defer state.Unlock()
	if len(state.records) != 0 || len(state.pending) != 0 {
		t.Fatal("legacy stream left lease state behind")
	}
}

// A platform retirement frame closes the stream and fences the generation.
func TestStreamAuthorizationRetiredFrameClosesStream(t *testing.T) {
	client := newAuthorizationTestClient(t, NewNoContractClientOob())
	manager := client.streamManager
	now := time.Now()
	anchorClock(manager, &now)

	sid, generation := NewId(), NewId()
	open := leasedOpen(sid, generation, NewId())
	receiveControl(manager, open)
	if !manager.IsStreamOpen(sid) {
		t.Fatal("leased stream did not open")
	}
	receiveControl(manager, &protocol.StreamAuthorization{StreamId: sid.Bytes(), AuthorizationGeneration: generation.Bytes(), Retired: true})
	awaitStreamClosed(t, manager, sid, "retired generation left the stream open")
	receiveControl(manager, open)
	if manager.IsStreamOpen(sid) {
		t.Fatal("retired generation reopened")
	}
}

// Ending the client's context retires every lease and refuses new grants.
func TestStreamAuthorizationClosesWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), DefaultClientSettings())
	defer client.Close()
	manager := client.streamManager
	cancel()
	state := manager.authorization
	deadline := time.Now().Add(10 * time.Second)
	for {
		state.Lock()
		closed := state.closed
		state.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("authorization state did not close with the context")
		}
		time.Sleep(5 * time.Millisecond)
	}
	opened := false
	_ = manager.authorizeStreamOpen(leasedOpen(NewId(), NewId(), NewId()), func() error { opened = true; return nil })
	if opened {
		t.Fatal("closed state admitted a grant")
	}
}
