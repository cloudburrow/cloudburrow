//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

const hostProbeImage = "dev.local/cloudburrow-hostprobe:compat"

// TestAPodReachesTheCLIHostedServices (#575): a pod in the namespace Cloud
// Run workloads use reads a secret the host wrote and creates a task,
// through the official clients, at the cloudburrow-host addresses `status
// --format json` reports — never 127.0.0.1. On the CI runner that is Docker
// Engine on Linux, so it is the gateway relay that carries it. The host then
// sees the task in the same server.
func TestAPodReachesTheCLIHostedServices(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	sm := secretsClient(t, h)
	tc := tasksClient(t, h)
	cli := os.Getenv(EnvCLI)
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cli == "" || kubeconfig == "" || cluster == "" {
		t.Skip("the CLI, CLOUDBURROW_TEST_KUBECONFIG and CLOUDBURROW_TEST_CLUSTER are required to run a pod")
	}
	for _, bin := range []string{"docker", "kind", "kubectl", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build and run the pod fixture", bin)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	out, err := exec.Command(cli, append([]string{"status", "--format", "json"}, strings.Fields(os.Getenv(EnvCLIArgs))...)...).Output()
	var st struct {
		Services []struct {
			ID        string `json:"id"`
			InCluster string `json:"in_cluster"`
		} `json:"services"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		t.Fatalf("status --format json (%v): %v\n%s", err, jerr, out)
	}
	inCluster := map[string]string{}
	for _, s := range st.Services {
		inCluster[s.ID] = s.InCluster
	}
	for _, id := range []string{"secretmanager", "tasks"} {
		a := inCluster[id]
		if !strings.HasPrefix(a, "cloudburrow-host.") || strings.Contains(a, "127.0.0.1") {
			t.Fatalf("status reports %s's in-cluster address as %q; want the cloudburrow-host name", id, a)
		}
	}

	id := "host-probe"
	secret := secretsParent(h) + "/secrets/" + id
	auto := &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: secretsParent(h), SecretId: id, Secret: &secretmanagerpb.Secret{Replication: auto}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: secret}) })
	if _, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: secret, Payload: &secretmanagerpb.SecretPayload{Data: []byte("from-the-host")}}); err != nil {
		t.Fatal(err)
	}
	queue := queue(t, h, tc, id)

	root := moduleRoot(t)
	dir := root + "/testdata/hostprobe"
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", dir+"/hostprobe", "./testdata/hostprobe")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile the host probe: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = os.Remove(dir + "/hostprobe") })
	if b, err := exec.CommandContext(ctx, "docker", "build", "-t", hostProbeImage, dir).CombinedOutput(); err != nil {
		t.Fatalf("build the host probe image: %v\n%s", err, b)
	}
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", hostProbeImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the host probe: %v\n%s", err, b)
	}

	// The namespace Cloud Run workloads run in, so the name resolves as it
	// does for a revision.
	ns := "default"
	kubectl := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig, "-n", ns}, args...)...)
		var b bytes.Buffer
		cmd.Stdout, cmd.Stderr = &b, &b
		err := cmd.Run()
		return b.String(), err
	}
	job := "host-probe"
	_, _ = kubectl("delete", "job", job, "--ignore-not-found", "--wait=true")
	t.Cleanup(func() {
		cmd := exec.Command("kubectl", "--kubeconfig", kubeconfig, "-n", ns, "delete", "job", job, "--ignore-not-found", "--wait=false")
		_ = cmd.Run()
	})
	manifest := fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: probe
        image: %s
        imagePullPolicy: Never
        env:
        - {name: PROJECT, value: %q}
        - {name: PROBE_ID, value: %q}
        - {name: SECRETS_ADDR, value: %q}
        - {name: TASKS_ADDR, value: %q}
`, job, hostProbeImage, h.Project(), id, inCluster["secretmanager"], inCluster["tasks"])
	apply := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", ns, "apply", "-f", "-")
	apply.Stdin = strings.NewReader(manifest)
	if b, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("apply the probe job: %v\n%s", err, b)
	}
	waitOut, waitErr := kubectl("wait", "--for=condition=complete", "job/"+job, "--timeout=5m")
	logs, _ := kubectl("logs", "job/"+job)
	if waitErr != nil || !strings.Contains(logs, "HOST PROBE: OK") {
		t.Fatalf("the probe did not succeed: %v %s\nlogs:\n%s", waitErr, waitOut, logs)
	}
	t.Logf("%s", strings.TrimSpace(logs))

	// The host reads what the pod wrote: the same server, not a copy.
	it := tc.ListTasks(ctx, &cloudtaskspb.ListTasksRequest{Parent: queue})
	if task, err := it.Next(); err != nil || task == nil {
		t.Errorf("the host sees no task from the pod: %v", err)
	}
}
