package connect

// natFragmentReassemblyMaxRetainedBytes bounds the fragment bytes one NAT send
// shard retains per family while it waits for a datagram to complete.
//
// The reassemblers charge the pool buffer's CAPACITY (ip_fragment.go), which
// is the memory actually pinned: a fragment-sized packet draws the 2048-byte
// pool bucket and costs 2061 charged bytes whatever its length (measured), so
// the charge runs about 1.9x the fragment payload. 64 KiB is upstream's own
// unbudgeted default (connect b31ec82df ip_nat_memory.go keeps 16 KiB only
// under a memory budget; this line has no per-nat budget, so it takes the
// larger value). It admits a 9 KiB fragmented reply (9 fragments, 18549
// charged - the measured 9124-byte peer TLS flight) and payloads up to about
// 33 KiB, while the datagram (16) and fragment (64) caps remain the tighter
// concurrency bounds in most cases.
const natFragmentReassemblyMaxRetainedBytes = 64 * 1024
