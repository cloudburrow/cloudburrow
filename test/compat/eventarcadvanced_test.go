//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	eventarc "cloud.google.com/go/eventarc/apiv1"
	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	publishing "cloud.google.com/go/eventarc/publishing/apiv1"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// covers: google.cloud.eventarc.v1.Eventarc/CreateMessageBus, google.cloud.eventarc.v1.Eventarc/GetMessageBus, google.cloud.eventarc.v1.Eventarc/ListMessageBuses, google.cloud.eventarc.v1.Eventarc/UpdateMessageBus, google.cloud.eventarc.v1.Eventarc/DeleteMessageBus, google.cloud.eventarc.v1.Eventarc/ListMessageBusEnrollments, google.cloud.eventarc.v1.Eventarc/CreatePipeline, google.cloud.eventarc.v1.Eventarc/GetPipeline, google.cloud.eventarc.v1.Eventarc/ListPipelines, google.cloud.eventarc.v1.Eventarc/UpdatePipeline, google.cloud.eventarc.v1.Eventarc/DeletePipeline, google.cloud.eventarc.v1.Eventarc/CreateEnrollment, google.cloud.eventarc.v1.Eventarc/GetEnrollment, google.cloud.eventarc.v1.Eventarc/ListEnrollments, google.cloud.eventarc.v1.Eventarc/UpdateEnrollment, google.cloud.eventarc.v1.Eventarc/DeleteEnrollment, google.cloud.eventarc.v1.Eventarc/CreateGoogleApiSource, google.cloud.eventarc.v1.Eventarc/GetGoogleApiSource, google.cloud.eventarc.v1.Eventarc/ListGoogleApiSources, google.cloud.eventarc.v1.Eventarc/UpdateGoogleApiSource, google.cloud.eventarc.v1.Eventarc/DeleteGoogleApiSource, google.cloud.eventarc.publishing.v1.Publisher/Publish
