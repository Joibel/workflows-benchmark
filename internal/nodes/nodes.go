// Package nodes watches the cluster's Node objects and exposes a
// running count of total + ready nodes, plus min/max seen during the
// run. This is informational: a plateau at a certain node count tells
// you whether throughput was capacity-bound (saturating pods per node)
// vs controller-bound (more nodes wouldn't help).
package nodes

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Snapshot is a point-in-time view for the reporter.
type Snapshot struct {
	Total    int64
	Ready    int64
	MinTotal int64
	MaxTotal int64
	MinReady int64
	MaxReady int64
}

// Watcher keeps atomic counters, updated by the informer on
// OnAdd/Update/Delete events.
type Watcher struct {
	client kubernetes.Interface

	total, ready                           atomic.Int64
	minTotal, maxTotal, minReady, maxReady atomic.Int64

	// readyByUID tracks last-seen Ready-ness per node UID so updates
	// diff cleanly without re-scanning the world. A delete looks up the
	// last value and decrements accordingly.
	mu         sync.Mutex
	readyByUID map[string]bool

	initMinOnce sync.Once
}

// New returns a Watcher ready for Run().
func New(client kubernetes.Interface) *Watcher {
	w := &Watcher{
		client:     client,
		readyByUID: map[string]bool{},
	}
	// Start min values high; they're overwritten once we see the first
	// real count. maxTotal/maxReady start at 0, which is fine.
	w.minTotal.Store(-1)
	w.minReady.Store(-1)
	return w
}

// Snapshot returns the current node counts and min/max so far.
func (w *Watcher) Snapshot() Snapshot {
	return Snapshot{
		Total:    w.total.Load(),
		Ready:    w.ready.Load(),
		MinTotal: loadNonNeg(&w.minTotal),
		MaxTotal: w.maxTotal.Load(),
		MinReady: loadNonNeg(&w.minReady),
		MaxReady: w.maxReady.Load(),
	}
}

func loadNonNeg(a *atomic.Int64) int64 {
	v := a.Load()
	if v < 0 {
		return 0
	}
	return v
}

// Run starts the Node informer and blocks until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(w.client, 0)
	informer := factory.Core().V1().Nodes().Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.handle(obj, false) },
		UpdateFunc: func(_, obj interface{}) { w.handle(obj, false) },
		DeleteFunc: func(obj interface{}) { w.handle(obj, true) },
	})
	if err != nil {
		return fmt.Errorf("add node event handler: %w", err)
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("node informer cache did not sync")
	}
	// Seed min values from the initial sync so the first snapshot isn't 0.
	w.initMinOnce.Do(func() {
		w.updateMinMax(w.total.Load(), w.ready.Load())
	})
	<-ctx.Done()
	return nil
}

func (w *Watcher) handle(obj interface{}, isDelete bool) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		if tomb, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
			if n2, ok3 := tomb.Obj.(*corev1.Node); ok3 {
				node = n2
			}
		}
		if node == nil {
			return
		}
	}
	uid := string(node.UID)
	if uid == "" {
		return
	}
	isReady := isNodeReady(node)

	w.mu.Lock()
	prevReady, had := w.readyByUID[uid]
	if isDelete {
		delete(w.readyByUID, uid)
	} else {
		w.readyByUID[uid] = isReady
	}
	w.mu.Unlock()

	switch {
	case isDelete:
		if had {
			w.total.Add(-1)
			if prevReady {
				w.ready.Add(-1)
			}
		}
	case !had:
		w.total.Add(1)
		if isReady {
			w.ready.Add(1)
		}
	case prevReady != isReady:
		if isReady {
			w.ready.Add(1)
		} else {
			w.ready.Add(-1)
		}
	}
	w.updateMinMax(w.total.Load(), w.ready.Load())
}

func (w *Watcher) updateMinMax(total, ready int64) {
	for {
		cur := w.maxTotal.Load()
		if total <= cur || w.maxTotal.CompareAndSwap(cur, total) {
			break
		}
	}
	for {
		cur := w.maxReady.Load()
		if ready <= cur || w.maxReady.CompareAndSwap(cur, ready) {
			break
		}
	}
	for {
		cur := w.minTotal.Load()
		if cur >= 0 && total >= cur {
			break
		}
		if w.minTotal.CompareAndSwap(cur, total) {
			break
		}
	}
	for {
		cur := w.minReady.Load()
		if cur >= 0 && ready >= cur {
			break
		}
		if w.minReady.CompareAndSwap(cur, ready) {
			break
		}
	}
}

func isNodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

