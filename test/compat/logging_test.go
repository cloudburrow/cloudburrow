//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
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
)

// EnvLogging is the Cloud Logging endpoint.
const EnvLogging = "CLOUDBURROW_TEST_LOGGING"

// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries, google.logging.v2.LoggingServiceV2/ListLogEntries, google.logging.v2.LoggingServiceV2/ListLogs
//
// TestLoggingWriteAndRead (#304), with cloud.google.com/go/logging and
// logadmin against the CI instance: structured entries written, read back
// with severity and logName filters, an unsupported filter refused naming
// its term, and the entries visible in the console's Logs Explorer.
func TestLoggingWriteAndRead(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	opts := []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	parent := "projects/" + h.Project()
	c, err := logging.NewClient(ctx, parent, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	lg := c.Logger("compat-app", logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	for _, e := range []logging.Entry{
		{Severity: logging.Info, Payload: map[string]any{"event": "boot"}},
		{Severity: logging.Warning, Payload: map[string]any{"event": "slow", "ms": 900}},
		{Severity: logging.Error, Payload: map[string]any{"event": "crash", "code": 7}},
	} {
		if err := lg.LogSync(ctx, e); err != nil {
			t.Fatalf("LogSync: %v", err)
		}
	}

	ac, err := logadmin.NewClient(ctx, parent, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	logName := parent + "/logs/compat-app"
	it := ac.Entries(ctx, logadmin.Filter(`logName = "`+logName+`" AND severity >= WARNING`))
	var events []string
	for {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		b, _ := json.Marshal(e.Payload)
		events = append(events, e.Severity.String()+":"+string(b))
	}
	if len(events) != 2 || !strings.Contains(events[0], "slow") || !strings.Contains(events[1], "crash") {
		t.Errorf("severity >= WARNING entries = %v, want the slow and crash entries", events)
	}

	if _, err := ac.Entries(ctx, logadmin.Filter(`jsonPayload.event = "boot"`)).Next(); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "jsonPayload.event") {
		t.Errorf("unsupported filter = %v, want InvalidArgument naming jsonPayload.event", err)
	}

	// In the console's Logs Explorer, attributed to the log.
	q := url.Values{"project": {h.Project()}, "resource": {logName}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body := consoleDo(t, consoleAddr(t, h), http.MethodGet, "/api/logs?"+q.Encode(), "")
		if code == http.StatusOK && strings.Contains(body, "crash") && strings.Contains(body, "slow") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the API-written entries are not in the Logs Explorer: %d %s", code, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// covers: google.logging.v2.LoggingServiceV2/DeleteLog
//
// TestConsoleDeleteLogIsSeenByTheOfficialClient (#799): entries written with
// cloud.google.com/go/logging, one log deleted from the console's Logs
// Explorer, and logadmin then reads no entries for it and no longer lists it,
// while the project's other log keeps its entries. The console calls DeleteLog
// on the in-process service, so this is the API an SDK calls.
func TestConsoleDeleteLogIsSeenByTheOfficialClient(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	opts := []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	parent := "projects/" + h.Project()
	c, err := logging.NewClient(ctx, parent, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	gone, kept := "console-delete-"+suffix, "console-keep-"+suffix
	for _, id := range []string{gone, gone, kept} {
		lg := c.Logger(id, logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
		if err := lg.LogSync(ctx, logging.Entry{Severity: logging.Info, Payload: "line of " + id}); err != nil {
			t.Fatalf("LogSync %s: %v", id, err)
		}
	}

	ac, err := logadmin.NewClient(ctx, parent, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	entries := func(id string) int {
		t.Helper()
		it := ac.Entries(ctx, logadmin.Filter(`logName = "`+parent+`/logs/`+id+`"`))
		n := 0
		for {
			_, err := it.Next()
			if err == iterator.Done {
				return n
			}
			if err != nil {
				t.Fatalf("Entries %s: %v", id, err)
			}
			n++
		}
	}
	listed := func(id string) bool {
		t.Helper()
		it := ac.Logs(ctx)
		for {
			name, err := it.Next()
			if err == iterator.Done {
				return false
			}
			if err != nil {
				t.Fatalf("Logs: %v", err)
			}
			if name == id {
				return true
			}
		}
	}
	if entries(gone) != 2 || !listed(gone) {
		t.Fatalf("before the delete: %d entries of %s, listed %v; want 2 and listed", entries(gone), gone, listed(gone))
	}

	// The Explorer offers the delete, and it goes through.
	addr := consoleAddr(t, h)
	if code, body := consoleDo(t, addr, http.MethodGet, "/api/logs?limit=1", ""); code != http.StatusOK || !strings.Contains(body, `"logDelete":true`) {
		t.Fatalf("the Logs Explorer does not offer Delete log: %d %s", code, body)
	}
	q := url.Values{"project": {h.Project()}, "log": {parent + "/logs/" + gone}}
	if code, body := consoleDo(t, addr, http.MethodDelete, "/api/logs?"+q.Encode(), ""); code != http.StatusOK {
		t.Fatalf("console Delete log = %d %s", code, body)
	}

	if n := entries(gone); n != 0 {
		t.Errorf("logadmin reads %d entries of the deleted log %s, want none", n, gone)
	}
	if listed(gone) {
		t.Errorf("logadmin still lists the deleted log %s", gone)
	}
	if n := entries(kept); n != 1 {
		t.Errorf("the project's other log %s has %d entries, want 1", kept, n)
	}
	// And the Explorer no longer shows it.
	rq := url.Values{"project": {h.Project()}, "resource": {parent + "/logs/" + gone}, "source": {"logging/"}}
	if _, body := consoleDo(t, addr, http.MethodGet, "/api/logs?"+rq.Encode(), ""); strings.Contains(body, "line of "+gone) {
		t.Errorf("the Logs Explorer still shows the deleted log: %s", body)
	}
}
