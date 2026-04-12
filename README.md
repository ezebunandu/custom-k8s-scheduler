# Alphabetical Scheduler

A custom Kubernetes scheduler that places pods on nodes by scoring a node label
alphabetically. Nodes whose label value starts with "a" score highest; each
subsequent letter scores lower. The project exists to learn how Kubernetes
scheduling works by building a minimal scheduler from scratch.

## Why this exists

When you run `kubectl apply` on a pod, the default scheduler decides which node
it lands on. That decision passes through a pipeline of **filter**, **score**,
and **bind** steps. This project explores that pipeline by implementing a single
custom scoring rule: *prefer nodes whose label value comes first
alphabetically*.

The scoring formula is intentionally simple so the focus stays on the scheduling
machinery, not the business logic:

| Label value | Score |
|-------------|-------|
| `alpha`     | 100   |
| `bravo`     | 96    |
| `charlie`   | 92    |
| *(missing)* | 0     |

By default the scheduler scores the `topology.kubernetes.io/zone` label. Pods
can override this by setting a `scheduler.io/rank-by-label` label to target a
different node label key.

## Two approaches

The project explores two ways to build a custom scheduler:

1. **Standalone polling binary** (`pkg/schedulercore`) — a simple loop that
   lists unscheduled pods, scores nodes using `client-go`, and binds directly
   via the Kubernetes API. This is what `cmd/scheduler/main.go` runs. It avoids
   the heavy `k8s.io/kubernetes` dependency tree needed for the framework
   approach.

2. **Scheduler framework plugin** (`pkg/alphabeticalscore`) — a proper
   `ScorePlugin` that plugs into the official
   [scheduling framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/)
   extension points. This is the "right" way to extend the default scheduler,
   but comes with significant dependency overhead.

Both implementations share the same scoring logic and label-override behavior.

## Project structure

```
cmd/scheduler/          Entry point — runs the standalone polling scheduler
pkg/
  schedulercore/        Standalone scheduler loop (list → score → bind)
  alphabeticalscore/    Scheduler framework ScorePlugin implementation
test/e2e/               End-to-end tests using kind clusters
deploy/                 Kubernetes manifests to run the scheduler in-cluster
demo/                   Demo deployments and walkthrough
```

## Running the demo

The [`demo/`](demo/README.md) directory contains a full walkthrough for running
the scheduler on a local [kind](https://kind.sigs.k8s.io/) cluster with three
worker nodes. It deploys two sets of test pods that demonstrate both the default
scoring behavior and the label-override feature — the two deployments end up on
different nodes even though they use the same scheduler. See
**[demo/README.md](demo/README.md)** for step-by-step instructions.

## Running tests

Unit tests:

```bash
go test ./pkg/...
```

End-to-end tests (requires Docker and kind):

```bash
go test -tags e2e -timeout 20m ./test/e2e/
```
