//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/storage"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// #1304's ninth batch of google provider resources, each through
// `cloudburrow terraform` with hashicorp/google and egress blocked, read
// back through an official SDK, planned clean, changed in place where the
// resource has an updatable field, planned clean again and destroyed. Each
// test asks for the endpoints of the services it uses first, so it skips in
// a shard without them.

// TestTerraformCloudTasksQueueIAM (#1304): google_cloud_tasks_queue_iam_member,
// _binding and _policy on a google_cloud_tasks_queue, through
// TestTerraformBigQueryIAM's phases (member and binding, none, policy,
// none), each read back with the official client's GetIamPolicy and planned
// clean; destroy removes the queue. Stored, never enforced (ADR-0006).
// covers: google.cloud.tasks.v2.CloudTasks/GetIamPolicy, google.cloud.tasks.v2.CloudTasks/SetIamPolicy, google.cloud.tasks.v2.CloudTasks/CreateQueue, google.cloud.tasks.v2.CloudTasks/GetQueue, google.cloud.tasks.v2.CloudTasks/DeleteQueue
func TestTerraformCloudTasksQueueIAM(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h) // asks for the Tasks endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	id := "tf-q-iam-" + strings.TrimPrefix(h.Project(), "cb-test-")
	name := runParent(h) + "/queues/" + id
	base := fmt.Sprintf(`
resource "google_cloud_tasks_queue" "q" {
  project  = %q
  name     = %q
  location = "us-central1"
}
`, h.Project(), id)
	targets := []tfIAMTarget{{
		kind: "google_cloud_tasks_queue", label: "q",
		ref: map[string]string{"project": "google_cloud_tasks_queue.q.project", "location": "google_cloud_tasks_queue.q.location",
			"name": "google_cloud_tasks_queue.q.name"},
		read: func() map[string][]string {
			t.Helper()
			pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			return policyMap(pol)
		},
	}}
	runTFIAMPhases(t, m, base, [2]string{"roles/cloudtasks.enqueuer", "roles/cloudtasks.viewer"}, targets)
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy after destroy: %v; want NotFound", err)
	}
}

// TestTerraformStorageBucketIAM (#1304): google_storage_bucket_iam_member,
// _binding and _policy on a google_storage_bucket, through
// TestTerraformBigQueryIAM's phases, each read back with the official
// client's BucketHandle.IAM().Policy and planned clean; destroy removes the
// bucket. The legacy roles a bucket is born with (roles/storage.legacy*)
// are left out of the comparison: an iam_policy replaces them, as on
// Google. Stored, never enforced (ADR-0006).
// covers: storage.buckets.getIamPolicy, storage.buckets.setIamPolicy, storage.buckets.insert, storage.buckets.get, storage.buckets.delete
func TestTerraformStorageBucketIAM(t *testing.T) {
	h := New(t)
	sc := storageClient(t, h) // asks for the Storage endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	bucket := h.Project() + "-tfbktiam"
	base := fmt.Sprintf(`
resource "google_storage_bucket" "b" {
  project                     = %q
  name                        = %q
  location                    = "US"
  force_destroy               = true
  uniform_bucket_level_access = true
}
`, h.Project(), bucket)
	targets := []tfIAMTarget{{
		kind: "google_storage_bucket", label: "b",
		ref: map[string]string{"bucket": "google_storage_bucket.b.name"},
		read: func() map[string][]string {
			t.Helper()
			p, err := sc.Bucket(bucket).IAM().Policy(ctx)
			if err != nil {
				t.Fatalf("Bucket(%s).IAM().Policy: %v", bucket, err)
			}
			out := map[string][]string{}
			for _, r := range p.Roles() {
				if !strings.HasPrefix(string(r), "roles/storage.legacy") {
					out[string(r)] = p.Members(r)
				}
			}
			return out
		},
	}}
	runTFIAMPhases(t, m, base, [2]string{"roles/storage.objectViewer", "roles/storage.objectAdmin"}, targets)
	if _, err := sc.Bucket(bucket).Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("bucket %s after destroy: %v; want it gone", bucket, err)
	}
}

