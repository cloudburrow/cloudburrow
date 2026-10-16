//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const eventProbeImage = "dev.local/cloudburrow-eventprobe:compat"

// loadEventProbe compiles testdata/eventprobe, a server that records the
// CloudEvents it is sent, builds its image and loads it into the instance's
// kind cluster, as loadEnvProbe does.
func loadEventProbe(ctx context.Context, t *testing.T) {
	t.Helper()
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if cluster == "" {
		t.Skip("CLOUDBURROW_TEST_CLUSTER is not set; the fixture must be loaded into the owned cluster")
	}
	for _, bin := range []string{"docker", "kind", "go"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to build the fixture", bin)
		}
	}
	root := moduleRoot(t)
	dir := root + "/testdata/eventprobe"
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", dir+"/eventprobe", "./testdata/eventprobe")
	build.Dir = root
	build.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile the event probe: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = os.Remove(dir + "/eventprobe") })
	if b, err := exec.CommandContext(ctx, "docker", "build", "-t", eventProbeImage, dir).CombinedOutput(); err != nil {
		t.Fatalf("build the event probe image: %v\n%s", err, b)
	}
	if b, err := exec.CommandContext(ctx, "kind", "load", "docker-image", eventProbeImage, "--name", cluster).CombinedOutput(); err != nil {
		t.Fatalf("load the event probe: %v\n%s", err, b)
	}
}

type probedEvent struct {
	Headers map[string]string `json:"headers"`
	Data    string            `json:"data"`
}

// probedEvents polls the probe's /events until want reports one matches,
// or a deadline passes.
func probedEvents(t *testing.T, base, host string, want func(probedEvent) bool) (probedEvent, []probedEvent) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var all []probedEvent
	for time.Now().Before(deadline) {
		code, body, err := doRequest(http.MethodGet, base+"/events", host, "", 15*time.Second)
		if err == nil && code == http.StatusOK {
			all = nil
			_ = json.Unmarshal([]byte(body), &all)
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

// tokenEmail reads the email claim of a Bearer ID token, unverified: the
// signature is the instance's, which TestTasksOIDCAndOAuthTokens checks.
func tokenEmail(authz string) string {
	parts := strings.Split(strings.TrimPrefix(authz, "Bearer "), ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(b, &c)
	return c.Email
}

// eventFunction deploys the event probe as the Cloud Run service a function
// named id runs as, then creates the function with trigger from a source
// that does not build, and redeploys its service config, which keeps the
// service's image: the function is ACTIVE on the probe, and its trigger is
// made, without a buildpacks build (that is TestFunctionsEventTriggerBuilds,
// gated).
func eventFunction(ctx context.Context, t *testing.T, h *Harness, id string, trigger *functionspb.EventTrigger) (*functionspb.Function, string) {
	t.Helper()
	rc, fc := runClient(t, h), functionsClient(t, h)
	parent := runParent(h)
	svcName := parent + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: id, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: eventProbeImage}},
			Scaling: &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 1}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: svcName})
	})
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("the probe service never became ready: %v", err)
	}
	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("not a zip: the probe's image is served"))
	name := parent + "/functions/" + id
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if op, err := fc.DeleteFunction(dctx, &functionspb.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	cop, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Probe",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}},
		ServiceConfig: &functionspb.ServiceConfig{MinInstanceCount: 1, MaxInstanceCount: 1},
		EventTrigger:  trigger}})
	if err != nil {
		t.Fatalf("CreateFunction with an event trigger: %v", err)
	}
	if _, err := cop.Wait(ctx); err == nil || !strings.Contains(err.Error(), "Build failed") {
		t.Fatalf("CreateFunction.Wait = %v; want the build's error", err)
	}
	uop, err := fc.UpdateFunction(ctx, &functionspb.UpdateFunctionRequest{
		Function:   &functionspb.Function{Name: name, ServiceConfig: &functionspb.ServiceConfig{AvailableMemory: "512M"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"service_config.available_memory"}}})
	if err != nil {
		t.Fatalf("UpdateFunction: %v", err)
	}
	fn, err := uop.Wait(ctx)
	if err != nil {
		t.Fatalf("UpdateFunction.Wait: %v", err)
	}
	if fn.GetState() != functionspb.Function_ACTIVE {
		t.Fatalf("the function is %v; want ACTIVE", fn.GetState())
	}
	return fn, name
}

