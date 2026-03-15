# Kubernetes Scheduler Framework - Research

**Researched:** 2026-03-08
**Domain:** Kubernetes Scheduler Framework (`pkg/scheduler/framework`)
**Confidence:** HIGH (sourced from official Kubernetes docs, pkg.go.dev, and kubernetes/kubernetes source)

---

## Kubernetes Scheduler Framework

### Overview

The Kubernetes scheduling framework is a pluggable architecture introduced in Kubernetes v1.15 and stabilized in v1.19. Every scheduling decision passes through a defined sequence of extension points. Custom logic is implemented as **plugins** that register at one or more of those extension points. The scheduler is compiled as a single binary; there is no dynamic plugin loading — custom schedulers are built by forking `cmd/kube-scheduler` or embedding the framework via `sigs.k8s.io/scheduler-plugins`.

---

### 1. Scheduling Cycle vs Binding Cycle

A single attempt to place a pod is called a **scheduling context**. It consists of two phases:

#### Scheduling Cycle
- **Purpose:** Select a node for the pod.
- **Execution:** Serial — only one scheduling cycle runs at a time.
- **Outcome:** Determines which node (if any) is feasible and has the highest score.
- **Extension points that run here:** `QueueSort` → `PreEnqueue` → `PreFilter` → `Filter` → `PostFilter` (only if no feasible nodes) → `PreScore` → `Score` → `NormalizeScore` → `Reserve` → `Permit`

