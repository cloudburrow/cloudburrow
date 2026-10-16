package logging

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

type memObjects struct {
	mu      sync.Mutex
	objects map[string]string // bucket/name: body
}

func (m *memObjects) WriteObject(_ context.Context, bucket, name, _ string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if bucket == "missing" {
		return status.Error(codes.NotFound, "no such bucket")
	}
	m.objects[bucket+"/"+name] = string(data)
	return nil
}

// CopyLogEntries (#1394) through the official REST client: the matching
// entries of _Default are written as a Cloud Storage sink names its objects,
// the operation's metadata and response say what was copied, a cancel ends a
// copy CANCELLED, and a failed write ends it FAILED.
func TestCopyLogEntries(t *testing.T) {
	ctx := context.Background()
	st := NewStore(0)
	srv := NewServer(st)
	a := NewAdmin(store.NewMemory())
	srv.UseAdmin(a)
	objs := &memObjects{objects: map[string]string{}}
	a.UseStorage(objs)
	a.copies.delay = 10 * time.Millisecond
	h := httptest.NewServer(NewRESTHandler(func(g grpc.ServiceRegistrar) { srv.Register(g); a.Register(g) }))
	defer h.Close()
	c, err := vlogging.NewConfigRESTClient(ctx, option.WithEndpoint(h.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	write := func(log string, ts time.Time, sev ltype.LogSeverity, text string) {
		if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: &monitoredres.MonitoredResource{Type: "global"}, Entries: []*loggingpb.LogEntry{{
			LogName: "projects/proj-1/logs/" + log, Timestamp: timestamppb.New(ts), Severity: sev,
			Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatal(err)
		}
	}
	// WriteLogEntries refuses entries older than the retention, so use
	// recent ones: the receive time is now, a later hour than t0's.
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour).Add(15 * time.Minute)
	write("app", t0, ltype.LogSeverity_ERROR, "one")
	write("app", t0.Add(time.Minute), ltype.LogSeverity_INFO, "skipped by the filter")
	write("cloudaudit.googleapis.com%2Factivity", t0.Add(time.Hour), ltype.LogSeverity_ERROR, "two")
	if _, err := srv.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{Resource: &monitoredres.MonitoredResource{Type: "global"}, Entries: []*loggingpb.LogEntry{{
		LogName: "projects/proj-2/logs/app", Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "another project"}}}}); err != nil {
		t.Fatal(err)
	}
	write("app", time.Now().UTC(), ltype.LogSeverity_ERROR, "three")

	op, err := c.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: "projects/proj-1/locations/global/buckets/_Default",
		Filter: "severity>=ERROR", Destination: "storage.googleapis.com/dest"})
	if err != nil {
		t.Fatalf("CopyLogEntries: %v", err)
	}
	if !strings.HasPrefix(op.Name(), "projects/proj-1/locations/global/operations/") {
		t.Errorf("operation name %q", op.Name())
	}
	resp, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if resp.GetLogEntriesCopiedCount() != 3 {
		t.Errorf("copied %d, want 3", resp.GetLogEntriesCopiedCount())
	}
	md, err := op.Metadata()
	if err != nil || md.GetState() != loggingpb.OperationState_OPERATION_STATE_SUCCEEDED || md.GetProgress() != 100 ||
		md.GetRequest().GetFilter() != "severity>=ERROR" || md.GetWriterIdentity() != "serviceAccount:service-proj-1@gcp-sa-logging.iam.gserviceaccount.com" ||
		md.GetEndTime() == nil {
		t.Errorf("metadata %v (%v)", md, err)
	}
	hour := func(ts time.Time) string {
		h := ts.Truncate(time.Hour)
		return h.Format("2006/01/02/15:04:05_") + h.Add(time.Hour-time.Second).Format("15:04:05")
	}
	objs.mu.Lock()
	var names []string
	for k := range objs.objects {
		names = append(names, k)
	}
	want := []string{"dest/app/" + hour(t0) + "_A0:", "dest/cloudaudit.googleapis.com/activity/" + hour(t0.Add(time.Hour)) + "_A0:",
		"dest/app/" + hour(time.Now().UTC()) + "_S0.json"}
	if len(names) != 3 {
		t.Errorf("objects %v, want 3", names)
	}
	for _, w := range want {
		found := false
		for k, body := range objs.objects {
			if strings.HasPrefix(k, w) && strings.HasSuffix(k, ".json") {
				found = true
				if strings.Count(body, "\n") != 1 || !strings.Contains(body, `"logName":"projects/proj-1/logs/`) {
					t.Errorf("%s holds %q", k, body)
				}
			}
		}
		if !found {
			t.Errorf("no object %s*; have %v", w, names)
		}
	}
	objs.mu.Unlock()

	// A cancel while it is scheduled.
	a.copies.delay = time.Hour
	op2, err := c.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: "projects/proj-1/locations/global/buckets/_Default",
		Destination: "storage.googleapis.com/dest2"})
	if err != nil {
		t.Fatal(err)
	}
	if md, _ := op2.Metadata(); md.GetState() != loggingpb.OperationState_OPERATION_STATE_SCHEDULED {
		t.Errorf("a new copy is %v, want SCHEDULED", md.GetState())
	}
	if err := c.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op2.Name()}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if _, err := op2.Wait(ctx); status.Code(err) != codes.Canceled {
		t.Errorf("a cancelled copy's Wait = %v, want CANCELED", err)
	}
	if md, _ := op2.Metadata(); md.GetState() != loggingpb.OperationState_OPERATION_STATE_CANCELLED || !md.GetCancellationRequested() {
		t.Errorf("cancelled metadata %v", md)
	}
	if err := c.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op2.Name()}); httpCode(err) != 400 {
		t.Errorf("cancelling a finished copy = %v", err)
	}

	// A write that fails.
	a.copies.delay = time.Millisecond
	op3, err := c.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: "projects/proj-1/locations/global/buckets/_Default",
		Destination: "storage.googleapis.com/missing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op3.Wait(ctx); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "Failed to write export data") {
		t.Errorf("a copy to a missing bucket = %v", err)
	}

	it := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: "projects/proj-1/locations/global", Filter: "request_type=CopyLogEntries"})
	n := 0
	for {
		_, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListOperations: %v", err)
		}
		n++
	}
	if n != 3 {
		t.Errorf("ListOperations listed %d, want 3", n)
	}

	for _, bad := range []*loggingpb.CopyLogEntriesRequest{
		{Name: "projects/proj-1/locations/global/buckets/_Default", Destination: "bigquery.googleapis.com/projects/proj-1/datasets/d"},
		{Name: "projects/proj-1/locations/global/buckets/_Default", Destination: "storage.googleapis.com/d", Filter: "severity>>"},
		{Name: "projects/proj-1/buckets/_Default", Destination: "storage.googleapis.com/d"},
	} {
		if _, err := c.CopyLogEntries(ctx, bad); httpCode(err) != 400 {
			t.Errorf("CopyLogEntries(%v) = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	if _, err := c.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: "projects/proj-1/locations/global/buckets/nope",
		Destination: "storage.googleapis.com/d"}); httpCode(err) != 404 {
		t.Errorf("copying a missing bucket = %v, want NOT_FOUND", err)
	}
	a.UseStorage(nil)
	if _, err := c.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: "projects/proj-1/locations/global/buckets/_Default",
		Destination: "storage.googleapis.com/d"}); httpCode(err) != 400 {
		t.Errorf("copying without storage = %v, want FAILED_PRECONDITION", err)
	}
}

// httpCode is a REST client error's HTTP status, or 0.
func httpCode(err error) int {
	var e *googleapi.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}
