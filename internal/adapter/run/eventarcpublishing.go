package run

// The Eventarc Publishing API (#1947), google.cloud.eventarc.publishing.v1
// on eventarcpublishing.googleapis.com, as Google's "Publish events
// directly" and Publishing REST reference document it:
//
//   - Publish puts one CloudEvent on a message bus, given as the proto
//     CloudEvent (proto_message) or the CloudEvents JSON format
//     (json_message);
//   - PublishEvents puts CloudEvents (Any-wrapped io.cloudevents.v1
//     CloudEvents, or text_events in the JSON format) on a channel, and
//     PublishChannelConnectionEvents on the channel a provider's
//     connection names.
//
// avro_message, the CloudEvents Avro format, is UNIMPLEMENTED: its schema
// is the CloudEvents specification's, which Google's pages cite but do not
// reproduce. The JSON API is served on the Run port under
// /eventarcpublishing/ (its v1 bus paths are Eventarc's own).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	publishingpb "cloud.google.com/go/eventarc/publishing/apiv1/publishingpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// EventarcPublisher serves the Publishing API.
type EventarcPublisher struct {
	publishingpb.UnimplementedPublisherServer
	f *FunctionsServer
}

// EventarcPublisher returns the Publishing API, on the functions' event
// transport.
func (f *FunctionsServer) EventarcPublisher() *EventarcPublisher { return &EventarcPublisher{f: f} }

// Register adds the service to a gRPC server.
func (p *EventarcPublisher) Register(g grpc.ServiceRegistrar) {
	publishingpb.RegisterPublisherServer(g, p)
}

// eaFromProto is a proto CloudEvent as a bus event.
func eaFromProto(ce *publishingpb.CloudEvent) eaEvent {
	ev := eaEvent{attrs: map[string]any{"id": ce.GetId(), "source": ce.GetSource(), "specversion": ce.GetSpecVersion(), "type": ce.GetType()}}
	for k, v := range ce.GetAttributes() {
		switch x := v.GetAttr().(type) {
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeBoolean:
			ev.attrs[k] = x.CeBoolean
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeInteger:
			ev.attrs[k] = int64(x.CeInteger)
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeString:
			ev.attrs[k] = x.CeString
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeBytes:
			ev.attrs[k] = x.CeBytes
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeUri:
			ev.attrs[k] = x.CeUri
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeUriRef:
			ev.attrs[k] = x.CeUriRef
		case *publishingpb.CloudEvent_CloudEventAttributeValue_CeTimestamp:
			ev.attrs[k] = x.CeTimestamp.AsTime().UTC().Format(time.RFC3339Nano)
		}
	}
	switch d := ce.GetData().(type) {
	case *publishingpb.CloudEvent_BinaryData:
		ev.data, ev.set = d.BinaryData, true
	case *publishingpb.CloudEvent_TextData:
		ev.data, ev.set = []byte(d.TextData), true
	case *publishingpb.CloudEvent_ProtoData:
		ev.data, ev.set = d.ProtoData.GetValue(), true
		if _, ok := ev.attrs["datacontenttype"]; !ok {
			ev.attrs["datacontenttype"] = "application/protobuf"
		}
		if _, ok := ev.attrs["dataschema"]; !ok && d.ProtoData.GetTypeUrl() != "" {
			ev.attrs["dataschema"] = d.ProtoData.GetTypeUrl()
		}
	}
	return ev
}

// eaFromJSON is an event in the CloudEvents JSON format.
func eaFromJSON(s string) (eaEvent, error) {
	v, err := eaJSONValue([]byte(s))
	m, ok := v.(map[string]any)
	if err != nil || !ok {
		return eaEvent{}, apierror.InvalidArgument("the message is not a CloudEvent in the JSON format: %v", err)
	}
	ev := eaEvent{attrs: map[string]any{}}
	for k, x := range m {
		switch k {
		case "data":
			ev.set = true
			ct, _ := m["datacontenttype"].(string)
			if str, isStr := x.(string); isStr && ct != "" && !strings.Contains(ct, "json") {
				ev.data = []byte(str)
				continue
			}
			ev.data, _ = json.Marshal(x)
		case "data_base64":
			str, _ := x.(string)
			b, err := base64.StdEncoding.DecodeString(str)
			if err != nil {
				return eaEvent{}, apierror.InvalidArgument("data_base64 is not base64: %v", err)
			}
			ev.data, ev.set = b, true
		default:
			ev.attrs[k] = x
		}
	}
	return ev, nil
}

// eaFromAny is one Any-wrapped CloudEvent of PublishEvents.
func eaFromAny(a *anypb.Any) (eaEvent, error) {
	if !strings.HasSuffix(a.GetTypeUrl(), "/io.cloudevents.v1.CloudEvent") && !strings.HasSuffix(a.GetTypeUrl(), "/google.cloud.eventarc.publishing.v1.CloudEvent") {
		return eaEvent{}, apierror.InvalidArgument("event type %q is not a CloudEvents v1.0 event: only io.cloudevents.v1.CloudEvent is allowed", a.GetTypeUrl())
	}
	ce := &publishingpb.CloudEvent{}
	if err := proto.Unmarshal(a.GetValue(), ce); err != nil {
		return eaEvent{}, apierror.InvalidArgument("the event is not a CloudEvent: %v", err)
	}
	return eaFromProto(ce), nil
}

