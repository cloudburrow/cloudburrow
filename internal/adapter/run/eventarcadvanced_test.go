package run

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// An enrollment's cel_match over the event's attributes, as
// https://docs.cloud.google.com/eventarc/advanced/docs/receive-events/use-cel
// documents it: message.type and the other attributes, extension
// attributes as strings, has(), time as a timestamp, the strings
// extension; a missing key does not match; message.data is reserved.
func TestEnrollmentCELMatchesAttributes(t *testing.T) {
	ev, err := parseJSONEvent(`{"specversion":"1.0","type":"com.example.someevent","source":"//my/source","id":"A234",
		"time":"2024-04-05T17:31:00Z","bucket":"bucketName","count":3,"datacontenttype":"application/json","data":{"key":"value"}}`)
	if err != nil {
		t.Fatal(err)
	}
	for expr, want := range map[string]bool{
		`message.type == "com.example.someevent"`: true,
		`message.type == 'hello-world-type'`:      false,
		`true`:                                    true,
		`has(message.bucket)`:                     true,
		`has(message.nothere)`:                    false,
		`message.nothere == "x"`:                  false,
		`message['bucket'] == "bucketName" && message.id == "A234"`: true,
		`message.count == "3"`: true,
		`message.source.startsWith("//my") && message.type.contains("some")`: true,
		`message.time.getFullYear() < 2020`:                                  false,
		`message.time.getFullYear() == 2024`:                                 true,
		`message.type.upperAscii() == "COM.EXAMPLE.SOMEEVENT"`:               true,
	} {
		prg, err := compileMatch(expr)
		if err != nil {
			t.Errorf("compile %s: %v", expr, err)
			continue
		}
		if got := enrollmentMatches(prg, ev.attrs); got != want {
			t.Errorf("%s = %v; want %v", expr, got, want)
		}
	}
	for _, bad := range []string{``, `message.data.key == "value"`, `has(message.data_base64)`, `message.type ==`, `"a string"`} {
		if _, err := compileMatch(bad); apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("compile %q = %v; want INVALID_ARGUMENT", bad, err)
		}
	}
	if _, err := parseJSONEvent(`{"specversion":"1.0","type":"t","source":"s"}`); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("an event without id = %v", err)
	}
}

// A pipeline has one destination: a workflow in its project or an HTTP
// endpoint on this machine; the rest are refused naming the field.
func TestPipelineDestinationsAreChecked(t *testing.T) {
	f := (&FunctionsServer{}).WithEvents(Events{Workflows: &fakeWorkflows{}})
	name := "projects/p/locations/us-central1/pipelines/pl"
	dest := func(d *eventarcpb.Pipeline_Destination) *eventarcpb.Pipeline {
		return &eventarcpb.Pipeline{Destinations: []*eventarcpb.Pipeline_Destination{d}}
	}
	wf := func(w string) *eventarcpb.Pipeline_Destination {
		return &eventarcpb.Pipeline_Destination{DestinationDescriptor: &eventarcpb.Pipeline_Destination_Workflow{Workflow: w}}
	}
	he := func(u string) *eventarcpb.Pipeline_Destination {
		return &eventarcpb.Pipeline_Destination{DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{
			HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: u}}}
	}
	for what, c := range map[string]struct {
		p    *eventarcpb.Pipeline
		code codes.Code
	}{
		"a deployed workflow":       {dest(wf("projects/p/locations/us-central1/workflows/w")), codes.OK},
		"a missing workflow":        {dest(wf("projects/p/locations/us-central1/workflows/x")), codes.InvalidArgument},
		"another project's":         {dest(wf("projects/q/locations/us-central1/workflows/w")), codes.InvalidArgument},
		"a local endpoint":          {dest(he("http://127.0.0.1:9/x")), codes.OK},
		"an endpoint off this host": {dest(he("https://example.com/x")), codes.InvalidArgument},
		"no destination":            {&eventarcpb.Pipeline{}, codes.InvalidArgument},
		"two destinations":          {&eventarcpb.Pipeline{Destinations: []*eventarcpb.Pipeline_Destination{he("http://localhost/"), he("http://localhost/")}}, codes.InvalidArgument},
		"a topic": {dest(&eventarcpb.Pipeline_Destination{DestinationDescriptor: &eventarcpb.Pipeline_Destination_Topic{Topic: "projects/p/topics/t"}}),
			codes.Unimplemented},
		"a bus": {dest(&eventarcpb.Pipeline_Destination{DestinationDescriptor: &eventarcpb.Pipeline_Destination_MessageBus{MessageBus: "projects/p/locations/us-central1/messageBuses/b"}}),
			codes.Unimplemented},
		"a mediation": {&eventarcpb.Pipeline{Destinations: []*eventarcpb.Pipeline_Destination{he("http://localhost/")},
			Mediations: []*eventarcpb.Pipeline_Mediation{{}}}, codes.Unimplemented},
		"a key": {&eventarcpb.Pipeline{Destinations: []*eventarcpb.Pipeline_Destination{he("http://localhost/")}, CryptoKeyName: "k"}, codes.Unimplemented},
	} {
		err := f.checkPipeline(name, c.p)
		got := codes.OK
		if err != nil {
			got = apierror.From(err).Code
		}
		if got != c.code {
			t.Errorf("%s = %v; want %v", what, err, c.code)
		}
	}
}

