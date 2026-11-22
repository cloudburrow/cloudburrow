package trace

// The Cloud Trace API v1 (google.devtools.cloudtrace.v1.TraceService), as
// its reference and protos document it (read 2026-11-21:
// docs.cloud.google.com/trace/docs/reference/v1/rest/v1/projects.traces/list
// and the TraceService proto):
//
//   - ListTraces: view MINIMAL (the default: "only the project and trace
//     IDs"), ROOTSPAN (the root spans too) or COMPLETE (every span, as
//     GetTrace); startTime and endTime bound the interval (inclusive);
//     filter (filter.go); orderBy one of traceId, name (the root span's),
//     duration or start, each optionally followed by " desc" ("Only one
//     sort field is permitted"); pageSize <= 0 lets the server choose;
//   - GetTrace: one trace, NOT_FOUND when the project has none by that ID;
//   - PatchTraces: "If the ID of a trace that you send matches that of an
//     existing trace, any fields in the existing trace and its spans are
//     overwritten by the provided values, and any new fields provided are
//     merged with the existing trace data. If the ID does not match, a new
//     trace is created."
//   - a TraceSpan's name must be less than 128 bytes, its label keys less
//     than 128 bytes and values less than 16 KiB; its kind is RPC_SERVER or
//     RPC_CLIENT, or unspecified.
//
// v1 and v2 read the same spans: a v2 span is a v1 TraceSpan whose labels
// are its attributes (integers in decimal, booleans true or false) and
// whose kind is RPC_SERVER for SERVER, RPC_CLIENT for CLIENT and
// unspecified otherwise; a v1 span is stored as the v2 span with those
// attributes and kind. Choices Google does not document (UNVERIFIED (docs
// re-read 2026-11-21: https://docs.cloud.google.com/trace/docs/reference/v1/rest/v1/projects.traces/list)):
// a trace is in the interval when any of it is; the server's page size is
// 100; and a trace ID that is not 32 hexadecimal characters is
// INVALID_ARGUMENT.

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	cloudtracepb "cloud.google.com/go/trace/apiv1/tracepb"
	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

const (
	defaultPageSize = 100
	maxV1Name       = 128
	maxV1LabelKey   = 128
	maxV1LabelValue = 16 * 1024
)

var traceIDRE = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

type v1Server struct {
	cloudtracepb.UnimplementedTraceServiceServer
	s *Server
}

// V1Span is a stored span as v1 reads it.
func V1Span(sp *Span) *cloudtracepb.TraceSpan {
	out := &cloudtracepb.TraceSpan{Name: sp.Name(), StartTime: sp.V2.GetStartTime(), EndTime: sp.V2.GetEndTime()}
	out.SpanId, _ = strconv.ParseUint(sp.SpanID, 16, 64)
	if p := sp.V2.GetParentSpanId(); p != "" {
		out.ParentSpanId, _ = strconv.ParseUint(p, 16, 64)
	}
	switch sp.V2.GetSpanKind() {
	case cloudtracev2pb.Span_SERVER:
		out.Kind = cloudtracepb.TraceSpan_RPC_SERVER
	case cloudtracev2pb.Span_CLIENT:
		out.Kind = cloudtracepb.TraceSpan_RPC_CLIENT
	}
	if attrs := sp.V2.GetAttributes().GetAttributeMap(); len(attrs) > 0 {
		out.Labels = make(map[string]string, len(attrs))
		for k, v := range attrs {
			out.Labels[k] = AttrString(v)
		}
	}
	return out
}

func v1Trace(t Trace, view cloudtracepb.ListTracesRequest_ViewType) *cloudtracepb.Trace {
	out := &cloudtracepb.Trace{ProjectId: t.Project, TraceId: t.TraceID}
	switch view {
	case cloudtracepb.ListTracesRequest_ROOTSPAN:
		if r := t.Root(); r != nil {
			out.Spans = []*cloudtracepb.TraceSpan{V1Span(r)}
		}
	case cloudtracepb.ListTracesRequest_COMPLETE:
		for _, sp := range t.Spans {
			out.Spans = append(out.Spans, V1Span(sp))
		}
	}
	return out
}

// Order is a parsed ListTraces orderBy.
type Order struct {
	Field string // traceId, name, duration or start; "" is traceId
	Desc  bool
}

