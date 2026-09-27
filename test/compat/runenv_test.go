//go:build compat

package compat

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

const envProbeImage = "dev.local/cloudburrow-envprobe:compat"

// TestCloudRunRevisionReachesStorageAndPubSubWithNoClientOptions (#576): a
// Cloud Run service whose container calls storage.NewClient(ctx) and
// pubsub.NewClient(ctx, project) with no options lists a bucket and
// publishes to a topic, through the official clients, because the adapter
// injected the in-cluster STORAGE_EMULATOR_HOST, PUBSUB_EMULATOR_HOST and
// GOOGLE_CLOUD_PROJECT into its pod. The deployment sets no variable of its
// own, and GetService shows none. The host then reads the object list back from the container
// and pulls the message it published: the same emulators, not a copy.
func TestCloudRunRevisionReachesStorageAndPubSubWithNoClientOptions(t *testing.T) {
	h := New(t)
	rc := runClient(t, h)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the fixture must be loaded into the owned cluster")
	}
	for _, bin := range []string{"docker", "kind", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the fixture", bin)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	root := moduleRoot(t)
	dir := root + "/testdata/envprobe"
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", dir+"/envprobe", "./testdata/envprobe")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile the env probe: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = os.Remove(dir + "/envprobe") })
	if b, err := exec.CommandContext(ctx, "docker", "build", "-t", envProbeImage, dir).CombinedOutput(); err != nil {
		t.Fatalf("build the env probe image: %v\n%s", err, b)
	}
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", envProbeImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the env probe: %v\n%s", err, b)
	}

	// The host's own clients, to set up what the revision reads and to
	// check what it wrote.
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageAddr+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	bh := bucket(t, h, sc)
	w := bh.Object("from-the-host.txt").NewWriter(ctx)
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write an object: %v", err)
	}
	const topicID, subID = "envprobe-topic", "envprobe-sub"
	tn := topic(t, h, pc, topicID)
	sn := subscription(t, h, pc, subID, tn)

	// No Env at all: everything the container needs is injected.
	id := "compat-envprobe"
	name := runParent(h) + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{
		Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{
			Containers: []*runpb.Container{{Image: envProbeImage}}}},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the service never became ready: %v", err)
	}

	// The Cloud Run API shows exactly the caller's env, which is none: the
	// injected variables are in the pod, not in the resource, as Cloud Run
	// keeps K_SERVICE and PORT out of it, so Terraform sees no drift.
	got, err := rc.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if env := got.GetTemplate().GetContainers()[0].GetEnv(); len(env) != 0 {
		t.Errorf("GetService shows env %v; want none, as none was set", env)
	}
	// The Knative Service holds them, in-cluster, and no host address or
	// credential.
	manifest, err := kubectlGet(t, "ksvc", id)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: STORAGE_EMULATOR_HOST", "name: PUBSUB_EMULATOR_HOST",
		"name: GOOGLE_CLOUD_PROJECT", "storage.", ".svc.cluster.local:", "cloudburrow.dev/injected-env"} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the Knative Service lacks %q:\n%s", want, manifest)
		}
	}
	for _, bad := range []string{"127.0.0.1", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if strings.Contains(manifest, bad) {
			t.Errorf("the Knative Service names %s:\n%s", bad, manifest)
		}
	}

	base, host := ingress(t), hostOf(t, svc.GetUri())
	q := url.Values{"bucket": {bh.BucketName()}, "topic": {topicID}}
	code, body := httpGet(t, base, host, "/?"+q.Encode())
	if code != http.StatusOK || !strings.Contains(body, "ENV PROBE: OK") {
		t.Fatalf("the revision's clients did not reach the emulators: %d %s", code, body)
	}
	if !strings.Contains(body, "objects=from-the-host.txt ") {
		t.Errorf("the revision listed a different bucket: %s", body)
	}
	// What the container itself read from its environment.
	if !strings.Contains(body, "project="+h.Project()+" ") ||
		!strings.Contains(body, "storage=http://storage.") || !strings.Contains(body, "pubsub=pubsub.") {
		t.Errorf("the container was not given the in-cluster addresses and its project: %s", body)
	}
	t.Logf("%s", strings.TrimSpace(body))

	// The message the revision published is on the host's subscription.
	recvCtx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	var data string
	if err := pc.Subscriber(sn).Receive(recvCtx, func(_ context.Context, m *pubsub.Message) {
		m.Ack()
		data = string(m.Data)
		stop()
	}); err != nil && recvCtx.Err() == nil {
		t.Fatalf("Receive: %v", err)
	}
	if data != "from-the-revision" {
		t.Errorf("the host's subscription received %q, want the revision's message", data)
	}
}

// runShardEndpoint is the host endpoint of a service a Cloud Run test needs
// beside Run. CI's run shard exports it under a name of its own, so that
// shard does not run the whole Storage or Pub/Sub suite a second time; an
// instance with every service needs only the ordinary variable.
func runShardEndpoint(h *Harness, own, shared string) string {
	h.t.Helper()
	if strings.TrimSpace(os.Getenv(own)) != "" {
		return h.Endpoint(own)
	}
	return h.Endpoint(shared)
}
