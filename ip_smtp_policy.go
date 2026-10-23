package connect

import (
	"bytes"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// SMTP routing and encryption policy is enforced before the general CFAA
// policy. Port 25 is deliberately local-only. Port 465 must begin with a TLS
// ClientHello; port 587 may send only bounded SMTP negotiation until STARTTLS
// is immediately followed by a TLS ClientHello.
const (
	smtpLocalPort               = 25
	smtpImplicitTlsPort         = 465
	smtpStartTlsPort            = 587
	smtpTlsRecordHeaderBytes    = 5
	smtpTlsHandshakeHeaderBytes = 4
	smtpMaxTlsRecordBytes       = 16 * 1024
	// The whole ClientHello is buffered and validated before a flow counts as
	// secure. Upstream allows 64 KiB of ClientHello (68 KiB on the wire); meso
	// caps it at one full record so that a client holding many half-sent
	// hellos open pins at most about 17 KiB per flow (the table holds 1024).
	// Real SMTP clients send a few KiB, post-quantum key shares included.
	smtpMaxTlsClientHelloBodyBytes = smtpMaxTlsRecordBytes - smtpTlsHandshakeHeaderBytes
	smtpMaxTlsClientHelloWireBytes = 17 * 1024
	// After a flow turns secure only this much of a 465 stream is kept, as the
	// retransmission guard (see inspectSecureRetransmission).
	smtpTlsClientHelloPrefixBytes = 9
	smtpMaxNegotiationBytes       = 2048
	smtpMaxCommandLineBytes       = 510 // RFC 5321's 512 includes CRLF.
	smtpMaxFlowCount              = 1024
	// One remote client may hold at most this many SMTP flows on a provider,
	// so one sender cannot fill the table for every other client.
	smtpMaxOwnerFlowCount = 128
	// Abandoned tuples (no RST seen) are retired after this long idle,
	// checked at most once per reap interval as packets arrive.
	smtpFlowIdleTimeout      = 5 * time.Minute
	smtpFlowIdleReapInterval = time.Minute
)

type smtpEgressVerdict uint8

const (
	smtpEgressNotApplicable smtpEgressVerdict = iota
	smtpEgressAllow
	smtpEgressReject
	// smtpEgressRejectLatched means the flow was already rejected and the
	// reset was already sent. Callers must drop silently, not re-send a
	// reset (avoids provider egress amplification for a client that ignores
	// the RST and keeps transmitting).
	smtpEgressRejectLatched
)

// permitsSend reports whether the client send path may forward the packet. A
// packet the SMTP guard does not govern (every non-SMTP flow) is permitted:
// only an explicit reject, or a latched reject, stops it. Treating
// smtpEgressNotApplicable as a refusal dropped all ordinary client traffic.
func (self smtpEgressVerdict) permitsSend() bool {
	return self == smtpEgressAllow || self == smtpEgressNotApplicable
}

func smtpRoutesLocally(ipPath *IpPath) bool {
	return ipPath != nil &&
		ipPath.Protocol == IpProtocolTcp &&
		ipPath.DestinationPort == smtpLocalPort
}

func smtpNeedsEncryptionInspection(ipPath *IpPath) bool {
	if ipPath == nil || ipPath.Protocol != IpProtocolTcp {
		return false
	}
	return ipPath.DestinationPort == smtpImplicitTlsPort ||
		ipPath.DestinationPort == smtpStartTlsPort
}

// smtpFlowKey is the exact outbound tuple. The IP version is explicit so an
// IPv4 flow cannot collide with an IPv4-mapped IPv6 flow.
type smtpFlowKey struct {
	// ownerId namespaces provider-side flows by the authenticated remote
	// client. Tunnel clients can reuse the same virtual IP address and TCP
	// tuple, so the provider must not let one client's verdict latch another
	// client's connection. Client-side guards use the zero Id.
	ownerId         Id
	sourceIp        [net.IPv6len]byte
	destinationIp   [net.IPv6len]byte
	sourcePort      uint16
	destinationPort uint16
	ipVersion       uint8
}

func smtpFlowKeyForOwnerPath(ownerId Id, ipPath *IpPath) (smtpFlowKey, bool) {
	if ipPath == nil ||
		ipPath.Protocol != IpProtocolTcp ||
		ipPath.SourcePort < 0 || 65535 < ipPath.SourcePort ||
		ipPath.DestinationPort < 0 || 65535 < ipPath.DestinationPort {
		return smtpFlowKey{}, false
	}

	var sourceIp net.IP
	var destinationIp net.IP
	switch ipPath.Version {
	case 4:
		sourceIp = ipPath.SourceIp.To4()
		destinationIp = ipPath.DestinationIp.To4()
	case 6:
		sourceIp = ipPath.SourceIp.To16()
		destinationIp = ipPath.DestinationIp.To16()
	default:
		return smtpFlowKey{}, false
	}
	if sourceIp == nil || destinationIp == nil {
		return smtpFlowKey{}, false
	}

	key := smtpFlowKey{
		ownerId:         ownerId,
		sourcePort:      uint16(ipPath.SourcePort),
		destinationPort: uint16(ipPath.DestinationPort),
		ipVersion:       uint8(ipPath.Version),
	}
	copy(key.sourceIp[:], sourceIp)
	copy(key.destinationIp[:], destinationIp)
	return key, true
}

type smtp587Phase uint8

const (
	smtp587Negotiating smtp587Phase = iota
	smtp587ExpectClientHello
)

type smtpFlowState struct {
	destinationPort int

	synSeen bool
	synSeq  uint32

	baseSequence uint32
	baseSet      bool
	stream       []byte
	parseOffset  int
	phase587     smtp587Phase

	secure       bool
	rejected     bool
	lastUsed     uint64
	lastUsedTime time.Time
}

// smtpEgressGuard keeps only the bounded, pre-encryption prefix of each SMTP
// flow. Once TLS is identified the prefix remains as a retransmission guard:
// bytes that overlap the validated negotiation must match, while later TLS
// sequence space stays opaque. A fresh SYN replaces the marker on tuple reuse.
//
// One lock is intentionally sufficient: it is touched only by ports 465/587,
// and a single SMTP stream's packets are ordered through the lock. The map and
// every field have useful zero values, which keeps fixture-built clients safe.
type smtpEgressGuard struct {
	stateLock        sync.Mutex
	flows            map[smtpFlowKey]*smtpFlowState
	clock            uint64
	timeNow          func() time.Time
	nextIdleReapTime time.Time
}

func (self *smtpEgressGuard) inspect(ipPath *IpPath, payload []byte) smtpEgressVerdict {
	return self.inspectForOwner(Id{}, ipPath, payload)
}

// inspectForOwner is the provider-side form of inspect. The authenticated
// source id is part of the key because separate remote clients commonly use
// identical tunnel addresses and ephemeral TCP tuples.
func (self *smtpEgressGuard) inspectForOwner(
	ownerId Id,
	ipPath *IpPath,
	payload []byte,
) smtpEgressVerdict {
	if !smtpNeedsEncryptionInspection(ipPath) {
		return smtpEgressNotApplicable
	}
	key, ok := smtpFlowKeyForOwnerPath(ownerId, ipPath)
	if !ok {
		return smtpEgressReject
	}

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.flows == nil {
		self.flows = map[smtpFlowKey]*smtpFlowState{}
	}
	now := self.currentTimeWithLock()
	self.reapIdleFlowsWithLock(now)
	if ipPath.Rst {
		delete(self.flows, key)
		return smtpEgressAllow
	}

	flow := self.flows[key]
	segmentSequence := ipPath.SequenceNumber
	if ipPath.Syn {
		segmentSequence += 1
		if flow == nil || !flow.synSeen || flow.synSeq != ipPath.SequenceNumber {
			flow = self.newFlowWithLock(key, ipPath.DestinationPort)
			if flow == nil {
				return smtpEgressReject
			}
			flow.synSeen = true
			flow.synSeq = ipPath.SequenceNumber
			flow.baseSequence = segmentSequence
			flow.baseSet = true
		}
	} else if flow == nil {
		flow = self.newFlowWithLock(key, ipPath.DestinationPort)
		if flow == nil {
			return smtpEgressReject
		}
	}

	self.clock += 1
	flow.lastUsed = self.clock
	flow.lastUsedTime = now
	if flow.rejected {
		return smtpEgressRejectLatched
	}
	if len(payload) == 0 {
		return smtpEgressAllow
	}
	if !flow.baseSet {
		flow.baseSequence = segmentSequence
		flow.baseSet = true
	}
	if !flow.inspectPayload(segmentSequence, payload) {
		flow.rejected = true
		// a rejected flow is latched and never inspected again
		flow.stream = nil
		return smtpEgressReject
	}
	return smtpEgressAllow
}

// newFlowWithLock admits state for a new tuple, or returns nil when there is
// no room. A full owner share (provider side) or a full table makes room only
// by evicting one of the inserting owner's own flows; another owner's flow is
// never evicted (that would kill another client's live session). With no room
// the packet is rejected, as upstream does, rather than passed unguarded.
func (self *smtpEgressGuard) newFlowWithLock(key smtpFlowKey, destinationPort int) *smtpFlowState {
	if _, replacing := self.flows[key]; !replacing {
		if key.ownerId != (Id{}) && smtpMaxOwnerFlowCount <= self.ownerFlowCountWithLock(key.ownerId) {
			if !self.evictFlowWithLock(key.ownerId) {
				return nil
			}
		}
		if smtpMaxFlowCount <= len(self.flows) {
			if !self.evictFlowWithLock(key.ownerId) {
				return nil
			}
		}
	}
	flow := &smtpFlowState{destinationPort: destinationPort}
	self.flows[key] = flow
	return flow
}

// currentTimeWithLock returns the guard's time source. Caller holds stateLock.
func (self *smtpEgressGuard) currentTimeWithLock() time.Time {
	if self.timeNow != nil {
		return self.timeNow()
	}
	return time.Now()
}

// reapIdleFlowsWithLock retires tuples idle past smtpFlowIdleTimeout, at most
// once per smtpFlowIdleReapInterval. Driven by packets, so there is no
// goroutine; it runs before a new flow is admitted. Caller holds stateLock.
func (self *smtpEgressGuard) reapIdleFlowsWithLock(now time.Time) {
	if !self.nextIdleReapTime.IsZero() && now.Before(self.nextIdleReapTime) {
		return
	}
	for key, flow := range self.flows {
		if !flow.lastUsedTime.IsZero() && !now.Before(flow.lastUsedTime.Add(smtpFlowIdleTimeout)) {
			delete(self.flows, key)
		}
	}
	self.nextIdleReapTime = now.Add(smtpFlowIdleReapInterval)
}

// ownerFlowCountWithLock counts ownerId's flows. Caller holds stateLock.
func (self *smtpEgressGuard) ownerFlowCountWithLock(ownerId Id) int {
	count := 0
	for key := range self.flows {
		if key.ownerId == ownerId {
			count += 1
		}
	}
	return count
}

// evictFlowWithLock removes one flow to make room for a new one. It prefers
// expendable entries belonging to the INSERTING OWNER: rejected flows
// (secure or not) and still-negotiating flows. It preserves an established,
// non-rejected secure flow unless every entry in the table is one; losing its
// marker would make the next opaque TLS record look like a fresh plaintext
// negotiation, and the provider would reset a valid session. Rejected flows
// are safe to evict (a retry is re-inspected) and negotiating flows re-parse
// their bounded prefix. Both passes are full scans so the choice is
// deterministic.
//
// Scoping the scan to the inserting owner's flows means one client can never
// evict another client's flow. A client that fills its own share and has no
// expendable flow simply cannot add more until one of its own flows becomes
// expendable (or the owner's insertion returns nil and the flow is unguarded).
// The global 1024-flow cap is unchanged, so total memory stays bounded.
func (self *smtpEgressGuard) evictFlowWithLock(ownerId Id) bool {
	var oldestKey smtpFlowKey
	var oldestUse uint64
	found := false
	for candidateKey, candidate := range self.flows {
		if candidateKey.ownerId != ownerId {
			continue
		}
		if candidate.secure && !candidate.rejected {
			continue
		}
		if !found || candidate.lastUsed < oldestUse {
			oldestKey = candidateKey
			oldestUse = candidate.lastUsed
			found = true
		}
	}
	if !found {
		for candidateKey, candidate := range self.flows {
			if candidateKey.ownerId != ownerId {
				continue
			}
			if !found || candidate.lastUsed < oldestUse {
				oldestKey = candidateKey
				oldestUse = candidate.lastUsed
				found = true
			}
		}
	}
	if found {
		delete(self.flows, oldestKey)
		return true
	}
	return false
}

func (self *smtpFlowState) inspectPayload(sequence uint32, payload []byte) bool {
	if self.secure {
		return self.inspectSecureRetransmission(sequence, payload)
	}

	// TCP serial arithmetic is safe here because the retained prefix is at
	// most about 19 KiB, far below the half-sequence-space ambiguity boundary.
	relative := int64(int32(sequence - self.baseSequence))
	if relative < 0 || int64(len(self.stream)) < relative {
		// A gap cannot be forwarded safely: bytes hidden in the gap could turn
		// an apparently harmless suffix into AUTH or plaintext credentials.
		return false
	}

	offset := int(relative)
	overlap := len(self.stream) - offset
	if len(payload) < overlap {
		overlap = len(payload)
	}
	if 0 < overlap && !bytes.Equal(self.stream[offset:offset+overlap], payload[:overlap]) {
		// A retransmission must reproduce the bytes already validated for that
		// sequence range. Conflicting overlap is a fail-closed stream splice.
		return false
	}
	if overlap == len(payload) {
		return true
	}

	newBytes := payload[overlap:]
	limit := smtpMaxTlsClientHelloWireBytes
	if self.destinationPort == smtpStartTlsPort {
		limit += smtpMaxNegotiationBytes
	}
	remainingCapacity := limit - len(self.stream)
	retainedBytes := newBytes
	if remainingCapacity < len(retainedBytes) {
		retainedBytes = retainedBytes[:remainingCapacity]
	}
	self.stream = append(self.stream, retainedBytes...)

	var valid bool
	switch self.destinationPort {
	case smtpImplicitTlsPort:
		valid, self.secure = tlsClientHelloStream(self.stream)
	case smtpStartTlsPort:
		valid, self.secure = self.inspect587Stream()
	default:
		return false
	}
	if self.secure {
		// Keep only a short verified prefix, in a buffer of its own so the
		// ClientHello buffer is freed. It prevents a conflicting retransmission
		// from replacing the negotiation bytes after the connection has moved
		// into opaque TLS sequence space.
		retain := smtpTlsClientHelloPrefixBytes
		if self.destinationPort == smtpStartTlsPort {
			retain = smtpMaxNegotiationBytes
		}
		self.stream = append([]byte(nil), self.stream[:min(retain, len(self.stream))]...)
		return valid
	}
	// More unverified bytes than the bounded prefix can represent fail closed.
	return valid && len(retainedBytes) == len(newBytes)
}

func (self *smtpFlowState) inspectSecureRetransmission(sequence uint32, payload []byte) bool {
	// The connection may legitimately advance past half of the TCP sequence
	// space over its lifetime, so the offset is unsigned here: once the flow
	// is secure only overlap with the retained negotiation prefix matters,
	// and any segment outside that prefix is opaque TLS data. The offset
	// stays uint32 so the comparison is correct on 32-bit platforms (a cast
	// to int before the bound check would wrap negative past 2^31 and panic
	// on the slice). The signed arithmetic stays in the negotiation phase,
	// where the bounded prefix makes gap detection fail closed. Deliberate
	// divergence from upstream, whose signed check falsely rejects (and
	// latches) a secure flow after 2^31 bytes of sequence space. One
	// residual edge remains: a flow that advances past a full 2^32 bytes
	// wraps the offset back into the prefix range and is compared against
	// the retained bytes, which can reject at exactly the wrap. That is far
	// beyond realistic SMTP session sizes and is inherent to not tracking
	// total progress after TLS starts.
	offset := uint32(sequence - self.baseSequence)
	if uint32(len(self.stream)) <= offset {
		return true
	}
	start := int(offset) // offset < len(stream) <= 2048, safe on 32-bit
	overlap := len(self.stream) - start
	if len(payload) < overlap {
		overlap = len(payload)
	}
	return bytes.Equal(self.stream[start:start+overlap], payload[:overlap])
}

// tlsHandshakeRecordHeaderPrefix validates an incomplete or complete TLS
// Handshake record header.
func tlsHandshakeRecordHeaderPrefix(header []byte) bool {
	for index, value := range header {
		switch index {
		case 0:
			if value != 0x16 {
				return false
			}
		case 1:
			if value != 0x03 {
				return false
			}
		case 2:
			if value < 0x01 || 0x04 < value {
				return false
			}
		}
	}
	if smtpTlsRecordHeaderBytes <= len(header) {
		recordBytes := int(binary.BigEndian.Uint16(header[3:5]))
		if recordBytes == 0 || smtpMaxTlsRecordBytes < recordBytes {
			return false
		}
	}
	return true
}

// validTlsClientHelloBody validates every length-delimited field in a complete
// ClientHello, including the extension vector and duplicate-extension rule.
func validTlsClientHelloBody(body []byte) bool {
	var extensionTypes [1024]uint64
	return validTlsClientHelloBodyWithScratch(body, &extensionTypes)
}

func validTlsClientHelloBodyWithScratch(body []byte, extensionTypes *[1024]uint64) bool {
	if len(body) < 41 || body[0] != 0x03 || body[1] < 0x01 || 0x03 < body[1] {
		return false
	}
	offset := 2 + 32
	sessionIdBytes := int(body[offset])
	offset += 1
	if 32 < sessionIdBytes || len(body) < offset+sessionIdBytes+2 {
		return false
	}
	offset += sessionIdBytes
	cipherSuiteBytes := int(binary.BigEndian.Uint16(body[offset : offset+2]))
	offset += 2
	if cipherSuiteBytes < 2 || cipherSuiteBytes%2 != 0 || len(body) < offset+cipherSuiteBytes+1 {
		return false
	}
	offset += cipherSuiteBytes
	compressionMethodBytes := int(body[offset])
	offset += 1
	if compressionMethodBytes == 0 || len(body) < offset+compressionMethodBytes {
		return false
	}
	offset += compressionMethodBytes
	if offset == len(body) {
		return true
	}
	if len(body) < offset+2 {
		return false
	}
	extensionBytes := int(binary.BigEndian.Uint16(body[offset : offset+2]))
	offset += 2
	if extensionBytes != len(body)-offset {
		return false
	}
	// All extension types fit this exact 8-KiB bitset. An attacker can send
	// thousands of zero-length extensions; a map scales with that count.
	clear(extensionTypes[:])
	for offset < len(body) {
		if len(body) < offset+4 {
			return false
		}
		extensionType := binary.BigEndian.Uint16(body[offset : offset+2])
		extensionDataBytes := int(binary.BigEndian.Uint16(body[offset+2 : offset+4]))
		offset += 4
		word, bit := extensionType/64, uint64(1)<<(extensionType%64)
		if extensionTypes[word]&bit != 0 || len(body) < offset+extensionDataBytes {
			return false
		}
		extensionTypes[word] |= bit
		offset += extensionDataBytes
	}
	return true
}

// tlsClientHelloStream validates complete TLS records until a complete,
// structurally valid ClientHello has arrived. Handshake bytes may span records;
// both logical and wire sizes are bounded before the flow can become secure.
func tlsClientHelloStream(stream []byte) (valid bool, complete bool) {
	if smtpMaxTlsClientHelloWireBytes < len(stream) {
		return false, false
	}
	handshake := make([]byte, 0, len(stream))
	for recordOffset := 0; ; {
		remaining := stream[recordOffset:]
		if len(remaining) < smtpTlsRecordHeaderBytes {
			return tlsHandshakeRecordHeaderPrefix(remaining), false
		}
		header := remaining[:smtpTlsRecordHeaderBytes]
		if !tlsHandshakeRecordHeaderPrefix(header) {
			return false, false
		}
		recordBytes := int(binary.BigEndian.Uint16(header[3:5]))
		recordEnd := recordOffset + smtpTlsRecordHeaderBytes + recordBytes
		if smtpMaxTlsClientHelloWireBytes < recordEnd {
			return false, false
		}
		if len(stream) < recordEnd {
			handshake = append(handshake, stream[recordOffset+smtpTlsRecordHeaderBytes:]...)
			if 0 < len(handshake) && handshake[0] != 0x01 {
				return false, false
			}
			if smtpTlsHandshakeHeaderBytes <= len(handshake) {
				handshakeBodyBytes := int(handshake[1])<<16 |
					int(handshake[2])<<8 |
					int(handshake[3])
				if handshakeBodyBytes < 41 || smtpMaxTlsClientHelloBodyBytes < handshakeBodyBytes {
					return false, false
				}
			}
			return true, false
		}
		handshake = append(handshake, stream[recordOffset+smtpTlsRecordHeaderBytes:recordEnd]...)
		if 0 < len(handshake) && handshake[0] != 0x01 {
			return false, false
		}
		if smtpTlsHandshakeHeaderBytes <= len(handshake) {
			handshakeBodyBytes := int(handshake[1])<<16 |
				int(handshake[2])<<8 |
				int(handshake[3])
			if handshakeBodyBytes < 41 || smtpMaxTlsClientHelloBodyBytes < handshakeBodyBytes {
				return false, false
			}
			handshakeEnd := smtpTlsHandshakeHeaderBytes + handshakeBodyBytes
			if handshakeEnd <= len(handshake) {
				body := handshake[smtpTlsHandshakeHeaderBytes:handshakeEnd]
				if !validTlsClientHelloBody(body) {
					return false, false
				}
				return true, true
			}
		}
		recordOffset = recordEnd
		if recordOffset == len(stream) {
			return true, false
		}
	}
}

func (self *smtpFlowState) inspect587Stream() (valid bool, secure bool) {
	for {
		if self.phase587 == smtp587ExpectClientHello {
			return tlsClientHelloStream(self.stream[self.parseOffset:])
		}
		if self.parseOffset == len(self.stream) {
			return true, false
		}

		remaining := self.stream[self.parseOffset:]
		lineEnd := bytes.Index(remaining, []byte("\r\n"))
		if lineEnd < 0 {
			return len(self.stream) <= smtpMaxNegotiationBytes &&
				validPartialSmtpNegotiationLine(remaining), false
		}
		if smtpMaxCommandLineBytes < lineEnd ||
			bytes.IndexByte(remaining[:lineEnd], '\r') >= 0 ||
			bytes.IndexByte(remaining[:lineEnd], '\n') >= 0 {
			return false, false
		}

		command, ok := completeSmtpNegotiationCommand(remaining[:lineEnd])
		if !ok {
			return false, false
		}
		self.parseOffset += lineEnd + 2
		if smtpMaxNegotiationBytes < self.parseOffset {
			return false, false
		}
		if command == smtpCommandStartTls {
			self.phase587 = smtp587ExpectClientHello
		}
	}
}

type smtpCommand uint8

const (
	smtpCommandEhlo smtpCommand = iota
	smtpCommandHelo
	smtpCommandQuit
	smtpCommandStartTls
)

var smtpNegotiationCommands = [...]struct {
	name    string
	command smtpCommand
}{
	// Keep the pre-TLS vocabulary deliberately smaller than SMTP itself:
	// identify the client, request TLS, or leave. Transaction commands and
	// extensible commands such as NOOP are unnecessary before encryption and
	// could otherwise carry arbitrary caller-supplied text.
	{name: "EHLO", command: smtpCommandEhlo},
	{name: "HELO", command: smtpCommandHelo},
	{name: "QUIT", command: smtpCommandQuit},
	{name: "STARTTLS", command: smtpCommandStartTls},
}

func validPartialSmtpNegotiationLine(line []byte) bool {
	if smtpMaxCommandLineBytes+1 < len(line) {
		return false
	}
	if 0 < len(line) && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if smtpMaxCommandLineBytes < len(line) {
		return false
	}
	if bytes.IndexByte(line, '\r') >= 0 || bytes.IndexByte(line, '\n') >= 0 ||
		!validSmtpAscii(line) {
		return false
	}

	token, argument, separated := splitSmtpCommand(line)
	if !separated {
		for _, candidate := range smtpNegotiationCommands {
			if asciiCasePrefix(candidate.name, token) {
				return true
			}
		}
		return false
	}

	command, ok := smtpNegotiationCommand(token)
	if !ok {
		return false
	}
	switch command {
	case smtpCommandEhlo, smtpCommandHelo:
		return true
	case smtpCommandQuit, smtpCommandStartTls:
		return len(bytes.Trim(argument, " \t")) == 0
	default:
		return false
	}
}

func completeSmtpNegotiationCommand(line []byte) (smtpCommand, bool) {
	if len(line) == 0 || !validSmtpAscii(line) {
		return 0, false
	}
	token, argument, _ := splitSmtpCommand(line)
	command, ok := smtpNegotiationCommand(token)
	if !ok {
		return 0, false
	}
	switch command {
	case smtpCommandEhlo, smtpCommandHelo:
		// Constrain the argument to a real EHLO/HELO shape: a domain or an
		// [addr-literal], e.g. "mail.example.com" or "[1.2.3.4]". Unbounded
		// arbitrary printable text (up to 510 bytes) would make 587 a
		// newly-reachable arbitrary-ASCII relay channel.
		if !validSmtpHeloArgument(argument) {
			return 0, false
		}
	case smtpCommandQuit, smtpCommandStartTls:
		if len(bytes.Trim(argument, " \t")) != 0 {
			return 0, false
		}
	}
	return command, true
}

// validSmtpHeloArgument accepts a domain (letters, digits, dots, hyphens) or
// an [addr-literal] (IPv4 / bracketed IPv6). Rejects spaces, control
// characters, and anything that is not a plausible hostname.
func validSmtpHeloArgument(argument []byte) bool {
	argument = bytes.Trim(argument, " \t")
	if len(argument) == 0 {
		return false
	}
	if argument[0] == '[' {
		// [addr-literal]: non-empty, no spaces, ends with ].
		if len(argument) < 3 || argument[len(argument)-1] != ']' {
			return false
		}
		inner := argument[1 : len(argument)-1]
		return len(inner) > 0 && validSmtpHostname(inner, true)
	}
	return validSmtpHostname(argument, false)
}

// validSmtpHostname checks hostname chars: alnum, dot, hyphen (and colon for
// IPv6 literals). No spaces or control bytes.
func validSmtpHostname(s []byte, allowColon bool) bool {
	for _, b := range s {
		if 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '.' || b == '-' {
			continue
		}
		if allowColon && b == ':' {
			continue
		}
		return false
	}
	return true
}

func splitSmtpCommand(line []byte) (token []byte, argument []byte, separated bool) {
	for index, value := range line {
		if value == ' ' || value == '\t' {
			return line[:index], line[index+1:], true
		}
	}
	return line, nil, false
}

func smtpNegotiationCommand(token []byte) (smtpCommand, bool) {
	for _, candidate := range smtpNegotiationCommands {
		if asciiCaseEqual(candidate.name, token) {
			return candidate.command, true
		}
	}
	return 0, false
}

func asciiCasePrefix(candidate string, prefix []byte) bool {
	if len(candidate) < len(prefix) {
		return false
	}
	return asciiCaseEqual(candidate[:len(prefix)], prefix)
}

func asciiCaseEqual(candidate string, value []byte) bool {
	if len(candidate) != len(value) {
		return false
	}
	for index, b := range value {
		if 'a' <= b && b <= 'z' {
			b -= 'a' - 'A'
		}
		if candidate[index] != b {
			return false
		}
	}
	return true
}

func validSmtpAscii(value []byte) bool {
	for _, b := range value {
		if b == '\t' {
			continue
		}
		if b < 0x20 || 0x7e < b {
			return false
		}
	}
	return true
}

// tcpRstForPolicyReject returns an RFC 793 reset addressed to the source of a
// rejected TCP packet. It shares the LocalUserNat orphan-reset builder so ACK
// and sequence behavior, IP checksums, and TCP checksums stay identical.
func tcpRstForPolicyReject(packet []byte) []byte {
	if len(packet) == 0 {
		return nil
	}
	ipVersion := int(packet[0] >> 4)
	var ipProtocol ipProtocolNumber
	var sourceIp net.IP
	var destinationIp net.IP
	var transport []byte
	var ok bool
	switch ipVersion {
	case 4:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv4(packet)
	case 6:
		ipProtocol, sourceIp, destinationIp, transport, ok = parseIpv6(packet)
	default:
		return nil
	}
	if !ok || ipProtocol != ipProtocolNumberTcp {
		return nil
	}
	var tcp parsedTcp
	if !parseTcpPacket(sourceIp, destinationIp, transport, &tcp) || tcp.rst {
		return nil
	}
	return tcpRstForOrphan(ipVersion, &tcp)
}

// deliverTcpPolicyReset builds an RFC 793 reset for a rejected TCP packet and
// hands it to the receive callback. The reset buffer is owned by this
// function and is valid only for the duration of the callback; the callback
// must not retain it. A callback that needs the reset beyond the call (for
// example to frame it for the tunnel) must take a read-only share, which the
// message pool refcount protects until the frame releases it. The reset bytes
// are reverse-addressed (destination back to source) while the passed ipPath
// is the original forward path; consumers must not key routing off the path
// alone. This matches the existing rstFlow delivery pattern.
func deliverTcpPolicyReset(
	receive ReceivePacketFunction,
	source TransferPath,
	provideMode protocol.ProvideMode,
	ipPath *IpPath,
	packet []byte,
) {
	reset := tcpRstForPolicyReject(packet)
	if reset == nil {
		return
	}
	defer MessagePoolReturn(reset)
	if receive != nil {
		HandleError(func() {
			receive(source, provideMode, ipPath, reset)
		})
	}
}

// tcpRstForOrphan returns an RFC 793 reset addressed to the source of the
// parsed TCP packet. Ported from upstream with the raw protocol number
// constant adapted to the fork's ipProtocolNumberTcp spelling.
func tcpRstForOrphan(ipVersion int, tcp *parsedTcp) []byte {
	var ipHeaderByteCount int
	switch ipVersion {
	case 4:
		ipHeaderByteCount = Ipv4HeaderSizeWithoutExtensions
	case 6:
		ipHeaderByteCount = Ipv6HeaderSize
	default:
		return nil
	}

	packet := MessagePoolGet(ipHeaderByteCount + TcpHeaderSizeWithoutExtensions)
	switch ipVersion {
	case 4:
		writeIpv4Header(packet, ipProtocolNumberTcp, tcp.destinationIp, tcp.sourceIp)
	case 6:
		writeIpv6Header(packet, ipProtocolNumberTcp, tcp.destinationIp, tcp.sourceIp)
	}

	t := packet[ipHeaderByteCount:]
	binary.BigEndian.PutUint16(t[0:2], uint16(tcp.destinationPort))
	binary.BigEndian.PutUint16(t[2:4], uint16(tcp.sourcePort))
	var seq uint32
	var ackNumber uint32
	flags := byte(tcpFlagRst)
	if tcp.ack {
		seq = tcp.ackNumber
	} else {
		ackNumber = tcp.seq + uint32(len(tcp.payload))
		if tcp.syn {
			ackNumber += 1
		}
		if tcp.fin {
			ackNumber += 1
		}
		flags |= tcpFlagAck
	}
	binary.BigEndian.PutUint32(t[4:8], seq)
	binary.BigEndian.PutUint32(t[8:12], ackNumber)
	// data offset, no options
	t[12] = byte(TcpHeaderSizeWithoutExtensions/4) << 4
	t[13] = flags
	// window
	t[14] = 0
	t[15] = 0
	// checksum, set below
	t[16] = 0
	t[17] = 0
	// urgent
	t[18] = 0
	t[19] = 0
	binary.BigEndian.PutUint16(t[16:18], transportChecksum(ipProtocolNumberTcp, tcp.destinationIp, tcp.sourceIp, t))
	return packet
}
