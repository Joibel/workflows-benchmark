# workflows-benchmark

A throughput benchmark for the argo workflow-controller + Kubernetes.

It submits many copies of a workflow YAML you supply, maintains a fixed
target count of **in-flight workflows** (`Pending + Running + Succeeded-
awaiting-GC`; `--target-in-flight`, default 1000), and reports the
steady-state completion rate that the controller+k8s combination
sustains. `Failed` and `Errored` workflows are excluded from the gating
count so a wave of failures doesn't starve the submitter. Including
`Succeeded` in the count — until the controller's TTL/podGC actually
deletes them — means if GC can't keep up, the tool stops submitting and
lets the cluster catch its breath.

## Build

```
go build ./cmd/benchmark
```

## Running in-cluster

Run the benchmark as a Job in the cluster it's benchmarking — handy for
high `--target-in-flight` values where a laptop's kube-client RTT would
become the bottleneck.

### 1. Build and push the image to Docker Hub

```
make push IMAGE=YOUR_DOCKERHUB_USER/workflows-benchmark TAG=latest
```

The Dockerfile is multi-stage (builder + distroless/static:nonroot), no
`CGO`, so the final image is small and the binary is static.

### 2. Apply RBAC

```
kubectl apply -f deploy/rbac.yaml
```

Creates a `workflows-benchmark` ServiceAccount in the `argo` namespace
with:
- Namespaced `Role` for `workflows.argoproj.io` (create/list/watch/delete)
- Cluster-scoped `ClusterRole` for `nodes` (list/watch, needed for the node-count reporter)

### 3. Apply the Job

Edit `deploy/job.yaml`:
- Replace `image: DOCKERHUB_USER/workflows-benchmark:latest` with your pushed tag.
- If your Docker Hub repo is private, create an image-pull secret (`kubectl create secret docker-registry dockerhub --docker-username=... --docker-password=... -n argo`) and uncomment the `imagePullSecrets` block on the Job's pod spec.
- The ConfigMap embeds the Workflow YAML; replace it with whatever you want to benchmark.
- Adjust the `args:` block — `--target-in-flight`, `--namespace`, `--service-account`, etc.

```
kubectl apply -f deploy/job.yaml
kubectl logs -n argo -f job/workflows-benchmark
```

When you've seen enough: `kubectl delete -f deploy/job.yaml`. The Job
uses `backoffLimit: 0` so it won't restart on its own.

### Notes

- `--controller-metrics` in the Job uses the in-cluster Service DNS
  `http://workflow-controller-metrics.argo.svc:9090/metrics`. If your
  argo install lacks that Service, uncomment the Service block in
  `deploy/job.yaml` to create one.
- The tool auto-detects in-cluster config (`rest.InClusterConfig()`) so
  no kubeconfig is needed inside the pod.

## Run

```
./benchmark \
  --workflow examples/hello-world.yaml \
  --namespace argo \
  --target-in-flight 1000
```

`--controller-metrics` defaults to `http://localhost:9090/metrics` and
must be reachable at startup (it's probed with retries before
submission begins). Mid-run drops are tolerated and auto-reconnect.

Required: `--workflow`. Everything else defaults.

`Ctrl-C` when you've seen enough — the tool drains in-flight workflows
for up to 30s, then prints a summary (human-readable to stdout; JSON if
you passed `--report path.json`).

### Live output

```
t=00:04:32 submitted=18234 inflight=1000/1000 (backlog=42 running=842 succ-gc=116) done=12341 (+5220/min 30s, +4920/min 5m) fail=12 (0.10%) nodes=5(5r) metrics=ok wf-q=3 ttl-q=8 pod-q=0 op-p99=120ms status=steady
```

- `inflight=N/T` — gating count (Pending+Running+Succeeded-awaiting-GC) vs the target (`--target-in-flight`). When it sits at the target, the submitter is actively refilling. When it drops below, something downstream is slow.
- `backlog` — Pending (not yet reconciled).
- `running` — workflows actively executing.
- `succ-gc` — Succeeded and waiting for TTL/podGC to delete them.
- `metrics=ok|down` — whether the last controller-metrics scrape succeeded.
- `wf-q` / `ttl-q` / `pod-q` — current workflow-controller workqueue depths (`workflow_queue`, `workflow_ttl_queue`, `pod_cleanup_queue`). If any of these climb steadily, the corresponding worker pool is the bottleneck — raise `--workflow-workers`, `--workflow-ttl-workers`, or `--pod-cleanup-workers` on the controller.
- `nodes=N(Rr)` — cluster-wide Node count (N total, R Ready). If it
  drifts during the run (autoscaling, node problems), the final summary
  reports the min/max range observed.

`status` is `warmup` → `ramping` → `steady` (the number you came for) →
possibly `declining` if something breaks downstream.

### Final summary (stdout)

Shows the steady-state rate with a 95% confidence interval, submit/
success/failure counts, a controller-metrics cross-check, and a hint
pointing at the relevant controller tuning knobs.

## What it does to your workflow YAML

- Strips `metadata.name`, sets `metadata.generateName` (preserves yours
  if present, else uses `bench-`).
- Adds label `workflows-benchmark.io/run-id=<run-id>` so the informer
  only watches this run's workflows.
- Injects cleanup defaults **only if missing**:
  - `spec.ttlStrategy.secondsAfterSuccess: 0` (delete successful
    workflows immediately)
  - `spec.ttlStrategy.secondsAfterFailure: 86400` (keep failures a day
    for inspection)
- If `--image-pull-secret` is set and the YAML has no
  `spec.imagePullSecrets`, injects the named secret(s) — e.g.
  `--image-pull-secret=test` or `--image-pull-secret=test,other`.
- If `--service-account` is set and the YAML has no
  `spec.serviceAccountName`, injects it — e.g. `--service-account=argo`.
  Argo needs a ServiceAccount allowed to create `workflowtaskresults`;
  the `default` SA typically isn't, which manifests as every wait
  container failing with a "forbidden" error.

If your YAML already sets `ttlStrategy`, `imagePullSecrets`, or
`serviceAccountName`, those are respected.

## Controller metrics

If `--controller-metrics` is set, the tool scrapes `argo_workflows_*`
metrics from the workflow-controller every 10s and displays queue depth,
operation p99 latency, and a cross-check of our informer-observed
success count vs the controller's `argo_workflows_total_count{phase="Succeeded"}`.

To expose the metrics port with kubectl:

```
kubectl -n argo port-forward deploy/workflow-controller 9090:9090
```

## Tuning

If the tool reports `status=steady` with a lower number than you expect,
relevant knobs on the workflow-controller:

- `--workflow-workers` (default 32)
- `--pod-cleanup-workers` (default 4)
- `--workflow-ttl-workers`
- `--qps` / `--burst` for the k8s client
- `PARALLELISM_LIMIT` env var

Raise one at a time and rerun the benchmark.

## Design notes

See the plan document for the rationale: load shape = maintain backlog
of ~500 unstarted workflows (Pending / pre-reconcile), submission path =
direct k8s API via kubeconfig (no argo-server in the measurement),
cleanup = inject-if-missing with respect for user values, steady-state
detection = 2-minute rolling rel-stddev check, exit = Ctrl-C with drain.
