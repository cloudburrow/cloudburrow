package run

// Eventarc Advanced's event flow (#1947). A message on a bus is a
// CloudEvent; each enrollment on the bus whose cel_match accepts it sends
// it to its pipeline, which, in a goroutine of its own:
//
//  1. decodes the data in the pipeline's input_payload_format (JSON, Avro
//     with its schema, Protobuf with its schema); a message that does not
//     decode is a persistent error and is dropped, as Google documents;
//  2. runs its transformation, whose result is the new message (the
//     required id, source, specversion and type are kept when the result
//     leaves them out);
//  3. encodes the data in the destination's output_payload_format, else
//     the input format;
//  4. delivers it, retrying with the pipeline's retry_policy (5 attempts,
//     1 s to 60 s apart, by default): to an HTTP endpoint in CloudEvents
//     binary content mode, or as its message binding template builds the
//     request, with a Google OIDC token when the destination asks for one
//     (an HTTP endpoint that is a Cloud Run service of this instance is
//     reached through the cluster's ingress); to a Pub/Sub topic, in the
//     CloudEvents Pub/Sub binding (ce- attributes); or to another bus.
//
// A bus, pipeline or Google API source with a logging_config writes its
// platform logs at or above its log_severity to the instance's Cloud
// Logging (UNVERIFIED, re-read 2026-11-19: Google's "Observability in
// Eventarc" page names the platform logs of a bus and a pipeline but not
// their log names or payloads; these follow its resource names).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/linkedin/goavro/v2"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ProtoSchemaParser reads a Protobuf schema definition and returns its
// message; the CLI sets it to Pub/Sub's schema parser, which this package
// does not import.
type ProtoSchemaParser func(definition string) (protoreflect.MessageDescriptor, error)

var eaHooks = struct {
	mu     sync.Mutex
	proto  ProtoSchemaParser
	logs   LogWriter
	retryD time.Duration // the unit of a retry delay; tests shorten it
}{retryD: time.Second}

// UseEventarcProtoSchemas sets the parser of a pipeline's Protobuf format.
func UseEventarcProtoSchemas(p ProtoSchemaParser) {
	eaHooks.mu.Lock()
	eaHooks.proto = p
	eaHooks.mu.Unlock()
}

// UseEventarcLogging sets where buses, pipelines and Google API sources
// write their platform logs.
func UseEventarcLogging(w LogWriter) {
	eaHooks.mu.Lock()
	eaHooks.logs = w
	eaHooks.mu.Unlock()
}

// eaFormat is a payload format.
type eaFormat struct {
	contentType string
	avro        *goavro.Codec
	proto       protoreflect.MessageDescriptor
}

// eaFormatOf compiles a payload format; nil when none is set.
func eaFormatOf(f *eventarcpb.Pipeline_MessagePayloadFormat) (*eaFormat, error) {
	switch {
	case f == nil:
		return nil, nil
	case f.GetJson() != nil:
		return &eaFormat{contentType: "application/json"}, nil
	case f.GetAvro() != nil:
		def := f.GetAvro().GetSchemaDefinition()
		if def == "" {
			return nil, fmt.Errorf("avro.schema_definition is required")
		}
		c, err := goavro.NewCodecForStandardJSONFull(def)
		if err != nil {
			return nil, fmt.Errorf("avro.schema_definition is not an Avro schema: %v", err)
		}
		return &eaFormat{contentType: "application/avro", avro: c}, nil
	case f.GetProtobuf() != nil:
		def := f.GetProtobuf().GetSchemaDefinition()
		if def == "" {
			return nil, fmt.Errorf("protobuf.schema_definition is required")
		}
		eaHooks.mu.Lock()
		parse := eaHooks.proto
		eaHooks.mu.Unlock()
		if parse == nil {
			return nil, apierror.Unimplemented("a Protobuf payload format needs this instance's Protobuf schema parser, which is not set")
		}
		md, err := parse(def)
		if err != nil {
			return nil, fmt.Errorf("protobuf.schema_definition: %v", err)
		}
		return &eaFormat{contentType: "application/protobuf", proto: md}, nil
	}
	return nil, fmt.Errorf("a payload format must be json, avro or protobuf")
}

