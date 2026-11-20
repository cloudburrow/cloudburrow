//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.eventarc.v1.Eventarc/CreateTrigger, google.cloud.eventarc.v1.Eventarc/GetTrigger, google.cloud.eventarc.v1.Eventarc/ListTriggers, google.cloud.eventarc.v1.Eventarc/UpdateTrigger, google.cloud.eventarc.v1.Eventarc/DeleteTrigger, google.cloud.eventarc.v1.Eventarc/GetProvider, google.cloud.eventarc.v1.Eventarc/ListProviders
//
// TestEventarcTriggersToCloudRun (#1877), with the official Eventarc,
// Cloud Run, Pub/Sub and Storage clients, as Google's "Route events to
// Cloud Run" documents it. ListProviders and GetProvider name Pub/Sub's and
// Cloud Storage's event types. A messagePublished trigger on a topic, with
// a Cloud Run destination and a path, makes a push subscription on it; a
// published message reaches the service at the path as a binary-mode
// CloudEvent (ce-type, ce-source, ce-specversion, ce-id the message's,
// ce-time) with MessagePublishedData and an ID token for the trigger's
// account. A trigger with no topic gets one Eventarc makes. A Cloud Storage
// finalized trigger filtered on a bucket delivers an upload with
// ce-subject objects/NAME and the object. Get, List and an Update of the
// labels read and change them; a change of the event filters, an unknown
// event type, a missing channel and a missing service are refused. Delete removes
// what each trigger made, and leaves the user's topic.
func TestEventarcTriggersToCloudRun(t *testing.T) {
	h := New(t)
	storageAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	pubsubAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	rc := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
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
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	parent := runParent(h)
	base := ingress(t)

	// The probe service the triggers deliver to.
	service := "eventarc-probe"
	sop, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: service, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: eventProbeImage}},
			Scaling: &runpb.RevisionScaling{MinInstanceCount: 1, MaxInstanceCount: 1}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: parent + "/services/" + service})
	})
	svc, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("the probe service never became ready: %v", err)
	}
	host := hostOf(t, svc.GetUri())

	// Providers.
	var providers []string
	for it := ec.ListProviders(ctx, &eventarcpb.ListProvidersRequest{Parent: parent}); ; {
		p, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListProviders: %v", err)
		}
		providers = append(providers, p.GetName())
	}
	if p := strings.Join(providers, ","); !strings.Contains(p, parent+"/providers/pubsub.googleapis.com,") ||
		!strings.Contains(p, parent+"/providers/storage.googleapis.com") || !strings.Contains(p, parent+"/providers/secretmanager.googleapis.com,") {
		t.Errorf("ListProviders = %v", providers)
	}
	if p, err := ec.GetProvider(ctx, &eventarcpb.GetProviderRequest{Name: parent + "/providers/storage.googleapis.com"}); err != nil ||
		len(p.GetEventTypes()) != 5 || p.GetEventTypes()[0].GetType() != "google.cloud.storage.object.v1.finalized" ||
		p.GetEventTypes()[4].GetType() != "google.cloud.audit.log.v1.written" {
		t.Errorf("GetProvider storage = %v, %v", p, err)
	}

	create := func(id string, tr *eventarcpb.Trigger) *eventarcpb.Trigger {
		t.Helper()
		op, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: id, Trigger: tr})
		if err != nil {
			t.Fatalf("CreateTrigger %s: %v", id, err)
		}
		got, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateTrigger %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: got.GetName(), AllowMissing: true}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return got
	}
	dest := func(path string) *eventarcpb.Destination {
		return &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{
			Service: service, Region: "us-central1", Path: path}}}
	}
	typeFilter := func(typ string) *eventarcpb.EventFilter {
		return &eventarcpb.EventFilter{Attribute: "type", Value: typ}
	}

	// Refusals.
	for name, bad := range map[string]*eventarcpb.Trigger{
		"an unknown event type": {EventFilters: []*eventarcpb.EventFilter{typeFilter("google.cloud.example.v1.unknown")}, Destination: dest("")},
	} {
		if _, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "bad", Trigger: bad}); status.Code(err) != codes.Unimplemented {
			t.Errorf("CreateTrigger with %s: %v; want Unimplemented", name, err)
		}
	}
	// Channels are served since #1947 (TestEventarcChannelsToCloudRun): a
	// trigger on one that does not exist is INVALID_ARGUMENT.
	if _, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "bad", Trigger: &eventarcpb.Trigger{
		Channel: parent + "/channels/c", EventFilters: []*eventarcpb.EventFilter{typeFilter("x")}, Destination: dest("")}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateTrigger with a missing channel: %v; want InvalidArgument", err)
	}
	missing := &eventarcpb.Trigger{EventFilters: []*eventarcpb.EventFilter{typeFilter("google.cloud.pubsub.topic.v1.messagePublished")},
		Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{CloudRun: &eventarcpb.CloudRun{Service: "no-such-service"}}}}
	if _, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: "bad", Trigger: missing}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateTrigger to a missing service: %v; want InvalidArgument", err)
	}

	// Pub/Sub, on the user's topic.
	tn := topic(t, h, pc, "eventarc-run")
	sa := "events@" + h.Project() + ".iam.gserviceaccount.com"
	ps := create("run-pubsub", &eventarcpb.Trigger{
		EventFilters:   []*eventarcpb.EventFilter{typeFilter("google.cloud.pubsub.topic.v1.messagePublished")},
		ServiceAccount: sa, Destination: dest("/events/pubsub"),
		Transport: &eventarcpb.Transport{Intermediary: &eventarcpb.Transport_Pubsub{Pubsub: &eventarcpb.Pubsub{Topic: tn}}}})
	sub := ps.GetTransport().GetPubsub().GetSubscription()
	if ps.GetName() != parent+"/triggers/run-pubsub" || ps.GetUid() == "" || ps.GetTransport().GetPubsub().GetTopic() != tn ||
		!strings.HasPrefix(sub, "projects/"+h.Project()+"/subscriptions/eventarc-us-central1-run-pubsub-sub-") {
		t.Fatalf("the trigger = %v", ps)
	}
	if s := eventarcSubscription(ctx, t, pc, tn); s == nil || s.GetName() != sub || s.GetLabels()["goog-managed-by"] != "eventarc" {
		t.Errorf("the trigger's subscription = %v", s)
	}
	res, err := pc.TopicAdminClient.Publish(ctx, &pubsubpb.PublishRequest{Topic: tn, Messages: []*pubsubpb.PubsubMessage{
		{Data: []byte("hello run"), Attributes: map[string]string{"k": "v"}}}})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	msgID := res.GetMessageIds()[0]
	ev, all := probedEvents(t, base, host, func(e probedEvent) bool { return e.Headers["ce-id"] == msgID })
	if ev.Headers == nil {
		t.Fatalf("no CloudEvent with ce-id %s reached the service; it got %v", msgID, all)
	}
	for k, want := range map[string]string{"ce-type": "google.cloud.pubsub.topic.v1.messagePublished",
		"ce-source": "//pubsub.googleapis.com/" + tn, "ce-specversion": "1.0", "content-type": "application/json"} {
		if got := ev.Headers[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if ev.Path != "/events/pubsub" {
		t.Errorf("the event reached %q; want the destination's path /events/pubsub", ev.Path)
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
	if err := json.Unmarshal([]byte(ev.Data), &mpd); err != nil || mpd.Subscription != sub || mpd.Message.MessageID != msgID ||
		mpd.Message.Attributes["k"] != "v" || mpd.Message.Data != base64.StdEncoding.EncodeToString([]byte("hello run")) {
		t.Errorf("MessagePublishedData = %s (%v)", ev.Data, err)
	}

	// Pub/Sub, on a topic Eventarc makes.
	own := create("run-own-topic", &eventarcpb.Trigger{
		EventFilters: []*eventarcpb.EventFilter{typeFilter("google.cloud.pubsub.topic.v1.messagePublished")}, Destination: dest("")})
	ownTopic := own.GetTransport().GetPubsub().GetTopic()
	if !strings.HasPrefix(ownTopic, "projects/"+h.Project()+"/topics/eventarc-us-central1-run-own-topic-") {
		t.Errorf("the trigger's own topic = %q", ownTopic)
	}

	// Get, List, Update.
	if got, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: ps.GetName()}); err != nil || got.GetUid() != ps.GetUid() {
		t.Errorf("GetTrigger = %v, %v", got, err)
	}
	var listed []string
	for it := ec.ListTriggers(ctx, &eventarcpb.ListTriggersRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListTriggers: %v", err)
		}
		listed = append(listed, x.GetName())
	}
	if strings.Join(listed, ",") != own.GetName()+","+ps.GetName() {
		t.Errorf("ListTriggers = %v", listed)
	}
	uop, err := ec.UpdateTrigger(ctx, &eventarcpb.UpdateTriggerRequest{Trigger: &eventarcpb.Trigger{Name: ps.GetName(),
		Labels: map[string]string{"team": "events"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	if got, err := uop.Wait(ctx); err != nil || got.GetLabels()["team"] != "events" || got.GetDestination().GetCloudRun().GetPath() != "/events/pubsub" {
		t.Errorf("UpdateTrigger = %v, %v", got, err)
	}
	if _, err := ec.UpdateTrigger(ctx, &eventarcpb.UpdateTriggerRequest{Trigger: &eventarcpb.Trigger{Name: ps.GetName()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"event_filters"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateTrigger of the event filters: %v; want InvalidArgument", err)
	}

	// Cloud Storage.
	bh := bucket(t, h, sc)
	gs := create("run-storage", &eventarcpb.Trigger{Destination: dest(""), EventFilters: []*eventarcpb.EventFilter{
		typeFilter("google.cloud.storage.object.v1.finalized"), {Attribute: "bucket", Value: bh.BucketName()}}})
	if !strings.HasPrefix(gs.GetTransport().GetPubsub().GetTopic(), "projects/"+h.Project()+"/topics/eventarc-us-central1-run-storage-") {
		t.Errorf("the storage trigger's topic = %v", gs.GetTransport())
	}
	w := bh.Object("dir/eventarc.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("abcd"))
	if err := w.Close(); err != nil {
		t.Fatalf("upload: %v", err)
	}
	gev, all := probedEvents(t, base, host, func(e probedEvent) bool { return e.Headers["ce-subject"] == "objects/dir/eventarc.txt" })
	if gev.Headers == nil {
		t.Fatalf("no CloudEvent for dir/eventarc.txt reached the service; it got %v", all)
	}
	if gev.Headers["ce-type"] != "google.cloud.storage.object.v1.finalized" ||
		gev.Headers["ce-source"] != "//storage.googleapis.com/projects/_/buckets/"+bh.BucketName() || gev.Headers["ce-id"] == "" {
		t.Errorf("headers = %v", gev.Headers)
	}
	var obj struct{ Bucket, Name, Size string }
	if err := json.Unmarshal([]byte(gev.Data), &obj); err != nil || obj.Name != "dir/eventarc.txt" || obj.Bucket != bh.BucketName() || obj.Size != "4" {
		t.Errorf("StorageObjectData = %s (%v)", gev.Data, err)
	}

	// Delete removes what each trigger made.
	for _, tr := range []*eventarcpb.Trigger{ps, own, gs} {
		op, err := ec.DeleteTrigger(ctx, &eventarcpb.DeleteTriggerRequest{Name: tr.GetName()})
		if err != nil {
			t.Fatalf("DeleteTrigger: %v", err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("DeleteTrigger.Wait: %v", err)
		}
	}
	if _, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: ps.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTrigger after delete: %v; want NotFound", err)
	}
	if s := eventarcSubscription(ctx, t, pc, tn); s != nil {
		t.Errorf("the trigger's subscription %s outlived it", s.GetName())
	}
	if _, err := pc.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tn}); err != nil {
		t.Errorf("the user's topic is gone: %v", err)
	}
	for _, tp := range []string{ownTopic, gs.GetTransport().GetPubsub().GetTopic()} {
		if _, err := pc.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: tp}); err == nil {
			t.Errorf("Eventarc's topic %s outlived its trigger", tp)
		}
	}
	if ns, err := bh.Notifications(ctx); err != nil || len(ns) != 0 {
		t.Errorf("the bucket's notifications after delete = %v, %v; want none", ns, err)
	}
}

// TestGcloudEventarcTriggers (#1877), gcloud through the harness, as
// Google's "Route events to Cloud Run" documents it: gcloud eventarc
// providers list names Pub/Sub and Cloud Storage; triggers create with a
// messagePublished event filter, a Cloud Run destination and no transport
// topic makes one Eventarc owns; describe, list, update of the destination
// path, and delete, read back with the official client.
func TestGcloudEventarcTriggers(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	g := newGcloudSession(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	rc := runClient(t, h)
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	parent := "projects/" + g.project + "/locations/us-central1"
	service := "gcloud-eventarc"
	sop, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: service, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: "ghcr.io/knative/helloworld-go:latest"}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: parent + "/services/" + service})
	})
	if _, err := sop.Wait(ctx); err != nil {
		t.Fatalf("the service never became ready: %v", err)
	}

	if out := g.must("eventarc", "providers", "list", "--location=us-central1", "--format=value(name)"); !strings.Contains(out, "pubsub.googleapis.com") ||
		!strings.Contains(out, "storage.googleapis.com") {
		t.Errorf("providers list:\n%s", out)
	}
	g.must("eventarc", "triggers", "create", "gcloud-pubsub", "--location=us-central1",
		"--destination-run-service="+service, "--destination-run-region=us-central1",
		"--event-filters=type=google.cloud.pubsub.topic.v1.messagePublished")
	t.Cleanup(func() {
		if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: parent + "/triggers/gcloud-pubsub", AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if out := g.must("eventarc", "triggers", "describe", "gcloud-pubsub", "--location=us-central1",
		"--format=value(transport.pubsub.topic)"); !strings.Contains(lastLine(out), "/topics/eventarc-us-central1-gcloud-pubsub-") {
		t.Errorf("triggers describe: topic = %q", out)
	}
	if out := g.must("eventarc", "triggers", "list", "--location=us-central1", "--format=value(name)"); !strings.Contains(out, "gcloud-pubsub") {
		t.Errorf("triggers list does not list the trigger:\n%s", out)
	}
	g.must("eventarc", "triggers", "update", "gcloud-pubsub", "--location=us-central1", "--destination-run-path=/events")
	if tr, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: parent + "/triggers/gcloud-pubsub"}); err != nil ||
		tr.GetDestination().GetCloudRun().GetPath() != "/events" || tr.GetDestination().GetCloudRun().GetService() != service {
		t.Errorf("the trigger after update = %v, %v", tr, err)
	}
	g.must("eventarc", "triggers", "delete", "gcloud-pubsub", "--location=us-central1", "--quiet")
	if _, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: parent + "/triggers/gcloud-pubsub"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTrigger after triggers delete: %v; want NotFound", err)
	}

	// Cloud Audit Logs, with a path pattern (#1877).
	g.must("eventarc", "triggers", "create", "gcloud-audit", "--location=us-central1",
		"--destination-run-service="+service, "--destination-run-region=us-central1",
		"--event-filters=type=google.cloud.audit.log.v1.written", "--event-filters=serviceName=pubsub.googleapis.com",
		"--event-filters=methodName=google.pubsub.v1.Publisher.CreateTopic",
		"--event-filters-path-pattern=resourceName=projects/"+g.project+"/topics/audited-*")
	t.Cleanup(func() {
		if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: parent + "/triggers/gcloud-audit", AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if tr, err := ec.GetTrigger(ctx, &eventarcpb.GetTriggerRequest{Name: parent + "/triggers/gcloud-audit"}); err != nil || len(tr.GetEventFilters()) != 4 {
		t.Errorf("the audit-log trigger = %v, %v", tr, err)
	} else {
		for _, f := range tr.GetEventFilters() {
			if f.GetAttribute() == "resourceName" && f.GetOperator() != "match-path-pattern" {
				t.Errorf("resourceName's operator = %q; want match-path-pattern", f.GetOperator())
			}
		}
	}
	if out := g.must("eventarc", "providers", "describe", "pubsub.googleapis.com", "--location=us-central1"); !strings.Contains(out, "google.cloud.audit.log.v1.written") ||
		!strings.Contains(out, "pathPatternSupported: true") {
		t.Errorf("providers describe pubsub.googleapis.com:\n%s", out)
	}
	g.must("eventarc", "triggers", "delete", "gcloud-audit", "--location=us-central1", "--quiet")
	if out, err := g.run(nil, "eventarc", "triggers", "create", "gcloud-bad", "--location=us-central1",
		"--destination-run-service="+service, "--destination-run-region=us-central1",
		"--event-filters=type=google.cloud.audit.log.v1.written", "--event-filters=serviceName=pubsub.googleapis.com"); err == nil ||
		!strings.Contains(out, "methodName") {
		t.Errorf("an audit-log trigger with no methodName: %v\n%s", err, out)
	}
}