// eventarcSubscription finds the subscription a trigger made on topic.
func eventarcSubscription(ctx context.Context, t *testing.T, pc *pubsub.Client, topic string) *pubsubpb.Subscription {
	t.Helper()
	it := pc.TopicAdminClient.ListTopicSubscriptions(ctx, &pubsubpb.ListTopicSubscriptionsRequest{Topic: topic})
	for {
		s, err := it.Next()
		if err == iterator.Done {
			return nil
		}
		if err != nil {
			t.Fatalf("ListTopicSubscriptions: %v", err)
		}
		if strings.Contains(s, "/subscriptions/eventarc-") {
			sub, err := pc.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: s})
			if err != nil {
				t.Fatalf("GetSubscription: %v", err)
			}
			return sub
		}
	}
}

// covers: google.cloud.functions.v2.FunctionService/CreateFunction, google.cloud.functions.v2.FunctionService/UpdateFunction, google.cloud.functions.v2.FunctionService/GetFunction, google.cloud.functions.v2.FunctionService/DeleteFunction
//
// TestFunctionsEventTriggers (#1414): 2nd-gen functions with event
// triggers, through the official Functions, Pub/Sub and Storage clients, on
// an instance with --services run,storage,pubsub. Each function runs the
// event probe, which records what it is sent. A Pub/Sub trigger makes an
// eventarc- push subscription on the topic, labelled goog-managed-by
// eventarc, and a published message reaches the function as a binary-mode
// CloudEvent with Google's headers, MessagePublishedData and an ID token for
// the trigger's account. A Cloud Storage finalized trigger makes an
// eventarc- topic, the bucket's notification on it and a subscription, and
// an upload reaches the function with ce-subject objects/NAME and the
// object. Deleting the functions removes what their triggers made, and
// leaves the user's topic.
func TestFunctionsEventTriggers(t *testing.T) {
	h := New(t)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	loadEventProbe(ctx, t)
	t.Setenv("PUBSUB_EMULATOR_HOST", pubsubAddr)
	pc, err := pubsub.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	sc, err := storage.NewClient(ctx, option.WithoutAuthentication(), option.WithEndpoint(storageAddr+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	fc := functionsClient(t, h)
	base := ingress(t)

	// Pub/Sub.
	tn := topic(t, h, pc, "fn-events")
	sa := "events@" + h.Project() + ".iam.gserviceaccount.com"
	fn, name := eventFunction(ctx, t, h, "evfn-pubsub", &functionspb.EventTrigger{
		EventType: "google.cloud.pubsub.topic.v1.messagePublished", PubsubTopic: tn,
		RetryPolicy: functionspb.EventTrigger_RETRY_POLICY_RETRY, ServiceAccountEmail: sa})
	et := fn.GetEventTrigger()
	if !strings.HasPrefix(et.GetTrigger(), "projects/"+h.Project()+"/locations/us-central1/triggers/evfn-pubsub-") ||
		et.GetTriggerRegion() != "us-central1" || et.GetRetryPolicy() != functionspb.EventTrigger_RETRY_POLICY_RETRY ||
		et.GetServiceAccountEmail() != sa {
		t.Errorf("event_trigger = %v; want its trigger named, with the region, retry policy and account", et)
	}
	if got, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); err != nil || got.GetEventTrigger().GetTrigger() != et.GetTrigger() {
		t.Errorf("GetFunction = %v, %v; want the trigger named", got.GetEventTrigger(), err)
	}
	sub := eventarcSubscription(ctx, t, pc, tn)
	if sub == nil || !strings.HasPrefix(sub.GetName(), "projects/"+h.Project()+"/subscriptions/eventarc-us-central1-evfn-pubsub-") ||
		!strings.Contains(sub.GetName(), "-sub-") || sub.GetLabels()["goog-managed-by"] != "eventarc" ||
		!strings.Contains(sub.GetPushConfig().GetPushEndpoint(), "/cloudburrow/eventarc/push/"+et.GetTrigger()) {
		t.Fatalf("the trigger's subscription = %v", sub)
	}
	res, err := pc.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tn, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte("hello functions"), Attributes: map[string]string{"k": "v"}}}})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgID := res.GetMessageIds()[0]
	host := hostOf(t, fn.GetServiceConfig().GetUri())
	ev, all := probedEvents(t, base, host, func(e probedEvent) bool { return e.Headers["ce-id"] == msgID })
	if ev.Headers == nil {
		t.Fatalf("no CloudEvent with ce-id %s reached the function; it got %v", msgID, all)
	}
	for k, want := range map[string]string{"ce-type": "google.cloud.pubsub.topic.v1.messagePublished",
		"ce-source": "//pubsub.googleapis.com/" + tn, "ce-specversion": "1.0", "content-type": "application/json"} {
		if ev.Headers[k] != want {
			t.Errorf("%s = %q, want %q", k, ev.Headers[k], want)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.Headers["ce-time"]); err != nil {
		t.Errorf("ce-time = %q: %v", ev.Headers["ce-time"], err)
	}
	if got := tokenEmail(ev.Headers["authorization"]); got != sa {
		t.Errorf("the delivery's token names %q; want the trigger's account %s", got, sa)
	}
	var mpd struct {
		Message struct {
			Data       string            `json:"data"`
			Attributes map[string]string `json:"attributes"`
			MessageID  string            `json:"messageId"`
		} `json:"message"`
		Subscription string `json:"subscription"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &mpd); err != nil || mpd.Subscription != sub.GetName() ||
		mpd.Message.MessageID != msgID || mpd.Message.Attributes["k"] != "v" ||
		mpd.Message.Data != base64.StdEncoding.EncodeToString([]byte("hello functions")) {
		t.Errorf("MessagePublishedData = %s (%v); want the message and the subscription", ev.Data, err)
	}

	// Cloud Storage.
	bh := bucket(t, h, sc)
	gfn, gname := eventFunction(ctx, t, h, "evfn-storage", &functionspb.EventTrigger{
		EventType:    "google.cloud.storage.object.v1.finalized",
		EventFilters: []*functionspb.EventFilter{{Attribute: "bucket", Value: bh.BucketName()}}})
	get := gfn.GetEventTrigger()
	if !strings.Contains(get.GetTrigger(), "/triggers/evfn-storage-") || get.GetRetryPolicy() != functionspb.EventTrigger_RETRY_POLICY_DO_NOT_RETRY ||
		!strings.HasSuffix(get.GetServiceAccountEmail(), "-compute@developer.gserviceaccount.com") {
		t.Errorf("event_trigger = %v; want it named, DO_NOT_RETRY and the default compute account", get)
	}
	var ntopic string
	if ns, err := bh.Notifications(ctx); err != nil || len(ns) != 1 {
		t.Fatalf("the bucket's notifications = %v, %v; want the trigger's", ns, err)
	} else {
		for _, n := range ns {
			ntopic = "projects/" + n.TopicProjectID + "/topics/" + n.TopicID
			if !strings.HasPrefix(n.TopicID, "eventarc-us-central1-evfn-storage-") || len(n.EventTypes) != 1 || n.EventTypes[0] != "OBJECT_FINALIZE" {
				t.Errorf("notification = %+v", n)
			}
		}
	}
	if s := eventarcSubscription(ctx, t, pc, ntopic); s == nil {
		t.Fatalf("no eventarc subscription on %s", ntopic)
	}
	w := bh.Object("dir/event.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("abc"))
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	gev, all := probedEvents(t, base, hostOf(t, gfn.GetServiceConfig().GetUri()), func(e probedEvent) bool {
		return e.Headers["ce-subject"] == "objects/dir/event.txt"
	})
	if gev.Headers == nil {
		t.Fatalf("no CloudEvent for dir/event.txt reached the function; it got %v", all)
	}
	if gev.Headers["ce-type"] != "google.cloud.storage.object.v1.finalized" ||
		gev.Headers["ce-source"] != "//storage.googleapis.com/projects/_/buckets/"+bh.BucketName() || gev.Headers["ce-id"] == "" {
		t.Errorf("headers = %v", gev.Headers)
	}
	var obj struct{ Bucket, Name, Size string }
	if err := json.Unmarshal([]byte(gev.Data), &obj); err != nil || obj.Name != "dir/event.txt" || obj.Bucket != bh.BucketName() || obj.Size != "3" {
		t.Errorf("StorageObjectData = %s (%v)", gev.Data, err)
	}

	// Deleting the functions removes what their triggers made.
	for _, n := range []string{name, gname} {
		op, err := fc.DeleteFunction(ctx, &functionspb.DeleteFunctionRequest{Name: n})
		if err != nil {
			t.Fatalf("DeleteFunction: %v", err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatalf("DeleteFunction.Wait: %v", err)
		}
	}
	if s := eventarcSubscription(ctx, t, pc, tn); s != nil {
		t.Errorf("the Pub/Sub trigger's subscription %s outlived its function", s.GetName())
	}
	if _, err := pc.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil {
		t.Errorf("the user's topic is gone: %v", err)
	}
	if ns, err := bh.Notifications(ctx); err != nil || len(ns) != 0 {
		t.Errorf("the bucket's notifications after delete = %v, %v; want none", ns, err)
	}
	if _, err := pc.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: ntopic}); err == nil {
		t.Errorf("the storage trigger's topic %s outlived its function", ntopic)
	}
}
