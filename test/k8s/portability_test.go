//go:build integration

// Package k8s proves native Kubernetes and Helm portability (#29), separately
// from any GCP API.
//
// This dimension is tracked separately in the support matrix on purpose: a
// cluster that answers GCP calls is not automatically a cluster your manifests
// run on, and inferring one from the other is how a tool over-promises.
package k8s

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func kubeconfig(t *testing.T) string {
	t.Helper()
	kc := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_KUBECONFIG"))
	if kc == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is not set; start an instance first")
	}
	return kc
}

func kubectl(t *testing.T, ctx context.Context, stdin string, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--kubeconfig", kubeconfig(t)}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestOrdinaryManifestsApply covers the manifests most applications are made
// of, applied directly with kubectl and never through a Google API.
func TestOrdinaryManifestsApply(t *testing.T) {
	ctx := testCtx(t)
	ns := fmt.Sprintf("portability-%d", time.Now().UnixNano()%1e9)

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: %s
data:
  GREETING: hello
---
apiVersion: v1
kind: Secret
metadata:
  name: app-secret
  namespace: %s
stringData:
  token: s3cret
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: %s
spec:
  replicas: 2
  selector:
    matchLabels: { app: portability }
  template:
    metadata:
      labels: { app: portability }
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.10
          envFrom:
            - configMapRef: { name: app-config }
          env:
            - name: TOKEN
              valueFrom:
                secretKeyRef: { name: app-secret, key: token }
---
apiVersion: v1
kind: Service
metadata:
  name: app
  namespace: %s
spec:
  selector: { app: portability }
  ports: [{ port: 80, targetPort: 8080 }]
`, ns, ns, ns, ns, ns)

	if out, err := kubectl(t, ctx, manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, context.Background(), "", "delete", "namespace", ns, "--wait=false")
	})

	if out, err := kubectl(t, ctx, "", "-n", ns, "wait", "--for=condition=Available",
		"deployment/app", "--timeout=180s"); err != nil {
		diag, _ := kubectl(t, ctx, "", "-n", ns, "describe", "deployment", "app")
		t.Fatalf("deployment never became available: %v\n%s\n%s", err, out, diag)
	}

	// Two replicas must actually be scheduled, not merely declared.
	out, err := kubectl(t, ctx, "", "-n", ns, "get", "deployment", "app",
		"-o", "jsonpath={.status.readyReplicas}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "2" {
		t.Errorf("readyReplicas = %q, want 2", strings.TrimSpace(out))
	}
}

// TestPersistentVolumeClaimBindsAndPersists covers PVCs, which the matrix
// previously listed as Partial because nothing asserted that data survived.
func TestPersistentVolumeClaimBindsAndPersists(t *testing.T) {
	ctx := testCtx(t)
	ns := fmt.Sprintf("pvc-%d", time.Now().UnixNano()%1e9)

	base := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata: { name: %s }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: data, namespace: %s }
spec:
  accessModes: [ReadWriteOnce]
  resources: { requests: { storage: 64Mi } }
`, ns, ns)
	if out, err := kubectl(t, ctx, base, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, context.Background(), "", "delete", "namespace", ns, "--wait=false")
	})

	job := func(name, script string) string {
		return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata: { name: %s, namespace: %s }
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: w
          image: busybox:1.36
          command: ["sh","-c",%q]
          volumeMounts: [{ name: data, mountPath: /data }]
      volumes:
        - name: data
          persistentVolumeClaim: { claimName: data }
`, name, ns, script)
	}

	// Write, then read back from a *different* pod, which is what proves the
	// volume persisted rather than the data living in one container.
	if out, err := kubectl(t, ctx, job("writer", "echo persisted > /data/file"), "apply", "-f", "-"); err != nil {
		t.Fatalf("writer: %v\n%s", err, out)
	}
	if out, err := kubectl(t, ctx, "", "-n", ns, "wait", "--for=condition=Complete",
		"job/writer", "--timeout=180s"); err != nil {
		t.Fatalf("writer job: %v\n%s", err, out)
	}

	if out, err := kubectl(t, ctx, job("reader", "cat /data/file"), "apply", "-f", "-"); err != nil {
		t.Fatalf("reader: %v\n%s", err, out)
	}
	if out, err := kubectl(t, ctx, "", "-n", ns, "wait", "--for=condition=Complete",
		"job/reader", "--timeout=180s"); err != nil {
		t.Fatalf("reader job: %v\n%s", err, out)
	}
	logs, err := kubectl(t, ctx, "", "-n", ns, "logs", "job/reader")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "persisted") {
		t.Errorf("data did not survive across pods; reader saw %q", strings.TrimSpace(logs))
	}
}

// TestCustomResourceDefinition covers the operator pattern: a CRD plus an
// instance of it.
func TestCustomResourceDefinition(t *testing.T) {
	ctx := testCtx(t)
	crd := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.cloudburrow.test
spec:
  group: cloudburrow.test
  scope: Namespaced
  names:
    plural: widgets
    singular: widget
    kind: Widget
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                size: { type: integer }
`
	if out, err := kubectl(t, ctx, crd, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply CRD: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, context.Background(), "", "delete", "crd", "widgets.cloudburrow.test", "--wait=false")
	})

	if out, err := kubectl(t, ctx, "", "wait", "--for=condition=Established",
		"crd/widgets.cloudburrow.test", "--timeout=60s"); err != nil {
		t.Fatalf("CRD never established: %v\n%s", err, out)
	}

	instance := `apiVersion: cloudburrow.test/v1
kind: Widget
metadata: { name: one, namespace: default }
spec: { size: 3 }
`
	if out, err := kubectl(t, ctx, instance, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply custom resource: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, context.Background(), "", "delete", "widget", "one", "-n", "default", "--ignore-not-found")
	})

	out, err := kubectl(t, ctx, "", "get", "widget", "one", "-n", "default", "-o", "jsonpath={.spec.size}")
	if err != nil {
		t.Fatalf("read custom resource: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "3" {
		t.Errorf("spec.size = %q, want 3", strings.TrimSpace(out))
	}
}

// TestHelmChartInstalls covers Helm, skipping when it is not installed rather
// than pretending the dimension is covered.
func TestHelmChartInstalls(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	ctx := testCtx(t)
	kc := kubeconfig(t)
	ns := fmt.Sprintf("helm-%d", time.Now().UnixNano()%1e9)
	dir := t.TempDir()

	// A minimal chart, written here so the test does not depend on a remote
	// repository being reachable.
	mustWrite(t, dir+"/Chart.yaml", `apiVersion: v2
name: portability
version: 0.1.0
appVersion: "1"
`)
	mustWrite(t, dir+"/values.yaml", "replicaCount: 1\n")
	_ = os.MkdirAll(dir+"/templates", 0o755)
	mustWrite(t, dir+"/templates/deployment.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels: { app: {{ .Release.Name }} }
  template:
    metadata:
      labels: { app: {{ .Release.Name }} }
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.10
`)

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "helm", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kc)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run("install", "portability", dir, "--namespace", ns,
		"--create-namespace", "--wait", "--timeout", "3m"); err != nil {
		t.Fatalf("helm install: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = run("uninstall", "portability", "--namespace", ns) })

	out, err := run("status", "portability", "--namespace", ns)
	if err != nil {
		t.Fatalf("helm status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "deployed") {
		t.Errorf("release is not deployed:\n%s", out)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