func eaEvents(anys []*anypb.Any, texts []string) ([]eaEvent, error) {
	if len(anys) > 0 && len(texts) > 0 {
		return nil, apierror.InvalidArgument("events and text_events cannot both be set")
	}
	var out []eaEvent
	for _, a := range anys {
		ev, err := eaFromAny(a)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	for _, s := range texts {
		ev, err := eaFromJSON(s)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if len(out) == 0 {
		return nil, apierror.InvalidArgument("events or text_events is required")
	}
	return out, nil
}

// Publish puts one event on a message bus.
func (p *EventarcPublisher) Publish(_ context.Context, req *publishingpb.PublishRequest) (*publishingpb.PublishResponse, error) {
	var ev eaEvent
	switch f := req.GetFormat().(type) {
	case *publishingpb.PublishRequest_ProtoMessage_:
		ev = eaFromProto(f.ProtoMessage_)
	case *publishingpb.PublishRequest_JsonMessage:
		var err error
		if ev, err = eaFromJSON(f.JsonMessage); err != nil {
			return nil, err
		}
	case *publishingpb.PublishRequest_AvroMessage:
		return nil, apierror.Unimplemented("avro_message is not supported: publish the event as json_message or proto_message")
	default:
		return nil, apierror.InvalidArgument("one of proto_message, json_message and avro_message is required")
	}
	if !strings.Contains(req.GetMessageBus(), "/messageBuses/") {
		return nil, apierror.InvalidArgument("message_bus %q must be projects/*/locations/*/messageBuses/*", req.GetMessageBus())
	}
	if err := p.f.eaPublishToBus(req.GetMessageBus(), ev, 0); err != nil {
		return nil, err
	}
	return &publishingpb.PublishResponse{}, nil
}

// PublishEvents puts events on a channel.
func (p *EventarcPublisher) PublishEvents(_ context.Context, req *publishingpb.PublishEventsRequest) (*publishingpb.PublishEventsResponse, error) {
	evs, err := eaEvents(req.GetEvents(), req.GetTextEvents())
	if err != nil {
		return nil, err
	}
	if err := p.f.eaPublishToChannel(req.GetChannel(), evs); err != nil {
		return nil, err
	}
	return &publishingpb.PublishEventsResponse{}, nil
}

// PublishChannelConnectionEvents puts a provider's events on its
// connection's channel.
func (p *EventarcPublisher) PublishChannelConnectionEvents(_ context.Context, req *publishingpb.PublishChannelConnectionEventsRequest) (*publishingpb.PublishChannelConnectionEventsResponse, error) {
	evs, err := eaEvents(req.GetEvents(), req.GetTextEvents())
	if err != nil {
		return nil, err
	}
	ch, err := eaChannelOfConnection(req.GetChannelConnection())
	if err != nil {
		return nil, err
	}
	if err := p.f.eaPublishToChannel(ch, evs); err != nil {
		return nil, err
	}
	return &publishingpb.PublishChannelConnectionEventsResponse{}, nil
}

// eaPubJSONPrefix is where the Publishing API's JSON is served.
const eaPubJSONPrefix = "/eventarcpublishing"

// EventarcPublishingHandlerOr serves the Publishing API's JSON and passes
// every other request to next. PublishEvents' events are read here, not
// by the transcoder: their @type, io.cloudevents.v1.CloudEvent, is the
// CloudEvents specification's message, which no Go package registers.
func EventarcPublishingHandlerOr(p *EventarcPublisher, next http.Handler) http.Handler {
	t := &rest.Transcoder{Packages: []string{"google.cloud.eventarc.publishing.v1"}}
	p.Register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, eaPubJSONPrefix+"/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, eaPubJSONPrefix+"/v1/")
		if r.Method == http.MethodPost && strings.HasSuffix(path, ":publishEvents") {
			eaServePublishEvents(w, r, p, strings.TrimSuffix(path, ":publishEvents"))
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, eaPubJSONPrefix)
		r2.URL.RawPath = ""
		t.ServeHTTP(w, r2)
	})
}

func eaServePublishEvents(w http.ResponseWriter, r *http.Request, p *EventarcPublisher, target string) {
	fail := func(err error) {
		rest.WriteError(w, err)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		fail(apierror.InvalidArgument("read the request: %v", err))
		return
	}
	var in struct {
		Events      []map[string]json.RawMessage `json:"events"`
		TextEvents  []string                     `json:"textEvents"`
		TextEvents2 []string                     `json:"text_events"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		fail(apierror.InvalidArgument("the request is not JSON: %v", err))
		return
	}
	var anys []*anypb.Any
	for _, e := range in.Events {
		var typ string
		_ = json.Unmarshal(e["@type"], &typ)
		delete(e, "@type")
		raw, _ := json.Marshal(e)
		ce := &publishingpb.CloudEvent{}
		if err := protojson.Unmarshal(raw, ce); err != nil {
			fail(apierror.InvalidArgument("an event is not a CloudEvent: %v", err))
			return
		}
		b, _ := proto.Marshal(ce)
		anys = append(anys, &anypb.Any{TypeUrl: typ, Value: b})
	}
	texts := append(in.TextEvents, in.TextEvents2...)
	ctx := r.Context()
	var resp proto.Message
	if strings.Contains(target, "/channelConnections/") {
		resp, err = p.PublishChannelConnectionEvents(ctx, &publishingpb.PublishChannelConnectionEventsRequest{ChannelConnection: target, Events: anys, TextEvents: texts})
	} else {
		resp, err = p.PublishEvents(ctx, &publishingpb.PublishEventsRequest{Channel: target, Events: anys, TextEvents: texts})
	}
	if err != nil {
		fail(err)
		return
	}
	out, _ := protojson.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, string(out))
}
