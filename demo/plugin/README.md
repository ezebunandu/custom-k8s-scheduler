# Plugin Scheduler Demo

Demonstrates the **scheduler framework plugin** approach running on a local
[kind](https://kind.sigs.k8s.io/) cluster. Unlike the standalone polling
scheduler in [`demo/`](../README.md), this version registers the
`AlphabeticalScore` plugin into the real `kube-scheduler` binary. Kubernetes
drives the full scheduling cycle (filter → score → bind) and calls our plugin
during the Score phase.

## How it differs from the polling scheduler

| Aspect | Polling (`cmd/scheduler`) | Plugin (`cmd/plugin-scheduler`) |
|--------|--------------------------|--------------------------------|
| **Scheduling loop** | Custom 2-second poll via `client-go` | Built-in kube-scheduler watch/queue |
| **Node filtering** | None — scores all nodes | Full default filter chain (resources, taints, affinity) |
| **Scoring** | Only `AlphabeticalScore` | Only `AlphabeticalScore` (default score plugins disabled) |
| **Binding** | Direct `Pods().Bind()` call | kube-scheduler binding cycle (Reserve → Permit → Bind) |
| **Binary** | ~15 MB standalone | ~70 MB (full kube-scheduler) |
| **Configuration** | Hardcoded in Go | `KubeSchedulerConfiguration` YAML |

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)

## 1. Create a multi-node cluster

Increase inotify limits first (see [troubleshooting](../README.md#troubleshooting)
if you skip this step):

```bash
docker run --rm --privileged alpine sysctl -w fs.inotify.max_user_watches=524288
docker run --rm --privileged alpine sysctl -w fs.inotify.max_user_instances=512
```

Then create the cluster:

```bash
kind create cluster --name plugin-scheduler-demo --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF
```

## 2. Build and load the plugin scheduler image

```bash
docker build -f Dockerfile.plugin -t alphabetical-plugin-scheduler:latest .
kind load docker-image alphabetical-plugin-scheduler:latest --name plugin-scheduler-demo
```

## 3. Deploy the plugin scheduler

The manifest creates a ServiceAccount, binds it to `system:kube-scheduler` and
`system:volume-scheduler` roles, installs a ConfigMap with the
`KubeSchedulerConfiguration`, and runs the scheduler as a Deployment:

```bash
kubectl apply -f deploy/plugin/scheduler.yaml
```

Verify it's running:

```bash
kubectl -n kube-system get pods -l app=alphabetical-plugin-scheduler
```

## 4. Label the worker nodes

Same as the polling demo — label workers with distinct zone and team values:

```bash
WORKERS=($(kubectl get nodes --no-headers -l '!node-role.kubernetes.io/control-plane' -o custom-columns=':metadata.name'))

kubectl label node "${WORKERS[0]}" topology.kubernetes.io/zone=alpha-1 team=zebra  --overwrite
kubectl label node "${WORKERS[1]}" topology.kubernetes.io/zone=beta-2  team=alpha  --overwrite
kubectl label node "${WORKERS[2]}" topology.kubernetes.io/zone=gamma-3 team=mike   --overwrite
```

## 5. Deploy the demo applications

```bash
kubectl apply -f demo/plugin/demo-app.yaml
```

## 6. Verify scheduling

Wait a few seconds for the scheduler to place the pods, then check:

```bash
kubectl get pods -o wide
```

**Expected results:**

| Deployment                  | Scoring label                 | Worker 0 score | Worker 1 score | Worker 2 score | Target          |
|-----------------------------|-------------------------------|----------------|----------------|----------------|-----------------|
| `plugin-demo-zone-default`  | `topology.kubernetes.io/zone` | 100 (`alpha-1`)| 96 (`beta-2`)  | 92 (`gamma-3`)  | `${WORKERS[0]}` |
| `plugin-demo-team-override` | `team`                        | 0 (`zebra`)    | 100 (`alpha`)  | 52 (`mike`)     | `${WORKERS[1]}` |

The results are identical to the polling demo — same scoring logic, different
execution model.

## 7. Check scheduler logs

```bash
kubectl -n kube-system logs -l app=alphabetical-plugin-scheduler --tail=50
```

With the plugin approach you'll see standard kube-scheduler log output (leader
election, informer sync, scheduling decisions) rather than the simple
`scheduled X -> Y` lines from the polling binary.

## Understanding the KubeSchedulerConfiguration

The scheduler config is embedded in the ConfigMap in `deploy/plugin/scheduler.yaml`:

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false          # single replica, no election needed
profiles:
  - schedulerName: alphabetical-scheduler
    plugins:
      score:
        enabled:
          - name: AlphabeticalScore
            weight: 1
        disabled:
          - name: "*"          # disable all default score plugins
```

Key points:

- **`schedulerName: alphabetical-scheduler`** — pods that set
  `spec.schedulerName: alphabetical-scheduler` are routed to this profile.
- **`disabled: ["*"]`** in the score section turns off all default score plugins
  (NodeResourcesFit, InterPodAffinity, etc.) so scoring is purely alphabetical.
  Default *filter* plugins still run — the scheduler still respects taints,
  resource requests, and node affinity.
- **`leaderElect: false`** — safe for a single-replica demo. Enable for
  production HA setups.

## Cleanup

```bash
kubectl delete -f demo/plugin/demo-app.yaml
kubectl delete -f deploy/plugin/scheduler.yaml
kind delete cluster --name plugin-scheduler-demo
```
