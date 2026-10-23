package connect

import (
	"sync"
	"testing"
)

// Pool Get/Return on many goroutines while EnhancedMetrics reads the per-shard
// counters. Under -race this guards the counters' locking: they are written
// under each shard's mutex and summed under the same mutex.
func TestEnhancedMetricsConcurrentWithPoolTraffic(t *testing.T) {
	sizes := []int{1500, 4000, 16000, 32000, 65000}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				EnhancedMetrics()
			}
		}
	}()
	var workers sync.WaitGroup
	for w := range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range 2000 {
				MessagePoolReturn(MessagePoolGet(sizes[(w+i)%len(sizes)]))
			}
		}()
	}
	workers.Wait()
	close(stop)
	wg.Wait()
}
