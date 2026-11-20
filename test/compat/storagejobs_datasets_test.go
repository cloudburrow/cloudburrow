//go:build compat

package compat

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/storage"
	si "cloud.google.com/go/storageinsights/apiv1"
	"cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// insightsDatasetProject is the project a dataset configuration test
// works in: BigQuery's, as the linked dataset is made in the
// configuration's project and the emulator serves one project.
func insightsDatasetProject(t *testing.T, h *Harness) string {
	t.Helper()
	h.Endpoint(EnvStorageInsights)
	h.Endpoint(EnvBigQuery)
	p := strings.TrimSpace(os.Getenv(EnvBigQueryProject))
	if p == "" {
		t.Skipf("%s is not set: a dataset configuration links a dataset in the BigQuery project", EnvBigQueryProject)
	}
	return p
}

// datasetBucket creates a bucket of project in loc with objects.
func datasetBucket(t *testing.T, h *Harness, sc *storage.Client, project, name, loc string, objects map[string]string) {
	t.Helper()
	bh := sc.Bucket(name)
	if err := bh.Create(h.Context(), project, &storage.BucketAttrs{Location: loc, Labels: map[string]string{"team": "burrow"}}); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	t.Cleanup(func() { emptyAndDelete(h.Context(), bh) })
	for n, body := range objects {
		putCompanionObject(t, h.Context(), bh, n, body)
	}
}

func createDatasetConfig(t *testing.T, h *Harness, c *si.Client, parent, id string, dc *storageinsightspb.DatasetConfig) *storageinsightspb.DatasetConfig {
	t.Helper()
	op, err := c.CreateDatasetConfig(h.Context(), &storageinsightspb.CreateDatasetConfigRequest{Parent: parent, DatasetConfigId: id,
		DatasetConfig: dc})
	if err != nil {
		t.Fatalf("CreateDatasetConfig: %v", err)
	}
	got, err := op.Wait(h.Context())
	if err != nil {
		t.Fatalf("CreateDatasetConfig wait: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.UnlinkDataset(h.Context(), &storageinsightspb.UnlinkDatasetRequest{Name: got.Name}); err == nil {
			_ = op.Wait(h.Context())
		}
		if op, err := c.DeleteDatasetConfig(h.Context(), &storageinsightspb.DeleteDatasetConfigRequest{Name: got.Name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	return got
}

// queryStrings runs sql and returns each row's columns joined by "|".
func queryStrings(t *testing.T, h *Harness, bq *bigquery.Client, sql string) []string {
	t.Helper()
	it, err := bq.Query(sql).Read(h.Context())
	if err != nil {
		t.Fatalf("query %s: %v", sql, err)
	}
	var out []string
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("query %s: %v", sql, err)
		}
		var cols []string
		for _, v := range row {
			cols = append(cols, strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(fmtValue(v)), "\n", " ")))
		}
		out = append(out, strings.Join(cols, "|"))
	}
	sort.Strings(out)
	return out
}

func fmtValue(v bigquery.Value) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case string:
		return x
	case int64:
		return i64s(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	}
	return "?"
}

func i64s(n int64) string { return strconv.FormatInt(n, 10) }

