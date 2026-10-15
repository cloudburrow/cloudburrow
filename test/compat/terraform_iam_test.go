//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Terraform's *_iam_member, *_iam_binding and *_iam_policy resources on the
// stored-IAM surfaces ADR-0006 adds for BigQuery (#1231) and Cloud Run
// (#1216), as its test rule asks (#1236). Each test applies the resources
// with the official hashicorp/google provider through `cloudburrow
// terraform`, reads the policy back through the official Go SDK, plans
// clean, and then takes the IAM resources away again and checks the
// bindings are gone, the resource itself staying, before destroying it.
//
// One resource cannot carry an *_iam_policy beside an *_iam_member or
// *_iam_binding (the policy is authoritative, and the two fight), so the
// module goes through three IAM phases on the same resources: member and
// binding, none, policy, none. Nothing is enforced (ADR-0006).

// tfIAMPhase is what the module holds for its IAM resources.
type tfIAMPhase int

const (
	tfIAMNone          tfIAMPhase = iota
	tfIAMMemberBinding            // an *_iam_member and an *_iam_binding, different roles
	tfIAMPolicy                   // an *_iam_policy from a google_iam_policy data source
)

// tfIAMTarget is one resource whose IAM the module manages.
type tfIAMTarget struct {
	kind  string            // the IAM resource's prefix, google_bigquery_table and so on
	label string            // the Terraform name of the target and its IAM resources
	ref   map[string]string // the IAM resource's arguments naming the target
	read  func() map[string][]string
}

const (
	tfIAMMember  = "user:ada@example.com"
	tfIAMMember2 = "group:burrowers@example.com"
)

// tfIAMResources renders the IAM resources of one phase for the targets.
// The member and binding take the target's two roles; the policy binds
// both.
func tfIAMResources(phase tfIAMPhase, roles [2]string, targets []tfIAMTarget) string {
	var b strings.Builder
	if phase == tfIAMPolicy {
		fmt.Fprintf(&b, `
data "google_iam_policy" "p" {
  binding {
    role    = %q
    members = [%q]
  }
  binding {
    role    = %q
    members = [%q, %q]
  }
}
`, roles[0], tfIAMMember, roles[1], tfIAMMember, tfIAMMember2)
	}
	for _, tg := range targets {
		keys := make([]string, 0, len(tg.ref))
		for k := range tg.ref {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var ref strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&ref, "  %s = %s\n", k, tg.ref[k])
		}
		switch phase {
		case tfIAMMemberBinding:
			fmt.Fprintf(&b, `
# Stored, never enforced (ADR-0006).
resource "%[1]s_iam_member" "%[2]s" {
%[3]s  role   = %[4]q
  member = %[5]q
}
resource "%[1]s_iam_binding" "%[2]s" {
%[3]s  role    = %[6]q
  members = [%[5]q, %[7]q]
}
`, tg.kind, tg.label, ref.String(), roles[0], tfIAMMember, roles[1], tfIAMMember2)
		case tfIAMPolicy:
			fmt.Fprintf(&b, `
# Stored, never enforced (ADR-0006).
resource "%[1]s_iam_policy" "%[2]s" {
%[3]s  policy_data = data.google_iam_policy.p.policy_data
}
`, tg.kind, tg.label, ref.String())
		}
	}
	return b.String()
}

