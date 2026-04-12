//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	dockerBuildTimeout   = 4 * time.Minute
	kindImageLoadTimeout = 2 * time.Minute
	kindKubeCfgTimeout   = 30 * time.Second
)

// projectRoot returns the absolute path to the repository root by walking up
// from this test file's location.
func projectRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile is .../test/e2e/helpers_test.go — walk up two directories
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// buildAndLoadSchedulerImage builds the scheduler binary, builds the Docker image,
// and loads it into the named kind cluster.
func buildAndLoadSchedulerImage(t *testing.T, clusterName string) {
	t.Helper()

	root := projectRoot()

	runCommandWithTimeout(t, root, dockerBuildTimeout, "docker", "build", "-t", "custom-scheduler:e2e", ".")
	runCommandWithTimeout(t, "", kindImageLoadTimeout, "kind", "load", "docker-image", "custom-scheduler:e2e", "--name", clusterName)
}

// getKubeClient retrieves the kubeconfig for the named kind cluster and returns
// a configured Kubernetes clientset.
func getKubeClient(t *testing.T, clusterName string) *kubernetes.Clientset {
	t.Helper()

	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), boundedTimeout(t, kindKubeCfgTimeout))
	defer cancel()

	cmd := exec.CommandContext(ctx, "kind", "get", "kubeconfig", "--name", clusterName)
	cmd.Stdout = &buf
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("kind get kubeconfig timed out after %s", boundedTimeout(t, kindKubeCfgTimeout))
		}
		t.Fatalf("kind get kubeconfig failed: %v", err)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(buf.Bytes())
	if err != nil {
		t.Fatalf("failed to parse kubeconfig: %v", err)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create kubernetes client: %v", err)
	}
	return client
}

// waitForPodRunning polls until the specified pod reaches Running phase or the
// timeout expires.
func waitForPodRunning(t *testing.T, client *kubernetes.Clientset, namespace, podName string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pod, err := client.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == v1.PodRunning {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("pod %s/%s did not reach Running phase within %s", namespace, podName, timeout)
}

// waitForDeploymentReady polls until all replicas of the named deployment are
// available or the timeout expires.
func waitForDeploymentReady(t *testing.T, client *kubernetes.Clientset, namespace, name string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dep, err := client.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil && dep.Status.ReadyReplicas >= 1 {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("deployment %s/%s did not become ready within %s", namespace, name, timeout)
}

// getWorkerNodes returns all nodes that do not carry the control-plane role label.
func getWorkerNodes(t *testing.T, client *kubernetes.Clientset) []v1.Node {
	t.Helper()

	nodeList, err := client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("failed to list nodes: %v", err)
	}

	var workers []v1.Node
	for _, node := range nodeList.Items {
		if _, isCP := node.Labels["node-role.kubernetes.io/control-plane"]; !isCP {
			workers = append(workers, node)
		}
	}
	return workers
}

func runCommandWithTimeout(t *testing.T, dir string, timeout time.Duration, command string, args ...string) {
	t.Helper()

	effectiveTimeout := boundedTimeout(t, timeout)
	ctx, cancel := context.WithTimeout(context.Background(), effectiveTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("%s timed out after %s (args: %v)", command, effectiveTimeout, args)
		}
		t.Fatalf("%s failed: %v (args: %v)", command, err, args)
	}
}

func boundedTimeout(t *testing.T, want time.Duration) time.Duration {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return want
	}

	remaining := time.Until(deadline) - 5*time.Second
	if remaining <= 0 {
		return time.Second
	}
	return minDuration(want, remaining)
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= b {
		return a
	}
	return b
}