// TestTerraformCloudFunctions2FunctionIAM (#1304):
// google_cloudfunctions2_function_iam_member, _binding and _policy on a 2nd
// gen function, through TestTerraformBigQueryIAM's phases, each read back
// with the official v2 client's GetIamPolicy and planned clean. The
// function is made by the official v2 client from an uploaded source that
// is not a zip, so it is FAILED (its build fails) and the test runs where
// no function can be built; a function's IAM policy does not depend on its
// build. Stored, never enforced (ADR-0006).
// covers: google.cloud.functions.v2.FunctionService/GenerateUploadUrl, google.cloud.functions.v2.FunctionService/CreateFunction, google.cloud.functions.v2.FunctionService/GetFunction, google.cloud.functions.v2.FunctionService/DeleteFunction, google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy
func TestTerraformCloudFunctions2FunctionIAM(t *testing.T) {
	h := New(t)
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage) // the upload URL is a bucket's
	fc := functionsClient(t, h)                                     // asks for the Run endpoint
	m := newTFModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	id := "tf-fn2-iam"
	name := parent + "/functions/" + id
	up, err := fc.GenerateUploadUrl(ctx, &functionspb.GenerateUploadUrlRequest{Parent: parent})
	if err != nil {
		t.Fatalf("GenerateUploadUrl: %v", err)
	}
	putSource(ctx, t, up.GetUploadUrl(), []byte("this is not a zip archive"))
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
		defer dcancel()
		if op, err := fc.DeleteFunction(dctx, &functionspb.DeleteFunctionRequest{Name: name}); err == nil {
			_ = op.Wait(dctx)
		}
	})
	op, err := fc.CreateFunction(ctx, &functionspb.CreateFunctionRequest{Parent: parent, FunctionId: id, Function: &functionspb.Function{
		BuildConfig: &functionspb.BuildConfig{Runtime: "go123", EntryPoint: "Hello",
			Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: up.GetStorageSource()}}}}})
	if err != nil {
		t.Fatalf("CreateFunction: %v", err)
	}
	_, _ = op.Wait(ctx) // fails with the build's error: the source is not a zip
	if f, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); err != nil || f.GetState() != functionspb.Function_FAILED {
		t.Fatalf("GetFunction = %v, %v; want it FAILED", f, err)
	}
	targets := []tfIAMTarget{{
		kind: "google_cloudfunctions2_function", label: "f",
		ref: map[string]string{"project": fmt.Sprintf("%q", h.Project()), "location": `"us-central1"`, "cloud_function": fmt.Sprintf("%q", id)},
		read: func() map[string][]string {
			t.Helper()
			pol, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
			if err != nil {
				t.Fatalf("GetIamPolicy on %s: %v", name, err)
			}
			return policyMap(pol)
		},
	}}
	runTFIAMPhases(t, m, "", [2]string{"roles/cloudfunctions.invoker", "roles/cloudfunctions.developer"}, targets)
	if got := targets[0].read(); len(got) != 0 {
		t.Errorf("the function's policy after destroy = %v; want it empty", got)
	}
	dop, err := fc.DeleteFunction(ctx, &functionspb.DeleteFunctionRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteFunction: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteFunction.Wait: %v", err)
	}
	if _, err := fc.GetFunction(ctx, &functionspb.GetFunctionRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetFunction after delete = %v; want NotFound", err)
	}
}

// auditConfigsOf flattens a policy's audit configs to
// service/LOG_TYPE/exempted,members, sorted.
func auditConfigsOf(p *iampb.Policy) []string {
	var got []string
	for _, a := range p.GetAuditConfigs() {
		for _, l := range a.GetAuditLogConfigs() {
			got = append(got, a.GetService()+"/"+l.GetLogType().String()+"/"+strings.Join(l.GetExemptedMembers(), ","))
		}
	}
	slices.Sort(got)
	return got
}

