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
distinct values:

```bash
WORKERS=($(kubectl get nodes --no-headers -l '!node-role.kubernetes.io/control-plane' -o custom-columns=':metadata.name'))

kubectl label node "${WORKERS[0]}" topology.kubernetes.io/zone=alpha-1 team=zebra --overwrite
kubectl label node "${WORKERS[1]}" topology.kubernetes.io/zone=beta-2  team=alpha --overwrite
kubectl label node "${WORKERS[2]}" topology.kubernetes.io/zone=gamma-3 team=mike  --overwrite
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

| Deployment           | Scoring label                 | Best node label value | Target node      |
|----------------------|-------------------------------|-----------------------|------------------|
| `demo-zone-default`  | `topology.kubernetes.io/zone` | `alpha-1`             | `${WORKERS[0]}`  |
| `demo-team-override` | `team`                        | `alpha`               | `${WORKERS[1]}`  |

- `demo-zone-default` pods should land on the worker labeled `zone=alpha-1`
  because "a" scores highest (100).
- `demo-team-override` pods should land on the worker labeled `team=alpha`
  because the pod's `scheduler.io/rank-by-label: team` overrides the default
  label key, and "alpha" again scores highest.

## 7. Check scheduler logs

```bash
kubectl -n kube-system logs -l app=alphabetical-scheduler --tail=50
```

You should see lines like:

```
scheduled default/demo-zone-default-xxx -> scheduler-demo-worker
scheduled default/demo-team-override-xxx -> scheduler-demo-worker2
```

## Cleanup

```bash
kubectl delete -f demo/demo-app.yaml
kubectl delete -f deploy/scheduler.yaml
kind delete cluster --name scheduler-demo
```
