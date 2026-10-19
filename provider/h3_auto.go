package main

import (
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// `h3 auto` sizing. Design note: scratch/rollout-v33/h3-auto-design.md.
//
// Auto sizes the FALLBACK SET for proxied identities, the same thing a count or
// all means, not always-connected H3. A fallback dials only while H1 is not
// connected and stands down once something better is, so the set is cheap in
// steady state. Auto never enables the beside-H1 auxiliary; that stays what
// `on` means. It is opt-in through the h3 control key, and without that key
// nothing here runs past the first control-state read.
//
// Two memory inputs, no new sensors:
//
//   - capacity from the effective memory limit (cgroup aware), one proxy slot
//     per h3AutoPerProxyBytes, none below h3AutoMinMemoryBytes, and at most
//     h3AutoMaxProxies. The ranking caps it again at the real pool size.
//   - pressure from the memory components of the existing pressure sweep,
//     smoothed with the pressure system's asymmetric EWMA.
//
// The set widens slowly and sheds fast, with a hold band between the grow and
// shed thresholds so one noisy sample cannot undo a shed.
const (
	h3AutoMaxProxies     = 64
	h3AutoShedTo         = 16
	h3AutoGrowStep       = 4
	h3AutoMinMemoryBytes = int64(2) << 30
	h3AutoPerProxyBytes  = int64(64) << 20
	h3AutoGrowBelow      = 0.30
	h3AutoShedAbove      = 0.50
	h3AutoDirectAbove    = 0.75
)

// h3AutoTarget is the proxy count the controller currently allows. The ranking
// reads it under h3ModeAuto.
var h3AutoTarget atomic.Int64

// h3AutoMemoryLimit is the effective-memory seam; tests override it.
var h3AutoMemoryLimit = detectEffectiveRAMLimitBytes

// h3AutoState carries the smoothing and the last target between sweeps.
type h3AutoState struct {
	mu       sync.Mutex
	smoothed float64
	target   int
}

var h3AutoController h3AutoState

// h3AutoCapacity is the most proxies the memory limit can justify: one per
// h3AutoPerProxyBytes, none on a box below h3AutoMinMemoryBytes, capped at
// h3AutoMaxProxies.
func h3AutoCapacity(memoryLimitBytes int64) int {
	if memoryLimitBytes < h3AutoMinMemoryBytes {
		return 0
	}
	n := int(memoryLimitBytes / h3AutoPerProxyBytes)
	if n > h3AutoMaxProxies {
		return h3AutoMaxProxies
	}
	if n < 0 {
		return 0
	}
	return n
}

// h3AutoMemoryPressure is the memory-only score for one sweep: the worst of the
// pressure system's memory components. CPU, IO, load and FD pressure do not
// size H3.
func h3AutoMemoryPressure(comps map[string]float64) float64 {
	p := 0.0
	for _, key := range []string{"psi_mem", "mem", "heap"} {
		if v := comps[key]; v > p {
			p = v
		}
	}
	return p
}

// h3AutoNextTarget applies the tiers. Growth is one small step per sample while
// memory is calm; any pressure tier sheds immediately to its level, never
// growing under pressure. A previous target above capacity is clamped down even
// in the hold band, so a lower memory limit (or a move to a smaller box) takes
// effect at once.
func h3AutoNextTarget(capacity int, pressure float64, previous int) int {
	if capacity <= 0 {
		return 0
	}
	switch {
	case pressure >= h3AutoDirectAbove:
		return 0
	case pressure >= h3AutoShedAbove:
		return min(previous, h3AutoShedTo, capacity)
	case pressure >= h3AutoGrowBelow:
		return min(previous, capacity)
	default:
		return min(previous+h3AutoGrowStep, capacity)
	}
}

// step folds one sample into the controller and returns the target move.
func (s *h3AutoState) step(memoryLimitBytes int64, comps map[string]float64) (previous, target int, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.smoothed = ewmaUpdate(s.smoothed, h3AutoMemoryPressure(comps))
	previous = s.target
	target = h3AutoNextTarget(h3AutoCapacity(memoryLimitBytes), s.smoothed, previous)
	s.target = target
	return previous, target, target != previous
}

// smoothedValue is the pressure the last step decided on, for the change log.
func (s *h3AutoState) smoothedValue() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.smoothed
}

// reset forgets the episode. Leaving auto calls it, so a later re-entry starts
// conservative instead of resuming an old target.
func (s *h3AutoState) reset() {
	s.mu.Lock()
	s.smoothed, s.target = 0, 0
	s.mu.Unlock()
}

// noteH3AutoPressure feeds one full pressure sweep to `h3 auto`. It runs from
// the pressure monitor goroutine, which sweeps whether or not self-heal is on,
// so the feature does not depend on self-heal. The control-state check comes
// first: when the key is anything but auto this returns without reading a
// sensor, touching the controller, or logging.
func noteH3AutoPressure(comps map[string]float64) {
	setting := resolveH3Mode(globalControlState, currentProxyResolution())
	if setting.Mode != h3ModeAuto {
		return
	}
	limit := h3AutoMemoryLimit()
	previous, target, changed := h3AutoController.step(limit, comps)
	if !changed {
		return
	}
	// The control socket may have left auto while this sample was in flight.
	// The key always wins: forget the episode and store nothing.
	if resolveH3Mode(globalControlState, currentProxyResolution()).Mode != h3ModeAuto {
		h3AutoController.reset()
		h3AutoTarget.Store(0)
		return
	}
	h3AutoTarget.Store(int64(target))
	// Re-rank at once so eligibility, the relaunch list and any identity that
	// starts in this window all see the new count. The relaunch pass is rate
	// limited and never cuts a proxy that has clients (relaunchH3Proxies).
	if rerankH3(setting) {
		tlog("[h3] auto: %d -> %d proxies get the fallback transports (memory pressure %.2f, capacity %d)\n",
			previous, target, h3AutoController.smoothedValue(), h3AutoCapacity(limit))
		go connect.HandleError(func() { relaunchH3ProxiesNow(setting) })
	}
}
