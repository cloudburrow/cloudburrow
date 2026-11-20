//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	container "cloud.google.com/go/container/apiv1"
	"cloud.google.com/go/container/apiv1/containerpb"
	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/service/gke"
)

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger, google.cloud.eventarc.v1.Eventarc/UpdateTrigger, google.cloud.eventarc.v1.Eventarc/DeleteTrigger
//
// TestEventarcGKEDestination (#1877), with the official Kubernetes Engine,
// Eventarc and Pub/Sub clients, as Google's "Route Pub/Sub events to GKE"
// documents it: on an Autopilot cluster the Kubernetes Engine API makes,
// a Deployment of the event probe behind a Service (port 80) in a
// namespace receives, at the destination's path, a message published to
// the topic Eventarc made for a messagePublished trigger whose
// destination.gke names the cluster, namespace, service and path, as a
// binary-mode CloudEvent with MessagePublishedData. The location defaults
// to the trigger's region; UpdateTrigger moves the path; a cluster the
// API does not know, and no service, are INVALID_ARGUMENT.
func TestEventarcGKEDestination(t *testing.T) {
	h := New(t)
	gkeAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_GKE", EnvGKE)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	instance := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if instance == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the probe must be loaded into the cluster")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to deploy the probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cm, err := container.NewClusterManagerClient(ctx, option.WithEndpoint(gkeAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cm.Close() })
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	parent := runParent(h) // projects/P/locations/us-central1
	typ := []*eventarcpb.EventFilter{{Attribute: "type", Value: "google.cloud.pubsub.topic.v1.messagePublished"}}
	toGKE := func(g *eventarcpb.GKE) *eventarcpb.Destination {
		return &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_Gke{Gke: g}}
	}
	for name, g := range map[string]*eventarcpb.GKE{
		"an unknown cluster": {Cluster: "no-such-cluster", Service: "probe"},
		"no service":         {Cluster: "ea-gke"},
	} {
		_, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "gke-bad",
			Trigger: &eventarcpb.Trigger{EventFilters: typ, Destination: toGKE(g)}})
		wantPrivateCACode(t, name, err, codes.InvalidArgument)
	}

	op, err := cm.CreateCluster(ctx, &containerpb.CreateClusterRequest{Parent: parent,
		Cluster: &containerpb.Cluster{Name: "ea-gke", Autopilot: &containerpb.Autopilot{Enabled: true},
			MasterAuth: &containerpb.MasterAuth{ClientCertificateConfig: &containerpb.ClientCertificateConfig{IssueClientCertificate: true}}}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	clusterName := parent + "/clusters/ea-gke"
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if dop, err := cm.DeleteCluster(dctx, &containerpb.DeleteClusterRequest{Name: clusterName}); err == nil {
			gkeWait(t, cm, h.Project(), "us-central1", dop)
		}
	})
	gkeWait(t, cm, h.Project(), "us-central1", op)
	cluster, err := cm.GetCluster(ctx, &containerpb.GetClusterRequest{Name: clusterName})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	kubeconfig := gkeKubeconfig(t, cluster)
	loadEventProbe(ctx, t)
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", eventProbeImage, "--name", gke.KindName(instance, clusterName)).CombinedOutput(); err != nil {
		t.Fatalf("load the probe into the GKE cluster: %v\n%s", err, b)
	}
	kubectl := func(stdin string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	kubectl(`apiVersion: v1
kind: Namespace
metadata: {name: events}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: probe, namespace: events}
spec:
  replicas: 1
  selector: {matchLabels: {app: probe}}
  template:
    metadata: {labels: {app: probe}}
    spec:
      containers:
      - name: probe
        image: `+eventProbeImage+`
        imagePullPolicy: Never
        ports: [{containerPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: probe, namespace: events}
spec:
  selector: {app: probe}
  ports: [{port: 80, targetPort: 8080}]
`, "apply", "-f", "-")
	kubectl("", "-n", "events", "rollout", "status", "deployment/probe", "--timeout=5m")

	cop, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "gke-pubsub", Trigger: &eventarcpb.Trigger{
		EventFilters: typ, Destination: toGKE(&eventarcpb.GKE{Cluster: "ea-gke", Namespace: "events", Service: "probe", Path: "/first"})}})
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	tr, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	t.Cleanup(func() {
		if dop, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: tr.GetName()}); err == nil {
			_, _ = dop.Wait(context.Background())
		}
	})
	if g := tr.GetDestination().GetGke(); g.GetLocation() != "us-central1" || g.GetNamespace() != "events" {
		t.Errorf("the trigger's destination = %v; want the location defaulted to the trigger's region", g)
	}
	topic := tr.GetTransport().GetPubsub().GetTopic()
	publish := func(data string) string {
		t.Helper()
		id, err := pc.Publisher(topic).Publish(ctx, &pubsub.Message{Data: []byte(data)}).Get(ctx)
		if err != nil {
			t.Fatalf("Publish to %s: %v", topic, err)
		}
		return id
	}
	events := func(want func(probedEvent) bool) (probedEvent, []probedEvent) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		var all []probedEvent
		for time.Now().Before(deadline) {
			out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "--raw",
				"/api/v1/namespaces/events/services/probe:80/proxy/events").Output()
			if err == nil && json.Unmarshal(out, &all) == nil {
				for _, e := range all {
					if want(e) {
						return e, all
					}
				}
			}
			time.Sleep(2 * time.Second)
		}
		return probedEvent{}, all
	}
	id := publish("to gke")
	got, all := events(func(e probedEvent) bool { return e.Headers["ce-id"] == id })
	if got.Headers == nil {
		t.Fatalf("the message %s did not reach the GKE service; it got %v", id, all)
	}
	for k, want := range map[string]string{"ce-type": "google.cloud.pubsub.topic.v1.messagePublished", "ce-specversion": "1.0",
		"ce-source": "//pubsub.googleapis.com/" + topic} {
		if got.Headers[k] != want {
			t.Errorf("%s = %q, want %q", k, got.Headers[k], want)
		}
	}
	if got.Path != "/first" {
		t.Errorf("the event reached %q, want the destination's path /first", got.Path)
	}
	var data struct {
		Message struct {
			Data string `json:"data"`
		} `json:"message"`
		Subscription string `json:"subscription"`
	}
	if err := json.Unmarshal([]byte(got.Data), &data); err != nil || data.Message.Data != base64.StdEncoding.EncodeToString([]byte("to gke")) ||
		data.Subscription != tr.GetTransport().GetPubsub().GetSubscription() {
		t.Errorf("MessagePublishedData = %s (%v)", got.Data, err)
	}

	uop, err := ec.UpdateTrigger(ctx, &eventarcpb.UpdateTriggerRequest{Trigger: &eventarcpb.Trigger{Name: tr.GetName(),
		Destination: toGKE(&eventarcpb.GKE{Path: "/second"})}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"destination.gke.path"}}})
	if err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	id = publish("moved")
	if got, all := events(func(e probedEvent) bool { return e.Headers["ce-id"] == id }); got.Path != "/second" {
		t.Errorf("after the update the event reached %q (%d events); want /second", got.Path, len(all))
	}
}
