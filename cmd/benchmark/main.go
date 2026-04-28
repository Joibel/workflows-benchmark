// workflows-benchmark saturates argo-workflows with a user-supplied
// Workflow YAML, maintains a target backlog of unstarted workflows, and
// reports the steady-state completion rate that the controller + k8s
// sustain. See /home/alan/.claude/plans/we-re-going-to-develop-merry-sloth.md
// for the full design rationale.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	versioned "github.com/argoproj/argo-workflows/v3/pkg/client/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/alan/workflows-benchmark/internal/loader"
	"github.com/alan/workflows-benchmark/internal/metrics"
	"github.com/alan/workflows-benchmark/internal/nodes"
	"github.com/alan/workflows-benchmark/internal/report"
	"github.com/alan/workflows-benchmark/internal/stats"
	"github.com/alan/workflows-benchmark/internal/submitter"
	"github.com/alan/workflows-benchmark/internal/watcher"
)

func main() {
	if err := run(); err != nil {
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		workflowPath  = flag.String("workflow", "", "path to user Workflow YAML (required)")
		namespace     = flag.String("namespace", "argo", "namespace to submit workflows into")
		targetInFlight = flag.Int("target-in-flight", 1000, "target count of in-flight workflows (Pending+Running+Succeeded-awaiting-GC); Failed/Errored are NOT counted so failures don't starve the submitter")
		kubeconfig    = flag.String("kubeconfig", defaultKubeconfig(), "path to kubeconfig; ignored if in-cluster")
		ctrlMetrics   = flag.String("controller-metrics", "http://localhost:9090/metrics", "workflow-controller /metrics URL; must be reachable at startup")
		runID         = flag.String("run-id", "", "unique id for this run; generated if empty")
		reportPath    = flag.String("report", "", "write final summary JSON to this path")
		logInterval   = flag.Duration("log-interval", 5*time.Second, "live stats interval")
		workers       = flag.Int("workers", 16, "submit worker goroutines")
		qps           = flag.Float64("qps", 200, "kube client QPS (client-side rate limit)")
		burst         = flag.Int("burst", 400, "kube client burst")
		pullSecrets   = flag.String("image-pull-secret", "", "comma-separated list of secret names to inject as spec.imagePullSecrets when the workflow YAML has none (e.g. \"test\"); respected if the YAML already sets imagePullSecrets")
		serviceAccount = flag.String("service-account", "", "inject spec.serviceAccountName when the workflow YAML leaves it blank (e.g. \"argo\"); respected if already set. Argo needs a SA allowed to create workflowtaskresults — the default \"default\" SA usually isn't")
	)
	flag.Parse()

	if *workflowPath == "" {
		flag.Usage()
		return errors.New("--workflow is required")
	}
	if *ctrlMetrics == "" {
		flag.Usage()
		return errors.New("--controller-metrics is required (pass an empty-forbidden URL; default is http://localhost:9090/metrics)")
	}
	if *runID == "" {
		*runID = generateRunID()
	}

	yamlBytes, err := os.ReadFile(*workflowPath)
	if err != nil {
		return fmt.Errorf("read workflow yaml: %w", err)
	}
	tpl, err := loader.Load(yamlBytes, *runID, loader.Options{
		ImagePullSecrets:   splitCSV(*pullSecrets),
		ServiceAccountName: *serviceAccount,
	})
	if err != nil {
		return fmt.Errorf("load workflow: %w", err)
	}
	log.Printf("run-id=%s namespace=%s target-in-flight=%d", *runID, *namespace, *targetInFlight)
	if tpl.Injected.TTLStrategy {
		log.Printf("injected ttlStrategy: secondsAfterSuccess=0, secondsAfterFailure=86400")
	}
	if tpl.Injected.ImagePullSecrets {
		log.Printf("injected imagePullSecrets=%s", *pullSecrets)
	}
	if tpl.Injected.ServiceAccountName {
		log.Printf("injected serviceAccountName=%s", *serviceAccount)
	}

	cfg, err := buildRestConfig(*kubeconfig)
	if err != nil {
		return fmt.Errorf("build kube config: %w", err)
	}
	cfg.QPS = float32(*qps)
	cfg.Burst = *burst
	client, err := versioned.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build argo clientset: %w", err)
	}
	kclient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build kubernetes clientset: %w", err)
	}

	sigCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Watcher context is independent of the signal so the informer keeps
	// receiving Delete events while we flush workflows on shutdown.
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()

	scraper := metrics.New(*ctrlMetrics)
	if err := probeMetrics(sigCtx, scraper, *ctrlMetrics); err != nil {
		return err
	}

	start := time.Now()
	st := stats.New(start)

	wch := watcher.New(client, *namespace, *runID)
	// Bridge the informer's terminal channel into stats.
	go func() {
		for phase := range wch.Terminal {
			if phase == "Succeeded" {
				st.IncCompletion()
			} else {
				st.IncFailure()
			}
		}
	}()

	// Start informer first so backlog reads are meaningful before we
	// start submitting.
	watcherErrCh := make(chan error, 1)
	go func() {
		watcherErrCh <- wch.Run(watcherCtx)
	}()

	nw := nodes.New(kclient)
	go func() {
		if err := nw.Run(sigCtx); err != nil {
			log.Printf("node watcher: %v", err)
		}
	}()

	sub := submitter.New(client, tpl, wch, submitter.Config{
		Namespace:      *namespace,
		TargetInFlight: *targetInFlight,
		Workers:        *workers,
	})
	submitterDone := make(chan struct{})
	go func() {
		sub.Run(sigCtx)
		close(submitterDone)
	}()

	live := &report.Live{
		Interval:       *logInterval,
		Stats:          st,
		Watcher:        wch,
		Submitter:      sub,
		Scraper:        scraper,
		Nodes:          nw,
		TargetInFlight: *targetInFlight,
	}
	go live.Run(sigCtx)

	<-sigCtx.Done()
	log.Printf("shutdown requested: stopping submitter, deleting workflows, draining for up to 5m")
	// Wait for submitter goroutines to wind down so we don't race with
	// late Create() calls outliving the DeleteCollection.
	select {
	case <-submitterDone:
	case <-time.After(10 * time.Second):
		log.Printf("submitter still running after 10s; proceeding with delete")
	}

	// Issue a server-side delete of every workflow tagged with this
	// run-id. Background propagation lets the API call return as soon as
	// the workflows are marked for deletion; the controller GCs pods.
	delCtx, delCancel := context.WithTimeout(context.Background(), 60*time.Second)
	policy := metav1.DeletePropagationBackground
	delErr := client.ArgoprojV1alpha1().Workflows(*namespace).DeleteCollection(
		delCtx,
		metav1.DeleteOptions{PropagationPolicy: &policy},
		metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", loader.RunIDLabel, *runID)},
	)
	delCancel()
	if delErr != nil {
		log.Printf("delete-collection: %v", delErr)
	} else {
		log.Printf("delete-collection: issued for run-id=%s", *runID)
	}

	// Drain: poll the watcher's tracked counts until everything is gone
	// or the 5-minute deadline elapses. Each tick logs progress so the
	// user sees the count going down.
	drainDeadline := time.Now().Add(5 * time.Minute)
	drainTick := time.NewTicker(5 * time.Second)
	for {
		snap := wch.Snapshot()
		total := snap.Unknown + snap.Pending + snap.Running + snap.Succeeded + snap.Failed + snap.Errored
		if total == 0 {
			log.Printf("drain: complete")
			break
		}
		remaining := time.Until(drainDeadline)
		if remaining <= 0 {
			log.Printf("drain: timeout, %d workflows still tracked", total)
			break
		}
		log.Printf("drain: %d workflows remaining (in-flight=%d), %s left", total, snap.InFlight(), remaining.Round(time.Second))
		select {
		case <-drainTick.C:
		case <-time.After(remaining):
		}
	}
	drainTick.Stop()

	// Stop the watcher and wait for it to exit so the Terminal channel
	// closes and any tail events are counted.
	watcherCancel()
	select {
	case <-watcherErrCh:
	case <-time.After(5 * time.Second):
	}

	end := time.Now()
	var lastMetric *metrics.Snapshot
	if *ctrlMetrics != "" {
		snapCtx, snapCancel := context.WithTimeout(context.Background(), 5*time.Second)
		lastMetric = scraper.Scrape(snapCtx)
		snapCancel()
	}
	inj := map[string]bool{
		"ttlStrategy":        tpl.Injected.TTLStrategy,
		"imagePullSecrets":   tpl.Injected.ImagePullSecrets,
		"serviceAccountName": tpl.Injected.ServiceAccountName,
	}
	fs := report.BuildSummary(*runID, *namespace, start, end, *targetInFlight, sub, wch, st, lastMetric, nw, inj)
	if err := fs.Write(os.Stdout, *reportPath); err != nil {
		log.Printf("write summary: %v", err)
	}
	return nil
}

