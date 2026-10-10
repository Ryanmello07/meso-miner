package connect

import (
	"fmt"
	"sync"
	"time"
)

// dialTimeoutError is what a dial returns when no dialer connected before the
// request deadline. It used to be a bare "Timeout." whatever had gone wrong, so
// a refused connection, a dead SOCKS proxy, a rejected upgrade and a platform
// that never answered all read the same in the logs and to every caller.
//
// The message keeps its leading "Timeout." on purpose: the auth rate limiter
// and the auth-failure cause classifier match on it, and a failed dial must
// keep reading as an overload signal to them. The real cause is appended, which
// also lets those classifiers see "proxy unreachable" and friends that the old
// message hid. last is nil when no dialer failed in time, which means the
// platform (or the proxy path) simply did not answer.
type dialTimeoutError struct {
	// failures counts dials that returned an error before the deadline
	failures int
	// rounds counts passes over the dialers: parallelEval keeps retrying all of
	// them for as long as there is time left
	rounds int
	waited time.Duration
	// last is the most recent real dial error, nil if none
	last error
}

func (self *dialTimeoutError) Error() string {
	if self.last == nil {
		return fmt.Sprintf("Timeout. no dialer connected within %s", self.waited.Round(time.Millisecond))
	}
	return fmt.Sprintf(
		"Timeout. %d dial(s) failed over %d round(s) in %s, last: %s",
		self.failures, self.rounds, self.waited.Round(time.Millisecond), self.last,
	)
}

func (self *dialTimeoutError) Unwrap() error { return self.last }

// Timeout lets net.Error style checks see this as a timeout.
func (self *dialTimeoutError) Timeout() bool { return true }

// evalErrorRecorder collects what the dialers of ONE parallelEval call returned
// while the call was still live. Errors that arrive after the deadline are the
// deadline's own doing (a cancelled dial), not causes, and are not recorded.
type evalErrorRecorder struct {
	mu       sync.Mutex
	failures int
	rounds   int
	last     error
}

func (self *evalErrorRecorder) round() {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.rounds += 1
}

func (self *evalErrorRecorder) note(err error) {
	if err == nil {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	self.failures += 1
	self.last = err
}

func (self *evalErrorRecorder) timeoutError(waited time.Duration) error {
	self.mu.Lock()
	defer self.mu.Unlock()
	return &dialTimeoutError{
		failures: self.failures,
		rounds:   self.rounds,
		waited:   waited,
		last:     self.last,
	}
}
