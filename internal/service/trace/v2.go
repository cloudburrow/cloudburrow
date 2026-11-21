package trace

// The Cloud Trace API v2 (google.devtools.cloudtrace.v2.TraceService), as
// its reference and protos document it (read 2026-11-21:
// docs.cloud.google.com/trace/docs/reference/v2/rest/v2/projects.traces/batchWrite
// and .../projects.traces.spans/createSpan):
//
//   - a span's name is projects/[PROJECT_ID]/traces/[TRACE_ID]/spans/[SPAN_ID],
//     TRACE_ID 32 hex characters and SPAN_ID 16, neither zero;
//   - spanId, displayName, startTime and endTime are required;
//   - displayName is up to 128 bytes; up to 32 attributes, each key up to
//     128 bytes and a string value up to 256 bytes; up to 32 annotations
//     (each with up to 4 attributes and a description of up to 256 bytes)
//     and 128 message events; up to 128 links; up to 128 stack frames;
//   - "You cannot update existing spans" (batchWrite), and CreateSpan: "If
//     a span ID already exists, an additional copy of the span will be
//     stored."
//
// Choices Google does not document (UNVERIFIED (docs re-read 2026-11-21:
// https://docs.cloud.google.com/trace/docs/reference/v2/rest/v2/projects.traces.spans)):
// a string over its limit is truncated as TruncatableString describes it
// (the first bytes kept, truncatedByteCount set) rather than refused, and
// what is over a count limit is dropped and counted in the matching
// dropped*Count field, as those fields describe; a span of another project
// than the request's, an end before the start, or a malformed ID is
// INVALID_ARGUMENT.

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The v2 limits.
const (
	MaxDisplayName     = 128
	MaxAttributes      = 32
	MaxAttributeKey    = 128
	MaxAttributeValue  = 256
	MaxAnnotations     = 32
	MaxMessageEvents   = 128
	MaxAnnotationAttrs = 4
	MaxAnnotationText  = 256
	MaxLinks           = 128
	MaxStackFrames     = 128
)

var (
	projectNameRE = regexp.MustCompile(`^projects/([^/]+)$`)
	spanNameRE    = regexp.MustCompile(`^projects/([^/]+)/traces/([0-9a-fA-F]{32})/spans/([0-9a-fA-F]{16})$`)
	spanIDRE      = regexp.MustCompile(`^[0-9a-fA-F]{16}$`)
	zeroTrace     = strings.Repeat("0", 32)
	zeroSpan      = strings.Repeat("0", 16)
)

type v2Server struct {
	cloudtracev2pb.UnimplementedTraceServiceServer
	s *Server
}

func (v *v2Server) BatchWriteSpans(_ context.Context, req *cloudtracev2pb.BatchWriteSpansRequest) (*emptypb.Empty, error) {
	m := projectNameRE.FindStringSubmatch(req.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("name %q is not projects/[PROJECT_ID]", req.GetName())
	}
	if len(req.GetSpans()) == 0 {
		return nil, apierror.InvalidArgument("spans is required: write at least one span")
	}
	var out []*Span
	for i, sp := range req.GetSpans() {
		st, err := v.s.checkV2(sp)
		if err != nil {
			return nil, apierror.InvalidArgument("spans[%d]: %s", i, apierror.From(err).Message)
		}
		if st.Project != m[1] {
			return nil, apierror.InvalidArgument("spans[%d]: span %s is not in %s", i, sp.GetName(), req.GetName())
		}
		out = append(out, st)
	}
	v.s.mu.Lock()
	err := v.s.add(out)
	v.s.mu.Unlock()
	if err != nil {
		return nil, apierror.Internal(err, "writing spans")
	}
	v.s.written(out)
	return &emptypb.Empty{}, nil
}

func (v *v2Server) CreateSpan(_ context.Context, sp *cloudtracev2pb.Span) (*cloudtracev2pb.Span, error) {
	st, err := v.s.checkV2(sp)
	if err != nil {
		return nil, err
	}
	v.s.mu.Lock()
	err = v.s.add([]*Span{st})
	v.s.mu.Unlock()
	if err != nil {
		return nil, apierror.Internal(err, "writing the span")
	}
	v.s.written([]*Span{st})
	return cloneSpan(st.V2), nil
}

