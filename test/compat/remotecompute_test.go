//go:build compat

package compat

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

// Remote compute (#2209): an instance started with `up --compute
// ssh://…` on the stand-in host testdata/remotehost, a container on this
// machine's Docker running sshd and its own Docker Engine
// (scripts/remote-compute-standin.sh). These tests skip on every other
// instance; the rest of the suite runs against such an instance unchanged.
//
//	CLOUDBURROW_TEST_REMOTE_STANDIN  the stand-in's container name
//	CLOUDBURROW_TEST_REMOTE_INGRESS  the instance's ingress, host:port here
//	                                 (--port-ingress), forwarded from the host
const (
	envRemoteStandIn = "CLOUDBURROW_TEST_REMOTE_STANDIN"
	envRemoteIngress = "CLOUDBURROW_TEST_REMOTE_INGRESS"
)

func remoteStandIn(t *testing.T) (standIn, cluster string) {
	t.Helper()
	standIn = strings.TrimSpace(os.Getenv(envRemoteStandIn))
	cluster = strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if standIn == "" || cluster == "" {
		t.Skipf("%s and CLOUDBURROW_TEST_CLUSTER name no instance on a stand-in remote host", envRemoteStandIn)
	}
	return standIn, cluster
}

func dockerOut(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	// This machine's own Docker, whatever the test's environment says.
	cmd.Env = append(os.Environ(), "DOCKER_HOST=", "DOCKER_CONTEXT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestRemoteComputeTheClusterRunsOnTheHost: the instance's kind node is a
// container of the host's Docker and not of this machine's, and the
// kubeconfig's API server, on this machine's loopback, is the port the
// host publishes for it.
func TestRemoteComputeTheClusterRunsOnTheHost(t *testing.T) {
	standIn, cluster := remoteStandIn(t)
	node := cluster + "-control-plane"
	if local := dockerOut(t, "ps", "-a", "--filter", "name=^"+node+"$", "--format", "{{.Names}}"); strings.TrimSpace(local) != "" {
		t.Fatalf("this machine's Docker has %s: the cluster was not created on the host", node)
	}
	onHost := dockerOut(t, "exec", standIn, "docker", "ps", "--filter", "name=^"+node+"$", "--format", "{{.Names}} {{.Ports}}")
	if !strings.HasPrefix(onHost, node+" ") {
		t.Fatalf("the host's Docker has no running %s: %q", node, onHost)
	}
	kc := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kc == "" {
		t.Skipf("%s is not set", envKubeconfig)
	}
	server, err := exec.Command("kubectl", "--kubeconfig", kc, "config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}").Output()
	if err != nil {
		t.Fatal(err)
	}
	port := strings.TrimPrefix(strings.TrimSpace(string(server)), "https://127.0.0.1:")
	if !strings.Contains(onHost, "127.0.0.1:"+port+"->6443/tcp") {
		t.Fatalf("the kubeconfig names %s, which the host does not publish for the node (%q)", server, onHost)
	}
	out, err := exec.Command("kubectl", "--kubeconfig", kc, "get", "nodes", "-o", "name").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "node/"+node) {
		t.Fatalf("kubectl through the forwarded API server: %v %s", err, out)
	}
}

// TestRemoteComputeServesTheDefaultServices drives each default service
// with Google's Go client, against an instance whose workloads are on the
// host: Cloud Storage and Pub/Sub are pods there, Secret Manager and Cloud
// Tasks run in this process, and Cloud Run deploys a revision on the host's
// cluster, invoked through the ingress the host publishes and this machine
// forwards.
func TestRemoteComputeServesTheDefaultServices(t *testing.T) {
	standIn, _ := remoteStandIn(t)
	parent := New(t)

	t.Run("storage", func(t *testing.T) {
		h := parent.For(t)
		ctx := h.Context()
		c := storageClient(t, h)
		b := bucket(t, h, c)
		w := b.Object("remote.txt").NewWriter(ctx)
		if _, err := io.WriteString(w, "written to a pod on the host"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write: %v", err)
		}
		r, err := b.Object("remote.txt").NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(r)
		_ = r.Close()
		if string(got) != "written to a pod on the host" {
			t.Fatalf("read %q", got)
		}
		_ = b.Object("remote.txt").Delete(ctx)
	})

	t.Run("pubsub", func(t *testing.T) {
		h := parent.For(t)
		ctx := h.Context()
		c := pubsubClient(t, h)
		tn := topic(t, h, c, "remote-topic")
		sn := subscription(t, h, c, "remote-sub", tn)
		pub := c.Publisher(tn)
		defer pub.Stop()
		if _, err := pub.Publish(ctx, &pubsub.Message{Data: []byte("over ssh")}).Get(ctx); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		res, err := c.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sn, MaxMessages: 1})
		if err != nil || len(res.ReceivedMessages) != 1 || string(res.ReceivedMessages[0].Message.Data) != "over ssh" {
			t.Fatalf("Pull = %v, %v", res, err)
		}
	})

	t.Run("secretmanager", func(t *testing.T) {
		h := parent.For(t)
		ctx := h.Context()
		c := secretsClient(t, h)
		parent := "projects/" + h.Project()
		s, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: "remote-secret",
			Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}})
		if err != nil {
			t.Fatalf("CreateSecret: %v", err)
		}
		t.Cleanup(func() { _ = c.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: s.Name}) })
		v, err := c.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: s.Name,
			Payload: &secretmanagerpb.SecretPayload{Data: []byte("kept on the host's cluster")}})
		if err != nil {
			t.Fatalf("AddSecretVersion: %v", err)
		}
		got, err := c.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{Name: v.Name})
		if err != nil || string(got.Payload.Data) != "kept on the host's cluster" {
			t.Fatalf("AccessSecretVersion = %v, %v", got, err)
		}
	})

	t.Run("tasks", func(t *testing.T) {
		h := parent.For(t)
		ctx := h.Context()
		c := tasksClient(t, h)
		q, err := c.CreateQueue(ctx, &cloudtaskspb.CreateQueueRequest{Parent: location(h),
			Queue: &cloudtaskspb.Queue{Name: location(h) + "/queues/remote-queue"}})
		if err != nil {
			t.Fatalf("CreateQueue: %v", err)
		}
		t.Cleanup(func() { _ = c.DeleteQueue(context.Background(), &cloudtaskspb.DeleteQueueRequest{Name: q.Name}) })
		task, err := c.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{Parent: q.Name, Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{HttpRequest: &cloudtaskspb.HttpRequest{
				Url: "http://127.0.0.1:9/never", HttpMethod: cloudtaskspb.HttpMethod_POST}}}})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if _, err := c.GetTask(ctx, &cloudtaskspb.GetTaskRequest{Name: task.Name}); err != nil {
			t.Fatalf("GetTask: %v", err)
		}
	})

	t.Run("run", func(t *testing.T) {
		h := parent.For(t)
		ingressAddr := strings.TrimSpace(os.Getenv(envRemoteIngress))
		if ingressAddr == "" {
			t.Skipf("%s is not set", envRemoteIngress)
		}
		c := runClient(t, h)
		rctx := runWait(t)
		id := "remote-hello"
		name := runParent(h) + "/services/" + id
		op, err := c.CreateService(rctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id,
			Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
				Image: "ghcr.io/knative/helloworld-go:latest",
				Env:   []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: "the remote host"}}},
			}}}}})
		if err != nil {
			t.Fatalf("CreateService: %v", err)
		}
		t.Cleanup(func() { _, _ = c.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: name}) })
		svc, err := waitLRO(rctx, op)
		if err != nil {
			t.Fatalf("waiting for the service: %v", err)
		}
		// The revision's pod is on the host's node.
		pods := dockerOut(t, "exec", standIn, "docker", "exec", os.Getenv("CLOUDBURROW_TEST_CLUSTER")+"-control-plane",
			"crictl", "ps", "--name", "user-container", "-o", "json")
		if !strings.Contains(pods, "user-container") {
			t.Fatalf("no revision container on the host's node: %s", pods)
		}
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, "http://"+ingressAddr+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = hostOf(t, svc.GetUri())
		var body []byte
		deadline := time.Now().Add(2 * time.Minute)
		for {
			res, err := http.DefaultClient.Do(req)
			if err == nil {
				body, _ = io.ReadAll(res.Body)
				_ = res.Body.Close()
				if res.StatusCode == http.StatusOK && bytes.Contains(body, []byte("the remote host")) {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("invoking %s through the forwarded ingress %s: %v %q", svc.GetUri(), ingressAddr, err, body)
			}
			time.Sleep(time.Second)
		}
		t.Logf("invoked %s: %s", svc.GetUri(), strings.TrimSpace(string(body)))
	})
}
