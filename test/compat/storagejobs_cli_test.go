//go:build compat

package compat

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storagebatchoperations/apiv1/storagebatchoperationspb"
	"cloud.google.com/go/storageinsights/apiv1/storageinsightspb"
	"cloud.google.com/go/storagetransfer/apiv1/storagetransferpb"
)

// Not yet passing (#1957 follow-up), so it covers nothing:
//
// TestGcloudTransferJobs (#1957): gcloud transfer jobs create (a bucket to
// bucket job with an include prefix, not run on create), list, describe,
// run (waiting for it), gcloud transfer operations list and describe, and
// jobs delete, against the local Storage Transfer Service.
func TestGcloudTransferJobs(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := transferClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	project := h.Project()
	src, dst := project+"-gts", project+"-gtd"
	sb := companionBucket(t, h, sc, src, "")
	db := companionBucket(t, h, sc, dst, "")
	putCompanionObject(t, ctx, sb, "in/a", "A")
	putCompanionObject(t, ctx, sb, "out/b", "B")
	name := "transferJobs/gcloud-" + strings.TrimPrefix(project, "cb-test-")
	g.must("transfer", "jobs", "create", "gs://"+src+"/", "gs://"+dst+"/", "--name", name, "--description", "gcloud job",
		"--include-prefixes", "in/", "--do-not-run", "--project", project)
	t.Cleanup(func() {
		_ = c.DeleteTransferJob(ctx, &storagetransferpb.DeleteTransferJobRequest{JobName: name, ProjectId: project})
	})
	if out := g.must("transfer", "jobs", "list", "--job-names", name, "--project", project, "--format=value(name)"); !strings.Contains(out, name) {
		t.Errorf("transfer jobs list lacks %s:\n%s", name, out)
	}
	if out := g.must("transfer", "jobs", "describe", name, "--project", project, "--format=value(description)"); strings.TrimSpace(out) != "gcloud job" {
		t.Errorf("transfer jobs describe: %q", out)
	}
	g.must("transfer", "jobs", "run", name, "--no-async", "--project", project)
	if got := objectBody(t, ctx, db, "in/a"); got != "A" {
		t.Errorf("after gcloud transfer jobs run: in/a = %q", got)
	}
	if got := objectBody(t, ctx, db, "out/b"); got != "<none>" {
		t.Errorf("out/b, outside the include prefix, was copied: %q", got)
	}
	ops := strings.Fields(g.must("transfer", "operations", "list", "--job-names", name, "--project", project, "--format=value(name)"))
	if len(ops) != 1 {
		t.Fatalf("transfer operations list: %v", ops)
	}
	if out := g.must("transfer", "operations", "describe", ops[0], "--project", project, "--format=value(metadata.status)"); strings.TrimSpace(out) != "SUCCESS" {
		t.Errorf("transfer operations describe: %q", out)
	}
	g.must("transfer", "jobs", "delete", name, "--project", project)
	if j, err := c.GetTransferJob(ctx, &storagetransferpb.GetTransferJobRequest{JobName: name, ProjectId: project}); err == nil &&
		j.GetStatus() != storagetransferpb.TransferJob_DELETED {
		t.Errorf("after gcloud transfer jobs delete: %v", j.GetStatus())
	}
}

// Not yet passing (#1957 follow-up), so it covers nothing:
//
// TestGcloudStorageBatchOperations (#1957): gcloud storage batch-operations
// jobs create (put metadata over a prefix), describe, list, cancel (refused
// once the job is done) and delete.
func TestGcloudStorageBatchOperations(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := batchClient(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	project := h.Project()
	setStorageIntelligence(t, h, "STANDARD")
	t.Cleanup(func() { setStorageIntelligence(t, h, "INHERIT") })
	name := project + "-gbo"
	bh := companionBucket(t, h, sc, name, "")
	putCompanionObject(t, ctx, bh, "p/a", "A")
	id := "gcloud-" + strings.TrimPrefix(project, "cb-test-")
	full := "projects/" + project + "/locations/global/jobs/" + id
	g.must("storage", "batch-operations", "jobs", "create", id, "--bucket", name, "--included-object-prefixes", "p/",
		"--put-metadata", "Cache-Control=no-store", "--description", "gcloud batch", "--project", project)
	t.Cleanup(func() { _ = c.DeleteJob(ctx, &storagebatchoperationspb.DeleteJobRequest{Name: full}) })
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := strings.TrimSpace(g.must("storage", "batch-operations", "jobs", "describe", id, "--project", project, "--format=value(state)"))
		if out == "SUCCEEDED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch-operations jobs describe: %q after 30 s", out)
		}
		time.Sleep(time.Second)
	}
	if a, err := bh.Object("p/a").Attrs(ctx); err != nil || a.CacheControl != "no-store" {
		t.Errorf("after the gcloud job: %+v, %v", a, err)
	}
	if out := g.must("storage", "batch-operations", "jobs", "list", "--project", project, "--format=value(name)"); !strings.Contains(out, full) {
		t.Errorf("batch-operations jobs list lacks %s:\n%s", full, out)
	}
	if out, err := g.run(nil, "storage", "batch-operations", "jobs", "cancel", id, "--project", project); err == nil {
		t.Errorf("cancel of a finished job succeeded:\n%s", out)
	}
	g.must("storage", "batch-operations", "jobs", "delete", id, "--project", project)
	if _, err := c.GetJob(ctx, &storagebatchoperationspb.GetJobRequest{Name: full}); err == nil {
		t.Error("the job is still there after gcloud delete")
	}
}

