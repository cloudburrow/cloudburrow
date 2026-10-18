//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	vlogging "cloud.google.com/go/logging/apiv2"
)

// copyObjectRe is a Cloud Storage sink's object name: LOG_ID/YYYY/MM/DD/
// HH:00:00_HH:59:59_S0.json, or _A0:UNIX.json for late entries.
var copyObjectRe = regexp.MustCompile(`^(.+)/(\d{4})/(\d{2})/(\d{2})/(\d{2}):00:00_(\d{2}):59:59_(S0|A0:\d+)\.json$`)

// copyBucketObjects reads every object in bucket.
func copyBucketObjects(t *testing.T, ctx context.Context, b *storage.BucketHandle) map[string]string {
	t.Helper()
	out := map[string]string{}
	it := b.Objects(ctx, nil)
	for {
		a, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("listing the copy: %v", err)
		}
		r, err := b.Object(a.Name).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r)
		r.Close()
		out[a.Name] = string(body)
	}
}

// copyClients are the Logging client options and the Storage client the copy
// tests use, under the emulators shard's SINK_ names, beside the sinks that
// deliver to Cloud Storage (#1404), so the Logging and Storage suites stay
// in their shards; on a developer's instance the shared names do. Logging
// is read first: the storage shard exports STORAGE without LOGGING, and a
// gcloud session there has no logging override (CI run 38910752877).
func copyClients(t *testing.T, h *Harness) ([]option.ClientOption, *storage.Client) {
	t.Helper()
	// Under the shared names too, for loggingParentsFixture and the gcloud
	// session (#1568).
	opts := []option.ClientOption{option.WithEndpoint(shardEndpoint(h, envSinkLogging, EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	se := shardEndpoint(h, envSinkStorage, EnvStorage)
	t.Setenv("STORAGE_EMULATOR_HOST", se)
	sc, err := storage.NewClient(h.Context(), option.WithoutAuthentication(), option.WithEndpoint(se+"/storage/v1/"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return opts, sc
}

// covers: google.logging.v2.ConfigServiceV2/CopyLogEntries
//
// TestLoggingCopyLogEntries (#1394): through the official ConfigClient,
// CopyLogEntries copies the _Default bucket's entries that a filter matches
// to a Cloud Storage bucket on the same instance, which the official Storage
// client reads back: objects named as a Cloud Storage sink names them
// (LOG_ID/YYYY/MM/DD/HH:00:00_HH:59:59_S0.json), each holding LogEntry JSON
// one per line. The operation's metadata is a CopyLogEntriesMetadata
// (SUCCEEDED, progress 100, the request, writer_identity) and its response
// the copied count. A copy cancelled through the client's CancelOperation
// while it is still scheduled ends CANCELLED, and a copy to a missing bucket
// fails "Failed to write export data". A folder's and the organization's
// _Default buckets copy their own entries, under operations named for the
// parent, written as its Logging service agent (#1411).
func TestLoggingCopyLogEntries(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	opts, sc := copyClients(t, h)
	lc, err := vlogging.NewClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	cc, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano())
	dest := sc.Bucket(project + "-logcopy")
	if err := dest.Create(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for n := range copyBucketObjects(t, context.Background(), dest) {
			_ = dest.Object(n).Delete(context.Background())
		}
		_ = dest.Delete(context.Background())
	})

	logID := "copy-" + suffix
	now := time.Now().UTC()
	var entries []*loggingpb.LogEntry
	for i, sev := range []ltype.LogSeverity{ltype.LogSeverity_ERROR, ltype.LogSeverity_INFO, ltype.LogSeverity_CRITICAL} {
		entries = append(entries, &loggingpb.LogEntry{LogName: "projects/" + project + "/logs/" + logID,
			Timestamp: timestamppb.New(now.Add(time.Duration(i) * time.Millisecond)), Severity: sev,
			Payload: &loggingpb.LogEntry_TextPayload{TextPayload: fmt.Sprintf("entry %d", i)}})
	}
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		Resource: &mrpb.MonitoredResource{Type: "global"}, Entries: entries}); err != nil {
		t.Fatal(err)
	}
	source := "projects/" + project + "/locations/global/buckets/_Default"
	filter := fmt.Sprintf(`logName="projects/%s/logs/%s" AND severity>=ERROR`, project, logID)
	op, err := cc.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: source, Filter: filter,
		Destination: "storage.googleapis.com/" + project + "-logcopy"})
	if err != nil {
		t.Fatalf("CopyLogEntries: %v", err)
	}
	if !strings.HasPrefix(op.Name(), "projects/"+project+"/locations/global/operations/") {
		t.Errorf("operation %q", op.Name())
	}
	resp, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if resp.GetLogEntriesCopiedCount() != 2 {
		t.Errorf("copied %d entries, want 2", resp.GetLogEntriesCopiedCount())
	}
	md, err := op.Metadata()
	if err != nil || md.GetState() != loggingpb.OperationState_OPERATION_STATE_SUCCEEDED || md.GetProgress() != 100 ||
		md.GetRequest().GetFilter() != filter || !strings.HasPrefix(md.GetWriterIdentity(), "serviceAccount:") ||
		md.GetStartTime() == nil || md.GetEndTime() == nil {
		t.Errorf("metadata %v (%v)", md, err)
	}
	objs := copyBucketObjects(t, ctx, dest)
	if len(objs) != 1 {
		t.Fatalf("the copy wrote %d objects, want 1: %v", len(objs), objs)
	}
	for name, body := range objs {
		m := copyObjectRe.FindStringSubmatch(name)
		if m == nil || m[1] != logID || m[2]+"/"+m[3]+"/"+m[4] != now.Format("2006/01/02") || m[5] != now.Format("15") || m[6] != now.Format("15") {
			t.Errorf("object %q is not %s/%s/HH:00:00_HH:59:59_S0.json", name, logID, now.Format("2006/01/02"))
		}
		lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("%s holds %d lines, want 2: %s", name, len(lines), body)
		}
		for i, l := range lines {
			var e struct {
				LogName, Severity, TextPayload string
			}
			if err := json.Unmarshal([]byte(l), &e); err != nil || e.LogName != entries[0].GetLogName() ||
				e.TextPayload != []string{"entry 0", "entry 2"}[i] || e.Severity != []string{"ERROR", "CRITICAL"}[i] {
				t.Errorf("line %d is not the LogEntry: %s (%v)", i, l, err)
			}
		}
	}

	// A cancel while the copy is scheduled.
	op2, err := cc.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: source,
		Destination: "storage.googleapis.com/" + project + "-logcopy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cc.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op2.Name()}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if _, err := op2.Wait(ctx); status.Code(err) != codes.Canceled {
		t.Errorf("a cancelled copy's Wait = %v, want CANCELED", err)
	}
	if md, _ := op2.Metadata(); md.GetState() != loggingpb.OperationState_OPERATION_STATE_CANCELLED || !md.GetCancellationRequested() {
		t.Errorf("the cancelled copy's metadata %v", md)
	}

	op3, err := cc.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: source,
		Destination: "storage.googleapis.com/" + project + "-no-such-bucket-" + suffix})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op3.Wait(ctx); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "Failed to write export data") {
		t.Errorf("a copy to a missing bucket = %v", err)
	}
	_, err = cc.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: source, Destination: "bigquery.googleapis.com/projects/" + project + "/datasets/d"})
	wantLoggingCode(t, "a BigQuery destination", err, codes.InvalidArgument)

	// A folder's and the organization's _Default buckets (#1411): their own
	// entries, the operation named under the parent and the writer identity
	// the parent's service agent.
	folder, _ := loggingParentsFixture(t, h, "lcp-")
	for _, pc := range []struct{ parent, agent string }{
		{folder, "service-fld-" + strings.TrimPrefix(folder, "folders/") + "@"},
		{localOrg, "service-org-" + strings.TrimPrefix(localOrg, "organizations/") + "@"},
	} {
		// A log ID of each parent's own: objects are named by log ID and
		// hour, so a second copy of the same log ID would replace the first.
		log := pc.parent + "/logs/copy-" + strings.SplitN(pc.parent, "/", 2)[0] + "-" + suffix
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: log, Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "from " + pc.parent}}}}); err != nil {
			t.Fatal(err)
		}
		op, err := cc.CopyLogEntries(ctx, &loggingpb.CopyLogEntriesRequest{Name: pc.parent + "/locations/global/buckets/_Default",
			Filter: fmt.Sprintf(`logName="%s"`, log), Destination: "storage.googleapis.com/" + project + "-logcopy"})
		if err != nil {
			t.Fatalf("CopyLogEntries from %s: %v", pc.parent, err)
		}
		if !strings.HasPrefix(op.Name(), pc.parent+"/locations/global/operations/") {
			t.Errorf("operation %q is not under %s", op.Name(), pc.parent)
		}
		resp, err := op.Wait(ctx)
		if err != nil || resp.GetLogEntriesCopiedCount() != 1 {
			t.Errorf("the copy from %s = %v, %v; want 1 entry copied", pc.parent, resp, err)
		}
		if md, _ := op.Metadata(); !strings.HasPrefix(md.GetWriterIdentity(), "serviceAccount:"+pc.agent) {
			t.Errorf("the copy from %s has writer identity %q, want %s...", pc.parent, md.GetWriterIdentity(), pc.agent)
		}
		got, err := cc.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.Name()})
		if err != nil || !got.GetDone() {
			t.Errorf("GetOperation(%s) = %v, %v", op.Name(), got, err)
		}
	}
	objs = copyBucketObjects(t, ctx, dest)
	for _, want := range []string{"from " + folder, "from " + localOrg} {
		found := false
		for _, body := range objs {
			found = found || strings.Contains(body, want)
		}
		if !found {
			t.Errorf("no copied object holds %q", want)
		}
	}
}

