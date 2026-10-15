// ip_protocol_number.go — upstream's iana protocol-number type, verbatim.
//
// This is upstream connect's own definition (ip.go, "iana ip protocol numbers"
// through the const block). It is carried here as its own file so that the
// ported ipv6/icmp files can be copied from upstream unedited and keep diffing
// cleanly against it on future syncs.
//
// It is deliberately a NEW type rather than a reuse of this line's existing
// types: IPProtocol (uint8, iana) and IpProtocol (int, this line's own enum,
// values Tcp=1/Udp=2) both already exist and neither is touched here. The three
// names are distinct, so nothing existing changes and the ported files stay
// inert.
//
// S3 moves this definition back into ip.go, where upstream keeps it.
package connect

// iana ip protocol numbers, as carried in the ipv4 protocol and ipv6 next
// header fields. tcp, udp, and icmp echo are handled; other protocols are
// dropped. the icmp numbers also label the flow-teardown unreachable
// messages built by `ipOosUnreachable`, which are injected toward the
// source.
//
// It is an ALIAS of IPProtocol rather than upstream's standalone
// `type ipProtocolNumber uint8`. Both are uint8 carrying the same iana values,
// so the alias is value-for-value identical, and it is what lets the ported
// files be copied from upstream WITHOUT EDITING: upstream's own test passes an
// ipProtocolNumber constant to transportChecksum, which on this line takes
// IPProtocol. The alias is the shim-level conversion the port asks for; a
// distinct type would force an edit inside the ported test and every future
// sync would then diff dirty. S3 can drop the alias and move upstream's real
// declaration into ip.go when it takes that file over.
type ipProtocolNumber = IPProtocol

const (
	ipProtocolNumberIcmp4 ipProtocolNumber = 1
	ipProtocolNumberTcp   ipProtocolNumber = 6
	ipProtocolNumberUdp   ipProtocolNumber = 17
	ipProtocolNumberIcmp6 ipProtocolNumber = 58
)