#### Binding Cycle
- **Purpose:** Commit the scheduling decision to the cluster (update the pod's `spec.nodeName` via the API server).
- **Execution:** Concurrent — multiple binding cycles may run in parallel for different pods.
- **Outcome:** Pod is bound to the node (or fails and triggers `Unreserve`).
- **Extension points that run here:** `PreBind` → `Bind` → `PostBind`

Either cycle can be aborted. If aborted, the pod is returned to the scheduling queue and `Unreserve` is called on all plugins that previously ran `Reserve`.

**Execution flow diagram:**

```
[Pod enters queue]
        |
   PreEnqueue  ← gate before entering activeQ
        |
  QueueSort    ← orders pods in activeQ
        |
──── SCHEDULING CYCLE (serial) ────
        |
   PreFilter   ← pre-process pod, early reject, gather shared state
        |
    Filter     ← per-node feasibility (runs concurrently across nodes)
        |
  PostFilter   ← only if 0 feasible nodes (preemption lives here)
        |
   PreScore    ← pre-score work, shared state for Score
        |
    Score      ← per-node score (runs concurrently across nodes)
        |
 NormalizeScore← normalize each plugin's scores to [0, 100]
        |
   Reserve     ← optimistic claim of resources (stateful)
        |
   Permit      ← final gate: Approve / Deny / Wait
        |
──── BINDING CYCLE (concurrent) ────
        |
   PreBind     ← e.g., provision storage
        |
     Bind      ← write pod.spec.nodeName to API server
        |
   PostBind    ← cleanup/notification after success
```

---

### 2. Extension Points — Detailed Reference

All extension point interfaces are defined in `k8s.io/kubernetes/pkg/scheduler/framework` (package path: `pkg/scheduler/framework`).

#### 2.1 PreEnqueue

```go
type PreEnqueuePlugin interface {
    Plugin
    PreEnqueue(ctx context.Context, pod *v1.Pod) *Status
}
```

- Runs **before** a pod enters `activeQ`.
- All PreEnqueue plugins must return `Success`; failure places the pod in the internal unschedulable list (no `Unschedulable` condition is set on the pod object).
- Plugins that implement `PreEnqueue` must also implement `EnqueueExtensions`.

#### 2.2 QueueSort

```go
type QueueSortPlugin interface {
    Plugin
    Less(podInfo1, podInfo2 *QueuedPodInfo) bool
}

// LessFunc is the function signature used by QueueSort plugins.
type LessFunc func(podInfo1, podInfo2 *QueuedPodInfo) bool
```

- Determines the ordering of pods in the scheduling queue.
- **Exactly one** QueueSort plugin may be enabled at a time. All profiles in a multi-profile setup must use the same QueueSort plugin with the same parameters.
- Default plugin: `PrioritySort` (sorts by `pod.Spec.Priority`, then by timestamp).

#### 2.3 PreFilter

```go
type PreFilterPlugin interface {
    Plugin
    PreFilter(ctx context.Context, state CycleState, pod *v1.Pod) (*PreFilterResult, *Status)
}

type PreFilterResult struct {
    // NodeNames restricts the set of nodes Filter runs against.
    // nil means all nodes. Empty set means no nodes (pod is unschedulable).
    NodeNames sets.Set[string]
}
```

- Runs once per scheduling cycle, serially, before any Filter calls.
- Use to: pre-compute information shared across Filter invocations; store the result in `CycleState`; or restrict which nodes are evaluated.
- Returning an error status aborts the scheduling cycle.
- Plugins implementing `PreFilter` must also implement `EnqueueExtensions`.

#### 2.4 Filter

```go
type FilterPlugin interface {
    Plugin
    Filter(ctx context.Context, state CycleState, pod *v1.Pod, nodeInfo NodeInfo) *Status
}
```

- Called **once per node** for each enabled Filter plugin.
- Nodes are evaluated concurrently (across goroutines); plugin order per node is the configured order.
- If any plugin returns `Unschedulable` or `UnschedulableAndUnresolvable` for a node, that node is eliminated and remaining plugins are skipped for it.
- Plugins implementing `Filter` must also implement `EnqueueExtensions`.

#### 2.5 PostFilter

```go
type PostFilterPlugin interface {
    Plugin
    PostFilter(
        ctx context.Context,
        state CycleState,
        pod *v1.Pod,
        filteredNodeStatusMap NodeToStatusReader,
    ) (*PostFilterResult, *Status)
}

type PostFilterResult struct {
    NominatedNodeName string
}
```

- Called **only when no feasible nodes were found** by Filter.
- Plugins run in configured order. If any plugin marks a node `Schedulable`, remaining PostFilter plugins are skipped.
- Primary use: **preemption** — evict lower-priority pods to make room. The default plugin is `DefaultPreemption`.
- On success, returns a `PostFilterResult` with the nominated node name.

#### 2.6 PreScore

```go
type PreScorePlugin interface {
    Plugin
    PreScore(ctx context.Context, state CycleState, pod *v1.Pod, nodes []NodeInfo) *Status
}
```

- Runs once, serially, with the list of feasible nodes before Score runs.
- Use to: perform expensive shared computations needed by Score (store result in `CycleState`).
- Returning an error status aborts the scheduling cycle.

#### 2.7 Score

```go
type ScorePlugin interface {
    Plugin
    Score(ctx context.Context, state CycleState, pod *v1.Pod, nodeName string) (int64, *Status)
    // ScoreExtensions returns nil if the plugin does not support NormalizeScore.
    ScoreExtensions() ScoreExtensions
}

type ScoreExtensions interface {
    NormalizeScore(ctx context.Context, state CycleState, pod *v1.Pod, scores NodeScoreList) *Status
}

type NodeScore struct {
    Name  string
    Score int64
}
type NodeScoreList []NodeScore
```

- Called **once per feasible node** for each Score plugin. Runs concurrently across nodes.
- Must return a score in `[MinNodeScore, MaxNodeScore]` = `[0, 100]` (after normalization).
- If the plugin needs to normalize, implement `ScoreExtensions()` returning non-nil.

#### 2.8 NormalizeScore

Exposed through `ScoreExtensions` (see above). Called once per Score plugin per scheduling cycle with all raw scores from that plugin. Must bring scores into `[0, 100]`.

#### 2.9 Reserve

```go
type ReservePlugin interface {
    Plugin
    Reserve(ctx context.Context, state CycleState, pod *v1.Pod, nodeName string) *Status
    Unreserve(ctx context.Context, state CycleState, pod *v1.Pod, nodeName string)
}
```

- Two methods on one interface.
- `Reserve` is called before the binding cycle to let stateful plugins optimistically claim resources (e.g., volume binding). All plugins' `Reserve` must succeed; any failure skips the rest and rolls back via `Unreserve`.
- `Unreserve` is called on **all** Reserve plugins in **reverse order** whenever Reserve or any later phase fails. Must be idempotent and must never fail.

#### 2.10 Permit

```go
type PermitPlugin interface {
    Plugin
    Permit(ctx context.Context, state CycleState, pod *v1.Pod, nodeName string) (*Status, time.Duration)
}
```

- Final gate at the **end of the scheduling cycle**, before the binding cycle starts.
- Returns `(status, timeout)`. Three valid outcomes:
  1. **Approve** (`Success`, any duration): pod proceeds to binding immediately.
  2. **Deny** (`Unschedulable`): pod returned to queue; `Unreserve` called.
  3. **Wait** (`Wait`, timeout duration): pod held in an internal "waiting pods" map. The binding cycle starts but blocks on `WaitOnPermit`. If the timeout expires, status becomes Deny.
- Waiting pods can be approved/rejected by other plugins via `Handle.AllowWaitingPod` / `Handle.RejectWaitingPod`.
- Use case: gang scheduling (wait until all pods of a group are ready).

#### 2.11 PreBind

```go
type PreBindPlugin interface {
    Plugin
    PreBind(ctx context.Context, state CycleState, pod *v1.Pod, nodeName string) *Status
}
```

- Runs in the binding cycle before actual bind.
- Use for: provisioning persistent volumes, setting up network prerequisites.
- Failure aborts the binding cycle and triggers `Unreserve`.

#### 2.12 Bind

```go
type BindPlugin interface {
    Plugin
    Bind(ctx context.Context, state CycleState, pod *v1.Pod, nodeName string) *Status
}
```

- Performs the actual pod-to-node binding (typically a `POST` to the API server's bind subresource).
- **At least one** Bind plugin must be enabled. Plugins are tried in order; once one returns `Success`, the rest are skipped.
- Default plugin: `DefaultBinder` (calls `client.CoreV1().Pods(ns).Bind(...)`).

#### 2.13 PostBind

```go
type PostBindPlugin interface {
    Plugin
    PostBind(ctx context.Context, state CycleState, pod *v1.Pod, nodeName string)
}
```

- Informational only. Called after a successful bind.
- No error return — failures here are logged but do not affect the scheduling result.

#### 2.14 EnqueueExtensions

```go
type EnqueueExtensions interface {
    // EventsToRegister returns a list of cluster events and a QueueingHintFn
    // that indicate which cluster events could make unschedulable pods schedulable.
    EventsToRegister(ctx context.Context, pod *v1.Pod) ([]ClusterEventWithHint, *Status)
}
```

- Any plugin implementing `PreEnqueue`, `PreFilter`, `Filter`, `Reserve`, or `Permit` **must** implement `EnqueueExtensions`.
- Tells the scheduler which cluster events (e.g., "Node added", "Pod deleted") might make a previously-rejected pod schedulable.
- From v1.32: each event registration can include a `QueueingHintFn` for finer-grained control.

#### 2.15 QueueingHint (v1.32+, stable in v1.34)

```go
// QueueingHintFn is called when a cluster event occurs.
// Returns Queue if the event might make the pod schedulable, QueueSkip otherwise.
type QueueingHintFn func(
    logger klog.Logger,
    pod *v1.Pod,
    oldObj, newObj interface{},
) (QueueingHint, error)

type QueueingHint int

const (
    QueueSkip QueueingHint = iota // Do not requeue
    Queue                          // Requeue to activeQ or backoffQ
)

type ClusterEventWithHint struct {
    Event          ClusterEvent
    QueueingHintFn QueueingHintFn // nil means always requeue
}
```

- Fine-grained companion to `EnqueueExtensions`. Instead of requeuing a pod for *every* event of a registered type, the hint function evaluates the specific event and returns `Queue` only when that event plausibly resolves the pod's scheduling failure.
- Significantly reduces unnecessary scheduling retries in large clusters.

---

### 3. Plugin Interface and Registration

#### Base Plugin Interface

```go
// All plugins implement this.
type Plugin interface {
    Name() string
}
```

#### Plugin Factory (Runtime Package)

Located at `k8s.io/kubernetes/pkg/scheduler/framework/runtime`:

```go
// Standard factory signature.
type PluginFactory = func(
    ctx context.Context,
    configuration runtime.Object,
    f fwk.Handle,
) (fwk.Plugin, error)

// Generic factory that also receives feature gate state (v1.23+).
type PluginFactoryWithFts[T fwk.Plugin] func(
    context.Context,
    runtime.Object,
    fwk.Handle,
    plfeature.Features,
) (T, error)

// Bridges PluginFactoryWithFts to PluginFactory.
func FactoryAdapter[T fwk.Plugin](fts plfeature.Features, withFts PluginFactoryWithFts[T]) PluginFactory
```

#### Registry

```go
// A Registry maps plugin name → factory function.
type Registry map[string]PluginFactory

func (r Registry) Register(name string, factory PluginFactory) error
func (r Registry) Unregister(name string) error
func (r Registry) Merge(in Registry) error
```

#### Registration Pattern (Out-of-Tree Plugin)

```go
// main.go for a custom scheduler binary:
import (
    "k8s.io/kubernetes/cmd/kube-scheduler/app"
    "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
    "example.com/my-scheduler/pkg/myplugin"
)

func main() {
    command := app.NewSchedulerCommand(
        app.WithPlugin(myplugin.Name, myplugin.New),
    )
    if err := command.Execute(); err != nil {
        os.Exit(1)
    }
}

// Plugin constructor (implements PluginFactory signature):
func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
    args, ok := obj.(*MyPluginArgs)
    if !ok {
        return nil, fmt.Errorf("unexpected args type %T", obj)
    }
    return &MyPlugin{handle: h, args: args}, nil
}
```

Plugin args structs must follow the naming convention `<PluginName>Args` to be decoded correctly by the framework. They must be registered with the scheme at `k8s.io/kubernetes/pkg/scheduler/apis/config/scheme`.

---

### 4. How the Scheduler Selects Nodes

The scheduler selects a node through a two-phase algorithm during the scheduling cycle:

#### Phase 1: Filtering (Finding Feasible Nodes)

1. The scheduler takes a **snapshot** of `NodeInfo` objects at the start of each scheduling cycle. This snapshot is immutable until the Permit extension point completes.
2. `PreFilter` plugins run once to gather shared state and optionally restrict the candidate node set.
3. `Filter` plugins run against each candidate node concurrently. Nodes failing any Filter plugin are removed from consideration.
4. **Short-circuit via `percentageOfNodesToScore`:** The scheduler iterates nodes in a round-robin fashion (continuing from where it stopped for the previous pod, interleaved across zones). It stops evaluating nodes once it has `max(percentageOfNodesToScore% of total nodes, minFeasibleNodesToFind)` feasible nodes. This trades placement optimality for scheduling throughput at large scale.
   - Default: scales from 50% (100-node cluster) down to ~5% (5000+ node cluster); floor is always 5%.
   - `minFeasibleNodesToFind` = 100 nodes (hardcoded). Clusters with fewer than 100 total nodes are always fully evaluated.

#### Phase 2: Scoring (Ranking Feasible Nodes)

1. `PreScore` runs once with the feasible node list — shared computation for Score plugins.
2. `Score` plugins run against each feasible node concurrently. Each plugin returns a raw score.
3. `NormalizeScore` (via `ScoreExtensions`) brings each plugin's scores into `[0, 100]`.
4. Each plugin's normalized score is multiplied by its configured **weight**. The weighted scores for all plugins are summed to produce the final score for each node.

#### Phase 3: Selection

1. The node with the highest final score is selected.
2. **Tie-breaking:** If two or more nodes have equal final scores, the scheduler picks one **at random** (uniform random selection).
3. The selected node name is recorded in the scheduling context; `Reserve` plugins are called next.

#### Preemption (PostFilter fallback)

If Phase 1 yields zero feasible nodes, `PostFilter` plugins are called. The default `DefaultPreemption` plugin:
1. Searches for lower-priority pods whose eviction would make the node feasible.
2. Nominates the target node (sets `pod.Status.NominatedNodeName`).
3. Does **not** immediately evict — eviction happens in a subsequent scheduling cycle.

---

### 5. Key Data Structures

All types below are in `k8s.io/kubernetes/pkg/scheduler/framework` unless noted otherwise.

#### 5.1 CycleState

```go
type CycleState struct {
    // Internal: sync.Map (thread-safe, optimized for write-once/read-many)
}

// Lifecycle
func NewCycleState() *CycleState

// State access (used by plugins to share data within a single scheduling cycle)
func (c *CycleState) Read(key StateKey) (StateData, error)
func (c *CycleState) Write(key StateKey, val StateData)
func (c *CycleState) Delete(key StateKey)
func (c *CycleState) Clone() CycleState

// Plugin skip hints (v1.34+)
func (c *CycleState) GetSkipFilterPlugins() sets.Set[string]
func (c *CycleState) SetSkipFilterPlugins(plugins sets.Set[string])
func (c *CycleState) GetSkipScorePlugins() sets.Set[string]
func (c *CycleState) SetSkipScorePlugins(plugins sets.Set[string])
func (c *CycleState) GetSkipPreBindPlugins() sets.Set[string]
func (c *CycleState) SetSkipPreBindPlugins(plugins sets.Set[string])

// Metrics
func (c *CycleState) ShouldRecordPluginMetrics() bool
func (c *CycleState) SetRecordPluginMetrics(flag bool)
```

`StateKey` is `type StateKey string`. `StateData` is:

```go
type StateData interface {
    Clone() StateData
}
```

`CycleState` is **not shared** between scheduling cycles. A new instance is created for each pod. The built-in key for activating pods from within a plugin is:

```go
var PodsToActivateKey StateKey = "kubernetes.io/pods-to-activate"
```

#### 5.2 NodeInfo

```go
type NodeInfo struct {
    Pods                         []PodInfo
    PodsWithAffinity             []PodInfo
    PodsWithRequiredAntiAffinity []PodInfo
    UsedPorts                    HostPortInfo
    Requested                    *Resource       // sum of all pod resource requests
    NonZeroRequested             *Resource       // sum with minimum 1m CPU / 4Mi Mem for zero requests
    Allocatable                  *Resource       // node.Status.Allocatable
    ImageStates                  map[string]*ImageStateSummary
    PVCRefCounts                 map[string]int
    Generation                   int64           // monotonic generation counter
    DeclaredFeatures             ndf.FeatureSet
}

func NewNodeInfo(pods ...*v1.Pod) *NodeInfo
func (n *NodeInfo) Node() *v1.Node
func (n *NodeInfo) SetNode(node *v1.Node)
func (n *NodeInfo) AddPod(pod *v1.Pod)
func (n *NodeInfo) AddPodInfo(podInfo PodInfo)
func (n *NodeInfo) RemovePod(logger klog.Logger, pod *v1.Pod) error
func (n *NodeInfo) Snapshot() NodeInfo
func (n *NodeInfo) String() string
```

`Resource` struct:

```go
type Resource struct {
    MilliCPU         int64
    Memory           int64
    EphemeralStorage int64
    AllowedPodNumber int
    ScalarResources  map[v1.ResourceName]int64
}
```

#### 5.3 PodInfo

```go
type PodInfo struct {
    Pod                        *v1.Pod
    RequiredAffinityTerms      []AffinityTerm
    RequiredAntiAffinityTerms  []AffinityTerm
    PreferredAffinityTerms     []WeightedAffinityTerm
    PreferredAntiAffinityTerms []WeightedAffinityTerm
}

func NewPodInfo(pod *v1.Pod) (*PodInfo, error)
func (pi *PodInfo) Update(pod *v1.Pod) error
func (pi *PodInfo) DeepCopy() *PodInfo
func (pi *PodInfo) CalculateResource() PodResource
```

`PodInfo` pre-computes and caches affinity terms to avoid repeated parsing of pod specs during Filter/Score.

#### 5.4 QueuedPodInfo

Wraps `PodInfo` with scheduling queue metadata:

```go
type QueuedPodInfo struct {
    *PodInfo
    Timestamp                 time.Time
    Attempts                  int
    BackoffExpiration         time.Time
    UnschedulableCount        int
    ConsecutiveErrorsCount    int
    InitialAttemptTimestamp   *time.Time
    UnschedulablePlugins      sets.Set[string]  // which plugins rejected this pod
    PendingPlugins            sets.Set[string]  // plugins waiting on events
    GatingPlugin              string
    GatingPluginEvents        []ClusterEvent
}

func (pqi *QueuedPodInfo) Gated() bool
```

#### 5.5 FrameworkHandle

The `Handle` interface (embedded in `Framework`) is the primary API given to plugins at instantiation time:

```go
// Handle is passed to plugin factories; plugins keep a reference to it.
type Handle interface {
    // Cluster state access
    SnapshotSharedLister() SharedLister          // node/pod snapshot for current cycle
    ClientSet() clientset.Interface              // full Kubernetes API client
    KubeConfig() *restclient.Config              // raw REST config
    EventRecorder() events.EventRecorder
    SharedInformerFactory() informers.SharedInformerFactory

    // Waiting pod management (used by Permit plugins)
    IterateOverWaitingPods(callback func(WaitingPod))
    GetWaitingPod(uid types.UID) WaitingPod
    RejectWaitingPod(uid types.UID) bool
    AllowWaitingPod(uid types.UID) bool

    // Parallelism
    Parallelizer() parallelize.Parallelizer
}

// SharedLister gives access to NodeInfo snapshots.
type SharedLister interface {
    NodeInfos() NodeInfoLister
    StorageInfos() StorageInfoLister
}

type NodeInfoLister interface {
    List() ([]NodeInfo, error)
    HavePodsWithAffinityList() ([]NodeInfo, error)
    HavePodsWithRequiredAntiAffinityList() ([]NodeInfo, error)
    Get(nodeName string) (NodeInfo, error)
}
```

`Handle` is the read-only, plugin-lifetime interface. The mutable `Framework` interface (which embeds `Handle`) is used internally by the scheduler itself.

#### 5.6 Status Type

```go
type Status struct {
    code    Code
    reasons []string
    err     error
    plugin  string
}

type Code int

const (
    Success                      Code = iota
    Error                              // internal error; pod retried
    Unschedulable                      // pod cannot be scheduled; pod retried
    UnschedulableAndUnresolvable       // pod cannot be scheduled ever; pod NOT retried
    Wait                               // Permit: hold in waiting pods map
    Skip                               // plugin skips this extension point
    Pending                            // waiting for async event
)

// Constructors
func NewStatus(code Code, reasons ...string) *Status
func AsStatus(err error) *Status

// Predicates
func (s *Status) IsSuccess() bool
func (s *Status) IsUnschedulable() bool
func (s *Status) IsUnschedulableAndUnresolvable() bool
func (s *Status) IsWait() bool
func (s *Status) IsSkip() bool
func (s *Status) IsError() bool
func (s *Status) AsError() error
func (s *Status) Code() Code
func (s *Status) Reasons() []string
func (s *Status) Plugin() string
```

`nil` status is equivalent to `Success`.

---

### 6. The Scheduler's Internal Queue

Located at `k8s.io/kubernetes/pkg/scheduler/internal/queue`. The concrete type is `PriorityQueue`, which implements the `SchedulingQueue` interface.

#### 6.1 Three Sub-Queues

| Queue | Type | Contents | When a pod moves here |
|---|---|---|---|
| `activeQ` | heap (ordered by QueueSort `Less`) | Pods ready to be scheduled | New pod added; pod becomes eligible after backoff/unschedulable timeout; cluster event triggers requeue |
| `backoffQ` | heap (ordered by backoff expiration) | Pods in exponential backoff after a failed attempt | Pod moved from `unschedulablePods` and backoff not yet expired |
| `unschedulablePods` | map[UID]*QueuedPodInfo | Pods that were attempted and found unschedulable | `AddUnschedulableIfNotPresent` called after scheduling failure |

#### 6.2 Backoff Timing

```
podInitialBackoffSeconds: 1s  (default)
podMaxBackoffSeconds:     10s (default)
```

Backoff duration = `min(initialBackoff * 2^(attempts-1), maxBackoff)`.

Pods in `unschedulablePods` are forcibly moved to `backoffQ` or `activeQ` after `podMaxInUnschedulablePodsDuration` (default **5 minutes**) to prevent starvation.

#### 6.3 SchedulingQueue Interface

```go
type SchedulingQueue interface {
    framework.PodNominator
    Add(logger klog.Logger, pod *v1.Pod) error
    Activate(logger klog.Logger, pods map[string]*v1.Pod)
    AddUnschedulableIfNotPresent(logger klog.Logger, pod *framework.QueuedPodInfo, podSchedulingCycle int64) error
    SchedulingCycle() int64
    Pop(logger klog.Logger) (*framework.QueuedPodInfo, error)
    Done(types.UID)
    Update(logger klog.Logger, oldPod, newPod *v1.Pod) error
    Delete(pod *v1.Pod) error
    MoveAllToActiveOrBackoffQueue(logger klog.Logger, event framework.ClusterEvent, oldObj, newObj interface{}, preCheck PreEnqueueCheck)
    AssignedPodAdded(logger klog.Logger, pod *v1.Pod)
    AssignedPodUpdated(logger klog.Logger, oldPod, newPod *v1.Pod, event framework.ClusterEvent)
    PendingPods() ([]*v1.Pod, string)
    PodsInActiveQ() []*v1.Pod
    Close()
    Run(logger klog.Logger)
}
```

Key behaviors:
- `Pop` blocks until `activeQ` is non-empty. Increments `SchedulingCycle()` counter each call.
- `Done` must be called for every pod returned by `Pop` — used to track in-flight pods.
- `MoveAllToActiveOrBackoffQueue` is triggered by cluster event handlers (node added, pod deleted, etc.) to wake unschedulable pods.
- `Activate` is called from within plugins via `PodsToActivate` state stored in `CycleState`. Plugins write to `CycleState` with key `PodsToActivateKey`; the framework reads it and calls `Activate`.

#### 6.4 QueueingHint Integration (v1.32+)

Before v1.32, any cluster event matching a plugin's registered event types triggered requeue of all unschedulable pods associated with that plugin — even if the specific event could not possibly resolve their failure. From v1.32, each plugin can register a `QueueingHintFn` alongside each event type. The queue calls the hint function with the old and new objects; only if the hint returns `Queue` is the pod moved back to `activeQ` or `backoffQ`.

```go
type QueueingHintFunction struct {
    PluginName     string
    QueueingHintFn framework.QueueingHintFn
}

type QueueingHintMap map[framework.ClusterEvent][]*QueueingHintFunction
```

#### 6.5 Scheduling Cycle Counter

```go
func (p *PriorityQueue) SchedulingCycle() int64
```

Incremented each time `Pop` is called. Passed to `AddUnschedulableIfNotPresent` to detect stale put-back requests (if the cycle has already advanced, the pod goes to `backoffQ` instead of `unschedulablePods`).

---

### 7. Scheduler Profiles

Profiles allow a **single scheduler binary** to behave as multiple named schedulers. They share one `PriorityQueue` but run independent plugin pipelines.

#### 7.1 Configuration API

```yaml
apiVersion: kubescheduler.config.k8s.io/v1   # stable since k8s v1.25
kind: KubeSchedulerConfiguration
parallelism: 16                               # goroutines for concurrent node evaluation
percentageOfNodesToScore: 0                   # 0 = auto-calculate
podInitialBackoffSeconds: 1
podMaxBackoffSeconds: 10
profiles:
  - schedulerName: default-scheduler
    plugins:
      score:
        enabled:
          - name: NodeResourcesFit
            weight: 1
          - name: InterPodAffinity
            weight: 2
        disabled:
          - name: PodTopologySpread
      multiPoint:         # enables plugin at ALL applicable extension points
        enabled:
          - name: MyGlobalPlugin
    pluginConfig:
      - name: NodeResourcesFit
        args:
          apiVersion: kubescheduler.config.k8s.io/v1
          kind: NodeResourcesFitArgs
          scoringStrategy:
            type: LeastAllocated  # or MostAllocated, RequestedToCapacityRatio
            resources:
              - name: cpu
                weight: 1
              - name: memory
                weight: 1

  - schedulerName: no-scoring-scheduler
    plugins:
      preScore:
        disabled:
          - name: '*'     # disable all default preScore plugins
      score:
        disabled:
          - name: '*'     # disable all default score plugins
```

#### 7.2 KubeSchedulerProfile Type

```go
// kubescheduler.config.k8s.io/v1
type KubeSchedulerProfile struct {
    SchedulerName            string
    PercentageOfNodesToScore *int32
    Plugins                  *Plugins
    PluginConfig             []PluginConfig
}

type Plugins struct {
    PreEnqueue PluginSet
    QueueSort  PluginSet
    PreFilter  PluginSet
    Filter     PluginSet
    PostFilter PluginSet
    PreScore   PluginSet
    Score      PluginSet
    Reserve    PluginSet
    Permit     PluginSet
    PreBind    PluginSet
    Bind       PluginSet
    PostBind   PluginSet
    MultiPoint PluginSet   // enables plugin at all applicable extension points
}

type PluginSet struct {
    Enabled  []Plugin
    Disabled []Plugin
}

type Plugin struct {
    Name   string
    Weight int32  // only meaningful for Score plugins
}

type PluginConfig struct {
    Name string
    Args runtime.RawExtension
}
```

#### 7.3 Profile Constraints

- All profiles **must** use the same `QueueSort` plugin and the same configuration for it (because there is only one `activeQ`).
- A profile named `default-scheduler` is required (it is the default assigned by kube-apiserver to pods without an explicit `spec.schedulerName`).
- Pods select a profile via `pod.spec.schedulerName`. The scheduler looks up the profile by that name and runs its plugin pipeline.
- Each profile gets its own `Framework` instance (its own plugin instances, lifecycle hooks, etc.).
- Scheduling events use `reportingController` = `schedulerName` of the profile that made the decision.

#### 7.4 MultiPoint Extension Point

`MultiPoint` is a configuration-only extension point. Listing a plugin under `multiPoint.enabled` is equivalent to listing it under every applicable extension point interface it implements. It is useful for plugins that span many extension points (like `NodeResourcesFit`) so you don't have to list them individually.

```yaml
plugins:
  multiPoint:
    enabled:
      - name: NodeResourcesFit
      - name: PodTopologySpread
```

---

### 8. Default Plugins Reference

| Plugin | Extension Points | Notes |
|---|---|---|
| `PrioritySort` | queueSort | Default; sorts by priority then timestamp |
| `NodeName` | filter | Exact node name match |
| `NodePorts` | preFilter, filter | Port conflict detection |
| `NodeUnschedulable` | filter | Respects `node.Spec.Unschedulable` |
| `NodeResourcesFit` | preFilter, filter, score | Resource requests vs allocatable |
| `NodeResourcesBalancedAllocation` | score | Balances CPU/memory usage |
| `NodeAffinity` | filter, score | `nodeSelector` and `nodeAffinity` |
| `TaintToleration` | filter, preScore, score | Taints and tolerations |
| `InterPodAffinity` | preFilter, filter, preScore, score | Pod affinity/anti-affinity |
| `PodTopologySpread` | preFilter, filter, preScore, score | `topologySpreadConstraints` |
| `VolumeBinding` | preFilter, filter, reserve, preBind, score | PVC binding, storage capacity scoring |
| `VolumeRestrictions` | preFilter, filter | Volume provider restrictions |
| `VolumeZone` | filter | Zone requirements for volumes |
| `NodeVolumeLimits` | filter | CSI volume limits |
| `EBSLimits` | filter | AWS EBS limits |
| `GCEPDLimits` | filter | GCP PD limits |
| `AzureDiskLimits` | filter | Azure disk limits |
| `ImageLocality` | score | Prefers nodes with image cached |
| `DefaultPreemption` | postFilter | Preemption; nominates nodes |
| `DefaultBinder` | bind | Calls API server bind subresource |

---

### 9. Package Layout

```
k8s.io/kubernetes/pkg/scheduler/
├── framework/
│   ├── interface.go          # Plugin, Handle, all extension point interfaces, Status, CycleState
│   ├── types.go              # NodeInfo, PodInfo, QueuedPodInfo, Resource, NodeScore
│   └── runtime/
│       ├── framework.go      # frameworkImpl (concrete Framework)
│       └── registry.go       # Registry, PluginFactory, FactoryAdapter
├── internal/
│   ├── cache/                # NodeInfo cache; snapshot logic
│   └── queue/
│       └── scheduling_queue.go  # PriorityQueue, SchedulingQueue interface
├── plugins/                  # All in-tree plugins (one package per plugin)
│   ├── noderesources/
│   ├── nodeaffinity/
│   ├── interpodaffinity/
│   └── ...
├── apis/config/
│   └── v1/                   # KubeSchedulerConfiguration, KubeSchedulerProfile, etc.
├── scheduler.go              # Top-level Scheduler struct, Run loop
└── schedule_one.go           # scheduleOne(): the per-pod scheduling entry point
```

For out-of-tree plugins, the canonical repository is [`sigs.k8s.io/scheduler-plugins`](https://github.com/kubernetes-sigs/scheduler-plugins), which provides production-ready examples (Capacity Scheduling, Coscheduling, NetworkAware) and the `app.WithPlugin` helper for embedding custom plugins into the scheduler binary.

---

### 10. Pitfalls and Non-Obvious Behaviors

1. **`Unreserve` must be idempotent.** It is called on all Reserve plugins in reverse order whenever any phase after Reserve fails. If your plugin only called `Reserve` successfully, its `Unreserve` may still be invoked. Design accordingly.

2. **`nil` Status == Success.** Every extension point that returns `*Status` treats `nil` as `Success`. Returning a non-nil `*Status` with code `Success` is also valid, but `nil` is the zero-value convention.

3. **`UnschedulableAndUnresolvable` suppresses retries.** Use `Unschedulable` for transient failures (pod will be retried when cluster state changes). Use `UnschedulableAndUnresolvable` only when the pod can never be scheduled regardless of cluster state (e.g., references a non-existent resource class). The pod goes to `unschedulablePods` but `EnqueueExtensions` will never re-trigger it.

4. **Snapshot immutability.** The `NodeInfo` snapshot taken at the start of a scheduling cycle is immutable through `Permit`. Do not attempt to mutate it via the `Handle` — use `ClientSet()` for writes, but understand that those writes are not reflected in the current cycle's snapshot.

5. **Single QueueSort constraint.** All profiles share `activeQ`. Adding a second profile with a different QueueSort plugin is rejected at startup with a validation error.

6. **`PreFilterResult.NodeNames` is not additive.** If multiple PreFilter plugins return non-nil `NodeNames`, the framework takes their **intersection**. Returning an empty `NodeNames` set marks the pod as immediately unschedulable.

7. **Score range enforcement.** Raw scores from `Score` that are outside `[0, 100]` after `NormalizeScore` will cause a panic. If a plugin does not implement `ScoreExtensions`, its raw scores must already be within range.

8. **Permit `Wait` blocks the binding goroutine, not the scheduling cycle.** The scheduling cycle completes (Reserve has run), and a new goroutine is started for binding. That goroutine blocks at `WaitOnPermit`. The scheduling cycle can advance to the next pod immediately.

9. **Plugin args type naming.** Plugin configuration structs must be named `<PluginName>Args` and registered with the scheduler's API scheme. Mismatched names cause silent decoding failures (args silently ignored).

10. **`EnqueueExtensions` is mandatory for Filter plugins.** If a Filter plugin does not implement `EnqueueExtensions`, pods that it rejects will never be requeued when relevant cluster events occur — effectively leaving them stuck in `unschedulablePods` until the 5-minute forced flush.

---

### Sources

| Source | Content | Trust |
|---|---|---|
| [Scheduling Framework — kubernetes.io](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/) | Extension points, cycles, plugin API overview | HIGH |
| [Scheduler Configuration — kubernetes.io](https://kubernetes.io/docs/reference/scheduling/config/) | KubeSchedulerConfiguration, profiles, default plugins | HIGH |
| [kube-scheduler Config v1 API — kubernetes.io](https://kubernetes.io/docs/reference/config-api/kube-scheduler-config.v1/) | Full config type reference | HIGH |
| [Scheduler Performance Tuning — kubernetes.io](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduler-perf-tuning/) | percentageOfNodesToScore, node iteration | HIGH |
| [pkg/scheduler/framework — pkg.go.dev](https://pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/framework) | All interface and struct definitions | HIGH |
| [pkg/scheduler/internal/queue — pkg.go.dev](https://pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/internal/queue) | PriorityQueue, SchedulingQueue interface | HIGH |
| [pkg/scheduler/framework/runtime — pkg.go.dev](https://pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/framework/runtime) | Registry, PluginFactory, FactoryAdapter | HIGH |
| [QueueingHint blog post — kubernetes.io (Dec 2024)](https://kubernetes.io/blog/2024/12/12/scheduler-queueinghint/) | QueueingHint mechanism, v1.32 changes | HIGH |
| [KEP-624 scheduling-framework — github.com/kubernetes/enhancements](https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/624-scheduling-framework/README.md) | Original design, FrameworkHandle rationale | HIGH |
| [kubernetes-sigs/scheduler-plugins](https://github.com/kubernetes-sigs/scheduler-plugins) | Out-of-tree plugin patterns | MEDIUM |

---

## Custom Scheduler Implementation Options

**Researched:** 2026-03-08
**Confidence:** HIGH (official k8s docs + pkg.go.dev), MEDIUM (community tutorials cross-verified against multiple sources)

This section documents four implementation strategies for a custom Kubernetes scheduler in Go, with particular focus on a label-based node selection learning exercise.

---

### Option A: Scheduler Extender (HTTP Webhook)

#### How It Works

The scheduler extender is a separate HTTP/HTTPS service. You configure the default `kube-scheduler` with an `extenders:` block in `KubeSchedulerConfiguration`, pointing it at your service's base URL. During scheduling, kube-scheduler calls out to your service via HTTP POST at specific phases, passing JSON-encoded `ExtenderArgs` and expecting typed JSON responses.

Calling flow:
1. kube-scheduler runs all built-in filter plugins on the full node list.
2. It POSTs the surviving candidate nodes to your extender's `filterVerb` endpoint.
3. kube-scheduler runs all built-in score plugins.
4. It POSTs surviving nodes to your extender's `prioritizeVerb` endpoint; scores are combined.
5. Binding proceeds normally (or via your `bindVerb` if set).

#### KubeSchedulerConfiguration Registration

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
extenders:
  - urlPrefix: "http://my-extender.kube-system.svc.cluster.local:8888"
    filterVerb: "filter"
    prioritizeVerb: "prioritize"
    preemptVerb: "preempt"
    bindVerb: ""            # leave empty to use default binder
    weight: 5
    enableHTTPS: false
    nodeCacheCapable: false # if true, extender receives node names only (not full NodeList)
    managedResources: []    # custom extended resources this extender handles
    ignorable: false        # if true, scheduler continues if extender is unreachable
```

#### Go Types (from `k8s.io/kube-scheduler/extender/v1`)

```go
// Request body for both /filter and /prioritize
type ExtenderArgs struct {
    Pod       *v1.Pod      // pod being scheduled
    Nodes     *v1.NodeList // candidate nodes (nodeCacheCapable == false)
    NodeNames *[]string    // candidate node names (nodeCacheCapable == true)
}

// Response from /filter
type ExtenderFilterResult struct {
    Nodes                      *v1.NodeList   // nodes that passed your filter
    NodeNames                  *[]string      // node names (nodeCacheCapable mode)
    FailedNodes                FailedNodesMap // map[nodeName]reason string
    FailedAndUnresolvableNodes FailedNodesMap // nodes where preemption won't help
    Error                      string
}

// Response from /prioritize
type HostPriorityList []HostPriority

type HostPriority struct {
    Host  string // node name
    Score int64  // convention: 0–10 (MinExtenderPriority=0, MaxExtenderPriority=10)
}

// Request/response from /preempt
type ExtenderPreemptionArgs struct {
    Pod                   *v1.Pod
    NodeNameToVictims     map[string]*Victims      // nodeCacheCapable == false
    NodeNameToMetaVictims map[string]*MetaVictims  // nodeCacheCapable == true
}

type ExtenderPreemptionResult struct {
    NodeNameToMetaVictims map[string]*MetaVictims
}
```

#### Minimal Go HTTP Server Skeleton

```go
package main

import (
    "encoding/json"
    "net/http"

    v1 "k8s.io/api/core/v1"
    extenderv1 "k8s.io/kube-scheduler/extender/v1"
)

func filterHandler(w http.ResponseWriter, r *http.Request) {
    var args extenderv1.ExtenderArgs
    if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    result := extenderv1.ExtenderFilterResult{}
    var passed []v1.Node
    failed := extenderv1.FailedNodesMap{}

    // Example: pod annotation drives which node label is required
    requiredLabelKey := args.Pod.Annotations["scheduler.example.com/required-node-label"]
    for _, node := range args.Nodes.Items {
        if _, ok := node.Labels[requiredLabelKey]; ok {
            passed = append(passed, node)
        } else {
            failed[node.Name] = "missing required label: " + requiredLabelKey
        }
    }

    result.Nodes = &v1.NodeList{Items: passed}
    result.FailedNodes = failed
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(result)
}

func main() {
    http.HandleFunc("/filter", filterHandler)
    http.ListenAndServe(":8888", nil)
}
```

#### Pros

- Language-agnostic: the extender can be written in any language with an HTTP server.
- No recompilation of `kube-scheduler` is required; deploy the extender as a sidecar or separate pod.
- Easily tested with `curl`; logs are standard HTTP access logs.
- Low initial setup overhead — works with an unmodified kube-scheduler.
- Good bridge option when the scheduling logic already lives in a non-Go service.

#### Cons

- **Performance**: every scheduling decision incurs an HTTP round-trip plus JSON marshal/unmarshal on both sides. Under benchmark, a predicate extender can consume up to ~50% of the total scheduling algorithm time (source: SoByte/Alibaba analysis). Unacceptable for high-pod-churn clusters.
- **Limited extension points**: only `Filter`, `Prioritize`, `Preempt`, and `Bind`. No access to `Reserve`, `Permit`, `PreBind`, `PostBind`, or `QueueSort`.
- **No cache sharing**: the extender does not receive or share the scheduler's in-memory NodeInfo snapshot. Node data is passed over the wire or must be re-fetched.
- **No abort notification**: if a later plugin (or the extender itself) causes the scheduling cycle to abort, the extender is never told. Cleanup is your problem.
- No `CycleState` — extenders cannot share computed data with other plugins within the same scheduling cycle.

#### When to Use

- Rapid prototyping without touching the scheduler binary.
- When the scheduling logic must live outside Go (e.g., a Python ML scoring service).
- When you do not control the cluster's scheduler binary (e.g., managed Kubernetes).
- Small clusters with low scheduling throughput requirements.
- **Not recommended** for production clusters with significant scheduling load.

---

### Option B: Out-of-Tree Scheduler Plugin (kubernetes-sigs/scheduler-plugins)

#### What It Is

[`kubernetes-sigs/scheduler-plugins`](https://github.com/kubernetes-sigs/scheduler-plugins) is the canonical upstream repository for out-of-tree scheduler plugins. It wraps the upstream `kube-scheduler` code, compiles custom plugins directly into the scheduler binary, and is maintained by the SIG Scheduling team. Your plugin has zero serialization overhead and full access to the scheduling framework.

The pattern: your plugin implements one or more framework extension-point interfaces, and is registered into the scheduler via `app.WithPlugin()`. The result is a single binary that includes all default kube-scheduler behavior plus your additions.

#### Module and Version Compatibility

```
Go module:  sigs.k8s.io/scheduler-plugins
Latest:     v0.33.5  (published 2025-10-27, built against k8s v1.33.5)

Version scheme: scheduler-plugins vX.Y.Z is built with k8s vX.Y.Z dependencies
  v0.33.5  →  k8s v1.33.5
  v0.32.7  →  k8s v1.32.7
  v0.31.8  →  k8s v1.31.8

Pre-built container images:
  registry.k8s.io/scheduler-plugins/kube-scheduler:v0.33.5
  registry.k8s.io/scheduler-plugins/controller:v0.33.5
  (architectures: linux/amd64, linux/arm64, linux/s390x, linux/ppc64le)
```

Always pin `sigs.k8s.io/scheduler-plugins` to the version matching your cluster's Kubernetes minor version.

#### Repo Structure

```
kubernetes-sigs/scheduler-plugins/
├── cmd/
│   └── scheduler/
│       └── main.go          # entry point: NewSchedulerCommand + all WithPlugin calls
├── pkg/
│   ├── coscheduling/        # gang/co-scheduling
│   ├── capacityscheduling/  # hierarchical capacity management
│   ├── trimaran/            # load-aware scheduling (CPU/memory utilization)
│   ├── noderesourcetopology/# NUMA-aware, resource topology
│   ├── networkaware/        # network bandwidth-aware
│   └── ...                  # one directory per plugin
├── apis/
│   └── config/              # plugin arg types; must be named <PluginName>Args
├── doc/
│   ├── develop.md           # how to add a new plugin to the repo
│   └── install.md           # Helm / manual deployment
├── manifests/               # Helm chart, ClusterRole, Deployment YAMLs
└── test/
    └── e2e/                 # end-to-end tests per plugin
```

#### Plugin Interfaces (from `k8s.io/kubernetes/pkg/scheduler/framework`)

```go
// Every plugin implements the base Plugin interface
type Plugin interface {
    Name() string
}

// Standard factory function signature
type PluginFactory = func(ctx context.Context, args runtime.Object, f Handle) (Plugin, error)

// Filter extension point
type FilterPlugin interface {
    Plugin
    Filter(ctx context.Context, state *CycleState, pod *v1.Pod, nodeInfo *NodeInfo) *Status
}

// Score extension point
type ScorePlugin interface {
    Plugin
    Score(ctx context.Context, state *CycleState, pod *v1.Pod, nodeName string) (int64, *Status)
    ScoreExtensions() ScoreExtensions // return nil if no NormalizeScore needed
}

// PreFilter extension point (runs once per pod, before parallel Filter calls)
type PreFilterPlugin interface {
    Plugin
    PreFilter(ctx context.Context, state *CycleState, pod *v1.Pod) (*PreFilterResult, *Status)
    PreFilterExtensions() PreFilterExtensions
}
```

`nil` is always a valid `*Status` return value and means Success.

#### Writing a Label-Based Filter Plugin

```go
package nodelabelplugin

import (
    "context"
    "fmt"

    v1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/kubernetes/pkg/scheduler/framework"
)

const Name = "NodeLabelPlugin"

// Annotation key on the pod that declares which node label key is required
const PodRequiredLabelAnnotation = "scheduler.example.com/required-label"

type NodeLabelPlugin struct {
    handle framework.Handle
}

// Compile-time assertion that NodeLabelPlugin implements FilterPlugin
var _ framework.FilterPlugin = &NodeLabelPlugin{}

func (p *NodeLabelPlugin) Name() string { return Name }

func (p *NodeLabelPlugin) Filter(
    ctx context.Context,
    state *framework.CycleState,
    pod *v1.Pod,
    nodeInfo *framework.NodeInfo,
) *framework.Status {
    requiredLabel, ok := pod.Annotations[PodRequiredLabelAnnotation]
    if !ok {
        return nil // no requirement — pass all nodes
    }

    node := nodeInfo.Node()
    if node == nil {
        return framework.AsStatus(fmt.Errorf("nodeInfo has no node"))
    }

    if _, hasLabel := node.Labels[requiredLabel]; hasLabel {
        return nil // node has the label — pass
    }
    return framework.NewStatus(
        framework.Unschedulable,
        fmt.Sprintf("node %q does not have required label key %q", node.Name, requiredLabel),
    )
}

// New is the PluginFactory
func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
    return &NodeLabelPlugin{handle: h}, nil
}
```

#### main.go Entry Point

```go
package main

import (
    "os"

    "k8s.io/kubernetes/cmd/kube-scheduler/app"
    "github.com/yourorg/yourscheduler/pkg/nodelabelplugin"
)

func main() {
    command := app.NewSchedulerCommand(
        app.WithPlugin(nodelabelplugin.Name, nodelabelplugin.New),
        // add more app.WithPlugin(...) calls here for additional plugins
    )
    if err := command.Execute(); err != nil {
        os.Exit(1)
    }
}
```

This produces a complete `kube-scheduler` binary with all default plugins plus yours.

#### KubeSchedulerConfiguration to Enable the Plugin

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
leaderElection:
  leaderElect: false
clientConnection:
  kubeconfig: "/etc/kubernetes/scheduler.conf"
profiles:
  - schedulerName: my-custom-scheduler
    plugins:
      filter:
        enabled:
          - name: "NodeLabelPlugin"
      # Alternatively use multiPoint to enable at all applicable extension points:
      # multiPoint:
      #   enabled:
      #     - name: "NodeLabelPlugin"
```

Pods opt in via `spec.schedulerName: my-custom-scheduler`.

#### Building the Binary

```bash
# Using the scheduler-plugins repo as a base:
git clone https://github.com/kubernetes-sigs/scheduler-plugins.git
cd scheduler-plugins
make build    # produces bin/kube-scheduler

# Or in your own module that imports the framework:
go build -o bin/my-scheduler ./cmd/scheduler/
```

#### Deployment

Deploy as a Kubernetes `Deployment` in `kube-system`. Mount a `ConfigMap` containing `KubeSchedulerConfiguration` YAML. The pod needs a `ServiceAccount` with RBAC to:
- `list`, `watch` `pods`, `nodes`
- `create` `pods/binding`, `pods/status`
- `create`, `patch` `events`
- `get`, `list`, `watch` `persistentvolumeclaims`, `storageclasses` (if using volume plugins)

The custom scheduler runs **alongside** the default scheduler; each handles pods that name it in `spec.schedulerName`.

#### Pros

- **Best performance**: plugins compile directly into the binary — zero HTTP or serialization overhead.
- **Full 11 extension points**: QueueSort through PostBind, including Reserve, Permit, PreBind.
- **Shares scheduler cache**: `framework.Handle.SnapshotSharedLister()` gives read access to the scheduler's consistent NodeInfo snapshot.
- **CycleState**: plugins share computed data within a scheduling cycle — avoid re-computing in Filter and Score.
- **EnqueueExtensions**: properly handle cluster event notification so rejected pods are re-queued when relevant state changes.
- Production-ready reference examples in the scheduler-plugins repo to copy from.
- Recommended approach by the Kubernetes SIG Scheduling team.

#### Cons

- Must be written in Go.
- Binary must be recompiled and re-deployed on every Kubernetes minor version upgrade.
- `k8s.io/kubernetes` vendoring requires `replace` directives in `go.mod` (see Option D).
- Slightly more scaffolding than a simple HTTP server to get started.
- Filter plugins that reject pods **must** implement `EnqueueExtensions` or rejected pods may be permanently stuck (see Pitfalls below).

#### When to Use

- **The recommended approach for any new production scheduling extension.**
- When you need access to extension points beyond Filter/Prioritize (Reserve, Permit, PreBind, etc.).
- When performance matters — high pod churn or large clusters.
- When you want to share the scheduler's cache and avoid re-fetching node/pod data.
- Learning the standard, community-supported plugin development model.

---

### Option C: Custom Scheduler from Scratch (client-go)

#### How It Works

You write an independent scheduler process using `k8s.io/client-go` only (no dependency on `k8s.io/kubernetes`). The process:
1. Watches for pods with `spec.nodeName == ""` and `spec.schedulerName == "my-scheduler"`.
2. Implements its own filter and scoring logic (list nodes, check labels, compute score).
3. Creates a `v1.Binding` object via the Kubernetes API to assign the pod to a chosen node.
4. Emits a Kubernetes Event to record the decision.

The scheduler runs as a separate `Deployment`; pods opt in via `spec.schedulerName`.

#### Core Imports

```go
import (
    "context"
    "time"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
    "k8s.io/client-go/tools/clientcmd"
)
```

No dependency on `k8s.io/kubernetes` — just `k8s.io/client-go`, `k8s.io/api`, and `k8s.io/apimachinery`.

#### Watching Unscheduled Pods (Poll-Based)

```go
func (s *Scheduler) getUnscheduledPods(ctx context.Context) ([]corev1.Pod, error) {
    podList, err := s.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
        FieldSelector: "spec.nodeName=",
    })
    if err != nil {
        return nil, err
    }
    var result []corev1.Pod
    for _, pod := range podList.Items {
        if pod.Spec.SchedulerName == "my-scheduler" {
            result = append(result, pod)
        }
    }
    return result, nil
}
```

A Watch-based approach using `client-go/tools/cache` Informers is more efficient in production (avoids polling the API server every N seconds), but adds complexity. For a learning exercise, polling is fine.

#### Binding a Pod to a Node

```go
func (s *Scheduler) bind(ctx context.Context, pod *corev1.Pod, nodeName string) error {
    binding := &corev1.Binding{
        ObjectMeta: metav1.ObjectMeta{
            Name:      pod.Name,
            Namespace: pod.Namespace,
        },
        Target: corev1.ObjectReference{
            APIVersion: "v1",
            Kind:       "Node",
            Name:       nodeName,
        },
    }
    return s.clientset.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
}
```

The API server atomically sets `pod.Spec.NodeName`. If another scheduler already bound the pod, the call returns a conflict error — handle it and move on (do not retry).

#### Core Scheduling Loop

```go
func (s *Scheduler) Run(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        pods, err := s.getUnscheduledPods(ctx)
        if err != nil {
            time.Sleep(5 * time.Second)
            continue
        }

        for i := range pods {
            nodeName, err := s.selectNode(ctx, &pods[i])
            if err != nil {
                // log and skip; pod will be retried next iteration
                continue
            }
            if err := s.bind(ctx, &pods[i], nodeName); err != nil {
                // may be a race (already bound) — log and continue
                continue
            }
        }
        time.Sleep(5 * time.Second)
    }
}
```

#### What You Must Implement Yourself

Everything the default scheduler provides for free becomes your responsibility:

| Feature | Default Scheduler Provides | From-Scratch Must Build |
|---|---|---|
| Node cache / snapshot | Consistent NodeInfo snapshot shared across plugins | Re-list nodes from API each cycle |
| Resource fitting | `NodeResourcesFit` plugin | Must check `node.Status.Allocatable` vs pod `requests` |
| Taint toleration | `TaintToleration` filter plugin | Must parse `node.Spec.Taints` and `pod.Spec.Tolerations` |
| Node affinity | `NodeAffinity` filter plugin | Must parse `pod.Spec.Affinity.NodeAffinity` |
| Pod affinity/anti-affinity | `InterPodAffinity` plugin | Complex — skip or implement partially |
| Unschedulable backoff queue | Exponential backoff, 5-min flush | Manual retry tracking |
| Preemption | `DefaultPreemption` PostFilter plugin | Skip or implement from scratch |
| Leader election | Built-in `--leader-elect` flag | Add `k8s.io/client-go/tools/leaderelection` |
| Event recording | Framework emits scheduling events automatically | Call `EventRecorder` manually |
| Race-safe binding | Framework ensures correct order | Must handle conflict errors from Bind() |

#### Race Conditions

Multiple schedulers watching the same pod pool will race. Two schedulers may each select the same pod and both attempt to bind it. The API server uses optimistic concurrency: the second Bind call fails with a "already exists" or conflict error. Your code must detect this and not retry that pod (it is already scheduled).

#### Pros

- Minimal Go dependencies — only `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery`.
- No complex vendoring (`replace` directives, k8s internal packages).
- Conceptually simple to start: ~150–300 lines of Go.
- Teaches you exactly what a scheduler does at the API level.
- Full freedom to implement any logic without framework constraints.
- Ideal for learning: you must understand what you're skipping when you use the framework.

#### Cons

- **Re-implement everything**: resource fitting, taints/tolerations, affinity, backoff, leader election.
- **Missing features by default**: pods using taints, node affinity, or resource constraints will be incorrectly scheduled unless you explicitly implement those checks.
- **Performance**: polling is inefficient; naive node listing re-fetches all node data every cycle.
- **No shared cache**: data is stale unless you build a full informer cache yourself.
- **Race conditions**: requires careful error handling for concurrent binding conflicts.
- High maintenance burden as cluster complexity grows.

#### When to Use

- **Learning exercise**: the best approach for understanding what the scheduler actually does under the hood.
- Minimal, single-purpose schedulers with very simple logic (e.g., "assign all pods to node X").
- When avoiding `k8s.io/kubernetes` vendoring complexity is a priority.
- **Not recommended for production** except for the narrowest, well-tested use cases.

---

### Option D: Fork/Embed kube-scheduler (NewSchedulerCommand Pattern)

#### What It Is

This is the production-standard pattern for adding custom plugins to the kube-scheduler without maintaining a source fork. You write a `main.go` that imports the upstream scheduler's `app` package and passes your plugins to `app.NewSchedulerCommand` via `app.WithPlugin`. The upstream scheduler code is consumed as a regular Go module dependency — you never copy-paste the scheduler source.

This is exactly the pattern that `kubernetes-sigs/scheduler-plugins` uses. Options B and D are two flavors of the same technique:
- **Option B** = use the scheduler-plugins repo as your starting point (import `sigs.k8s.io/scheduler-plugins`).
- **Option D** = write your own module from scratch, importing `k8s.io/kubernetes` directly.

#### The Entry Point

```go
// From k8s.io/kubernetes/cmd/kube-scheduler/app:
func NewSchedulerCommand(opts ...Option) *cobra.Command

// WithPlugin creates an Option that registers one plugin by name + factory:
func WithPlugin(name string, factory runtime.PluginFactory) Option
```

#### Full main.go

```go
package main

import (
    "os"

    "k8s.io/kubernetes/cmd/kube-scheduler/app"

    // your plugins
    "github.com/yourorg/yourscheduler/pkg/nodelabelplugin"
    "github.com/yourorg/yourscheduler/pkg/anotherplugin"
)

func main() {
    command := app.NewSchedulerCommand(
        app.WithPlugin(nodelabelplugin.Name, nodelabelplugin.New),
        app.WithPlugin(anotherplugin.Name, anotherplugin.New),
    )
    if err := command.Execute(); err != nil {
        os.Exit(1)
    }
}
```

The resulting binary is a complete `kube-scheduler` with all default plugins pre-registered, plus your custom plugins added on top.

#### What NewSchedulerCommand Provides for Free

- All default filter and score plugins pre-registered (NodeAffinity, TaintToleration, NodeResourcesFit, InterPodAffinity, PodTopologySpread, etc.).
- Leader election via `--leader-elect` flag.
- `--config` flag accepting `KubeSchedulerConfiguration` YAML.
- `/healthz` and `/readyz` HTTP endpoints.
- pprof profiling endpoint.
- Standard `klog` logging flags.
- Scheduling queue with backoff and 5-minute flush.
- Concurrent node evaluation (parallelism configurable).
- Metrics via Prometheus.

#### The Vendoring Challenge

`k8s.io/kubernetes` does not follow standard Go module semantics. Its sub-packages (`k8s.io/api`, `k8s.io/apimachinery`, etc.) are separate published modules, but the monorepo uses `replace` directives to pin them to specific commits. Without those `replace` directives in your `go.mod`, `go build` fails.

The correct approach is to copy the `replace` block from `kubernetes-sigs/scheduler-plugins`'s `go.mod` for the k8s version you are targeting. An abbreviated example:

```
module github.com/yourorg/yourscheduler

go 1.23

require (
    k8s.io/api v0.33.5
    k8s.io/apimachinery v0.33.5
    k8s.io/client-go v0.33.5
    k8s.io/component-base v0.33.5
    k8s.io/kubernetes v1.33.5
    sigs.k8s.io/scheduler-plugins v0.33.5  // optional; include if using any of its plugins
)

// Required: k8s.io/kubernetes does not publish sub-packages as independent modules
replace (
    k8s.io/api => k8s.io/api v0.33.5
    k8s.io/apimachinery => k8s.io/apimachinery v0.33.5
    k8s.io/client-go => k8s.io/client-go v0.33.5
    k8s.io/component-base => k8s.io/component-base v0.33.5
    // ... all other k8s.io/* sub-packages must be listed
)
```

Copy the full `replace` block from the matching `kubernetes-sigs/scheduler-plugins` release branch — do not attempt to construct it manually.

#### Relation to Option B

| | Option B | Option D |
|---|---|---|
| Starting point | Fork/import `sigs.k8s.io/scheduler-plugins` | Your own module |
| Who maintains examples | SIG Scheduling team | You |
| Pre-built images | Available (`registry.k8s.io/scheduler-plugins/...`) | Build your own |
| Production plugins included | Yes (coscheduling, trimaran, etc.) | No (only what you add) |
| Use when | You want existing plugins or a proven scaffold | Writing a net-new standalone plugin module |

Both use `app.NewSchedulerCommand` + `app.WithPlugin`. Both compile to the same kind of binary.

#### Pros

- Inherits **all** default scheduler capabilities with zero extra implementation.
- You write only the code for your specific scheduling extension.
- Clean upgrade path: bump the k8s module version to get new scheduler features.
- The exact pattern used by the official scheduler-plugins repo and by production users (CockroachDB, etc.).
- All 11 extension points available.

#### Cons

- `k8s.io/kubernetes` vendoring requires careful `replace` directive management.
- Binary tracks the cluster's Kubernetes minor version — must be recompiled on upgrades.
- Larger binary size than a pure client-go scheduler (full kube-scheduler included).
- More initial setup than a simple HTTP server or client-go program.

#### When to Use

- Writing a **net-new plugin** that needs to run alongside all default scheduler behavior.
- Any production scheduling extension.
- When you want the full framework (all extension points, cache sharing, CycleState).
- Learning the plugin development pattern that the community uses.

---

### Comparison Table for a Label-Based Node Selection Learning Exercise

| Criterion | A: Extender | B: scheduler-plugins | C: From Scratch | D: NewSchedulerCommand |
|---|---|---|---|---|
| **Complexity to start** | Low | Medium | Low | Medium |
| **Language** | Any (HTTP server) | Go | Go | Go |
| **k8s vendoring pain** | None | Medium (pin minor ver) | Low (client-go only) | High (replace directives) |
| **Performance** | Poor (HTTP + JSON round-trip) | Best (in-process) | Medium (polling) | Best (in-process) |
| **Extension points available** | 4 (Filter, Prioritize, Preempt, Bind) | 11 (full framework) | Unlimited (custom) | 11 (full framework) |
| **Default plugins included** | Yes (runs alongside) | Yes | No (must re-implement) | Yes |
| **Cache sharing with scheduler** | No | Yes (via Handle) | No | Yes (via Handle) |
| **Taint/affinity handled for free** | Yes | Yes | No | Yes |
| **Leader election** | Not needed (stateless) | Built-in flag | Must add manually | Built-in flag |
| **Best for learning** | HTTP APIs, extender config | Plugin interfaces, framework lifecycle | Core binding API, what scheduler does | Full plugin + scheduler integration |
| **Recommended for production** | No | Yes | No | Yes |

---

### Recommendation for a Label-Based Node Selection Learning Exercise

**Phase 1 — Learn the fundamentals (Option C: from scratch)**

Start with a client-go-only scheduler. Implement:
1. List pods where `spec.nodeName == ""` and `spec.schedulerName == "my-scheduler"`.
2. List all nodes.
3. Filter nodes by label (e.g., pod annotation specifies required label key).
4. Create a `Binding` to assign the pod to the first matching node.

This takes 150–300 lines of Go and forces you to understand exactly what the binding API does, what information lives on a pod vs a node, and what happens when scheduling fails. There is no magic.

**Phase 2 — Learn the correct production pattern (Option D / B: plugin)**

Implement the same label logic as a `FilterPlugin` (see the code example in Option B above). Use `app.NewSchedulerCommand` + `app.WithPlugin` to build the binary. Deploy it with a `KubeSchedulerConfiguration` that enables your plugin. Observe:
- Default taint/toleration and node affinity behavior still works for free.
- The `CycleState` mechanism for sharing data between PreFilter and Filter.
- How `EnqueueExtensions` ensures pods are requeued when labels change.

**The label-based use case maps directly to the `Filter` extension point.** A `FilterPlugin` that checks `nodeInfo.Node().Labels` against a pod annotation and returns `framework.Unschedulable` for non-matching nodes is ~25 lines of Go and covers the full plugin interface pattern.

**Skip Option A (extender)** unless you specifically need to understand the webhook-based legacy approach. Its performance limitations and restricted extension points make it a poor learning vehicle for understanding how modern Kubernetes scheduling works.

---

### Common Pitfalls

#### Pitfall 1: Not Implementing EnqueueExtensions on Filter Plugins (Options B/D)

**What goes wrong**: A `FilterPlugin` that does not implement `EnqueueExtensions` will cause rejected pods to be permanently stuck in `unschedulablePods` until the 5-minute forced flush, rather than being requeued promptly when the cluster state that caused rejection changes (e.g., a new node with the required label is added).

**How to avoid**: Every plugin that implements `Filter`, `PreFilter`, `Reserve`, or `Permit` must also implement `EnqueueExtensions.EventsToRegister()`. For a label-based filter, register `{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: framework.Add | framework.Update}}`.

#### Pitfall 2: Race Conditions in from-Scratch Schedulers (Option C)

**What goes wrong**: Two scheduler instances (or your scheduler racing with the default scheduler) both try to bind the same pod. The second call to `Pods(ns).Bind(...)` returns an error.

**How to avoid**: Check for `errors.IsAlreadyExists(err)` on the Bind response and treat it as "already handled" — log it and continue to the next pod. Do not retry.

#### Pitfall 3: k8s.io/kubernetes Vendoring Without replace Directives (Options B/D)

**What goes wrong**: `go build` fails with messages about ambiguous imports or missing packages because `k8s.io/kubernetes` uses `replace` directives for its sub-packages that are not inherited by downstream modules.

**How to avoid**: Copy the entire `replace` block from `kubernetes-sigs/scheduler-plugins`'s `go.mod` for the matching k8s version. Do not attempt to construct replace directives manually.

#### Pitfall 4: Missing Taint/Toleration Checks in from-Scratch Schedulers (Option C)

**What goes wrong**: Pods scheduled to nodes with `effect: NoSchedule` taints that the pod does not tolerate will fail to start, producing cryptic kubelet errors.

**How to avoid**: Either implement taint/toleration checking in your node filter logic, or restrict your scheduler to test clusters with no taints, or add a `tolerations: [{operator: Exists}]` to test pods for the learning exercise.

#### Pitfall 5: Extender JSON Performance at Scale (Option A)

**What goes wrong**: In clusters with hundreds of nodes, the extender receives large `ExtenderArgs` payloads (full `NodeList`) on every scheduling decision. Under load, JSON encoding/decoding becomes the scheduling bottleneck.

**How to avoid**: Enable `nodeCacheCapable: true` to receive only node names and fetch node details directly from your extender's own cache. Or — for production — switch to Option B/D.

---

### Sources

#### Primary (HIGH confidence — official documentation and pkg.go.dev)
- [Kubernetes Scheduling Framework](https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/) — extension points, plugin interfaces, cycle flow
- [Scheduler Configuration](https://kubernetes.io/docs/reference/scheduling/config/) — KubeSchedulerConfiguration, extenders field, profiles, default plugins
- [k8s.io/kube-scheduler/extender/v1 — Go Packages](https://pkg.go.dev/k8s.io/kube-scheduler/extender/v1) — ExtenderArgs, ExtenderFilterResult, HostPriorityList, all extender types with full field definitions
- [sigs.k8s.io/scheduler-plugins — Go Packages](https://pkg.go.dev/sigs.k8s.io/scheduler-plugins) — version v0.33.5, compatibility table, available packages
- [kubernetes-sigs/scheduler-plugins GitHub](https://github.com/kubernetes-sigs/scheduler-plugins) — repo structure, cmd/scheduler/main.go, doc/develop.md

#### Secondary (MEDIUM confidence — community tutorials verified against official docs)
- [How to Write a Custom Kubernetes Scheduler from Scratch in Go — OneUptime (2026-02-09)](https://oneuptime.com/blog/post/2026-02-09-custom-kubernetes-scheduler-go/view) — client-go patterns: getUnscheduledPods, bind, scheduling loop
- [How to Build Kubernetes Scheduler Extenders — OneUptime (2026-01-30)](https://oneuptime.com/blog/post/2026-01-30-kubernetes-scheduler-extenders/view) — extender HTTP contract, KubeSchedulerConfiguration YAML
- [Guide to Building a Custom Kubernetes Scheduler — menraromial.com (2025)](https://menraromial.com/blog/2025/guide-building-custom-kubernetes-scheduler/) — Filter + Score plugin code, KubeSchedulerConfiguration YAML, Networkspeed plugin example
- [Writing CRL Custom Scheduler — Kubernetes Blog (2020-12)](https://kubernetes.io/blog/2020/12/21/writing-crl-scheduler/) — real-world plugin case study: what the framework provides vs from-scratch, zone-aware scheduling
- [Scheduler Framework and Extender Comparison — SoByte](https://www.sobyte.net/post/2022-02/kubernetes-scheduling-framework-and-extender/) — performance analysis (extender ~50% of scheduling duration), extension point comparison
- [K8S: Creating a kube-scheduler plugin — Julio Renner (Medium)](https://medium.com/@juliorenner123/k8s-creating-a-kube-scheduler-plugin-8a826c486a1) — plugin registration pattern
- [Your Guide to Extend Kubernetes Scheduler — Heba Elayoty](https://medium.com/@helayoty/your-guide-to-extend-kubernetes-scheduler-04fd4d15a130) — FilterPlugin and ScorePlugin interface definitions
- [Customizing K8S Scheduler — Gemini Open Cloud / CNCF (2022)](https://www.cncf.io/blog/2022/04/19/customizing-k8s-scheduler/) — extender pros/cons, comparison with framework

---

## Scheduler Plugin Implementation Details

**Research date:** 2026-03-08
**Confidence:** HIGH (interfaces from pkg.go.dev official docs; deployment pattern from kubernetes.io official docs; code examples verified against working out-of-tree plugin at github.com/rossgray/custom-k8s-scheduler)

This section answers the concrete "how do I build and run it" questions for a Score-based plugin that selects nodes based on pod labels alphabetically, deployed as a standalone container on the cluster.

---

### A. Exact Interface Definitions

All types live in `k8s.io/kubernetes/pkg/scheduler/framework` (package import path).

#### Plugin (base — every plugin must satisfy this)

```go
type Plugin interface {
    Name() string
}
```

#### FilterPlugin

```go
type FilterPlugin interface {
    Plugin
    Filter(ctx context.Context, state *CycleState, pod *v1.Pod, nodeInfo *NodeInfo) *Status
}
```

Critical notes:
- `nodeInfo *NodeInfo` is a **concrete struct pointer**, not an interface. Call `nodeInfo.Node()` to get `*v1.Node`.
- Returning `nil` is identical to `NewStatus(Success)`.
- Filter runs **concurrently across nodes** — do not write to shared mutable state inside Filter.

#### ScorePlugin

```go
type ScorePlugin interface {
    Plugin
    Score(ctx context.Context, state *CycleState, p *v1.Pod, nodeName string) (int64, *Status)
    ScoreExtensions() ScoreExtensions
}
```

Critical notes:
- `nodeName string` — Score receives a node **name**, NOT a NodeInfo. To get node properties, call `handle.SnapshotSharedLister().NodeInfos().Get(nodeName)`.
- Return `nil` from `ScoreExtensions()` when you do not need normalization.

#### ScoreExtensions (NormalizeScore)

```go
type ScoreExtensions interface {
    NormalizeScore(ctx context.Context, state *CycleState, p *v1.Pod, scores NodeScoreList) *Status
}
```

To implement: return `pl` (the plugin struct itself) from `ScoreExtensions()`, then add `NormalizeScore` as a method on the struct.

#### Score Range Constants

```go
const (
    MaxNodeScore int64 = 100
    MinNodeScore int64 = 0
)
```

#### NodeScore / NodeScoreList

```go
type NodeScore struct {
    Name  string
    Score int64
}
type NodeScoreList []NodeScore
```

---

### B. Status Type

```go
type Code int

const (
    Success                      Code = iota // 0 — or return nil
    Error                                     // internal error; pod retried
    Unschedulable                             // transient failure; pod retried on cluster events
    UnschedulableAndUnresolvable             // permanent failure; pod NOT retried
    Wait                                      // Permit plugins only
    Skip                                      // plugin skips this extension point
)

func NewStatus(code Code, reasons ...string) *Status
```

`nil` status equals `Success`. Use `Unschedulable` (not `Error`) when a node cannot host the pod — it allows retry; `Error` is for internal plugin errors.

---

### C. CycleState — Passing Data Between Extension Points

`CycleState` is a `sync.Map`-backed key-value store scoped to one scheduling cycle (one pod). Used to share pre-computed state between PreFilter → Filter or PreScore → Score.

```go
type StateKey string

type StateData interface {
    Clone() StateData  // required; called when CycleState is cloned
}

func (c *CycleState) Read(key StateKey) (StateData, error)
func (c *CycleState) Write(key StateKey, val StateData)
func (c *CycleState) Delete(key StateKey)
```

**Pattern — compute once in PreFilter (serial), read many times in Filter (concurrent):**

```go
type myState struct{ desiredZone string }
func (s *myState) Clone() framework.StateData { return &myState{desiredZone: s.desiredZone} }

const myStateKey framework.StateKey = "MyPlugin/state"

func (p *MyPlugin) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
    state.Write(myStateKey, &myState{desiredZone: pod.Labels["preferred-zone"]})
    return nil, nil
}

func (p *MyPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
    raw, err := state.Read(myStateKey)
    if err != nil {
        return framework.NewStatus(framework.Error, err.Error())
    }
    s := raw.(*myState)
    // s.desiredZone is available here
    return nil
}
```

Thread-safety rule: `sync.Map` concurrent reads are safe; writes during concurrent Filter calls are a data race. Only write in serial phases (PreFilter, PreScore).

---

### D. PluginFactory — Constructor Signature

```go
// k8s.io/kubernetes/pkg/scheduler/framework/runtime
type PluginFactory = func(ctx context.Context, configuration runtime.Object, f framework.Handle) (framework.Plugin, error)
```

The `ctx context.Context` **first parameter is required** in current Kubernetes (1.22+). Older tutorials showing a 2-argument form `func(runtime.Object, framework.Handle)` will not compile.

**Minimal constructor (no args):**

```go
func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
    return &MyPlugin{handle: h}, nil
}
```

**Constructor with typed plugin args:**

```go
func New(_ context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error) {
    args, ok := obj.(*MyPluginArgs)
    if !ok {
        return nil, fmt.Errorf("expected *MyPluginArgs, got %T", obj)
    }
    return &MyPlugin{handle: h, labelKey: args.LabelKey}, nil
}
```

Plugin args struct naming convention: must be `<PluginName>Args` (e.g., plugin `AlphabeticalScore` → struct `AlphabeticalScoreArgs`).

---

### E. framework.Handle — Accessing Cluster State

```go
type Handle interface {
    SnapshotSharedLister() SharedLister         // node/pod snapshot for current cycle
    ClientSet() clientset.Interface             // full Kubernetes API client
    EventRecorder() events.EventRecorder
    SharedInformerFactory() informers.SharedInformerFactory
    Parallelizer() parallelize.Parallelizer
    // + waiting pod management for Permit plugins
}

type SharedLister interface {
    NodeInfos() NodeInfoLister
}

type NodeInfoLister interface {
    Get(nodeName string) (*NodeInfo, error)
    List() ([]*NodeInfo, error)
}
```

**Lookup pattern inside Score:**

```go
func (pl *MyPlugin) Score(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
    nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
    if err != nil {
        return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q: %v", nodeName, err))
    }
    node := nodeInfo.Node() // *v1.Node — access node.Labels, node.Name, etc.
    if node == nil {
        return 0, framework.NewStatus(framework.Error, fmt.Sprintf("node %q not found", nodeName))
    }
    return computeScore(node, pod), nil
}
```

---

### F. Complete Score Plugin — Alphabetical Label Scoring

Full working implementation. Scores nodes by the alphabetical order of a label value; earlier in the alphabet = higher score.

```go
package alphabeticalscore

import (
    "context"
    "fmt"

    v1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/kubernetes/pkg/scheduler/framework"
)

const Name = "AlphabeticalScore"

type AlphabeticalScore struct {
    handle   framework.Handle
    labelKey string
}

var _ framework.ScorePlugin = &AlphabeticalScore{}

func (pl *AlphabeticalScore) Name() string { return Name }

func (pl *AlphabeticalScore) Score(
    ctx context.Context,
    state *framework.CycleState,
    pod *v1.Pod,
    nodeName string,
) (int64, *framework.Status) {
    nodeInfo, err := pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)
    if err != nil {
        return 0, framework.NewStatus(framework.Error, fmt.Sprintf("getting node %q: %v", nodeName, err))
    }
    node := nodeInfo.Node()
    if node == nil {
        return 0, framework.NewStatus(framework.Error, fmt.Sprintf("node %q not found", nodeName))
    }

    // Pod can override the label key to rank by via its own label
    labelKey := pl.labelKey
    if v, ok := pod.Labels["scheduler.io/rank-by-label"]; ok {
        labelKey = v
    }

    labelVal := node.Labels[labelKey]
    if len(labelVal) == 0 {
        return framework.MinNodeScore, nil
    }

    // 'a' → score 100, each subsequent letter reduces score by 4
    score := framework.MaxNodeScore - int64(labelVal[0]-'a')*4
    if score < framework.MinNodeScore {
        score = framework.MinNodeScore
    }
    return score, nil
}

// ScoreExtensions returns nil — raw scores already in [0,100], no normalization needed
func (pl *AlphabeticalScore) ScoreExtensions() framework.ScoreExtensions {
    return nil
}

func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
    return &AlphabeticalScore{
        handle:   h,
        labelKey: "topology.kubernetes.io/zone",
    }, nil
}
```

---

### G. Complete Filter Plugin — Label Match Example

Verified against github.com/rossgray/custom-k8s-scheduler/plugin/plugin.go (working out-of-tree plugin):

```go
package labelmatch

import (
    "context"
    "fmt"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/runtime"
    "k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
    Name       = "LabelMatch"
    MatchLabel = "nodeGroup"
)

type LabelMatch struct{ handle framework.Handle }

var _ framework.FilterPlugin = &LabelMatch{}

func (pl *LabelMatch) Name() string { return Name }

func (pl *LabelMatch) Filter(
    ctx context.Context,
    state *framework.CycleState,
    pod *corev1.Pod,
    nodeInfo *framework.NodeInfo,
) *framework.Status {
    node := nodeInfo.Node()

    podGroup, hasPodLabel := pod.GetLabels()[MatchLabel]
    if !hasPodLabel {
        return nil // no constraint on pod — allow anywhere
    }

    nodeGroup, hasNodeLabel := node.GetLabels()[MatchLabel]
    if !hasNodeLabel {
        return framework.NewStatus(framework.Unschedulable, "node missing required label")
    }

    if nodeGroup != podGroup {
        return framework.NewStatus(framework.Unschedulable,
            fmt.Sprintf("label mismatch: pod=%s node=%s", podGroup, nodeGroup))
    }
    return nil
}

func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
    return &LabelMatch{handle: h}, nil
}
```

---

### H. Plugin Registration — Out-of-Tree Binary Pattern

#### Registry and Option types

```go
// k8s.io/kubernetes/pkg/scheduler/framework/runtime
type Registry map[string]PluginFactory
func (r Registry) Register(name string, factory PluginFactory) error

// k8s.io/kubernetes/cmd/kube-scheduler/app
type Option func(runtime.Registry) error
func WithPlugin(name string, factory runtime.PluginFactory) Option
func NewSchedulerCommand(registryOptions ...Option) *cobra.Command
```

#### main.go — current pattern (used by sigs.k8s.io/scheduler-plugins)

```go
package main

import (
    "os"

    "k8s.io/component-base/cli"
    "k8s.io/kubernetes/cmd/kube-scheduler/app"

    "github.com/yourorg/custom-scheduler/pkg/alphabeticalscore"
    "github.com/yourorg/custom-scheduler/pkg/labelmatch"
)

func main() {
    command := app.NewSchedulerCommand(
        app.WithPlugin(alphabeticalscore.Name, alphabeticalscore.New),
        app.WithPlugin(labelmatch.Name, labelmatch.New),
    )
    code := cli.Run(command)
    os.Exit(code)
}
```

#### main.go — older pattern (still works, used in rossgray example)

```go
func main() {
    command := app.NewSchedulerCommand(
        app.WithPlugin(myplugin.Name, myplugin.New),
    )
    if err := command.Execute(); err != nil {
        klog.Fatal(err)
    }
}
```

---

### I. go.mod Setup

`k8s.io/kubernetes` is a monorepo. Its own `go.mod` uses `replace` directives that are **not inherited** by consumers. You must replicate all relevant replace directives in your module's go.mod.

#### Version mapping rule

| `k8s.io/kubernetes` | All `k8s.io/*` sub-packages |
|---------------------|------------------------------|
| v1.27.x             | v0.27.x                      |
| v1.28.x             | v0.28.x                      |
| v1.29.x             | v0.29.x                      |
| v1.30.x             | v0.30.x                      |
| v1.34.x             | v0.34.x (sigs.k8s.io/scheduler-plugins master, early 2026) |

#### Minimal go.mod (targeting k8s v1.27.3)

```
module github.com/yourorg/custom-scheduler

go 1.21

require (
    k8s.io/api            v0.27.3
    k8s.io/apimachinery   v0.27.3
    k8s.io/client-go      v0.27.3
    k8s.io/component-base v0.27.3
    k8s.io/kubernetes     v1.27.3
    k8s.io/klog/v2        v2.100.1
)

replace (
    k8s.io/api                          => k8s.io/api                          v0.27.3
    k8s.io/apimachinery                 => k8s.io/apimachinery                 v0.27.3
    k8s.io/apiserver                    => k8s.io/apiserver                    v0.27.3
    k8s.io/apiextensions-apiserver      => k8s.io/apiextensions-apiserver      v0.27.3
    k8s.io/cli-runtime                  => k8s.io/cli-runtime                  v0.27.3
    k8s.io/client-go                    => k8s.io/client-go                    v0.27.3
    k8s.io/cloud-provider               => k8s.io/cloud-provider               v0.27.3
    k8s.io/cluster-bootstrap            => k8s.io/cluster-bootstrap            v0.27.3
    k8s.io/code-generator               => k8s.io/code-generator               v0.27.3
    k8s.io/component-base               => k8s.io/component-base               v0.27.3
    k8s.io/component-helpers            => k8s.io/component-helpers            v0.27.3
    k8s.io/controller-manager           => k8s.io/controller-manager           v0.27.3
    k8s.io/cri-api                      => k8s.io/cri-api                      v0.27.3
    k8s.io/csi-translation-lib          => k8s.io/csi-translation-lib          v0.27.3
    k8s.io/dynamic-resource-allocation  => k8s.io/dynamic-resource-allocation  v0.27.3
    k8s.io/endpointslice                => k8s.io/endpointslice                v0.27.3
    k8s.io/kube-aggregator              => k8s.io/kube-aggregator              v0.27.3
    k8s.io/kube-controller-manager      => k8s.io/kube-controller-manager      v0.27.3
    k8s.io/kube-proxy                   => k8s.io/kube-proxy                   v0.27.3
    k8s.io/kube-scheduler               => k8s.io/kube-scheduler               v0.27.3
    k8s.io/kubectl                      => k8s.io/kubectl                      v0.27.3
    k8s.io/kubelet                      => k8s.io/kubelet                      v0.27.3
    k8s.io/legacy-cloud-providers       => k8s.io/legacy-cloud-providers       v0.27.3
    k8s.io/metrics                      => k8s.io/metrics                      v0.27.3
    k8s.io/mount-utils                  => k8s.io/mount-utils                  v0.27.3
    k8s.io/pod-security-admission       => k8s.io/pod-security-admission       v0.27.3
    k8s.io/sample-apiserver             => k8s.io/sample-apiserver             v0.27.3
    k8s.io/sample-cli-plugin            => k8s.io/sample-cli-plugin            v0.27.3
    k8s.io/sample-controller            => k8s.io/sample-controller            v0.27.3
)
```

This list is representative, not exhaustive. The complete list depends on which internal packages are transitively imported. If the build fails with "cannot find package" errors, extract the remaining replace directives from k8s's own go.mod.

**Extraction helper:**

```bash
KUBE_VERSION=v1.27.3
KUBE_GOPATH=$(go env GOPATH)/pkg/mod/k8s.io/kubernetes@${KUBE_VERSION}
# Apply all k8s replace directives to your module
grep -E '^\s+k8s\.io/' "${KUBE_GOPATH}/go.mod" | grep '=>' | \
  sed 's/=>//g' | awk '{print $1, $NF}' | \
  while read mod ver; do
    go mod edit -replace="${mod}=${mod}@${ver}"
  done
```

Reference: https://github.com/ereslibre/vendor-kubernetes

---

### J. Deploying as a Separate Container (Deployment in kube-system)

The canonical deployment model for an out-of-tree scheduler is a **Deployment** (not DaemonSet) with 1 replica in `kube-system`. Only pods that set `spec.schedulerName: custom-scheduler` are routed to it; all other pods continue using the default scheduler.

Source: kubernetes.io/docs/tasks/extend-kubernetes/configure-multiple-schedulers/ (official guide, HIGH confidence)

#### Required RBAC

The scheduler container runs under a ServiceAccount that must be bound to three roles:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: custom-scheduler
  namespace: kube-system
---
# Core scheduling permissions (pod binding, node/pod listing, events, leases)
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: custom-scheduler-as-kube-scheduler
subjects:
- kind: ServiceAccount
  name: custom-scheduler
  namespace: kube-system
roleRef:
  kind: ClusterRole
  name: system:kube-scheduler
  apiGroup: rbac.authorization.k8s.io
---
# Volume/storage scheduling permissions
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: custom-scheduler-as-volume-scheduler
subjects:
- kind: ServiceAccount
  name: custom-scheduler
  namespace: kube-system
roleRef:
  kind: ClusterRole
  name: system:volume-scheduler
  apiGroup: rbac.authorization.k8s.io
---
# Read scheduler auth configuration (needed for TLS)
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: custom-scheduler-auth-reader
  namespace: kube-system
roleRef:
  kind: Role
  name: extension-apiserver-authentication-reader
  apiGroup: rbac.authorization.k8s.io
subjects:
- kind: ServiceAccount
  name: custom-scheduler
  namespace: kube-system
```

#### KubeSchedulerConfiguration ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: custom-scheduler-config
  namespace: kube-system
data:
  scheduler-config.yaml: |
    apiVersion: kubescheduler.config.k8s.io/v1
    kind: KubeSchedulerConfiguration
    profiles:
      - schedulerName: custom-scheduler
        plugins:
          score:
            enabled:
              - name: AlphabeticalScore
                weight: 10
          filter:
            enabled:
              - name: LabelMatch
    leaderElection:
      leaderElect: false   # set true for HA with multiple replicas
```

#### Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: custom-scheduler
  namespace: kube-system
  labels:
    component: scheduler
spec:
  replicas: 1
  selector:
    matchLabels:
      component: scheduler
  template:
    metadata:
      labels:
        component: scheduler
    spec:
      serviceAccountName: custom-scheduler
      containers:
      - name: custom-scheduler
        image: yourregistry/custom-scheduler:latest
        imagePullPolicy: Always
        command:
        - /usr/local/bin/custom-scheduler
        - --config=/etc/kubernetes/scheduler/scheduler-config.yaml
        livenessProbe:
          httpGet:
            path: /healthz
            port: 10259
            scheme: HTTPS
          initialDelaySeconds: 15
        readinessProbe:
          httpGet:
            path: /healthz
            port: 10259
            scheme: HTTPS
        resources:
          requests:
            cpu: "0.1"
        securityContext:
          privileged: false
        volumeMounts:
        - name: scheduler-config
          mountPath: /etc/kubernetes/scheduler
      hostNetwork: false
      hostPID: false
      volumes:
      - name: scheduler-config
        configMap:
          name: custom-scheduler-config
```

#### Dockerfile (minimal)

```dockerfile
FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o custom-scheduler ./cmd/main.go

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/custom-scheduler /usr/local/bin/custom-scheduler
ENTRYPOINT ["/usr/local/bin/custom-scheduler"]
```

#### How pods opt in

Pods must set `spec.schedulerName` to the profile's `schedulerName` value:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-pod
spec:
  schedulerName: custom-scheduler   # routes this pod to the custom scheduler
  containers:
  - name: app
    image: nginx
    labels:               # pod-level labels used by AlphabeticalScore plugin
      preferred-zone: "a"
      nodeGroup: "gpu"
```

Pods without `spec.schedulerName` (or with `spec.schedulerName: default-scheduler`) continue using the default scheduler. The two schedulers operate independently.

#### DaemonSet note

A DaemonSet is NOT appropriate for deploying the scheduler itself. A DaemonSet runs one copy per node, which is not needed — the scheduler is a centralized controller. Use a Deployment with 1 replica (or more with `leaderElect: true` for HA).

---

### K. Common Implementation Pitfalls

#### Pitfall 1: Wrong PluginFactory signature
Old tutorials show `New(runtime.Object, framework.Handle)`. Current PluginFactory requires `ctx context.Context` as first argument.
**Fix:** Always use `func New(ctx context.Context, obj runtime.Object, h framework.Handle) (framework.Plugin, error)`.

#### Pitfall 2: Incomplete replace directives in go.mod
Build may succeed but `go build ./...` fails with "cannot find package" for internal k8s packages.
**Fix:** Copy all replace directives from k8s's own go.mod for your target version.

#### Pitfall 3: Score receives nodeName, not NodeInfo
Score's parameter is `nodeName string` — you MUST call `handle.SnapshotSharedLister().NodeInfos().Get(nodeName)` to look up node properties.

#### Pitfall 4: Writing to CycleState during concurrent Filter
**Fix:** Only write to CycleState in PreFilter (serial). Read in Filter (concurrent reads from sync.Map are safe).

#### Pitfall 5: Raw scores outside [0, 100]
If `ScoreExtensions()` returns `nil`, raw scores must already be within [0, 100] or behavior is undefined.
**Fix:** Clamp scores explicitly, or implement NormalizeScore via ScoreExtensions.

#### Pitfall 6: Plugin registered but not enabled in KubeSchedulerConfiguration
`WithPlugin` makes a plugin available; KubeSchedulerConfiguration enables it.
**Fix:** Add the plugin name under `plugins.score.enabled` or `plugins.filter.enabled` in the ConfigMap.

#### Pitfall 7: schedulerName mismatch
The `schedulerName` in KubeSchedulerConfiguration profile must exactly match the `spec.schedulerName` on pods.
**Fix:** Use consistent naming (e.g., `custom-scheduler`) in both places.

#### Pitfall 8: leaderElect: true with a single replica
With `leaderElect: true` and 1 replica, the scheduler may fail to acquire the leader lease and refuse to start.
**Fix:** Use `leaderElect: false` for single-replica deployments (most development/simple production setups).

---

### L. Reference Implementations

| Source | Interfaces | Key Takeaway |
|--------|-----------|--------------|
| `k8s.io/kubernetes/.../plugins/nodename` | FilterPlugin | Simplest possible filter |
| `k8s.io/kubernetes/.../plugins/noderesources/fit.go` | FilterPlugin, ScorePlugin | Both interfaces on one struct |
| `k8s.io/kubernetes/.../plugins/nodeaffinity/node_affinity.go` | FilterPlugin, PreScorePlugin, ScorePlugin | Label-based selection — closest analog to alphabetical label scoring |
| `github.com/rossgray/custom-k8s-scheduler` | FilterPlugin | Complete minimal out-of-tree plugin: verified plugin.go, main.go, go.mod with replace directives, deploy YAML |
| `sigs.k8s.io/scheduler-plugins` | Multiple | Production-grade examples; current go.mod structure for k8s v1.34.x |

---

### Sources for This Section

| Source | What Was Verified | Confidence |
|--------|-------------------|-----------|
| `pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/framework` | ScorePlugin, FilterPlugin, CycleState, NodeInfo, Handle, Status, constants | HIGH |
| `pkg.go.dev/k8s.io/kubernetes/cmd/kube-scheduler/app` | WithPlugin, Option, NewSchedulerCommand signatures | HIGH |
| `pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/framework/runtime` | Registry, PluginFactory type | HIGH |
| `kubernetes.io/docs/tasks/extend-kubernetes/configure-multiple-schedulers/` | Deployment YAML, RBAC, spec.schedulerName pattern | HIGH |
| `kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/` | Extension point ordering and semantics | HIGH |
| `github.com/rossgray/custom-k8s-scheduler` (raw source read) | Working plugin.go, main.go, deploy YAML with RBAC and ConfigMap | MEDIUM-HIGH |
| `github.com/kubernetes-sigs/scheduler-plugins` | go.mod structure for v1.34.1; cli.Run main.go pattern | MEDIUM-HIGH |
| `oneuptime.com/blog/.../scheduler-plugins-custom-scoring-logic/` | Score plugin patterns (NodeAge, CostAware, DataLocality examples) | MEDIUM |

---

## Testing Strategies for Scheduler Plugins

**Researched:** 2026-03-08
**Domain:** Go testing for Kubernetes scheduler plugins (unit, integration, E2E)
**Confidence:** HIGH (sourced from kubernetes/kubernetes source, pkg.go.dev official docs, kubernetes-sigs/scheduler-plugins repo patterns)

This section answers: how do you test a Filter or Score plugin at each level of the testing pyramid, from fast isolated unit tests through full integration tests using an in-process API server?

---

### 1. Key Packages for Testing

| Package | Import path | Purpose |
|---------|-------------|---------|
| Scheduler test framework | `k8s.io/kubernetes/pkg/scheduler/testing/framework` | `RegisterFilterPlugin`, `RegisterScorePlugin`, `NewFramework`, `BuildNodeInfos`, fake plugins |
| Scheduler test wrappers | `k8s.io/kubernetes/pkg/scheduler/testing` | `MakePod()`, `MakeNode()` builder chains |
| Framework runtime | `k8s.io/kubernetes/pkg/scheduler/framework/runtime` | `NewFramework`, `WithSnapshotSharedLister` |
| Scheduler cache | `k8s.io/kubernetes/pkg/scheduler/internal/cache` | `NewSnapshot` — feed nodes into a framework |
| Fake cache | `k8s.io/kubernetes/pkg/scheduler/internal/cache/fake` | `fake.Cache` for injecting custom pod/node behavior |
| Fake clientset | `k8s.io/client-go/kubernetes/fake` | `fake.NewSimpleClientset()` — in-memory Kubernetes API client |
| Framework interface | `k8s.io/kube-scheduler/framework` (via `k8s.io/kubernetes/pkg/scheduler/framework`) | `NewCycleState`, `NewNodeInfo`, `NodeScore`, `Status` |
| Integration test util | `k8s.io/kubernetes/test/integration/scheduler/util` | `InitTestSchedulerWithNS`, `CreateAndWaitForNodesInCache` |
| Integration test framework | `k8s.io/kubernetes/test/integration/framework` | `StartTestServer` — in-process kube-apiserver + etcd |

---

### 2. Unit Testing Scheduler Plugins

#### 2.1 The Three-Layer Setup

Every unit test for a plugin method needs three things:

1. **A plugin instance** — call your `New(ctx, args, handle)` factory directly.
2. **A `framework.CycleState`** — call `framework.NewCycleState()`.
3. **A `framework.NodeInfo`** — call `framework.NewNodeInfo()` then `.SetNode(&node)`.

The `framework.Handle` parameter to `New` is needed only if your plugin calls methods on it at **test time** (e.g., `SnapshotSharedLister` in Score). For pure Filter plugins that only read `nodeInfo.Node()`, you can pass `nil` as the handle.

#### 2.2 Constructing Test Objects

**Building a pod (using the `st` builder package):**

```go
import st "k8s.io/kubernetes/pkg/scheduler/testing"

pod := st.MakePod().
    Name("test-pod").
    Namespace("default").
    Label("scheduler.io/rank-by-label", "zone").
    Obj()
```

**Building a node:**

```go
node := st.MakeNode().
    Name("node-alpha").
    Label("topology.kubernetes.io/zone", "alpha").
    Capacity(map[v1.ResourceName]string{"cpu": "4", "memory": "8Gi"}).
    Obj()
```

**Building NodeInfo from a raw `v1.Node`:**

```go
import "k8s.io/kubernetes/pkg/scheduler/framework"

nodeInfo := framework.NewNodeInfo()
nodeInfo.SetNode(node)
```

**Building multiple NodeInfos from a node slice:**

```go
import tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"

nodeInfos := tf.BuildNodeInfos(nodes)  // []*v1.Node → []framework.NodeInfo
```

#### 2.3 Mocking `framework.Handle`

The `framework.Handle` interface is large. There are two practical approaches:

**Option A — Pass `nil` if your plugin does not use the handle in the method under test.**

A Filter plugin that only reads `nodeInfo.Node().Labels` never touches the handle in its `Filter()` method body. Passing `nil` is safe and avoids any mock setup.

```go
plugin, err := myplugin.New(ctx, nil, nil)  // nil handle is fine for Filter-only tests
require.NoError(t, err)
```

**Option B — Use `runtime.NewFramework` with `WithSnapshotSharedLister` for Score plugins.**

Score plugins typically call `handle.SnapshotSharedLister().NodeInfos().Get(nodeName)`. Build a real (lightweight) framework instance with a node snapshot:

```go
import (
    "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
    "k8s.io/kubernetes/pkg/scheduler/internal/cache"
    tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
)

nodes := []*v1.Node{
    st.MakeNode().Name("node-alpha").Label("zone", "alpha").Obj(),
    st.MakeNode().Name("node-beta").Label("zone", "beta").Obj(),
}

fh, err := tf.NewFramework(
    ctx,
    []tf.RegisterPluginFunc{
        tf.RegisterQueueSortPlugin("PrioritySort", ...), // required placeholder
        tf.RegisterBindPlugin("DefaultBinder", ...),    // required placeholder
    },
    "test-profile",
    runtime.WithSnapshotSharedLister(cache.NewSnapshot(nil, nodes)),
)
require.NoError(t, err)

plugin, err := myplugin.New(ctx, nil, fh)
```

Alternatively, from many in-tree test files, the even simpler pattern using `runtime.NewFramework` directly:

```go
fh, _ := runtime.NewFramework(
    ctx,
    nil,  // empty registry
    nil,  // no profile needed for just using Handle
    runtime.WithSnapshotSharedLister(cache.NewSnapshot(nil, nodes)),
)
```

#### 2.4 `framework.NewCycleState()`

`CycleState` is a thin `sync.Map` wrapper. Create it fresh per test case — no setup needed:

```go
state := framework.NewCycleState()
```

If your plugin writes state in `PreFilter` and reads it in `Filter`, call `PreFilter` first on the same state object before calling `Filter`:

```go
// PreFilter writes state
preResult, status := p.(framework.PreFilterPlugin).PreFilter(ctx, state, pod)
require.Nil(t, status)

// Filter reads state
filterStatus := p.(framework.FilterPlugin).Filter(ctx, state, pod, nodeInfo)
```

#### 2.5 Using `fake.NewSimpleClientset`

When a plugin calls `handle.ClientSet()` to read Kubernetes resources at test time:

```go
import "k8s.io/client-go/kubernetes/fake"

cs := fake.NewSimpleClientset(
    &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
    &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default"}},
)

fh, _ := runtime.NewFramework(ctx, nil, nil,
    runtime.WithClientSet(cs),
    runtime.WithSnapshotSharedLister(cache.NewSnapshot(nil, nodes)),
)
```

`fake.NewSimpleClientset` accepts any number of `runtime.Object` arguments as pre-seeded state. Subsequent API calls (`List`, `Get`, `Create`) operate on the in-memory store.

#### 2.6 Testing the `Filter` Method Directly

```go
func TestMyFilter(t *testing.T) {
    tests := []struct {
        name       string
        pod        *v1.Pod
        nodeLabels map[string]string
        wantStatus *framework.Status
    }{
        {
            name:       "node has required label — pass",
            pod:        st.MakePod().Label("required-label", "gpu").Obj(),
            nodeLabels: map[string]string{"gpu": "true"},
            wantStatus: nil, // nil == Success
        },
        {
            name:       "node missing required label — unschedulable",
            pod:        st.MakePod().Label("required-label", "gpu").Obj(),
            nodeLabels: map[string]string{"cpu": "amd"},
            wantStatus: framework.NewStatus(framework.Unschedulable, "..."),
        },
        {
            name:       "pod has no requirement — all nodes pass",
            pod:        st.MakePod().Obj(),
            nodeLabels: map[string]string{},
            wantStatus: nil,
        },
    }

    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            ctx := context.Background()

            // Build NodeInfo
            node := &v1.Node{
                ObjectMeta: metav1.ObjectMeta{
                    Name:   "test-node",
                    Labels: tc.nodeLabels,
                },
            }
            nodeInfo := framework.NewNodeInfo()
            nodeInfo.SetNode(node)

            // Instantiate plugin (nil handle is fine if Filter doesn't use it)
            p, err := myplugin.New(ctx, nil, nil)
            require.NoError(t, err)

            state := framework.NewCycleState()
            gotStatus := p.(framework.FilterPlugin).Filter(ctx, state, tc.pod, nodeInfo)

            if diff := cmp.Diff(tc.wantStatus, gotStatus, cmpopts.EquateErrors()); diff != "" {
                t.Errorf("Filter status mismatch (-want +got):\n%s", diff)
            }
        })
    }
}
```

#### 2.7 Testing the `Score` Method — Table-Driven Pattern

The Score method receives a `nodeName string`, not a `NodeInfo`. The plugin must look up the node from the handle's snapshot. This means you need a framework handle with a pre-populated snapshot.

```go
func TestAlphabeticalScore(t *testing.T) {
    tests := []struct {
        name          string
        nodes         []*v1.Node
        pod           *v1.Pod
        expectedScores framework.NodeScoreList
    }{
        {
            name: "earlier letter scores higher",
            nodes: []*v1.Node{
                st.MakeNode().Name("n1").Label("zone", "alpha").Obj(),
                st.MakeNode().Name("n2").Label("zone", "beta").Obj(),
                st.MakeNode().Name("n3").Label("zone", "gamma").Obj(),
            },
            pod: st.MakePod().Obj(),
            expectedScores: framework.NodeScoreList{
                {Name: "n1", Score: 100}, // 'a' → highest
                {Name: "n2", Score: 96},  // 'b'
                {Name: "n3", Score: 92},  // 'g'
            },
        },
        {
            name: "missing label gets minimum score",
            nodes: []*v1.Node{
                st.MakeNode().Name("n1").Label("zone", "alpha").Obj(),
                st.MakeNode().Name("n2").Obj(), // no zone label
            },
            pod: st.MakePod().Obj(),
            expectedScores: framework.NodeScoreList{
                {Name: "n1", Score: 100},
                {Name: "n2", Score: 0}, // framework.MinNodeScore
            },
        },
        {
            name: "empty label value gets minimum score",
            nodes: []*v1.Node{
                st.MakeNode().Name("n1").Label("zone", "").Obj(),
            },
            pod: st.MakePod().Obj(),
            expectedScores: framework.NodeScoreList{
                {Name: "n1", Score: 0},
            },
        },
        {
            name: "identical label values — equal scores",
            nodes: []*v1.Node{
                st.MakeNode().Name("n1").Label("zone", "alpha").Obj(),
                st.MakeNode().Name("n2").Label("zone", "alpha").Obj(),
            },
            pod: st.MakePod().Obj(),
            expectedScores: framework.NodeScoreList{
                {Name: "n1", Score: 100},
                {Name: "n2", Score: 100},
            },
        },
    }

    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            ctx := context.Background()

            // Build framework with node snapshot so Score can look up nodes
            fh, err := runtime.NewFramework(
                ctx, nil, nil,
                runtime.WithSnapshotSharedLister(cache.NewSnapshot(nil, tc.nodes)),
            )
            require.NoError(t, err)

            p, err := alphabeticalscore.New(ctx, nil, fh)
            require.NoError(t, err)

            state := framework.NewCycleState()

            // Run Score for each node, collect results
            var gotScores framework.NodeScoreList
            for _, node := range tc.nodes {
                score, status := p.(framework.ScorePlugin).Score(ctx, state, tc.pod, node.Name)
                require.Nil(t, status)
                gotScores = append(gotScores, framework.NodeScore{Name: node.Name, Score: score})
            }

            // If plugin has NormalizeScore, run it too
            if ext := p.(framework.ScorePlugin).ScoreExtensions(); ext != nil {
                status := ext.NormalizeScore(ctx, state, tc.pod, gotScores)
                require.Nil(t, status)
            }

            if diff := cmp.Diff(tc.expectedScores, gotScores); diff != "" {
                t.Errorf("scores mismatch (-want +got):\n%s", diff)
            }
        })
    }
}
```

**Key edge cases to cover for a label-based Score plugin:**

| Case | What to test | Expected result |
|------|-------------|-----------------|
| Label present, normal value | Node has `zone=alpha` | Score based on first character |
| Label missing entirely | Node has no `zone` label key | `framework.MinNodeScore` (0) |
| Label present, empty value | Node has `zone=""` | `framework.MinNodeScore` (0) |
| Identical label values | Two nodes, same zone value | Same score for both |
| Pod overrides label key | Pod label `scheduler.io/rank-by-label` set | Plugin uses that key instead |
| All nodes have same label | Normalization case | Scores equal after NormalizeScore |

#### 2.8 Testing PreScore → Score Chain

When a plugin uses `PreScore` to write to `CycleState` and `Score` reads from it:

```go
// Must call PreScore first, passing all nodes as NodeInfo slice
nodeInfos := tf.BuildNodeInfos(tc.nodes)
status := p.(framework.PreScorePlugin).PreScore(ctx, state, tc.pod, nodeInfos)
require.Nil(t, status)

// Then Score reads state that PreScore wrote
for _, node := range tc.nodes {
    score, status := p.(framework.ScorePlugin).Score(ctx, state, tc.pod, node.Name)
    require.Nil(t, status)
    gotScores = append(gotScores, framework.NodeScore{Name: node.Name, Score: score})
}
```

Pattern source: verified from `kubernetes/pkg/scheduler/framework/plugins/nodeaffinity/node_affinity_test.go` — the `TestNodeAffinityPriority` function.

---

### 3. Using the `testing/framework` Test Helpers

The `k8s.io/kubernetes/pkg/scheduler/testing/framework` package provides production-tested fake plugin implementations and a convenience `NewFramework` wrapper.

#### 3.1 Pre-Built Fake Plugins

These are useful when you need a complete `framework.Framework` instance (e.g., integration-style unit tests) and need to satisfy required extension points:

| Fake plugin | Type | Behavior |
|------------|------|----------|
| `TrueFilterPlugin` | FilterPlugin | Always returns `Success` |
| `FalseFilterPlugin` | FilterPlugin | Always returns `Unschedulable` |
| `MatchFilterPlugin` | FilterPlugin | Returns Success if `pod.Name == node.Name` |
| `FakeFilterPlugin` | FilterPlugin | Configurable per-node return code map |
| `FakePreScoreAndScorePlugin` | PreScore + Score | Configurable fixed score |

#### 3.2 `tf.NewFramework` — Convenience Wrapper

```go
import tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"

fwk, err := tf.NewFramework(
    ctx,
    []tf.RegisterPluginFunc{
        tf.RegisterFilterPlugin("TrueFilter", tf.NewTrueFilterPlugin()),
        tf.RegisterScorePlugin("MyScore", myplugin.New, 1),
        tf.RegisterQueueSortPlugin("PrioritySort", defaultQueueSortPlugin),
        tf.RegisterBindPlugin("DefaultBinder", defaultBindPlugin),
    },
    "test-profile",
    runtime.WithSnapshotSharedLister(cache.NewSnapshot(nil, testNodes)),
)
```

`tf.NewFramework` wraps `runtime.NewFramework` and handles the profile construction for you.

#### 3.3 `tf.BuildNodeInfos`

```go
nodes := []*v1.Node{
    st.MakeNode().Name("node1").Label("zone", "us-west").Obj(),
    st.MakeNode().Name("node2").Label("zone", "us-east").Obj(),
}
nodeInfos := tf.BuildNodeInfos(nodes) // returns []framework.NodeInfo
```

Used when you need to pass `NodeInfo` slice to `PreScore` or similar methods that take the full candidate set.

---

### 4. Integration Testing with an In-Process API Server

For tests that need real Kubernetes API semantics (watch, informers, pod status updates), the kubernetes-sigs/scheduler-plugins project uses an **in-process kube-apiserver** with etcd — no external cluster required.

This approach is used in `test/integration/` across the upstream kubernetes/kubernetes and kubernetes-sigs/scheduler-plugins repos.

#### 4.1 How the In-Process API Server Works

The `k8s.io/kubernetes/test/integration/framework` package provides `StartTestServer`, which:
- Starts a real `kube-apiserver` in-process (as a goroutine, not a subprocess)
- Starts an etcd instance (the etcd binary must be available on `PATH`)
- Returns a `*rest.Config` and a teardown function

The scheduler-plugins repo references this via `testutils.InitTestSchedulerWithNS` from `k8s.io/kubernetes/test/integration/util`.

#### 4.2 Scheduler Integration Test Pattern (from scheduler-plugins repo)

```go
package integration_test

import (
    "context"
    "testing"
    "time"

    v1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/util/wait"
    clientset "k8s.io/client-go/kubernetes"
    testutils "k8s.io/kubernetes/test/integration/util"
    fwkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"

    "github.com/yourorg/custom-scheduler/pkg/myplugin"
)

func TestMyPluginScheduling(t *testing.T) {
    // 1. Create a cancellable test context
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // 2. Register out-of-tree plugin and configure a scheduler profile
    registry := fwkruntime.Registry{
        myplugin.Name: myplugin.New,
    }

    profile := schedapi.KubeSchedulerProfile{
        SchedulerName: "default-scheduler",
        Plugins: &schedapi.Plugins{
            Score: schedapi.PluginSet{
                Enabled: []schedapi.Plugin{{Name: myplugin.Name}},
            },
        },
    }

    // 3. Start in-process API server + scheduler
    testCtx := testutils.InitTestSchedulerWithOptions(
        t,
        ctx,
        testutils.WithKubeSchedulerOptions(
            scheduler.WithFrameworkOutOfTreeRegistry(registry),
            scheduler.WithProfiles(profile),
        ),
    )
    testutils.SyncSchedulerInformerFactory(testCtx)
    go testCtx.Scheduler.Run(testCtx.Ctx)
    defer testutils.CleanupTest(t, testCtx)

    cs := testCtx.ClientSet

    // 4. Create test nodes
    for _, nodeName := range []string{"node-alpha", "node-beta", "node-gamma"} {
        node := st.MakeNode().
            Name(nodeName).
            Label("zone", nodeName[len("node-"):]).
            Capacity(map[v1.ResourceName]string{"cpu": "4", "memory": "8Gi"}).
            Obj()
        _, err := cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
        if err != nil {
            t.Fatalf("creating node %s: %v", nodeName, err)
        }
    }

    // 5. Create a pod and wait for it to be scheduled
    pod := st.MakePod().
        Name("test-pod").
        Namespace(testCtx.NS.Name).
        Obj()
    pod, err := cs.CoreV1().Pods(testCtx.NS.Name).Create(ctx, pod, metav1.CreateOptions{})
    if err != nil {
        t.Fatalf("creating pod: %v", err)
    }

    // 6. Poll until pod is scheduled; assert it landed on expected node
    err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true,
        func(ctx context.Context) (bool, error) {
            p, err := cs.CoreV1().Pods(testCtx.NS.Name).Get(ctx, pod.Name, metav1.GetOptions{})
            if err != nil {
                return false, err
            }
            return p.Spec.NodeName != "", nil
        })
    if err != nil {
        t.Fatalf("pod never scheduled: %v", err)
    }

    scheduledPod, _ := cs.CoreV1().Pods(testCtx.NS.Name).Get(ctx, pod.Name, metav1.GetOptions{})
    if scheduledPod.Spec.NodeName != "node-alpha" {
        t.Errorf("expected pod on node-alpha (earliest alphabetically), got %s", scheduledPod.Spec.NodeName)
    }
}
```

**What `InitTestSchedulerWithNS` / `InitTestSchedulerWithOptions` provides:**
- Spins up an in-process kube-apiserver and etcd
- Creates a `clientset` you can use to create nodes, pods, namespaces
- Creates a `scheduler.Scheduler` instance wired to the API server
- Returns a `testCtx` with `ClientSet`, `Scheduler`, `NS` (test namespace), and `Ctx`

**Running integration tests:**

```bash
# Requires etcd binary on PATH (port 2379 must be free)
make integration-test

# Or directly:
go test ./test/integration/... -v -count=1 -timeout=40m
```

#### 4.3 `envtest` — When and When Not to Use It

`sigs.k8s.io/controller-runtime/pkg/envtest` starts an in-process kube-apiserver and etcd but does **NOT** run kubelet, scheduler, or controller-manager. It is suitable for testing:
- Kubernetes controllers (reconcilers)
- Admission webhooks
- CRD CRUD operations

It is **NOT suitable** for testing scheduler plugin behavior because:
- No scheduler is running; pods will never get `spec.nodeName` set
- No kubelet, so pods will never transition to Running

For scheduler plugin integration tests, use `k8s.io/kubernetes/test/integration/framework.StartTestServer` (which includes a real scheduler instance) or the `testutils.InitTestSchedulerWithNS` pattern shown above.

---

### 5. E2E Testing Against a Real Cluster (kind)

For end-to-end validation of the full scheduling flow, deploy your custom scheduler to a kind cluster.

#### 5.1 Setup Flow

```bash
# 1. Create a kind cluster
kind create cluster --name scheduler-test

# 2. Build your scheduler image
docker build -t custom-scheduler:test .

# 3. Load the image into kind (avoids pushing to a registry)
kind load docker-image custom-scheduler:test --name scheduler-test

# 4. Apply RBAC + ConfigMap + Deployment manifests
kubectl apply -f manifests/rbac.yaml
kubectl apply -f manifests/configmap.yaml
kubectl apply -f manifests/deployment.yaml

# 5. Wait for scheduler pod to be running
kubectl wait pod -l component=scheduler -n kube-system --for=condition=Ready --timeout=60s
```

#### 5.2 Accessing the kind Control Plane for Config Changes

If you need to modify the static pod manifest (e.g., for testing the plugin as a replacement rather than a secondary scheduler):

```bash
# Enter the control plane container
docker exec -it $(docker ps --filter name=control-plane --format '{{.ID}}') bash

# Edit or add the scheduler config
cat > /etc/kubernetes/sched-cc.yaml << 'EOF'
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
- schedulerName: default-scheduler
  plugins:
    score:
      enabled:
      - name: AlphabeticalScore
EOF

# Patch the static pod manifest
# (kubelet will automatically restart the scheduler)
```

#### 5.3 Verifying Plugin Behavior in kind

```bash
# Create test nodes with labels (kind nodes are fake — label them manually)
kubectl label node kind-worker zone=alpha
kubectl label node kind-worker2 zone=beta

# Create a pod using your custom scheduler
kubectl apply -f - << 'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: test-pod
spec:
  schedulerName: custom-scheduler
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
EOF

# Verify scheduling decision
kubectl get pod test-pod -o jsonpath='{.spec.nodeName}'

# Check scheduler logs for plugin events
kubectl logs -l component=scheduler -n kube-system | grep AlphabeticalScore
```

#### 5.4 When to Use kind vs In-Process Integration Tests

| Concern | In-Process (testutils) | kind |
|---------|----------------------|------|
| Speed | Fast (~seconds per test) | Slow (cluster spin-up ~1-2 min) |
| CI setup | Needs etcd binary | Needs Docker + kind binary |
| Isolation | Per-test API server | Shared cluster |
| Real kubelet behavior | No | Yes |
| Image build required | No | Yes |
| Best for | Plugin logic correctness | Full deployment validation |

**Recommendation:** Use in-process integration tests (`testutils.InitTestSchedulerWithNS`) for correctness testing in CI. Use kind for final deployment validation and smoke testing.

---

### 6. What the kubernetes/kubernetes Repo Itself Does

The upstream kubernetes repo provides the canonical reference patterns. Key files to study:

#### 6.1 `pkg/scheduler/framework/plugins/nodeaffinity/node_affinity_test.go`

The closest analog to a label-based Score plugin. Patterns used:

- **Table struct fields:** `name`, `pod`, `nodes []*v1.Node`, `expectedList framework.NodeScoreList`, `args config.NodeAffinityArgs`, `runPreScore bool`
- **Framework construction:** `runtime.NewFramework(ctx, nil, nil, runtime.WithSnapshotSharedLister(cache.NewSnapshot(nil, test.nodes)))`
- **Plugin construction:** `New(ctx, &test.args, fh, feature.Features{})`
- **Node construction:** Raw `v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: label1}}`
- **Calling PreScore:** `p.(fwk.PreScorePlugin).PreScore(ctx, state, test.pod, tf.BuildNodeInfos(test.nodes))`
- **Calling Score:** `p.(fwk.ScorePlugin).Score(ctx, state, test.pod, nodeInfo.Node().Name)`
- **Assertion:** `cmp.Diff(test.wantStatus, gotStatus)` with `t.Errorf`

#### 6.2 `pkg/scheduler/framework/plugins/noderesources/fit_test.go`

Patterns used for Filter testing:

- **Imports:** `ktesting "k8s.io/test/utils/ktesting"`, `require "github.com/stretchr/testify/require"`
- **Node helper:** `makeResources(milliCPU, memory, pods, extendedA, storage, hugePageA)` returns `v1.ResourceList`
- **NodeInfo setup:** `framework.NewNodeInfo(existingPods...)` then `nodeInfo.SetNode(&node)`
- **Test runner:** `tCtx.SyncTest(test.name, func(tCtx ktesting.TContext) { ... })`
- **Filter call:** `p.(framework.FilterPlugin).Filter(ctx, state, test.pod, test.nodeInfo)`

#### 6.3 `pkg/scheduler/framework/runtime/framework_test.go`

Shows how to build a `TestPlugin` struct with injected result codes, useful when you need a plugin that behaves differently per test case:

```go
type TestPlugin struct {
    name string
    inj  injectedResult
}

type injectedResult struct {
    FilterStatus int
    ScoreStatus  int
    ScoreRes     int64
}

func (pl *TestPlugin) Filter(ctx context.Context, state *fwk.CycleState,
    pod *v1.Pod, nodeInfo fwk.NodeInfo) *fwk.Status {
    return fwk.NewStatus(fwk.Code(pl.inj.FilterStatus), injectFilterReason)
}

func (pl *TestPlugin) Score(ctx context.Context, state *fwk.CycleState,
    pod *v1.Pod, nodeName string) (int64, *fwk.Status) {
    return pl.inj.ScoreRes, fwk.NewStatus(fwk.Code(pl.inj.ScoreStatus))
}
```

The framework is built with a registry mapping plugin names to factory closures over the injected result:

```go
r := make(runtime.Registry)
r.Register("TestPlugin", func(_ context.Context, _ runtime.Object, fh fwk.Handle) (fwk.Plugin, error) {
    return &TestPlugin{name: "TestPlugin", inj: injectedResult{FilterStatus: int(fwk.Unschedulable)}}, nil
})

fwk, err := runtime.NewFramework(ctx, r, &profile)
```

#### 6.4 `test/integration/scheduler/scheduler_test.go`

The main scheduler integration tests. Pattern:

```go
testCtx := testutils.InitTestSchedulerWithNS(t, "my-test-ns")
defer testutils.CleanupTest(t, testCtx)
go testCtx.Scheduler.Run(testCtx.Ctx)

// Create nodes and pods via testCtx.ClientSet
// Poll with wait.PollUntilContextTimeout to assert pod.Spec.NodeName
```

---

### 7. Complete Test File Template for a Label-Based Score Plugin

Below is a self-contained template you can adapt directly. Assumes the plugin package is `alphabeticalscore` with exported symbol `Name` and factory `New`.

```go
package alphabeticalscore_test

import (
    "context"
    "testing"

    "github.com/google/go-cmp/cmp"
    v1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/runtime"
    fwk "k8s.io/kubernetes/pkg/scheduler/framework"
    "k8s.io/kubernetes/pkg/scheduler/framework/runtime" // fwkruntime
    "k8s.io/kubernetes/pkg/scheduler/internal/cache"
    st "k8s.io/kubernetes/pkg/scheduler/testing"
    tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
    "github.com/stretchr/testify/require"

    "github.com/yourorg/custom-scheduler/pkg/alphabeticalscore"
)

// newFrameworkWithNodes builds a minimal framework handle seeded with the given nodes.
func newFrameworkWithNodes(t *testing.T, nodes []*v1.Node) fwk.Handle {
    t.Helper()
    ctx := context.Background()
    fh, err := runtime.NewFramework(
        ctx,
        nil,
        nil,
        runtime.WithSnapshotSharedLister(cache.NewSnapshot(nil, nodes)),
    )
    require.NoError(t, err)
    return fh
}

func TestAlphabeticalScore_Score(t *testing.T) {
    tests := []struct {
        name           string
        nodes          []*v1.Node
        labelKey       string  // label key the plugin ranks by
        pod            *v1.Pod
        wantScores     fwk.NodeScoreList
        wantScoreErr   bool
    }{
        {
            name: "earlier letter gets higher score",
            nodes: []*v1.Node{
                st.MakeNode().Name("n-alpha").Label("zone", "alpha").Obj(),
                st.MakeNode().Name("n-beta").Label("zone", "beta").Obj(),
                st.MakeNode().Name("n-gamma").Label("zone", "gamma").Obj(),
            },
            pod: st.MakePod().Name("p").Obj(),
            wantScores: fwk.NodeScoreList{
                {Name: "n-alpha", Score: 100},
                {Name: "n-beta",  Score: 96},
                {Name: "n-gamma", Score: 68},
            },
        },
        {
            name: "missing label — min score",
            nodes: []*v1.Node{
                st.MakeNode().Name("n-labeled").Label("zone", "alpha").Obj(),
                st.MakeNode().Name("n-bare").Obj(),
            },
            pod: st.MakePod().Name("p").Obj(),
            wantScores: fwk.NodeScoreList{
                {Name: "n-labeled", Score: 100},
                {Name: "n-bare",    Score: 0},
            },
        },
        {
            name: "empty label value — min score",
            nodes: []*v1.Node{
                st.MakeNode().Name("n-empty").Label("zone", "").Obj(),
            },
            pod: st.MakePod().Name("p").Obj(),
            wantScores: fwk.NodeScoreList{
                {Name: "n-empty", Score: 0},
            },
        },
        {
            name: "identical labels — equal scores",
            nodes: []*v1.Node{
                st.MakeNode().Name("n1").Label("zone", "alpha").Obj(),
                st.MakeNode().Name("n2").Label("zone", "alpha").Obj(),
            },
            pod: st.MakePod().Name("p").Obj(),
            wantScores: fwk.NodeScoreList{
                {Name: "n1", Score: 100},
                {Name: "n2", Score: 100},
            },
        },
        {
            name: "pod overrides label key",
            nodes: []*v1.Node{
                st.MakeNode().Name("n1").Label("region", "amer").Obj(),
                st.MakeNode().Name("n2").Label("region", "emea").Obj(),
            },
            pod: st.MakePod().Name("p").Label("scheduler.io/rank-by-label", "region").Obj(),
            wantScores: fwk.NodeScoreList{
                {Name: "n1", Score: 100}, // 'a' (amer)
                {Name: "n2", Score: 84},  // 'e' (emea)
            },
        },
    }

    for _, tc := range tests {
        t.Run(tc.name, func(t *testing.T) {
            ctx := context.Background()
            fh := newFrameworkWithNodes(t, tc.nodes)

            p, err := alphabeticalscore.New(ctx, nil, fh)
            require.NoError(t, err)

            state := fwk.NewCycleState()

            var gotScores fwk.NodeScoreList
            for _, node := range tc.nodes {
                score, status := p.(fwk.ScorePlugin).Score(ctx, state, tc.pod, node.Name)
                if tc.wantScoreErr {
                    require.NotNil(t, status)
                    return
                }
                require.Nil(t, status, "Score for node %s returned unexpected status", node.Name)
                gotScores = append(gotScores, fwk.NodeScore{Name: node.Name, Score: score})
            }

            // Run NormalizeScore if the plugin implements it
            if ext := p.(fwk.ScorePlugin).ScoreExtensions(); ext != nil {
                status := ext.NormalizeScore(ctx, state, tc.pod, gotScores)
                require.Nil(t, status, "NormalizeScore returned unexpected status")
            }

            if diff := cmp.Diff(tc.wantScores, gotScores); diff != "" {
                t.Errorf("score mismatch (-want +got):\n%s", diff)
            }
        })
    }
}

// TestAlphabeticalScore_NodeSelection verifies that the highest-scoring node
// is selected when all nodes are run through the full Score pipeline.
func TestAlphabeticalScore_NodeSelection(t *testing.T) {
    nodes := []*v1.Node{
        st.MakeNode().Name("n-charlie").Label("zone", "charlie").Obj(),
        st.MakeNode().Name("n-alpha").Label("zone", "alpha").Obj(),
        st.MakeNode().Name("n-bravo").Label("zone", "bravo").Obj(),
    }
    pod := st.MakePod().Name("p").Obj()

    ctx := context.Background()
    fh := newFrameworkWithNodes(t, nodes)
    p, err := alphabeticalscore.New(ctx, nil, fh)
    require.NoError(t, err)

    state := fwk.NewCycleState()
    var scores fwk.NodeScoreList
    for _, node := range nodes {
        score, status := p.(fwk.ScorePlugin).Score(ctx, state, pod, node.Name)
        require.Nil(t, status)
        scores = append(scores, fwk.NodeScore{Name: node.Name, Score: score})
    }

    // Find winner
    var best fwk.NodeScore
    for _, s := range scores {
        if s.Score > best.Score {
            best = s
        }
    }

    if best.Name != "n-alpha" {
        t.Errorf("expected n-alpha to win (earliest alphabetically), got %s (score %d)", best.Name, best.Score)
    }
}
```

---

### 8. Common Pitfalls in Plugin Tests

#### Pitfall 1: Passing `nil` handle when Score calls `SnapshotSharedLister`

**What goes wrong:** `Score` calls `pl.handle.SnapshotSharedLister().NodeInfos().Get(nodeName)`, but the test passes `nil` as the handle. Result: nil pointer dereference at test time.

**Fix:** Build a real framework with `runtime.NewFramework(..., runtime.WithSnapshotSharedLister(...))` and pass it as the handle.

#### Pitfall 2: Calling `Score` before `PreScore` when plugin uses CycleState

**What goes wrong:** Plugin's `Score` reads a key from `state` that `PreScore` was supposed to write. If `PreScore` never ran, `state.Read` returns `ErrNotFound` and Score returns an error status.

**Fix:** Always call `PreScore` on the same `CycleState` before calling `Score` in unit tests, matching the runtime's call order.

#### Pitfall 3: `NodeScoreList` order dependency in assertions

**What goes wrong:** `cmp.Diff` on `NodeScoreList` is order-sensitive. If your test iterates nodes in a different order than expected, the diff fails even if scores are correct.

**Fix:** Sort both lists by `Name` before diffing, or use `cmpopts.SortSlices(func(a, b fwk.NodeScore) bool { return a.Name < b.Name })`.

#### Pitfall 4: `cmp.Diff` on `*framework.Status` does not compare cleanly without options

`*framework.Status` has unexported fields. `cmp.Diff` panics or produces misleading output.

**Fix:** Use `cmp.AllowUnexported(framework.Status{})` or compare via `status.Code()` and `status.Reasons()` directly, or use `cmpopts.IgnoreUnexported`.

#### Pitfall 5: Integration tests failing because etcd port 2379 is in use

**What goes wrong:** `testutils.InitTestSchedulerWithNS` starts an in-process etcd. If port 2379 is occupied, the test panics.

**Fix:** Ensure no other etcd is running before running integration tests. Use `make integration-test` from the scheduler-plugins repo which handles this.

#### Pitfall 6: Forgetting `go testCtx.Scheduler.Run(testCtx.Ctx)` in integration tests

**What goes wrong:** The scheduler is initialized but never started. Pods are never scheduled; `wait.PollUntilContextTimeout` times out.

**Fix:** Always `go testCtx.Scheduler.Run(testCtx.Ctx)` before creating pods in integration tests.

---

### Sources for This Section

| Source | What Was Verified | Confidence |
|--------|-------------------|------------|
| [kubernetes/kubernetes: `pkg/scheduler/framework/plugins/nodeaffinity/node_affinity_test.go`](https://github.com/kubernetes/kubernetes/blob/master/pkg/scheduler/framework/plugins/nodeaffinity/node_affinity_test.go) | PreScore/Score test structure, BuildNodeInfos usage, framework construction with WithSnapshotSharedLister | HIGH |
| [kubernetes/kubernetes: `pkg/scheduler/framework/plugins/noderesources/fit_test.go`](https://github.com/kubernetes/kubernetes/blob/master/pkg/scheduler/framework/plugins/noderesources/fit_test.go) | Filter test structure, NodeInfo.SetNode pattern, ktesting usage | HIGH |
| [kubernetes/kubernetes: `pkg/scheduler/framework/runtime/framework_test.go`](https://github.com/kubernetes/kubernetes/blob/master/pkg/scheduler/framework/runtime/framework_test.go) | TestPlugin injected-result pattern, NewFramework in tests, registry construction | HIGH |
| [pkg.go.dev: `k8s.io/kubernetes/pkg/scheduler/testing/framework`](https://pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/testing/framework) | RegisterFilterPlugin, RegisterScorePlugin, BuildNodeInfos, fake plugins, NewFramework signature | HIGH |
| [pkg.go.dev: `k8s.io/kubernetes/pkg/scheduler/testing`](https://pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/testing) | MakePod, MakeNode builder methods and Obj() terminator | HIGH |
| [pkg.go.dev: `k8s.io/kubernetes/pkg/scheduler/framework/runtime`](https://pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/framework/runtime) | NewFramework, WithSnapshotSharedLister, WithClientSet option signatures | HIGH |
| [pkg.go.dev: `k8s.io/kube-scheduler/framework`](https://pkg.go.dev/k8s.io/kube-scheduler/framework) | NodeInfo interface (SetNode, Node, GetPods), CycleState interface, Handle interface methods | HIGH |
| [pkg.go.dev: `k8s.io/kubernetes/pkg/scheduler/internal/cache/fake`](https://pkg.go.dev/k8s.io/kubernetes/pkg/scheduler/internal/cache/fake) | fake.Cache struct and injectable function fields | HIGH |
| [kubernetes-sigs/scheduler-plugins: `test/integration/coscheduling_test.go`](https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/test/integration/coscheduling_test.go) | Integration test pattern: registry, profile, InitTestScheduler, wait.PollUntilContextTimeout | HIGH |
| [kubernetes-sigs/scheduler-plugins: `doc/develop.md`](https://scheduler-plugins.sigs.k8s.io/docs/user-guide/develop/) | `make unit-test`, `make integration-test`, etcd requirement | HIGH |
| [scheduler-plugins installation — kind cluster](https://scheduler-plugins.sigs.k8s.io/docs/user-guide/installation/) | kind create cluster, docker exec control-plane, sched-cc.yaml config, image load | HIGH |
| [pkg.go.dev: `sigs.k8s.io/controller-runtime/pkg/envtest`](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest) | envtest scope (no scheduler/kubelet) — confirmed NOT suitable for scheduler plugin testing | HIGH |
| [kubernetes/kubernetes: `test/integration/scheduler/scheduler_test.go`](https://github.com/kubernetes/kubernetes/blob/master/test/integration/scheduler/scheduler_test.go) | `testutils.InitTestSchedulerWithNS`, integration test pattern | HIGH |
| [pkg.go.dev: `k8s.io/kubernetes/test/integration/framework`](https://pkg.go.dev/k8s.io/kubernetes/test/integration/framework) | StartTestServer, TestServerSetup, SharedEtcd | HIGH |
