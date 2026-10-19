package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

// resetH3AutoForTest clears the process-wide auto state and restores it after
// the test. The controller and ranking are globals (the pressure monitor owns
// them at runtime), so every test that touches them must isolate them.
func resetH3AutoForTest(t *testing.T) {
	t.Helper()
	prevLimit := h3AutoMemoryLimit
	prevApplied := h3AppliedSetting.Load()
	h3AutoController.reset()
	h3AutoTarget.Store(0)
	h3CandidatesMutex.Lock()
	prevCandidates := h3Candidates
	h3Candidates = nil
	h3CandidatesMutex.Unlock()
	h3RankingValue.Store(nil)
	t.Cleanup(func() {
		h3AutoMemoryLimit = prevLimit
		h3AppliedSetting.Store(prevApplied)
		h3AutoController.reset()
		h3AutoTarget.Store(0)
		h3CandidatesMutex.Lock()
		h3Candidates = prevCandidates
		h3CandidatesMutex.Unlock()
		h3RankingValue.Store(nil)
	})
}

func TestH3AutoCapacity(t *testing.T) {
	gib := int64(1) << 30
	// testH3AutoMemoryLimit returns a cgroup-aware limit; the constants are
	// deliberately conservative until a canary measures per-carrier RSS.
	for _, c := range []struct {
		memory int64
		want   int
	}{
		{-1, 0}, {0, 0}, {gib, 0}, {2*gib - 1, 0},
		{2 * gib, 32}, {3 * gib, 48}, {4 * gib, 64}, {64 * gib, 64},
	} {
		if got := h3AutoCapacity(c.memory); got != c.want {
			t.Fatalf("capacity(%d GiB) = %d, want %d", c.memory/gib, got, c.want)
		}
	}
}

