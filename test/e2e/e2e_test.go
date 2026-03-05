//go:build e2e

// Package e2e contains end-to-end tests that spin up kind clusters and verify
// the full proxy pipeline.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	mgmtCluster     = "proxy-e2e-mgmt"
	workloadCluster  = "proxy-e2e-workload"
	serverImage      = "argocd-cluster-proxy-server:e2e"
	agentImage       = "argocd-cluster-proxy-agent:e2e"
	namespace        = "proxy-system"
	testToken        = "e2e-test-token-1234"
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "setup failed: %v\n", err)
		teardown()
		os.Exit(1)
	}
	code := m.Run()
	teardown()
	os.Exit(code)
}

func setup() error {
	fmt.Println("=== Building binaries and images ===")
	if err := buildImages(); err != nil {
		return fmt.Errorf("build images: %w", err)
	}

	fmt.Println("=== Creating kind clusters ===")
	if err := createClusters(); err != nil {
		return fmt.Errorf("create clusters: %w", err)
	}

	fmt.Println("=== Loading images into kind ===")
	if err := loadImages(); err != nil {
		return fmt.Errorf("load images: %w", err)
	}

	fmt.Println("=== Deploying server to management cluster ===")
	if err := deployServer(); err != nil {
		return fmt.Errorf("deploy server: %w", err)
	}

	fmt.Println("=== Deploying agent to workload cluster ===")
	if err := deployAgent(); err != nil {
		return fmt.Errorf("deploy agent: %w", err)
	}

	fmt.Println("=== Waiting for pods to be ready ===")
	if err := waitForReady(); err != nil {
		return fmt.Errorf("wait ready: %w", err)
	}

	return nil
}

func teardown() {
	fmt.Println("=== Tearing down ===")
	_ = run("kind", "delete", "cluster", "--name", mgmtCluster)
	_ = run("kind", "delete", "cluster", "--name", workloadCluster)
}

// ---------- Test cases ----------

func TestProxyAPIAccess(t *testing.T) {
	// Port-forward the proxy server and make a request through it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start port-forward in background.
	pf := exec.CommandContext(ctx, "kubectl", "--context", "kind-"+mgmtCluster,
		"-n", namespace, "port-forward", "svc/proxy-server", "18080:8080")
	pf.Stdout = os.Stdout
	pf.Stderr = os.Stderr
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()

	// Wait for port-forward to be ready.
	time.Sleep(3 * time.Second)

	// Request the workload cluster's API through the proxy.
	out, err := runOutput("curl", "-sf", "--max-time", "10",
		"http://127.0.0.1:18080/tunnel/workload/api/v1/namespaces")
	if err != nil {
		// Dump logs for debugging.
		serverLogs, _ := runOutput("kubectl", "--context", "kind-"+mgmtCluster,
			"-n", namespace, "logs", "-l", "app=proxy-server", "--tail=50")
		agentLogs, _ := runOutput("kubectl", "--context", "kind-"+workloadCluster,
			"-n", namespace, "logs", "-l", "app=proxy-agent", "--tail=50")
		t.Logf("server logs:\n%s", serverLogs)
		t.Logf("agent logs:\n%s", agentLogs)
		t.Fatalf("proxy request failed: %v\noutput: %s", err, out)
	}

	// Verify we got a valid NamespaceList.
	var result struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parse response: %v\nbody: %s", err, out)
	}
	if result.Kind != "NamespaceList" {
		t.Errorf("unexpected kind: got %q, want NamespaceList", result.Kind)
	}
}

func TestHealthEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pf := exec.CommandContext(ctx, "kubectl", "--context", "kind-"+mgmtCluster,
		"-n", namespace, "port-forward", "svc/proxy-server", "18081:8080")
	pf.Stdout = os.Stdout
	pf.Stderr = os.Stderr
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(3 * time.Second)

	out, err := runOutput("curl", "-sf", "--max-time", "5", "http://127.0.0.1:18081/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("healthz: got %q, want ok", out)
	}
}

func TestReadyEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pf := exec.CommandContext(ctx, "kubectl", "--context", "kind-"+mgmtCluster,
		"-n", namespace, "port-forward", "svc/proxy-server", "18082:8080")
	pf.Stdout = os.Stdout
	pf.Stderr = os.Stderr
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(3 * time.Second)

	out, err := runOutput("curl", "-sf", "--max-time", "5", "http://127.0.0.1:18082/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	if strings.TrimSpace(out) != "ok" {
		t.Errorf("readyz: got %q, want ok", out)
	}
}

// ---------- Helpers ----------

func buildImages() error {
	root := projectRoot()
	if err := runDir(root, "docker", "build", "-t", serverImage, "-f", "Dockerfile.server", "."); err != nil {
		return err
	}
	return runDir(root, "docker", "build", "-t", agentImage, "-f", "Dockerfile.agent", ".")
}

func createClusters() error {
	// Both clusters must share a Docker network so the agent can reach the
	// management cluster's NodePort. Kind defaults to the "kind" network
	// but we pass it explicitly to be safe across kind versions.
	if err := run("kind", "create", "cluster", "--name", mgmtCluster, "--wait", "60s"); err != nil {
		return err
	}
	return run("kind", "create", "cluster", "--name", workloadCluster, "--wait", "60s")
}

func loadImages() error {
	if err := run("kind", "load", "docker-image", serverImage, "--name", mgmtCluster); err != nil {
		return err
	}
	return run("kind", "load", "docker-image", agentImage, "--name", workloadCluster)
}