type chanWorkflows struct{ got chan [2]string }

func (w *chanWorkflows) WorkflowExists(name string) bool {
	return name == "projects/p/locations/us-central1/workflows/w"
}
func (w *chanWorkflows) StartWorkflow(_ context.Context, name, arg string) error {
	w.got <- [2]string{name, arg}
	return nil
}

// Publish routes an event to each matching enrollment's pipeline: a
// workflow receives the CloudEvent in JSON as its argument, a local HTTP
// endpoint a binary-mode CloudEvent; a non-matching event goes nowhere.
func TestPublishRoutesToPipelines(t *testing.T) {
	w := &chanWorkflows{got: make(chan [2]string, 4)}
	f := (&FunctionsServer{}).WithEvents(Events{Workflows: w})
	var mu sync.Mutex
	var hits []*http.Request
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits, bodies = append(hits, r), append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	bus := "projects/p/locations/us-central1/messageBuses/b"
	wfPipe := "projects/p/locations/us-central1/pipelines/to-wf"
	httpPipe := "projects/p/locations/us-central1/pipelines/to-http"
	eaAdv.mu.Lock()
	eaAdv.buses[bus] = &eventarcpb.MessageBus{Name: bus}
	eaAdv.pipelines[wfPipe] = &eventarcpb.Pipeline{Name: wfPipe, Destinations: []*eventarcpb.Pipeline_Destination{{
		DestinationDescriptor: &eventarcpb.Pipeline_Destination_Workflow{Workflow: "projects/p/locations/us-central1/workflows/w"}}}}
	eaAdv.pipelines[httpPipe] = &eventarcpb.Pipeline{Name: httpPipe, Destinations: []*eventarcpb.Pipeline_Destination{{
		DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: srv.URL + "/in"}}}}}
	eaAdv.mu.Unlock()
	t.Cleanup(func() {
		eaAdv.mu.Lock()
		delete(eaAdv.buses, bus)
		delete(eaAdv.pipelines, wfPipe)
		delete(eaAdv.pipelines, httpPipe)
		delete(eaAdv.enrollments, "projects/p/locations/us-central1/enrollments/e1")
		delete(eaAdv.enrollments, "projects/p/locations/us-central1/enrollments/e2")
		eaAdv.mu.Unlock()
	})
	for id, c := range map[string][2]string{"e1": {`message.type == "order.created"`, wfPipe}, "e2": {`message.source == "//shop"`, httpPipe}} {
		en := &eventarcpb.Enrollment{CelMatch: c[0], MessageBus: bus, Destination: c[1]}
		prg, err := checkEnrollment("projects/p/locations/us-central1/enrollments/"+id, en)
		if err != nil {
			t.Fatalf("enrollment %s: %v", id, err)
		}
		eaAdv.mu.Lock()
		eaAdv.enrollments["projects/p/locations/us-central1/enrollments/"+id] = &eaEnrollment{e: en, prg: prg}
		eaAdv.mu.Unlock()
	}
	p := f.EventarcPublisher()
	ctx := context.Background()
	if _, err := p.Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus, Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","type":"order.created","source":"//shop","id":"1","data":{"order":42}}`}}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-w.got:
		var arg map[string]any
		if err := json.Unmarshal([]byte(got[1]), &arg); err != nil || arg["type"] != "order.created" || arg["id"] != "1" {
			t.Errorf("the workflow's argument %s (%v)", got[1], err)
		}
		if d, _ := arg["data"].(map[string]any); d["order"] != float64(42) {
			t.Errorf("the argument's data %v", arg["data"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the workflow was not started")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(hits)
		mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	if len(hits) != 1 || hits[0].Header.Get("ce-type") != "order.created" || hits[0].Header.Get("ce-source") != "//shop" ||
		hits[0].URL.Path != "/in" || bodies[0] != `{"order":42}` {
		t.Errorf("the HTTP endpoint got %d requests: %v %v", len(hits), hits, bodies)
	}
	mu.Unlock()
	// Neither enrollment matches.
	if _, err := p.Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus, Format: &publishingpb.PublishRequest_ProtoMessage_{
		ProtoMessage_: &publishingpb.CloudEvent{Id: "2", Source: "//other", SpecVersion: "1.0", Type: "order.deleted"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-w.got:
		t.Errorf("a non-matching event started %v", got)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := p.Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus + "x", Format: &publishingpb.PublishRequest_JsonMessage{
		JsonMessage: `{"specversion":"1.0","type":"t","source":"s","id":"3"}`}}); apierror.From(err).Code != codes.NotFound {
		t.Errorf("a missing bus = %v", err)
	}
	if _, err := p.Publish(ctx, &publishingpb.PublishRequest{MessageBus: bus, Format: &publishingpb.PublishRequest_AvroMessage{AvroMessage: []byte{1}}}); apierror.From(err).Code != codes.Unimplemented {
		t.Errorf("avro = %v", err)
	}
	// An enrollment's pipeline must be in the bus's region.
	if _, err := checkEnrollment("projects/p/locations/us-central1/enrollments/e3", &eventarcpb.Enrollment{CelMatch: "true", MessageBus: bus,
		Destination: "projects/p/locations/europe-west1/pipelines/to-wf"}); apierror.From(err).Code != codes.InvalidArgument {
		t.Errorf("a pipeline in another region = %v", err)
	}
}
