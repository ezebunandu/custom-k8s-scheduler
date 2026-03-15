//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/kind/pkg/cluster"
)

const (
	schedulerImage     = "custom-scheduler:e2e"
	schedulerName      = "alphabetical-scheduler"
	schedulerNamespace = "kube-system"
	schedulerSAName    = "custom-scheduler"
	schedulerCRName    = "custom-scheduler"
	schedulerCRBName   = "custom-scheduler"
	schedulerDepName   = "custom-scheduler"
)

// kindClusterConfig is the multi-node kind cluster configuration.
const kindClusterConfig = `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
`

// createCluster creates a new kind cluster and registers t.Cleanup to delete it.
// It returns the cluster name.
func createCluster(t *testing.T) string {
	t.Helper()

	clusterName := fmt.Sprintf("alphabetical-e2e-%d", time.Now().Unix())

	provider := cluster.NewProvider()
	if err := provider.Create(
		clusterName,
		cluster.CreateWithRawConfig([]byte(kindClusterConfig)),
	); err != nil {
		t.Fatalf("failed to create kind cluster %q: %v", clusterName, err)
	}

	t.Cleanup(func() {
		if err := provider.Delete(clusterName, ""); err != nil {
			t.Logf("warning: failed to delete kind cluster %q: %v", clusterName, err)
		}
	})

	return clusterName
}

// deployScheduler creates all Kubernetes resources required to run the custom
// scheduler inside the cluster.
func deployScheduler(t *testing.T, client *kubernetes.Clientset) {
	t.Helper()
	ctx := context.Background()

	// ServiceAccount
	sa := &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      schedulerSAName,
			Namespace: schedulerNamespace,
		},
	}
	if _, err := client.CoreV1().ServiceAccounts(schedulerNamespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ServiceAccount: %v", err)
	}

	// ClusterRole
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: schedulerCRName},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods", "nodes", "events", "endpoints"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
			{
				// Required for the Bind API call: POST /api/v1/namespaces/{ns}/pods/{name}/binding
				APIGroups: []string{""},
				Resources: []string{"pods/binding", "pods/status"},
				Verbs:     []string{"create", "update", "patch"},
			},
			{
				APIGroups: []string{"coordination.k8s.io"},
				Resources: []string{"leases"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
		},
	}
	if _, err := client.RbacV1().ClusterRoles().Create(ctx, cr, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ClusterRole: %v", err)
	}

	// ClusterRoleBinding
	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: schedulerCRBName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     schedulerCRName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      schedulerSAName,
				Namespace: schedulerNamespace,
			},
		},
	}
	if _, err := client.RbacV1().ClusterRoleBindings().Create(ctx, crb, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ClusterRoleBinding: %v", err)
	}

	// Deployment — the scheduler binary is self-contained; no config file needed.
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      schedulerDepName,
			Namespace: schedulerNamespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "custom-scheduler"},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "custom-scheduler"},
				},
				Spec: v1.PodSpec{
					ServiceAccountName: schedulerSAName,
					Containers: []v1.Container{
						{
							Name:            "scheduler",
							Image:           schedulerImage,
							ImagePullPolicy: v1.PullNever,
						},
					},
				},
			},
		},
	}
	if _, err := client.AppsV1().Deployments(schedulerNamespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
}

// labelWorkers applies a label key=value to the worker at index i.
func labelWorker(t *testing.T, client *kubernetes.Clientset, node v1.Node, key, value string) {
	t.Helper()
	ctx := context.Background()

	nodeCopy := node.DeepCopy()
	if nodeCopy.Labels == nil {
		nodeCopy.Labels = map[string]string{}
	}
	nodeCopy.Labels[key] = value

	if _, err := client.CoreV1().Nodes().Update(ctx, nodeCopy, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("label node %s: %v", node.Name, err)
	}
}

// createTestPod creates a pod that uses the custom scheduler.
func createTestPod(t *testing.T, client *kubernetes.Clientset, namespace, name string, extraLabels map[string]string) *v1.Pod {
	t.Helper()
	ctx := context.Background()

	labels := map[string]string{"test": name}
	for k, v := range extraLabels {
		labels[k] = v
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: v1.PodSpec{
			SchedulerName: schedulerName,
			Containers: []v1.Container{
				{
					Name:  "pause",
					Image: "registry.k8s.io/pause:3.9",
				},
			},
		},
	}

	created, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod %s/%s: %v", namespace, name, err)
	}
	return created
}