// checkV2 checks a written span and returns it as stored, limits applied.
func (s *Server) checkV2(in *cloudtracev2pb.Span) (*Span, error) {
	m := spanNameRE.FindStringSubmatch(in.GetName())
	if m == nil {
		return nil, apierror.InvalidArgument("span name %q is not projects/[PROJECT_ID]/traces/[TRACE_ID]/spans/[SPAN_ID], TRACE_ID 32 and SPAN_ID 16 hexadecimal characters", in.GetName())
	}
	traceID, spanID := strings.ToLower(m[2]), strings.ToLower(m[3])
	if traceID == zeroTrace {
		return nil, apierror.InvalidArgument("span %s: the trace ID must not be zero", in.GetName())
	}
	if spanID == zeroSpan {
		return nil, apierror.InvalidArgument("span %s: the span ID must not be zero", in.GetName())
	}
	switch {
	case in.GetSpanId() == "":
		return nil, apierror.InvalidArgument("span %s: spanId is required", in.GetName())
	case !strings.EqualFold(in.GetSpanId(), spanID):
		return nil, apierror.InvalidArgument("span %s: spanId %q is not the name's [SPAN_ID]", in.GetName(), in.GetSpanId())
	case in.GetDisplayName().GetValue() == "":
		return nil, apierror.InvalidArgument("span %s: displayName is required", in.GetName())
	case in.GetStartTime() == nil:
		return nil, apierror.InvalidArgument("span %s: startTime is required", in.GetName())
	case in.GetEndTime() == nil:
		return nil, apierror.InvalidArgument("span %s: endTime is required", in.GetName())
	case in.GetEndTime().AsTime().Before(in.GetStartTime().AsTime()):
		return nil, apierror.InvalidArgument("span %s: endTime is before startTime", in.GetName())
	}
	if p := in.GetParentSpanId(); p != "" && !spanIDRE.MatchString(p) {
		return nil, apierror.InvalidArgument("span %s: parentSpanId %q is not 16 hexadecimal characters", in.GetName(), p)
	}
	sp := cloneSpan(in)
	sp.Name = "projects/" + m[1] + "/traces/" + traceID + "/spans/" + spanID
	sp.SpanId = spanID
	sp.ParentSpanId = strings.ToLower(sp.GetParentSpanId())
	applyLimits(sp)
	return &Span{Project: m[1], TraceID: traceID, SpanID: spanID, V2: sp, Received: s.now()}, nil
}

// truncate keeps the first n bytes of t, at a rune boundary, counting what
// it removed.
func truncate(t *cloudtracev2pb.TruncatableString, n int) {
	if t == nil || len(t.Value) <= n {
		return
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(t.Value[cut]) {
		cut--
	}
	t.TruncatedByteCount += int32(len(t.Value) - cut)
	t.Value = t.Value[:cut]
}

// limitAttributes keeps at most max attributes, in key order, dropping
// keys over MaxAttributeKey and truncating string values.
func limitAttributes(a *cloudtracev2pb.Span_Attributes, max int) {
	if a == nil {
		return
	}
	keys := make([]string, 0, len(a.AttributeMap))
	for k := range a.AttributeMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kept := 0
	for _, k := range keys {
		if len(k) > MaxAttributeKey || kept >= max {
			delete(a.AttributeMap, k)
			a.DroppedAttributesCount++
			continue
		}
		kept++
		if sv := a.AttributeMap[k].GetStringValue(); sv != nil {
			truncate(sv, MaxAttributeValue)
		}
	}
}

func applyLimits(sp *cloudtracev2pb.Span) {
	truncate(sp.DisplayName, MaxDisplayName)
	limitAttributes(sp.Attributes, MaxAttributes)
	if te := sp.TimeEvents; te != nil {
		var kept []*cloudtracev2pb.Span_TimeEvent
		ann, msg := 0, 0
		for _, e := range te.TimeEvent {
			switch v := e.GetValue().(type) {
			case *cloudtracev2pb.Span_TimeEvent_Annotation_:
				if ann >= MaxAnnotations {
					te.DroppedAnnotationsCount++
					continue
				}
				ann++
				truncate(v.Annotation.GetDescription(), MaxAnnotationText)
				limitAttributes(v.Annotation.GetAttributes(), MaxAnnotationAttrs)
			case *cloudtracev2pb.Span_TimeEvent_MessageEvent_:
				if msg >= MaxMessageEvents {
					te.DroppedMessageEventsCount++
					continue
				}
				msg++
			}
			kept = append(kept, e)
		}
		te.TimeEvent = kept
	}
	if l := sp.Links; l != nil && len(l.Link) > MaxLinks {
		l.DroppedLinksCount += int32(len(l.Link) - MaxLinks)
		l.Link = l.Link[:MaxLinks]
	}
	if st := sp.StackTrace; st != nil && st.StackFrames != nil && len(st.StackFrames.Frame) > MaxStackFrames {
		st.StackFrames.DroppedFramesCount += int32(len(st.StackFrames.Frame) - MaxStackFrames)
		st.StackFrames.Frame = st.StackFrames.Frame[:MaxStackFrames]
	}
}

// StringAttr is a v2 string attribute value.
func StringAttr(v string) *cloudtracev2pb.AttributeValue {
	return &cloudtracev2pb.AttributeValue{Value: &cloudtracev2pb.AttributeValue_StringValue{StringValue: &cloudtracev2pb.TruncatableString{Value: v}}}
}

// AttrString is an attribute value as v1 labels and the console show it.
func AttrString(v *cloudtracev2pb.AttributeValue) string {
	switch x := v.GetValue().(type) {
	case *cloudtracev2pb.AttributeValue_StringValue:
		return x.StringValue.GetValue()
	case *cloudtracev2pb.AttributeValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *cloudtracev2pb.AttributeValue_BoolValue:
		if x.BoolValue {
			return "true"
		}
		return "false"
	}
	return ""
}