// TestTerraformFolderIAMAuditConfig (#1304): a google_folder_iam_audit_config
// on a google_folder applies, the official v3 FoldersClient reads the
// folder's audit configs back, the plan is clean, new exempted members
// apply in place, the plan is clean again, and destroy removes the config
// and the folder. Stored, never enforced: nothing writes audit logs from
// it (ADR-0006).
// covers: google.cloud.resourcemanager.v3.Folders/CreateFolder, google.cloud.resourcemanager.v3.Folders/GetFolder, google.cloud.resourcemanager.v3.Folders/DeleteFolder, google.cloud.resourcemanager.v3.Folders/SearchFolders, google.cloud.resourcemanager.v3.Folders/GetIamPolicy, google.cloud.resourcemanager.v3.Folders/SetIamPolicy
func TestTerraformFolderIAMAuditConfig(t *testing.T) {
	h := New(t)
	fc, _, _ := foldersClients(t, h) // asks for the Resource Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	folder := folderLabel(h, "tfaud-")
	module := func(exempt string) string {
		return fmt.Sprintf(`
resource "google_folder" "f" {
  display_name        = %q
  parent              = %q
  deletion_protection = false
}
resource "google_folder_iam_audit_config" "a" {
  folder  = google_folder.f.name
  service = "storage.googleapis.com"
  audit_log_config {
    log_type         = "DATA_READ"
    exempted_members = [%q]
  }
  audit_log_config {
    log_type = "ADMIN_READ"
  }
}
`, folder, localOrg, exempt)
	}
	var folderName string
	check := func(when, exempt string) {
		t.Helper()
		if folderName == "" {
			f, err := fc.SearchFolders(ctx, &rmpb.SearchFoldersRequest{Query: fmt.Sprintf("displayName=%q", folder)}).Next()
			if err != nil {
				t.Fatalf("%s: SearchFolders(%q): %v", when, folder, err)
			}
			folderName = f.GetName()
		}
		p, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: folderName})
		if err != nil {
			t.Fatalf("%s: GetIamPolicy on %s: %v", when, folderName, err)
		}
		want := []string{"storage.googleapis.com/ADMIN_READ/", "storage.googleapis.com/DATA_READ/" + exempt}
		if got := auditConfigsOf(p); !slices.Equal(got, want) {
			t.Errorf("%s: the folder's audit configs = %v; want %v", when, got, want)
		}
	}
	m.write(module(tfIAMMember))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", tfIAMMember)
	m.planClean()

	m.write(module(tfIAMMember2))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", tfIAMMember2)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	f, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: folderName})
	if err == nil && f.GetState() != rmpb.Folder_DELETE_REQUESTED {
		t.Errorf("the folder after destroy = %v; want it DELETE_REQUESTED or gone", f)
	}
}

