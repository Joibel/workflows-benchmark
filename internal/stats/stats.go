// Package stats keeps a time-series of cumulative workflow completions
// and derives rolling rates (completions/second over 30s and 5m windows)
// plus a plateau detector. The detector is what tells the reporter that
// the benchmark has reached steady state — it is informational only, the
// tool never stops on its own.
package stats

import (
	"math"
	"sync"
	"time"
)

// Sample is one (t, cumulative) reading. Stats stores these at roughly 1s
// resolution by coalescing updates.
type Sample struct {
	T  time.Time
	N  int64
}

// Status classifies the rolling throughput.
type Status string

const (
	StatusWarmup    Status = "warmup"    // not enough data yet
	StatusRamping   Status = "ramping"   // rate still rising
	StatusSteady    Status = "steady"    // rate stable
	StatusDeclining Status = "declining" // rate falling
)

// Snapshot is a point-in-time view of the stats for the reporter.
// All rate fields are completions per minute.
type Snapshot struct {
	Elapsed       time.Duration
	Completions   int64
	Failures      int64
	Rate30s       float64 // completions per minute, over a rolling 30s window
	Rate5m        float64 // completions per minute, over a rolling 5m window
	Status        Status
	SteadyRate    float64 // wf/min, mean of 30s-rate samples during current steady stretch (0 if not steady)
	SteadyStdDev  float64 // wf/min, stddev of same
	SteadySamples int
}

// Stats is safe for concurrent use. Call Tick() periodically (e.g. every
// second) to freeze the current counts into the ring buffer; call
// IncCompletion / IncFailure on each terminal-phase transition.
type Stats struct {
	mu sync.Mutex

	startedAt time.Time
	completed int64
	failed    int64

	// Ring of (t, completed) samples at ~1s resolution. We keep a long
	// history (say 30 minutes) so we can compute both 30s and 5m windows
	// cheaply and show plateau context.
	samples []Sample
	maxHist time.Duration

	// Plateau detector state — the last time we evaluated, so we only
	// recompute once per tick.
	lastPlateauAt time.Time
	status        Status
	steadySince   time.Time // zero if not steady
	steadyRate    float64
	steadyStd     float64
	steadyN       int
}

// New returns a Stats ready to use. startedAt is typically time.Now().
func New(startedAt time.Time) *Stats {
	return &Stats{
		startedAt: startedAt,
		maxHist:   30 * time.Minute,
		status:    StatusWarmup,
	}
}

// IncCompletion records one successful workflow completion.
func (s *Stats) IncCompletion() {
	s.mu.Lock()
	s.completed++
	s.mu.Unlock()
}

// IncFailure records one failed/errored workflow. Note: failures are not
// counted toward Completions — they are reported separately.
func (s *Stats) IncFailure() {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

// Tick appends the current cumulative completion count to the ring.
// Call this on a steady timer (e.g. time.Tick(1s)).
func (s *Stats) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Coalesce: if the last sample is within 500ms, overwrite it.
	if n := len(s.samples); n > 0 && now.Sub(s.samples[n-1].T) < 500*time.Millisecond {
		s.samples[n-1] = Sample{T: now, N: s.completed}
	} else {
		s.samples = append(s.samples, Sample{T: now, N: s.completed})
	}

	// Trim old samples.
	cutoff := now.Add(-s.maxHist)
	i := 0
	for i < len(s.samples) && s.samples[i].T.Before(cutoff) {
		i++
	}
	if i > 0 {
		s.samples = append(s.samples[:0], s.samples[i:]...)
	}

	s.evaluatePlateau(now)
}

// rate returns completions/min computed across the tail `window` of the
// ring. If the ring is shorter than `window`, uses whatever is available.
// Returns 0 if fewer than 2 samples.
func (s *Stats) rateLocked(now time.Time, window time.Duration) float64 {
	if len(s.samples) < 2 {
		return 0
	}
	cutoff := now.Add(-window)
	// Find first sample at-or-after cutoff.
	i := len(s.samples) - 1
	for i > 0 && s.samples[i-1].T.After(cutoff) {
		i--
	}
	first := s.samples[i]
	last := s.samples[len(s.samples)-1]
	dt := last.T.Sub(first.T).Minutes()
	if dt <= 0 {
		return 0
	}
	return float64(last.N-first.N) / dt
}