// covers: storageinsights.projects.locations.reportConfigs.create, storageinsights.projects.locations.reportConfigs.list, storageinsights.projects.locations.reportConfigs.get, storageinsights.projects.locations.reportConfigs.delete
//
// TestGcloudStorageInsightsInventoryReports (#1957): gcloud storage insights
// inventory-reports create (daily CSV with a header, into another bucket),
// list, describe and delete.
func TestGcloudStorageInsightsInventoryReports(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := insightsClient(t, h)
	g := newGcloudSession(t, h)
	project := h.Project()
	src, dst := project+"-gis", project+"-gid"
	companionBucket(t, h, sc, src, "")
	companionBucket(t, h, sc, dst, "")
	now := storageJobsClock(t, h.Endpoint(EnvStorageInsights), 0).UTC()
	out := g.must("storage", "insights", "inventory-reports", "create", "gs://"+src, "--destination", "gs://"+dst+"/gcloud/",
		"--metadata-fields", "project,bucket,name,size", "--csv-header", "--schedule-starts", now.Format("2006-01-02"),
		"--schedule-repeats", "daily", "--schedule-repeats-until", now.Add(72*time.Hour).Format("2006-01-02"),
		"--project", project, "--format=value(name)")
	// gcloud prints "Created report configuration: NAME" whatever --format
	// says, and names the project by number, as Google does.
	name := regexp.MustCompile(`projects/[^/\s]+/locations/[^/\s]+/reportConfigs/[^/\s]+`).FindString(out)
	if !strings.HasPrefix(name, "projects/") || !strings.Contains(name, "/reportConfigs/") {
		t.Fatalf("inventory-reports create printed %q", out)
	}
	t.Cleanup(func() {
		_ = c.DeleteReportConfig(h.Context(), &storageinsightspb.DeleteReportConfigRequest{Name: name})
	})
	if out := g.must("storage", "insights", "inventory-reports", "list", "--source", "gs://"+src, "--project", project, "--format=value(name)"); !strings.Contains(out, name) {
		t.Errorf("inventory-reports list lacks %s:\n%s", name, out)
	}
	if out := g.must("storage", "insights", "inventory-reports", "describe", name, "--format=value(objectMetadataReportOptions.storageDestinationOptions.bucket)"); strings.TrimSpace(out) != dst {
		t.Errorf("inventory-reports describe: %q", out)
	}
	g.must("storage", "insights", "inventory-reports", "delete", name, "--force")
	if _, err := c.GetReportConfig(h.Context(), &storageinsightspb.GetReportConfigRequest{Name: name}); err == nil {
		t.Error("the configuration is still there after gcloud delete")
	}
}

// tfDate is a Terraform date block's body.
func tfDate(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("year = %d\n    month = %d\n    day = %d", t.Year(), int(t.Month()), t.Day())
}