func deployServer() error {
	manifests := fmt.Sprintf(`---
apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: proxy-clusters
  namespace: %s
data:
  clusters.json: |
    [{"id": "workload", "token": "%s", "targetAddr": "kubernetes.default.svc:443"}]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: proxy-server
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: proxy-server
  template:
    metadata:
      labels:
        app: proxy-server
    spec:
      containers:
      - name: server
        image: %s
        imagePullPolicy: Never
        args:
        - -addr=:8080
        - -clusters=/config/clusters.json
        - -log-level=debug
        ports:
        - containerPort: 8080
        volumeMounts:
        - name: config
          mountPath: /config
        readinessProbe:
          httpGet:
            path: /healthz
            port: 8080
          initialDelaySeconds: 2
          periodSeconds: 3
      volumes:
      - name: config
        configMap:
          name: proxy-clusters
---
apiVersion: v1
kind: Service
metadata:
  name: proxy-server
  namespace: %s
spec:
  selector:
    app: proxy-server
  ports:
  - port: 8080
    targetPort: 8080
`, namespace, namespace, testToken, namespace, serverImage, namespace)

	return applyManifest("kind-"+mgmtCluster, manifests)
}

func deployAgent() error {
	// Get the management cluster's API server address accessible from the
	// workload cluster. Since both are kind clusters on the same Docker
	// network, we use the management cluster's control plane container IP.
	// Use the "kind" network IP specifically. The range template would
	// concatenate all network IPs without a separator if the container is
	// on multiple networks.
	mgmtIP, err := runOutput("docker", "inspect", "-f",
		"{{(index .NetworkSettings.Networks \"kind\").IPAddress}}",
		mgmtCluster+"-control-plane")
	if err != nil {
		return fmt.Errorf("get mgmt IP: %w", err)
	}
	mgmtIP = strings.TrimSpace(mgmtIP)

	// Get the server NodePort or use the proxy-server service.
	// Since clusters are on the same docker network, the agent can reach
	// the mgmt cluster pod IPs directly. But we need the proxy server's
	// ClusterIP, which is only accessible from within the mgmt cluster.
	// Instead, use a NodePort or the control-plane IP with port-forward.
	// Simplest: create the proxy-server as a NodePort and use mgmt control-plane IP.

	// Patch server service to NodePort.
	_ = run("kubectl", "--context", "kind-"+mgmtCluster, "-n", namespace,
		"patch", "svc", "proxy-server", "-p",
		`{"spec":{"type":"NodePort","ports":[{"port":8080,"targetPort":8080,"nodePort":30080}]}}`)

	// The agent needs a ServiceAccount with permissions to read the local
	// K8s API. For testing, we'll use the default SA and pass --insecure.
	manifests := fmt.Sprintf(`---
apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: proxy-agent
  namespace: %s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: proxy-agent-admin
subjects:
- kind: ServiceAccount
  name: proxy-agent
  namespace: %s
roleRef:
  kind: ClusterRole
  name: cluster-admin
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: proxy-agent
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: proxy-agent
  template:
    metadata:
      labels:
        app: proxy-agent
    spec:
      serviceAccountName: proxy-agent
      containers:
      - name: agent
        image: %s
        imagePullPolicy: Never
        args:
        - -server=ws://%s:30080/connect
        - -token=%s
        - -target=kubernetes.default.svc:443
        - -allow-insecure-server
        - -log-level=debug
`, namespace, namespace, namespace, namespace, agentImage, mgmtIP, testToken)

	return applyManifest("kind-"+workloadCluster, manifests)
}

func waitForReady() error {
	// Wait for server pod.
	if err := run("kubectl", "--context", "kind-"+mgmtCluster, "-n", namespace,
		"wait", "--for=condition=available", "deployment/proxy-server", "--timeout=60s"); err != nil {
		return fmt.Errorf("server not ready: %w", err)
	}

	// Wait for agent pod.
	if err := run("kubectl", "--context", "kind-"+workloadCluster, "-n", namespace,
		"wait", "--for=condition=available", "deployment/proxy-agent", "--timeout=60s"); err != nil {
		return fmt.Errorf("agent not ready: %w", err)
	}

	// Wait for agent to connect. The server container is distroless (no
	// shell/wget), so we port-forward and check /readyz from the test host.
	pfCtx, pfCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer pfCancel()

	pf := exec.CommandContext(pfCtx, "kubectl", "--context", "kind-"+mgmtCluster,
		"-n", namespace, "port-forward", "svc/proxy-server", "18090:8080")
	pf.Stdout = os.Stdout
	pf.Stderr = os.Stderr
	if err := pf.Start(); err != nil {
		return fmt.Errorf("port-forward: %w", err)
	}
	defer func() { _ = pf.Process.Kill() }()

	// Wait for port-forward to establish.
	time.Sleep(2 * time.Second)

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		out, err := runOutput("curl", "-sf", "--max-time", "2", "http://127.0.0.1:18090/readyz")
		if err == nil && strings.TrimSpace(out) == "ok" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	// Dump logs for debugging on failure.
	serverLogs, _ := runOutput("kubectl", "--context", "kind-"+mgmtCluster,
		"-n", namespace, "logs", "-l", "app=proxy-server", "--tail=50")
	agentLogs, _ := runOutput("kubectl", "--context", "kind-"+workloadCluster,
		"-n", namespace, "logs", "-l", "app=proxy-agent", "--tail=50")
	return fmt.Errorf("agent did not connect within timeout\nserver logs:\n%s\nagent logs:\n%s",
		serverLogs, agentLogs)
}

func applyManifest(kubeContext, manifest string) error {
	cmd := exec.Command("kubectl", "--context", kubeContext, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runDir(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func projectRoot() string {
	// Walk up from test/e2e/ to find go.mod.
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}
