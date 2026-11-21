//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	cloudtracepb "cloud.google.com/go/trace/apiv1/tracepb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunRequestTraces (#2118): "Incoming requests to Cloud Run
// services automatically generate traces", and "traceparent is
// automatically populated" (docs.cloud.google.com/run/docs/trace). A
// request carrying a sampled traceparent reaches the container with a
// traceparent of the same trace whose parent is Cloud Run's own request
// span; that span is in Cloud Trace, read back with the official v1
// client; and the line the container logs with the trace and spanId is in
// Cloud Logging under that trace, which the console links to the span.
func TestCloudRunRequestTraces(t *testing.T) {
	h := New(t)
	rc := runClient(t, h)
	traceAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_TRACE", EnvTrace)
	logAddr := runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_LOGGING", EnvLogging)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	loadEnvProbe(ctx, t)
	tc := traceV1Clients(t, h, traceAddr)["grpc"]
	// The generated client, as logadmin's Entry has no span ID.
	lc, err := vlogging.NewClient(ctx, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lc.Close() })

	id := "compat-runtrace"
	name := runParent(h) + "/services/" + id
	op, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: envProbeImage}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = rc.DeleteService(delCtx, &runpb.DeleteServiceRequest{Name: name})
	})
	svc, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the service never became ready: %v", err)
	}
	traceID := newTraceID("runtrace")
	callerSpan := "00f067aa0ba902b7"
	marker := "trace-" + time.Now().Format("150405.000000")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ingress(t)+"/trace?marker="+marker+"&project="+h.Project(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = hostOf(t, svc.GetUri())
	req.Header.Set("traceparent", "00-"+traceID+"-"+callerSpan+"-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	buf := make([]byte, 512)
	n, _ := resp.Body.Read(buf)
	body.Write(buf[:n])
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/trace = %d %s", resp.StatusCode, body.String())
	}
	got := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body.String()), "TRACE PROBE:"))
	parts := strings.Split(got, "-")
	if len(parts) != 4 || parts[1] != traceID || parts[2] == callerSpan {
		t.Fatalf("the container's traceparent = %q; want trace %s under a span of Cloud Run's, not the caller's %s", got, traceID, callerSpan)
	}
	// Cloud Run's request span, a child of the caller's, in Cloud Trace.
	deadline := time.Now().Add(2 * time.Minute)
	var tr *cloudtracepb.Trace
	// Each component exports in its own batch, so the trace fills in.
	for {
		tr, err = tc.GetTrace(ctx, &cloudtracepb.GetTraceRequest{ProjectId: h.Project(), TraceId: traceID})
		var hasCallerParent bool
		for _, sp := range tr.GetSpans() {
			if sp.GetParentSpanId() == 0x00f067aa0ba902b7 {
				hasCallerParent = true
			}
		}
		if err == nil && hasCallerParent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GetTrace(%s): %v %v; want Cloud Run's request span, a child of the caller's", traceID, tr, err)
		}
		time.Sleep(2 * time.Second)
	}
	// The container's log line, under the trace and the span it was given.
	filter := `trace = "projects/` + h.Project() + `/traces/` + traceID + `" AND jsonPayload.message:"` + marker + `"`
	deadline = time.Now().Add(time.Minute)
	for {
		var got []*loggingpb.LogEntry
		it := lc.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + h.Project()}, Filter: filter})
		for {
			e, err := it.Next()
			if errors.Is(err, iterator.Done) {
				break
			}
			if err != nil {
				t.Fatalf("ListLogEntries(%s): %v", filter, err)
			}
			got = append(got, e)
		}
		if len(got) > 0 {
			if e := got[0]; e.GetSpanId() != parts[2] || !strings.Contains(e.GetTrace(), traceID) {
				t.Errorf("the correlated entry: trace %q span %q; want %s and %s", e.GetTrace(), e.GetSpanId(), traceID, parts[2])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no entry of the trace holds the container's line %s", marker)
		}
		time.Sleep(time.Second)
	}
}
