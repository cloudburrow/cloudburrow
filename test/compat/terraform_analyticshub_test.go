//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery/analyticshub/apiv1/analyticshubpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformBigQueryAnalyticsHub (#1960): a
// google_bigquery_analytics_hub_data_exchange, a _listing of a
// google_bigquery_dataset with restricted export, and a
// _listing_subscription into a linked dataset apply with the official
// hashicorp/google provider through `cloudburrow terraform`
// (bigquery_analytics_hub_custom_endpoint); the official client reads them
// back; the plan is clean; the listing's description changes in place; and
// destroy deletes them.
// covers: google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/CreateDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteDataExchange, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/CreateListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/UpdateListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/SubscribeListing, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/GetSubscription, google.cloud.bigquery.analyticshub.v1.AnalyticsHubService/DeleteSubscription
func TestTerraformBigQueryAnalyticsHub(t *testing.T) {
	h := New(t)
	g, _ := analyticsHubClients(t, h) // skips without CLOUDBURROW_TEST_ANALYTICSHUB
	_, project := bigqueryClient(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	suffix := strings.ReplaceAll(h.Project(), "-", "_")

	module := func(description string) string {
		return fmt.Sprintf(`
resource "google_bigquery_dataset" "shared" {
  project                    = %[1]q
  dataset_id                 = "tfah_shared_%[2]s"
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_analytics_hub_data_exchange" "x" {
  project          = %[1]q
  location         = "US"
  data_exchange_id = "tfah_%[2]s"
  display_name     = "Terraform exchange"
}
resource "google_bigquery_analytics_hub_listing" "l" {
  project          = %[1]q
  location         = "US"
  data_exchange_id = google_bigquery_analytics_hub_data_exchange.x.data_exchange_id
  listing_id       = "orders"
  display_name     = "Orders"
  description      = %[3]q
  bigquery_dataset {
    dataset = google_bigquery_dataset.shared.id
  }
  restricted_export_config {
    enabled               = true
    restrict_query_result = true
  }
}
resource "google_bigquery_analytics_hub_listing_subscription" "s" {
  project          = %[1]q
  location         = "US"
  data_exchange_id = google_bigquery_analytics_hub_data_exchange.x.data_exchange_id
  listing_id       = google_bigquery_analytics_hub_listing.l.listing_id
  destination_dataset {
    location = "US"
    dataset_reference {
      project_id = %[1]q
      dataset_id = "tfah_linked_%[2]s"
    }
  }
}
output "subscription" { value = google_bigquery_analytics_hub_listing_subscription.s.name }
`, project, suffix, description)
	}
	exchange := "projects/" + project + "/locations/us/dataExchanges/tfah_" + suffix
	listing := exchange + "/listings/orders"

	m.write(module("first"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if l, err := g.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing}); err != nil || l.GetDescription() != "first" || !l.GetRestrictedExportConfig().GetEnabled() {
		t.Fatalf("after apply: %v, %v", l, err)
	}
	sub := strings.TrimSpace(m.must("output", "-raw", "subscription"))
	if s, err := g.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: sub}); err != nil || s.GetState() != analyticshubpb.Subscription_STATE_ACTIVE {
		t.Fatalf("subscription %q: %v, %v", sub, s, err)
	}
	m.planClean()

	m.write(module("second"))
	if out := m.must("apply", "-auto-approve", "-input=false", "-no-color"); !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not one in-place update:\n%s", lastLines(out, 20))
	}
	if l, err := g.GetListing(ctx, &analyticshubpb.GetListingRequest{Name: listing}); err != nil || l.GetDescription() != "second" {
		t.Fatalf("after update: %v, %v", l, err)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := g.GetDataExchange(ctx, &analyticshubpb.GetDataExchangeRequest{Name: exchange}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy: %v, want NOT_FOUND", err)
	}
	if _, err := g.GetSubscription(ctx, &analyticshubpb.GetSubscriptionRequest{Name: sub}); status.Code(err) != codes.NotFound {
		t.Errorf("subscription after destroy: %v, want NOT_FOUND", err)
	}
}
