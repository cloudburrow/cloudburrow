package logging

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"cloud.google.com/go/logging/logadmin"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"cloud.google.com/go/logging/apiv2/loggingpb"
)

const project = "demo-project"

func serve(t *testing.T) (*Store, []option.ClientOption) {
	t.Helper()
	st := NewStore(100)
	g := grpc.NewServer()
	NewServer(st).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return st, []option.ClientOption{option.WithEndpoint(ln.Addr().String()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

func write(t *testing.T, opts []option.ClientOption) {
	t.Helper()
	ctx := context.Background()
	c, err := logging.NewClient(ctx, "projects/"+project, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A fixed resource, so the client does not probe for GCE to detect one.
	lg := c.Logger("app", logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	for _, e := range []logging.Entry{
		{Severity: logging.Info, Payload: map[string]any{"msg": "started", "n": 1}},
		{Severity: logging.Warning, Payload: "disk nearly full"},
		{Severity: logging.Error, Payload: map[string]any{"msg": "crashed"}},
	} {
		if err := lg.LogSync(ctx, e); err != nil {
			t.Fatalf("LogSync: %v", err)
		}
	}
	if err := c.Logger("other", logging.CommonResource(&mrpb.MonitoredResource{Type: "global"})).
		LogSync(ctx, logging.Entry{Severity: logging.Critical, Payload: "elsewhere"}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteAndReadBackWithLogadminFilters(t *testing.T) {
	_, opts := serve(t)
	write(t, opts)
	ctx := context.Background()
	ac, err := logadmin.NewClient(ctx, "projects/"+project, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	// logadmin appends its own `AND timestamp >= "…"`; the grammar must take it.
	it := ac.Entries(ctx, logadmin.Filter(`logName = "projects/`+project+`/logs/app" AND severity >= WARNING`))
	var got []string
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		got = append(got, e.Severity.String())
		if e.Severity == logging.Error {
			if m, ok := e.Payload.(*structpb.Struct); !ok || m.GetFields()["msg"].GetStringValue() != "crashed" {
				t.Errorf("structured payload = %#v", e.Payload)
			}
		}
	}
	if strings.Join(got, ",") != "Warning,Error" {
		t.Errorf("severity >= WARNING in app = %v, want [Warning Error]", got)
	}

	logs := ac.Logs(ctx)
	var names []string
	for {
		n, err := logs.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	// The client also writes its own instrumentation entry, to
	// "diagnostic-log"; the two the test wrote must both be listed.
	joined := "," + strings.Join(names, ",") + ","
	if !strings.Contains(joined, ",app,") || !strings.Contains(joined, ",other,") {
		t.Errorf("Logs = %v, want app and other among them", names)
	}
}

func TestUnsupportedFiltersAreRefusedByName(t *testing.T) {
	_, opts := serve(t)
	write(t, opts)
	ctx := context.Background()
	ac, err := logadmin.NewClient(ctx, "projects/"+project, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	for filter, term := range map[string]string{
		`jsonPayload.msg = "crashed"`:        "jsonPayload.msg",
		`severity >= WARNING OR logName = x`: "OR",
		`textPayload:"disk"`:                 "textPayload",
	} {
		_, err := ac.Entries(ctx, logadmin.Filter(filter)).Next()
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), term) {
			t.Errorf("filter %q = %v, want InvalidArgument naming %q", filter, err, term)
		}
	}
}

func TestBoundedAndObserved(t *testing.T) {
	st, opts := serve(t)
	var seen []*loggingpb.LogEntry
	st.Observe(func(e *loggingpb.LogEntry) { seen = append(seen, e) })
	st.limit = 2
	write(t, opts)
	if n := len(st.snapshot()); n != 2 {
		t.Errorf("store kept %d entries, want the limit of 2", n)
	}
	if len(seen) < 4 {
		t.Errorf("observer saw %d entries, want at least the 4 written", len(seen))
	}
	if seen[0].GetInsertId() == "" || seen[0].GetReceiveTimestamp().AsTime().After(time.Now()) {
		t.Errorf("entry not completed: %v", seen[0])
	}
}

// TestLabelFiltersWithLogadmin is #1333: the filter gcloud run services logs
// read sends — resource.labels.* and severity >= DEFAULT, newline separated —
// and labels.* terms, read through the official logadmin client.
func TestLabelFiltersWithLogadmin(t *testing.T) {
	_, opts := serve(t)
	ctx := context.Background()
	c, err := logging.NewClient(ctx, "projects/"+project, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rev := func(svc string) *mrpb.MonitoredResource {
		return &mrpb.MonitoredResource{Type: "cloud_run_revision", Labels: map[string]string{
			"project_id": project, "service_name": svc, "location": "us-central1", "revision_name": svc + "-00001-abc"}}
	}
	for _, w := range []struct {
		svc, payload string
		sev          logging.Severity
		labels       map[string]string
	}{
		{"hello", "one", logging.Default, map[string]string{"instanceId": "i1", "compute.googleapis.com/zone": "a"}},
		{"hello", "two", logging.Error, map[string]string{"instanceId": "I2"}},
		{"other", "three", logging.Info, nil},
	} {
		if err := c.Logger("run.googleapis.com%2Fstdout", logging.CommonResource(rev(w.svc))).
			LogSync(ctx, logging.Entry{Severity: w.sev, Payload: w.payload, Labels: w.labels}); err != nil {
			t.Fatal(err)
		}
	}
	ac, err := logadmin.NewClient(ctx, "projects/"+project, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	read := func(filter string) string {
		t.Helper()
		it := ac.Entries(ctx, logadmin.Filter(filter))
		var got []string
		for {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("filter %q: %v", filter, err)
			}
			got = append(got, e.Payload.(string))
		}
		return strings.Join(got, ",")
	}
	gcloud := "resource.type = cloud_run_revision \n AND resource.labels.service_name = hello \n AND " +
		"resource.labels.location = us-central1 \n AND severity >= DEFAULT \n"
	for filter, want := range map[string]string{
		gcloud:                                     "one,two",
		gcloud + " AND severity>=ERROR":            "two",
		gcloud + " AND severity >= 500":            "two",
		`resource.labels.service_name = "HELLO"`:   "one,two", // comparisons ignore letter case
		`resource.labels.service_name != hello`:    "three",
		`resource.labels.service_name >= hp`:       "three",
		`resource.labels.service_name < "i"`:       "one,two",
		`labels.instanceId = i2`:                   "two",
		`labels.instanceId != i1`:                  "two", // a missing label fails !=, too
		`labels."compute.googleapis.com/zone" = a`: "one",
		`labels.nothere = x`:                       "",
		`resource.labels.Service_name = hello`:     "", // keys are case-sensitive
	} {
		if got := read(filter); got != want {
			t.Errorf("filter %q = [%s], want [%s]", filter, got, want)
		}
	}
	for _, filter := range []string{`labels.a.b = x`, `resource.labels = x`, `labels."unterminated = x`} {
		if _, err := ac.Entries(ctx, logadmin.Filter(filter)).Next(); status.Code(err) != codes.InvalidArgument {
			t.Errorf("filter %q = %v, want InvalidArgument", filter, err)
		}
	}
}