// rateLookback is how far back each rate sample reaches when measuring
// the instantaneous throughput. Longer = smoother. 30s was tried first
// but is too noisy for DAG workflows where many pods finish together
// within each wf: the 30s-rate relative stddev rarely drops below 10%
// even at true steady state. 2 minutes absorbs those bursts while
// staying responsive enough to catch genuine ramps.
const rateLookback = 2 * time.Minute

// evaluatePlateau classifies the current status. Called from Tick with
// the lock held.
//
// Definition of steady: over the trailing 2-minute window of rate
// samples (each sample averaged over the preceding 2 minutes),
// relative stddev < 10% AND the window mean hasn't risen by >5% vs
// the previous 2-minute window.
func (s *Stats) evaluatePlateau(now time.Time) {
	// Only re-evaluate once per second at most.
	if now.Sub(s.lastPlateauAt) < time.Second {
		return
	}
	s.lastPlateauAt = now

	if now.Sub(s.startedAt) < 90*time.Second {
		s.status = StatusWarmup
		return
	}

	currWin := s.rateSamplesOverLookback(now.Add(-2*time.Minute), now, rateLookback)
	if len(currWin) < 30 {
		s.status = StatusWarmup
		return
	}

	currMean, currStd := meanStd(currWin)
	if currMean <= 0 {
		s.status = StatusWarmup
		return
	}
	relStd := currStd / currMean

	// Prior 2-minute window, used for the "is the rate still rising?" check.
	prior := s.rateSamplesOverLookback(now.Add(-4*time.Minute), now.Add(-2*time.Minute), rateLookback)
	var priorMean float64
	if len(prior) >= 10 {
		priorMean, _ = meanStd(prior)
	}

	rising := priorMean > 0 && (currMean-priorMean)/priorMean > 0.05
	falling := priorMean > 0 && (priorMean-currMean)/priorMean > 0.05

	switch {
	case relStd < 0.10 && !rising && !falling:
		if s.status != StatusSteady {
			s.steadySince = now
		}
		s.status = StatusSteady
		s.steadyRate = currMean
		s.steadyStd = currStd
		s.steadyN = len(currWin)
	case falling:
		s.status = StatusDeclining
		s.steadySince = time.Time{}
	default:
		s.status = StatusRamping
		s.steadySince = time.Time{}
	}
}

// rateSamplesOverLookback returns rate samples (completions per
// minute) whose anchor time falls in [from, to], where each sample
// averages the rate over `lookback` ending at its anchor. If the ring
// is shorter than `lookback` at a given anchor, the sample degrades
// gracefully to "whatever data we have" — the oldest-available sample
// is used as the window start.
func (s *Stats) rateSamplesOverLookback(from, to time.Time, lookback time.Duration) []float64 {
	if len(s.samples) < 2 {
		return nil
	}
	var out []float64
	for j := 0; j < len(s.samples); j++ {
		tj := s.samples[j].T
		if tj.Before(from) || tj.After(to) {
			continue
		}
		windowStart := tj.Add(-lookback)
		i := 0
		for i < j && s.samples[i].T.Before(windowStart) {
			i++
		}
		if i == j {
			continue
		}
		dt := s.samples[j].T.Sub(s.samples[i].T).Minutes()
		if dt <= 0 {
			continue
		}
		out = append(out, float64(s.samples[j].N-s.samples[i].N)/dt)
	}
	return out
}

// Snapshot returns a copy of the current derived stats for display.
func (s *Stats) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap := Snapshot{
		Elapsed:     now.Sub(s.startedAt),
		Completions: s.completed,
		Failures:    s.failed,
		Rate30s:     s.rateLocked(now, 30*time.Second),
		Rate5m:      s.rateLocked(now, 5*time.Minute),
		Status:      s.status,
	}
	if s.status == StatusSteady {
		snap.SteadyRate = s.steadyRate
		snap.SteadyStdDev = s.steadyStd
		snap.SteadySamples = s.steadyN
	}
	return snap
}

func meanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	var sq float64
	for _, x := range xs {
		d := x - mean
		sq += d * d
	}
	std = math.Sqrt(sq / float64(len(xs)))
	return mean, std
}