// checkTFIAMPhase reads every target's policy back and compares it with what
// the phase applied.
func checkTFIAMPhase(t *testing.T, when string, phase tfIAMPhase, roles [2]string, targets []tfIAMTarget) {
	t.Helper()
	want := map[string]string{}
	switch phase {
	case tfIAMMemberBinding, tfIAMPolicy:
		want[roles[0]] = tfIAMMember
		want[roles[1]] = tfIAMMember2 + "," + tfIAMMember
	}
	for _, tg := range targets {
		got := map[string]string{}
		for role, members := range tg.read() {
			m := append([]string(nil), members...)
			sort.Strings(m)
			if len(m) != 0 {
				got[role] = strings.Join(m, ",")
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s, %s %s's policy = %v; want %v", when, tg.kind, tg.label, got, want)
		}
	}
}

// runTFIAMPhases drives the module through the phases, then destroys it.
func runTFIAMPhases(t *testing.T, m *tfModule, base string, roles [2]string, targets []tfIAMTarget) {
	t.Helper()
	m.write(base)
	m.must("init", "-input=false", "-no-color")
	for i, phase := range []tfIAMPhase{tfIAMMemberBinding, tfIAMNone, tfIAMPolicy, tfIAMNone} {
		when := fmt.Sprintf("phase %d (%s)", i+1, map[tfIAMPhase]string{
			tfIAMNone: "IAM resources removed", tfIAMMemberBinding: "iam_member and iam_binding", tfIAMPolicy: "iam_policy"}[phase])
		m.write(base + tfIAMResources(phase, roles, targets))
		m.apply()
		checkTFIAMPhase(t, "after "+when, phase, roles, targets)
		m.planClean()
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}

// TestTerraformBigQueryIAM (#1236): google_bigquery_table_iam_* on a table
// and on a view, and google_bigquery_routine_iam_* on a routine, against
// the BigQuery front (#1231), with the dataset, table, view and routine
// made by the provider too. The policies are read back with the official
// client's Table.IAM and the generated client's routines.getIamPolicy.
// covers: bigquery.datasets.insert, bigquery.datasets.get, bigquery.datasets.delete, bigquery.tables.insert, bigquery.tables.get, bigquery.tables.delete, bigquery.routines.insert, bigquery.routines.get, bigquery.routines.delete, bigquery.tables.getIamPolicy, bigquery.tables.setIamPolicy, bigquery.routines.getIamPolicy, bigquery.routines.setIamPolicy
func TestTerraformBigQueryIAM(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h) // skips without CLOUDBURROW_TEST_BIGQUERY
	svc := bigqueryService(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	ds := "tf_" + strings.ReplaceAll(h.Project(), "-", "_")

	base := fmt.Sprintf(`
resource "google_bigquery_dataset" "d" {
  project                    = %[1]q
  dataset_id                 = %[2]q
  location                   = "US"
  delete_contents_on_destroy = true
}
resource "google_bigquery_table" "t" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.d.dataset_id
  table_id            = "orders"
  deletion_protection = false
  schema = jsonencode([
    { name = "id", type = "INTEGER", mode = "NULLABLE" },
    { name = "region", type = "STRING", mode = "NULLABLE" },
  ])
}
resource "google_bigquery_table" "v" {
  project             = %[1]q
  dataset_id          = google_bigquery_dataset.d.dataset_id
  table_id            = "orders_view"
  deletion_protection = false
  view {
    query          = "SELECT id FROM `+"`%[1]s.%[2]s.orders`"+`"
    use_legacy_sql = false
  }
  depends_on = [google_bigquery_table.t]
}
resource "google_bigquery_routine" "r" {
  project         = %[1]q
  dataset_id      = google_bigquery_dataset.d.dataset_id
  routine_id      = "plus_one"
  routine_type    = "SCALAR_FUNCTION"
  language        = "SQL"
  definition_body = "x + 1"
  arguments {
    name      = "x"
    data_type = jsonencode({ typeKind = "INT64" })
  }
  return_type = jsonencode({ typeKind = "INT64" })
}
`, project, ds)

	table := func(id string) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			p, err := c.Dataset(ds).Table(id).IAM().Policy(ctx)
			if err != nil {
				t.Fatalf("Table(%s).IAM().Policy: %v", id, err)
			}
			out := map[string][]string{}
			for _, r := range p.Roles() {
				out[string(r)] = p.Members(r)
			}
			return out
		}
	}
	routine := "projects/" + project + "/datasets/" + ds + "/routines/plus_one"
	targets := []tfIAMTarget{
		{kind: "google_bigquery_table", label: "t", read: table("orders"), ref: map[string]string{
			"project": fmt.Sprintf("%q", project), "dataset_id": "google_bigquery_table.t.dataset_id", "table_id": "google_bigquery_table.t.table_id"}},
		{kind: "google_bigquery_table", label: "v", read: table("orders_view"), ref: map[string]string{
			"project": fmt.Sprintf("%q", project), "dataset_id": "google_bigquery_table.v.dataset_id", "table_id": "google_bigquery_table.v.table_id"}},
		{kind: "google_bigquery_routine", label: "r", ref: map[string]string{
			"project": fmt.Sprintf("%q", project), "dataset_id": "google_bigquery_routine.r.dataset_id", "routine_id": "google_bigquery_routine.r.routine_id"},
			read: func() map[string][]string {
				t.Helper()
				p, err := svc.Routines.GetIamPolicy(routine, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
				if err != nil {
					t.Fatalf("routines.getIamPolicy: %v", err)
				}
				out := map[string][]string{}
				for _, b := range p.Bindings {
					out[b.Role] = b.Members
				}
				return out
			}},
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/bigquery.dataViewer", "roles/bigquery.dataEditor"}, targets)

	// Destroy took the dataset, and the tables, view and routine with it.
	if _, err := c.Dataset(ds).Metadata(ctx); !isCode(err, http.StatusNotFound) {
		t.Errorf("the dataset after destroy: %v; want 404", err)
	}
	if _, err := svc.Routines.GetIamPolicy(routine, &bq.GetIamPolicyRequest{}).Context(ctx).Do(); !isCode(err, http.StatusNotFound) {
		t.Errorf("routines.getIamPolicy after destroy: %v; want 404", err)
	}
}

// TestTerraformRunIAM (#1236): google_cloud_run_v2_service_iam_* and
// google_cloud_run_v2_job_iam_* against the Cloud Run adapter (#1216), over
// its JSON API, read back with the official gRPC clients.
// covers: google.cloud.run.v2.Services/GetIamPolicy, google.cloud.run.v2.Services/SetIamPolicy, google.cloud.run.v2.Jobs/GetIamPolicy, google.cloud.run.v2.Jobs/SetIamPolicy
func TestTerraformRunIAM(t *testing.T) {
	h := New(t)
	svcs := runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	jobs, _ := runJobClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	// Knative names are the IDs, shared across projects, so they carry the
	// test's unique suffix.
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	svcID, jobID := "tfiam-"+suffix, "tfiamj-"+suffix
	svc, job := runParent(h)+"/services/"+svcID, runParent(h)+"/jobs/"+jobID

	base := fmt.Sprintf(`
resource "google_cloud_run_v2_service" "s" {
  project             = %[1]q
  name                = %[2]q
  location            = "us-central1"
  deletion_protection = false
  template {
    containers {
      image = "ghcr.io/knative/helloworld-go:latest"
    }
  }
}
resource "google_cloud_run_v2_job" "j" {
  project             = %[1]q
  name                = %[3]q
  location            = "us-central1"
  deletion_protection = false
  template {
    template {
      containers {
        image   = %[4]q
        command = ["sh", "-c", "echo hi"]
      }
      max_retries = 1
    }
  }
}
`, h.Project(), svcID, jobID, jobImage)

	read := func(get func(*iampb.GetIamPolicyRequest) (*iampb.Policy, error), name string) func() map[string][]string {
		return func() map[string][]string {
			t.Helper()
			p, err := get(&iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy %s: %v", name, err)
			}
			out := map[string][]string{}
			for _, b := range p.GetBindings() {
				out[b.GetRole()] = b.GetMembers()
			}
			return out
		}
	}
	ref := func(res string) map[string]string {
		return map[string]string{"project": res + ".project", "location": res + ".location", "name": res + ".name"}
	}
	targets := []tfIAMTarget{
		{kind: "google_cloud_run_v2_service", label: "s", ref: ref("google_cloud_run_v2_service.s"),
			read: read(func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return svcs.GetIamPolicy(ctx, r) }, svc)},
		{kind: "google_cloud_run_v2_job", label: "j", ref: ref("google_cloud_run_v2_job.j"),
			read: read(func(r *iampb.GetIamPolicyRequest) (*iampb.Policy, error) { return jobs.GetIamPolicy(ctx, r) }, job)},
	}
	runTFIAMPhases(t, m, base, [2]string{"roles/run.invoker", "roles/run.developer"}, targets)

	if _, err := svcs.GetService(ctx, &runpb.GetServiceRequest{Name: svc}); status.Code(err) != codes.NotFound {
		t.Errorf("GetService after destroy: %v; want NotFound", err)
	}
	if _, err := jobs.GetJob(ctx, &runpb.GetJobRequest{Name: job}); status.Code(err) != codes.NotFound {
		t.Errorf("GetJob after destroy: %v; want NotFound", err)
	}
}
