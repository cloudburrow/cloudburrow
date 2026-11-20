//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformBigQueryDataTransferConfig (#1948): a scheduled query's
// google_bigquery_data_transfer_config applies with the official
// hashicorp/google provider through `cloudburrow terraform`
// (bigquery_data_transfer_custom_endpoint); the official client reads it
// back; the plan is clean; display_name and schedule change in place; and
// destroy deletes it.
// covers: google.cloud.bigquery.datatransfer.v1.DataTransferService/CreateTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/GetTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/UpdateTransferConfig, google.cloud.bigquery.datatransfer.v1.DataTransferService/DeleteTransferConfig
func TestTerraformBigQueryDataTransferConfig(t *testing.T) {
	h := New(t)
	g, _ := dataTransferClients(t, h) // skips without CLOUDBURROW_TEST_DATATRANSFER
	_, project := bigqueryClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	ds := "tfdt_" + strings.ReplaceAll(h.Project(), "-", "_")

	module := func(name, schedule string) string {
		return fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_data_transfer_config" "q" {
  project                = %[1]q
  display_name           = %[3]q
  location               = "US"
  data_source_id         = "scheduled_query"
  schedule               = %[4]q
  destination_dataset_id = google_bigquery_dataset.d.dataset_id
  params = {
    destination_table_name_template = "out_{run_date}"
    write_disposition               = "WRITE_APPEND"
    query                           = "SELECT @run_time AS t"
  }
}
output "name" { value = google_bigquery_data_transfer_config.q.name }
`, project, ds, name, schedule)
	}
	find := func(when string) *datatransferpb.TransferConfig {
		t.Helper()
		it := g.ListTransferConfigs(ctx, &datatransferpb.ListTransferConfigsRequest{Parent: "projects/" + project + "/locations/us"})
		for {
			c, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				t.Fatalf("%s, ListTransferConfigs: %v", when, err)
			}
			if c.GetDestinationDatasetId() == ds {
				return c
			}
		}
	}

	m.write(module("nightly", "every 24 hours"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	c := find("after apply")
	if c == nil || c.GetDisplayName() != "nightly" || c.GetSchedule() != "every 24 hours" {
		t.Fatalf("after apply: %v", c)
	}
	m.planClean()

	m.write(module("hourly", "every 1 hours"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not one in-place update:\n%s", lastLines(out, 20))
	}
	if c := find("after update"); c == nil || c.GetDisplayName() != "hourly" || c.GetSchedule() != "every 1 hours" {
		t.Fatalf("after update: %v", c)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := g.GetTransferConfig(ctx, &datatransferpb.GetTransferConfigRequest{Name: c.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy: %v, want NOT_FOUND", err)
	}
}