func defaultKubeconfig() string {
	if p := os.Getenv("KUBECONFIG"); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kube", "config")
	}
	return ""
}

func buildRestConfig(kubeconfig string) (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

// probeMetrics verifies the controller's /metrics endpoint is reachable
// before we start submitting. We retry a handful of times to tolerate a
// port-forward that is still coming up, then abort. Once the run starts,
// transient failures are handled inside the Live reporter with
// reconnect logging — here we only care about startup reachability.
func probeMetrics(ctx context.Context, s *metrics.Scraper, url string) error {
	const attempts = 5
	backoff := time.Second
	for i := 1; i <= attempts; i++ {
		pCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		snap := s.Scrape(pCtx)
		cancel()
		if !snap.Unavailable {
			log.Printf("metrics: connected to %s", url)
			return nil
		}
		log.Printf("metrics: probe %d/%d failed: %s", i, attempts, snap.UnavailableErr)
		if i == attempts {
			return fmt.Errorf("metrics endpoint %s unreachable after %d attempts: %s", url, attempts, snap.UnavailableErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
	return nil
}

// splitCSV splits "a,b, c" into ["a","b","c"], trimming whitespace and
// dropping empty entries. Returns nil for an empty input.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func generateRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "r-" + hex.EncodeToString(b[:])
}
