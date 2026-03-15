package alphabeticalscore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// --- Mock framework.Handle hierarchy ---

// fakeHandle embeds the framework.Handle interface to satisfy it while only
// overriding SnapshotSharedLister. Calling any other method will panic.
type fakeHandle struct {
	framework.Handle
	lister framework.SharedLister
}

func (h *fakeHandle) SnapshotSharedLister() framework.SharedLister {
	return h.lister
}

// fakeSharedLister implements framework.SharedLister.
type fakeSharedLister struct {
	nodeInfoLister framework.NodeInfoLister
}

func (f *fakeSharedLister) NodeInfos() framework.NodeInfoLister {
	return f.nodeInfoLister
}

func (f *fakeSharedLister) StorageInfos() framework.StorageInfoLister {
	return nil
}

// fakeNodeInfoLister implements framework.NodeInfoLister by embedding the interface
// (so unimplemented methods panic) and overriding Get and List.
type fakeNodeInfoLister struct {
	framework.NodeInfoLister
	nodeInfos map[string]*framework.NodeInfo
}

func (f *fakeNodeInfoLister) Get(nodeName string) (*framework.NodeInfo, error) {
	ni, ok := f.nodeInfos[nodeName]
	if !ok {
		return nil, nil
	}
	return ni, nil
}

func (f *fakeNodeInfoLister) List() ([]*framework.NodeInfo, error) {
	list := make([]*framework.NodeInfo, 0, len(f.nodeInfos))
	for _, ni := range f.nodeInfos {
		list = append(list, ni)
	}
	return list, nil
}

// newTestHandle builds a fakeHandle populated with the given nodes.
func newTestHandle(nodes []*v1.Node) *fakeHandle {
	niMap := make(map[string]*framework.NodeInfo, len(nodes))
	for _, node := range nodes {
		ni := framework.NewNodeInfo()
		ni.SetNode(node)
		niMap[node.Name] = ni
	}
	return &fakeHandle{
		lister: &fakeSharedLister{
			nodeInfoLister: &fakeNodeInfoLister{nodeInfos: niMap},
		},
	}
}

// newNode creates a v1.Node with the given name and labels.
func newNode(name string, labels map[string]string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}

// newPlugin creates an AlphabeticalScore plugin backed by the provided handle.
func newPlugin(t *testing.T, nodes []*v1.Node) *AlphabeticalScore {
	t.Helper()
	h := newTestHandle(nodes)
	plugin, err := New(context.Background(), nil, h)
	require.NoError(t, err)
	return plugin.(*AlphabeticalScore)
}

// --- Tests ---

func TestName(t *testing.T) {
	pl := newPlugin(t, nil)
	assert.Equal(t, "AlphabeticalScore", pl.Name())
}

func TestScore(t *testing.T) {
	cases := []struct {
		name          string
		nodes         []*v1.Node
		pod           *v1.Pod
		targetNode    string
		expectedScore int64
	}{
		{
			name: "label alpha scores 100 (a=0)",
			nodes: []*v1.Node{
				newNode("node1", map[string]string{DefaultLabelKey: "alpha"}),
			},
			pod:           &v1.Pod{},
			targetNode:    "node1",
			expectedScore: 100,
		},
		{
			name: "label bravo scores 96 (b=1)",
			nodes: []*v1.Node{
				newNode("node1", map[string]string{DefaultLabelKey: "bravo"}),
			},
			pod:           &v1.Pod{},
			targetNode:    "node1",
			expectedScore: 96,
		},
		{
			name: "label charlie scores 92 (c=2)",
			nodes: []*v1.Node{
				newNode("node1", map[string]string{DefaultLabelKey: "charlie"}),
			},
			pod:           &v1.Pod{},
			targetNode:    "node1",
			expectedScore: 92,
		},
		{
			name: "label gamma scores 76 (g=6)",
			nodes: []*v1.Node{
				newNode("node1", map[string]string{DefaultLabelKey: "gamma"}),
			},
			pod:           &v1.Pod{},
			targetNode:    "node1",
			expectedScore: 76,
		},
		{
			name: "missing label scores 0",
			nodes: []*v1.Node{
				newNode("node1", map[string]string{}),
			},
			pod:           &v1.Pod{},
			targetNode:    "node1",
			expectedScore: framework.MinNodeScore,
		},
		{
			name: "empty label value scores 0",
			nodes: []*v1.Node{
				newNode("node1", map[string]string{DefaultLabelKey: ""}),
			},
			pod:           &v1.Pod{},
			targetNode:    "node1",
			expectedScore: framework.MinNodeScore,
		},
		{
			name: "uppercase label treated case-insensitively (Alpha == alpha == 100)",
			nodes: []*v1.Node{
				newNode("node1", map[string]string{DefaultLabelKey: "Alpha"}),
			},
			pod:           &v1.Pod{},
			targetNode:    "node1",
			expectedScore: 100,
		},
		{
			name: "pod overrides label key via annotation",
			nodes: []*v1.Node{
				// Default label key has a bad value; custom key has "alpha"
				newNode("node1", map[string]string{
					DefaultLabelKey:  "zulu",
					"custom-label":   "alpha",
				}),
			},
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						PodLabelOverrideKey: "custom-label",
					},
				},
			},
			targetNode:    "node1",
			expectedScore: 100,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pl := newPlugin(t, tc.nodes)
			score, status := pl.Score(context.Background(), framework.NewCycleState(), tc.pod, tc.targetNode)
			assert.Nil(t, status)
			assert.Equal(t, tc.expectedScore, score)
		})
	}
}

func TestScoreTwoNodesIdenticalLabel(t *testing.T) {
	nodes := []*v1.Node{
		newNode("node-a", map[string]string{DefaultLabelKey: "alpha"}),
		newNode("node-b", map[string]string{DefaultLabelKey: "alpha"}),
	}
	pl := newPlugin(t, nodes)
	pod := &v1.Pod{}
	state := framework.NewCycleState()

	scoreA, statusA := pl.Score(context.Background(), state, pod, "node-a")
	scoreB, statusB := pl.Score(context.Background(), state, pod, "node-b")

	assert.Nil(t, statusA)
	assert.Nil(t, statusB)
	assert.Equal(t, int64(100), scoreA)
	assert.Equal(t, int64(100), scoreB)
}

func TestNodeSelection(t *testing.T) {
	// Three nodes deliberately out of alphabetical order.
	nodes := []*v1.Node{
		newNode("charlie-node", map[string]string{DefaultLabelKey: "charlie"}),
		newNode("alpha-node", map[string]string{DefaultLabelKey: "alpha"}),
		newNode("bravo-node", map[string]string{DefaultLabelKey: "bravo"}),
	}
	pl := newPlugin(t, nodes)
	pod := &v1.Pod{}
	state := framework.NewCycleState()

	type result struct {
		name  string
		score int64
	}

	var best result
	for _, node := range nodes {
		score, status := pl.Score(context.Background(), state, pod, node.Name)
		require.Nil(t, status)
		if score > best.score {
			best = result{name: node.Name, score: score}
		}
	}

	assert.Equal(t, "alpha-node", best.name, "node with label 'alpha' should win")
}

// Ensure the plugin satisfies the ScorePlugin interface at compile time.
var _ framework.ScorePlugin = &AlphabeticalScore{}

// Ensure New returns a valid plugin.
func TestNew(t *testing.T) {
	h := newTestHandle(nil)
	plugin, err := New(context.Background(), &runtime.Unknown{}, h)
	require.NoError(t, err)
	require.NotNil(t, plugin)
	assert.Equal(t, Name, plugin.Name())
}
