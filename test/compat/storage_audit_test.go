//go:build compat

package compat

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/storage"
	si "cloud.google.com/go/storageinsights/apiv1"
	"cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	"google.golang.org/api/iterator"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// storageAuditEntries lists the entries of log logID ("data_access" or
// "activity") about bucket in project, as the official Logging client reads
// them.
func storageAuditEntries(t *testing.T, h *Harness, lc *vlogging.Client, project, logID, bucket string) []*loggingpb.LogEntry {
	t.Helper()
	filter := `logName="projects/` + project + `/logs/cloudaudit.googleapis.com%2F` + logID + `" AND resource.type="gcs_bucket" AND ` +
		`resource.labels.bucket_name="` + bucket + `"`
	it := lc.ListLogEntries(h.Context(), &loggingpb.ListLogEntriesRequest{ResourceNames: []string{"projects/" + project}, Filter: filter})
	var out []*loggingpb.LogEntry
	for {
		e, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("ListLogEntries %s: %v", filter, err)
		}
		out = append(out, e)
	}
}

// auditMethods is each entry's methodName and resourceName.
func auditMethods(t *testing.T, entries []*loggingpb.LogEntry) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, e := range entries {
		var al auditpb.AuditLog
		if err := e.GetProtoPayload().UnmarshalTo(&al); err != nil {
			t.Fatalf("protoPayload of %s: %v", e.GetInsertId(), err)
		}
		if al.GetServiceName() != "storage.googleapis.com" {
			t.Errorf("serviceName = %q", al.GetServiceName())
		}
		out[al.GetMethodName()+" "+al.GetResourceName()] = true
	}
	return out
}

// storageAuditFixture is a project of the test's own, a bucket in it with
// one object, the Logging client, and the project client.
func storageAuditFixture(t *testing.T, h *Harness, prefix string) (string, *storage.BucketHandle, *vlogging.Client) {
	t.Helper()
	h.Endpoint(EnvLogging)
	h.Endpoint(EnvResourceManager)
	sc := storageClient(t, h)
	project := iamProject(t, h, prefix)
	bucket := project + "-audit"
	bh := sc.Bucket(bucket)
	if err := bh.Create(h.Context(), project, &storage.BucketAttrs{Location: "US-CENTRAL1"}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(h.Context(), bh) })
	lc, err := vlogging.NewClient(h.Context(), loggingClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lc.Close() })
	return project, bh, lc
}

