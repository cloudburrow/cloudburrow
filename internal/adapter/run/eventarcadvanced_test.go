package run

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

func eaPut[T eaRecord](t *testing.T, k *eaKind[T], x T) {
	t.Helper()
	eaAdv.mu.Lock()
	k.m[x.GetName()] = x
	eaAdv.mu.Unlock()
	t.Cleanup(func() { eaAdv.mu.Lock(); delete(k.m, x.GetName()); eaAdv.mu.Unlock() })
}

type eaGot struct {
	header http.Header
	body   []byte
}

// A JSON event published to a bus reaches the HTTP endpoint of the
// pipeline of the enrollment whose CEL match accepts it, transformed, in
// binary content mode; the other enrollment's pipeline gets nothing.
func TestBusEventFlowsThroughEnrollmentAndPipeline(t *testing.T) {
	got := make(chan eaGot, 4)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- eaGot{r.Header.Clone(), b}
	}))
	defer srv.Close()
	f := (&FunctionsServer{}).WithEvents(Events{Transport: &fakePublisher{}, Client: srv.Client()})
	loc := "projects/eaflow/locations/us-central1"
	bus := loc + "/messageBuses/b"
	eaPut(t, eaAdv.buses, &eventarcpb.MessageBus{Name: bus})
	p := &eventarcpb.Pipeline{Name: loc + "/pipelines/p",
		Destinations: []*eventarcpb.Pipeline_Destination{{DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{
			HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: srv.URL + "/route"}}}},
		InputPayloadFormat: &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Json{Json: &eventarcpb.Pipeline_MessagePayloadFormat_JsonFormat{}}},
		Mediations: []*eventarcpb.Pipeline_Mediation{{MediationDescriptor: &eventarcpb.Pipeline_Mediation_Transformation_{
			Transformation: &eventarcpb.Pipeline_Mediation_Transformation{TransformationTemplate: `message.removeFields(["data.card"]).setField("data.seen", true)`}}}},
		RetryPolicy: &eventarcpb.Pipeline_RetryPolicy{MaxAttempts: 1}}
	if err := eaCheckPipeline(f.Eventarc())(p); err != nil {
		t.Fatalf("check the pipeline: %v", err)
	}
	eaPut(t, eaAdv.pipelines, p)
	other := proto.Clone(p).(*eventarcpb.Pipeline)
	other.Name = loc + "/pipelines/other"
	eaPut(t, eaAdv.pipelines, other)
	en := &eventarcpb.Enrollment{Name: loc + "/enrollments/e", MessageBus: bus, Destination: p.Name,
		CelMatch: `message.type == "order.created" && message.data.total > 10`}
	if err := eaCheckEnrollment(en); err != nil {
		t.Fatalf("check the enrollment: %v", err)
	}
	eaPut(t, eaAdv.enrollments, en)
	eaPut(t, eaAdv.enrollments, &eventarcpb.Enrollment{Name: loc + "/enrollments/no", MessageBus: bus, Destination: other.Name,
		CelMatch: `message.source == "elsewhere"`})

	pub := f.EventarcPublisher()
	if _, err := pub.Publish(context.Background(), &publishingpb.PublishRequest{MessageBus: bus, Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","id":"e1","source":"//shop","type":"order.created","datacontenttype":"application/json","region":"eu","data":{"total":42,"card":"4111"}}`}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case g := <-got:
		if g.header.Get("ce-id") != "e1" || g.header.Get("ce-type") != "order.created" || g.header.Get("ce-source") != "//shop" ||
			g.header.Get("ce-region") != "eu" || g.header.Get("Content-Type") != "application/json" {
			t.Errorf("headers = %v", g.header)
		}
		var data map[string]any
		if err := json.Unmarshal(g.body, &data); err != nil || data["card"] != nil || data["seen"] != true || data["total"] != float64(42) {
			t.Errorf("body = %s (%v)", g.body, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no event reached the endpoint")
	}
	select {
	case g := <-got:
		t.Errorf("a second delivery: %v %s", g.header, g.body)
	case <-time.After(300 * time.Millisecond):
	}
	// A total of 5 does not match.
	_, _ = pub.Publish(context.Background(), &publishingpb.PublishRequest{MessageBus: bus, Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","id":"e2","source":"//shop","type":"order.created","data":{"total":5}}`}})
	select {
	case g := <-got:
		t.Errorf("an unmatched event was delivered: %v", g.header)
	case <-time.After(300 * time.Millisecond):
	}
	// An event without a type is refused.
	if _, err := pub.Publish(context.Background(), &publishingpb.PublishRequest{MessageBus: bus, Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","id":"e3","source":"//shop"}`}}); err == nil {
		t.Error("Publish without a type was accepted")
	}
}

// An Avro output format encodes the JSON input with the schema; a failing
// endpoint is retried as the retry policy says.
func TestPipelineAvroOutputAndRetries(t *testing.T) {
	eaHooks.mu.Lock()
	eaHooks.retryD = time.Millisecond
	eaHooks.mu.Unlock()
	t.Cleanup(func() { eaHooks.mu.Lock(); eaHooks.retryD = time.Second; eaHooks.mu.Unlock() })
	const schema = `{"type":"record","name":"Order","fields":[{"name":"total","type":"long"},{"name":"note","type":["null","string"]}]}`
	got := make(chan eaGot, 8)
	calls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		b, _ := io.ReadAll(r.Body)
		got <- eaGot{r.Header.Clone(), b}
	}))
	defer srv.Close()
	f := (&FunctionsServer{}).WithEvents(Events{Transport: &fakePublisher{}, Client: srv.Client()})
	loc := "projects/eaavro/locations/us-central1"
	bus := loc + "/messageBuses/b"
	eaPut(t, eaAdv.buses, &eventarcpb.MessageBus{Name: bus})
	jsonFmt := &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Json{Json: &eventarcpb.Pipeline_MessagePayloadFormat_JsonFormat{}}}
	avroFmt := &eventarcpb.Pipeline_MessagePayloadFormat{Kind: &eventarcpb.Pipeline_MessagePayloadFormat_Avro{Avro: &eventarcpb.Pipeline_MessagePayloadFormat_AvroFormat{SchemaDefinition: schema}}}
	p := &eventarcpb.Pipeline{Name: loc + "/pipelines/p", InputPayloadFormat: jsonFmt,
		Destinations: []*eventarcpb.Pipeline_Destination{{OutputPayloadFormat: avroFmt, DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{
			HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: srv.URL}}}},
		RetryPolicy: &eventarcpb.Pipeline_RetryPolicy{MaxAttempts: 3, MinRetryDelay: durationpb.New(time.Second), MaxRetryDelay: durationpb.New(2 * time.Second)}}
	if err := eaCheckPipeline(f.Eventarc())(p); err != nil {
		t.Fatal(err)
	}
	eaPut(t, eaAdv.pipelines, p)
	eaPut(t, eaAdv.enrollments, &eventarcpb.Enrollment{Name: loc + "/enrollments/e", MessageBus: bus, Destination: p.Name, CelMatch: "true"})
	ce := &publishingpb.CloudEvent{Id: "a1", Source: "//s", SpecVersion: "1.0", Type: "t", Data: &publishingpb.CloudEvent_TextData{TextData: `{"total":7,"note":"hi"}`}}
	if _, err := f.EventarcPublisher().Publish(context.Background(), &publishingpb.PublishRequest{MessageBus: bus,
		Format: &publishingpb.PublishRequest_ProtoMessage_{ProtoMessage_: ce}}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		codec, _ := goavro.NewCodecForStandardJSONFull(schema)
		native, _, err := codec.NativeFromBinary(g.body)
		text, _ := codec.TextualFromNative(nil, native)
		if err != nil || !strings.Contains(string(text), `"total":7`) || !strings.Contains(string(text), `"note":"hi"`) || g.header.Get("Content-Type") != "application/avro" {
			t.Errorf("avro body = %s, %v, %v", text, err, g.header)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no delivery after %d calls", calls)
	}
	// Checks Google documents.
	bad := proto.Clone(p).(*eventarcpb.Pipeline)
	bad.RetryPolicy.MaxAttempts = 101
	if err := eaCheckPipeline(f.Eventarc())(bad); err == nil {
		t.Error("max_attempts 101 was accepted")
	}
	bad = proto.Clone(p).(*eventarcpb.Pipeline)
	bad.InputPayloadFormat = nil
	if err := eaCheckPipeline(f.Eventarc())(bad); err == nil || !strings.Contains(err.Error(), "output_payload_format") {
		t.Errorf("an output format without an input format: %v", err)
	}
	bad = proto.Clone(p).(*eventarcpb.Pipeline)
	bad.Destinations = append(bad.Destinations, bad.Destinations[0])
	if err := eaCheckPipeline(f.Eventarc())(bad); err == nil {
		t.Error("two destinations were accepted")
	}
	bad = proto.Clone(p).(*eventarcpb.Pipeline)
	bad.Destinations[0].DestinationDescriptor = &eventarcpb.Pipeline_Destination_Workflow{Workflow: loc + "/workflows/w"}
	if err := eaCheckPipeline(f.Eventarc())(bad); err == nil || !strings.Contains(err.Error(), "Workflows") {
		t.Errorf("a workflow destination: %v", err)
	}
}

// A pipeline to a topic publishes in the CloudEvents Pub/Sub binding; one
// to a bus republishes there.
func TestPipelineToTopicAndBus(t *testing.T) {
	pub := &fakePublisher{}
	f := (&FunctionsServer{}).WithEvents(Events{Transport: pub})
	loc := "projects/eatopic/locations/us-central1"
	a, b := loc+"/messageBuses/a", loc+"/messageBuses/b"
	eaPut(t, eaAdv.buses, &eventarcpb.MessageBus{Name: a})
	eaPut(t, eaAdv.buses, &eventarcpb.MessageBus{Name: b})
	toBus := &eventarcpb.Pipeline{Name: loc + "/pipelines/tobus", Destinations: []*eventarcpb.Pipeline_Destination{{
		DestinationDescriptor: &eventarcpb.Pipeline_Destination_MessageBus{MessageBus: b}}}}
	toTopic := &eventarcpb.Pipeline{Name: loc + "/pipelines/totopic", Destinations: []*eventarcpb.Pipeline_Destination{{
		DestinationDescriptor: &eventarcpb.Pipeline_Destination_Topic{Topic: "projects/eatopic/topics/out"}}}}
	for _, p := range []*eventarcpb.Pipeline{toBus, toTopic} {
		if err := eaCheckPipeline(f.Eventarc())(p); err != nil {
			t.Fatal(err)
		}
		eaPut(t, eaAdv.pipelines, p)
	}
	eaPut(t, eaAdv.enrollments, &eventarcpb.Enrollment{Name: loc + "/enrollments/ab", MessageBus: a, Destination: toBus.Name, CelMatch: "true"})
	eaPut(t, eaAdv.enrollments, &eventarcpb.Enrollment{Name: loc + "/enrollments/bt", MessageBus: b, Destination: toTopic.Name, CelMatch: `message.type == "x"`})
	if err := f.eaPublishToBus(a, eaEvent{attrs: map[string]any{"id": "1", "source": "s", "specversion": "1.0", "type": "x",
		"datacontenttype": "text/plain"}, data: []byte("hello"), set: true}, 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		eaAdv.mu.Lock()
		n := len(pub.topics)
		eaAdv.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(pub.topics) != 1 || pub.topics[0] != "projects/eatopic/topics/out" || string(pub.data) != "hello" ||
		pub.attrs["ce-type"] != "x" || pub.attrs["content-type"] != "text/plain" {
		t.Errorf("published %v %q %v", pub.topics, pub.data, pub.attrs)
	}
}

// A channel's events reach the triggers on it whose type and filters
// they match.
func TestChannelEventsReachTriggers(t *testing.T) {
	got := make(chan eaGot, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- eaGot{r.Header.Clone(), b}
	}))
	defer srv.Close()
	f := (&FunctionsServer{}).WithEvents(Events{Transport: &fakePublisher{}})
	loc := "projects/eachan/locations/us-central1"
	ch := loc + "/channels/c"
	eaPut(t, eaAdv.channels, &eventarcpb.Channel{Name: ch, State: eventarcpb.Channel_ACTIVE})
	name := loc + "/triggers/t"
	tr := &eventarcpb.Trigger{Name: name, Channel: ch, Destination: &eventarcpb.Destination{Descriptor_: &eventarcpb.Destination_HttpEndpoint{
		HttpEndpoint: &eventarcpb.HttpEndpoint{Uri: srv.URL}}}}
	spec, err := eaSpec(&eventarcpb.Trigger{Channel: ch, EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: "custom.v1"},
		{Attribute: "color", Value: "red"}}, Destination: tr.Destination}, "us-central1")
	if err == nil {
		t.Fatal("an HTTP endpoint without a network attachment was accepted")
	}
	tr.Destination.NetworkConfig = &eventarcpb.NetworkConfig{NetworkAttachment: "projects/eachan/regions/us-central1/networkAttachments/n"}
	if spec, err = eaSpec(&eventarcpb.Trigger{Channel: ch, EventFilters: []*eventarcpb.EventFilter{{Attribute: "type", Value: "custom.v1"},
		{Attribute: "color", Value: "red"}}, Destination: tr.Destination}, "us-central1"); err != nil {
		t.Fatal(err)
	}
	eaState.mu.Lock()
	eaState.triggers[name] = &eaTrigger{t: tr, live: &liveTrigger{spec: spec}}
	eaState.mu.Unlock()
	t.Cleanup(func() { eaState.mu.Lock(); delete(eaState.triggers, name); eaState.mu.Unlock() })
	b, _ := proto.Marshal(&publishingpb.CloudEvent{Id: "1", Source: "//app", SpecVersion: "1.0", Type: "custom.v1",
		Attributes: map[string]*publishingpb.CloudEvent_CloudEventAttributeValue{"color": {Attr: &publishingpb.CloudEvent_CloudEventAttributeValue_CeString{CeString: "red"}}},
		Data:       &publishingpb.CloudEvent_TextData{TextData: "hi"}})
	any1 := &anypb.Any{TypeUrl: "type.googleapis.com/io.cloudevents.v1.CloudEvent", Value: b}
	if _, err := f.EventarcPublisher().PublishEvents(context.Background(), &publishingpb.PublishEventsRequest{Channel: ch, Events: []*anypb.Any{any1},
		TextEvents: nil}); err != nil {
		t.Fatal(err)
	}
	select {
	case g := <-got:
		if g.header.Get("ce-type") != "custom.v1" || g.header.Get("ce-color") != "red" || string(g.body) != "hi" {
			t.Errorf("delivered %v %q", g.header, g.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no channel event reached the trigger")
	}
	_, _ = f.EventarcPublisher().PublishEvents(context.Background(), &publishingpb.PublishEventsRequest{Channel: ch,
		TextEvents: []string{`{"specversion":"1.0","id":"2","source":"//app","type":"custom.v1","color":"blue","data":"x"}`}})
	select {
	case g := <-got:
		t.Errorf("a blue event was delivered: %v", g.header)
	case <-time.After(300 * time.Millisecond):
	}
	eaAdv.mu.Lock()
	eaAdv.channels.m[ch].State = eventarcpb.Channel_PENDING
	eaAdv.mu.Unlock()
	if _, err := f.EventarcPublisher().PublishEvents(context.Background(), &publishingpb.PublishEventsRequest{Channel: ch, Events: []*anypb.Any{any1}}); err == nil {
		t.Error("a PENDING channel accepted events")
	}
}

// The transformation functions Google's page documents.
func TestTransformationFunctions(t *testing.T) {
	msg := map[string]any{"id": "1", "data": map[string]any{"a": map[string]any{"b": int64(1)}, "list": []any{"x", "y"}, "s": "order-123"}}
	for expr, want := range map[string]string{
		`message.setField("data.a.c", 2)`:                   `{"data":{"a":{"b":1,"c":2},"list":["x","y"],"s":"order-123"},"id":"1"}`,
		`message.removeFields(["data.a", "data.list"])`:     `{"data":{"s":"order-123"},"id":"1"}`,
		`{"data": message.data.merge({"k": "v"})}`:          `{"data":{"a":{"b":1},"k":"v","list":["x","y"],"s":"order-123"}}`,
		`{"d": message.data.list.denormalize()}`:            `{"d":{"0":"x","1":"y"}}`,
		`{"d": message.data.denormalize()}`:                 `{"d":{"a.b":1,"list.0":"x","list.1":"y","s":"order-123"}}`,
		`{"n": re.capture(message.data.s, "order-(\\d+)")}`: `{"n":"123"}`,
		`{"m": [{"a": 1}, {"b": 2}].toMap()}`:               `{"m":{"a":1,"b":2}}`,
		`{"j": message.data.a.toJsonString()}`:              `{"j":"{\"b\":1}"}`,
	} {
		out, err := eaEvalTemplate(expr, false, nil, map[string]any{"message": msg})
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		b, _ := json.Marshal(out)
		if string(b) != want {
			t.Errorf("%s = %s; want %s", expr, b, want)
		}
	}
	if _, err := eaCompileMatch(`message.type ==`); err == nil {
		t.Error("a bad CEL match compiled")
	}
	if ok, err := eaEvalMatch(`message.data.a.b == 1 && message.id == "1"`, msg); !ok || err != nil {
		t.Errorf("match = %v, %v", ok, err)
	}
}