// covers: storageinsights.projects.locations.datasetConfigs.create, storageinsights.projects.locations.datasetConfigs.get, storageinsights.projects.locations.datasetConfigs.list, storageinsights.projects.locations.datasetConfigs.patch, storageinsights.projects.locations.datasetConfigs.delete, storageinsights.projects.locations.datasetConfigs.linkDataset, storageinsights.projects.locations.datasetConfigs.unlinkDataset, storageinsights.projects.locations.operations.get, storageinsights.projects.locations.operations.list, storageinsights.projects.locations.operations.cancel, storageinsights.projects.locations.operations.delete
//
// TestStorageInsightsLinkedDataset (#1976): a dataset configuration over
// the project's buckets named by a regex, excluding one location, links a
// dataset that the official BigQuery client queries: its views list the
// local buckets' objects and buckets with Google's columns, the tables
// they read are hidden, writes are refused, a day on the instance clock
// adds a snapshot, a 1-day retention keeps only the latest, a linked
// configuration cannot be deleted, and unlink removes the dataset.
func TestStorageInsightsLinkedDataset(t *testing.T) {
	h := New(t)
	project := insightsDatasetProject(t, h)
	sc := storageClient(t, h)
	c := insightsClient(t, h)
	bq, _ := bigqueryClient(t, h)
	setStorageIntelligenceOf(t, h, project, "STANDARD")
	t.Cleanup(func() { setStorageIntelligenceOf(t, h, project, "INHERIT") })
	pre := h.Project()
	datasetBucket(t, h, sc, project, pre+"-dsa", "US-CENTRAL1", map[string]string{"a.txt": "alpha", "dir/b.txt": "bravo!"})
	datasetBucket(t, h, sc, project, pre+"-dsb", "US-CENTRAL1", map[string]string{"c.txt": "charlie"})
	datasetBucket(t, h, sc, project, pre+"-dsx", "EU", map[string]string{"x.txt": "excluded"})
	attrs, err := sc.Bucket(pre + "-dsa").Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	number := int64(attrs.ProjectNumber)
	parent := "projects/" + project + "/locations/us-central1"
	id := "cfg_" + strings.ReplaceAll(pre, "-", "_")
	dc := createDatasetConfig(t, h, c, parent, id, &storageinsightspb.DatasetConfig{
		SourceOptions: &storageinsightspb.DatasetConfig_SourceProjects_{SourceProjects: &storageinsightspb.DatasetConfig_SourceProjects{
			ProjectNumbers: []int64{number}}},
		CloudStorageLocations: &storageinsightspb.DatasetConfig_ExcludeCloudStorageLocations{ExcludeCloudStorageLocations: &storageinsightspb.DatasetConfig_CloudStorageLocations{
			Locations: []string{"eu"}}},
		CloudStorageBuckets: &storageinsightspb.DatasetConfig_IncludeCloudStorageBuckets{IncludeCloudStorageBuckets: &storageinsightspb.DatasetConfig_CloudStorageBuckets{
			CloudStorageBuckets: []*storageinsightspb.DatasetConfig_CloudStorageBuckets_CloudStorageBucket{{
				CloudStorageBucket: &storageinsightspb.DatasetConfig_CloudStorageBuckets_CloudStorageBucket_BucketPrefixRegex{BucketPrefixRegex: pre + "-ds"}}}}},
		RetentionPeriodDays: 30,
		Description:         "compat",
		Identity:            &storageinsightspb.Identity{Type: storageinsightspb.Identity_IDENTITY_TYPE_PER_PROJECT},
	})
	if dc.GetUid() == "" || dc.GetDatasetConfigState() != storageinsightspb.DatasetConfig_CONFIG_STATE_ACTIVE ||
		dc.GetIdentity().GetName() != "service-"+i64s(number)+"@gcp-sa-storageinsights.iam.gserviceaccount.com" {
		t.Errorf("created %+v", dc)
	}
	if got, err := c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: dc.Name}); err != nil || got.GetUid() != dc.GetUid() {
		t.Errorf("GetDatasetConfig: %v %v", got, err)
	}
	found := false
	it := c.ListDatasetConfigs(h.Context(), &storageinsightspb.ListDatasetConfigsRequest{Parent: parent})
	for {
		x, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListDatasetConfigs: %v", err)
		}
		found = found || x.Name == dc.Name
	}
	if !found {
		t.Errorf("ListDatasetConfigs lacks %s", dc.Name)
	}

	lop, err := c.LinkDataset(h.Context(), &storageinsightspb.LinkDatasetRequest{Name: dc.Name})
	if err != nil {
		t.Fatalf("LinkDataset: %v", err)
	}
	if _, err := lop.Wait(h.Context()); err != nil {
		t.Fatalf("LinkDataset wait: %v", err)
	}
	dc, err = c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: dc.Name})
	if err != nil || !dc.GetLink().GetLinked() {
		t.Fatalf("after link: %+v %v", dc, err)
	}
	_, ds, _ := strings.Cut(dc.GetLink().GetDataset(), ".")
	if !strings.HasPrefix(ds, id) {
		t.Errorf("linked dataset %q is not named after %s", dc.GetLink().GetDataset(), id)
	}
	fq := "`" + project + "." + ds + "."

	objs := queryStrings(t, h, bq, "SELECT bucket, name, size, project FROM "+fq+"object_attributes_latest_snapshot_view`")
	want := []string{pre + "-dsa|a.txt|5|" + i64s(number), pre + "-dsa|dir/b.txt|6|" + i64s(number), pre + "-dsb|c.txt|7|" + i64s(number)}
	if strings.Join(objs, ",") != strings.Join(want, ",") {
		t.Errorf("object_attributes_latest_snapshot_view = %q, want %q", objs, want)
	}
	buckets := queryStrings(t, h, bq, "SELECT name, objectCount, totalSize, versioning FROM "+fq+"bucket_attributes_view`")
	if strings.Join(buckets, ",") != pre+"-dsa|2|11|false,"+pre+"-dsb|1|7|false" {
		t.Errorf("bucket_attributes_view = %q", buckets)
	}
	if got := queryStrings(t, h, bq, "SELECT id, number FROM "+fq+"project_attributes_view`"); len(got) != 1 || got[0] != project+"|"+i64s(number) {
		t.Errorf("project_attributes_view = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT eventCode FROM "+fq+"events_view` WHERE eventCode = '2'"); len(got) != 1 {
		t.Errorf("events_view eventCode 2 rows = %q", got)
	}
	// The views have Google's columns.
	md, err := bq.Dataset(ds).Table("object_attributes_view").Metadata(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]bool{}
	for _, f := range md.Schema {
		cols[f.Name] = true
	}
	for _, f := range []string{"snapshotTime", "bucket", "name", "size", "crc32c", "md5Hash", "metadata", "contexts", "encryptionType", "softDeleteTime"} {
		if !cols[f] {
			t.Errorf("object_attributes_view has no %s column: %v", f, cols)
		}
	}
	// Only the views are listed.
	var tables []string
	tit := bq.Dataset(ds).Tables(h.Context())
	for {
		tb, err := tit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		tables = append(tables, tb.TableID)
	}
	sort.Strings(tables)
	for _, v := range []string{"bucket_attributes_latest_snapshot_view", "bucket_attributes_view", "error_attributes_view",
		"events_view", "object_attributes_latest_snapshot_view", "object_attributes_view", "project_attributes_view"} {
		if sort.SearchStrings(tables, v) == len(tables) || tables[sort.SearchStrings(tables, v)] != v {
			t.Errorf("tables.list lacks %s: %v", v, tables)
		}
	}
	for _, tb := range tables {
		if !strings.HasSuffix(tb, "_view") {
			t.Errorf("tables.list shows %s, which is not a view", tb)
		}
	}
	// The dataset is read-only.
	if err := bq.Dataset(ds).Table("mine").Create(h.Context(), &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "a", Type: bigquery.StringFieldType}}}); err == nil {
		t.Error("a table was created in the linked dataset")
	}

	// A day later a second snapshot is taken, with the new object.
	putCompanionObject(t, h.Context(), sc.Bucket(pre+"-dsb"), "d.txt", "delta")
	storageJobsClock(t, h.Endpoint(EnvStorageInsights), 24*time.Hour+time.Minute)
	if got := queryStrings(t, h, bq, "SELECT COUNT(DISTINCT snapshotTime) FROM "+fq+"bucket_attributes_view`"); len(got) != 1 || got[0] != "2" {
		t.Errorf("snapshots after a day = %q", got)
	}
	if got := queryStrings(t, h, bq, "SELECT name FROM "+fq+"object_attributes_latest_snapshot_view` WHERE bucket = '"+pre+"-dsb'"); strings.Join(got, ",") != "c.txt,d.txt" {
		t.Errorf("latest snapshot of %s-dsb = %q", pre, got)
	}

	// A retention of one day keeps only the latest snapshot.
	uop, err := c.UpdateDatasetConfig(h.Context(), &storageinsightspb.UpdateDatasetConfigRequest{
		DatasetConfig: &storageinsightspb.DatasetConfig{Name: dc.Name, RetentionPeriodDays: 1},
		UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"retention_period_days"}}})
	if err != nil {
		t.Fatalf("UpdateDatasetConfig: %v", err)
	}
	if got, err := uop.Wait(h.Context()); err != nil || got.GetRetentionPeriodDays() != 1 {
		t.Fatalf("UpdateDatasetConfig wait: %v %v", got, err)
	}
	if got := queryStrings(t, h, bq, "SELECT COUNT(DISTINCT snapshotTime) FROM "+fq+"bucket_attributes_view`"); len(got) != 1 || got[0] != "1" {
		t.Errorf("snapshots with a 1-day retention = %q", got)
	}

	// A linked configuration cannot be deleted.
	if _, err := c.DeleteDatasetConfig(h.Context(), &storageinsightspb.DeleteDatasetConfigRequest{Name: dc.Name}); storageJobsCode(err) != codes.FailedPrecondition {
		t.Errorf("delete while linked: %v", err)
	}
	unop, err := c.UnlinkDataset(h.Context(), &storageinsightspb.UnlinkDatasetRequest{Name: dc.Name})
	if err != nil {
		t.Fatalf("UnlinkDataset: %v", err)
	}
	if err := unop.Wait(h.Context()); err != nil {
		t.Fatalf("UnlinkDataset wait: %v", err)
	}
	if _, err := bq.Dataset(ds).Metadata(h.Context()); err == nil {
		t.Error("the linked dataset is still there after unlink")
	}

	// Operations.
	var opNames []string
	oit := c.ListOperations(h.Context(), &longrunningpb.ListOperationsRequest{Name: parent})
	for {
		op, err := oit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListOperations: %v", err)
		}
		if strings.Contains(op.GetName(), "/operations/") {
			opNames = append(opNames, op.GetName())
		}
	}
	if len(opNames) < 4 {
		t.Fatalf("ListOperations = %v", opNames)
	}
	if op, err := c.GetOperation(h.Context(), &longrunningpb.GetOperationRequest{Name: opNames[0]}); err != nil || !op.GetDone() {
		t.Errorf("GetOperation: %v %v", op, err)
	}
	if err := c.CancelOperation(h.Context(), &longrunningpb.CancelOperationRequest{Name: opNames[0]}); err != nil {
		t.Errorf("CancelOperation: %v", err)
	}
	if err := c.DeleteOperation(h.Context(), &longrunningpb.DeleteOperationRequest{Name: opNames[0]}); err != nil {
		t.Errorf("DeleteOperation: %v", err)
	}
	if _, err := c.GetOperation(h.Context(), &longrunningpb.GetOperationRequest{Name: opNames[0]}); storageJobsCode(err) != codes.NotFound {
		t.Errorf("GetOperation after delete: %v", err)
	}

	dop, err := c.DeleteDatasetConfig(h.Context(), &storageinsightspb.DeleteDatasetConfigRequest{Name: dc.Name})
	if err != nil {
		t.Fatalf("DeleteDatasetConfig: %v", err)
	}
	if err := dop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteDatasetConfig wait: %v", err)
	}
	if _, err := c.GetDatasetConfig(h.Context(), &storageinsightspb.GetDatasetConfigRequest{Name: dc.Name}); storageJobsCode(err) != codes.NotFound {
		t.Errorf("get after delete: %v", err)
	}
}

