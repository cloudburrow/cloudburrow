//go:build compat

package compat

// #2001: the tools' updates of a topic's message_storage_policy, which
// CloudBurrow's front applies (#2000), each recovering an
// IN_TRANSIT_LOCATION_RESTRICTION BigQuery subscription.

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// gcloudStoragePolicy is a topic's messageStoragePolicy as gcloud
// describes it.
func gcloudStoragePolicy(t *testing.T, g *gcloudSession, topic, project string) (regions []string, enforced bool) {
	t.Helper()
	var d struct {
		MessageStoragePolicy struct {
			AllowedPersistenceRegions []string
			EnforceInTransit          bool
		}
	}
	out := g.must("pubsub", "topics", "describe", topic, "--project="+project, "--format=json")
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("gcloud pubsub topics describe: %v\n%s", err, out)
	}
	return d.MessageStoragePolicy.AllowedPersistenceRegions, d.MessageStoragePolicy.EnforceInTransit
}

// covers: google.pubsub.v1.Publisher/CreateTopic, google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Subscriber/GetSubscription
//
// TestGcloudPubSubTopicStoragePolicyUpdate (#2001): a topic gcloud creates
// with --message-storage-policy-allowed-regions=us-east1 and
// --message-storage-policy-enforce-in-transit leaves a BigQuery
// subscription to a us-central1 dataset IN_TRANSIT_LOCATION_RESTRICTION;
// `gcloud pubsub topics update` adding us-central1 makes it ACTIVE, its
// held message written; an update back to us-east1 restricts it again;
// --recompute-message-storage-policy (gcloud 593's way to clear the
// policy: it has no --clear-message-storage-policy) with no organization
// policy allows all regions, so the topic describes no policy and the
// subscription is ACTIVE. Under gcp.resourceLocations in:us-locations the
// recompute pushes the US regions to the topic.
func TestGcloudPubSubTopicStoragePolicyUpdate(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager) // the organization policy, asked before any gcloud call
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "gcloud2001", "us-central1")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: dataSchema1874}); err != nil {
		t.Fatal(err)
	}
	g := newGcloudSession(t, h)
	p := "--project=" + h.Project()
	g.must("pubsub", "topics", "create", "gcloud-msp2001", p,
		"--message-storage-policy-allowed-regions=us-east1", "--message-storage-policy-enforce-in-transit")
	t.Cleanup(func() { _, _ = g.run(nil, "pubsub", "topics", "delete", "gcloud-msp2001", p) })
	g.must("pubsub", "subscriptions", "create", "gcloud-msp2001-bq", p, "--topic=gcloud-msp2001",
		"--bigquery-table="+bqProject+"."+ds.DatasetID+".t")
	t.Cleanup(func() { _, _ = g.run(nil, "pubsub", "subscriptions", "delete", "gcloud-msp2001-bq", p) })
	sub := "projects/" + h.Project() + "/subscriptions/gcloud-msp2001-bq"
	waitExportState(t, h, ps, sub, "IN_TRANSIT_LOCATION_RESTRICTION")
	g.must("pubsub", "topics", "publish", "gcloud-msp2001", p, "--message=held")

	g.must("pubsub", "topics", "update", "gcloud-msp2001", p,
		"--message-storage-policy-allowed-regions=us-east1,us-central1", "--message-storage-policy-enforce-in-transit")
	if r, e := gcloudStoragePolicy(t, g, "gcloud-msp2001", h.Project()); !slices.Equal(r, []string{"us-east1", "us-central1"}) || !e {
		t.Errorf("gcloud describes %v enforced=%v after the update, want [us-east1 us-central1] enforced", r, e)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the held message written once gcloud allows the region", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "held"
	})

	g.must("pubsub", "topics", "update", "gcloud-msp2001", p,
		"--message-storage-policy-allowed-regions=us-east1", "--message-storage-policy-enforce-in-transit")
	waitExportState(t, h, ps, sub, "IN_TRANSIT_LOCATION_RESTRICTION")

	g.must("pubsub", "topics", "update", "gcloud-msp2001", p, "--recompute-message-storage-policy")
	if r, e := gcloudStoragePolicy(t, g, "gcloud-msp2001", h.Project()); len(r) != 0 || e {
		t.Errorf("gcloud describes %v enforced=%v after the recompute, want no policy (all regions)", r, e)
	}
	waitExportState(t, h, ps, sub, "ACTIVE")
	if out, err := g.run(nil, "pubsub", "topics", "update", "gcloud-msp2001", p, "--message-storage-policy-allowed-regions=us-central1-a"); err == nil ||
		!strings.Contains(out, "INVALID_ARGUMENT") {
		t.Errorf("gcloud update to a zone: %v\n%s\nwant INVALID_ARGUMENT", err, out)
	}

	// Recomputed under gcp.resourceLocations: the US regions.
	project, _ := orgLocationProject(t, h, "msp-")
	g.must("pubsub", "topics", "create", "gcloud-msp2001-org", "--project="+project, "--message-storage-policy-allowed-regions=us-east1")
	t.Cleanup(func() { _, _ = g.run(nil, "pubsub", "topics", "delete", "gcloud-msp2001-org", "--project="+project) })
	g.must("pubsub", "topics", "update", "gcloud-msp2001-org", "--project="+project, "--recompute-message-storage-policy")
	r, _ := gcloudStoragePolicy(t, g, "gcloud-msp2001-org", project)
	if !slices.Contains(r, "us-central1") || !slices.Contains(r, "us-east1") ||
		slices.ContainsFunc(r, func(s string) bool { return !strings.HasPrefix(s, "us-") }) {
		t.Errorf("recomputed under in:us-locations: %v, want the US regions only", r)
	}
}

