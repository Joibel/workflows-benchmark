package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A slimmed-down sample of what the workflow-controller exposes. Based
// on argo-workflows/docs/metrics.md; only the series we consume are
// included.
const fixture = `
# HELP argo_workflows_total_count Total number of workflow state transitions.
# TYPE argo_workflows_total_count counter
argo_workflows_total_count{phase="Succeeded"} 1234
argo_workflows_total_count{phase="Failed"} 7
argo_workflows_total_count{phase="Error"} 1
# HELP argo_workflows_workflows_gauge Current count of workflows.
# TYPE argo_workflows_workflows_gauge gauge
argo_workflows_workflows_gauge{phase="Running"} 42
argo_workflows_workflows_gauge{phase="Pending"} 3
# HELP argo_workflows_queue_depth_gauge Depth of workqueues.
# TYPE argo_workflows_queue_depth_gauge gauge
argo_workflows_queue_depth_gauge{queue_name="workflow_queue"} 5
argo_workflows_queue_depth_gauge{queue_name="pod_cleanup_queue"} 0
# HELP argo_workflows_error_count Counter of errors by cause.
# TYPE argo_workflows_error_count counter
argo_workflows_error_count{cause="OperationPanic"} 0
argo_workflows_error_count{cause="CronWorkflowSubmissionError"} 2
# HELP argo_workflows_operation_duration_seconds Duration of workflow operation.
# TYPE argo_workflows_operation_duration_seconds histogram
argo_workflows_operation_duration_seconds_bucket{le="0.1"} 10
argo_workflows_operation_duration_seconds_bucket{le="0.5"} 90
argo_workflows_operation_duration_seconds_bucket{le="1"} 95
argo_workflows_operation_duration_seconds_bucket{le="+Inf"} 100
argo_workflows_operation_duration_seconds_sum 25.5
argo_workflows_operation_duration_seconds_count 100
`

func TestScrapeParsesFixture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	s := New(srv.URL)
	snap := s.Scrape(t.Context())
	if snap.Unavailable {
		t.Fatalf("Unavailable: %s", snap.UnavailableErr)
	}

	if got := snap.CompletedTotal["Succeeded"]; got != 1234 {
		t.Errorf("CompletedTotal[Succeeded]=%v, want 1234", got)
	}
	if got := snap.CompletedTotal["Failed"]; got != 7 {
		t.Errorf("CompletedTotal[Failed]=%v, want 7", got)
	}
	if got := snap.WorkflowsGauge["Running"]; got != 42 {
		t.Errorf("WorkflowsGauge[Running]=%v, want 42", got)
	}
	if got := snap.QueueDepth["workflow_queue"]; got != 5 {
		t.Errorf("QueueDepth[workflow_queue]=%v, want 5", got)
	}
	if got := snap.ErrorCount["CronWorkflowSubmissionError"]; got != 2 {
		t.Errorf("ErrorCount[CronWorkflowSubmissionError]=%v, want 2", got)
	}

	// With the fixture buckets, the p50 falls in (0.1, 0.5] and the p99
	// falls in (1, +Inf]. Check plausible ranges rather than exact values.
	if snap.OpDurationP50 <= 0.1 || snap.OpDurationP50 > 0.5 {
		t.Errorf("p50=%v, want in (0.1, 0.5]", snap.OpDurationP50)
	}
	if snap.OpDurationP99 < 1 {
		t.Errorf("p99=%v, want >= 1", snap.OpDurationP99)
	}
}

func TestScrapeHandlesNetworkError(t *testing.T) {
	s := New("http://127.0.0.1:1/does-not-exist")
	snap := s.Scrape(t.Context())
	if !snap.Unavailable {
		t.Errorf("expected Unavailable=true on connection failure")
	}
}

func TestScrapeHandlesNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	s := New(srv.URL)
	snap := s.Scrape(t.Context())
	if !snap.Unavailable {
		t.Errorf("expected Unavailable=true on 500")
	}
	if !strings.Contains(snap.UnavailableErr, "500") {
		t.Errorf("UnavailableErr=%q, want to contain 500", snap.UnavailableErr)
	}
}
