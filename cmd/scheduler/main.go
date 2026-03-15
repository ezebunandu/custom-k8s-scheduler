package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	schedulerName       = "alphabetical-scheduler"
	defaultLabelKey     = "topology.kubernetes.io/zone"
	podLabelOverrideKey = "scheduler.io/rank-by-label"
	pollInterval        = 2 * time.Second
)

type podResultStatus string

const (
	statusSkipped    podResultStatus = "skipped"
	statusNoNode     podResultStatus = "no_node"
	statusBindFailed podResultStatus = "bind_failed"
	statusScheduled  podResultStatus = "scheduled"
)

type podResult struct {
	namespace string
	name      string
	nodeName  string
	status    podResultStatus
	err       error
}

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			log.Fatalf("failed to build kubeconfig: %v", err)
		}
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("failed to create kubernetes client: %v", err)
	}

	log.Printf("alphabetical-scheduler started")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	wait.Until(func() {
		results, err := scheduleOnce(ctx, client)
		if err != nil {
			log.Printf("scheduler error: %v", err)
		}
		for _, r := range results {
			switch r.status {
			case statusNoNode:
				log.Printf("no suitable node for pod %s/%s", r.namespace, r.name)
			case statusBindFailed:
				log.Printf("bind %s/%s -> %s: %v", r.namespace, r.name, r.nodeName, r.err)
			case statusScheduled:
				log.Printf("scheduled %s/%s -> %s", r.namespace, r.name, r.nodeName)
			}
		}
	}, pollInterval, ctx.Done())

	log.Printf("alphabetical-scheduler stopped")
}

func scheduleOnce(ctx context.Context, client kubernetes.Interface) ([]podResult, error) {
	// List unscheduled pods that want our scheduler.
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

	results := make([]podResult, 0, len(pods.Items))
	for i := range pods.Items {
		r := schedulePod(ctx, client, &pods.Items[i], nodes.Items)
		if r.status != statusSkipped {
			results = append(results, r)
		}
	}
	return results, nil
}

func schedulePod(ctx context.Context, client kubernetes.Interface, pod *v1.Pod, nodes []v1.Node) podResult {
	if pod.Spec.SchedulerName != schedulerName || pod.DeletionTimestamp != nil {
		return podResult{status: statusSkipped}
	}

	nodeName := selectNode(nodes, pod)
	if nodeName == "" {
		return podResult{
			namespace: pod.Namespace,
			name:      pod.Name,
			status:    statusNoNode,
		}
	}

	if err := bind(ctx, client, pod, nodeName); err != nil {
		return podResult{
			namespace: pod.Namespace,
			name:      pod.Name,
			nodeName:  nodeName,
			status:    statusBindFailed,
			err:       err,
		}
	}

	return podResult{
		namespace: pod.Namespace,
		name:      pod.Name,
		nodeName:  nodeName,
		status:    statusScheduled,
	}
}

func selectNode(nodes []v1.Node, pod *v1.Pod) string {
	labelKey := defaultLabelKey
	if override, ok := pod.Labels[podLabelOverrideKey]; ok && override != "" {
		labelKey = override
	}

	type candidate struct {
		name  string
		score int64
	}
	var cs []candidate
	for _, node := range nodes {
		cs = append(cs, candidate{node.Name, computeScore(node.Labels[labelKey])})
	}
	if len(cs) == 0 {
		return ""
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
	return cs[0].name
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
