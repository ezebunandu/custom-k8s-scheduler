package schedulercore

import (
	"context"
	"fmt"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	SchedulerName       = "alphabetical-scheduler"
	DefaultLabelKey     = "topology.kubernetes.io/zone"
	PodLabelOverrideKey = "scheduler.io/rank-by-label"
)

type ResultStatus string

const (
	StatusSkipped    ResultStatus = "skipped"
	StatusNoNode     ResultStatus = "no_node"
	StatusBindFailed ResultStatus = "bind_failed"
	StatusScheduled  ResultStatus = "scheduled"
)

type Result struct {
	Namespace string
	PodName   string
	NodeName  string
	Status    ResultStatus
	Err       error
}

func RunOnce(ctx context.Context, client kubernetes.Interface) ([]Result, error) {
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=",
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	results := make([]Result, 0, len(pods.Items))
	for i := range pods.Items {
		result := schedulePod(ctx, client, &pods.Items[i], nodes.Items)
		if result.Status != StatusSkipped {
			results = append(results, result)
		}
	}

	return results, nil
}

func schedulePod(ctx context.Context, client kubernetes.Interface, pod *v1.Pod, nodes []v1.Node) Result {
	if pod.Spec.SchedulerName != SchedulerName || pod.DeletionTimestamp != nil {
		return Result{Status: StatusSkipped}
	}

	nodeName := selectNode(nodes, pod)
	if nodeName == "" {
		return Result{
			Namespace: pod.Namespace,
			PodName:   pod.Name,
			Status:    StatusNoNode,
		}
	}

	if err := bind(ctx, client, pod, nodeName); err != nil {
		return Result{
			Namespace: pod.Namespace,
			PodName:   pod.Name,
			NodeName:  nodeName,
			Status:    StatusBindFailed,
			Err:       err,
		}
	}

	return Result{
		Namespace: pod.Namespace,
		PodName:   pod.Name,
		NodeName:  nodeName,
		Status:    StatusScheduled,
	}
}

func selectNode(nodes []v1.Node, pod *v1.Pod) string {
	labelKey := DefaultLabelKey
	if override, ok := pod.Labels[PodLabelOverrideKey]; ok && override != "" {
		labelKey = override
	}

	type candidate struct {
		name  string
		score int64
	}

	candidates := make([]candidate, 0, len(nodes))
	for _, node := range nodes {
		candidates = append(candidates, candidate{
			name:  node.Name,
			score: computeScore(node.Labels[labelKey]),
		})
	}

	if len(candidates) == 0 {
		return ""
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	return candidates[0].name
}

func computeScore(labelVal string) int64 {
	if len(labelVal) == 0 {
		return 0
	}

	first := strings.ToLower(labelVal)[0]
	if first < 'a' || first > 'z' {
		return 0
	}

	score := int64(100) - int64(first-'a')*4
	if score < 0 {
		return 0
	}
	return score
}

func bind(ctx context.Context, client kubernetes.Interface, pod *v1.Pod, nodeName string) error {
	binding := &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		Target: v1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       nodeName,
		},
	}

	return client.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
}
