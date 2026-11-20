//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	si "cloud.google.com/go/storageinsights/apiv1"
	"cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/date"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func insightsClient(t *testing.T, h *Harness) *si.Client {
	t.Helper()
	c, err := si.NewRESTClient(h.Context(), option.WithEndpoint("http://"+stripScheme(h.Endpoint(EnvStorageInsights))),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func civilDate(t time.Time) *date.Date {
	t = t.UTC()
	return &date.Date{Year: int32(t.Year()), Month: int32(t.Month()), Day: int32(t.Day())}
}

// inventoryConfig is a daily object-metadata report of src written under
// dst/path, from the instance clock's today for a week.
func inventoryConfig(now time.Time, src, dst, path string, fields []string, parquet bool) *storageinsightspb.ReportConfig {
	rc := &storageinsightspb.ReportConfig{
		FrequencyOptions: &storageinsightspb.FrequencyOptions{Frequency: storageinsightspb.FrequencyOptions_DAILY,
			StartDate: civilDate(now), EndDate: civilDate(now.Add(7 * 24 * time.Hour))},
		ReportKind: &storageinsightspb.ReportConfig_ObjectMetadataReportOptions{ObjectMetadataReportOptions: &storageinsightspb.ObjectMetadataReportOptions{
			MetadataFields: fields,
			Filter:         &storageinsightspb.ObjectMetadataReportOptions_StorageFilters{StorageFilters: &storageinsightspb.CloudStorageFilters{Bucket: src}},
			DestinationOptions: &storageinsightspb.ObjectMetadataReportOptions_StorageDestinationOptions{
				StorageDestinationOptions: &storageinsightspb.CloudStorageDestinationOptions{Bucket: dst, DestinationPath: path}},
		}},
		Labels: map[string]string{"team": "burrow"},
	}
	if parquet {
		rc.ReportFormat = &storageinsightspb.ReportConfig_ParquetOptions{ParquetOptions: &storageinsightspb.ParquetOptions{}}
	} else {
		rc.ReportFormat = &storageinsightspb.ReportConfig_CsvOptions{CsvOptions: &storageinsightspb.CSVOptions{
			Delimiter: ",", RecordSeparator: "\n", HeaderRequired: true}}
	}
	return rc
}

func reportDetails(t *testing.T, h *Harness, c *si.Client, config string) []*storageinsightspb.ReportDetail {
	t.Helper()
	var out []*storageinsightspb.ReportDetail
	it := c.ListReportDetails(h.Context(), &storageinsightspb.ListReportDetailsRequest{Parent: config})
	for {
		d, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("ListReportDetails: %v", err)
		}
		out = append(out, d)
	}
}

// waitReports waits (up to 30 s) until config has at least n reports.
func waitReports(t *testing.T, h *Harness, c *si.Client, config string, n int) []*storageinsightspb.ReportDetail {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ds := reportDetails(t, h, c, config)
		if len(ds) >= n {
			return ds
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s has %d reports after 30 s, want %d", config, len(ds), n)
		}
		time.Sleep(time.Second)
	}
}

func readCompanionObject(t *testing.T, h *Harness, bh *storage.BucketHandle, name string) []byte {
	t.Helper()
	r, err := bh.Object(name).NewReader(h.Context())
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func companionObjectNames(t *testing.T, h *Harness, bh *storage.BucketHandle, prefix string) []string {
	t.Helper()
	var out []string
	it := bh.Objects(h.Context(), &storage.Query{Prefix: prefix})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a.Name)
	}
}