// ParseOrder parses a ListTraces orderBy.
func ParseOrder(s string) (Order, error) {
	f := strings.Fields(s)
	var o Order
	switch {
	case len(f) == 0:
		return o, nil
	case len(f) == 2 && f[1] == "desc":
		o.Desc = true
	case len(f) != 1:
		return o, apierror.InvalidArgument("orderBy %q: only one sort field is permitted: traceId, name, duration or start, optionally followed by desc", s)
	}
	switch f[0] {
	case "traceId", "name", "duration", "start":
		o.Field = f[0]
	default:
		return o, apierror.InvalidArgument("orderBy %q: the sort field is traceId, name, duration or start", s)
	}
	return o, nil
}

// Sort orders traces.
func (o Order) Sort(ts []Trace) {
	key := func(t Trace) (string, float64) {
		switch o.Field {
		case "name":
			if r := t.Root(); r != nil {
				return r.Name(), 0
			}
			return "", 0
		case "duration":
			return "", float64(t.End().Sub(t.Start()))
		case "start":
			return "", float64(t.Start().UnixNano())
		}
		return t.TraceID, 0
	}
	sort.SliceStable(ts, func(i, j int) bool {
		si, ni := key(ts[i])
		sj, nj := key(ts[j])
		less := si < sj || (si == sj && ni < nj)
		if si == sj && ni == nj {
			return ts[i].TraceID < ts[j].TraceID
		}
		if o.Desc {
			return !less
		}
		return less
	})
}

// Query is a trace search, as ListTraces and the Trace Explorer run it.
type Query struct {
	Projects   []string
	Start, End time.Time // zero is unbounded
	Filter     Filter
	Order      Order
}

// Search runs a query over the projects' traces.
func (s *Server) Search(q Query) []Trace {
	var out []Trace
	for _, t := range s.Traces(q.Projects...) {
		if !q.Start.IsZero() && t.End().Before(q.Start) {
			continue
		}
		if !q.End.IsZero() && t.Start().After(q.End) {
			continue
		}
		if !q.Filter.Match(t) {
			continue
		}
		out = append(out, t)
	}
	q.Order.Sort(out)
	return out
}

