package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/logging"
)

// TestConsoleDeleteLogThroughDeleteLog (#799): the Logs Explorer's Delete log
// calls DeleteLog on the same server the gRPC and JSON transports serve, so
// the deleted log is gone from ListLogs and ListLogEntries, the other log of
// the project and every pod line keep their entries, and the Explorer stops
// showing the deleted entries. A log of another project is refused before the
// API is called, and a failure carries the API's own message.
func TestConsoleDeleteLogThroughDeleteLog(t *testing.T) {
	ctx := context.Background()
	store := logging.NewStore(logging.DefaultLimit)
	svc := &loggingService{store: store, api: logging.NewServer(store)}

	srv := console.New("127.0.0.1:0", func(context.Context) console.Status { return console.Status{} })
	svc.toConsole(srv.Logs())
	srv.SetLogDeleter(svc.logDeleter(nil))
	web := httptest.NewServer(srv.Handler())
	t.Cleanup(web.Close)

	const project = "demo-app"
	app, other := "projects/demo-app/logs/app", "projects/demo-app/logs/other"
	for _, name := range []string{app, app, other, "projects/elsewhere-app/logs/app"} {
		if _, err := svc.api.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
			LogName:  name,
			Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "line of " + name}}},
		}); err != nil {
			t.Fatalf("WriteLogEntries %s: %v", name, err)
		}
	}
	srv.Logs().Log(console.Entry{Source: "kubernetes/api-pod", Project: project, Resource: app, Message: "pod line"})

	do := func(method, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, web.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	del := func(p, log string) (int, string) {
		return do(http.MethodDelete, "/api/logs?"+url.Values{"project": {p}, "log": {log}}.Encode())
	}

	if _, body := do(http.MethodGet, "/api/logs?limit=1"); !strings.Contains(body, `"logDelete":true`) {
		t.Errorf("/api/logs does not offer Delete log with Logging served: %s", body)
	}

	// Another project's log, from this project's screen.
	if code, body := del(project, "projects/elsewhere-app/logs/app"); code != http.StatusBadRequest ||
		!strings.Contains(body, "is not a log of project demo-app") {
		t.Errorf("cross-project delete = %d %s, want 400 naming the project", code, body)
	}

	if code, body := del(project, app); code != http.StatusOK {
		t.Fatalf("delete log = %d %s", code, body)
	}

	logs, err := svc.api.ListLogs(ctx, &loggingpb.ListLogsRequest{Parent: "projects/demo-app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(logs.GetLogNames(), ","); got != other {
		t.Errorf("ListLogs after the delete = %q, want only %s", got, other)
	}
	for name, want := range map[string]int{app: 0, other: 1, "projects/elsewhere-app/logs/app": 1} {
		resp, err := svc.api.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{
			ResourceNames: []string{name[:strings.Index(name, "/logs/")]}, Filter: `logName = "` + name + `"`,
		})
		if err != nil {
			t.Fatalf("ListLogEntries %s: %v", name, err)
		}
		if len(resp.GetEntries()) != want {
			t.Errorf("%s has %d entries after the delete, want %d", name, len(resp.GetEntries()), want)
		}
	}

	// The Explorer no longer shows the log, and still shows the rest.
	_, body := do(http.MethodGet, "/api/logs?limit=100")
	var got struct{ Entries []console.Entry }
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	var sources []string
	for _, e := range got.Entries {
		if strings.HasPrefix(e.Source, console.LoggingSourcePrefix) && e.Resource == app {
			t.Errorf("the Explorer still shows a deleted entry: %+v", e)
		}
		sources = append(sources, e.Source+" "+e.Resource)
	}
	for _, want := range []string{"logging/other " + other, "kubernetes/api-pod " + app, "logging/app projects/elsewhere-app/logs/app"} {
		if !strings.Contains(strings.Join(sources, "\n"), want) {
			t.Errorf("the Explorer lost %q; it shows %v", want, sources)
		}
	}

	// Deleted twice: the API's NOT_FOUND, in its own words.
	if code, body := del(project, app); code != http.StatusBadRequest || !strings.Contains(body, "NotFound: log "+app+" not found") {
		t.Errorf("second delete = %d %s, want the API's NotFound", code, body)
	}
}

// TestNoDeleteLogWithoutLogging: with no Logging API there is no Delete log,
// neither offered by /api/logs nor accepted by the route.
func TestNoDeleteLogWithoutLogging(t *testing.T) {
	var svc *loggingService
	if d := svc.logDeleter(nil); d != nil {
		t.Fatalf("a nil Logging service gave a deleter: %v", d)
	}
	srv := console.New("127.0.0.1:0", func(context.Context) console.Status { return console.Status{} })
	web := httptest.NewServer(srv.Handler())
	t.Cleanup(web.Close)

	resp, err := http.Get(web.URL + "/api/logs?limit=1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"logDelete":false`) {
		t.Errorf("/api/logs offers Delete log with no Logging API: %s", b)
	}
	req, _ := http.NewRequest(http.MethodDelete, web.URL+"/api/logs?project=demo-app&log=projects/demo-app/logs/app", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("DELETE /api/logs with no Logging API = %d, want 501", resp.StatusCode)
	}
}
