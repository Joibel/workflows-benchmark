// Package metrics scrapes the argo workflow-controller Prometheus
// endpoint and extracts the handful of series we use to cross-check and
// enrich the benchmark report.
//
// The controller exposes these relevant metrics (see
// argo-workflows/docs/metrics.md):
//
//	argo_workflows_total_count{phase=...}          counter (v3.6+)
//	argo_workflows_workflows_gauge{phase=...}      gauge
//	argo_workflows_queue_depth_gauge{queue_name}   gauge
//	argo_workflows_operation_duration_seconds      histogram
//	argo_workflows_error_count{cause=...}          counter
package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// Snapshot is one scrape's worth of interesting numbers. All fields are
// best-effort — any missing series is silently omitted.
type Snapshot struct {
	At              time.Time
	CompletedTotal  map[string]float64 // phase -> count (from argo_workflows_total_count)
	WorkflowsGauge  map[string]float64 // phase -> count (from argo_workflows_workflows_gauge)
	QueueDepth      map[string]float64 // queue_name -> depth
	ErrorCount      map[string]float64 // cause -> count
	OpDurationP50   float64            // seconds, from histogram quantiles we compute
	OpDurationP99   float64
	ScrapeDuration  time.Duration
	Unavailable     bool   // true when scrape failed; other fields may be zero
	UnavailableErr  string // diagnostic for the reporter
}

// Scraper holds the HTTP client and URL. Zero value is not usable — use
// New.
type Scraper struct {
	url    string
	client *http.Client
}

// New returns a Scraper for the given /metrics URL. The URL must be
// non-empty; callers are expected to validate at the CLI boundary.
func New(url string) *Scraper {
	return &Scraper{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Scrape performs one HTTP GET and parses the response. Always returns a
// Snapshot (never nil); on failure, Unavailable is set.
func (s *Scraper) Scrape(ctx context.Context) *Snapshot {
	start := time.Now()
	snap := &Snapshot{At: start}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		snap.Unavailable = true
		snap.UnavailableErr = err.Error()
		return snap
	}
	resp, err := s.client.Do(req)
	if err != nil {
		snap.Unavailable = true
		snap.UnavailableErr = err.Error()
		return snap
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		snap.Unavailable = true
		snap.UnavailableErr = fmt.Sprintf("HTTP %d from %s", resp.StatusCode, s.url)
		return snap
	}
	if err := parseInto(resp.Body, snap); err != nil {
		snap.Unavailable = true
		snap.UnavailableErr = err.Error()
		return snap
	}
	snap.ScrapeDuration = time.Since(start)
	return snap
}

func parseInto(r io.Reader, snap *Snapshot) error {
	var parser expfmt.TextParser
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return fmt.Errorf("parse metrics: %w", err)
	}

	snap.CompletedTotal = map[string]float64{}
	snap.WorkflowsGauge = map[string]float64{}
	snap.QueueDepth = map[string]float64{}
	snap.ErrorCount = map[string]float64{}

	// Argo >=3.6 renamed to argo_workflows_total_count; older releases
	// used argo_workflows_count. Handle both.
	if mf, ok := families["argo_workflows_total_count"]; ok {
		collectByLabel(mf, "phase", snap.CompletedTotal, counterValue)
	} else if mf, ok := families["argo_workflows_count"]; ok {
		collectByLabel(mf, "status", snap.CompletedTotal, counterValue)
	}

	if mf, ok := families["argo_workflows_workflows_gauge"]; ok {
		collectByLabel(mf, "phase", snap.WorkflowsGauge, gaugeValue)
	}
	if mf, ok := families["argo_workflows_queue_depth_gauge"]; ok {
		collectByLabel(mf, "queue_name", snap.QueueDepth, gaugeValue)
	}
	if mf, ok := families["argo_workflows_error_count"]; ok {
		collectByLabel(mf, "cause", snap.ErrorCount, counterValue)
	}
	if mf, ok := families["argo_workflows_operation_duration_seconds"]; ok {
		// One histogram is expected (no interesting labels). If multiple
		// series are present, we pick the first — good enough for a
		// benchmark snapshot.
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h == nil {
				continue
			}
			snap.OpDurationP50 = histogramQuantile(h, 0.50)
			snap.OpDurationP99 = histogramQuantile(h, 0.99)
			break
		}
	}
	return nil
}

func collectByLabel(mf *dto.MetricFamily, label string, out map[string]float64, extract func(*dto.Metric) float64) {
	for _, m := range mf.GetMetric() {
		key := ""
		for _, lp := range m.GetLabel() {
			if lp.GetName() == label {
				key = lp.GetValue()
				break
			}
		}
		out[key] = extract(m)
	}
}

func counterValue(m *dto.Metric) float64 {
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	return 0
}

func gaugeValue(m *dto.Metric) float64 {
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	return 0
}

// histogramQuantile returns an approximate quantile from a Prometheus
// cumulative histogram. Buckets are sorted by upper-bound; we find the
// bucket containing the target rank and linearly interpolate within it.
// This is the same approximation `histogram_quantile()` uses in PromQL.
func histogramQuantile(h *dto.Histogram, q float64) float64 {
	buckets := h.GetBucket()
	if len(buckets) == 0 {
		return 0
	}
	total := float64(h.GetSampleCount())
	if total == 0 {
		return 0
	}
	target := q * total
	var prevCum float64
	var prevBound float64
	for _, b := range buckets {
		cum := float64(b.GetCumulativeCount())
		bound := b.GetUpperBound()
		if cum >= target {
			if cum == prevCum || bound == prevBound {
				return bound
			}
			// Linear interpolation within the bucket.
			frac := (target - prevCum) / (cum - prevCum)
			return prevBound + frac*(bound-prevBound)
		}
		prevCum = cum
		prevBound = bound
	}
	// All bucket boundaries below target — return +Inf bucket bound if present,
	// else the last finite bound.
	return buckets[len(buckets)-1].GetUpperBound()
}
