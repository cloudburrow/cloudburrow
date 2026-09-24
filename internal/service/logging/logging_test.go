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
