//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	control "cloud.google.com/go/storage/control/apiv2"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	insights "cloud.google.com/go/storageinsights/apiv1"
	"cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	sbo "google.golang.org/api/storagebatchoperations/v1"
	stv1 "google.golang.org/api/storagetransfer/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The Cloud Storage companion APIs' endpoints (#1957), as the harness
// exports them.
const (
	envStorageBatch    = "CLOUDBURROW_TEST_STORAGE_BATCH"
	envStorageInsights = "CLOUDBURROW_TEST_STORAGE_INSIGHTS"
	envStorageTransfer = "CLOUDBURROW_TEST_STORAGE_TRANSFER"
)

// writeObjects writes each named object, with its name as its content.
func writeObjects(t *testing.T, c *storage.Client, bucket string, names ...string) {
	t.Helper()
	for _, n := range names {
		w := c.Bucket(bucket).Object(n).NewWriter(context.Background())
		if _, err := w.Write([]byte(n)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write gs://%s/%s: %v", bucket, n, err)
		}
	}
}

// enableIntelligence sets the project's Storage Intelligence to STANDARD,
// which a batch operations job needs, and back to INHERIT afterwards.
func enableIntelligence(t *testing.T, project string) {
	t.Helper()
	ep := strings.TrimRight(strings.TrimSpace(os.Getenv(envStorage)), "/")
	if !strings.Contains(ep, "://") {
		ep = "http://" + ep
	}
	ctx := context.Background()
	c, err := control.NewStorageControlRESTClient(ctx, option.WithEndpoint(ep), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	name := "projects/" + project + "/locations/global/intelligenceConfig"
	set := func(e controlpb.IntelligenceConfig_EditionConfig) error {
		_, err := c.UpdateProjectIntelligenceConfig(context.Background(), &controlpb.UpdateProjectIntelligenceConfigRequest{
			IntelligenceConfig: &controlpb.IntelligenceConfig{Name: name, EditionConfig: e},
			UpdateMask:         &fieldmaskpb.FieldMask{Paths: []string{"edition_config"}}})
		return err
	}
	if err := set(controlpb.IntelligenceConfig_STANDARD); err != nil {
		t.Fatalf("enable Storage Intelligence: %v", err)
	}
	t.Cleanup(func() { _ = set(controlpb.IntelligenceConfig_INHERIT) })
}

// eventually polls cond for up to a minute.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !cond(); time.Sleep(300 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestStorageBatchOperationsThroughThePage (#1957), in the storage shard.
// Create job on Cloud Storage > Batch operations, over the prefix tmp/ with
// Update object holds setting the temporary hold, makes a job the official
// Storage Batch Operations client reads SUCCEEDED with two objects done;
// the objects under tmp/ are held and keep/c is not, as the official Cloud
// Storage client reads them. The job's page lists its bucket operation;
// Delete from its row, confirmed by its name, deletes it.
func TestStorageBatchOperationsThroughThePage(t *testing.T) {
	needService(t, "storage-batch-operations")
	c, err := sbo.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envStorageBatch)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	gcs, _ := storageSDK(t)
	project := uniqueProject(t)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	bucket, id := "cb-batch-"+suffix, "browser-hold-"+suffix
	regionalBucket(t, gcs, project, bucket)
	writeObjects(t, gcs, bucket, "tmp/a", "tmp/b", "keep/c")
	t.Cleanup(func() {
		for _, n := range []string{"tmp/a", "tmp/b", "keep/c"} {
			o := gcs.Bucket(bucket).Object(n)
			_, _ = o.Update(context.Background(), storage.ObjectAttrsToUpdate{TemporaryHold: false})
			_ = o.Delete(context.Background())
		}
	})
	enableIntelligence(t, project)
	name := "projects/" + project + "/locations/global/jobs/" + id

	p := open(t)
	p.navigate("/storage/batch-operations/create?project=" + project)
	p.submitForm(map[string]string{"jobId": id, "bucket": "gs://" + bucket + "/", "source": "prefixes", "prefixes": "tmp/",
		"operation": "hold", "temporaryHold": "SET"}, "jobId")
	var job *sbo.Job
	eventually(t, "the job to succeed", func() bool {
		job, err = c.Projects.Locations.Jobs.Get(name).Do()
		return err == nil && job.State == "SUCCEEDED"
	})
	if job.Counters == nil || job.Counters.SucceededObjectCount != 2 || job.PutObjectHold == nil || job.PutObjectHold.TemporaryHold != "SET" {
		t.Errorf("the job made on the page = %+v; want a temporary hold set on 2 objects", job)
	}
	for n, want := range map[string]bool{"tmp/a": true, "tmp/b": true, "keep/c": false} {
		a, err := gcs.Bucket(bucket).Object(n).Attrs(context.Background())
		if err != nil || a.TemporaryHold != want {
			t.Errorf("gs://%s/%s temporary hold = %v (%v), want %v", bucket, n, a != nil && a.TemporaryHold, err, want)
		}
	}

	p.navigate("/storage/batch-operations/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("SUCCEEDED")`)
	p.clickText("#view [role=tab]", "Bucket operations")
	p.waitFor(fmt.Sprintf(`document.querySelector("#detail-panel").textContent.includes(%q)`, bucket))

	p.navigate("/storage/batch-operations?project=" + project)
	p.openRowDelete(id, "Delete")
	p.confirmByName(id)
	eventually(t, "the job to be deleted", func() bool {
		_, err := c.Projects.Locations.Jobs.Get(name).Do()
		return err != nil
	})
}

// TestStorageInventoryReportsThroughThePage (#1957), in the storage shard.
// Create report configuration on Cloud Storage > Inventory reports, from a
// bucket to a second one in its location, from today to tomorrow, makes a
// configuration the official Storage Insights client reads; its first
// report is written into the destination's inv/ folder with its manifest,
// and the configuration's Report history lists it. Advance clock
// (CloudBurrow extension) by a day writes the second. Delete from the row,
// confirmed, deletes the configuration.
func TestStorageInventoryReportsThroughThePage(t *testing.T) {
	needService(t, "storage-inventory-reports")
	ctx := context.Background()
	c, err := insights.NewRESTClient(ctx, option.WithEndpoint("http://"+endpoint(t, envStorageInsights)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	gcs, _ := storageSDK(t)
	project := uniqueProject(t)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	src, dst := "cb-inv-src-"+suffix, "cb-inv-dst-"+suffix
	regionalBucket(t, gcs, project, src)
	regionalBucket(t, gcs, project, dst)
	writeObjects(t, gcs, src, "one", "two")
	t.Cleanup(func() {
		it := gcs.Bucket(dst).Objects(context.Background(), nil)
		for a, err := it.Next(); err == nil; a, err = it.Next() {
			_ = gcs.Bucket(dst).Object(a.Name).Delete(context.Background())
		}
		_ = gcs.Bucket(src).Object("one").Delete(context.Background())
		_ = gcs.Bucket(src).Object("two").Delete(context.Background())
	})
	parent := "projects/" + project + "/locations/us-central1"
	now := time.Now().UTC()

	p := open(t)
	p.navigate("/storage/inventory-reports/create?project=" + project)
	p.submitForm(map[string]string{"displayName": "browser-" + suffix, "sourceBucket": "gs://" + src + "/",
		"destinationBucket": "gs://" + dst + "/", "destinationPath": "inv", "startDate": now.Format("2006-01-02"),
		"endDate": now.AddDate(0, 0, 1).Format("2006-01-02")}, "displayName")
	var cfg *storageinsightspb.ReportConfig
	eventually(t, "the configuration", func() bool {
		it := c.ListReportConfigs(ctx, &storageinsightspb.ListReportConfigsRequest{Parent: parent})
		for r, err := it.Next(); err == nil; r, err = it.Next() {
			if r.GetDisplayName() == "browser-"+suffix {
				cfg = r
			}
		}
		return cfg != nil
	})
	if cfg.GetObjectMetadataReportOptions().GetStorageDestinationOptions().GetBucket() != dst || cfg.GetCsvOptions() == nil {
		t.Errorf("the configuration made on the page = %v; want CSV reports into %s", cfg, dst)
	}
	details := func() int {
		n := 0
		it := c.ListReportDetails(ctx, &storageinsightspb.ListReportDetailsRequest{Parent: cfg.GetName()})
		for _, err := it.Next(); err != iterator.Done; _, err = it.Next() {
			if err != nil {
				return 0
			}
			n++
		}
		return n
	}
	eventually(t, "the first report", func() bool { return details() >= 1 })
	manifest := false
	it := gcs.Bucket(dst).Objects(ctx, &storage.Query{Prefix: "inv/"})
	for a, err := it.Next(); err == nil; a, err = it.Next() {
		manifest = manifest || strings.HasSuffix(a.Name, "_manifest.json")
	}
	if !manifest {
		t.Errorf("gs://%s/inv/ holds no report manifest", dst)
	}
	id := cfg.GetName()[strings.LastIndex(cfg.GetName(), "/")+1:]
	p.navigate("/storage/inventory-reports/" + id + "?project=" + project)
	p.clickText("#view [role=tab]", "Report history")
	p.waitFor(`document.querySelector("#detail-panel").textContent.includes("Succeeded")`)

	p.navigate("/storage/inventory-reports?project=" + project)
	if rows := p.listAction("advance-storage-clock", map[string]string{"duration": "1d"}); rows == "" {
		t.Error("Advance clock answered nothing")
	}
	eventually(t, "the second report", func() bool { return details() >= 2 })

	p.navigate("/storage/inventory-reports?project=" + project)
	p.openRowDelete(id, "Delete")
	p.confirmByName(id)
	eventually(t, "the configuration to be deleted", func() bool {
		_, err := c.GetReportConfig(ctx, &storageinsightspb.GetReportConfigRequest{Name: cfg.GetName()})
		return err != nil
	})
}

// TestStorageTransferThroughThePage (#1957), in the storage shard. Create
// transfer job on Storage Transfer Service > Transfer jobs, between two
// buckets on demand with the prefix a/, makes a job the official Storage
// Transfer client reads; Start a run on its page copies a/1 and a/2 and not
// b/3, the run's counters say so; Disable job disables it. Create pool on
// Agent pools makes a pool with its bandwidth limit. Delete from the job's
// row, confirmed, deletes the job.
func TestStorageTransferThroughThePage(t *testing.T) {
	needService(t, "storage-transfer")
	c, err := stv1.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envStorageTransfer)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	gcs, _ := storageSDK(t)
	ctx := context.Background()
	project := uniqueProject(t)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	src, dst := "cb-sts-src-"+suffix, "cb-sts-dst-"+suffix
	regionalBucket(t, gcs, project, src)
	regionalBucket(t, gcs, project, dst)
	writeObjects(t, gcs, src, "a/1", "a/2", "b/3")
	t.Cleanup(func() {
		for _, b := range []string{src, dst} {
			for _, n := range []string{"a/1", "a/2", "b/3"} {
				_ = gcs.Bucket(b).Object(n).Delete(context.Background())
			}
		}
	})
	filter := fmt.Sprintf(`{"projectId":%q}`, project)

	p := open(t)
	p.navigate("/transfer/jobs/create?project=" + project)
	p.submitForm(map[string]string{"sourceType": "gcs", "sourceBucket": "gs://" + src + "/", "sinkType": "gcs",
		"sinkBucket": "gs://" + dst + "/", "when": "none", "includePrefixes": "a/", "description": "browser-" + suffix}, "sourceBucket")
	var job *stv1.TransferJob
	eventually(t, "the transfer job", func() bool {
		l, err := c.TransferJobs.List(filter).Do()
		if err != nil {
			return false
		}
		for _, j := range l.TransferJobs {
			if j.Description == "browser-"+suffix {
				job = j
			}
		}
		return job != nil
	})
	t.Cleanup(func() {
		_, _ = c.TransferJobs.Delete(job.Name, project).Do()
	})
	if job.TransferSpec == nil || job.TransferSpec.GcsDataSink == nil || job.TransferSpec.GcsDataSink.BucketName != dst ||
		job.TransferSpec.ObjectConditions == nil || strings.Join(job.TransferSpec.ObjectConditions.IncludePrefixes, ",") != "a/" {
		t.Errorf("the job made on the page = %+v", job.TransferSpec)
	}
	id := strings.TrimPrefix(job.Name, "transferJobs/")
	p.navigate("/transfer/jobs/" + id + "?project=" + project)
	p.clickAction("Start a run", "")
	var op *stv1.Operation
	eventually(t, "the run to succeed", func() bool {
		l, err := c.TransferOperations.List("transferOperations", fmt.Sprintf(`{"projectId":%q,"jobNames":[%q]}`, project, job.Name)).Do()
		if err != nil || len(l.Operations) == 0 {
			return false
		}
		op = l.Operations[0]
		return op.Done
	})
	if !strings.Contains(string(op.Metadata), `"SUCCESS"`) || !strings.Contains(string(op.Metadata), `"objectsCopiedToSink":"2"`) {
		t.Errorf("the run's TransferOperation = %s; want SUCCESS with 2 objects copied", op.Metadata)
	}
	for n, want := range map[string]bool{"a/1": true, "a/2": true, "b/3": false} {
		_, err := gcs.Bucket(dst).Object(n).Attrs(ctx)
		if (err == nil) != want {
			t.Errorf("gs://%s/%s copied = %v, want %v", dst, n, err == nil, want)
		}
	}
	p.navigate("/transfer/jobs/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Run history")`)
	p.clickAction("Disable job", "")
	eventually(t, "the job to be disabled", func() bool {
		j, err := c.TransferJobs.Get(job.Name, project).Do()
		return err == nil && j.Status == "DISABLED"
	})

	pool := "browser-pool-" + suffix
	p.navigate("/transfer/agent-pools/create?project=" + project)
	p.submitForm(map[string]string{"agentPoolId": pool, "limitMbps": "3"}, "agentPoolId")
	t.Cleanup(func() { _, _ = c.Projects.AgentPools.Delete("projects/" + project + "/agentPools/" + pool).Do() })
	eventually(t, "the agent pool", func() bool {
		a, err := c.Projects.AgentPools.Get("projects/" + project + "/agentPools/" + pool).Do()
		return err == nil && a.BandwidthLimit != nil && a.BandwidthLimit.LimitMbps == 3
	})

	p.navigate("/transfer/jobs?project=" + project)
	p.openRowDelete(id, "Delete")
	p.confirmByName(id)
	eventually(t, "the job to be deleted", func() bool {
		j, err := c.TransferJobs.Get(job.Name, project).Do()
		return err != nil || j.Status == "DELETED"
	})
}

// TestStorageCompanionScreensMeetWCAG (#1957), in the storage shard: the
// Batch operations, Inventory reports, Transfer jobs and Agent pools lists
// and their create pages pass the console's WCAG 2.1 AA audit at 320 and
// 640 CSS pixels.
func TestStorageCompanionScreensMeetWCAG(t *testing.T) {
	needService(t, "storage")
	project := uniqueProject(t)
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Batch operations", "/storage/batch-operations"}, {"Inventory reports", "/storage/inventory-reports"},
		{"Insights datasets", "/storage/insights-datasets"},
		{"Transfer jobs", "/transfer/jobs"}, {"Agent pools", "/transfer/agent-pools"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	auditScreens(t, pages)
}

// TestStorageInsightsDatasetsThroughThePage (#1976), in the storage shard.
// Create dataset configuration on Cloud Storage > Insights datasets, over
// the project's number with a bucket regex, makes a configuration the
// official Storage Insights client reads; with BigQuery served, Link
// dataset on its page links it, and the page names the dataset. Delete
// from the row, once unlinked, deletes it.
func TestStorageInsightsDatasetsThroughThePage(t *testing.T) {
	needService(t, "storage-insights-datasets")
	ctx := context.Background()
	c, err := insights.NewRESTClient(ctx, option.WithEndpoint("http://"+endpoint(t, envStorageInsights)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	gcs, _ := storageSDK(t)
	// The linked dataset is made in the configuration's project, which
	// BigQuery must serve.
	project := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_BIGQUERY_PROJECT"))
	link := project != ""
	if !link {
		project = uniqueProject(t)
	}
	enableIntelligence(t, project)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	bucket := "cb-ds-" + suffix
	regionalBucket(t, gcs, project, bucket)
	writeObjects(t, gcs, bucket, "one")
	t.Cleanup(func() { _ = gcs.Bucket(bucket).Object("one").Delete(context.Background()) })
	attrs, err := gcs.Bucket(bucket).Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := "browser_" + suffix
	name := "projects/" + project + "/locations/us-central1/datasetConfigs/" + id
	t.Cleanup(func() {
		if op, err := c.UnlinkDataset(context.Background(), &storageinsightspb.UnlinkDatasetRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
		if op, err := c.DeleteDatasetConfig(context.Background(), &storageinsightspb.DeleteDatasetConfigRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	p := open(t)
	p.navigate("/storage/insights-datasets/create?project=" + project)
	p.submitForm(map[string]string{"id": id, "description": "from the page", "location": "us-central1",
		"projects": fmt.Sprint(attrs.ProjectNumber), "bucketFilter": "include", "buckets": "regex:cb-ds-" + suffix,
		"retention": "14"}, "id")
	var cfg *storageinsightspb.DatasetConfig
	eventually(t, "the configuration", func() bool {
		cfg, err = c.GetDatasetConfig(ctx, &storageinsightspb.GetDatasetConfigRequest{Name: name})
		return err == nil
	})
	if cfg.GetRetentionPeriodDays() != 14 || cfg.GetDescription() != "from the page" ||
		len(cfg.GetIncludeCloudStorageBuckets().GetCloudStorageBuckets()) != 1 {
		t.Errorf("the configuration made on the page = %v", cfg)
	}
	p.navigate("/storage/insights-datasets/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#detail-panel") !== null && document.querySelector("#detail-panel").textContent.includes("14 days")`)
	if link && serviceServed(t, "bigquery") {
		p.clickAction("Link dataset", "")
		eventually(t, "the link", func() bool {
			cfg, err = c.GetDatasetConfig(ctx, &storageinsightspb.GetDatasetConfigRequest{Name: name})
			return err == nil && cfg.GetLink().GetLinked()
		})
		p.navigate("/storage/insights-datasets/" + id + "?project=" + project)
		p.waitFor(fmt.Sprintf(`document.querySelector("#detail-panel") !== null && document.body.textContent.includes(%q)`, cfg.GetLink().GetDataset()))
		op, err := c.UnlinkDataset(ctx, &storageinsightspb.UnlinkDatasetRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if err := op.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	p.navigate("/storage/insights-datasets?project=" + project)
	p.openRowDelete(id, "Delete")
	p.confirmByName(id)
	eventually(t, "the configuration to be deleted", func() bool {
		_, err := c.GetDatasetConfig(ctx, &storageinsightspb.GetDatasetConfigRequest{Name: name})
		return err != nil
	})
}

// serviceServed reports whether the console has the screen id.
func serviceServed(t *testing.T, id string) bool {
	t.Helper()
	_, body := consoleDo(t, http.MethodGet, "/api/services", "")
	return strings.Contains(body, `"id":"`+id+`"`)
}