func (v *v1Server) ListTraces(_ context.Context, req *cloudtracepb.ListTracesRequest) (*cloudtracepb.ListTracesResponse, error) {
	if req.GetProjectId() == "" {
		return nil, apierror.InvalidArgument("projectId is required")
	}
	f, err := ParseFilter(req.GetFilter())
	if err != nil {
		return nil, err
	}
	o, err := ParseOrder(req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	q := Query{Projects: []string{req.GetProjectId()}, Filter: f, Order: o}
	if req.GetStartTime() != nil {
		q.Start = req.GetStartTime().AsTime()
	}
	if req.GetEndTime() != nil {
		q.End = req.GetEndTime().AsTime()
	}
	if !q.Start.IsZero() && !q.End.IsZero() && q.End.Before(q.Start) {
		return nil, apierror.InvalidArgument("endTime is before startTime")
	}
	ts := v.s.Search(q)
	keys := make([]string, len(ts))
	for i, t := range ts {
		keys[i] = fmt.Sprintf("%09d|%s", i, t.TraceID)
	}
	size := int(req.GetPageSize())
	if size <= 0 {
		size = defaultPageSize
	}
	scope := fmt.Sprintf("cloudtrace|%s|%s|%s|%s|%v|%v|%d", req.GetProjectId(), req.GetFilter(), req.GetOrderBy(), req.GetView(),
		req.GetStartTime().AsTime().UnixNano(), req.GetEndTime().AsTime().UnixNano(), size)
	page, next, err := paging.Page(scope, keys, req.GetPageToken(), size)
	if err != nil {
		return nil, apierror.InvalidArgument("pageToken: %v", err)
	}
	view := req.GetView()
	if view == cloudtracepb.ListTracesRequest_VIEW_TYPE_UNSPECIFIED {
		view = cloudtracepb.ListTracesRequest_MINIMAL
	}
	out := &cloudtracepb.ListTracesResponse{NextPageToken: next}
	for _, k := range page {
		i, _ := strconv.Atoi(k[:strings.IndexByte(k, '|')])
		out.Traces = append(out.Traces, v1Trace(ts[i], view))
	}
	return out, nil
}

func (v *v1Server) GetTrace(_ context.Context, req *cloudtracepb.GetTraceRequest) (*cloudtracepb.Trace, error) {
	if req.GetProjectId() == "" {
		return nil, apierror.InvalidArgument("projectId is required")
	}
	if !traceIDRE.MatchString(req.GetTraceId()) {
		return nil, apierror.InvalidArgument("traceId %q is not 32 hexadecimal characters", req.GetTraceId())
	}
	t, ok := v.s.Get(req.GetProjectId(), strings.ToLower(req.GetTraceId()))
	if !ok {
		return nil, apierror.NotFound("trace %s does not exist in project %s", req.GetTraceId(), req.GetProjectId())
	}
	return v1Trace(t, cloudtracepb.ListTracesRequest_COMPLETE), nil
}

func (v *v1Server) PatchTraces(_ context.Context, req *cloudtracepb.PatchTracesRequest) (*emptypb.Empty, error) {
	if req.GetProjectId() == "" {
		return nil, apierror.InvalidArgument("projectId is required")
	}
	if len(req.GetTraces().GetTraces()) == 0 {
		return nil, apierror.InvalidArgument("traces is required")
	}
	// Checked first, so a bad span changes nothing.
	for i, t := range req.GetTraces().GetTraces() {
		if !traceIDRE.MatchString(t.GetTraceId()) || strings.Trim(t.GetTraceId(), "0") == "" {
			return nil, apierror.InvalidArgument("traces[%d]: traceId %q is not 32 hexadecimal characters, not all zero", i, t.GetTraceId())
		}
		if p := t.GetProjectId(); p != "" && p != req.GetProjectId() {
			return nil, apierror.InvalidArgument("traces[%d]: projectId %q is not the request's %q", i, p, req.GetProjectId())
		}
		for j, sp := range t.GetSpans() {
			if sp.GetSpanId() == 0 {
				return nil, apierror.InvalidArgument("traces[%d].spans[%d]: spanId must be non-zero", i, j)
			}
			if len(sp.GetName()) >= maxV1Name {
				return nil, apierror.InvalidArgument("traces[%d].spans[%d]: name must be less than %d bytes", i, j, maxV1Name)
			}
			for k, val := range sp.GetLabels() {
				if len(k) >= maxV1LabelKey || len(val) >= maxV1LabelValue {
					return nil, apierror.InvalidArgument("traces[%d].spans[%d]: label %q: keys must be less than %d bytes and values less than %d", i, j, k, maxV1LabelKey, maxV1LabelValue)
				}
			}
		}
	}
	s := v.s
	s.mu.Lock()
	defer s.mu.Unlock()
	project := req.GetProjectId()
	for _, t := range req.GetTraces().GetTraces() {
		traceID := strings.ToLower(t.GetTraceId())
		byTrace := s.traces[project]
		if byTrace == nil {
			byTrace = map[string][]*Span{}
			s.traces[project] = byTrace
		}
		for _, in := range t.GetSpans() {
			spanID := fmt.Sprintf("%016x", in.GetSpanId())
			var cur *Span
			for _, sp := range byTrace[traceID] {
				if sp.SpanID == spanID {
					cur = sp
					break
				}
			}
			if cur == nil {
				cur = &Span{Project: project, TraceID: traceID, SpanID: spanID, Received: s.now(),
					V2: &cloudtracev2pb.Span{Name: "projects/" + project + "/traces/" + traceID + "/spans/" + spanID, SpanId: spanID}}
				byTrace[traceID] = append(byTrace[traceID], cur)
			}
			mergeV1(cur.V2, in)
		}
	}
	if err := s.save(); err != nil {
		return nil, apierror.Internal(err, "writing traces")
	}
	return &emptypb.Empty{}, nil
}

// mergeV1 overwrites the fields a v1 span provides and merges its labels.
func mergeV1(dst *cloudtracev2pb.Span, in *cloudtracepb.TraceSpan) {
	if in.GetName() != "" {
		dst.DisplayName = &cloudtracev2pb.TruncatableString{Value: in.GetName()}
	}
	if in.GetStartTime() != nil {
		dst.StartTime = timestamppb.New(in.GetStartTime().AsTime())
	}
	if in.GetEndTime() != nil {
		dst.EndTime = timestamppb.New(in.GetEndTime().AsTime())
	}
	if in.GetParentSpanId() != 0 {
		dst.ParentSpanId = fmt.Sprintf("%016x", in.GetParentSpanId())
	}
	switch in.GetKind() {
	case cloudtracepb.TraceSpan_RPC_SERVER:
		dst.SpanKind = cloudtracev2pb.Span_SERVER
	case cloudtracepb.TraceSpan_RPC_CLIENT:
		dst.SpanKind = cloudtracev2pb.Span_CLIENT
	}
	if len(in.GetLabels()) > 0 {
		if dst.Attributes == nil {
			dst.Attributes = &cloudtracev2pb.Span_Attributes{}
		}
		if dst.Attributes.AttributeMap == nil {
			dst.Attributes.AttributeMap = map[string]*cloudtracev2pb.AttributeValue{}
		}
		for k, v := range in.GetLabels() {
			dst.Attributes.AttributeMap[k] = StringAttr(v)
		}
	}
}
