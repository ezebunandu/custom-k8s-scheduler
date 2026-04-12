# Alphabetical Scheduler Demo

Demonstrates the custom `alphabetical-scheduler` running on a local
[kind](https://kind.sigs.k8s.io/) cluster. The scheduler scores nodes by the
alphabetical order of a label value and binds each pod to the highest-scoring
node.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)

## 1. Create a multi-node cluster

Increase inotify limits first — kind runs systemd inside each node container and
the defaults are too low for a 4-node cluster (see
[Troubleshooting](#troubleshooting) if you skip this step):

```bash
docker run --rm --privileged alpine sysctl -w fs.inotify.max_user_watches=524288
docker run --rm --privileged alpine sysctl -w fs.inotify.max_user_instances=512
```

Then create the cluster:

```bash
kind create cluster --name scheduler-demo --config - <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
EOF
```

## 2. Build and load the scheduler image

```bash
docker build -t alphabetical-scheduler:latest .
kind load docker-image alphabetical-scheduler:latest --name scheduler-demo
```

## 3. Deploy the scheduler

```bash
kubectl apply -f deploy/scheduler.yaml
```

Verify it's running:

```bash
kubectl -n kube-system get pods -l app=alphabetical-scheduler
```

## 4. Label the worker nodes

The scheduler scores nodes by the `topology.kubernetes.io/zone` label (default)
and by the `team` label (for the override demo). Label each worker node with
distinct values so the two deployments land on different nodes:

```bash
WORKERS=($(kubectl get nodes --no-headers -l '!node-role.kubernetes.io/control-plane' -o custom-columns=':metadata.name'))

kubectl label node "${WORKERS[0]}" topology.kubernetes.io/zone=alpha-1 team=zebra  --overwrite
kubectl label node "${WORKERS[1]}" topology.kubernetes.io/zone=beta-2  team=alpha  --overwrite
kubectl label node "${WORKERS[2]}" topology.kubernetes.io/zone=gamma-3 team=mike   --overwrite
```

## 5. Deploy the demo applications

```bash
kubectl apply -f demo/demo-app.yaml
```

## 6. Verify scheduling

Wait a few seconds for the scheduler to place the pods, then check:

```bash
kubectl get pods -o wide
```

**Expected results:**

| Deployment           | Scoring label                 | Worker 0 score | Worker 1 score | Worker 2 score | Target          |
|----------------------|-------------------------------|----------------|----------------|----------------|-----------------|
| `demo-zone-default`  | `topology.kubernetes.io/zone` | 100 (`alpha-1`)| 96 (`beta-2`)  | 92 (`gamma-3`)  | `${WORKERS[0]}` |
| `demo-team-override` | `team`                        | 0 (`zebra`)    | 100 (`alpha`)  | 52 (`mike`)     | `${WORKERS[1]}` |

- `demo-zone-default` pods land on worker 0 because `alpha-1` scores highest
  (100) across all three workers.
- `demo-team-override` pods land on worker 1 because the pod's
  `scheduler.io/rank-by-label: team` overrides the scoring key, and `alpha`
  scores highest (100) for the `team` label.

The two deployments end up on **different nodes** even though they use the same
scheduler — the label override is what makes the difference.

## 7. Check scheduler logs

```bash
kubectl -n kube-system logs -l app=alphabetical-scheduler --tail=50
```

You should see lines like:

```
scheduled default/demo-zone-default-xxx -> scheduler-demo-worker
scheduled default/demo-team-override-xxx -> scheduler-demo-worker2
```

## Troubleshooting

### Cluster creation fails with "could not find a log line that matches"

```
ERROR: failed to create cluster: could not find a log line that matches
"Reached target .*Multi-User System.*|detected cgroup v1"
```

Each kind node is a Docker container running systemd. Systemd and the services
inside it (kubelet, containerd) need inotify watches, and the default OS limits
are too low for a multi-node cluster. The control-plane node exhausts them,
leaving the workers unable to boot.

**Diagnose** — recreate with `--retain` and check a worker's logs:

```bash
kind create cluster --name scheduler-demo --retain --config ...
docker logs scheduler-demo-worker 2>&1 | grep -i inotify
```

If you see `Failed to create inotify object: Too many open files`, the fix is:

```bash
docker run --rm --privileged alpine sysctl -w fs.inotify.max_user_watches=524288
docker run --rm --privileged alpine sysctl -w fs.inotify.max_user_instances=512
```

Then delete and recreate the cluster. These settings reset when Docker Desktop
restarts. See the [kind known-issues docs](https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files)
for how to make them permanent.

## Cleanup

```bash
kubectl delete -f demo/demo-app.yaml
kubectl delete -f deploy/scheduler.yaml
kind delete cluster --name scheduler-demo
```
