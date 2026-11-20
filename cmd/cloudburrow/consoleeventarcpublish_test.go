package main

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// eaFake is an Eventarc and Publishing API that lists fixed resources and
// records the updates and publishes it is sent.
type eaFake struct {
	eventarcpb.UnimplementedEventarcServer
	publishingpb.UnimplementedPublisherServer
	mu        sync.Mutex
	bus       *eventarcpb.MessageBus
	pipeline  *eventarcpb.Pipeline
	source    *eventarcpb.GoogleApiSource
	plUpdate  *eventarcpb.UpdatePipelineRequest
	srcUpdate *eventarcpb.UpdateGoogleApiSourceRequest
	published *publishingpb.PublishRequest
}

func eaDone(m proto.Message) (*longrunningpb.Operation, error) {
	a, err := anypb.New(m)
	if err != nil {
		return nil, err
	}
	return &longrunningpb.Operation{Name: "operations/x", Done: true, Result: &longrunningpb.Operation_Response{Response: a}}, nil
}

func (f *eaFake) ListMessageBuses(context.Context, *eventarcpb.ListMessageBusesRequest) (*eventarcpb.ListMessageBusesResponse, error) {
	return &eventarcpb.ListMessageBusesResponse{MessageBuses: []*eventarcpb.MessageBus{f.bus}}, nil
}

func (f *eaFake) ListPipelines(context.Context, *eventarcpb.ListPipelinesRequest) (*eventarcpb.ListPipelinesResponse, error) {
	return &eventarcpb.ListPipelinesResponse{Pipelines: []*eventarcpb.Pipeline{f.pipeline}}, nil
}

func (f *eaFake) ListGoogleApiSources(context.Context, *eventarcpb.ListGoogleApiSourcesRequest) (*eventarcpb.ListGoogleApiSourcesResponse, error) {
	return &eventarcpb.ListGoogleApiSourcesResponse{GoogleApiSources: []*eventarcpb.GoogleApiSource{f.source}}, nil
}

func (f *eaFake) UpdatePipeline(_ context.Context, r *eventarcpb.UpdatePipelineRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	f.plUpdate = r
	f.mu.Unlock()
	return eaDone(r.GetPipeline())
}

func (f *eaFake) UpdateGoogleApiSource(_ context.Context, r *eventarcpb.UpdateGoogleApiSourceRequest) (*longrunningpb.Operation, error) {
	f.mu.Lock()
	f.srcUpdate = r
	f.mu.Unlock()
	return eaDone(r.GetGoogleApiSource())
}

func (f *eaFake) Publish(_ context.Context, r *publishingpb.PublishRequest) (*publishingpb.PublishResponse, error) {
	f.mu.Lock()
	f.published = r
	f.mu.Unlock()
	return &publishingpb.PublishResponse{}, nil
}