// TestStorageInsightsDatasetConfigChecks (#1976): Google's validation of a
// dataset configuration, and Storage Intelligence required.
func TestStorageInsightsDatasetConfigChecks(t *testing.T) {
	h := New(t)
	project := insightsDatasetProject(t, h)
	c := insightsClient(t, h)
	src := &storageinsightspb.DatasetConfig_SourceProjects_{SourceProjects: &storageinsightspb.DatasetConfig_SourceProjects{ProjectNumbers: []int64{1}}}
	create := func(loc, id string, dc *storageinsightspb.DatasetConfig) error {
		op, err := c.CreateDatasetConfig(h.Context(), &storageinsightspb.CreateDatasetConfigRequest{
			Parent: "projects/" + project + "/locations/" + loc, DatasetConfigId: id, DatasetConfig: dc})
		if err != nil {
			return err
		}
		got, err := op.Wait(h.Context())
		if err == nil {
			if dop, err := c.DeleteDatasetConfig(h.Context(), &storageinsightspb.DeleteDatasetConfigRequest{Name: got.Name}); err == nil {
				_ = dop.Wait(h.Context())
			}
		}
		return err
	}
	setStorageIntelligenceOf(t, h, project, "DISABLED")
	t.Cleanup(func() { setStorageIntelligenceOf(t, h, project, "INHERIT") })
	id := "chk_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := create("us-central1", id, &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 7}); storageJobsCode(err) != codes.FailedPrecondition ||
		!strings.Contains(err.Error(), "Storage Intelligence") {
		t.Errorf("without Storage Intelligence: %v", err)
	}
	setStorageIntelligenceOf(t, h, project, "STANDARD")
	for _, tc := range []struct {
		name, loc, id string
		dc            *storageinsightspb.DatasetConfig
	}{
		{"hyphen in the ID", "us-central1", "bad-id", &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 7}},
		{"ID starting with a digit", "us-central1", "1abc", &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 7}},
		{"unsupported location", "us-west1", id, &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 7}},
		{"no scope", "us-central1", id, &storageinsightspb.DatasetConfig{RetentionPeriodDays: 7}},
		{"retention 0", "us-central1", id, &storageinsightspb.DatasetConfig{SourceOptions: src}},
		{"retention 91", "us-central1", id, &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 91}},
		{"organization scope without organization", "us", id, &storageinsightspb.DatasetConfig{
			SourceOptions: &storageinsightspb.DatasetConfig_OrganizationScope{OrganizationScope: true}, RetentionPeriodDays: 7}},
		{"bad bucket regex", "us", id, &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 7,
			CloudStorageBuckets: &storageinsightspb.DatasetConfig_IncludeCloudStorageBuckets{IncludeCloudStorageBuckets: &storageinsightspb.DatasetConfig_CloudStorageBuckets{
				CloudStorageBuckets: []*storageinsightspb.DatasetConfig_CloudStorageBuckets_CloudStorageBucket{{
					CloudStorageBucket: &storageinsightspb.DatasetConfig_CloudStorageBuckets_CloudStorageBucket_BucketPrefixRegex{BucketPrefixRegex: "("}}}}}}},
		{"description over 256", "us", id, &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 7,
			Description: strings.Repeat("d", 257)}},
	} {
		if err := create(tc.loc, tc.id, tc.dc); storageJobsCode(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", tc.name, err)
		}
	}
	// A valid one is made, a second with its ID is refused, and an update
	// of an output-only field is refused.
	dc := createDatasetConfig(t, h, c, "projects/"+project+"/locations/us", id, &storageinsightspb.DatasetConfig{SourceOptions: src,
		RetentionPeriodDays: 7})
	if dc.GetIdentity().GetType() != storageinsightspb.Identity_IDENTITY_TYPE_PER_CONFIG || !strings.HasSuffix(dc.GetIdentity().GetName(), "@gcp-sa-storageinsights.iam.gserviceaccount.com") {
		t.Errorf("default identity: %+v", dc.GetIdentity())
	}
	if err := create("us", id, &storageinsightspb.DatasetConfig{SourceOptions: src, RetentionPeriodDays: 7}); storageJobsCode(err) != codes.AlreadyExists {
		t.Errorf("a second %s: %v", id, err)
	}
	if _, err := c.UpdateDatasetConfig(h.Context(), &storageinsightspb.UpdateDatasetConfigRequest{
		DatasetConfig: &storageinsightspb.DatasetConfig{Name: dc.Name, Uid: "x"},
		UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"uid"}}}); storageJobsCode(err) != codes.InvalidArgument {
		t.Errorf("update of uid: %v", err)
	}
	if _, err := c.UnlinkDataset(h.Context(), &storageinsightspb.UnlinkDatasetRequest{Name: dc.Name}); storageJobsCode(err) != codes.FailedPrecondition {
		t.Errorf("unlink of an unlinked configuration: %v", err)
	}
}
