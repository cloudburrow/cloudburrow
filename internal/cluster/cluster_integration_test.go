//go:build integration

package cluster

// Integration tests that create a real kind cluster. They need Docker, kind and
// kubectl, and are excluded from `make check`:
//
//	make test-integration
//
// Every test uses a unique instance name and always cleans up, so a developer's
// own clusters are never touched.

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func uniqueCluster(t *testing.T) *Cluster {
	t.Helper()
	for _, bin := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	name := fmt.Sprintf("cloudburrow-it-%d", time.Now().UnixNano()%1e9)
	c, err := New(Options{
		Name:       name,
		NodeImage:  "kindest/node:v1.36.4",
		Kubeconfig: filepath.Join(t.TempDir(), "kubeconfig"),
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = c.Delete(ctx)
	})
	return c
}

// The full owned lifecycle against a real cluster: create, ready, stop, start,
// delete — with the developer's environment left untouched throughout.
func TestClusterLifecycle(t *testing.T) {
	c := uniqueCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	if err := c.Create(ctx, ""); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if err := c.WaitReady(ctx, 5*time.Minute); err != nil {
		t.Fatalf("WaitReady() = %v", err)
	}

	version, err := c.ServerVersion(ctx)
	if err != nil {
		t.Fatalf("ServerVersion() = %v", err)
	}
	if !strings.HasPrefix(version, "v1.36.") {
		t.Errorf("ServerVersion() = %q, want the pinned v1.36.x", version)
	}
	t.Logf("cluster %s running Kubernetes %s", c.Name(), version)

	if got, _ := c.Status(ctx); got != StatusRunning {
		t.Fatalf("Status() = %v, want running", got)
	}

	// Creating again must be idempotent, not a recreate.
	if err := c.Create(ctx, ""); err != nil {
		t.Fatalf("second Create() = %v", err)
	}

	// A native workload proves the cluster is usable without any Google API.
	kubectl := func(args ...string) (string, error) {
		full := append([]string{"--kubeconfig", c.KubeconfigPath()}, args...)
		out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
		return string(out), err
	}
	if out, err := kubectl("create", "deployment", "probe", "--image=registry.k8s.io/pause:3.10"); err != nil {
		t.Fatalf("create deployment: %v (%s)", err, out)
	}
	if out, err := kubectl("wait", "--for=condition=Available", "deploy/probe", "--timeout=180s"); err != nil {
		t.Fatalf("wait for deployment: %v (%s)", err, out)
	}

	// Stop must preserve the cluster and its workloads.
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if got, _ := c.Status(ctx); got != StatusStopped {
		t.Errorf("Status() after Stop = %v, want stopped", got)
	}
	exists, err := c.Exists(ctx)
	if err != nil || !exists {
		t.Fatalf("Stop() destroyed the cluster; exists=%v err=%v", exists, err)
	}

	// Starting again must restore the workload, proving it was not recreated.
	if err := c.Create(ctx, ""); err != nil {
		t.Fatalf("Create() after Stop = %v", err)
	}
	if err := c.WaitReady(ctx, 5*time.Minute); err != nil {
		t.Fatalf("WaitReady() after restart = %v", err)
	}
	if out, err := kubectl("get", "deploy", "probe"); err != nil {
		t.Errorf("workload did not survive stop/start, so the cluster was recreated: %v (%s)", err, out)
	}

	if err := c.Delete(ctx); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if exists, _ := c.Exists(ctx); exists {
		t.Error("cluster still exists after Delete()")
	}
}

// Reset must refuse a namespace that does not carry our ownership label, and
// proceed only for one that does.
func TestDeleteNamespaceOwnershipGuard(t *testing.T) {
	c := uniqueCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	if err := c.Create(ctx, ""); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if err := c.WaitReady(ctx, 5*time.Minute); err != nil {
		t.Fatalf("WaitReady() = %v", err)
	}
	kubectl := func(args ...string) (string, error) {
		full := append([]string{"--kubeconfig", c.KubeconfigPath()}, args...)
		out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
		return string(out), err
	}

	// Protected namespaces are refused outright.
	for _, ns := range []string{"default", "kube-system", ""} {
		if err := c.DeleteNamespace(ctx, ns); err == nil {
			t.Errorf("DeleteNamespace(%q) = nil, want refusal", ns)
		}
	}

	// An unlabelled namespace is refused and survives.
	if out, err := kubectl("create", "namespace", "not-ours"); err != nil {
		t.Fatalf("create namespace: %v (%s)", err, out)
	}
	if err := c.DeleteNamespace(ctx, "not-ours"); err == nil {
		t.Error("DeleteNamespace() deleted a namespace without the ownership label")
	}
	if out, err := kubectl("get", "ns", "not-ours"); err != nil {
		t.Errorf("unowned namespace was removed: %v (%s)", err, out)
	}

	// A labelled namespace is removed.
	if out, err := kubectl("create", "namespace", "ours"); err != nil {
		t.Fatalf("create namespace: %v (%s)", err, out)
	}
	if out, err := kubectl("label", "ns", "ours", "cloudburrow.dev/owned=true"); err != nil {
		t.Fatalf("label namespace: %v (%s)", err, out)
	}
	if err := c.DeleteNamespace(ctx, "ours"); err != nil {
		t.Fatalf("DeleteNamespace(ours) = %v", err)
	}
	if _, err := kubectl("get", "ns", "ours"); err == nil {
		t.Error("owned namespace still present after DeleteNamespace()")
	}
}
