//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformBigQueryListsAndServiceAccount (#1304): two
// google_bigquery_dataset, one with two google_bigquery_table, apply; the
// data sources google_bigquery_datasets (of the project),
// google_bigquery_tables (of the dataset) and
// google_bigquery_default_service_account read what Google's generated
// BigQuery client lists and projects.getServiceAccount answers; the plan
// is clean; new labels on a dataset and a table apply in place and the
// lists follow; the plan is clean again; destroy deletes the datasets. The
// emulator serves one project (CLOUDBURROW_TEST_BIGQUERY_PROJECT), which
// other tests share, so the datasets compared are this test's.
// covers: bigquery.datasets.list, bigquery.tables.list, bigquery.projects.getServiceAccount
func TestTerraformBigQueryListsAndServiceAccount(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	svc := bigqueryService(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	suffix := strings.ReplaceAll(strings.TrimPrefix(h.Project(), "cb-test-"), "-", "_")
	ds1, ds2 := "tflist_a_"+suffix, "tflist_b_"+suffix
	module := func(label string) string {
		return fmt.Sprintf(`
resource "google_bigquery_dataset" "a" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  friendly_name              = "first"
  location                   = "US"
  labels                     = { env = %[4]q }
  delete_contents_on_destroy = true
}
resource "google_bigquery_dataset" "b" {
  project                    = %[1]q
  dataset_id                 = %[3]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_table" "t1" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.a.dataset_id
  table_id            = "orders"
  deletion_protection = false
  labels              = { env = %[4]q }
  schema              = jsonencode([{ name = "id", type = "INT64" }])
}
resource "google_bigquery_table" "t2" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.a.dataset_id
  table_id            = "customers"
  deletion_protection = false
  schema              = jsonencode([{ name = "id", type = "INT64" }])
}
data "google_bigquery_datasets" "all" {
  project    = %[1]q
  depends_on = [google_bigquery_dataset.a, google_bigquery_dataset.b]
}
data "google_bigquery_tables" "a" {
  project    = %[1]q
  dataset_id = google_bigquery_dataset.a.dataset_id
  depends_on = [google_bigquery_table.t1, google_bigquery_table.t2]
}
data "google_bigquery_default_service_account" "sa" {
  project = %[1]q
}
output "outputs" {
  value = {
    datasets = { for d in data.google_bigquery_datasets.all.datasets : d.dataset_id => "${d.friendly_name}|${d.location}|${jsonencode(d.labels)}" }
    tables   = { for t in data.google_bigquery_tables.a.tables : t.table_id => jsonencode(t.labels) }
    email    = data.google_bigquery_default_service_account.sa.email
    member   = data.google_bigquery_default_service_account.sa.member
  }
}
`, project, ds1, ds2, label)
	}
	labels := func(m map[string]string) string {
		if m == nil {
			m = map[string]string{}
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	check := func(when, label string) {
		t.Helper()
		var got struct {
			Datasets, Tables map[string]string
			Email, Member    string
		}
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		list, err := svc.Datasets.List(project).All(true).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: datasets.list: %v", when, err)
		}
		want := map[string]string{}
		for _, d := range list.Datasets {
			id := d.DatasetReference.DatasetId
			if id == ds1 || id == ds2 {
				want[id] = d.FriendlyName + "|" + d.Location + "|" + labels(d.Labels)
			}
		}
		mine := map[string]string{ds1: got.Datasets[ds1], ds2: got.Datasets[ds2]}
		if fmt.Sprint(mine) != fmt.Sprint(want) || !strings.Contains(want[ds1], `"env":"`+label+`"`) {
			t.Errorf("%s: google_bigquery_datasets reads %v; datasets.list %v", when, mine, want)
		}
		tl, err := svc.Tables.List(project, ds1).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: tables.list: %v", when, err)
		}
		wantT := map[string]string{}
		for _, tb := range tl.Tables {
			wantT[tb.TableReference.TableId] = labels(tb.Labels)
		}
		if fmt.Sprint(got.Tables) != fmt.Sprint(wantT) || len(wantT) != 2 || !strings.Contains(wantT["orders"], `"env":"`+label+`"`) {
			t.Errorf("%s: google_bigquery_tables reads %v; tables.list %v", when, got.Tables, wantT)
		}
		sa, err := svc.Projects.GetServiceAccount(project).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: projects.getServiceAccount: %v", when, err)
		}
		if got.Email != sa.Email || got.Member != "serviceAccount:"+sa.Email {
			t.Errorf("%s: google_bigquery_default_service_account = %q, %q; projects.getServiceAccount names %q", when, got.Email, got.Member, sa.Email)
		}
	}

	m.write(module("dev"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "dev")
	m.planClean()

	m.write(module("prod"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("the labels were not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "prod")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, ds := range []string{ds1, ds2} {
		if _, err := svc.Datasets.Get(project, ds).Context(ctx).Do(); httpCode(err) != 404 {
			t.Errorf("dataset %s after destroy: %v; want 404", ds, err)
		}
	}
}

// TestTerraformRedisClusterACLPolicy (#1304): a
// google_redis_cluster_acl_policy with two rules applies and the official
// CloudRedisCluster client's GetAclPolicy reads the rules back; the data
// source google_redis_cluster_acl_policy reads the same rules, etag and
// state; the plan is clean; a changed rule applies in place
// (UpdateAclPolicy) and both read it; the plan is clean again; destroy
// deletes the policy (NOT_FOUND).
// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateAclPolicy, google.cloud.redis.cluster.v1.CloudRedisCluster/GetAclPolicy, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateAclPolicy, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteAclPolicy
func TestTerraformRedisClusterACLPolicy(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h) // skips without CLOUDBURROW_TEST_MEMORYSTORE_ADMIN
	m := newTFModule(t)
	ctx := h.Context()
	id := "tf-acl-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := "projects/" + project + "/locations/us-central1/aclPolicies/" + id
	module := func(readerRule string) string {
		return fmt.Sprintf(`
resource "google_redis_cluster_acl_policy" "p" {
  project       = %[1]q
  location      = "us-central1"
  acl_policy_id = %[2]q
  rules {
    username = "reader"
    rule     = %[3]q
  }
  rules {
    username = "writer"
    rule     = "on >writerpass ~app:* +@all"
  }
}
data "google_redis_cluster_acl_policy" "p" {
  project       = %[1]q
  location      = "us-central1"
  acl_policy_id = google_redis_cluster_acl_policy.p.acl_policy_id
  depends_on    = [google_redis_cluster_acl_policy.p]
}
output "outputs" {
  value = {
    name  = data.google_redis_cluster_acl_policy.p.name
    etag  = data.google_redis_cluster_acl_policy.p.etag
    state = data.google_redis_cluster_acl_policy.p.state
    rules = sort([for r in data.google_redis_cluster_acl_policy.p.rules : "${r.username}=${r.rule}"])
  }
}
`, project, id, readerRule)
	}
	check := func(when, readerRule string) {
		t.Helper()
		pol, err := c.GetAclPolicy(ctx, &clusterpb.GetAclPolicyRequest{Name: name})
		if err != nil {
			t.Fatalf("%s: GetAclPolicy: %v", when, err)
		}
		var rules []string
		for _, r := range pol.GetRules() {
			rules = append(rules, r.GetUsername()+"="+r.GetRule())
		}
		sort.Strings(rules)
		var got struct {
			Name, Etag, State string
			Rules             []string
		}
		if err := json.Unmarshal([]byte(m.must("output", "-json", "outputs")), &got); err != nil {
			t.Fatalf("%s, the outputs: %v", when, err)
		}
		if got.Name != pol.GetName() || got.Etag != pol.GetEtag() || got.State != pol.GetState().String() ||
			fmt.Sprint(got.Rules) != fmt.Sprint(rules) || !strings.Contains(fmt.Sprint(rules), "reader="+readerRule) {
			t.Errorf("%s: the data source reads %+v; GetAclPolicy %v", when, got, pol)
		}
	}

	m.write(module("on >readerpass ~app:* +@read"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "on >readerpass ~app:* +@read")
	m.planClean()

	m.write(module("on >readerpass ~cache:* +@read +ping"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the rule was not changed in place:\n%s", lastLines(out, 20))
	}
	check("after the change", "on >readerpass ~cache:* +@read +ping")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetAclPolicy(ctx, &clusterpb.GetAclPolicyRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetAclPolicy after destroy: %v; want NotFound", err)
	}
}
