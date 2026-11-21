package trace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	cloudtracepb "cloud.google.com/go/trace/apiv1/tracepb"
	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const tid = "0123456789abcdef0123456789abcdef"

func span(project, trace, id, parent, name string, start time.Time, d time.Duration, attrs map[string]string) *cloudtracev2pb.Span {
	sp := &cloudtracev2pb.Span{Name: "projects/" + project + "/traces/" + trace + "/spans/" + id, SpanId: id, ParentSpanId: parent,
		DisplayName: &cloudtracev2pb.TruncatableString{Value: name}, StartTime: timestamppb.New(start), EndTime: timestamppb.New(start.Add(d)),
		SpanKind: cloudtracev2pb.Span_SERVER}
	if len(attrs) > 0 {
		sp.Attributes = &cloudtracev2pb.Span_Attributes{AttributeMap: map[string]*cloudtracev2pb.AttributeValue{}}
		for k, v := range attrs {
			sp.Attributes.AttributeMap[k] = StringAttr(v)
		}
	}
	return sp
}

func code(err error) codes.Code {
	if s, ok := status.FromError(err); ok && s.Code() != codes.Unknown {
		return s.Code()
	}
	return apierror.From(err).Code
}

func TestWriteV2ReadV1(t *testing.T) {
	s, err := NewServer(filepath.Join(t.TempDir(), "trace.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	t0 := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	tid2 := strings.Repeat("ab", 16)
	_, err = s.v2.BatchWriteSpans(ctx, &cloudtracev2pb.BatchWriteSpansRequest{Name: "projects/p", Spans: []*cloudtracev2pb.Span{
		span("p", tid, "00000000000000a1", "", "/api/orders", t0, 300*time.Millisecond, map[string]string{"/http/method": "GET", "/http/url": "http://x/api/orders"}),
		span("p", tid, "00000000000000a2", "00000000000000a1", "db.query", t0.Add(10*time.Millisecond), 100*time.Millisecond, map[string]string{"db": "orders"}),
		span("p", tid2, "00000000000000b1", "", "/healthz", t0, 2*time.Millisecond, map[string]string{"/http/method": "POST"}),
	}})
	if err != nil {
		t.Fatal(err)
	}
	list := func(filter, order string, view cloudtracepb.ListTracesRequest_ViewType) []*cloudtracepb.Trace {
		t.Helper()
		r, err := s.v1.ListTraces(ctx, &cloudtracepb.ListTracesRequest{ProjectId: "p", Filter: filter, OrderBy: order, View: view})
		if err != nil {
			t.Fatalf("ListTraces(%q): %v", filter, err)
		}
		return r.GetTraces()
	}
	cases := map[string]int{"": 2, "/api": 1, "+/api": 0, "+/api/orders": 1, "root:/health": 1, "span:db": 1, "+span:db.query": 1,
		"latency:100ms": 1, "latency:1": 2, "latency:1s": 0, "method:GET": 1, "+method:GE": 0, "^db:orders": 0, "db:ord": 1, "label:db": 1,
		"^label:db": 0, `url:"http://x/api"`: 1, "method:POST /healthz": 1}
	for f, want := range cases {
		if got := len(list(f, "", 0)); got != want {
			t.Errorf("filter %q: %d traces, want %d", f, got, want)
		}
	}
	if ts := list("", "duration desc", cloudtracepb.ListTracesRequest_ROOTSPAN); ts[0].GetTraceId() != tid || len(ts[0].GetSpans()) != 1 || ts[0].GetSpans()[0].GetName() != "/api/orders" {
		t.Errorf("ROOTSPAN, duration desc: %v", ts)
	}
	if ts := list("", "", 0); len(ts[0].GetSpans()) != 0 || ts[0].GetProjectId() != "p" {
		t.Errorf("MINIMAL holds spans: %v", ts[0])
	}
	if _, err := s.v1.ListTraces(ctx, &cloudtracepb.ListTracesRequest{ProjectId: "p", OrderBy: "name,start"}); code(err) != codes.InvalidArgument {
		t.Errorf("two sort fields: %v", err)
	}
	tr, err := s.v1.GetTrace(ctx, &cloudtracepb.GetTraceRequest{ProjectId: "p", TraceId: tid})
	if err != nil || len(tr.GetSpans()) != 2 {
		t.Fatalf("GetTrace: %v %v", tr, err)
	}
	if sp := tr.GetSpans()[1]; sp.GetParentSpanId() != 0xa1 || sp.GetKind() != cloudtracepb.TraceSpan_RPC_SERVER || sp.GetLabels()["db"] != "orders" {
		t.Errorf("child span: %v", sp)
	}
	if _, err := s.v1.GetTrace(ctx, &cloudtracepb.GetTraceRequest{ProjectId: "p", TraceId: strings.Repeat("1", 32)}); code(err) != codes.NotFound {
		t.Errorf("missing trace: %v", err)
	}
	// PatchTraces overwrites the given fields and merges labels.
	_, err = s.v1.PatchTraces(ctx, &cloudtracepb.PatchTracesRequest{ProjectId: "p", Traces: &cloudtracepb.Traces{Traces: []*cloudtracepb.Trace{{
		TraceId: tid, Spans: []*cloudtracepb.TraceSpan{{SpanId: 0xa2, Labels: map[string]string{"rows": "3"}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ = s.v1.GetTrace(ctx, &cloudtracepb.GetTraceRequest{ProjectId: "p", TraceId: tid})
	if l := tr.GetSpans()[1].GetLabels(); l["db"] != "orders" || l["rows"] != "3" || tr.GetSpans()[1].GetName() != "db.query" {
		t.Errorf("patched span: %v", tr.GetSpans()[1])
	}
	// Persisted.
	s2, err := NewServer(s.state)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.Traces("p")); got != 2 {
		t.Errorf("reloaded %d traces", got)
	}
}

func TestV2Rules(t *testing.T) {
	s, _ := NewServer("")
	ctx := context.Background()
	now := time.Now()
	bad := []*cloudtracev2pb.Span{
		span("p", strings.Repeat("0", 32), "00000000000000a1", "", "x", now, time.Second, nil),
		span("p", tid, "0000000000000000", "", "x", now, time.Second, nil),
		span("p", "abc", "00000000000000a1", "", "x", now, time.Second, nil),
		span("p", tid, "00000000000000a1", "", "", now, time.Second, nil),
		span("p", tid, "00000000000000a1", "", "x", now, -time.Second, nil),
		span("q", tid, "00000000000000a1", "", "x", now, time.Second, nil),
	}
	for i, sp := range bad {
		if _, err := s.v2.BatchWriteSpans(ctx, &cloudtracev2pb.BatchWriteSpansRequest{Name: "projects/p", Spans: []*cloudtracev2pb.Span{sp}}); code(err) != codes.InvalidArgument {
			t.Errorf("bad span %d: %v", i, err)
		}
	}
	attrs := map[string]string{strings.Repeat("k", 129): "v", "long": strings.Repeat("é", 200)}
	for i := 0; i < 40; i++ {
		attrs["a"+string(rune('A'+i))] = "v"
	}
	got, err := s.v2.CreateSpan(ctx, span("p", tid, "00000000000000a1", "", strings.Repeat("n", 200), now, time.Second, attrs))
	if err != nil {
		t.Fatal(err)
	}
	if n := got.GetDisplayName(); len(n.GetValue()) != 128 || n.GetTruncatedByteCount() != 72 {
		t.Errorf("display name: %d bytes, %d truncated", len(n.GetValue()), n.GetTruncatedByteCount())
	}
	if a := got.GetAttributes(); len(a.GetAttributeMap()) != 32 || a.GetDroppedAttributesCount() != 10 {
		t.Errorf("attributes: %d kept, %d dropped", len(a.GetAttributeMap()), a.GetDroppedAttributesCount())
	}
	// A second copy of the same span ID is stored.
	if _, err := s.v2.CreateSpan(ctx, span("p", tid, "00000000000000a1", "", "again", now, time.Second, nil)); err != nil {
		t.Fatal(err)
	}
	if tr, _ := s.Get("p", tid); len(tr.Spans) != 2 {
		t.Errorf("%d copies", len(tr.Spans))
	}
}

func TestOTLPHTTPJSON(t *testing.T) {
	s, _ := NewServer("")
	s.SetDefaultProject("dflt")
	now := time.Now().UnixNano()
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}},{"key":"gcp.project_id","value":{"stringValue":"p"}}]},
"scopeSpans":[{"scope":{"name":"lib"},"spans":[{"traceId":"` + tid + `","spanId":"00000000000000c1","name":"GET /cart","kind":2,
"startTimeUnixNano":"` + strconv.FormatInt(now, 10) + `","endTimeUnixNano":"` + strconv.FormatInt(now+5e6, 10) + `","attributes":[{"key":"http.status_code","value":{"intValue":"200"}}],
"status":{"code":2,"message":"boom"}},{"traceId":"zz","spanId":"00","name":"bad"}]}]}]}`
	r := httptest.NewRequest(http.MethodPost, OTLPPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if !s.OTLPHandler(w, r) {
		t.Fatal("not served")
	}
	if w.Code != 400 && w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// "zz" is not hex: the whole JSON body is refused.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad hex: %d %s", w.Code, w.Body)
	}
	body = strings.Replace(body, `"traceId":"zz","spanId":"00"`, `"traceId":"","spanId":""`, 1)
	r = httptest.NewRequest(http.MethodPost, OTLPPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.OTLPHandler(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"rejectedSpans":"1"`) {
		t.Fatalf("partial success: %d %s", w.Code, w.Body)
	}
	tr, ok := s.Get("p", tid)
	if !ok || len(tr.Spans) != 1 {
		t.Fatalf("trace: %v", tr)
	}
	sp := tr.Spans[0].V2
	if sp.GetDisplayName().GetValue() != "GET /cart" || sp.GetSpanKind() != cloudtracev2pb.Span_SERVER || sp.GetStatus().GetCode() != 2 ||
		AttrString(sp.GetAttributes().GetAttributeMap()["service.name"]) != "checkout" || AttrString(sp.GetAttributes().GetAttributeMap()["http.status_code"]) != "200" {
		t.Errorf("span: %v", sp)
	}
}