// covers: storageinsights.projects.locations.reportConfigs.create, storageinsights.projects.locations.reportConfigs.get, storageinsights.projects.locations.reportConfigs.list, storageinsights.projects.locations.reportConfigs.patch, storageinsights.projects.locations.reportConfigs.delete, storageinsights.projects.locations.reportConfigs.reportDetails.list, storageinsights.projects.locations.reportConfigs.reportDetails.get
//
// TestStorageInsightsInventoryReports (#1957), with
// cloud.google.com/go/storageinsights/apiv1 (REST): a daily CSV inventory
// report configuration starting today writes today's report into the
// destination bucket (a shard with a header row and one row per object, and
// a manifest), recorded as a report detail; moving the instance clock a day
// writes the next. The configuration is listed, patched under a mask and
// deleted, and its reports stay.
func TestStorageInsightsInventoryReports(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := insightsClient(t, h)
	ctx := h.Context()
	src, dst := h.Project()+"-sis", h.Project()+"-sid"
	sb := companionBucket(t, h, sc, src, "")
	db := companionBucket(t, h, sc, dst, "")
	putCompanionObject(t, ctx, sb, "a.txt", "aaaa")
	putCompanionObject(t, ctx, sb, "b/c.txt", "cc")
	now := storageJobsClock(t, h.Endpoint(EnvStorageInsights), 0)
	parent := "projects/" + h.Project() + "/locations/us"
	rc, err := c.CreateReportConfig(ctx, &storageinsightspb.CreateReportConfigRequest{Parent: parent,
		ReportConfig: inventoryConfig(now, src, dst, "inv", []string{"project", "bucket", "name", "size"}, false)})
	if err != nil {
		t.Fatalf("CreateReportConfig: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteReportConfig(ctx, &storageinsightspb.DeleteReportConfigRequest{Name: rc.GetName()})
	})
	if !strings.HasPrefix(rc.GetName(), parent+"/reportConfigs/") || rc.GetCreateTime() == nil {
		t.Errorf("created %s, createTime %v", rc.GetName(), rc.GetCreateTime())
	}
	uuid := rc.GetName()[strings.LastIndex(rc.GetName(), "/")+1:]

	ds := waitReports(t, h, c, rc.GetName(), 1)
	d := ds[0]
	if d.GetStatus().GetCode() != 0 || d.GetReportMetrics().GetProcessedRecordsCount() != 2 || d.GetShardsCount() != 1 ||
		!strings.HasPrefix(d.GetReportPathPrefix(), "gs://"+dst+"/inv/"+uuid+"_") {
		t.Errorf("report detail: %+v", d)
	}
	if got, err := c.GetReportDetail(ctx, &storageinsightspb.GetReportDetailRequest{Name: d.GetName()}); err != nil || got.GetName() != d.GetName() {
		t.Errorf("GetReportDetail: %+v, %v", got, err)
	}
	names := companionObjectNames(t, h, db, "inv/"+uuid+"_")
	var shard, manifest string
	for _, n := range names {
		switch {
		case strings.HasSuffix(n, "_0.csv"):
			shard = n
		case strings.HasSuffix(n, "_manifest.json"):
			manifest = n
		}
	}
	if shard == "" || manifest == "" {
		t.Fatalf("report objects under inv/: %v; want a _0.csv shard and a _manifest.json", names)
	}
	lines := strings.Split(strings.TrimRight(string(readCompanionObject(t, h, db, shard)), "\n"), "\n")
	want := []string{"project,bucket,name,size", h.Project() + "," + src + ",a.txt,4", h.Project() + "," + src + ",b/c.txt,2"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n%s\nwant:\n%s", shard, strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	var m struct {
		RecordsProcessed int64    `json:"records_processed"`
		ShardCount       int64    `json:"shard_count"`
		Shards           []string `json:"report_shards_file_names"`
		TargetDatetime   any      `json:"target_datetime"`
	}
	if err := json.Unmarshal(readCompanionObject(t, h, db, manifest), &m); err != nil || m.RecordsProcessed != 2 || m.ShardCount != 1 ||
		len(m.Shards) != 1 || m.TargetDatetime == nil {
		t.Errorf("manifest %s: %+v, %v", manifest, m, err)
	}

	storageJobsClock(t, h.Endpoint(EnvStorageInsights), 24*time.Hour)
	if ds := reportDetails(t, h, c, rc.GetName()); len(ds) != 2 {
		t.Errorf("after a day: %d reports, want 2", len(ds))
	}

	rc.Labels = map[string]string{"team": "warren"}
	up, err := c.UpdateReportConfig(ctx, &storageinsightspb.UpdateReportConfigRequest{ReportConfig: rc,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil || up.GetLabels()["team"] != "warren" {
		t.Errorf("UpdateReportConfig: %+v, %v", up, err)
	}
	if got, err := c.GetReportConfig(ctx, &storageinsightspb.GetReportConfigRequest{Name: rc.GetName()}); err != nil ||
		got.GetLabels()["team"] != "warren" || got.GetObjectMetadataReportOptions().GetStorageDestinationOptions().GetBucket() != dst {
		t.Errorf("GetReportConfig after patch: %+v, %v", got, err)
	}
	found := false
	it := c.ListReportConfigs(ctx, &storageinsightspb.ListReportConfigsRequest{Parent: parent})
	for {
		l, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		found = found || l.GetName() == rc.GetName()
	}
	if !found {
		t.Errorf("ListReportConfigs lacks %s", rc.GetName())
	}
	if err := c.DeleteReportConfig(ctx, &storageinsightspb.DeleteReportConfigRequest{Name: rc.GetName()}); err != nil {
		t.Fatalf("DeleteReportConfig: %v", err)
	}
	if _, err := c.GetReportConfig(ctx, &storageinsightspb.GetReportConfigRequest{Name: rc.GetName()}); storageJobsCode(err) != codes.NotFound {
		t.Errorf("get after delete: %v, want NOT_FOUND", err)
	}
	if after := companionObjectNames(t, h, db, "inv/"+uuid+"_"); len(after) < len(names) {
		t.Errorf("deleting the configuration removed its reports: %v", after)
	}
}

// covers: storageinsights.projects.locations.reportConfigs.create, storageinsights.projects.locations.reportConfigs.reportDetails.list
//
// TestStorageInsightsParquetReport (#1957): a Parquet report configuration
// writes a Parquet shard (PAR1 at both ends) at the destination bucket's
// root.
func TestStorageInsightsParquetReport(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := insightsClient(t, h)
	ctx := h.Context()
	src := h.Project() + "-sip"
	sb := companionBucket(t, h, sc, src, "")
	putCompanionObject(t, ctx, sb, "one", "1")
	now := storageJobsClock(t, h.Endpoint(EnvStorageInsights), 0)
	rc, err := c.CreateReportConfig(ctx, &storageinsightspb.CreateReportConfigRequest{Parent: "projects/" + h.Project() + "/locations/us",
		ReportConfig: inventoryConfig(now, src, src, "", []string{"project", "bucket", "name", "size", "storageClass", "updated"}, true)})
	if err != nil {
		t.Fatalf("CreateReportConfig: %v", err)
	}
	t.Cleanup(func() {
		_ = c.DeleteReportConfig(ctx, &storageinsightspb.DeleteReportConfigRequest{Name: rc.GetName()})
	})
	waitReports(t, h, c, rc.GetName(), 1)
	uuid := rc.GetName()[strings.LastIndex(rc.GetName(), "/")+1:]
	var shard string
	for _, n := range companionObjectNames(t, h, sb, uuid+"_") {
		if strings.HasSuffix(n, "_0.parquet") {
			shard = n
		}
	}
	if shard == "" {
		t.Fatalf("no %s_*_0.parquet at the bucket root", uuid)
	}
	b := readCompanionObject(t, h, sb, shard)
	if len(b) < 12 || string(b[:4]) != "PAR1" || string(b[len(b)-4:]) != "PAR1" {
		t.Errorf("%s is not a Parquet file (%d bytes)", shard, len(b))
	}
}

// covers: storageinsights.projects.locations.reportConfigs.create
//
// TestStorageInsightsReportConfigChecks (#1957): a destination bucket in
// another location than the source, a source bucket that does not exist and
// metadata fields without the required name are refused; dataset
// configurations (BigQuery linked datasets) are not served.
func TestStorageInsightsReportConfigChecks(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := insightsClient(t, h)
	ctx := h.Context()
	src, eu := h.Project()+"-sic", h.Project()+"-sie"
	companionBucket(t, h, sc, src, "")
	companionBucket(t, h, sc, eu, "EUROPE-WEST1")
	now := storageJobsClock(t, h.Endpoint(EnvStorageInsights), 0)
	parent := "projects/" + h.Project() + "/locations/us"
	for _, tc := range []struct {
		what string
		rc   *storageinsightspb.ReportConfig
	}{
		{"a destination in another location", inventoryConfig(now, src, eu, "", []string{"project", "bucket", "name"}, false)},
		{"a source bucket that does not exist", inventoryConfig(now, src+"-none", src, "", []string{"project", "bucket", "name"}, false)},
		{"metadata fields without name", inventoryConfig(now, src, src, "", []string{"project", "bucket", "size"}, false)},
	} {
		rc, err := c.CreateReportConfig(ctx, &storageinsightspb.CreateReportConfigRequest{Parent: parent, ReportConfig: tc.rc})
		if err == nil {
			_ = c.DeleteReportConfig(ctx, &storageinsightspb.DeleteReportConfigRequest{Name: rc.GetName()})
		}
		if code := storageJobsCode(err); code != codes.InvalidArgument {
			t.Errorf("%s: %v (%v), want INVALID_ARGUMENT", tc.what, err, code)
		}
	}
	_, err := c.CreateDatasetConfig(ctx, &storageinsightspb.CreateDatasetConfigRequest{Parent: parent, DatasetConfigId: "ds",
		DatasetConfig: &storageinsightspb.DatasetConfig{}})
	if storageJobsCode(err) != codes.Unimplemented {
		t.Errorf("CreateDatasetConfig: %v, want UNIMPLEMENTED", err)
	}
}