// TestGcloudLoggingCopy (#1394): `gcloud logging copy` starts a copy of the
// _Default bucket's entries to a Cloud Storage bucket on the instance, and
// `gcloud logging operations describe` and `list` read its operation back,
// through the configuration `cloudburrow gcloud-setup` writes. The official
// Storage client reads the objects. `--folder` and `--organization` copy
// those parents' _Default buckets (#1411).
func TestGcloudLoggingCopy(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	_, sc := copyClients(t, h)
	g := newGcloudSession(t, h)
	project := h.Project()
	p := "--project=" + project
	dest := sc.Bucket(project + "-gcloudcopy")
	if err := dest.Create(ctx, project, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for n := range copyBucketObjects(t, context.Background(), dest) {
			_ = dest.Object(n).Delete(context.Background())
		}
		_ = dest.Delete(context.Background())
	})
	logID := fmt.Sprint("gcloud-copy-", time.Now().UnixNano())
	g.must("logging", "write", logID, "copied by gcloud", p, "--severity=WARNING")
	out := g.must("logging", "copy", "_Default", "storage.googleapis.com/"+project+"-gcloudcopy", "--location=global",
		`--log-filter=logName="projects/`+project+`/logs/`+logID+`"`, p)
	m := regexp.MustCompile(`projects/[^/\s]+/locations/global/operations/([^\s"']+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("gcloud logging copy named no operation:\n%s", out)
	}
	opID := m[1]
	var desc string
	for i := 0; i < 30; i++ {
		desc = g.must("logging", "operations", "describe", opID, "--location=global", p, "--format=json")
		if strings.Contains(desc, "OPERATION_STATE_SUCCEEDED") {
			break
		}
		time.Sleep(time.Second)
	}
	var op struct {
		Done     bool
		Metadata struct {
			State          string
			Progress       int
			WriterIdentity string
			Request        struct{ Destination, Filter string }
		}
		Response struct{ LogEntriesCopiedCount string }
	}
	if err := json.Unmarshal([]byte(desc), &op); err != nil || !op.Done || op.Metadata.State != "OPERATION_STATE_SUCCEEDED" ||
		op.Metadata.Request.Destination != "storage.googleapis.com/"+project+"-gcloudcopy" || op.Response.LogEntriesCopiedCount != "1" {
		t.Errorf("operations describe = %s (%v)", desc, err)
	}
	if got := g.must("logging", "operations", "list", "--location=global", "--operation-filter=request_type=CopyLogEntries", p, "--format=value(name)"); !strings.Contains(got, opID) {
		t.Errorf("operations list does not list %s:\n%s", opID, got)
	}
	objs := copyBucketObjects(t, ctx, dest)
	if len(objs) != 1 {
		t.Fatalf("the copy wrote %v, want one object", objs)
	}
	for name, body := range objs {
		if m := copyObjectRe.FindStringSubmatch(name); m == nil || m[1] != logID || !strings.Contains(body, `"copied by gcloud"`) {
			t.Errorf("object %s holds %s", name, body)
		}
	}

	// --folder and --organization (#1411): the parent's _Default bucket,
	// its operation described under the same flag.
	folder, _ := loggingParentsFixture(t, h, "gcp-")
	for _, flag := range []string{"--folder=" + strings.TrimPrefix(folder, "folders/"), "--organization=" + strings.TrimPrefix(localOrg, "organizations/")} {
		parent := map[bool]string{true: folder, false: localOrg}[strings.HasPrefix(flag, "--folder")]
		plog := fmt.Sprint("gcloud-pcopy-", time.Now().UnixNano())
		g.must("logging", "write", plog, "copied from "+parent, flag)
		out := g.must("logging", "copy", "_Default", "storage.googleapis.com/"+project+"-gcloudcopy", "--location=global",
			`--log-filter=logName="`+parent+`/logs/`+plog+`"`, flag)
		m := regexp.MustCompile(regexp.QuoteMeta(parent) + `/locations/global/operations/([^\s"']+)`).FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("gcloud logging copy %s named no operation under %s:\n%s", flag, parent, out)
		}
		var desc string
		for i := 0; i < 30; i++ {
			desc = g.must("logging", "operations", "describe", m[1], "--location=global", flag, "--format=json")
			if strings.Contains(desc, "OPERATION_STATE_SUCCEEDED") {
				break
			}
			time.Sleep(time.Second)
		}
		if !strings.Contains(desc, "OPERATION_STATE_SUCCEEDED") || !strings.Contains(desc, `"logEntriesCopiedCount": "1"`) {
			t.Errorf("operations describe %s = %s", flag, desc)
		}
		found := false
		for name, body := range copyBucketObjects(t, ctx, dest) {
			found = found || (strings.HasPrefix(name, plog+"/") && strings.Contains(body, "copied from "+parent))
		}
		if !found {
			t.Errorf("gcloud logging copy %s wrote no %s object", flag, plog)
		}
	}
}
