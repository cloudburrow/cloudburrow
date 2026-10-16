//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// loggingParentsFixture is a folder under the organization holding a new
// project, for the folder and organization Logging parents (#1395).
func loggingParentsFixture(t *testing.T, h *Harness, prefix string) (folder, project string) {
	t.Helper()
	ctx := h.Context()
	fc, _, pc := foldersClients(t, h)
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, prefix)}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	f, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	project = v1ID(h, prefix)
	cop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: project, Parent: f.GetName()}})
	if err != nil {
		t.Fatalf("CreateProject in %s: %v", f.GetName(), err)
	}
	if _, err := cop.Wait(ctx); err != nil {
		t.Fatalf("CreateProject in %s: %v", f.GetName(), err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if op, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/" + project}); err == nil {
			_, _ = op.Wait(ctx)
		}
		_, _ = fc.DeleteFolder(ctx, &rmpb.DeleteFolderRequest{Name: f.GetName()})
	})
	return f.GetName(), project
}

func entryTexts(t *testing.T, h *Harness, c *vlogging.Client, names ...string) []string {
	t.Helper()
	var out []string
	it := c.ListLogEntries(h.Context(), &loggingpb.ListLogEntriesRequest{ResourceNames: names})
	for {
		e, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("ListLogEntries(%v): %v", names, err)
		}
		out = append(out, e.GetTextPayload())
	}
}