// covers: storagetransfer.transferJobs.create, storagetransfer.transferJobs.get, storagetransfer.transferJobs.patch, storagetransfer.googleServiceAccounts.get
//
// TestTerraformStorageTransferJob (#1957): google_storage_transfer_job, a
// scheduled bucket-to-bucket job with object conditions and transfer
// options, applies beside the google_storage_transfer_project_service_account
// data source, plans clean, updates its description in place and destroys
// (the job is then DELETED).
func TestTerraformStorageTransferJob(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := transferClient(t, h)
	m := newTFModule(t)
	project := h.Project()
	src, dst := project+"-tts", project+"-ttd"
	companionBucket(t, h, sc, src, "")
	companionBucket(t, h, sc, dst, "")
	start := storageJobsClock(t, h.Endpoint(EnvStorageTransfer), 0).Add(48 * time.Hour)
	module := func(desc string) string {
		return fmt.Sprintf(`
data "google_storage_transfer_project_service_account" "sa" {
  project = %[1]q
}

resource "google_storage_transfer_job" "j" {
  project     = %[1]q
  description = %[2]q
  transfer_spec {
    gcs_data_source {
      bucket_name = %[3]q
    }
    gcs_data_sink {
      bucket_name = %[4]q
    }
    object_conditions {
      include_prefixes = ["logs/"]
    }
    transfer_options {
      overwrite_when = "DIFFERENT"
    }
  }
  schedule {
    schedule_start_date {
    %[5]s
    }
    schedule_end_date {
    %[6]s
    }
    start_time_of_day {
      hours   = 3
      minutes = 0
      seconds = 0
      nanos   = 0
    }
    repeat_interval = "86400s"
  }
}

output "email" {
  value = data.google_storage_transfer_project_service_account.sa.email
}

output "job" {
  value = google_storage_transfer_job.j.name
}
`, project, desc, src, dst, tfDate(start), tfDate(start.Add(7*24*time.Hour)))
	}
	m.write(module("tf transfer"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if out := m.must("output", "-raw", "email"); !strings.HasSuffix(strings.TrimSpace(out), "@storage-transfer-service.iam.gserviceaccount.com") {
		t.Errorf("service account email: %q", out)
	}
	name := strings.TrimSpace(m.must("output", "-raw", "job"))
	t.Cleanup(func() {
		_ = c.DeleteTransferJob(h.Context(), &storagetransferpb.DeleteTransferJobRequest{JobName: name, ProjectId: project})
	})
	m.planClean()
	m.write(module("tf transfer 2"))
	m.apply()
	j, err := c.GetTransferJob(h.Context(), &storagetransferpb.GetTransferJobRequest{JobName: name, ProjectId: project})
	if err != nil || j.GetDescription() != "tf transfer 2" || j.GetSchedule().GetStartTimeOfDay().GetHours() != 3 ||
		len(j.GetTransferSpec().GetObjectConditions().GetIncludePrefixes()) != 1 {
		t.Errorf("after the in-place update: %+v, %v", j, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if j, err := c.GetTransferJob(h.Context(), &storagetransferpb.GetTransferJobRequest{JobName: name, ProjectId: project}); err == nil &&
		j.GetStatus() != storagetransferpb.TransferJob_DELETED {
		t.Errorf("after destroy: %v", j.GetStatus())
	}
}

// covers: storageinsights.projects.locations.reportConfigs.create, storageinsights.projects.locations.reportConfigs.get, storageinsights.projects.locations.reportConfigs.patch, storageinsights.projects.locations.reportConfigs.delete
//
// TestTerraformStorageInsightsReportConfig (#1957):
// google_storage_insights_report_config, a daily CSV inventory report of
// one bucket into another, applies, plans clean, updates its metadata
// fields in place and destroys.
func TestTerraformStorageInsightsReportConfig(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h)
	c := insightsClient(t, h)
	m := newTFModule(t)
	project := h.Project()
	src, dst := project+"-tis", project+"-tid"
	companionBucket(t, h, sc, src, "")
	companionBucket(t, h, sc, dst, "")
	now := storageJobsClock(t, h.Endpoint(EnvStorageInsights), 0)
	module := func(fields string) string {
		return fmt.Sprintf(`
resource "google_storage_insights_report_config" "r" {
  project  = %[1]q
  location = "us"
  frequency_options {
    frequency = "DAILY"
    start_date {
    %[2]s
    }
    end_date {
    %[3]s
    }
  }
  csv_options {
    record_separator = "\n"
    delimiter        = ","
    header_required  = true
  }
  object_metadata_report_options {
    metadata_fields = [%[4]s]
    storage_filters {
      bucket = %[5]q
    }
    storage_destination_options {
      bucket           = %[6]q
      destination_path = "tf"
    }
  }
}

output "name" {
  value = google_storage_insights_report_config.r.name
}
`, project, tfDate(now), tfDate(now.Add(30*24*time.Hour)), fields, src, dst)
	}
	m.write(module(`"project", "bucket", "name"`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	id := strings.TrimSpace(m.must("output", "-raw", "name"))
	name := id
	if !strings.HasPrefix(name, "projects/") {
		name = "projects/" + project + "/locations/us/reportConfigs/" + id
	}
	t.Cleanup(func() {
		_ = c.DeleteReportConfig(h.Context(), &storageinsightspb.DeleteReportConfigRequest{Name: name})
	})
	m.planClean()
	m.write(module(`"project", "bucket", "name", "size"`))
	m.apply()
	rc, err := c.GetReportConfig(h.Context(), &storageinsightspb.GetReportConfigRequest{Name: name})
	if err != nil || len(rc.GetObjectMetadataReportOptions().GetMetadataFields()) != 4 {
		t.Errorf("after the in-place update: %+v, %v", rc, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetReportConfig(h.Context(), &storageinsightspb.GetReportConfigRequest{Name: name}); err == nil {
		t.Error("the configuration is still there after destroy")
	}
}
