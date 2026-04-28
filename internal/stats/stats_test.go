package stats

import (
	"math/rand"
	"testing"
	"time"
)

// feed drives Stats forward in 1-second ticks, increasing `completed` at
// the given per-second rate. Returns the time at the end of the run.
func feed(s *Stats, start time.Time, durationSec int, ratePerSec func(sec int) float64) time.Time {
	t := start
	// Snapshot current completions so feed segments compose.
	baseline := s.completed
	accumulated := float64(baseline)
	for i := 0; i < durationSec; i++ {
		t = t.Add(time.Second)
		accumulated += ratePerSec(i)
		// Compute how many completions we need to add to reach the running total.
		target := int64(accumulated)
		for s.completed < target {
			s.IncCompletion()
		}
		s.Tick(t)
	}
	return t
}

func TestWarmupBeforeEnoughData(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	s := New(start)
	feed(s, start, 30, func(int) float64 { return 10 })
	snap := s.Snapshot(start.Add(30 * time.Second))
	if snap.Status != StatusWarmup {
		t.Errorf("status=%s, want warmup (only 30s elapsed)", snap.Status)
	}
}

func TestSteadyAfterRampThenFlat(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	s := New(start)
	// 60s ramp from 0 to 100/s.
	end := feed(s, start, 60, func(i int) float64 { return float64(i) * 100.0 / 60.0 })
	// 6 minutes flat at 100/s with small noise. Detector uses a 2-min
	// rate lookback inside a 2-min window inside a 2-min prior-window
	// check, so we need ~6 min of flat data past the ramp for both
	// windows to see only the steady signal.
	rng := rand.New(rand.NewSource(1))
	end = feed(s, end, 360, func(int) float64 { return 100 + (rng.Float64()-0.5)*4 })

	snap := s.Snapshot(end)
	if snap.Status != StatusSteady {
		t.Fatalf("status=%s, want steady after 6 min flat", snap.Status)
	}
	// Feed was ~100 wf/s == ~6000 wf/min.
	if snap.Rate30s < 5400 || snap.Rate30s > 6600 {
		t.Errorf("Rate30s=%.1f, want ~6000 (wf/min)", snap.Rate30s)
	}
	if snap.SteadyRate < 5400 || snap.SteadyRate > 6600 {
		t.Errorf("SteadyRate=%.1f, want ~6000 (wf/min)", snap.SteadyRate)
	}
	if snap.SteadyStdDev/snap.SteadyRate >= 0.10 {
		t.Errorf("relStd=%.3f, want <0.10", snap.SteadyStdDev/snap.SteadyRate)
	}
}

func TestRampingWhileStillRising(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	s := New(start)
	// 300s of monotonic rise (still climbing at the end)
	end := feed(s, start, 300, func(i int) float64 { return 50 + float64(i)*0.5 })
	snap := s.Snapshot(end)
	if snap.Status == StatusSteady {
		t.Errorf("status=%s, want ramping/other while rate still climbing", snap.Status)
	}
}

func TestDecliningDetection(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	s := New(start)
	// 2 min at 200/s, then 3 min at 100/s
	end := feed(s, start, 120, func(int) float64 { return 200 })
	end = feed(s, end, 180, func(int) float64 { return 100 })
	snap := s.Snapshot(end)
	// After 5 total minutes, prior window (2-4 min ago) had ~200, current
	// 2-min window is ~100 — should detect decline.
	if snap.Status != StatusDeclining {
		t.Errorf("status=%s, want declining after drop from 200 to 100/s", snap.Status)
	}
}

func TestFailuresCountSeparately(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	s := New(start)
	s.IncCompletion()
	s.IncCompletion()
	s.IncFailure()
	s.Tick(start.Add(time.Second))
	snap := s.Snapshot(start.Add(time.Second))
	if snap.Completions != 2 {
		t.Errorf("Completions=%d, want 2", snap.Completions)
	}
	if snap.Failures != 1 {
		t.Errorf("Failures=%d, want 1", snap.Failures)
	}
}