// TestEventarcAdvancedEditAndPublish (#2006): a pipeline's page has
// Google's Edit, prefilled with every setting the pipeline holds, whose
// Save sends UpdatePipeline with those settings in its mask and the etag
// read; a Google API source's page has Edit, sending
// UpdateGoogleApiSource; and a bus's page has Publish message, which puts
// a structured CloudEvent on the bus through the Publishing API with
// gcloud's event fields.
func TestEventarcAdvancedEditAndPublish(t *testing.T) {
	loc := "projects/demo/locations/us-central1"
	fake := &eaFake{
		bus: &eventarcpb.MessageBus{Name: loc + "/messageBuses/orders", Etag: "b1"},
		pipeline: &eventarcpb.Pipeline{Name: loc + "/pipelines/to-http", Etag: "p1", DisplayName: "Orders",
			Destinations: []*eventarcpb.Pipeline_Destination{{DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{
				HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: "https://hook.example/in"}},
				AuthenticationConfig: &eventarcpb.Pipeline_Destination_AuthenticationConfig{AuthenticationMethodDescriptor: &eventarcpb.Pipeline_Destination_AuthenticationConfig_GoogleOidc{
					GoogleOidc: &eventarcpb.Pipeline_Destination_AuthenticationConfig_OidcToken{ServiceAccount: "sa@demo.iam.gserviceaccount.com"}}}}},
			InputPayloadFormat: &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Json{Json: &eventarcpb.Pipeline_MessagePayloadFormat_JsonFormat{}}},
			Mediations: []*eventarcpb.Pipeline_Mediation{{MediationDescriptor: &eventarcpb.Pipeline_Mediation_Transformation_{
				Transformation: &eventarcpb.Pipeline_Mediation_Transformation{TransformationTemplate: `message.setField("data.seen", true)`}}}},
			RetryPolicy:   &eventarcpb.Pipeline_RetryPolicy{MaxAttempts: 3, MinRetryDelay: durationpb.New(2 * time.Second), MaxRetryDelay: durationpb.New(30 * time.Second)},
			LoggingConfig: &eventarcpb.LoggingConfig{LogSeverity: eventarcpb.LoggingConfig_INFO},
			Labels:        map[string]string{"team": "a"}},
		source: &eventarcpb.GoogleApiSource{Name: loc + "/googleApiSources/audit", Etag: "s1", Destination: loc + "/messageBuses/orders"},
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	eventarcpb.RegisterEventarcServer(srv, fake)
	publishingpb.RegisterPublisherServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	addr := lis.Addr().String()
	base := eaAdvBase{run: runProvider{runAddr: func() string { return addr }, defaultProject: "demo"}}
	ctx := context.Background()

	// Pipelines: Edit, prefilled; a form submitted unchanged sends back
	// what the pipeline holds.
	pp := eaPipelinesProvider{base}
	d, err := pp.Detail(ctx, "demo", []string{"to-http"})
	if err != nil || d.Edit == nil || d.Edit.Label != "Edit" || d.Edit.Submit != "Save" {
		t.Fatalf("the pipeline's page has no Edit: %+v, %v", d.Edit, err)
	}
	values := map[string]string{}
	for _, f := range d.Edit.Fields {
		values[f.Name] = f.Default
	}
	for name, want := range map[string]string{"destination": "HTTP endpoint", "uri": "https://hook.example/in", "inputFormat": "JSON",
		"serviceAccount": "sa@demo.iam.gserviceaccount.com", "transformation": `message.setField("data.seen", true)`,
		"maxAttempts": "3", "minRetryDelay": "2", "maxRetryDelay": "30", "logSeverity": "INFO", "displayName": "Orders"} {
		if values[name] != want {
			t.Errorf("Edit pipeline's %s = %q, want %q", name, values[name], want)
		}
	}
	values["uri"], values["maxAttempts"] = "https://hook.example/v2", "7"
	if err := pp.Edit(ctx, "demo", []string{"to-http"}, values); err != nil {
		t.Fatalf("Edit pipeline: %v", err)
	}
	got := fake.plUpdate.GetPipeline()
	if got.GetName() != fake.pipeline.GetName() || got.GetEtag() != "p1" || !slices.Equal(fake.plUpdate.GetUpdateMask().GetPaths(), eaPipelineMask) ||
		got.GetDestinations()[0].GetHttpEndpoint().GetUri() != "https://hook.example/v2" || got.GetRetryPolicy().GetMaxAttempts() != 7 ||
		got.GetRetryPolicy().GetMaxRetryDelay().AsDuration() != 30*time.Second || got.GetInputPayloadFormat().GetJson() == nil ||
		got.GetMediations()[0].GetTransformation().GetTransformationTemplate() != `message.setField("data.seen", true)` ||
		got.GetLoggingConfig().GetLogSeverity() != eventarcpb.LoggingConfig_INFO || got.GetLabels()["team"] != "a" ||
		got.GetDestinations()[0].GetAuthenticationConfig().GetGoogleOidc().GetServiceAccount() != "sa@demo.iam.gserviceaccount.com" {
		t.Errorf("UpdatePipeline = %v", fake.plUpdate)
	}

	// Google API sources: Edit, with the project's buses.
	sp := eaSourcesProvider{base}
	d, err = sp.Detail(ctx, "demo", []string{"audit"})
	if err != nil || d.Edit == nil {
		t.Fatalf("the Google API source's page has no Edit: %v", err)
	}
	values = map[string]string{}
	for _, f := range d.Edit.Fields {
		values[f.Name] = f.Default
		if f.Name == "bus" && (f.Type != "select" || !slices.Contains(f.Options, "orders")) {
			t.Errorf("Edit's Destination bus = %+v", f)
		}
	}
	values["logSeverity"] = "ERROR"
	if err := sp.Edit(ctx, "demo", []string{"audit"}, values); err != nil {
		t.Fatalf("Edit Google API source: %v", err)
	}
	if s := fake.srcUpdate.GetGoogleApiSource(); s.GetEtag() != "s1" || s.GetDestination() != loc+"/messageBuses/orders" ||
		s.GetLoggingConfig().GetLogSeverity() != eventarcpb.LoggingConfig_ERROR ||
		!slices.Equal(fake.srcUpdate.GetUpdateMask().GetPaths(), []string{"display_name", "destination", "logging_config", "labels"}) {
		t.Errorf("UpdateGoogleApiSource = %v", fake.srcUpdate)
	}

	// Buses: Publish message.
	bp := eaBusesProvider{base}
	acts := bp.DetailActions(ctx, "demo", []string{"orders"})
	if len(acts) != 1 || acts[0].ID != eaPublishAction || !strings.Contains(acts[0].Label, "CloudBurrow extension") || acts[0].Submit != "Publish" {
		t.Fatalf("the bus's actions = %+v", acts)
	}
	if err := bp.ActAt(ctx, "demo", []string{"orders"}, eaPublishAction, map[string]string{"eventId": "e-1", "eventSource": "//shop",
		"eventType": "order.created", "eventData": `{"total":42}`, "eventAttributes": `{"region":"eu"}`}); err != nil {
		t.Fatalf("Publish message: %v", err)
	}
	if fake.published.GetMessageBus() != fake.bus.GetName() {
		t.Errorf("published to %q", fake.published.GetMessageBus())
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(fake.published.GetJsonMessage()), &ev); err != nil || ev["id"] != "e-1" || ev["source"] != "//shop" ||
		ev["type"] != "order.created" || ev["specversion"] != "1.0" || ev["datacontenttype"] != "application/json" || ev["region"] != "eu" ||
		ev["data"].(map[string]any)["total"] != float64(42) {
		t.Errorf("the published message = %s, %v", fake.published.GetJsonMessage(), err)
	}
	for _, bad := range []map[string]string{
		{"eventSource": "//shop"},
		{"eventSource": "//shop", "eventType": "t", "eventData": "{not json"},
		{"eventSource": "//shop", "eventType": "t", "eventAttributes": `{"id":"x"}`},
	} {
		if _, err := eaPublishMessage(bad, time.Now()); err == nil {
			t.Errorf("the message %v was accepted", bad)
		}
	}
	if m, err := eaPublishMessage(map[string]string{"eventSource": "//s", "eventType": "t"}, time.Unix(1, 0)); err != nil ||
		!strings.Contains(m, `"id":"console-`) || !strings.Contains(m, `"data":{}`) {
		t.Errorf("a message with no ID or data = %s, %v", m, err)
	}
	if err := bp.ActAt(ctx, "demo", []string{"orders"}, "seek", nil); err == nil {
		t.Error("an unknown bus action was performed")
	}
}
