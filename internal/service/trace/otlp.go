package trace

// OTLP trace ingestion, as the Telemetry API (telemetry.googleapis.com)
// serves it (read 2026-11-21: docs.cloud.google.com/stackdriver/docs/
// reference/telemetry/overview and docs.cloud.google.com/trace/docs/
// migrate-to-otlp-endpoints):
//
//   - OTLP/gRPC: opentelemetry.proto.collector.trace.v1.TraceService/Export
//     on the endpoint's root; OTLP/HTTP: POST /v1/traces ("OpenTelemetry
//     detects the data type and automatically appends /v1/traces"), with
//     http/protobuf, and JSON as the OTLP specification defines it (IDs in
//     hex);
//   - the project is the resource attribute gcp.project_id
//     (OTEL_RESOURCE_ATTRIBUTES="gcp.project_id=PROJECT_ID"), else the
//     quota project, x-goog-user-project;
//   - the data is Cloud Trace's: what OTLP writes, v1 and the Trace
//     Explorer read.
//
// Choices Google does not document (UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/trace/docs/migrate-to-otlp-endpoints)):
// with neither gcp.project_id nor x-goog-user-project the instance's
// default project is used (there are no credentials to take a quota
// project from); span and resource attributes become the span's
// attributes, keys unchanged (the span's win), a double, bytes, array or
// key-value list value its string form; an event is an annotation, a link
// a link, status ERROR is google.rpc.Code UNKNOWN with the message and OK
// is OK; v2's count and length limits are not applied ("the proprietary
// format might have lower limits"); and a span with a malformed trace or
// span ID is rejected and counted in partialSuccess, as OTLP specifies.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlptracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// OTLPPath is OTLP/HTTP's trace path.
const OTLPPath = "/v1/traces"

// ProjectAttribute is the resource attribute naming the project.
const ProjectAttribute = "gcp.project_id"

type otlpServer struct {
	coltracepb.UnimplementedTraceServiceServer
	s *Server
}

func (o *otlpServer) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	quota := ""
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("x-goog-user-project"); len(v) > 0 {
			quota = v[0]
		}
	}
	return o.s.ExportOTLP(req, quota)
}

// ExportOTLP writes an OTLP request's spans; quota is the request's
// x-goog-user-project.
func (s *Server) ExportOTLP(req *coltracepb.ExportTraceServiceRequest, quota string) (*coltracepb.ExportTraceServiceResponse, error) {
	var spans []*Span
	var rejected int64
	var reasons []string
	for _, rs := range req.GetResourceSpans() {
		resAttrs := rs.GetResource().GetAttributes()
		project := ""
		for _, kv := range resAttrs {
			if kv.GetKey() == ProjectAttribute {
				project = kv.GetValue().GetStringValue()
			}
		}
		named := project != ""
		if project == "" {
			project = quota
		}
		if project == "" {
			project = s.defaultProject
		}
		if project == "" {
			return nil, apierror.InvalidArgument("no project: set the resource attribute %s, or x-goog-user-project", ProjectAttribute)
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, in := range ss.GetSpans() {
				sp, err := s.fromOTLP(project, resAttrs, ss.GetScope(), in)
				if err != nil {
					rejected++
					reasons = append(reasons, apierror.From(err).Message)
					continue
				}
				if !named && s.router != nil {
					attrs := map[string]string{}
					for k, v := range sp.V2.GetAttributes().GetAttributeMap() {
						attrs[k] = AttrString(v)
					}
					p, keep := s.router(attrs)
					if !keep {
						continue
					}
					if p != "" && p != sp.Project {
						sp.Project = p
						sp.V2.Name = "projects/" + p + "/traces/" + sp.TraceID + "/spans/" + sp.SpanID
					}
				}
				spans = append(spans, sp)
			}
		}
	}
	s.mu.Lock()
	err := s.add(spans)
	s.mu.Unlock()
	if err != nil {
		return nil, apierror.Internal(err, "writing spans")
	}
	s.written(spans)
	resp := &coltracepb.ExportTraceServiceResponse{}
	if rejected > 0 {
		resp.PartialSuccess = &coltracepb.ExportTracePartialSuccess{RejectedSpans: rejected, ErrorMessage: strings.Join(reasons, "; ")}
	}
	return resp, nil
}