func TestH3AutoMemoryPressureIsMemoryOnly(t *testing.T) {
	for _, c := range []struct {
		name  string
		comps map[string]float64
		want  float64
	}{
		{"empty", nil, 0},
		{"psi_mem", map[string]float64{"psi_mem": 0.4, "psi_cpu": 0.9, "load": 0.9}, 0.4},
		{"availability", map[string]float64{"psi_mem": 0.1, "mem": 0.6}, 0.6},
		{"heap", map[string]float64{"mem": 0.2, "heap": 0.7}, 0.7},
		{"cpu ignored", map[string]float64{"psi_cpu": 1, "psi_io": 1, "fd": 1, "goro": 1}, 0},
	} {
		if got := h3AutoMemoryPressure(c.comps); got != c.want {
			t.Fatalf("%s: pressure = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestH3AutoNextTargetTiers(t *testing.T) {
	for _, c := range []struct {
		name     string
		capacity int
		pressure float64
		previous int
		want     int
	}{
		{"no capacity", 0, 0, 40, 0},
		{"grow from zero", 64, 0.10, 0, 4},
		{"grow one step", 64, 0.29, 30, 34},
		{"clamp at capacity", 64, 0.10, 62, 64},
		{"already at capacity", 64, 0.10, 64, 64},
		{"hold", 64, 0.35, 40, 40},
		{"hold clamps a shrunken limit", 32, 0.40, 48, 32},
		{"shed to the tier", 64, 0.55, 64, 16},
		{"shed never grows", 64, 0.60, 8, 8},
		{"shed clamps to capacity", 4, 0.60, 64, 4},
		{"extreme pressure", 64, 0.90, 64, 0},
		{"hysteresis: below grow threshold grows", 64, 0.299, 20, 24},
		{"hysteresis: just below shed holds", 64, 0.499, 20, 20},
	} {
		if got := h3AutoNextTarget(c.capacity, c.pressure, c.previous); got != c.want {
			t.Fatalf("%s: next(%d, %.3f, %d) = %d, want %d", c.name, c.capacity, c.pressure, c.previous, got, c.want)
		}
	}
}

// The smoothed score reacts to onset within a sample or two and relaxes over
// minutes; the hold band keeps a shed from being undone by one calm reading.
func TestH3AutoStateSlowUpFastDown(t *testing.T) {
	var s h3AutoState
	limit := int64(4) << 30 // capacity 64
	s.target = 64

	// A full memory stall: 1.0 rises to 0.5 (shed tier) on the first sample,
	// to 0.75 (direct only) on the second, and the hold band never re-adds.
	if prev, target, changed := s.step(limit, map[string]float64{"heap": 1}); !changed || prev != 64 || target != 16 {
		t.Fatalf("first stall sample = %d -> %d changed=%v, want 64 -> 16", prev, target, changed)
	}
	if _, target, _ := s.step(limit, map[string]float64{"heap": 1}); target != 0 {
		t.Fatalf("second stall sample = %d, want direct only", target)
	}
	if _, target, changed := s.step(limit, map[string]float64{"heap": 0}); changed || target != 0 {
		t.Fatalf("the first calm sample must not regrow, got %d changed=%v", target, changed)
	}

	// Relaxation takes several calm samples (decay 0.1), then growth resumes
	// in steps of 4 and stops at capacity.
	grew := -1
	for i := 0; i < 20; i++ {
		before := s.target
		_, target, _ := s.step(limit, map[string]float64{"heap": 0})
		if target > before {
			grew = i
			if target != before+h3AutoGrowStep {
				t.Fatalf("growth step = %d -> %d, want +%d", before, target, h3AutoGrowStep)
			}
			break
		}
	}
	if grew < 0 {
		t.Fatal("the controller never regrew after a calm period")
	}
	for i := 0; i < 30; i++ {
		s.step(limit, map[string]float64{"heap": 0})
	}
	if s.target != h3AutoMaxProxies {
		t.Fatalf("after sustained calm target = %d, want %d", s.target, h3AutoMaxProxies)
	}
}

func TestH3AutoStateSkipsBeforeCapacity(t *testing.T) {
	var s h3AutoState
	if _, target, changed := s.step(int64(1)<<30, nil); changed || target != 0 {
		t.Fatalf("a small box must stay at zero, got %d changed=%v", target, changed)
	}
	if _, target, changed := s.step(0, nil); changed || target != 0 {
		t.Fatalf("an unknown limit must stay at zero, got %d changed=%v", target, changed)
	}
}

// Auto ranks, admits and shrinks exactly through the shared h3 ranking, and
// keeps its members across a widening step.
func TestH3AutoRankingAndEligibility(t *testing.T) {
	withTempHome(t)
	resetH3AutoForTest(t)
	candidates := testH3Proxies(6)
	auto := h3Setting{Mode: h3ModeAuto}

	if !noteH3Candidates(candidates, auto) {
		t.Fatal("the first auto ranking must report a change")
	}
	ranking := h3RankingValue.Load()
	if ranking == nil || !ranking.auto || len(ranking.keys) != 0 {
		t.Fatalf("auto at target 0 = %+v, want an empty auto ranking", ranking)
	}
	if h3ProxyEligible(auto, candidates[0].Key()) {
		t.Fatal("auto at target 0 must not admit any proxy")
	}

	h3AutoTarget.Store(2)
	if !rerankH3(auto) {
		t.Fatal("raising the target to 2 must change the ranking")
	}
	ranking = h3RankingValue.Load()
	if ranking == nil || !ranking.auto || ranking.cap != 2 || len(ranking.keys) != 2 {
		t.Fatalf("auto at target 2 = %+v, want two ranked proxies", ranking)
	}
	kept := map[string]bool{}
	for key := range ranking.keys {
		if !h3ProxyEligible(auto, key) {
			t.Fatalf("ranked key %s is not eligible", key)
		}
		kept[key] = true
	}

	h3AutoTarget.Store(3)
	if !rerankH3(auto) {
		t.Fatal("raising the target to 3 must change the ranking")
	}
	ranking = h3RankingValue.Load()
	if ranking == nil || len(ranking.keys) != 3 {
		t.Fatalf("auto at target 3 = %+v, want three ranked proxies", ranking)
	}
	for key := range kept {
		if !ranking.keys[key] {
			t.Fatalf("widening displaced a working member %s", key)
		}
	}

	// A count ranking is a different kind and must not be reused by auto.
	if !rerankH3(h3Setting{Mode: h3ModeTop, Cap: 3}) {
		t.Fatal("auto -> count at the same size must still re-store the ranking")
	}
	if h3RankingValue.Load().auto {
		t.Fatal("a count ranking must not be marked auto")
	}
	h3AutoTarget.Store(3)
	if !rerankH3(auto) || !h3RankingValue.Load().auto {
		t.Fatal("count -> auto must re-store an auto ranking")
	}

	// Leaving auto drops the ranking and the members.
	if !rerankH3(h3Setting{Mode: h3ModeDirect}) || h3RankingValue.Load() != nil {
		t.Fatal("leaving auto must drop the ranking")
	}
}

// The whole path, through the pressure sweep entry point: opt-in only, grows
// while calm, sheds on a memory stall, and resets when auto is left.
func TestH3AutoControllerThroughPressureSweep(t *testing.T) {
	withTempHome(t)
	resetH3AutoForTest(t)
	restoreH3Gate(t)
	prevState := globalControlState
	t.Cleanup(func() { globalControlState = prevState })
	state := newControlState()
	globalControlState = state
	h3AutoMemoryLimit = func() int64 { return int64(4) << 30 } // capacity 64
	candidates := testH3Proxies(6)
	noteH3Candidates(candidates, h3Setting{Mode: h3ModeDirect})
	calm := map[string]float64{"psi_mem": 0.0, "mem": 0.0, "heap": 0.05}

	// Default off: no h3 key, so the sweep must not touch the controller.
	if got := resolveH3Mode(state, proxyResolutionNoSource); got.Mode != h3ModeDirect {
		t.Fatalf("default setting = %v, want direct", got)
	}
	noteH3AutoPressure(calm)
	if h3AutoTarget.Load() != 0 || h3RankingValue.Load() != nil {
		t.Fatalf("default off changed state: target=%d ranking=%v", h3AutoTarget.Load(), h3RankingValue.Load())
	}

	if err := state.set("h3", "auto"); err != nil {
		t.Fatal(err)
	}
	noteH3AutoPressure(calm)
	if got := h3AutoTarget.Load(); got != h3AutoGrowStep {
		t.Fatalf("first calm sweep target = %d, want %d", got, h3AutoGrowStep)
	}
	if ranking := h3RankingValue.Load(); ranking == nil || !ranking.auto || len(ranking.keys) != 4 {
		t.Fatalf("first calm sweep ranking = %+v, want 4 proxies", ranking)
	}

	// A memory stall sheds to direct only within two sweeps.
	noteH3AutoPressure(map[string]float64{"heap": 1})
	noteH3AutoPressure(map[string]float64{"heap": 1})
	if got := h3AutoTarget.Load(); got != 0 {
		t.Fatalf("after a stall target = %d, want 0", got)
	}
	if ranking := h3RankingValue.Load(); ranking == nil || len(ranking.keys) != 0 {
		t.Fatalf("after a stall ranking = %+v, want no proxies", ranking)
	}

	// Leaving auto through the control path resets the episode.
	applyH3Mode(h3Setting{Mode: h3ModeDirect})
	if got := h3AutoTarget.Load(); got != 0 {
		t.Fatalf("leaving auto left target = %d", got)
	}
	if got := h3AutoController.smoothedValue(); got != 0 {
		t.Fatalf("leaving auto left smoothed pressure = %v", got)
	}
}

// Auto is never the beside-H1 auxiliary and always gives the direct identity
// the ordinary fallbacks.
func TestH3AutoDirectPolicyAndAuxiliary(t *testing.T) {
	auto := h3Setting{Mode: h3ModeAuto}
	if !auto.proxiesMayUseTransports() {
		t.Fatal("auto may give proxies the fallbacks")
	}
	if auto.directPolicy() != h3ModeDirect {
		t.Fatalf("auto direct policy = %v, want direct", auto.directPolicy())
	}
	prefs := h3ModePreferences(auto, true, false)
	want := connect.DefaultTransportModePreferences()
	for mode, priority := range want {
		if prefs[mode] != priority {
			t.Fatalf("auto direct mode %s priority %d, want %d", mode, prefs[mode], priority)
		}
	}
	if prefs[connect.TransportModeH3] == prefs[connect.TransportModeH1] {
		t.Fatalf("auto must not enable the beside-H1 auxiliary: %v", prefs)
	}
	if got := h3ModePreferences(auto, false, false); len(got) != 1 || got[connect.TransportModeH1] != 1 {
		t.Fatalf("an ineligible proxy under auto must be H1 only, got %v", got)
	}
}

// The default stays fail-open only for the controller: with no memory sensors
// the target grows to capacity, but only under auto.
func TestH3AutoPressureFailsOpen(t *testing.T) {
	var s h3AutoState
	for i := 0; i < 20; i++ {
		s.step(int64(4)<<30, nil)
	}
	if s.target != h3AutoMaxProxies {
		t.Fatalf("no sensors = %d, want capacity %d", s.target, h3AutoMaxProxies)
	}
	if s.smoothedValue() != 0 {
		t.Fatalf("no sensors must read as zero pressure, got %v", s.smoothedValue())
	}
}
