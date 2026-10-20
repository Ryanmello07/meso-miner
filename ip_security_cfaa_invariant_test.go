package connect

import (
	"testing"
)

// Table/constant integrity for the generated CFAA blocklist. These are the
// upstream invariant checks (connect 467c7898), kept next to the fork's
// policy tests: the packed data must be a whole number of records, every
// record must have lo <= hi, and records must be sorted, disjoint and
// non-adjacent so cfaaBlockedIp4/cfaaBlockedIp6's binary searches are valid.

// cfaaRangeAt decodes the i-th packed [lo,hi] IPv4 range from
// cfaaBlockedPrefixData.
func cfaaRangeAt(i int) (lo, hi uint32) {
	o := i * 8
	lo = uint32(cfaaBlockedPrefixData[o])<<24 | uint32(cfaaBlockedPrefixData[o+1])<<16 |
		uint32(cfaaBlockedPrefixData[o+2])<<8 | uint32(cfaaBlockedPrefixData[o+3])
	hi = uint32(cfaaBlockedPrefixData[o+4])<<24 | uint32(cfaaBlockedPrefixData[o+5])<<16 |
		uint32(cfaaBlockedPrefixData[o+6])<<8 | uint32(cfaaBlockedPrefixData[o+7])
	return
}

// packV6 packs sorted [lo,hi] 16-byte ranges into the generated table format.
// It is shared with the blocker tests.
func packV6(ranges [][2][16]byte) (string, int) {
	var b []byte
	for _, r := range ranges {
		b = append(b, r[0][:]...)
		b = append(b, r[1][:]...)
	}
	return string(b), len(ranges)
}

func TestCfaaBlockedPrefixInvariant(t *testing.T) {
	if cfaaBlockedPrefixCount < 10_000 {
		t.Fatalf("v4 records = %d, want at least reviewed minimum 10000", cfaaBlockedPrefixCount)
	}
	if len(cfaaBlockedPrefixData) != cfaaBlockedPrefixCount*8 {
		t.Fatalf("data length %d, want %d (= %d records * 8)",
			len(cfaaBlockedPrefixData), cfaaBlockedPrefixCount*8, cfaaBlockedPrefixCount)
	}
	var prevHi uint32
	for i := 0; i < cfaaBlockedPrefixCount; i += 1 {
		lo, hi := cfaaRangeAt(i)
		if lo > hi {
			t.Fatalf("range %d: lo %08x > hi %08x", i, lo, hi)
		}
		if 0 < i && uint64(lo) <= uint64(prevHi)+1 {
			t.Fatalf("range %d: lo %08x not strictly after previous hi %08x (unsorted/overlapping/adjacent)", i, lo, prevHi)
		}
		prevHi = hi
	}
}

func TestCfaaBlockedPrefix6Invariant(t *testing.T) {
	if cfaaBlockedPrefix6Count < 100 {
		t.Fatalf("v6 records = %d, want at least reviewed minimum 100", cfaaBlockedPrefix6Count)
	}
	if len(cfaaBlockedPrefix6Data) != cfaaBlockedPrefix6Count*32 {
		t.Fatalf("data length %d, want %d", len(cfaaBlockedPrefix6Data), cfaaBlockedPrefix6Count*32)
	}
	var prevHiHi, prevHiLo uint64
	for i := 0; i < cfaaBlockedPrefix6Count; i += 1 {
		o := i * 32
		loHi := be64(cfaaBlockedPrefix6Data, o)
		loLo := be64(cfaaBlockedPrefix6Data, o+8)
		hiHi := be64(cfaaBlockedPrefix6Data, o+16)
		hiLo := be64(cfaaBlockedPrefix6Data, o+24)
		if hiHi < loHi || (hiHi == loHi && hiLo < loLo) {
			t.Fatalf("range %d: lo > hi", i)
		}
		if 0 < i && (loHi < prevHiHi || (loHi == prevHiHi && loLo <= prevHiLo)) {
			t.Fatalf("range %d: not strictly after previous (unsorted/overlapping)", i)
		}
		prevHiHi, prevHiLo = hiHi, hiLo
	}
}
