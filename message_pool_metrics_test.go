package connect

import (
	"testing"
)

func TestEnhancedMetricsShape(t *testing.T) {
	m := EnhancedMetrics()
	for _, key := range []string{"hits", "misses", "returns", "active_buffers", "pooled_buffers", "size_distribution", "last_reset_time", "gc_pauses"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("EnhancedMetrics missing key %q", key)
		}
	}
	// active_buffers is process-wide and other tests hold buffers, so assert
	// on the delta of a Get/Return round-trip in every size class.
	before := m["active_buffers"].(uint64)
	beforeReturns := m["returns"].(uint64)
	beforeSizes := m["size_distribution"].(map[string]uint64)

	sizes := []int{1500, 4000, 16000, 32000, 65000}
	for _, size := range sizes {
		MessagePoolReturn(MessagePoolGet(size))
	}
	m2 := EnhancedMetrics()
	if after := m2["active_buffers"].(uint64); after != before {
		t.Fatalf("expected active_buffers to net back to %d after round-trip, got %d", before, after)
	}
	if got := m2["returns"].(uint64) - beforeReturns; got != uint64(len(sizes)) {
		t.Fatalf("expected %d returns after round-trip, got %d", len(sizes), got)
	}
	sd := m2["size_distribution"].(map[string]uint64)
	for _, size := range []string{"2048", "4096", "16384", "32768", "65536"} {
		if sd[size] != beforeSizes[size]+1 {
			t.Fatalf("size %s: %d gets, want %d", size, sd[size], beforeSizes[size]+1)
		}
	}
}