// decode reads data in the format, as JSON-like values.
func (f *eaFormat) decode(data []byte) (any, error) {
	var js []byte
	switch {
	case f.avro != nil:
		native, rest, err := f.avro.NativeFromBinary(data)
		if err != nil {
			return nil, fmt.Errorf("the data is not Avro of the pipeline's schema: %v", err)
		}
		if len(rest) > 0 {
			return nil, fmt.Errorf("the data has %d bytes after its Avro value", len(rest))
		}
		if js, err = f.avro.TextualFromNative(nil, native); err != nil {
			return nil, err
		}
	case f.proto != nil:
		m := dynamicpb.NewMessage(f.proto)
		if err := (proto.UnmarshalOptions{}).Unmarshal(data, m); err != nil {
			return nil, fmt.Errorf("the data is not a %s: %v", f.proto.FullName(), err)
		}
		var err error
		if js, err = (protojson.MarshalOptions{UseProtoNames: true}).Marshal(m); err != nil {
			return nil, err
		}
	default:
		js = data
	}
	v, err := eaJSONValue(js)
	if err != nil {
		return nil, fmt.Errorf("the data is not JSON: %v", err)
	}
	return v, nil
}

// encode writes data in the format.
func (f *eaFormat) encode(v any) ([]byte, error) {
	if s, ok := v.(string); ok && f.avro == nil && f.proto == nil {
		// A JSON payload given as a JSON string, as the transformation
		// page's scrubbed example writes it.
		if json.Valid([]byte(s)) {
			return []byte(s), nil
		}
	}
	js, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	switch {
	case f.avro != nil:
		native, _, err := f.avro.NativeFromTextual(js)
		if err != nil {
			return nil, fmt.Errorf("the data does not fit the output Avro schema: %v", err)
		}
		return f.avro.BinaryFromNative(nil, native)
	case f.proto != nil:
		m := dynamicpb.NewMessage(f.proto)
		if err := (protojson.UnmarshalOptions{}).Unmarshal(js, m); err != nil {
			return nil, fmt.Errorf("the data does not fit the output message %s: %v", f.proto.FullName(), err)
		}
		return proto.Marshal(m)
	}
	return js, nil
}

// eaJSONValue decodes JSON, with integers as int64.
func eaJSONValue(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	if d.More() {
		return nil, fmt.Errorf("trailing data after the JSON value")
	}
	return eaNumbers(v), nil
}

func eaNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = eaNumbers(e)
		}
	case []any:
		for i, e := range x {
			x[i] = eaNumbers(e)
		}
	}
	return v
}

// eaEvent is a CloudEvent on a bus: its context attributes and
// extensions, and its data.
type eaEvent struct {
	attrs map[string]any
	data  []byte
	// set reports the event has data (which may be empty).
	set bool
}