//
// TestEventarcAdvancedBusToCloudRun (#1947), with the official Eventarc,
// Eventarc Publishing, Cloud Run and Pub/Sub clients, as Google's
// "Eventarc Advanced overview" and "Publish events directly" document
// them. A bus with a logging config, two pipelines (an HTTP endpoint that
// is a Cloud Run service, reached with a Google OIDC token, with a JSON
// input format, a transformation and a retry policy; and a Pub/Sub topic)
// and two enrollments (a CEL match on the type and the data, and true)
// are created, read, listed and changed. Publish puts a JSON CloudEvent on
// the bus: it reaches the service transformed, in binary content mode,
// and the topic in the Pub/Sub binding; an event the first match refuses
// reaches only the topic. A Google API source on the bus then puts the
// audit-log event of an Admin Activity call (a CreateMessageBus) on it,
// which an enrollment on its type sends to the service. Everything is
// deleted.
func TestEventarcAdvancedBusToCloudRun(t *testing.T) {
	h := New(t)
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
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	pub, err := publishing.NewPublisherClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	parent := runParent(h)
	base := ingress(t)

	service := "eventarc-adv-probe"
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
	compute := strconv.FormatInt(resource.ProjectNumber(h.Project()), 10) + "-compute@developer.gserviceaccount.com"

	// The bus.
	busName := parent + "/messageBuses/adv-bus"
	bop, err := ec.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: parent, MessageBusId: "adv-bus", MessageBus: &eventarcpb.MessageBus{
		DisplayName: "orders", LoggingConfig: &eventarcpb.LoggingConfig{LogSeverity: eventarcpb.LoggingConfig_DEBUG}}})
	if err != nil {
		t.Fatalf("CreateMessageBus: %v", err)
	}
	bus, err := bop.Wait(ctx)
	if err != nil || bus.GetName() != busName || bus.GetUid() == "" || bus.GetEtag() == "" || bus.GetCreateTime() == nil {
		t.Fatalf("CreateMessageBus = %v, %v", bus, err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteMessageBus(context.Background(), &eventarcpb.DeleteMessageBusRequest{Name: busName, AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if _, err := ec.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: parent, MessageBusId: "adv-bus", MessageBus: &eventarcpb.MessageBus{}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second adv-bus = %v", err)
	}
	if _, err := ec.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: parent, MessageBusId: "Bad_ID", MessageBus: &eventarcpb.MessageBus{}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a bad bus ID = %v", err)
	}
	uop, err := ec.UpdateMessageBus(ctx, &eventarcpb.UpdateMessageBusRequest{MessageBus: &eventarcpb.MessageBus{Name: busName, DisplayName: "all orders",
		Labels: map[string]string{"team": "shop"}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "labels"}}})
	if err != nil {
		t.Fatalf("UpdateMessageBus: %v", err)
	}
	if got, err := uop.Wait(ctx); err != nil || got.GetDisplayName() != "all orders" || got.GetLabels()["team"] != "shop" ||
		got.GetLoggingConfig().GetLogSeverity() != eventarcpb.LoggingConfig_DEBUG || got.GetEtag() == bus.GetEtag() {
		t.Errorf("UpdateMessageBus = %v, %v", got, err)
	}
	if got, err := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: busName}); err != nil || got.GetDisplayName() != "all orders" {
		t.Errorf("GetMessageBus = %v, %v", got, err)
	}
	var buses []string
	for it := ec.ListMessageBuses(ctx, &eventarcpb.ListMessageBusesRequest{Parent: parent}); ; {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListMessageBuses: %v", err)
		}
		buses = append(buses, b.GetName())
	}
	if !strings.Contains(strings.Join(buses, ","), busName) {
		t.Errorf("ListMessageBuses = %v", buses)
	}

	// The pipelines.
	tn := topic(t, h, pc, "eventarc-adv-out")
	sub := subscription(t, h, pc, "eventarc-adv-out", tn)
	jsonFmt := &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Json{Json: &eventarcpb.Pipeline_MessagePayloadFormat_JsonFormat{}}}
	createPipeline := func(id string, p *eventarcpb.Pipeline) *eventarcpb.Pipeline {
		t.Helper()
		op, err := ec.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: parent, PipelineId: id, Pipeline: p})
		if err != nil {
			t.Fatalf("CreatePipeline %s: %v", id, err)
		}
		got, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreatePipeline %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := ec.DeletePipeline(context.Background(), &eventarcpb.DeletePipelineRequest{Name: got.GetName(), AllowMissing: true}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return got
	}
	toRun := createPipeline("adv-run", &eventarcpb.Pipeline{
		Destinations: []*eventarcpb.Pipeline_Destination{{
			DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: strings.TrimSuffix(svc.GetUri(), "/") + "/adv"}},
			NetworkConfig:         &eventarcpb.Pipeline_Destination_NetworkConfig{NetworkAttachment: "projects/" + h.Project() + "/regions/us-central1/networkAttachments/adv"},
			AuthenticationConfig: &eventarcpb.Pipeline_Destination_AuthenticationConfig{AuthenticationMethodDescriptor: &eventarcpb.Pipeline_Destination_AuthenticationConfig_GoogleOidc{
				GoogleOidc: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OidcToken{ServiceAccount: compute}}}}},
		InputPayloadFormat: jsonFmt,
		Mediations: []*eventarcpb.Pipeline_Mediation{{MediationDescriptor: &eventarcpb.Pipeline_Mediation_Transformation_{
			Transformation: &eventarcpb.Pipeline_Mediation_Transformation{TransformationTemplate: `message.removeFields(["data.card"]).setField("data.via", "pipeline")`}}}},
		RetryPolicy:   &eventarcpb.Pipeline_RetryPolicy{MaxAttempts: 3, MinRetryDelay: durationpb.New(time.Second), MaxRetryDelay: durationpb.New(5 * time.Second)},
		LoggingConfig: &eventarcpb.LoggingConfig{LogSeverity: eventarcpb.LoggingConfig_INFO}})
	toTopic := createPipeline("adv-topic", &eventarcpb.Pipeline{Destinations: []*eventarcpb.Pipeline_Destination{{
		DestinationDescriptor: &eventarcpb.Pipeline_Destination_Topic{Topic: tn}}}})
	for name, bad := range map[string]*eventarcpb.Pipeline{
		"no destination":   {},
		"a workflow":       {Destinations: []*eventarcpb.Pipeline_Destination{{DestinationDescriptor: &eventarcpb.Pipeline_Destination_Workflow{Workflow: parent + "/workflows/w"}}}},
		"an http uri":      {Destinations: []*eventarcpb.Pipeline_Destination{{DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: "http://10.0.0.1/x"}}}}},
		"a bad template":   {Destinations: toTopic.GetDestinations(), Mediations: []*eventarcpb.Pipeline_Mediation{{MediationDescriptor: &eventarcpb.Pipeline_Mediation_Transformation_{Transformation: &eventarcpb.Pipeline_Mediation_Transformation{TransformationTemplate: "message.("}}}}},
		"101 attempts":     {Destinations: toTopic.GetDestinations(), RetryPolicy: &eventarcpb.Pipeline_RetryPolicy{MaxAttempts: 101}},
		"output, no input": {Destinations: []*eventarcpb.Pipeline_Destination{{OutputPayloadFormat: jsonFmt, DestinationDescriptor: &eventarcpb.Pipeline_Destination_Topic{Topic: tn}}}},
	} {
		_, err := ec.CreatePipeline(ctx, &eventarcpb.CreatePipelineRequest{Parent: parent, PipelineId: "adv-bad", Pipeline: bad})
		if want := codes.InvalidArgument; name == "a workflow" {
			if status.Code(err) != codes.Unimplemented {
				t.Errorf("CreatePipeline with %s = %v; want Unimplemented", name, err)
			}
		} else if status.Code(err) != want {
			t.Errorf("CreatePipeline with %s = %v; want %v", name, err, want)
		}
	}
	upop, err := ec.UpdatePipeline(ctx, &eventarcpb.UpdatePipelineRequest{Pipeline: &eventarcpb.Pipeline{Name: toTopic.GetName(), DisplayName: "to Pub/Sub"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdatePipeline: %v", err)
	}
	if got, err := upop.Wait(ctx); err != nil || got.GetDisplayName() != "to Pub/Sub" || got.GetDestinations()[0].GetTopic() != tn {
		t.Errorf("UpdatePipeline = %v, %v", got, err)
	}
	if got, err := ec.GetPipeline(ctx, &eventarcpb.GetPipelineRequest{Name: toRun.GetName()}); err != nil || got.GetRetryPolicy().GetMaxAttempts() != 3 {
		t.Errorf("GetPipeline = %v, %v", got, err)
	}
	n := 0
	for it := ec.ListPipelines(ctx, &eventarcpb.ListPipelinesRequest{Parent: parent}); ; {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListPipelines: %v", err)
		}
		n++
	}
	if n < 2 {
		t.Errorf("ListPipelines listed %d", n)
	}

	// The enrollments.
	createEnrollment := func(id, match, pipeline string) *eventarcpb.Enrollment {
		t.Helper()
		op, err := ec.CreateEnrollment(ctx, &eventarcpb.CreateEnrollmentRequest{Parent: parent, EnrollmentId: id, Enrollment: &eventarcpb.Enrollment{
			CelMatch: match, MessageBus: busName, Destination: pipeline}})
		if err != nil {
			t.Fatalf("CreateEnrollment %s: %v", id, err)
		}
		got, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateEnrollment %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := ec.DeleteEnrollment(context.Background(), &eventarcpb.DeleteEnrollmentRequest{Name: got.GetName(), AllowMissing: true}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return got
	}
	orders := createEnrollment("adv-orders", `message.type == "com.example.order.v1" && message.data.total > 100`, toRun.GetName())
	createEnrollment("adv-all", "true", toTopic.GetName())
	if _, err := ec.CreateEnrollment(ctx, &eventarcpb.CreateEnrollmentRequest{Parent: parent, EnrollmentId: "adv-bad", Enrollment: &eventarcpb.Enrollment{
		CelMatch: "message.type ==", MessageBus: busName, Destination: toRun.GetName()}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a bad CEL match = %v", err)
	}
	eop, err := ec.UpdateEnrollment(ctx, &eventarcpb.UpdateEnrollmentRequest{Enrollment: &eventarcpb.Enrollment{Name: orders.GetName(),
		CelMatch: `message.type == "com.example.order.v1" && message.data.total > 10`}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cel_match"}}})
	if err != nil {
		t.Fatalf("UpdateEnrollment: %v", err)
	}
	if got, err := eop.Wait(ctx); err != nil || !strings.HasSuffix(got.GetCelMatch(), "> 10") {
		t.Errorf("UpdateEnrollment = %v, %v", got, err)
	}
	if got, err := ec.GetEnrollment(ctx, &eventarcpb.GetEnrollmentRequest{Name: orders.GetName()}); err != nil || got.GetMessageBus() != busName {
		t.Errorf("GetEnrollment = %v, %v", got, err)
	}
	var enrolled []string
	for it := ec.ListMessageBusEnrollments(ctx, &eventarcpb.ListMessageBusEnrollmentsRequest{Parent: busName}); ; {
		n, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListMessageBusEnrollments: %v", err)
		}
		enrolled = append(enrolled, n)
	}
	if strings.Join(enrolled, ",") != parent+"/enrollments/adv-all,"+parent+"/enrollments/adv-orders" {
		t.Errorf("ListMessageBusEnrollments = %v", enrolled)
	}
	m := 0
	for it := ec.ListEnrollments(ctx, &eventarcpb.ListEnrollmentsRequest{Parent: parent}); ; {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListEnrollments: %v", err)
		}
		m++
	}
	if m < 2 {
		t.Errorf("ListEnrollments listed %d", m)
	}

	// Publish.
	if _, err := pub.Publish(ctx, &publishingpb.PublishRequest{MessageBus: busName, Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","id":"adv-1","source":"//shop/orders","type":"com.example.order.v1","datacontenttype":"application/json","region":"eu","data":{"total":42,"card":"4111"}}`}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := pub.Publish(ctx, &publishingpb.PublishRequest{MessageBus: busName, Format: &publishingpb.PublishRequest_ProtoMessage_{
		ProtoMessage_: &publishingpb.CloudEvent{Id: "adv-2", Source: "//shop/orders", SpecVersion: "1.0", Type: "com.example.order.v1",
			Attributes: map[string]*publishingpb.CloudEvent_CloudEventAttributeValue{"datacontenttype": {Attr: &publishingpb.CloudEvent_CloudEventAttributeValue_CeString{CeString: "application/json"}}},
			Data:       &publishingpb.CloudEvent_TextData{TextData: `{"total":3}`}}}}); err != nil {
		t.Fatalf("Publish proto_message: %v", err)
	}
	if _, err := pub.Publish(ctx, &publishingpb.PublishRequest{MessageBus: busName, Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","id":"adv-x","source":"//shop"}`}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("Publish with no type = %v", err)
	}
	ev, all := probedEvents(t, base, host, func(e probedEvent) bool { return e.Headers["ce-id"] == "adv-1" })
	if ev.Headers["ce-id"] == "" {
		t.Fatalf("the bus's event never reached the service; it got %v", all)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(ev.Data), &data); err != nil || data["via"] != "pipeline" || data["card"] != nil || data["total"] != float64(42) ||
		ev.Path != "/adv" || ev.Headers["ce-type"] != "com.example.order.v1" || ev.Headers["ce-region"] != "eu" ||
		!strings.HasPrefix(ev.Headers["content-type"], "application/json") {
		t.Errorf("the service got %+v (%v)", ev, err)
	}
	for _, e := range all {
		if e.Headers["ce-id"] == "adv-2" {
			t.Errorf("adv-2 (total 3) reached the service: the match is total > 10")
		}
	}
	// The topic pipeline gets both, in the Pub/Sub binding.
	got := map[string]*pubsubpb.PubsubMessage{}
	deadline := time.Now().Add(90 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		resp, err := pc.SubscriptionAdminClient.Pull(ctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		if err != nil {
			t.Fatalf("Pull: %v", err)
		}
		for _, rm := range resp.GetReceivedMessages() {
			got[rm.GetMessage().GetAttributes()["ce-id"]] = rm.GetMessage()
			_ = pc.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: []string{rm.GetAckId()}})
		}
	}
	if m := got["adv-2"]; m == nil || m.GetAttributes()["ce-type"] != "com.example.order.v1" || string(m.GetData()) != `{"total":3}` {
		t.Errorf("the topic got %v", got)
	}

	// A Google API source puts audit-log events on the bus.
	sop2, err := ec.CreateGoogleApiSource(ctx, &eventarcpb.CreateGoogleApiSourceRequest{Parent: parent, GoogleApiSourceId: "adv-google",
		GoogleApiSource: &eventarcpb.GoogleApiSource{Destination: busName, LoggingConfig: &eventarcpb.LoggingConfig{LogSeverity: eventarcpb.LoggingConfig_INFO}}})
	if err != nil {
		t.Fatalf("CreateGoogleApiSource: %v", err)
	}
	src, err := sop2.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateGoogleApiSource: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteGoogleApiSource(context.Background(), &eventarcpb.DeleteGoogleApiSourceRequest{Name: src.GetName(), AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if _, err := ec.CreateGoogleApiSource(ctx, &eventarcpb.CreateGoogleApiSourceRequest{Parent: parent, GoogleApiSourceId: "adv-nobus",
		GoogleApiSource: &eventarcpb.GoogleApiSource{Destination: parent + "/messageBuses/none"}}); status.Code(err) != codes.NotFound {
		t.Errorf("a source to a missing bus = %v", err)
	}
	gop, err := ec.UpdateGoogleApiSource(ctx, &eventarcpb.UpdateGoogleApiSourceRequest{GoogleApiSource: &eventarcpb.GoogleApiSource{Name: src.GetName(),
		DisplayName: "audit"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateGoogleApiSource: %v", err)
	}
	if g, err := gop.Wait(ctx); err != nil || g.GetDisplayName() != "audit" || g.GetDestination() != busName {
		t.Errorf("UpdateGoogleApiSource = %v, %v", g, err)
	}
	if g, err := ec.GetGoogleApiSource(ctx, &eventarcpb.GetGoogleApiSourceRequest{Name: src.GetName()}); err != nil || g.GetDisplayName() != "audit" {
		t.Errorf("GetGoogleApiSource = %v, %v", g, err)
	}
	if it := ec.ListGoogleApiSources(ctx, &eventarcpb.ListGoogleApiSourcesRequest{Parent: parent}); true {
		if g, err := it.Next(); err != nil || g.GetName() != src.GetName() {
			t.Errorf("ListGoogleApiSources = %v, %v", g, err)
		}
	}
	createEnrollment("adv-audit", `message.type == "google.cloud.audit.log.v1.written" && message.methodname.endsWith("CreateMessageBus")`, toRun.GetName())
	// The Admin Activity call: a CreateMessageBus.
	cop, err := ec.CreateMessageBus(ctx, &eventarcpb.CreateMessageBusRequest{Parent: parent, MessageBusId: "adv-other", MessageBus: &eventarcpb.MessageBus{}})
	if err != nil {
		t.Fatalf("CreateMessageBus adv-other: %v", err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := ec.DeleteMessageBus(context.Background(), &eventarcpb.DeleteMessageBusRequest{Name: parent + "/messageBuses/adv-other", AllowMissing: true}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	aev, all := probedEvents(t, base, host, func(e probedEvent) bool {
		return e.Headers["ce-type"] == "google.cloud.audit.log.v1.written" && strings.HasSuffix(e.Headers["ce-methodname"], "CreateMessageBus")
	})
	if aev.Headers["ce-type"] == "" {
		t.Fatalf("the Google API source's audit-log event never reached the service; it got %d events", len(all))
	}
	if !strings.Contains(aev.Data, `"via":"pipeline"`) || !strings.Contains(aev.Data, "cloudaudit.googleapis.com") ||
		aev.Headers["ce-servicename"] != "eventarc.googleapis.com" {
		t.Errorf("the audit-log event = %+v", aev)
	}

	// Delete, and what is gone is gone.
	for _, del := range []func() (string, error){
		func() (string, error) {
			op, err := ec.DeleteGoogleApiSource(ctx, &eventarcpb.DeleteGoogleApiSourceRequest{Name: src.GetName()})
			if err == nil {
				_, err = op.Wait(ctx)
			}
			return "DeleteGoogleApiSource", err
		},
		func() (string, error) {
			op, err := ec.DeleteEnrollment(ctx, &eventarcpb.DeleteEnrollmentRequest{Name: orders.GetName()})
			if err == nil {
				_, err = op.Wait(ctx)
			}
			return "DeleteEnrollment", err
		},
		func() (string, error) {
			op, err := ec.DeletePipeline(ctx, &eventarcpb.DeletePipelineRequest{Name: toRun.GetName()})
			if err == nil {
				_, err = op.Wait(ctx)
			}
			return "DeletePipeline", err
		},
		func() (string, error) {
			cur, _ := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: busName})
			if _, err := ec.DeleteMessageBus(ctx, &eventarcpb.DeleteMessageBusRequest{Name: busName, Etag: "stale"}); status.Code(err) != codes.Aborted {
				t.Errorf("DeleteMessageBus with a stale etag = %v", err)
			}
			op, err := ec.DeleteMessageBus(ctx, &eventarcpb.DeleteMessageBusRequest{Name: busName, Etag: cur.GetEtag()})
			if err == nil {
				_, err = op.Wait(ctx)
			}
			return "DeleteMessageBus", err
		},
	} {
		if name, err := del(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ec.GetMessageBus(ctx, &eventarcpb.GetMessageBusRequest{Name: busName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetMessageBus after delete = %v", err)
	}
	if _, err := ec.GetEnrollment(ctx, &eventarcpb.GetEnrollmentRequest{Name: orders.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetEnrollment after delete = %v", err)
	}
}

// covers: google.cloud.eventarc.v1.Eventarc/CreateChannel, google.cloud.eventarc.v1.Eventarc/GetChannel, google.cloud.eventarc.v1.Eventarc/ListChannels, google.cloud.eventarc.v1.Eventarc/UpdateChannel, google.cloud.eventarc.v1.Eventarc/DeleteChannel, google.cloud.eventarc.v1.Eventarc/CreateChannelConnection, google.cloud.eventarc.v1.Eventarc/GetChannelConnection, google.cloud.eventarc.v1.Eventarc/ListChannelConnections, google.cloud.eventarc.v1.Eventarc/DeleteChannelConnection, google.cloud.eventarc.v1.Eventarc/GetGoogleChannelConfig, google.cloud.eventarc.v1.Eventarc/UpdateGoogleChannelConfig, google.cloud.eventarc.publishing.v1.Publisher/PublishEvents, google.cloud.eventarc.publishing.v1.Publisher/PublishChannelConnectionEvents
//
// TestEventarcChannelsToCloudRun (#1947), as Google's "Publish events
// from custom sources" and "Third-party providers" pages document them: a
// channel with no provider is ACTIVE, and a trigger on it with a custom
// event type and an attribute filter delivers PublishEvents' matching
// events to a Cloud Run service; a channel for a provider is PENDING with
// an activation token until a channel connection gives it, and
// PublishChannelConnectionEvents then reaches the channel's trigger.
// GetGoogleChannelConfig and UpdateGoogleChannelConfig keep a CMEK key.
func TestEventarcChannelsToCloudRun(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_PUBSUB", EnvPubSub)
	rc := runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	loadEventProbe(ctx, t)
	ec, err := eventarc.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ec.Close() })
	pub, err := publishing.NewPublisherClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pub.Close() })
	parent := runParent(h)
	base := ingress(t)
	service := "eventarc-chan-probe"
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

	createChannel := func(id, provider string) *eventarcpb.Channel {
		t.Helper()
		op, err := ec.CreateChannel(ctx, &eventarcpb.CreateChannelRequest{Parent: parent, ChannelId: id, Channel: &eventarcpb.Channel{Provider: provider}})
		if err != nil {
			t.Fatalf("CreateChannel %s: %v", id, err)
		}
		ch, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateChannel %s: %v", id, err)
		}
		t.Cleanup(func() {
			if op, err := ec.DeleteChannel(context.Background(), &eventarcpb.DeleteChannelRequest{Name: ch.GetName()}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return ch
	}
	createTrigger := func(id, channel string, filters ...*eventarcpb.EventFilter) {
		t.Helper()
		op, err := ec.CreateTrigger(ctx, &eventarcpb.CreateTriggerRequest{Parent: parent, TriggerId: id, Trigger: &eventarcpb.Trigger{
			Channel: channel, EventFilters: filters, Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_CloudRun{
				CloudRun: &eventarcpb.CloudRun{Service: service, Region: "us-central1", Path: "/" + id}}}}})
		if err != nil {
			t.Fatalf("CreateTrigger %s: %v", id, err)
		}
		tr, err := op.Wait(ctx)
		if err != nil || tr.GetChannel() != channel {
			t.Fatalf("CreateTrigger %s = %v, %v", id, tr, err)
		}
		t.Cleanup(func() {
			if op, err := ec.DeleteTrigger(context.Background(), &eventarcpb.DeleteTriggerRequest{Name: tr.GetName(), AllowMissing: true}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
	}
	event := func(id, typ, color, data string) *anypb.Any {
		b, _ := proto.Marshal(&publishingpb.CloudEvent{Id: id, Source: "//app", SpecVersion: "1.0", Type: typ,
			Attributes: map[string]*publishingpb.CloudEvent_CloudEventAttributeValue{
				"color":           {Attr: &publishingpb.CloudEvent_CloudEventAttributeValue_CeString{CeString: color}},
				"datacontenttype": {Attr: &publishingpb.CloudEvent_CloudEventAttributeValue_CeString{CeString: "application/json"}}},
			Data: &publishingpb.CloudEvent_TextData{TextData: data}})
		return &anypb.Any{TypeUrl: "type.googleapis.com/io.cloudevents.v1.CloudEvent", Value: b}
	}

	// A custom channel.
	ch := createChannel("adv-custom", "")
	if ch.GetState() != eventarcpb.Channel_ACTIVE || ch.GetActivationToken() != "" {
		t.Errorf("a custom channel = %v", ch)
	}
	createTrigger("chan-red", ch.GetName(), &eventarcpb.EventFilter{Attribute: "type", Value: "com.example.custom.v1"},
		&eventarcpb.EventFilter{Attribute: "color", Value: "red"})
	if _, err := pub.PublishEvents(ctx, &publishingpb.PublishEventsRequest{Channel: ch.GetName(), Events: []*anypb.Any{
		event("chan-1", "com.example.custom.v1", "red", `{"n":1}`), event("chan-2", "com.example.custom.v1", "blue", `{"n":2}`)}}); err != nil {
		t.Fatalf("PublishEvents: %v", err)
	}
	if _, err := pub.PublishEvents(ctx, &publishingpb.PublishEventsRequest{Channel: ch.GetName(), TextEvents: []string{
		`{"specversion":"1.0","id":"chan-3","source":"//app","type":"com.example.custom.v1","color":"red","datacontenttype":"application/json","data":{"n":3}}`}}); err != nil {
		t.Fatalf("PublishEvents text_events: %v", err)
	}
	for _, id := range []string{"chan-1", "chan-3"} {
		ev, all := probedEvents(t, base, host, func(e probedEvent) bool { return e.Headers["ce-id"] == id })
		if ev.Path != "/chan-red" || ev.Headers["ce-color"] != "red" || ev.Headers["ce-type"] != "com.example.custom.v1" || !strings.Contains(ev.Data, `"n"`) {
			t.Errorf("%s reached the service as %+v (all %d)", id, ev, len(all))
		}
		for _, e := range all {
			if e.Headers["ce-id"] == "chan-2" {
				t.Error("the blue event reached the red trigger")
			}
		}
	}
	upd, err := ec.UpdateChannel(ctx, &eventarcpb.UpdateChannelRequest{Channel: &eventarcpb.Channel{Name: ch.GetName(), Labels: map[string]string{"k": "v"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if got, err := upd.Wait(ctx); err != nil || got.GetLabels()["k"] != "v" || got.GetState() != eventarcpb.Channel_ACTIVE {
		t.Errorf("UpdateChannel = %v, %v", got, err)
	}

	// A provider's channel.
	pch := createChannel("adv-provider", parent+"/providers/acme")
	if pch.GetState() != eventarcpb.Channel_PENDING || pch.GetActivationToken() == "" || pch.GetProvider() != parent+"/providers/acme" {
		t.Fatalf("a provider's channel = %v", pch)
	}
	if _, err := pub.PublishEvents(ctx, &publishingpb.PublishEventsRequest{Channel: pch.GetName(), Events: []*anypb.Any{event("p-0", "acme.v1", "red", "{}")}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("PublishEvents to a PENDING channel = %v", err)
	}
	createTrigger("chan-acme", pch.GetName(), &eventarcpb.EventFilter{Attribute: "type", Value: "acme.v1"})
	if _, err := ec.CreateChannelConnection(ctx, &eventarcpb.CreateChannelConnectionRequest{Parent: parent, ChannelConnectionId: "adv-bad",
		ChannelConnection: &eventarcpb.ChannelConnection{Channel: pch.GetName(), ActivationToken: "wrong"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a connection with the wrong token = %v", err)
	}
	cop, err := ec.CreateChannelConnection(ctx, &eventarcpb.CreateChannelConnectionRequest{Parent: parent, ChannelConnectionId: "adv-conn",
		ChannelConnection: &eventarcpb.ChannelConnection{Channel: pch.GetName(), ActivationToken: pch.GetActivationToken()}})
	if err != nil {
		t.Fatalf("CreateChannelConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateChannelConnection: %v", err)
	}
	if got, err := ec.GetChannel(ctx, &eventarcpb.GetChannelRequest{Name: pch.GetName()}); err != nil || got.GetState() != eventarcpb.Channel_ACTIVE {
		t.Errorf("the provider's channel after the connection = %v, %v", got, err)
	}
	if got, err := ec.GetChannelConnection(ctx, &eventarcpb.GetChannelConnectionRequest{Name: conn.GetName()}); err != nil || got.GetChannel() != pch.GetName() {
		t.Errorf("GetChannelConnection = %v, %v", got, err)
	}
	if it := ec.ListChannelConnections(ctx, &eventarcpb.ListChannelConnectionsRequest{Parent: parent}); true {
		if c, err := it.Next(); err != nil || c.GetName() != conn.GetName() {
			t.Errorf("ListChannelConnections = %v, %v", c, err)
		}
	}
	var chans []string
	for it := ec.ListChannels(ctx, &eventarcpb.ListChannelsRequest{Parent: parent}); ; {
		c, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListChannels: %v", err)
		}
		chans = append(chans, c.GetName())
	}
	if strings.Join(chans, ",") != ch.GetName()+","+pch.GetName() && !strings.Contains(strings.Join(chans, ","), pch.GetName()) {
		t.Errorf("ListChannels = %v", chans)
	}
	if _, err := pub.PublishChannelConnectionEvents(ctx, &publishingpb.PublishChannelConnectionEventsRequest{ChannelConnection: conn.GetName(),
		Events: []*anypb.Any{event("acme-1", "acme.v1", "green", `{"acme":true}`)}}); err != nil {
		t.Fatalf("PublishChannelConnectionEvents: %v", err)
	}
	if ev, all := probedEvents(t, base, host, func(e probedEvent) bool { return e.Headers["ce-id"] == "acme-1" }); ev.Path != "/chan-acme" {
		t.Errorf("the provider's event = %+v (all %d)", ev, len(all))
	}
	dop, err := ec.DeleteChannelConnection(ctx, &eventarcpb.DeleteChannelConnectionRequest{Name: conn.GetName()})
	if err == nil {
		_, err = dop.Wait(ctx)
	}
	if err != nil {
		t.Errorf("DeleteChannelConnection: %v", err)
	}

	// The Google channel config.
	cfgName := parent + "/googleChannelConfig"
	if c, err := ec.GetGoogleChannelConfig(ctx, &eventarcpb.GetGoogleChannelConfigRequest{Name: cfgName}); err != nil || c.GetName() != cfgName {
		t.Errorf("GetGoogleChannelConfig = %v, %v", c, err)
	}
	key := "projects/" + h.Project() + "/locations/us-central1/keyRings/r/cryptoKeys/k"
	if c, err := ec.UpdateGoogleChannelConfig(ctx, &eventarcpb.UpdateGoogleChannelConfigRequest{GoogleChannelConfig: &eventarcpb.GoogleChannelConfig{
		Name: cfgName, CryptoKeyName: key}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"crypto_key_name"}}}); err != nil || c.GetCryptoKeyName() != key {
		t.Errorf("UpdateGoogleChannelConfig = %v, %v", c, err)
	}
	if c, err := ec.GetGoogleChannelConfig(ctx, &eventarcpb.GetGoogleChannelConfigRequest{Name: cfgName}); err != nil || c.GetCryptoKeyName() != key || c.GetUpdateTime() == nil {
		t.Errorf("GetGoogleChannelConfig after the update = %v, %v", c, err)
	}
	if _, err := ec.UpdateGoogleChannelConfig(ctx, &eventarcpb.UpdateGoogleChannelConfigRequest{GoogleChannelConfig: &eventarcpb.GoogleChannelConfig{
		Name: cfgName, CryptoKeyName: "not-a-key"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateGoogleChannelConfig with a bad key = %v", err)
	}
	_, _ = ec.UpdateGoogleChannelConfig(ctx, &eventarcpb.UpdateGoogleChannelConfigRequest{GoogleChannelConfig: &eventarcpb.GoogleChannelConfig{Name: cfgName},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"crypto_key_name"}}})
}
