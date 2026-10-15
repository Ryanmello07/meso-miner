package connect

// natFragmentReassemblyMaxRetainedBytes bounds the fragment bytes one NAT send
// shard retains per family while it waits for a datagram to complete. A
// fragmented datagram on this line is a QUIC initial or an oversized reply,
// a few kilobytes at most, so this is generous for benign bursts and small for
// an incomplete datagram sent to exhaust memory. Datagram and fragment counts
// are bounded separately (ip_fragment.go).
const natFragmentReassemblyMaxRetainedBytes = 16 * 1024
