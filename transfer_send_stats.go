package connect

import "sync/atomic"

// Where the bytes go. The transfer layer writes each frame once when it is first
// sent and again every time its resend timer fires before an ack arrives. The
// two look identical on the wire, so a provider whose acks are slow can send many
// times more than it ever delivers while the billable counters (which count each
// client packet once) stay flat. These counters split the two.

// ResendDepth buckets count resends by how many times the item had already been
// sent: second send, third or fourth, fifth to eighth, ninth and later.
const transferResendDepthBuckets = 4

var transferSendCounters struct {
	firstFrames  atomic.Int64
	firstBytes   atomic.Int64
	resendFrames atomic.Int64
	resendBytes  atomic.Int64
	resendDepth  [transferResendDepthBuckets]atomic.Int64

	deferredFrames atomic.Int64
	deferredBytes  atomic.Int64

	wouldDeferFrames atomic.Int64
	wouldDeferBytes  atomic.Int64
	earnedBytes      atomic.Int64
}

// TransferSendStats is a snapshot of the process-wide transfer send counters.
type TransferSendStats struct {
	FirstFrames  int64
	FirstBytes   int64
	ResendFrames int64
	ResendBytes  int64
	ResendDepth  [transferResendDepthBuckets]int64

	// ResendDeferred counts resends the retry budget refused to send yet.
	ResendDeferredFrames int64
	ResendDeferredBytes  int64

	// ResendWouldDefer counts resends shadow mode sent anyway that enforce would
	// have refused. BudgetEarnedBytes is acknowledged bytes credited to the budget.
	ResendWouldDeferFrames int64
	ResendWouldDeferBytes  int64
	BudgetEarnedBytes      int64
}

func TransferSendStatsNow() TransferSendStats {
	var s TransferSendStats
	s.FirstFrames = transferSendCounters.firstFrames.Load()
	s.FirstBytes = transferSendCounters.firstBytes.Load()
	s.ResendFrames = transferSendCounters.resendFrames.Load()
	s.ResendBytes = transferSendCounters.resendBytes.Load()
	for i := range s.ResendDepth {
		s.ResendDepth[i] = transferSendCounters.resendDepth[i].Load()
	}
	s.ResendDeferredFrames = transferSendCounters.deferredFrames.Load()
	s.ResendDeferredBytes = transferSendCounters.deferredBytes.Load()
	s.ResendWouldDeferFrames = transferSendCounters.wouldDeferFrames.Load()
	s.ResendWouldDeferBytes = transferSendCounters.wouldDeferBytes.Load()
	s.BudgetEarnedBytes = transferSendCounters.earnedBytes.Load()
	return s
}

func recordTransferFirstSend(byteCount int) {
	transferSendCounters.firstFrames.Add(1)
	transferSendCounters.firstBytes.Add(int64(byteCount))
}

func recordTransferResendWouldDefer(byteCount int) {
	transferSendCounters.wouldDeferFrames.Add(1)
	transferSendCounters.wouldDeferBytes.Add(int64(byteCount))
}

// recordTransferAckedBytes credits acknowledged bytes to the resend budget.
func recordTransferAckedBytes(byteCount int) {
	transferSendCounters.earnedBytes.Add(int64(byteCount))
	transferResendBudget.earn(byteCount)
}

func recordTransferResendDeferred(byteCount int) {
	transferSendCounters.deferredFrames.Add(1)
	transferSendCounters.deferredBytes.Add(int64(byteCount))
}

// recordTransferResend records a resend. sendNumber is which send this is for the
// item: 2 for the first resend.
func recordTransferResend(byteCount int, sendNumber int) {
	transferSendCounters.resendFrames.Add(1)
	transferSendCounters.resendBytes.Add(int64(byteCount))
	bucket := 3
	switch {
	case sendNumber <= 2:
		bucket = 0
	case sendNumber <= 4:
		bucket = 1
	case sendNumber <= 8:
		bucket = 2
	}
	transferSendCounters.resendDepth[bucket].Add(1)
}
