package logging

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
)

// restServer is the Cloud Logging JSON API over a store, registered as
// cmd/cloudburrow registers it.
func restServer(t *testing.T) *httptest.Server {
	t.Helper()
	api := NewServer(NewStore(100))
	srv := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { api.Register(g) }))
	t.Cleanup(srv.Close)
	return srv
}

// Google's bindings are the routes, LoggingServiceV2's among them.
func TestRESTRoutesAreGooglesBindings(t *testing.T) {
	t.Parallel()
	have := map[string]bool{}
	for _, r := range Routes() {
		have[r.Method+" "+r.Pattern+" "+r.RPC] = true
	}
	for _, want := range []string{
		"POST /v2/entries:write LoggingServiceV2/WriteLogEntries",
		"POST /v2/entries:list LoggingServiceV2/ListLogEntries",
		"GET /v2/{parent=projects/*}/logs LoggingServiceV2/ListLogs",
		"DELETE /v2/{log_name=projects/*/logs/*} LoggingServiceV2/DeleteLog",
		"POST /v2/entries:tail LoggingServiceV2/TailLogEntries",
		"POST /v2/{parent=projects/*}/sinks ConfigServiceV2/CreateSink",
		"POST /v2/{parent=projects/*}/metrics MetricsServiceV2/CreateLogMetric",
	} {
		if !have[want] {
			t.Errorf("no route %s", want)
		}
	}
}

// The official client's REST transport writes entries, reads them back with
// a filter, lists the logs and deletes one, over JSON.
func TestRESTWriteAndReadWithTheOfficialRESTClient(t *testing.T) {
	t.Parallel()
	srv := restServer(t)
	ctx := context.Background()
	c, err := vlogging.NewRESTClient(ctx, option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	logName := "projects/" + project + "/logs/app%2Frest"
	if _, err := c.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		LogName:  logName,
		Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{
			{Severity: 200, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "started"}},
			{Severity: 400, Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "disk nearly full"}},
		},
	}); err != nil {
		t.Fatalf("WriteLogEntries: %v", err)
	}
	it := c.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{
		ResourceNames: []string{"projects/" + project}, Filter: "severity>=WARNING"})
	var got []string
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListLogEntries: %v", err)
		}
		got = append(got, e.GetLogName()+" "+e.GetTextPayload())
	}
	if want := logName + " disk nearly full"; len(got) != 1 || got[0] != want {
		t.Errorf("entries at WARNING or above = %q; want [%q]", got, want)
	}
	logs := c.ListLogs(ctx, &loggingpb.ListLogsRequest{Parent: "projects/" + project})
	if l, err := logs.Next(); err != nil || l != logName {
		t.Errorf("ListLogs = %q, %v; want %q", l, err, logName)
	}
	if err := c.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: logName}); err != nil {
		t.Fatalf("DeleteLog: %v", err)
	}
	if _, err := c.ListLogs(ctx, &loggingpb.ListLogsRequest{Parent: "projects/" + project}).Next(); err != iterator.Done {
		t.Errorf("ListLogs after DeleteLog: %v; want none", err)
	}
}

// The wire: gcloud's bodies, a stream and the unregistered services, which
// answer 501, and an unbound path, which answers 404.
func TestRESTWire(t *testing.T) {
	t.Parallel()
	srv := restServer(t)
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, c := range []struct {
		method, path, body string
		code               int
		has                string
	}{
		{"POST", "/v2/entries:write", `{"entries":[{"logName":"projects/demo-project/logs/g","resource":{"type":"global"},` +
			`"severity":"ERROR","jsonPayload":{"msg":"boom"}}]}`, 200, "{}"},
		{"POST", "/v2/entries:list", `{"resourceNames":["projects/demo-project"],"filter":"logName=\"projects/demo-project/logs/g\"",` +
			`"orderBy":"timestamp desc","pageSize":10}`, 200, `"msg":"boom"`},
		{"GET", "/v2/projects/demo-project/logs", "", 200, "projects/demo-project/logs/g"},
		{"POST", "/v2/entries:write", `{"entries":[],"noSuchField":1}`, 400, "INVALID_ARGUMENT"},
		{"POST", "/v2/entries:tail", `{"resourceNames":["projects/demo-project"]}`, 501, "UNIMPLEMENTED"},
		{"GET", "/v2/projects/demo-project/sinks", "", 501, "UNIMPLEMENTED"},
		{"GET", "/v2/projects/demo-project/metrics", "", 501, "UNIMPLEMENTED"},
		{"GET", "/v2/projects/demo-project/entries", "", 404, "NOT_FOUND"},
		{"DELETE", "/v2/projects/demo-project/logs/g", "", 200, "{}"},
	} {
		code, body := do(c.method, c.path, c.body)
		if code != c.code || !strings.Contains(body, c.has) || !json.Valid([]byte(body)) {
			t.Errorf("%s %s: %d %s; want %d containing %q", c.method, c.path, code, body, c.code, c.has)
		}
	}
}
