package connect

import (
	"time"

	"github.com/urnetwork/connect/protocol"
)

// fork: the icmp echo path (upstream e05ecee0b) refers to three symbols that
// upstream keeps in its own ip.go. Our ip.go does not carry that refactor, so
// they are declared here instead. Nothing else of ours changes for this file.
const IpProtocolIcmp IpProtocol = 3

type receivePacketsBatchFunction func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packets [][]byte) (batched bool)

const lruEvictionSampleSize = 32

// applyLruMapLimit bounds eviction work independently of table cardinality.
// Go map iteration starts at a pseudo-random bucket, so choosing the oldest of
// the first fixed-size sample is an approximate LRU without collecting and
// sorting the entire flow table under its dispatch lock. limitCallback must
// remove an accepted resource from resources; it owns any cancellation.
func applyLruMapLimit[K comparable, R UserLimited](
	resources map[K]R,
	limit int,
	limitCallback func(K, R) bool,
) {
	for limit < len(resources) {
		var oldestKey K
		var oldest R
		var oldestTime time.Time
		found := false
		sampled := 0
		for key, resource := range resources {
			activityTime := resource.LastActivityTime()
			if !found || activityTime.Before(oldestTime) {
				oldestKey = key
				oldest = resource
				oldestTime = activityTime
				found = true
			}
			sampled += 1
			if lruEvictionSampleSize <= sampled {
				break
			}
		}
		if !found || !limitCallback(oldestKey, oldest) {
			return
		}
	}
}
