// Package report emits live throughput lines during the run and a final
// JSON summary on exit. The reporter is read-only: it pulls snapshots
// from the watcher, submitter, stats, and metrics scraper.
package report

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"time"

	"github.com/alan/workflows-benchmark/internal/metrics"
	"github.com/alan/workflows-benchmark/internal/nodes"
	"github.com/alan/workflows-benchmark/internal/stats"
	"github.com/alan/workflows-benchmark/internal/submitter"
	"github.com/alan/workflows-benchmark/internal/watcher"
)

// Live prints one status line per interval to `out`. It returns when
// ctx is cancelled.
type Live struct {
	Interval time.Duration
	Out      io.Writer

	Stats          *stats.Stats
	Watcher        *watcher.Watcher
	Submitter      *submitter.Submitter
	Scraper        *metrics.Scraper
	Nodes          *nodes.Watcher // optional
	TargetInFlight int            // shown alongside actual in-flight count
}

// Run blocks until ctx is done. Metrics scrapes happen on a separate
// cadence from log prints; the last snapshot is reused between prints.
// Scrape failures are logged on first-failure and first-recovery
// transitions so the user can see that the link dropped and came back
// without being spammed every tick.
func (l *Live) Run(ctx context.Context) {
	if l.Out == nil {
		l.Out = os.Stderr
	}
	if l.Interval <= 0 {
		l.Interval = 5 * time.Second
	}
	// Scrape on its own cadence — faster when disconnected so we notice
	// recovery promptly, slower when healthy to keep controller load low.
	const (
		scrapeEveryHealthy = 10 * time.Second
		scrapeEveryDown    = 2 * time.Second
	)
	lastScrape := time.Time{}
	var lastSnap *metrics.Snapshot
	lastDown := false // true if the previous scrape was Unavailable

	logTick := time.NewTicker(l.Interval)
	defer logTick.Stop()
	statsTick := time.NewTicker(time.Second)
	defer statsTick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-statsTick.C:
			l.Stats.Tick(now)
			if l.Scraper != nil {
				interval := scrapeEveryHealthy
				if lastDown {
					interval = scrapeEveryDown
				}
				if now.Sub(lastScrape) >= interval {
					scrapeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					lastSnap = l.Scraper.Scrape(scrapeCtx)
					cancel()
					lastScrape = now
					if lastSnap.Unavailable && !lastDown {
						log.Printf("metrics: connection lost (%s); retrying every %s", lastSnap.UnavailableErr, scrapeEveryDown)
						lastDown = true
					} else if !lastSnap.Unavailable && lastDown {
						log.Printf("metrics: reconnected")
						lastDown = false
					}
				}
			}
		case now := <-logTick.C:
			l.printLine(now, lastSnap)
		}
	}
}

