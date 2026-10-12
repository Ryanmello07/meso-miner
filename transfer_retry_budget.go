package connect

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// The transfer retry budget.
//
// The transfer layer sends each frame once and then again whenever its resend
// timer fires before an ack arrives. That is right when frames are occasionally
// lost, and ruinous when the far end is merely slow: acks arrive late, so every
// timer fires, the resends add load to the thing that is already struggling,
// the acks get later, and each frame goes out up to MaxResendCount times. On
// 2026-10-10 a provider with a slow backend sent about 9.5 times as many resend
// bytes as first-send bytes (90% of everything it transmitted) for a billable
// payload that was a fraction of the first sends.
//
// A retry budget bounds that. Resends are paid for out of tokens earned by first
// sends: every first-sent byte earns retryBudgetRatio of a token, so resends can
// never exceed that share of fresh traffic, plus a small floor refill so a quiet
// flow can still recover a lost frame. A resend that cannot pay is rescheduled
// without being sent, the same way the resend-cap park works.
//
// With ordinary loss (a few percent) the budget never binds. It binds exactly
// when the resend share explodes.

const (
	// retryBudgetRatio is the share of first-sent bytes that may be resent.
	retryBudgetRatio = 0.25
	// retryBudgetFloorBytesPerSecond refills regardless of traffic, so a flow with
	// almost no first sends can still resend a lost frame.
	retryBudgetFloorBytesPerSecond = 64 << 10
	// retryBudgetBurstBytes caps what can be saved up.
	retryBudgetBurstBytes = 8 << 20
	// retryBudgetInitialBytes is the grant a fresh process starts with.
	retryBudgetInitialBytes = 1 << 20
)

type transferRetryBudget struct {
	mu     sync.Mutex
	now    func() time.Time
	tokens float64
	last   time.Time
}

func newTransferRetryBudget(now func() time.Time) *transferRetryBudget {
	return &transferRetryBudget{now: now, tokens: retryBudgetInitialBytes, last: now()}
}

// refillLocked adds the floor refill for the time since the last call.
func (self *transferRetryBudget) refillLocked() {
	now := self.now()
	if dt := now.Sub(self.last); 0 < dt {
		self.tokens = min(self.tokens+retryBudgetFloorBytesPerSecond*dt.Seconds(), retryBudgetBurstBytes)
	}
	self.last = now
}

// earn credits byteCount bytes the far end acknowledged. Credit comes from
// acknowledged frames, not sent ones: a destination that acknowledges nothing
// earns nothing, so it cannot fund resends to itself.
func (self *transferRetryBudget) earn(byteCount int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.refillLocked()
	self.tokens = min(self.tokens+retryBudgetRatio*float64(byteCount), retryBudgetBurstBytes)
}

// allow pays for a resend of byteCount bytes, or refuses without charging.
func (self *transferRetryBudget) allow(byteCount int) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.refillLocked()
	if float64(byteCount) <= self.tokens {
		self.tokens -= float64(byteCount)
		return true
	}
	return false
}

// balance reports the current tokens.
func (self *transferRetryBudget) balance() float64 {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.refillLocked()
	return self.tokens
}

// reset sets the balance. For tests.
func (self *transferRetryBudget) reset(tokens float64) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.tokens = tokens
	self.last = self.now()
}

// transferResendBudget is the process-wide budget: the pressure is on the host's
// pipe, so it is shared by every send sequence.
var transferResendBudget = newTransferRetryBudget(time.Now)

// retryAllowed asks whether a resend of byteCount bytes may go out. Enforce
// refuses when the budget cannot pay. Shadow sends anyway but counts and logs
// what enforce would have refused, so a shadow canary measures the budget.
// Off behaves as before the budget existed.
func retryAllowed(byteCount int) bool {
	mode := currentGovernMode()
	if mode == governOff {
		return true
	}
	if transferResendBudget.allow(byteCount) {
		return true
	}
	if mode == governEnforce {
		retryBudgetReport.note(true, byteCount)
		return false
	}
	recordTransferResendWouldDefer(byteCount)
	retryBudgetReport.note(false, byteCount)
	return true
}

// retryBudgetReportInterval is how often refusals are summarised in the log.
const retryBudgetReportInterval = 60 * time.Second

// retryBudgetReporter turns a stream of refusals into one log line per interval,
// so an overloaded host does not log once per refused resend.
type retryBudgetReporter struct {
	now      func() time.Time
	emit     func(string)
	lastNano atomic.Int64
	frames   atomic.Int64
	bytes    atomic.Int64
}

var retryBudgetReport = &retryBudgetReporter{now: time.Now, emit: emitGovernLine}

func (self *retryBudgetReporter) note(enforced bool, byteCount int) {
	self.frames.Add(1)
	self.bytes.Add(int64(byteCount))
	now := self.now().UnixNano()
	last := self.lastNano.Load()
	if last == 0 {
		self.lastNano.CompareAndSwap(0, now)
		return
	}
	if now-last < int64(retryBudgetReportInterval) || !self.lastNano.CompareAndSwap(last, now) {
		return
	}
	frames := self.frames.Swap(0)
	bytes := self.bytes.Swap(0)
	verb := "deferred"
	if !enforced {
		verb = "would have deferred (shadow, sent anyway)"
	}
	self.emit(fmt.Sprintf(
		"[govern] retry budget %s %d resends (%.1f MiB) in the last %s; tokens left %.0f KiB. Resends are paid for by bytes the far end acknowledged, so this means acks are not keeping up with what we send",
		verb, frames, float64(bytes)/(1<<20), time.Duration(now-last).Round(time.Second), transferResendBudget.balance()/1024,
	))
}

// resendIsFree reports whether the next send of an item is exempt from the budget.
// sendCount is how many times the item has been sent so far. The second send is
// free for every frame, and the frame at the head of the sequence is free through
// its fifth send.
func resendIsFree(sendCount int, isHead bool) bool {
	next := sendCount + 1
	return next <= 2 || (isHead && next <= 5)
}

// TransferRetryBudgetTokens reports the process-wide resend budget balance in bytes.
func TransferRetryBudgetTokens() float64 { return transferResendBudget.balance() }
