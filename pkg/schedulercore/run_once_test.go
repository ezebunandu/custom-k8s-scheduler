package schedulercore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestRunOnce_TableDriven(t *testing.T) {
	now := metav1.NewTime(time.Now())

	tests := []struct {
		name           string
		pods           []v1.Pod
		nodes          []v1.Node
		reactors       []func(*fake.Clientset)
		wantErr        string
		wantStatuses   []ResultStatus
		wantNodeByPod  map[string]string
		wantBindCalled bool
	}{
		{
			name: "filters skipped pods and returns no_node when no nodes exist",
			pods: []v1.Pod{
				newPod("default", "scheduled-by-us", SchedulerName, nil),
				newPod("default", "other-scheduler", "other-scheduler", nil),
				newPod("default", "deleting", SchedulerName, &now),
			},
			nodes:         nil,
			wantStatuses:  []ResultStatus{StatusNoNode},
			wantNodeByPod: map[string]string{"scheduled-by-us": ""},
		},
		{
			name: "returns scheduled with selected node",
			pods: []v1.Pod{
				newPod("default", "p1", SchedulerName, nil),
			},
			nodes: []v1.Node{
				newNode("node-z", map[string]string{DefaultLabelKey: "zeta"}),
				newNode("node-a", map[string]string{DefaultLabelKey: "alpha"}),
			},
			wantStatuses:   []ResultStatus{StatusScheduled},
			wantNodeByPod:  map[string]string{"p1": "node-a"},
			wantBindCalled: true,
		},
		{
			name: "supports pod label override for ranking key",
			pods: []v1.Pod{
				newPod("default", "override", SchedulerName, nil, map[string]string{PodLabelOverrideKey: "custom.label"}),
			},
			nodes: []v1.Node{
				newNode("node-default-better", map[string]string{
					DefaultLabelKey: "alpha",
					"custom.label":  "zeta",
				}),
				newNode("node-override-better", map[string]string{
					DefaultLabelKey: "zeta",
					"custom.label":  "alpha",
				}),
			},
			wantStatuses:   []ResultStatus{StatusScheduled},
			wantNodeByPod:  map[string]string{"override": "node-override-better"},
			wantBindCalled: true,
		},
		{
			name: "returns bind_failed when binding fails",
			pods: []v1.Pod{
				newPod("default", "p1", SchedulerName, nil),
			},
			nodes: []v1.Node{
				newNode("node-a", map[string]string{DefaultLabelKey: "alpha"}),
			},
			reactors: []func(*fake.Clientset){
				func(client *fake.Clientset) {
					client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
						createAction, ok := action.(k8stesting.CreateAction)
						if !ok || createAction.GetSubresource() != "binding" {
							return false, nil, nil
						}
						return true, nil, errors.New("bind boom")
					})
				},
			},
			wantStatuses:   []ResultStatus{StatusBindFailed},
			wantNodeByPod:  map[string]string{"p1": "node-a"},
			wantBindCalled: true,
		},
		{
			name: "returns top-level error when listing pods fails",
			reactors: []func(*fake.Clientset){
				func(client *fake.Clientset) {
					client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, errors.New("pods unavailable")
					})
				},
			},
			wantErr: "list pods: pods unavailable",
		},
		{
			name: "returns top-level error when listing nodes fails",
			pods: []v1.Pod{
				newPod("default", "p1", SchedulerName, nil),
			},
			reactors: []func(*fake.Clientset){
				func(client *fake.Clientset) {
					client.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, errors.New("nodes unavailable")
					})
				},
			},
			wantErr: "list nodes: nodes unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(tt.pods)+len(tt.nodes))
			for i := range tt.pods {
				pod := tt.pods[i]
				objects = append(objects, &pod)
			}
			for i := range tt.nodes {
				node := tt.nodes[i]
				objects = append(objects, &node)
			}

			client := fake.NewSimpleClientset(objects...)
			for _, reactor := range tt.reactors {
				reactor(client)
			}

			results, err := RunOnce(context.Background(), client)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				require.Nil(t, results)
				return
			}

			require.NoError(t, err)
			require.Len(t, results, len(tt.wantStatuses))
			for i, got := range results {
				require.Equal(t, tt.wantStatuses[i], got.Status)
				if wantNode, ok := tt.wantNodeByPod[got.PodName]; ok {
					require.Equal(t, wantNode, got.NodeName)
				}
			}

			bindCalls := 0
			for _, action := range client.Actions() {
				if action.GetVerb() == "create" && action.GetResource().Resource == "pods" && action.GetSubresource() == "binding" {
					bindCalls++
				}
			}
			if tt.wantBindCalled {
				require.Greater(t, bindCalls, 0)
			} else {
				require.Equal(t, 0, bindCalls)
			}
		})
	}
}

func newPod(namespace, name, scheduler string, deletionTimestamp *metav1.Time, labels ...map[string]string) v1.Pod {
	podLabels := map[string]string{}
	if len(labels) > 0 {
		podLabels = labels[0]
	}

	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			Labels:            podLabels,
			DeletionTimestamp: deletionTimestamp,
		},
		Spec: v1.PodSpec{
			SchedulerName: scheduler,
		},
	}
}

func newNode(name string, labels map[string]string) v1.Node {
	return v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}