// covers: google.logging.v2.ConfigServiceV2/CreateSink
// covers: google.logging.v2.ConfigServiceV2/GetSink
// covers: google.logging.v2.ConfigServiceV2/ListSinks
// covers: google.logging.v2.ConfigServiceV2/UpdateSink
// covers: google.logging.v2.ConfigServiceV2/DeleteSink
// covers: google.logging.v2.ConfigServiceV2/CreateExclusion
// covers: google.logging.v2.ConfigServiceV2/GetExclusion
// covers: google.logging.v2.ConfigServiceV2/ListExclusions
// covers: google.logging.v2.ConfigServiceV2/DeleteExclusion
// covers: google.logging.v2.ConfigServiceV2/GetBucket
// covers: google.logging.v2.ConfigServiceV2/CreateBucket
// covers: google.logging.v2.ConfigServiceV2/DeleteBucket
// covers: google.logging.v2.ConfigServiceV2/ListViews
// covers: google.logging.v2.ConfigServiceV2/GetSettings
// covers: google.logging.v2.ConfigServiceV2/UpdateSettings
// covers: google.logging.v2.ConfigServiceV2/GetCmekSettings
// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
// covers: google.logging.v2.LoggingServiceV2/ListLogs
// covers: google.logging.v2.LoggingServiceV2/DeleteLog
//
// TestLoggingFolderOrganizationAndBillingParents (#1395), with the official
// apiv2 ConfigClient and Client: under a folder Resource Manager holds, an
// aggregated sink (include_children) whose destination is a log bucket
// routes its child project's matching entries there, readable through the
// bucket's _AllLogs view, while the folder's own entries are listed under
// folders/{n}; an exclusion and the folder's _Default bucket, views and CMEK
// settings are served; the organization's sinks, buckets and settings are
// stored; a billing account's sinks are stored without include_children; a
// folder that does not exist is NOT_FOUND, and a metric under an
// organization INVALID_ARGUMENT.
func TestLoggingFolderOrganizationAndBillingParents(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	folder, child := loggingParentsFixture(t, h, "lgp-")
	cc, err := vlogging.NewConfigClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	lc, err := vlogging.NewClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())

	// The destination: a bucket in the harness project.
	bucketID := "agg-" + suffix
	bucket := "projects/" + h.Project() + "/locations/global/buckets/" + bucketID
	if _, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: "projects/" + h.Project() + "/locations/global", BucketId: bucketID}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	defer func() { _ = cc.DeleteBucket(context.Background(), &loggingpb.DeleteBucketRequest{Name: bucket}) }()

	childLog := "projects/" + child + "/logs/app"
	sink, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: folder, Sink: &loggingpb.LogSink{
		Name: "agg", Destination: "logging.googleapis.com/" + bucket, IncludeChildren: true, Filter: `logName="` + childLog + `"`}})
	if err != nil {
		t.Fatalf("CreateSink(%s): %v", folder, err)
	}
	if sink.GetName() != "agg" || !sink.GetIncludeChildren() {
		t.Errorf("folder sink = %v", sink)
	}
	if got, err := cc.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: folder + "/sinks/agg"}); err != nil || got.GetDestination() != sink.GetDestination() {
		t.Errorf("GetSink = %v, %v", got, err)
	}
	if s, err := cc.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: folder}).Next(); err != nil || s.GetName() != "agg" {
		t.Errorf("ListSinks(%s) = %v, %v", folder, s, err)
	}

	write := func(logName, text string) {
		t.Helper()
		if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{LogName: logName,
			Resource: &mrpb.MonitoredResource{Type: "global"},
			Entries:  []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: text}}}}); err != nil {
			t.Fatalf("WriteLogEntries(%s): %v", logName, err)
		}
	}
	write(childLog, "from-child-"+suffix)
	write("projects/"+child+"/logs/other", "unrouted-"+suffix)
	write(folder+"/logs/audit", "folder-own-"+suffix)

	if got := entryTexts(t, h, lc, bucket+"/views/_AllLogs"); strings.Join(got, ",") != "from-child-"+suffix {
		t.Errorf("the aggregated sink's bucket holds %v; want only the child's entry its filter matches", got)
	}
	if got := entryTexts(t, h, lc, folder); strings.Join(got, ",") != "folder-own-"+suffix {
		t.Errorf("ListLogEntries(%s) = %v; want the folder's own entry", folder, got)
	}
	if logs, err := lc.ListLogs(ctx, &loggingpb.ListLogsRequest{Parent: folder}).Next(); err != nil || logs != folder+"/logs/audit" {
		t.Errorf("ListLogs(%s) = %v, %v", folder, logs, err)
	}
	if err := lc.DeleteLog(ctx, &loggingpb.DeleteLogRequest{LogName: folder + "/logs/audit"}); err != nil {
		t.Errorf("DeleteLog: %v", err)
	}
	if got := entryTexts(t, h, lc, folder); len(got) != 0 {
		t.Errorf("after DeleteLog, %s lists %v", folder, got)
	}

	// Disabling the sink stops the routing.
	if _, err := cc.UpdateSink(ctx, &loggingpb.UpdateSinkRequest{SinkName: folder + "/sinks/agg", Sink: &loggingpb.LogSink{Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); err != nil {
		t.Fatalf("UpdateSink: %v", err)
	}
	write(childLog, "after-disable-"+suffix)
	if got := entryTexts(t, h, lc, bucket+"/views/_AllLogs"); len(got) != 1 {
		t.Errorf("a disabled sink still routed: %v", got)
	}
	if err := cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: folder + "/sinks/agg"}); err != nil {
		t.Errorf("DeleteSink: %v", err)
	}

	// Exclusions, buckets, views and settings of the folder.
	if _, err := cc.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: folder, Exclusion: &loggingpb.LogExclusion{Name: "quiet", Filter: "severity<WARNING"}}); err != nil {
		t.Fatalf("CreateExclusion(%s): %v", folder, err)
	}
	if e, err := cc.GetExclusion(ctx, &loggingpb.GetExclusionRequest{Name: folder + "/exclusions/quiet"}); err != nil || e.GetFilter() != "severity<WARNING" {
		t.Errorf("GetExclusion = %v, %v", e, err)
	}
	if e, err := cc.ListExclusions(ctx, &loggingpb.ListExclusionsRequest{Parent: folder}).Next(); err != nil || e.GetName() != "quiet" {
		t.Errorf("ListExclusions = %v, %v", e, err)
	}
	if err := cc.DeleteExclusion(ctx, &loggingpb.DeleteExclusionRequest{Name: folder + "/exclusions/quiet"}); err != nil {
		t.Errorf("DeleteExclusion: %v", err)
	}
	if b, err := cc.GetBucket(ctx, &loggingpb.GetBucketRequest{Name: folder + "/locations/global/buckets/_Default"}); err != nil || b.GetRetentionDays() != 30 {
		t.Errorf("GetBucket(folder _Default) = %v, %v", b, err)
	}
	if v, err := cc.ListViews(ctx, &loggingpb.ListViewsRequest{Parent: folder + "/locations/global/buckets/_Default"}).Next(); err != nil || v.GetName() != folder+"/locations/global/buckets/_Default/views/_AllLogs" {
		t.Errorf("ListViews(folder _Default) = %v, %v", v, err)
	}
	if c, err := cc.GetCmekSettings(ctx, &loggingpb.GetCmekSettingsRequest{Name: folder + "/cmekSettings"}); err != nil || c.GetName() != folder+"/cmekSettings" {
		t.Errorf("GetCmekSettings(folder) = %v, %v", c, err)
	}
	if st, err := cc.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: folder + "/settings"}); err != nil || st.GetName() != folder+"/settings" {
		t.Errorf("GetSettings(folder) = %v, %v", st, err)
	}
	_, err = cc.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: folder + "/settings", Settings: &loggingpb.Settings{DisableDefaultSink: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disable_default_sink"}}})
	wantLoggingCode(t, "UpdateSettings(folder): settings are configured for organizations", err, codes.FailedPrecondition)

	// The organization: a sink, a bucket and its settings, restored after.
	orgSink := "org-" + suffix
	if _, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: localOrg, Sink: &loggingpb.LogSink{
		Name: orgSink, Destination: "storage.googleapis.com/some-bucket", IncludeChildren: true}}); err != nil {
		t.Fatalf("CreateSink(org): %v", err)
	}
	if err := cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: localOrg + "/sinks/" + orgSink}); err != nil {
		t.Errorf("DeleteSink(org): %v", err)
	}
	orgBucket := localOrg + "/locations/global/buckets/b-" + suffix
	if _, err := cc.CreateBucket(ctx, &loggingpb.CreateBucketRequest{Parent: localOrg + "/locations/global", BucketId: "b-" + suffix}); err != nil {
		t.Errorf("CreateBucket(org): %v", err)
	} else if err := cc.DeleteBucket(ctx, &loggingpb.DeleteBucketRequest{Name: orgBucket}); err != nil {
		t.Errorf("DeleteBucket(org): %v", err)
	}
	before, err := cc.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: localOrg + "/settings"})
	if err != nil {
		t.Fatalf("GetSettings(org): %v", err)
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"storage_location"}}
	if st, err := cc.UpdateSettings(ctx, &loggingpb.UpdateSettingsRequest{Name: localOrg + "/settings", Settings: &loggingpb.Settings{StorageLocation: "europe-west1"}, UpdateMask: mask}); err != nil || st.GetStorageLocation() != "europe-west1" {
		t.Errorf("UpdateSettings(org) = %v, %v", st, err)
	}
	if st, err := cc.GetSettings(ctx, &loggingpb.GetSettingsRequest{Name: localOrg + "/settings"}); err != nil || st.GetStorageLocation() != "europe-west1" {
		t.Errorf("the organization's settings were not stored: %v, %v", st, err)
	}
	_, _ = cc.UpdateSettings(context.Background(), &loggingpb.UpdateSettingsRequest{Name: localOrg + "/settings",
		Settings: &loggingpb.Settings{StorageLocation: before.GetStorageLocation()}, UpdateMask: mask})

	// A billing account: stored, with no children.
	ba := "billingAccounts/0A0B0C-0D0E0F-" + fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	if _, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: ba, Sink: &loggingpb.LogSink{Name: "b", Destination: "storage.googleapis.com/some-bucket"}}); err != nil {
		t.Errorf("CreateSink(billing account): %v", err)
	} else if err := cc.DeleteSink(ctx, &loggingpb.DeleteSinkRequest{SinkName: ba + "/sinks/b"}); err != nil {
		t.Errorf("DeleteSink(billing account): %v", err)
	}
	_, err = cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: ba, Sink: &loggingpb.LogSink{Name: "c", Destination: "storage.googleapis.com/b", IncludeChildren: true}})
	wantLoggingCode(t, "include_children on a billing account sink", err, codes.InvalidArgument)

	_, err = cc.ListSinks(ctx, &loggingpb.ListSinksRequest{Parent: "folders/987654321987"}).Next()
	wantLoggingCode(t, "a folder that does not exist", err, codes.NotFound)
	mc, err := vlogging.NewMetricsClient(ctx, loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Close()
	_, err = mc.CreateLogMetric(ctx, &loggingpb.CreateLogMetricRequest{Parent: localOrg, Metric: &loggingpb.LogMetric{Name: "m", Filter: "severity>=ERROR"}})
	wantLoggingCode(t, "a metric under an organization", err, codes.InvalidArgument)
}

// covers: serviceusage.services.list
//
// TestServiceUsageFolderParentRefused (#1395): Service Usage v1 documents
// only projects/{project} as a parent, so services.list under a folder or the
// organization is 501 with that reason, through
// google.golang.org/api/serviceusage/v1.
func TestServiceUsageFolderParentRefused(t *testing.T) {
	h := New(t)
	c := serviceUsageClient(t, h)
	for _, parent := range []string{localOrg, "folders/123456789"} {
		_, err := c.Services.List(parent).Context(h.Context()).Do()
		var gerr *googleapi.Error
		if !errors.As(err, &gerr) || gerr.Code != 501 || !strings.Contains(gerr.Message, "documents only projects/{project}") {
			t.Errorf("services.list(%s) = %v; want 501 naming the documented parent", parent, err)
		}
	}
}