func anyString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			b, _ := json.Marshal(anyString(e))
			parts = append(parts, string(b))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *commonpb.AnyValue_KvlistValue:
		m := map[string]string{}
		for _, kv := range x.KvlistValue.GetValues() {
			m[kv.GetKey()] = anyString(kv.GetValue())
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	return ""
}

func attrValue(v *commonpb.AnyValue) *cloudtracev2pb.AttributeValue {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_BoolValue:
		return &cloudtracev2pb.AttributeValue{Value: &cloudtracev2pb.AttributeValue_BoolValue{BoolValue: x.BoolValue}}
	case *commonpb.AnyValue_IntValue:
		return &cloudtracev2pb.AttributeValue{Value: &cloudtracev2pb.AttributeValue_IntValue{IntValue: x.IntValue}}
	}
	return StringAttr(anyString(v))
}

func attributes(kvs ...[]*commonpb.KeyValue) *cloudtracev2pb.Span_Attributes {
	m := map[string]*cloudtracev2pb.AttributeValue{}
	for _, list := range kvs {
		for _, kv := range list {
			m[kv.GetKey()] = attrValue(kv.GetValue())
		}
	}
	if len(m) == 0 {
		return nil
	}
	return &cloudtracev2pb.Span_Attributes{AttributeMap: m}
}

func (s *Server) fromOTLP(project string, res []*commonpb.KeyValue, scope *commonpb.InstrumentationScope, in *otlptracepb.Span) (*Span, error) {
	if len(in.GetTraceId()) != 16 || bytes.Equal(in.GetTraceId(), make([]byte, 16)) {
		return nil, apierror.InvalidArgument("span %q: traceId must be 16 bytes, not all zero", in.GetName())
	}
	if len(in.GetSpanId()) != 8 || bytes.Equal(in.GetSpanId(), make([]byte, 8)) {
		return nil, apierror.InvalidArgument("span %q: spanId must be 8 bytes, not all zero", in.GetName())
	}
	if p := in.GetParentSpanId(); len(p) != 0 && len(p) != 8 {
		return nil, apierror.InvalidArgument("span %q: parentSpanId must be 8 bytes", in.GetName())
	}
	traceID, spanID := hex.EncodeToString(in.GetTraceId()), hex.EncodeToString(in.GetSpanId())
	sp := &cloudtracev2pb.Span{
		Name:         "projects/" + project + "/traces/" + traceID + "/spans/" + spanID,
		SpanId:       spanID,
		ParentSpanId: hex.EncodeToString(in.GetParentSpanId()),
		DisplayName:  &cloudtracev2pb.TruncatableString{Value: in.GetName()},
		StartTime:    timestamppb.New(unixNano(in.GetStartTimeUnixNano())),
		EndTime:      timestamppb.New(unixNano(in.GetEndTimeUnixNano())),
		SpanKind:     cloudtracev2pb.Span_SpanKind(in.GetKind()),
	}
	var scopeAttrs []*commonpb.KeyValue
	if scope.GetName() != "" {
		scopeAttrs = append(scopeAttrs, &commonpb.KeyValue{Key: "otel.scope.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: scope.GetName()}}})
	}
	if scope.GetVersion() != "" {
		scopeAttrs = append(scopeAttrs, &commonpb.KeyValue{Key: "otel.scope.version", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: scope.GetVersion()}}})
	}
	sp.Attributes = attributes(res, scopeAttrs, in.GetAttributes())
	if sp.Attributes != nil {
		sp.Attributes.DroppedAttributesCount = int32(in.GetDroppedAttributesCount())
	}
	if f := in.GetFlags(); sp.ParentSpanId != "" && f&uint32(otlptracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK) != 0 {
		// OTLP says whether the parent was remote only through these flags.
		sp.SameProcessAsParentSpan = wrapperspb.Bool(f&uint32(otlptracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK) == 0)
	}
	switch in.GetStatus().GetCode() {
	case otlptracepb.Status_STATUS_CODE_ERROR:
		sp.Status = &statuspb.Status{Code: int32(codes.Unknown), Message: in.GetStatus().GetMessage()}
	case otlptracepb.Status_STATUS_CODE_OK:
		sp.Status = &statuspb.Status{Code: int32(codes.OK)}
	}
	if evs := in.GetEvents(); len(evs) > 0 {
		te := &cloudtracev2pb.Span_TimeEvents{DroppedAnnotationsCount: int32(in.GetDroppedEventsCount())}
		for _, e := range evs {
			te.TimeEvent = append(te.TimeEvent, &cloudtracev2pb.Span_TimeEvent{
				Time: timestamppb.New(unixNano(e.GetTimeUnixNano())),
				Value: &cloudtracev2pb.Span_TimeEvent_Annotation_{Annotation: &cloudtracev2pb.Span_TimeEvent_Annotation{
					Description: &cloudtracev2pb.TruncatableString{Value: e.GetName()},
					Attributes:  attributes(e.GetAttributes()),
				}},
			})
		}
		sp.TimeEvents = te
	}
	if ls := in.GetLinks(); len(ls) > 0 {
		l := &cloudtracev2pb.Span_Links{DroppedLinksCount: int32(in.GetDroppedLinksCount())}
		for _, k := range ls {
			l.Link = append(l.Link, &cloudtracev2pb.Span_Link{TraceId: hex.EncodeToString(k.GetTraceId()), SpanId: hex.EncodeToString(k.GetSpanId()),
				Attributes: attributes(k.GetAttributes())})
		}
		sp.Links = l
	}
	return &Span{Project: project, TraceID: traceID, SpanID: spanID, V2: sp, Received: s.now()}, nil
}