func (ev eaEvent) str(a string) string {
	if v, ok := ev.attrs[a]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

// eaCheckEvent refuses an event without CloudEvents' required attributes.
func eaCheckEvent(ev eaEvent) error {
	for _, a := range []string{"id", "source", "specversion", "type"} {
		if ev.str(a) == "" {
			return apierror.InvalidArgument("the CloudEvent has no %q attribute, which the CloudEvents specification requires", a)
		}
	}
	if v := ev.str("specversion"); v != "1.0" {
		return apierror.InvalidArgument("the CloudEvent's specversion %q is not 1.0", v)
	}
	return nil
}

// message is the event as CEL reads it: with no input format, data whose
// content type is JSON (or unset) is read as JSON, other data as a string.
func (ev eaEvent) message(in *eaFormat) (map[string]any, error) {
	m := map[string]any{}
	for k, v := range ev.attrs {
		m[k] = v
	}
	if !ev.set {
		return m, nil
	}
	if in != nil {
		v, err := in.decode(ev.data)
		if err != nil {
			return nil, err
		}
		m["data"] = v
		return m, nil
	}
	ct := ev.str("datacontenttype")
	if ct == "" || strings.Contains(ct, "json") {
		if v, err := eaJSONValue(ev.data); err == nil {
			m["data"] = v
			return m, nil
		}
	}
	m["data"] = string(ev.data)
	return m, nil
}

// eaRequired are the attributes a transformation keeps when its result
// leaves them out.
var eaRequired = []string{"id", "source", "specversion", "type"}

// PublishToBus publishes one event to a bus: each enrollment that matches
// it sends it to its pipeline, in the background. hops counts the buses
// it has passed through, so a loop of buses ends.
func (f *FunctionsServer) eaPublishToBus(bus string, ev eaEvent, hops int) error {
	b, err := eaGet(eaAdv.buses, bus)
	if err != nil {
		return err
	}
	if err := eaCheckEvent(ev); err != nil {
		return err
	}
	eaLog(b.GetLoggingConfig(), bus, "eventarc.googleapis.com/MessageBus", ltype.LogSeverity_DEBUG, "message received", ev, "")
	if hops > 8 {
		eaLog(b.GetLoggingConfig(), bus, "eventarc.googleapis.com/MessageBus", ltype.LogSeverity_ERROR, "message dropped", ev,
			"the message has passed through more than 8 buses")
		return nil
	}
	msg, _ := ev.message(nil)
	for _, en := range eaEnrollmentsOn(bus) {
		ok, err := eaEvalMatch(en.GetCelMatch(), msg)
		if err != nil {
			slog.Debug("eventarc: an enrollment's match failed", "enrollment", en.GetName(), "err", err)
		}
		if !ok {
			continue
		}
		eaLog(b.GetLoggingConfig(), bus, "eventarc.googleapis.com/MessageBus", ltype.LogSeverity_INFO, "message matched enrollment "+en.GetName(), ev, "")
		go f.eaRunPipeline(en.GetDestination(), ev, hops)
	}
	return nil
}

// eaRunPipeline processes one event through a pipeline and delivers it.
func (f *FunctionsServer) eaRunPipeline(name string, ev eaEvent, hops int) {
	p, err := eaGet(eaAdv.pipelines, name)
	if err != nil {
		slog.Warn("eventarc: an enrollment's pipeline is gone", "pipeline", name)
		return
	}
	lc := p.GetLoggingConfig()
	const res = "eventarc.googleapis.com/Pipeline"
	fail := func(stage string, err error) {
		eaLog(lc, name, res, ltype.LogSeverity_ERROR, stage, ev, err.Error())
		slog.Warn("eventarc: pipeline delivery failed", "pipeline", name, "stage", stage, "err", err)
	}
	in, err := eaFormatOf(p.GetInputPayloadFormat())
	if err != nil {
		fail("input format", err)
		return
	}
	msg, err := ev.message(in)
	if err != nil {
		fail("the message does not match the input payload format (a persistent error)", err)
		return
	}
	if ms := p.GetMediations(); len(ms) > 0 {
		out, err := eaEvalTemplate(ms[0].GetTransformation().GetTransformationTemplate(), false, nil, map[string]any{"message": msg})
		if err != nil {
			fail("transformation", err)
			return
		}
		for _, a := range eaRequired {
			if _, ok := out[a]; !ok {
				out[a] = msg[a]
			}
		}
		msg = out
	}
	d := p.GetDestinations()[0]
	outFmt := in
	if d.GetOutputPayloadFormat() != nil {
		if outFmt, err = eaFormatOf(d.GetOutputPayloadFormat()); err != nil {
			fail("output format", err)
			return
		}
	}
	rp := p.GetRetryPolicy()
	attempts := int(rp.GetMaxAttempts())
	if attempts == 0 {
		attempts = 5
	}
	minD, maxD := rp.GetMinRetryDelay().AsDuration(), rp.GetMaxRetryDelay().AsDuration()
	if minD == 0 {
		minD = time.Second
	}
	if maxD == 0 {
		maxD = 60 * time.Second
	}
	eaHooks.mu.Lock()
	unit := eaHooks.retryD
	eaHooks.mu.Unlock()
	delay := minD
	for attempt := 1; ; attempt++ {
		err := f.eaDeliver(name, d, msg, outFmt, hops)
		if err == nil {
			eaLog(lc, name, res, ltype.LogSeverity_INFO, "message delivered", ev, "")
			return
		}
		if attempt >= attempts {
			fail(fmt.Sprintf("delivery, attempt %d of %d, the last", attempt, attempts), err)
			return
		}
		eaLog(lc, name, res, ltype.LogSeverity_WARNING, fmt.Sprintf("delivery, attempt %d of %d", attempt, attempts), ev, err.Error())
		time.Sleep(time.Duration(float64(delay) * float64(unit) / float64(time.Second)))
		if delay *= 2; delay > maxD {
			delay = maxD
		}
	}
}

// eaPayload is the message's data in the format: its bytes and content
// type.
func eaPayload(msg map[string]any, format *eaFormat) ([]byte, string, error) {
	ct, _ := msg["datacontenttype"].(string)
	data, has := msg["data"]
	if !has {
		return nil, ct, nil
	}
	if format != nil {
		b, err := format.encode(data)
		if err != nil {
			return nil, "", err
		}
		// A datacontenttype that is a prefix of the format's type wins.
		if ct == "" || !strings.HasPrefix(format.contentType, ct) {
			ct = format.contentType
		}
		return b, ct, nil
	}
	switch x := data.(type) {
	case []byte:
		return x, ct, nil
	case string:
		return []byte(x), ct, nil
	}
	b, err := json.Marshal(data)
	if ct == "" {
		ct = "application/json"
	}
	return b, ct, err
}

// eaHeaderValue is an attribute's canonical string.
func eaHeaderValue(v any) string {
	switch x := v.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// eaDeliver sends the message to the destination once.
func (f *FunctionsServer) eaDeliver(pipeline string, d *eventarcpb.Pipeline_Destination, msg map[string]any, format *eaFormat, hops int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	switch {
	case d.GetMessageBus() != "":
		data, ct, err := eaPayload(msg, format)
		if err != nil {
			return err
		}
		ev := eaEvent{attrs: map[string]any{}, data: data, set: data != nil}
		for k, v := range msg {
			if k != "data" {
				ev.attrs[k] = v
			}
		}
		if ct != "" {
			ev.attrs["datacontenttype"] = ct
		}
		return f.eaPublishToBus(d.GetMessageBus(), ev, hops+1)
	case d.GetTopic() != "":
		pub, ok := f.publisher()
		if !ok {
			return fmt.Errorf("this instance cannot publish to Pub/Sub: start it with --services run,pubsub")
		}
		data, ct, err := eaPayload(msg, format)
		if err != nil {
			return err
		}
		attrs := map[string]string{}
		for k, v := range msg {
			if k != "data" && k != "datacontenttype" {
				attrs["ce-"+k] = eaHeaderValue(v)
			}
		}
		if ct != "" {
			attrs["content-type"] = ct
		}
		return pub.Publish(ctx, eaTopicName(d.GetTopic()), data, attrs)
	case d.GetWorkflow() != "":
		if err := f.checkWorkflow(d.GetWorkflow()); err != nil {
			return err
		}
		arg, err := eaPipelineWorkflowArgument(msg, format)
		if err != nil {
			return err
		}
		return f.events.Workflows.StartWorkflow(ctx, d.GetWorkflow(), arg)
	case d.GetHttpEndpoint() != nil:
		return f.eaDeliverHTTP(ctx, pipeline, d, msg, format)
	}
	return fmt.Errorf("the destination is not one this instance delivers to")
}

// eaPipelineWorkflowArgument is a pipeline's message as a workflow's
// runtime argument (#1996): the CloudEvent in its JSON format, its
// attributes beside data (the JSON value when the payload is JSON, else
// the text, or data_base64 when it is not UTF-8), as Eventarc passes it.
func eaPipelineWorkflowArgument(msg map[string]any, format *eaFormat) (string, error) {
	body, ct, err := eaPayload(msg, format)
	if err != nil {
		return "", err
	}
	out := map[string]any{}
	for k, v := range msg {
		if k != "data" {
			out[k] = eaHeaderValue(v)
		}
	}
	if ct != "" {
		out["datacontenttype"] = ct
	}
	if _, has := msg["data"]; has {
		var data any
		switch {
		case json.Unmarshal(body, &data) == nil:
			out["data"] = data
		case utf8.Valid(body):
			out["data"] = string(body)
		default:
			out["data_base64"] = base64.StdEncoding.EncodeToString(body)
		}
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// eaDeliverHTTP posts the message to an HTTP endpoint.
func (f *FunctionsServer) eaDeliverHTTP(ctx context.Context, pipeline string, d *eventarcpb.Pipeline_Destination, msg map[string]any, format *eaFormat) error {
	he := d.GetHttpEndpoint()
	body, ct, err := eaPayload(msg, format)
	if err != nil {
		return err
	}
	h := http.Header{}
	for k, v := range msg {
		if k != "data" && k != "datacontenttype" {
			h.Set("ce-"+k, eaHeaderValue(v))
		}
	}
	if ct != "" {
		h.Set("Content-Type", ct)
	}
	if tmpl := he.GetMessageBindingTemplate(); tmpl != "" {
		var fm eaFormatter
		if format != nil {
			fm = func(v any) (any, error) {
				b, err := format.encode(v)
				if err != nil || format.avro != nil || format.proto != nil {
					return b, err
				}
				return string(b), nil
			}
		}
		out, err := eaEvalTemplate(tmpl, true, fm, map[string]any{"message": msg, "headers": map[string]any{}})
		if err != nil {
			return fmt.Errorf("the message binding: %v", err)
		}
		if hs, ok := out["headers"].(map[string]any); ok {
			h = http.Header{}
			for k, v := range hs {
				h.Set(k, eaHeaderValue(v))
			}
		}
		if b, ok := out["body"]; ok {
			switch x := b.(type) {
			case string:
				body = []byte(x)
			case []byte:
				body = x
			default:
				if body, err = json.Marshal(x); err != nil {
					return err
				}
			}
		}
	}
	loc := eaLocOf(pipeline)
	target, local, err := (&EventarcServer{f: f}).eaEndpoint(loc, he.GetUri())
	if err != nil {
		return err
	}
	dest := he.GetUri()
	if local {
		if f.events == nil || f.events.Ingress == "" {
			return fmt.Errorf("no ingress port is published on this machine, so the Cloud Run service cannot be reached")
		}
		u, _ := url.Parse(he.GetUri())
		dest = "http://" + f.events.Ingress + u.RequestURI()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dest, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = h
	if local {
		req.Host = target
	}
	if oidc := d.GetAuthenticationConfig().GetGoogleOidc(); oidc != nil {
		if f.events == nil || f.events.Tokens == nil {
			return fmt.Errorf("this instance cannot mint the destination's OIDC token")
		}
		aud := oidc.GetAudience()
		if aud == "" {
			aud = he.GetUri()
		}
		tok, err := f.events.Tokens.ServiceAccountIDToken(oidc.GetServiceAccount(), aud)
		if err != nil {
			return fmt.Errorf("mint the destination's OIDC token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := guardedDefault
	if f.events != nil && f.events.Client != nil {
		client = f.events.Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the endpoint answered %s", resp.Status)
	}
	return nil
}

// eaEndpoint reports whether an endpoint URI is a Cloud Run service of
// this instance in the location, and its host.
func (e *EventarcServer) eaEndpoint(loc, uri string) (string, bool, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false, apierror.InvalidArgument("destinations[0].http_endpoint.uri %q is not an http(s) URI", uri)
	}
	if e.f == nil || e.f.s == nil {
		return u.Host, false, nil
	}
	resp, err := e.f.s.ListServices(context.Background(), &runpb.ListServicesRequest{Parent: loc})
	if err != nil {
		return u.Host, false, nil
	}
	for _, s := range resp.GetServices() {
		if su, err := url.Parse(s.GetUri()); err == nil && su.Host != "" && strings.EqualFold(su.Host, u.Host) {
			return su.Host, true, nil
		}
	}
	return u.Host, false, nil
}

// eaLog writes one platform log entry when the config's severity allows.
func eaLog(c *eventarcpb.LoggingConfig, name, resType string, sev ltype.LogSeverity, what string, ev eaEvent, detail string) {
	min := c.GetLogSeverity()
	if min == eventarcpb.LoggingConfig_LOG_SEVERITY_UNSPECIFIED || min == eventarcpb.LoggingConfig_NONE {
		return
	}
	// LoggingConfig's severities are Cloud Logging's, one step up: DEBUG
	// is 2 there and 100 here.
	if int32(sev) < int32(min-1)*100 {
		return
	}
	eaHooks.mu.Lock()
	w := eaHooks.logs
	eaHooks.mu.Unlock()
	if w == nil {
		return
	}
	parts := strings.Split(name, "/")
	if len(parts) < 6 {
		return
	}
	project, location, id := parts[1], parts[3], parts[5]
	labelKey := "message_bus_id"
	logID := "eventarc.googleapis.com%2Fmessage_bus"
	if strings.HasSuffix(resType, "Pipeline") {
		labelKey, logID = "pipeline_id", "eventarc.googleapis.com%2Fpipeline"
	} else if strings.HasSuffix(resType, "GoogleApiSource") {
		labelKey, logID = "google_api_source_id", "eventarc.googleapis.com%2Fgoogle_api_source"
	}
	fields := map[string]any{"message": what, "messageUid": ev.str("id"), "source": ev.str("source"), "type": ev.str("type")}
	if detail != "" {
		fields["details"] = detail
	}
	payload, _ := structpb.NewStruct(fields)
	entry := &loggingpb.LogEntry{
		LogName:   "projects/" + project + "/logs/" + logID,
		Resource:  &monitoredres.MonitoredResource{Type: resType, Labels: map[string]string{"resource_container": project, "location": location, labelKey: id}},
		Timestamp: timestamppb.Now(),
		Severity:  sev,
		Payload:   &loggingpb.LogEntry_JsonPayload{JsonPayload: payload},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w(ctx, entry); err != nil {
		slog.Debug("eventarc: write a platform log entry", "err", err)
	}
}

// emitToSources publishes a Google event of a project to the bus of each
// Google API source that receives the project's events.
func (f *FunctionsServer) emitToSources(project string, ev eaEvent) int {
	n := 0
	for _, s := range eaSourcesFor(project) {
		eaLog(s.GetLoggingConfig(), s.GetName(), "eventarc.googleapis.com/GoogleApiSource", ltype.LogSeverity_DEBUG, "event received", ev, "")
		if err := f.eaPublishToBus(s.GetDestination(), ev, 0); err != nil {
			eaLog(s.GetLoggingConfig(), s.GetName(), "eventarc.googleapis.com/GoogleApiSource", ltype.LogSeverity_ERROR, "event not published", ev, err.Error())
			continue
		}
		n++
	}
	return n
}
