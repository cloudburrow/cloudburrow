//go:build e2e

// Package e2e proves the acceptance workflow that judges the release (#19):
// upload an object, publish an event, run a worker that reads it, and save the
// result — every step through an official Google SDK.
//
// If any step needed a CloudBurrow-specific client, the release would have
// failed its own test.
package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// Environment variables the harness reads, exported by `cloudburrow up`.
const (
	envStorage    = "CLOUDBURROW_TEST_STORAGE"           // host endpoint, with scheme
	envPubSub     = "CLOUDBURROW_TEST_PUBSUB"            // host endpoint
	envRun        = "CLOUDBURROW_TEST_RUN"               // host endpoint
	envInCluster  = "CLOUDBURROW_TEST_STORAGE_INCLUSTER" // in-cluster storage address
	envKubeconfig = "CLOUDBURROW_TEST_KUBECONFIG"
)

func require(t *testing.T, name string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Skipf("%s is not set; see test/e2e/README.md", name)
	}
	return v
}

func kubectl(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--kubeconfig", require(t, envKubeconfig)}, args...)
	out, err := exec.CommandContext(ctx, "kubectl", full...).CombinedOutput()
	return string(out), err
}

// TestAcceptanceWorkflow is the release gate.
//
//	upload -> publish -> worker reads and writes -> result read back
func TestAcceptanceWorkflow(t *testing.T) {
	storageHost := require(t, envStorage)
	pubsubHost := require(t, envPubSub)
	inCluster := require(t, envInCluster)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	project := fmt.Sprintf("cb-e2e-%d", time.Now().UnixNano()%1e9)
	bucket := project + "-bucket"

	// --- 1. Upload, through the official Cloud Storage SDK ---
	t.Setenv("STORAGE_EMULATOR_HOST", storageHost)
	sc, err := storage.NewClient(ctx,
		option.WithoutAuthentication(),
		option.WithEndpoint(storageHost+"/storage/v1/"))
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	defer sc.Close()

	bh := sc.Bucket(bucket)
	if err := bh.Create(ctx, project, nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	w := bh.Object("input.txt").NewWriter(ctx)
	if _, err := w.Write([]byte("cloudburrow end to end")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close input: %v", err)
	}
	t.Log("1. uploaded input.txt via the official Cloud Storage SDK")

	// --- 2. Deploy the worker as a Cloud Run service ---
	//
	// The worker is given the *in-cluster* storage address: a pod's loopback
	// is the pod itself, so the host address would be unreachable.
	workerName := deployWorker(t, ctx, bucket, inCluster)
	t.Logf("2. worker deployed and ready at %s", workerName)

	// --- 3. Publish an event, through the official Pub/Sub SDK ---
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubHost)
	pc, err := pubsub.NewClient(ctx, project)
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	defer pc.Close()

	topic := fmt.Sprintf("projects/%s/topics/uploads-topic", project)
	if _, err := pc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatalf("create topic: %v", err)
	}
	sub := fmt.Sprintf("projects/%s/subscriptions/worker-subscription", project)
	if _, err := pc.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name:               sub,
		Topic:              topic,
		AckDeadlineSeconds: 60,
		PushConfig:         &pubsubpb.PushConfig{PushEndpoint: workerName},
	}); err != nil {
		t.Fatalf("create push subscription: %v", err)
	}

	pub := pc.Publisher(topic)
	defer pub.Stop()
	id, err := pub.Publish(ctx, &pubsub.Message{Data: []byte("input.txt")}).Get(ctx)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	t.Logf("3. published message id=%s to a push subscription targeting the worker", id)

	// --- 4. Read the result back, through the official SDK ---
	//
	// Bounded polling: the worker is an external process whose clock we
	// cannot advance.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		r, err := bh.Object("result.txt").NewReader(ctx)
		if err == nil {
			got, _ := io.ReadAll(r)
			r.Close()
			if want := "CLOUDBURROW END TO END"; string(got) != want {
				t.Fatalf("result.txt = %q, want %q", got, want)
			}
			t.Logf("4. worker wrote result.txt = %q", got)
			t.Log("ACCEPTANCE WORKFLOW: PASS")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}

	logs, _ := kubectl(t, ctx, "logs", "-l", "serving.knative.dev/service=e2e-worker",
		"-c", "user-container", "--tail=40")
	t.Fatalf("worker never wrote result.txt.\nworker logs:\n%s", logs)
}

// deployWorker builds the worker, loads it into the cluster, deploys it as a
// Cloud Run service, and returns the address Pub/Sub should push to.
func deployWorker(t *testing.T, ctx context.Context, bucket, storageInCluster string) string {
	t.Helper()
	for _, bin := range []string{"docker", "kind", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to deploy the worker", bin)
		}
	}

	// dev.local is required: Knative resolves tags against a registry, and a
	// locally built image has none.
	const image = "dev.local/cloudburrow-e2e-worker:test"
	root := repoRoot(t)
	build := exec.CommandContext(ctx, "docker", "build", "-t", image, "testdata/worker")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker image in %s: %v\n%s", root, err, out)
	}

	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set")
	}
	if out, err := exec.CommandContext(ctx, "kind", "load", "docker-image", image, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load worker image: %v\n%s", err, out)
	}

	manifest := fmt.Sprintf(`apiVersion: serving.knative.dev/v1
kind: Service
metadata:
  name: e2e-worker
  namespace: default
spec:
  template:
    metadata:
      annotations:
        autoscaling.knative.dev/min-scale: "1"
    spec:
      containers:
        - image: %s
          imagePullPolicy: Never
          env:
            - name: STORAGE_ENDPOINT
              value: %q
            - name: BUCKET
              value: %q
`, image, "http://"+storageInCluster, bucket)

	apply := exec.CommandContext(ctx, "kubectl", "--kubeconfig", require(t, envKubeconfig), "apply", "-f", "-")
	apply.Stdin = strings.NewReader(manifest)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("deploy worker: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = kubectl(t, context.Background(), "delete", "ksvc", "e2e-worker", "--ignore-not-found")
	})

	if out, err := kubectl(t, ctx, "wait", "ksvc/e2e-worker", "--for=condition=Ready", "--timeout=300s"); err != nil {
		diag, _ := kubectl(t, ctx, "get", "ksvc", "e2e-worker", "-o", "yaml")
		t.Fatalf("worker never became ready: %v\n%s\n%s", err, out, diag)
	}
	// Push delivery happens inside the cluster, so the worker must be
	// addressed by its cluster-local name.
	return "http://e2e-worker.default.svc.cluster.local"
}

// repoRoot walks up from the test's working directory to the module root, so
// the worker build does not depend on how far nested this package is.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root from the test working directory")
	return ""
}
