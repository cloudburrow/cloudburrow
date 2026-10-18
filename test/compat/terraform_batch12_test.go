//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iterator"

	vlogging "cloud.google.com/go/logging/apiv2"
)

// #1304's twelfth batch: the google provider data sources over APIs the
// storage and served shards' instances serve. The *_iam_policy data sources
// of the stored-IAM resources are read by runTFIAMPhases (policyData); these
// are the others. Each goes through `cloudburrow terraform` with
// hashicorp/google and egress blocked, is compared with what an official
// client reads, planned clean, changed, planned clean again and destroyed.
// Each test asks for its services' endpoints first, so it skips in a shard
// without them.

// TestTerraformClientConfigAndTagLists (#1304): google_client_config names
// the instance's project, the one `cloudburrow terraform` configures, and
// google_organizations lists what organizations.search (v1) does; a
// google_tags_tag_key with two google_tags_tag_value under the organization
// applies, and the data sources google_tags_tag_keys (of the organization)
// and google_tags_tag_values (of the key) list them as the official
// TagKeys and TagValues clients do; the plan is clean; a new description
// on the key and on a value applies in place and the lists follow; the plan
// is clean again; destroy deletes the key and its values.
// covers: google.cloud.resourcemanager.v3.TagKeys/ListTagKeys, google.cloud.resourcemanager.v3.TagValues/ListTagValues, cloudresourcemanager.organizations.search
func TestTerraformClientConfigAndTagLists(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := newTagClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	short := v1ID(h, "tfl-")
	module := func(desc string) string {
		return fmt.Sprintf(`
data "google_client_config" "c" {}
data "google_organizations" "o" {}
resource "google_tags_tag_key" "k" {
  parent      = %[1]q
  short_name  = %[2]q
  description = %[3]q
}
resource "google_tags_tag_value" "prod" {
  parent      = google_tags_tag_key.k.id
  short_name  = "prod"
  description = %[3]q
}
resource "google_tags_tag_value" "dev" {
  parent      = google_tags_tag_key.k.id
  short_name  = "dev"
  description = "development"
}
data "google_tags_tag_keys" "all" {
  parent     = %[1]q
  depends_on = [google_tags_tag_key.k]
}
data "google_tags_tag_values" "mine" {
  parent     = google_tags_tag_key.k.id
  depends_on = [google_tags_tag_value.prod, google_tags_tag_value.dev]
}
output "outputs" {
  value = {
    project = data.google_client_config.c.project
    orgs    = sort([for o in data.google_organizations.o.organizations : "${o.name}|${o.org_id}|${o.display_name}|${o.lifecycle_state}|${o.directory_customer_id}"])
    keys    = { for k in data.google_tags_tag_keys.all.keys : k.name => k.description }
    values  = { for v in data.google_tags_tag_values.mine.values : v.name => "${v.short_name}:${v.description}" }
  }
}
`, localOrg, short, desc)
	}
	check := func(when, desc string) {
		t.Helper()
		var got struct {
			Project string
			Orgs    []string
			Keys    map[string]string
			Values  map[string]string
		}
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		if got.Project != m.project {
			t.Errorf("%s: google_client_config's project = %q; want %q", when, got.Project, m.project)
		}
		orgs, err := rmV1(t, h).Organizations.Search(&crmv1.SearchOrganizationsRequest{}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: organizations.search: %v", when, err)
		}
		var wantOrgs []string
		for _, o := range orgs.Organizations {
			customer := ""
			if o.Owner != nil {
				customer = o.Owner.DirectoryCustomerId
			}
			wantOrgs = append(wantOrgs, fmt.Sprintf("%s|%s|%s|%s|%s", o.Name, strings.TrimPrefix(o.Name, "organizations/"), o.DisplayName, o.LifecycleState, customer))
		}
		sort.Strings(wantOrgs)
		if fmt.Sprint(got.Orgs) != fmt.Sprint(wantOrgs) || len(wantOrgs) == 0 {
			t.Errorf("%s: google_organizations = %v; organizations.search %v", when, got.Orgs, wantOrgs)
		}
		key, err := c.keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: strings.TrimPrefix(localOrg, "organizations/") + "/" + short})
		if err != nil {
			t.Fatalf("%s: GetNamespacedTagKey: %v", when, err)
		}
		// Other tests make keys under the organization too: every key the
		// official client lists is compared, and ours must be among them.
		keys := map[string]string{}
		it := c.keys.ListTagKeys(ctx, &rmpb.ListTagKeysRequest{Parent: localOrg})
		for {
			k, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("%s: ListTagKeys: %v", when, err)
			}
			keys[k.GetName()] = k.GetDescription()
		}
		if got.Keys[key.GetName()] != desc || keys[key.GetName()] != desc {
			t.Errorf("%s: google_tags_tag_keys names %s as %q, the official client as %q; want %q", when, key.GetName(), got.Keys[key.GetName()], keys[key.GetName()], desc)
		}
		values := map[string]string{}
		vit := c.values.ListTagValues(ctx, &rmpb.ListTagValuesRequest{Parent: key.GetName()})
		for {
			v, err := vit.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("%s: ListTagValues: %v", when, err)
			}
			values[v.GetName()] = v.GetShortName() + ":" + v.GetDescription()
		}
		if fmt.Sprint(got.Values) != fmt.Sprint(values) || len(values) != 2 {
			t.Errorf("%s: google_tags_tag_values = %v; the official client lists %v", when, got.Values, values)
		}
		var prod string
		for _, v := range values {
			if strings.HasPrefix(v, "prod:") {
				prod = v
			}
		}
		if prod != "prod:"+desc {
			t.Errorf("%s: the prod value reads %q; want description %q", when, prod, desc)
		}
	}

	m.write(module("env"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "env")
	m.planClean()

	m.write(module("environment"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("the descriptions were not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "environment")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: strings.TrimPrefix(localOrg, "organizations/") + "/" + short}); err == nil {
		t.Error("the tag key survived destroy")
	}
}

// TestTerraformLoggingProjectSettings (#1304): the data source
// google_logging_project_settings reads the project's Cloud Logging
// settings as Google's generated logging/v2 JSON client's
// projects.getSettings does, before and
// after a google_logging_project_sink in the module changes (the settings
// are the project's, which no Terraform resource of the project manages).
// The plan is clean each time; destroy deletes the sink.
// covers: google.logging.v2.ConfigServiceV2/GetSettings
func TestTerraformLoggingProjectSettings(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h) // skips without CLOUDBURROW_TEST_LOGGING
	svc := loggingJSONService(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	config, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	p := h.Project()
	module := func(filter string) string {
		return fmt.Sprintf(`
resource "google_logging_project_sink" "k" {
  project     = %[1]q
  name        = "tf-settings-sink"
  destination = "logging.googleapis.com/projects/%[1]s/locations/global/buckets/_Default"
  filter      = %[2]q
}
data "google_logging_project_settings" "s" {
  project    = %[1]q
  depends_on = [google_logging_project_sink.k]
}
output "outputs" {
  value = {
    name                       = data.google_logging_project_settings.s.name
    storage_location           = data.google_logging_project_settings.s.storage_location
    kms_key_name               = data.google_logging_project_settings.s.kms_key_name
    kms_service_account_id     = data.google_logging_project_settings.s.kms_service_account_id
    logging_service_account_id = data.google_logging_project_settings.s.logging_service_account_id
    disable_default_sink       = tostring(data.google_logging_project_settings.s.disable_default_sink)
  }
}
`, p, filter)
	}
	check := func(when string) {
		t.Helper()
		s, err := svc.Projects.GetSettings("projects/" + p).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: GetSettings: %v", when, err)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		want := map[string]string{
			"name": s.Name, "storage_location": s.StorageLocation, "kms_key_name": s.KmsKeyName,
			"kms_service_account_id": s.KmsServiceAccountId, "logging_service_account_id": s.LoggingServiceAccountId,
			"disable_default_sink": fmt.Sprint(s.DisableDefaultSink),
		}
		if fmt.Sprint(got) != fmt.Sprint(want) || got["name"] == "" || got["logging_service_account_id"] == "" {
			t.Errorf("%s: google_logging_project_settings = %v; the official client reads %v", when, got, want)
		}
	}

	m.write(module("severity>=ERROR"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply")
	m.planClean()

	m.write(module("severity>=WARNING"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the sink was not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := config.GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: "projects/" + p + "/sinks/tf-settings-sink"}); err == nil {
		t.Error("the sink survived destroy")
	}
}