// OTLPHandler serves POST /v1/traces, OTLP/HTTP, and reports false for
// any other request.
func (s *Server) OTLPHandler(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != OTLPPath {
		return false
	}
	ct := r.Header.Get("Content-Type")
	isJSON := strings.HasPrefix(ct, "application/json")
	write := func(code int, m proto.Message) {
		var b []byte
		if isJSON {
			b, _ = protojson.Marshal(m)
			w.Header().Set("Content-Type", "application/json")
		} else {
			b, _ = proto.Marshal(m)
			w.Header().Set("Content-Type", "application/x-protobuf")
		}
		w.WriteHeader(code)
		_, _ = w.Write(b)
	}
	fail := func(err error) {
		e := apierror.From(err)
		write(e.HTTPStatus(), &statuspb.Status{Code: int32(e.Code), Message: e.Message})
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(&apierror.Error{Code: codes.Unimplemented, Message: "OTLP/HTTP takes POST " + OTLPPath})
		return true
	}
	if !isJSON && !strings.HasPrefix(ct, "application/x-protobuf") && !strings.HasPrefix(ct, "application/protobuf") {
		fail(apierror.InvalidArgument("Content-Type %q: OTLP/HTTP takes application/x-protobuf or application/json", ct))
		return true
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, 64<<20)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(body)
		if err != nil {
			fail(apierror.InvalidArgument("gzip body: %v", err))
			return true
		}
		defer zr.Close()
		body = zr
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		fail(apierror.InvalidArgument("reading the body: %v", err))
		return true
	}
	req := &coltracepb.ExportTraceServiceRequest{}
	if isJSON {
		raw, err = otlpJSONIDs(raw)
		if err == nil {
			err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, req)
		}
	} else {
		err = proto.Unmarshal(raw, req)
	}
	if err != nil {
		fail(apierror.InvalidArgument("decoding the ExportTraceServiceRequest: %v", err))
		return true
	}
	resp, err := s.ExportOTLP(req, r.Header.Get("X-Goog-User-Project"))
	if err != nil {
		fail(err)
		return true
	}
	write(http.StatusOK, resp)
	return true
}

// otlpJSONIDs rewrites OTLP/JSON's hex traceId, spanId and parentSpanId
// as the base64 protojson reads bytes in.
func otlpJSONIDs(raw []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var walk func(any) error
	walk = func(x any) error {
		switch t := x.(type) {
		case map[string]any:
			for k, e := range t {
				if s, ok := e.(string); ok && (k == "traceId" || k == "spanId" || k == "parentSpanId" || k == "trace_id" || k == "span_id" || k == "parent_span_id") {
					if s == "" {
						continue
					}
					b, err := hex.DecodeString(s)
					if err != nil {
						return fmt.Errorf("%s %q is not hex", k, s)
					}
					t[k] = base64.StdEncoding.EncodeToString(b)
					continue
				}
				if err := walk(e); err != nil {
					return err
				}
			}
		case []any:
			for _, e := range t {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func unixNano(n uint64) time.Time { return time.Unix(0, int64(n)).UTC() }
