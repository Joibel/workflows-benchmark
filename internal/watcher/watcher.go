// Package watcher runs a client-go informer on the Workflow CRD,
// filtered to a single benchmark run via the run-id label, and keeps
// atomic phase counts so the submitter can decide whether to refill the
// backlog and the stats package can record terminal-phase transitions.
package watcher

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v3/pkg/apis/workflow/v1alpha1"
	versioned "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	externalversions "github.com/argoproj/argo-workflows/v3/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/alan/workflows-benchmark/internal/loader"
)

// PhaseCounts is a snapshot of current workflow counts per phase.
type PhaseCounts struct {
	Unknown   int64 // phase == "" — submitted, not yet reconciled
	Pending   int64
	Running   int64
	Succeeded int64
	Failed    int64
	Errored   int64
}

// Backlog is Unknown + Pending — workflows sitting in the cluster that
// haven't started executing yet. Informational only.
func (c PhaseCounts) Backlog() int64 { return c.Unknown + c.Pending }

// InFlight is the submitter's gating count: everything consuming
// cluster resources or waiting for GC. Failed and Errored are excluded
// so a wave of failures doesn't starve us, but Succeeded is included so
// we stop submitting if the TTL controller can't keep up — this is how
// we protect the cluster from pileup.
func (c PhaseCounts) InFlight() int64 {
	return c.Unknown + c.Pending + c.Running + c.Succeeded
}

// Terminal is Succeeded + Failed + Errored — cumulative finished count
// observed via the informer. This climbs monotonically over the run
// (thanks to TTL=0 cleanup we delete workflows on success, but phase
// transitions are seen before the Delete event).
func (c PhaseCounts) Terminal() int64 { return c.Succeeded + c.Failed + c.Errored }

// Watcher owns an informer and atomic counters.
type Watcher struct {
	client     versioned.Interface
	namespace  string
	runID      string
	resync     time.Duration

	// Counters — accessed lock-free from OnAdd/OnUpdate callbacks.
	unknown, pending, running, succeeded, failed, errored atomic.Int64

	// observed counts unique workflow UIDs the informer has seen at least
	// once. Monotonic — never decremented on delete. The submitter uses
	// (submitted - observed) to account for creates that returned but
	// haven't yet been delivered by the watch stream, which is the only
	// way to keep inflight near the target when the controller is slow
	// enough that observation lag is significant.
	observed atomic.Int64

	// perWfPhase tracks last-seen phase for each workflow so updates
	// increment/decrement the right counters without double-counting.
	mu         sync.Mutex
	perWfPhase map[string]wfv1.WorkflowPhase
	// firstTerminal records UIDs we've already emitted a terminal event
	// for, so replays don't produce duplicate stats increments.
	firstTerminal map[string]struct{}

	// Terminal is closed by Run() and receives the phase of every
	// workflow the first time it reaches a terminal state.
	Terminal chan wfv1.WorkflowPhase
}

// New builds a Watcher. Call Run() to start the informer. The workflow
// client can be built from rest.Config via versioned.NewForConfig.
func New(client versioned.Interface, namespace, runID string) *Watcher {
	return &Watcher{
		client:        client,
		namespace:     namespace,
		runID:         runID,
		resync:        0,
		perWfPhase:    map[string]wfv1.WorkflowPhase{},
		firstTerminal: map[string]struct{}{},
		// Buffered so a slow stats consumer never blocks the informer.
		Terminal: make(chan wfv1.WorkflowPhase, 1024),
	}
}

// Observed returns the monotonic count of unique workflow UIDs the
// informer has observed at least once during this run. Used by the
// submitter to compensate for watch-stream lag when projecting in-flight.
func (w *Watcher) Observed() int64 { return w.observed.Load() }