// readAndWrite writes, reads, lists and deletes objects of bh.
func readAndWrite(t *testing.T, h *Harness, bh *storage.BucketHandle) {
	t.Helper()
	putCompanionObject(t, h.Context(), bh, "dir/a.txt", "audited")
	r, err := bh.Object("dir/a.txt").NewReader(h.Context())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if b, _ := io.ReadAll(r); string(b) != "audited" {
		t.Errorf("read %q", b)
	}
	_ = r.Close()
	it := bh.Objects(h.Context(), nil)
	for {
		if _, err := it.Next(); err != nil {
			break
		}
	}
	if err := bh.Object("dir/a.txt").Delete(h.Context()); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// covers: google.logging.v2.LoggingServiceV2/ListLogEntries, google.cloud.resourcemanager.v3.Projects/SetIamPolicy
//
// TestStorageDataAccessAuditLogs (#1979): with DATA_READ and DATA_WRITE
// enabled for storage.googleapis.com in the project's IAM policy
// auditConfigs, the official storage client's object writes, reads, lists
// and deletes are Data Access audit log entries in
// projects/P/logs/cloudaudit.googleapis.com%2Fdata_access, on resource
// gcs_bucket, which the official Logging client reads; the bucket's
// creation is an Admin Activity entry, which is always written.
func TestStorageDataAccessAuditLogs(t *testing.T) {
	h := New(t)
	project, bh, lc := storageAuditFixture(t, h, "sda")
	rm := rmProjects(t, h)
	res := "projects/" + project
	if _, err := rm.SetIamPolicy(h.Context(), &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
		AuditConfigs: []*iampb.AuditConfig{{Service: "storage.googleapis.com", AuditLogConfigs: []*iampb.AuditLogConfig{
			{LogType: iampb.AuditLogConfig_DATA_READ}, {LogType: iampb.AuditLogConfig_DATA_WRITE}}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bindings", "etag", "audit_configs"}}}); err != nil {
		t.Fatalf("SetIamPolicy with audit configs: %v", err)
	}
	readAndWrite(t, h, bh)
	obj := "projects/_/buckets/" + bh.BucketName() + "/objects/dir/a.txt"
	want := []string{"storage.objects.insert " + obj, "storage.objects.get " + obj, "storage.objects.delete " + obj,
		"storage.objects.list projects/_/buckets/" + bh.BucketName()}
	var got map[string]bool
	var entries []*loggingpb.LogEntry
	deadline := time.Now().Add(30 * time.Second)
	for {
		entries = storageAuditEntries(t, h, lc, project, "data_access", bh.BucketName())
		got = auditMethods(t, entries)
		missing := false
		for _, w := range want {
			missing = missing || !got[w]
		}
		if !missing || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("no Data Access entry %q: %v", w, got)
		}
	}
	for _, e := range entries {
		l := e.GetResource().GetLabels()
		if l["project_id"] != project || l["location"] != "us-central1" {
			t.Errorf("resource labels = %v", l)
		}
		var al auditpb.AuditLog
		_ = e.GetProtoPayload().UnmarshalTo(&al)
		if len(al.GetAuthorizationInfo()) != 1 || !strings.HasPrefix(al.GetAuthorizationInfo()[0].GetPermission(), "storage.objects.") {
			t.Errorf("authorizationInfo = %v", al.GetAuthorizationInfo())
		}
	}
	// Admin Activity is always on: the bucket's creation.
	if got := auditMethods(t, storageAuditEntries(t, h, lc, project, "activity", bh.BucketName())); len(got) == 0 {
		t.Error("no Admin Activity entry for the bucket")
	}

	// DATA_READ alone: a read is logged, a write is not.
	if _, err := rm.SetIamPolicy(h.Context(), &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
		AuditConfigs: []*iampb.AuditConfig{{Service: "storage.googleapis.com", AuditLogConfigs: []*iampb.AuditLogConfig{
			{LogType: iampb.AuditLogConfig_DATA_READ}}}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"audit_configs"}}}); err != nil {
		t.Fatalf("SetIamPolicy DATA_READ: %v", err)
	}
	putCompanionObject(t, h.Context(), bh, "only-read.txt", "x")
	if _, err := bh.Object("only-read.txt").Attrs(h.Context()); err != nil {
		t.Fatal(err)
	}
	ro := "projects/_/buckets/" + bh.BucketName() + "/objects/only-read.txt"
	deadline = time.Now().Add(30 * time.Second)
	for {
		got = auditMethods(t, storageAuditEntries(t, h, lc, project, "data_access", bh.BucketName()))
		if got["storage.objects.get "+ro] || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if !got["storage.objects.get "+ro] || got["storage.objects.insert "+ro] {
		t.Errorf("with DATA_READ alone: %v", got)
	}
}

// covers: google.logging.v2.LoggingServiceV2/ListLogEntries
//
// TestStorageDataAccessAuditLogsOffByDefault (#1979): a project whose
// policy has no audit configs, Google's default, gets no Data Access entry
// for its object reads and writes, while the bucket's creation is still an
// Admin Activity entry.
func TestStorageDataAccessAuditLogsOffByDefault(t *testing.T) {
	h := New(t)
	project, bh, lc := storageAuditFixture(t, h, "sdo")
	readAndWrite(t, h, bh)
	// Admin Activity entries arrive by the same path; once the bucket's is
	// there, every earlier record has been handled.
	deadline := time.Now().Add(30 * time.Second)
	for len(storageAuditEntries(t, h, lc, project, "activity", bh.BucketName())) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	time.Sleep(3 * time.Second)
	if got := storageAuditEntries(t, h, lc, project, "data_access", bh.BucketName()); len(got) != 0 {
		t.Errorf("Data Access entries with auditing off: %v", auditMethods(t, got))
	}
	if len(storageAuditEntries(t, h, lc, project, "activity", bh.BucketName())) == 0 {
		t.Error("no Admin Activity entry for the bucket")
	}
}

// covers: storageinsights.projects.locations.datasetConfigs.create, storageinsights.projects.locations.datasetConfigs.linkDataset
//
// TestStorageInsightsActivityViews (#1979): requests made with the
// official storage client after a dataset configuration's first snapshot
// are counted in the next one: the official BigQuery client reads them in
// project_activity_view, bucket_activity_view (with its topPrefixes
// records), bucket_region_activity_view and object_events_view (the
// object's write and delete), and an events_view row of eventCode 3; a
// configuration with activityDataRetentionPeriodDays 0 has none.
func TestStorageInsightsActivityViews(t *testing.T) {
	h := New(t)
	project := insightsDatasetProject(t, h)
	sc := storageClient(t, h)
	c := insightsClient(t, h)
	bq, _ := bigqueryClient(t, h)
	setStorageIntelligenceOf(t, h, project, "STANDARD")
	t.Cleanup(func() { setStorageIntelligenceOf(t, h, project, "INHERIT") })
	pre := h.Project()
	bucket := pre + "-act"
	datasetBucket(t, h, sc, project, bucket, "US-CENTRAL1", map[string]string{"keep.txt": "kept"})
	attrs, err := sc.Bucket(bucket).Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	number := int64(attrs.ProjectNumber)
	parent := "projects/" + project + "/locations/us-central1"
	id := "act_" + strings.ReplaceAll(pre, "-", "_")
	dc := createDatasetConfig(t, h, c, parent, id, &storageinsightspb.DatasetConfig{
		SourceOptions: &storageinsightspb.DatasetConfig_SourceProjects_{SourceProjects: &storageinsightspb.DatasetConfig_SourceProjects{
			ProjectNumbers: []int64{number}}},
		CloudStorageBuckets: &storageinsightspb.DatasetConfig_IncludeCloudStorageBuckets{IncludeCloudStorageBuckets: &storageinsightspb.DatasetConfig_CloudStorageBuckets{
			CloudStorageBuckets: []*storageinsightspb.DatasetConfig_CloudStorageBuckets_CloudStorageBucket{{
				CloudStorageBucket: &storageinsightspb.DatasetConfig_CloudStorageBuckets_CloudStorageBucket_BucketName{BucketName: bucket}}}}},
		RetentionPeriodDays: 30,
		Identity:            &storageinsightspb.Identity{Type: storageinsightspb.Identity_IDENTITY_TYPE_PER_PROJECT},
	})
	lop, err := c.LinkDataset(h.Context(), &storageinsightspb.LinkDatasetRequest{Name: dc.Name})
	if err != nil {
		t.Fatalf("LinkDataset: %v", err)
	}
	if _, err := lop.Wait(h.Context()); err != nil {
		t.Fatalf("LinkDataset wait: %v", err)
	}
	dc, err = c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: dc.Name})
	if err != nil {
		t.Fatal(err)
	}
	_, ds, _ := strings.Cut(dc.GetLink().GetDataset(), ".")
	fq := "`" + project + "." + ds + "."

	bh := sc.Bucket(bucket)
	putCompanionObject(t, h.Context(), bh, "logs/a.txt", "activity!")
	for i := 0; i < 3; i++ {
		r, err := bh.Object("logs/a.txt").NewReader(h.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(r)
		_ = r.Close()
	}
	putCompanionObject(t, h.Context(), bh, "gone.txt", "x")
	if err := bh.Object("gone.txt").Delete(h.Context()); err != nil {
		t.Fatal(err)
	}
	storageJobsClock(t, h.Endpoint(EnvStorageInsights), 24*time.Hour+time.Minute)

	// The window since the first snapshot, which also counted the setup.
	last := func(view string) string {
		return " WHERE snapshotEndTime = (SELECT MAX(snapshotEndTime) FROM " + fq + view + "`)"
	}
	got := queryStrings(t, h, bq, "SELECT name, totalReads, totalWrites, totalDeletes, count2xx, totalRequestBytes FROM "+fq+
		"bucket_activity_view`"+last("bucket_activity_view"))
	if len(got) != 1 || !strings.HasPrefix(got[0], bucket+"|3|2|1|6|") {
		t.Errorf("bucket_activity_view = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT p.prefix, p.count FROM "+fq+"bucket_activity_view`, UNNEST(topPrefixesHighestOperations) AS p"+
		last("bucket_activity_view")); len(got) == 0 ||
		!strings.Contains(strings.Join(got, ","), "logs/|4") {
		t.Errorf("topPrefixesHighestOperations = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT project, totalReads >= 3, totalWrites >= 2 FROM "+fq+"project_activity_view`"+last("project_activity_view")); len(got) != 1 ||
		got[0] != i64s(number)+"|true|true" {
		t.Errorf("project_activity_view = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT objectName, requestOperation, responseStatus FROM "+fq+"object_events_view` WHERE objectName != 'keep.txt'"); strings.Join(got, ",") !=
		"gone.txt|delete|204,gone.txt|write|200,logs/a.txt|write|200" {
		t.Errorf("object_events_view = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT name, bucketLocation, requestBytes > 0 FROM "+fq+"bucket_region_activity_view`"+last("bucket_region_activity_view")); len(got) != 1 ||
		got[0] != bucket+"|US-CENTRAL1|true" {
		t.Errorf("bucket_region_activity_view = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT manifest.viewName FROM "+fq+"events_view` WHERE eventCode = '3' AND manifest.snapshotTime = "+
		"(SELECT MAX(manifest.snapshotTime) FROM "+fq+"events_view`)"); strings.Join(got, ",") !=
		"bucket_activity_view,bucket_region_activity_view,object_events_view,project_activity_view" {
		t.Errorf("events_view eventCode 3 = %q", got)
	}

	// activityDataRetentionPeriodDays 0 keeps no activity data.
	setActivityRetention(t, h, c, dc.Name, 0)
	if got := queryStrings(t, h, bq, "SELECT COUNT(*) FROM "+fq+"bucket_activity_view`"); len(got) != 1 || got[0] != "0" {
		t.Errorf("bucket_activity_view with no activity retention = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT COUNT(*) FROM "+fq+"events_view` WHERE eventCode = '3'"); len(got) != 1 || got[0] != "0" {
		t.Errorf("eventCode 3 rows with no activity retention = %q", got)
	}
}

// setActivityRetention patches activityDataRetentionPeriodDays, which the
// Go proto does not have, through the REST API.
func setActivityRetention(t *testing.T, h *Harness, _ *si.Client, name string, days int) {
	t.Helper()
	body := `{"activityDataRetentionPeriodDays":` + i64s(int64(days)) + `}`
	u := "http://" + stripScheme(h.Endpoint(EnvStorageInsights)) + "/v1/" + name + "?updateMask=activityDataRetentionPeriodDays"
	req, err := http.NewRequestWithContext(h.Context(), http.MethodPatch, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch activityDataRetentionPeriodDays: %d %s", resp.StatusCode, out)
	}
}