// covers: google.pubsub.v1.Publisher/UpdateTopic, google.pubsub.v1.Publisher/GetTopic, google.pubsub.v1.Subscriber/GetSubscription
//
// TestTerraformPubSubTopicStoragePolicyInPlace (#2001): a google_pubsub_topic
// whose message_storage_policy allows us-east1 with enforce_in_transit, and
// a BigQuery google_pubsub_subscription to a us-central1 dataset, which is
// IN_TRANSIT_LOCATION_RESTRICTION; adding us-central1 plans as an update of
// the topic (no replace, nothing created or destroyed), applies, reads
// back through the official client, plans clean, and the subscription is
// ACTIVE, its held message written.
func TestTerraformPubSubTopicStoragePolicyInPlace(t *testing.T) {
	h := New(t)
	bq, bqProject := bigqueryClient(t, h)
	ps, _ := exportClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	ds := bqDataset1874(t, h, bq, "tf2001", "us-central1")
	tb := ds.Table("t")
	if err := tb.Create(ctx, &bigquery.TableMetadata{Schema: dataSchema1874}); err != nil {
		t.Fatal(err)
	}
	module := func(regions string) string {
		return fmt.Sprintf(`
resource "google_pubsub_topic" "t" {
  project = %[1]q
  name    = "tf-msp2001"
  message_storage_policy {
    allowed_persistence_regions = [%[3]s]
    enforce_in_transit          = true
  }
}
resource "google_pubsub_subscription" "bq" {
  project = %[1]q
  name    = "tf-msp2001-bq"
  topic   = google_pubsub_topic.t.id
  bigquery_config {
    table = %[2]q
  }
}
`, h.Project(), bqProject+"."+ds.DatasetID+".t", regions)
	}
	topic := "projects/" + h.Project() + "/topics/tf-msp2001"
	sub := "projects/" + h.Project() + "/subscriptions/tf-msp2001-bq"
	m.write(module(`"us-east1"`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()
	waitExportState(t, h, ps, sub, "IN_TRANSIT_LOCATION_RESTRICTION")
	publish1874(t, h, ps, topic, "held")

	m.write(module(`"us-east1", "us-central1"`))
	m.must("plan", "-input=false", "-no-color", "-out=change.tfplan")
	var plan struct {
		ResourceChanges []struct {
			Address string
			Change  struct{ Actions []string }
		} `json:"resource_changes"`
	}
	if err := json.Unmarshal([]byte(m.must("show", "-json", "change.tfplan")), &plan); err != nil {
		t.Fatal(err)
	}
	for _, rc := range plan.ResourceChanges {
		want := []string{"no-op"}
		if rc.Address == "google_pubsub_topic.t" {
			want = []string{"update"}
		}
		if !slices.Equal(rc.Change.Actions, want) {
			t.Errorf("the plan %v %s, want %v", rc.Change.Actions, rc.Address, want)
		}
	}
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color", "change.tfplan")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	got, err := ps.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topic})
	if want := []string{"us-east1", "us-central1"}; err != nil || !slices.Equal(got.GetMessageStoragePolicy().GetAllowedPersistenceRegions(), want) ||
		!got.GetMessageStoragePolicy().GetEnforceInTransit() {
		t.Errorf("GetTopic after the apply = %v, %v; want %v enforced", got.GetMessageStoragePolicy(), err, want)
	}
	m.planClean()
	waitExportState(t, h, ps, sub, "ACTIVE")
	eventually1874(t, "the held message written once Terraform allows the region", 60*time.Second, func() bool {
		rows := bqRows1874(t, h, tb)
		return len(rows) == 1 && rows[0]["data"] == "held"
	})
}