// Snapshot returns current phase counts.
func (w *Watcher) Snapshot() PhaseCounts {
	return PhaseCounts{
		Unknown:   w.unknown.Load(),
		Pending:   w.pending.Load(),
		Running:   w.running.Load(),
		Succeeded: w.succeeded.Load(),
		Failed:    w.failed.Load(),
		Errored:   w.errored.Load(),
	}
}

// Run starts the informer and blocks until ctx is done. It returns once
// the informer has stopped.
func (w *Watcher) Run(ctx context.Context) error {
	factory := externalversions.NewSharedInformerFactoryWithOptions(
		w.client,
		w.resync,
		externalversions.WithNamespace(w.namespace),
		externalversions.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = fmt.Sprintf("%s=%s", loader.RunIDLabel, w.runID)
		}),
	)
	informer := factory.Argoproj().V1alpha1().Workflows().Informer()

	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.handle(obj, false) },
		UpdateFunc: func(_, obj interface{}) { w.handle(obj, false) },
		DeleteFunc: func(obj interface{}) { w.handle(obj, true) },
	})
	if err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("informer cache did not sync")
	}
	<-ctx.Done()
	// factory.Start wired the informer to ctx.Done(); goroutines exit
	// when the stop channel closes. Close our terminal channel so
	// downstream stats consumers see EOF.
	close(w.Terminal)
	return nil
}

// handle is called for every Add/Update/Delete. It diff-applies the
// phase transition against our per-workflow state so counters stay
// consistent even under list-relist / resync storms.
//
// Deletes decrement whatever phase we last recorded; terminal workflows
// that go through TTL=0 cleanup will first update to Succeeded and then
// delete, so by the time DeleteFunc fires we've already counted them as
// terminal — the delete just removes them from the per-wf map.
func (w *Watcher) handle(obj interface{}, isDelete bool) {
	wf, ok := obj.(*wfv1.Workflow)
	if !ok {
		// DeletedFinalStateUnknown wraps the object when we miss events.
		if tomb, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
			if wf2, ok3 := tomb.Obj.(*wfv1.Workflow); ok3 {
				wf = wf2
			}
		}
		if wf == nil {
			return
		}
	}
	key := string(wf.UID)
	if key == "" {
		return
	}
	newPhase := wf.Status.Phase

	w.mu.Lock()
	prev, had := w.perWfPhase[key]
	if isDelete {
		delete(w.perWfPhase, key)
	} else {
		w.perWfPhase[key] = newPhase
	}
	// Emit terminal event on first observation.
	emitTerminal := false
	if !isDelete && isTerminal(newPhase) {
		if _, already := w.firstTerminal[key]; !already {
			w.firstTerminal[key] = struct{}{}
			emitTerminal = true
		}
	}
	w.mu.Unlock()

	if isDelete {
		w.adjust(prev, -1)
		return
	}
	if !had {
		w.observed.Add(1)
		w.adjust(newPhase, +1)
	} else if prev != newPhase {
		w.adjust(prev, -1)
		w.adjust(newPhase, +1)
	}

	if emitTerminal {
		// Non-blocking send; drop if the channel is full (we still
		// count it in the counters, so nothing is truly lost — the
		// stats package just falls back to delta-from-snapshot).
		select {
		case w.Terminal <- newPhase:
		default:
		}
	}
}

func (w *Watcher) adjust(phase wfv1.WorkflowPhase, delta int64) {
	switch phase {
	case wfv1.WorkflowUnknown:
		w.unknown.Add(delta)
	case wfv1.WorkflowPending:
		w.pending.Add(delta)
	case wfv1.WorkflowRunning:
		w.running.Add(delta)
	case wfv1.WorkflowSucceeded:
		w.succeeded.Add(delta)
	case wfv1.WorkflowFailed:
		w.failed.Add(delta)
	case wfv1.WorkflowError:
		w.errored.Add(delta)
	}
}

func isTerminal(p wfv1.WorkflowPhase) bool {
	return p == wfv1.WorkflowSucceeded || p == wfv1.WorkflowFailed || p == wfv1.WorkflowError
}
