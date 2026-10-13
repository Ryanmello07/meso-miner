package connect

// The outbound half of IPv6 fragmentation: converted from upstream, one
// complete IPv6 packet with no extension headers becomes RFC 8200 fragments.
// Oversized UDP replies are fragmented here instead of being split into
// several independent datagrams, which corrupts application framing; the
// source stack reassembles. The receive-side reassembler is not ported.

import (
	"encoding/binary"
	"errors"
	"sync/atomic"
)

const (
	ipv6NextHeaderHopByHop           = 0
	ipv6NextHeaderRouting            = 43
	ipv6NextHeaderFragment           = 44
	ipv6NextHeaderAuthentication     = 51
	ipv6NextHeaderDestinationOptions = 60
	ipv6NextHeaderMobility           = 135
	ipv6NextHeaderHostIdentity       = 139
	ipv6NextHeaderShim6              = 140

	// the fixed size of a Fragment extension header
	ipv6FragmentHeaderSize = 8
)

var (
	errInvalidIpv6Packet            = errors.New("invalid IPv6 packet")
	errIpv6MtuTooSmall              = errors.New("IPv6 MTU is too small for fragmentation")
	errIpv6ExtensionHeadersFragment = errors.New("IPv6 packet with extension headers cannot be fragmented here")
)

// isIpv6ExtensionHeader reports whether a next-header value is one the walker
// steps through. Used by hot paths to skip the walk for the overwhelmingly
// common packet whose base header points straight at tcp, udp or icmpv6.
func isIpv6ExtensionHeader(nextHeader byte) bool {
	switch nextHeader {
	case ipv6NextHeaderHopByHop,
		ipv6NextHeaderRouting,
		ipv6NextHeaderFragment,
		ipv6NextHeaderAuthentication,
		ipv6NextHeaderDestinationOptions,
		ipv6NextHeaderMobility,
		ipv6NextHeaderHostIdentity,
		ipv6NextHeaderShim6:
		return true
	default:
		return false
	}
}

var ipv6FragmentIdentification atomic.Uint32

func nextIpv6FragmentIdentification() uint32 {
	for {
		identification := ipv6FragmentIdentification.Add(1)
		if identification != 0 {
			return identification
		}
	}
}

// fragmentIpv6Packet converts one complete IPv6 packet with no extension
// headers -- the only shape this package's builders emit -- into RFC 8200
// fragments. The base header becomes the unfragmentable part, a Fragment
// header is inserted after it, and the transport header plus payload is the
// fragmentable part. It consumes packet on every return path.
func fragmentIpv6Packet(packet []byte, mtu int) ([][]byte, error) {
	if len(packet) < Ipv6HeaderSize || packet[0]>>4 != 6 {
		MessagePoolReturn(packet)
		return nil, errInvalidIpv6Packet
	}
	declaredEnd := Ipv6HeaderSize + int(binary.BigEndian.Uint16(packet[4:6]))
	if len(packet) < declaredEnd {
		MessagePoolReturn(packet)
		return nil, errInvalidIpv6Packet
	}
	if isIpv6ExtensionHeader(packet[6]) {
		// the unfragmentable part would have to be split from the
		// fragmentable extension headers; nothing here builds such a packet
		MessagePoolReturn(packet)
		return nil, errIpv6ExtensionHeadersFragment
	}
	fragmentPayloadByteCount := (mtu - Ipv6HeaderSize - ipv6FragmentHeaderSize) &^ 7
	if fragmentPayloadByteCount < 8 {
		MessagePoolReturn(packet)
		return nil, errIpv6MtuTooSmall
	}
	nextHeader := packet[6]
	ipPayload := packet[Ipv6HeaderSize:declaredEnd]
	identification := nextIpv6FragmentIdentification()
	fragments := make([][]byte, 0, (len(ipPayload)+fragmentPayloadByteCount-1)/fragmentPayloadByteCount)
	for offset := 0; offset < len(ipPayload); {
		end := min(offset+fragmentPayloadByteCount, len(ipPayload))
		fragment := MessagePoolGet(Ipv6HeaderSize + ipv6FragmentHeaderSize + end - offset)
		copy(fragment[:Ipv6HeaderSize], packet[:Ipv6HeaderSize])
		binary.BigEndian.PutUint16(fragment[4:6], uint16(len(fragment)-Ipv6HeaderSize))
		fragment[6] = ipv6NextHeaderFragment
		fragmentHeader := fragment[Ipv6HeaderSize : Ipv6HeaderSize+ipv6FragmentHeaderSize]
		fragmentHeader[0] = nextHeader
		fragmentHeader[1] = 0
		offsetAndFlags := uint16(offset)
		if end < len(ipPayload) {
			offsetAndFlags |= 0x1
		}
		binary.BigEndian.PutUint16(fragmentHeader[2:4], offsetAndFlags)
		binary.BigEndian.PutUint32(fragmentHeader[4:8], identification)
		copy(fragment[Ipv6HeaderSize+ipv6FragmentHeaderSize:], ipPayload[offset:end])
		fragments = append(fragments, fragment)
		offset = end
	}
	MessagePoolReturn(packet)
	return fragments, nil
}