// TestTerraformProjectOrganizationPolicy (#1304, #1296): a boolean and a
// list google_project_organization_policy on a project of the test's own
// apply; the official cloudresourcemanager/v1 client reads both back with
// getOrgPolicy; the plan is clean; enforced = false and other allowed
// values apply in place (setOrgPolicy); both read back changed; the plan is
// clean again; destroy clears them (clearOrgPolicy), after which each reads
// back unset. Stored, never enforced.
// covers: cloudresourcemanager.projects.create, cloudresourcemanager.projects.delete, cloudresourcemanager.projects.getOrgPolicy, cloudresourcemanager.projects.setOrgPolicy, cloudresourcemanager.projects.clearOrgPolicy
func TestTerraformProjectOrganizationPolicy(t *testing.T) {
	h := New(t)
	v1 := rmV1(t, h) // asks for the Resource Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	id := iamProject(t, h, "tfop-")
	const boolC, listC = "constraints/iam.disableServiceAccountKeyCreation", "constraints/gcp.resourceLocations"
	module := func(enforced bool, location string) string {
		return fmt.Sprintf(`
resource "google_project_organization_policy" "b" {
  project    = %[1]q
  constraint = %[2]q
  boolean_policy {
    enforced = %[3]t
  }
}
resource "google_project_organization_policy" "l" {
  project    = %[1]q
  constraint = %[4]q
  list_policy {
    allow {
      values = [%[5]q]
    }
  }
}
`, id, boolC, enforced, listC, location)
	}
	get := func(when, constraint string) *crmv1.OrgPolicy {
		t.Helper()
		p, err := v1.Projects.GetOrgPolicy("projects/"+id, &crmv1.GetOrgPolicyRequest{Constraint: constraint}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("%s: getOrgPolicy(%s): %v", when, constraint, err)
		}
		return p
	}
	check := func(when string, enforced bool, location string) {
		t.Helper()
		if b := get(when, boolC); b.BooleanPolicy == nil || b.BooleanPolicy.Enforced != enforced {
			t.Errorf("%s: %s = %+v; want enforced = %t", when, boolC, b.BooleanPolicy, enforced)
		}
		if l := get(when, listC); l.ListPolicy == nil || !slices.Equal(l.ListPolicy.AllowedValues, []string{location}) {
			t.Errorf("%s: %s = %+v; want %s allowed", when, listC, l.ListPolicy, location)
		}
	}
	m.write(module(true, "in:us-locations"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", true, "in:us-locations")
	m.planClean()

	m.write(module(false, "in:eu-locations"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", false, "in:eu-locations")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, c := range []string{boolC, listC} {
		if p := get("after destroy", c); p.BooleanPolicy != nil || p.ListPolicy != nil || p.RestoreDefault != nil {
			t.Errorf("after destroy, %s = %+v; want it unset", c, p)
		}
	}
}

// TestTerraformProjectIAMMemberRemove (#1304): a google_project_iam_member_remove
// takes one member out of a role the official v3 client set on a project
// of the test's own, keeping the role's other member and the project's
// other roles; the plan is clean; naming the other member replaces it (the
// provider updates no argument in place), which takes that member out too,
// the role going with its last member; the plan is clean again; destroy
// only drops it from state, restoring no one. Stored, never enforced
// (ADR-0006).
// covers: cloudresourcemanager.projects.getIamPolicy, cloudresourcemanager.projects.setIamPolicy, google.cloud.resourcemanager.v3.Projects/GetIamPolicy, google.cloud.resourcemanager.v3.Projects/SetIamPolicy
func TestTerraformProjectIAMMemberRemove(t *testing.T) {
	h := New(t)
	c := rmProjects(t, h) // asks for the Resource Manager endpoint first
	m := newTFModule(t)
	ctx := h.Context()
	id := iamProject(t, h, "tfrm-")
	res := "projects/" + id
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Bindings: []*iampb.Binding{
		{Role: "roles/viewer", Members: []string{tfIAMMember, tfIAMMember2}},
		{Role: "roles/editor", Members: []string{tfIAMMember}},
	}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	module := func(member string) string {
		return fmt.Sprintf(`
resource "google_project_iam_member_remove" "r" {
  project = %q
  role    = "roles/viewer"
  member  = %q
}
`, id, member)
	}
	check := func(when string, want map[string]string) {
		t.Helper()
		p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
		if err != nil {
			t.Fatalf("%s: GetIamPolicy: %v", when, err)
		}
		if got := rolesOf(p); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: the project's policy = %v; want %v", when, got, want)
		}
	}
	m.write(module(tfIAMMember))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", map[string]string{"roles/viewer": tfIAMMember2, "roles/editor": tfIAMMember})
	m.planClean()

	m.write(module(tfIAMMember2))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "1 added, 0 changed, 1 destroyed") {
		t.Errorf("the change did not replace the resource:\n%s", lastLines(out, 20))
	}
	check("after the change", map[string]string{"roles/editor": tfIAMMember})
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	check("after destroy", map[string]string{"roles/editor": tfIAMMember})
}