func (l *Live) printLine(now time.Time, ms *metrics.Snapshot) {
	snap := l.Stats.Snapshot(now)
	pc := l.Watcher.Snapshot()
	submitted := l.Submitter.Submitted()
	failPct := 0.0
	if snap.Completions+snap.Failures > 0 {
		failPct = 100 * float64(snap.Failures) / float64(snap.Completions+snap.Failures)
	}
	wfQ, ttlQ, podQ, opP99 := "-", "-", "-", "-"
	metricsState := "?"
	if ms != nil {
		if ms.Unavailable {
			metricsState = "down"
		} else {
			metricsState = "ok"
			if v, ok := ms.QueueDepth["workflow_queue"]; ok {
				wfQ = fmt.Sprintf("%.0f", v)
			}
			if v, ok := ms.QueueDepth["workflow_ttl_queue"]; ok {
				ttlQ = fmt.Sprintf("%.0f", v)
			}
			if v, ok := ms.QueueDepth["pod_cleanup_queue"]; ok {
				podQ = fmt.Sprintf("%.0f", v)
			}
			if ms.OpDurationP99 > 0 {
				opP99 = fmt.Sprintf("%.0fms", ms.OpDurationP99*1000)
			}
		}
	}
	nodeField := ""
	if l.Nodes != nil {
		ns := l.Nodes.Snapshot()
		nodeField = fmt.Sprintf(" nodes=%d(%dr)", ns.Total, ns.Ready)
	}
	fmt.Fprintf(l.Out,
		"t=%s submitted=%d inflight=%d/%d (backlog=%d running=%d succ-gc=%d) done=%d (+%.0f/min 30s, +%.0f/min 5m) fail=%d (%.2f%%)%s metrics=%s wf-q=%s ttl-q=%s pod-q=%s op-p99=%s status=%s\n",
		formatElapsed(snap.Elapsed), submitted, pc.InFlight(), l.TargetInFlight,
		pc.Backlog(), pc.Running, pc.Succeeded,
		snap.Completions, snap.Rate30s, snap.Rate5m,
		snap.Failures, failPct, nodeField, metricsState, wfQ, ttlQ, podQ, opP99, snap.Status,
	)
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// FinalSummary is written to stdout (as a human-readable block) and
// optionally as JSON to a file path. Controller metric snapshot and
// cross-check vs informer counts are included so discrepancies are
// visible.
type FinalSummary struct {
	RunID             string          `json:"run_id"`
	Namespace         string          `json:"namespace"`
	Elapsed           string          `json:"elapsed"`
	TargetInFlight    int             `json:"target_in_flight"`
	Submitted         int64           `json:"submitted"`
	SubmitErrors      int64           `json:"submit_errors"`
	Throttled         int64           `json:"throttled"`
	Succeeded         int64           `json:"succeeded"`
	Failed            int64           `json:"failed"`
	Errored           int64           `json:"errored"`
	FailureRatePct    float64         `json:"failure_rate_pct"`
	FinalStatus       stats.Status    `json:"final_status"`
	SteadyRate        float64         `json:"steady_rate"`         // completions/sec, mean across steady stretch
	SteadyStdDev      float64         `json:"steady_stddev"`
	SteadyRateCI95Lo  float64         `json:"steady_rate_ci95_lo"` // 95% CI derived from sample std
	SteadyRateCI95Hi  float64         `json:"steady_rate_ci95_hi"`
	SteadySamples     int             `json:"steady_samples"`
	Peak30sRate       float64         `json:"peak_30s_rate_per_min"`
	ControllerMetrics *ControllerView `json:"controller_metrics,omitempty"`
	InformerVsCtrl    *Discrepancy    `json:"informer_vs_controller,omitempty"`
	Injections        map[string]bool `json:"cleanup_injections,omitempty"`
	Nodes             *NodeView       `json:"nodes,omitempty"`
	TuningHint        string          `json:"tuning_hint"`
}

type NodeView struct {
	FinalTotal int64 `json:"final_total"`
	FinalReady int64 `json:"final_ready"`
	MinTotal   int64 `json:"min_total"`
	MaxTotal   int64 `json:"max_total"`
	MinReady   int64 `json:"min_ready"`
	MaxReady   int64 `json:"max_ready"`
}

type ControllerView struct {
	CompletedByPhase map[string]float64 `json:"completed_by_phase"`
	QueueDepth       map[string]float64 `json:"queue_depth"`
	OpDurationP50S   float64            `json:"op_duration_p50_s"`
	OpDurationP99S   float64            `json:"op_duration_p99_s"`
	ScrapeError      string             `json:"scrape_error,omitempty"`
}

type Discrepancy struct {
	InformerSucceeded   int64   `json:"informer_succeeded"`
	ControllerSucceeded float64 `json:"controller_succeeded"`
	DeltaPct            float64 `json:"delta_pct"`
}

// BuildSummary collects everything needed for a final report. `start` is
// the run start time; `peakRate` is caller-tracked (Live can expose it).
func BuildSummary(
	runID, namespace string,
	start, end time.Time,
	targetInFlight int,
	sub *submitter.Submitter,
	w *watcher.Watcher,
	st *stats.Stats,
	ms *metrics.Snapshot,
	nw *nodes.Watcher,
	injections map[string]bool,
) *FinalSummary {
	snap := st.Snapshot(end)
	pc := w.Snapshot()
	total := snap.Completions + snap.Failures
	failPct := 0.0
	if total > 0 {
		failPct = 100 * float64(snap.Failures) / float64(total)
	}

	fs := &FinalSummary{
		RunID:          runID,
		Namespace:      namespace,
		Elapsed:        end.Sub(start).Round(time.Second).String(),
		TargetInFlight: targetInFlight,
		Submitted:      sub.Submitted(),
		SubmitErrors:   sub.Errors(),
		Throttled:      sub.Throttled(),
		Succeeded:      pc.Succeeded,
		Failed:         pc.Failed,
		Errored:        pc.Errored,
		FailureRatePct: failPct,
		FinalStatus:    snap.Status,
		SteadyRate:     snap.SteadyRate,
		SteadyStdDev:   snap.SteadyStdDev,
		SteadySamples:  snap.SteadySamples,
		Peak30sRate:    snap.Rate30s, // best proxy available from Snapshot
		Injections:     injections,
	}
	if snap.SteadySamples > 0 {
		// 1.96 * stddev / sqrt(n) — the classic normal-approximation 95% CI
		// for the sample mean. Fine at this sample size.
		half := 1.96 * snap.SteadyStdDev / math.Sqrt(float64(snap.SteadySamples))
		fs.SteadyRateCI95Lo = snap.SteadyRate - half
		fs.SteadyRateCI95Hi = snap.SteadyRate + half
	}
	if ms != nil {
		cv := &ControllerView{
			CompletedByPhase: ms.CompletedTotal,
			QueueDepth:       ms.QueueDepth,
			OpDurationP50S:   ms.OpDurationP50,
			OpDurationP99S:   ms.OpDurationP99,
		}
		if ms.Unavailable {
			cv.ScrapeError = ms.UnavailableErr
		}
		fs.ControllerMetrics = cv

		if !ms.Unavailable {
			ctrlSucc := ms.CompletedTotal["Succeeded"]
			if ctrlSucc > 0 {
				delta := 100 * (float64(pc.Succeeded) - ctrlSucc) / ctrlSucc
				fs.InformerVsCtrl = &Discrepancy{
					InformerSucceeded:   pc.Succeeded,
					ControllerSucceeded: ctrlSucc,
					DeltaPct:            delta,
				}
			}
		}
	}
	if nw != nil {
		ns := nw.Snapshot()
		fs.Nodes = &NodeView{
			FinalTotal: ns.Total,
			FinalReady: ns.Ready,
			MinTotal:   ns.MinTotal,
			MaxTotal:   ns.MaxTotal,
			MinReady:   ns.MinReady,
			MaxReady:   ns.MaxReady,
		}
	}
	fs.TuningHint = tuningHint(sub, snap)
	return fs
}

func tuningHint(sub *submitter.Submitter, snap stats.Snapshot) string {
	if sub.Throttled() > 0 {
		return "controller throttled submit-side (HTTP 429). Consider raising --qps/--burst on workflow-controller and kube-apiserver."
	}
	if snap.Status != stats.StatusSteady {
		return "run did not reach steady state before exit. Let it run longer, or raise --target-in-flight."
	}
	return "relevant controller knobs: --workflow-workers, --pod-cleanup-workers, --workflow-ttl-workers, --qps, --burst, PARALLELISM_LIMIT env var."
}

// Write emits the summary to stdout (human block) and, if path != "",
// as JSON to that file path. Errors are returned; the caller typically
// logs them and continues.
func (fs *FinalSummary) Write(humanOut io.Writer, jsonPath string) error {
	if humanOut == nil {
		humanOut = os.Stdout
	}
	fmt.Fprintln(humanOut, "=== workflows-benchmark summary ===")
	fmt.Fprintf(humanOut, "run_id:          %s\n", fs.RunID)
	fmt.Fprintf(humanOut, "namespace:       %s\n", fs.Namespace)
	fmt.Fprintf(humanOut, "elapsed:         %s\n", fs.Elapsed)
	fmt.Fprintf(humanOut, "target_in_flight: %d\n", fs.TargetInFlight)
	fmt.Fprintf(humanOut, "submitted:       %d (errors=%d throttled=%d)\n", fs.Submitted, fs.SubmitErrors, fs.Throttled)
	fmt.Fprintf(humanOut, "succeeded:       %d\n", fs.Succeeded)
	fmt.Fprintf(humanOut, "failed+errored:  %d+%d (%.2f%% failure rate)\n", fs.Failed, fs.Errored, fs.FailureRatePct)
	fmt.Fprintf(humanOut, "final_status:    %s\n", fs.FinalStatus)
	if fs.SteadySamples > 0 {
		fmt.Fprintf(humanOut, "steady_rate:     %.0f wf/min  (95%% CI %.0f–%.0f, n=%d)\n",
			fs.SteadyRate, fs.SteadyRateCI95Lo, fs.SteadyRateCI95Hi, fs.SteadySamples)
	} else {
		fmt.Fprintf(humanOut, "steady_rate:     n/a (never reached steady state)\n")
	}
	if cv := fs.ControllerMetrics; cv != nil {
		if cv.ScrapeError != "" {
			fmt.Fprintf(humanOut, "controller_metrics: unavailable (%s)\n", cv.ScrapeError)
		} else {
			fmt.Fprintf(humanOut, "controller:      p50=%.0fms p99=%.0fms, succeeded_total=%.0f\n",
				cv.OpDurationP50S*1000, cv.OpDurationP99S*1000, cv.CompletedByPhase["Succeeded"])
		}
	}
	if d := fs.InformerVsCtrl; d != nil {
		fmt.Fprintf(humanOut, "cross-check:     informer=%d, controller=%.0f (delta %+.2f%%)\n",
			d.InformerSucceeded, d.ControllerSucceeded, d.DeltaPct)
	}
	if n := fs.Nodes; n != nil {
		fmt.Fprintf(humanOut, "nodes:           final=%d (%d ready)  range total=%d–%d  ready=%d–%d\n",
			n.FinalTotal, n.FinalReady, n.MinTotal, n.MaxTotal, n.MinReady, n.MaxReady)
	}
	fmt.Fprintf(humanOut, "hint:            %s\n", fs.TuningHint)

	if jsonPath != "" {
		f, err := os.Create(jsonPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", jsonPath, err)
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(fs); err != nil {
			return fmt.Errorf("encode json: %w", err)
		}
	}
	return nil
}

