package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	cloudtracev2pb "cloud.google.com/go/trace/apiv2/tracepb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/logging"
	"github.com/cloudburrow/cloudburrow/internal/service/trace"
)

type v2Writer struct {
	cloudtracev2pb.TraceServiceServer
}

func (w *v2Writer) RegisterService(d *grpc.ServiceDesc, impl any) {
	if d.ServiceName == "google.devtools.cloudtrace.v2.TraceService" {
		w.TraceServiceServer = impl.(cloudtracev2pb.TraceServiceServer)
	}
}

// Trace explorer's search, filters and trace details, with the trace's log
// entry from Cloud Logging, over the servers in process (#2118).
func TestConsoleTraceExplorer(t *testing.T) {
	ctx := context.Background()
	api, err := trace.NewServer("")
	if err != nil {
		t.Fatal(err)
	}
	store := logging.NewStore(logging.DefaultLimit)
	logs := &loggingService{store: store, api: logging.NewServer(store)}
	p := traceExplorerProvider{svc: &traceService{api: api}, logging: logs}
	const project, tid = "trace-console", "4bf92f3577b34da6a3ce929d0e0e4736"

	var w v2Writer
	api.RegisterCloudTrace(&w)
	now := time.Now().Add(-time.Minute)
	mk := func(id, parent, name, service string, off, d time.Duration, failed bool) *cloudtracev2pb.Span {
		sp := &cloudtracev2pb.Span{Name: "projects/" + project + "/traces/" + tid + "/spans/" + id, SpanId: id, ParentSpanId: parent,
			DisplayName: &cloudtracev2pb.TruncatableString{Value: name}, StartTime: timestamppb.New(now.Add(off)),
			EndTime: timestamppb.New(now.Add(off + d)), SpanKind: cloudtracev2pb.Span_SERVER,
			Attributes: &cloudtracev2pb.Span_Attributes{AttributeMap: map[string]*cloudtracev2pb.AttributeValue{"service.name": trace.StringAttr(service)}}}
		if failed {
			sp.Status = &statuspb.Status{Code: 2, Message: "boom"}
		}
		return sp
	}
	if _, err := w.BatchWriteSpans(ctx, &cloudtracev2pb.BatchWriteSpansRequest{Name: "projects/" + project, Spans: []*cloudtracev2pb.Span{
		mk("00f067aa0ba902b7", "", "GET /cart", "frontend", 0, 200*time.Millisecond, false),
		mk("00f067aa0ba902b8", "00f067aa0ba902b7", "db.query", "cartdb", 20*time.Millisecond, 50*time.Millisecond, true),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Entries: []*loggingpb.LogEntry{{
		LogName: "projects/" + project + "/logs/app", Resource: &monitoredres.MonitoredResource{Type: "global"}, Trace: "projects/" + project + "/traces/" + tid, SpanId: "00f067aa0ba902b8",
		Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "query took long"}, Timestamp: timestamppb.New(now.Add(30 * time.Millisecond))}}}); err != nil {
		t.Fatal(err)
	}

	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 || !strings.HasSuffix(l.Items[0].Name, "/traceScopes/_Default") || l.Items[0].Fields["Default"] != "Default" {
		t.Fatalf("trace scopes = %+v, %v", l.Items, err)
	}
	search := func(values map[string]string) []string {
		t.Helper()
		out, err := p.Build(ctx, project, []string{"spans"}, values)
		if err != nil {
			t.Fatalf("spans %v: %v", values, err)
		}
		var names []string
		for _, it := range out.Items {
			names = append(names, it.Name)
		}
		return names
	}
	if got := search(map[string]string{"range": "3600"}); len(got) != 2 || got[0] != "db.query" {
		t.Errorf("every span, newest first = %v", got)
	}
	for values, want := range map[string]string{"service=frontend": "GET /cart", "status=Error": "db.query", "minDuration=100": "GET /cart",
		"filter=+root:/nope": "", "spanName=db": "db.query"} {
		k, v, _ := strings.Cut(values, "=")
		got := search(map[string]string{k: v})
		if strings.Join(got, ",") != want {
			t.Errorf("%s: %v, want %q", values, got, want)
		}
	}
	if _, err := p.Build(ctx, project, []string{"spans"}, map[string]string{"filter": `"unterminated`}); err == nil {
		t.Error("a bad filter was taken")
	}

	d, err := p.Build(ctx, project, []string{"trace"}, map[string]string{"trace": tid})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Items) != 2 || d.Items[0].Name != "GET /cart" || d.Items[1].Fields["depth"] != "1" || d.Items[1].Fields["Status"] != "Error: boom" {
		t.Fatalf("details = %+v", d.Items)
	}
	var evs []traceEvent
	if err := json.Unmarshal([]byte(d.Items[1].Fields["events"]), &evs); err != nil || len(evs) != 1 || evs[0].Text != "query took long" || evs[0].Kind != "log" {
		t.Errorf("the child span's Logs & Events = %v, %v", evs, err)
	}
	if _, err := p.Build(ctx, project, []string{"trace"}, map[string]string{"trace": strings.Repeat("1", 32)}); err == nil {
		t.Error("a missing trace opened")
	}
}
