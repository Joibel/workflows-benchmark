// Package submitter drives workflow submission: it maintains a target
// backlog of unstarted workflows (phase "" or "Pending") by watching
// the informer's snapshot and pushing new Create() calls through a
// bounded worker pool. The benchmark's design hinges on this loop — as
// long as backlog > 0 at all times, the controller is never starved and
// the resulting completion rate reflects controller+k8s throughput.
package submitter

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	versioned "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alan/workflows-benchmark/internal/loader"
	"github.com/alan/workflows-benchmark/internal/watcher"
)

// Config drives submission behavior.
type Config struct {
	Namespace      string
	TargetInFlight int           // desired count of Unknown+Pending+Running+Succeeded
	Workers        int           // worker goroutines calling Create()
	RefillPeriod   time.Duration // how often we read the snapshot
}

// Default parameters when zero-value fields are passed.
func (c *Config) applyDefaults() {
	if c.Workers <= 0 {
		c.Workers = 16
	}
	if c.RefillPeriod <= 0 {
		c.RefillPeriod = 100 * time.Millisecond
	}
	if c.TargetInFlight <= 0 {
		c.TargetInFlight = 1000
	}
}

// Submitter pushes Workflow creates at the cluster.
type Submitter struct {
	cfg      Config
	client   versioned.Interface
	template *loader.Template
	w        *watcher.Watcher

	submitted atomic.Int64 // successful Create() responses
	errored   atomic.Int64 // non-retryable Create() failures
	throttled atomic.Int64 // 429 / TooManyRequests responses

	// sentInFlight tracks how many creates have been dispatched but not
	// yet observed by the informer. Without this, the refill loop would
	// re-submit while API-server writes are still in flight, overshooting
	// the target backlog significantly.
	sentInFlight atomic.Int64
}

// New builds a Submitter. Use Run() to drive it.
func New(client versioned.Interface, tpl *loader.Template, w *watcher.Watcher, cfg Config) *Submitter {
	cfg.applyDefaults()
	return &Submitter{cfg: cfg, client: client, template: tpl, w: w}
}

// Submitted returns the count of successful Create() calls.
func (s *Submitter) Submitted() int64 { return s.submitted.Load() }

// Errors returns the count of non-retryable Create() failures.
func (s *Submitter) Errors() int64 { return s.errored.Load() }

// Throttled returns the count of 429 responses (retried, still worth reporting).
func (s *Submitter) Throttled() int64 { return s.throttled.Load() }

// Run starts the worker pool and refill loop; blocks until ctx is done.
func (s *Submitter) Run(ctx context.Context) {
	// Buffered enough that the refill loop can push up to one refill
	// batch without blocking, but small enough that a paused informer
	// (e.g. reconnect) won't let us overshoot by tens of thousands.
	jobs := make(chan struct{}, s.cfg.TargetInFlight)

	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx, jobs)
		}()
	}

	// Refill loop.
	ticker := time.NewTicker(s.cfg.RefillPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case <-ticker.C:
			s.refill(jobs)
		}
	}
}

func (s *Submitter) refill(jobs chan<- struct{}) {
	snap := s.w.Snapshot()
	// in-flight (Unknown+Pending+Running+Succeeded) as seen by the
	// informer, plus two adjustments for work the informer hasn't
	// counted yet:
	//   sentInFlight  — creates dispatched to a worker but not yet
	//                   returned from the API server.
	//   observationLag — creates that returned successfully but whose
	//                   first informer event hasn't arrived yet. When
	//                   the controller is queue-saturated this window
	//                   can be many seconds, so without this term the
	//                   refill loop overshoots the target dramatically.
	// Failed/Errored are intentionally excluded from InFlight: a wave of
	// failures should not block us from submitting new work.
	observationLag := s.submitted.Load() - s.w.Observed()
	if observationLag < 0 {
		observationLag = 0
	}
	projected := snap.InFlight() + s.sentInFlight.Load() + observationLag
	need := int64(s.cfg.TargetInFlight) - projected
	for i := int64(0); i < need; i++ {
		s.sentInFlight.Add(1)
		select {
		case jobs <- struct{}{}:
		default:
			// Channel full — undo the reservation and stop refilling this
			// tick. Workers are already saturated; we'll catch up next
			// tick.
			s.sentInFlight.Add(-1)
			return
		}
	}
}

func (s *Submitter) worker(ctx context.Context, jobs <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-jobs:
			if !ok {
				return
			}
			s.submitOne(ctx)
			s.sentInFlight.Add(-1)
		}
	}
}

func (s *Submitter) submitOne(ctx context.Context) {
	wf := s.template.Clone()
	// Short-circuit if we're shutting down.
	if ctx.Err() != nil {
		return
	}
	const maxAttempts = 5
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_, err := s.client.ArgoprojV1alpha1().
			Workflows(s.cfg.Namespace).
			Create(ctx, wf, metav1.CreateOptions{})
		if err == nil {
			s.submitted.Add(1)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if apierrors.IsTooManyRequests(err) || apierrors.IsServerTimeout(err) {
			s.throttled.Add(1)
			backoff := time.Duration(attempt) * 200 * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		// Non-retryable failure (quota, validation, etc.): record and give up.
		s.errored.Add(1)
		return
	}
	// Exhausted retries — treat as error.
	s.errored.Add(1)
}