// getPodNodeName waits briefly for a pod to be scheduled and returns its node.
func getPodNodeName(t *testing.T, client *kubernetes.Clientset, namespace, podName string, timeout time.Duration) string {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err == nil && pod.Spec.NodeName != "" {
			return pod.Spec.NodeName
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("pod %s/%s was not scheduled within %s", namespace, podName, timeout)
	return ""
}

// TestAlphabeticalNodeSelection verifies that when three workers are labelled
// alpha, bravo, and charlie the scheduler places the pod on the alpha node.
func TestAlphabeticalNodeSelection(t *testing.T) {
	clusterName := createCluster(t)
	buildAndLoadSchedulerImage(t, clusterName)
	client := getKubeClient(t, clusterName)

	workers := getWorkerNodes(t, client)
	if len(workers) < 3 {
		t.Fatalf("expected at least 3 worker nodes, got %d", len(workers))
	}

	zoneLabels := []string{"alpha", "bravo", "charlie"}
	for i, label := range zoneLabels {
		labelWorker(t, client, workers[i], "topology.kubernetes.io/zone", label)
	}

	deployScheduler(t, client)
	waitForDeploymentReady(t, client, schedulerNamespace, schedulerDepName, 3*time.Minute)

	pod := createTestPod(t, client, "default", "test-alphabetical", nil)
	waitForPodRunning(t, client, "default", pod.Name, 3*time.Minute)

	assignedNode := getPodNodeName(t, client, "default", pod.Name, time.Minute)

	// workers[0] was labelled "alpha" — we know this by assignment order above.
	alphaNodeName := workers[0].Name
	if assignedNode != alphaNodeName {
		t.Errorf("expected pod on node %q (alpha), got %q", alphaNodeName, assignedNode)
	}
}

// TestCustomLabelKeyOverride verifies that the pod-level label override causes
// the scheduler to sort by a different label key, selecting the node with
// the alphabetically earliest value for that key.
func TestCustomLabelKeyOverride(t *testing.T) {
	clusterName := createCluster(t)
	buildAndLoadSchedulerImage(t, clusterName)
	client := getKubeClient(t, clusterName)

	workers := getWorkerNodes(t, client)
	if len(workers) < 3 {
		t.Fatalf("expected at least 3 worker nodes, got %d", len(workers))
	}

	customLabels := []string{"zulu", "mango", "apple"}
	for i, label := range customLabels {
		labelWorker(t, client, workers[i], "custom-zone", label)
	}

	deployScheduler(t, client)
	waitForDeploymentReady(t, client, schedulerNamespace, schedulerDepName, 3*time.Minute)

	pod := createTestPod(t, client, "default", "test-custom-label", map[string]string{
		"scheduler.io/rank-by-label": "custom-zone",
	})
	waitForPodRunning(t, client, "default", pod.Name, 3*time.Minute)

	assignedNode := getPodNodeName(t, client, "default", pod.Name, time.Minute)

	// workers[2] was labelled "apple" — earliest alphabetically of ["zulu","mango","apple"].
	appleNodeName := workers[2].Name
	if assignedNode != appleNodeName {
		t.Errorf("expected pod on node %q (apple), got %q", appleNodeName, assignedNode)
	}
}

// TestAllNodesEqualLabel verifies that when all nodes have the same zone label
// value the pod still gets scheduled and reaches Running phase.
func TestAllNodesEqualLabel(t *testing.T) {
	clusterName := createCluster(t)
	buildAndLoadSchedulerImage(t, clusterName)
	client := getKubeClient(t, clusterName)

	workers := getWorkerNodes(t, client)
	if len(workers) < 3 {
		t.Fatalf("expected at least 3 worker nodes, got %d", len(workers))
	}

	for i := 0; i < 3; i++ {
		labelWorker(t, client, workers[i], "topology.kubernetes.io/zone", "bravo")
	}

	deployScheduler(t, client)
	waitForDeploymentReady(t, client, schedulerNamespace, schedulerDepName, 3*time.Minute)

	pod := createTestPod(t, client, "default", "test-equal-labels", nil)
	waitForPodRunning(t, client, "default", pod.Name, 3*time.Minute)
	// Reaching Running phase on any node is sufficient for this test.
}
