//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTerraformBigtableAppProfileAndIAM (#1440), in the emulators shard:
// google_bigtable_app_profile and google_bigtable_instance_iam_member,
// _binding and _policy, which the provider manages through Bigtable's admin
// REST API (bigtableadmin v2, JSON), served by the Bigtable front beside
// gRPC and given to the provider as bigtable_custom_endpoint by the
// wrapper. On a google_bigtable_instance (gRPC, through
// BIGTABLE_EMULATOR_HOST), an app profile applies, is read back through the
// official instance admin client, plans clean, takes an in-place update
// (its description and routing) and plans clean again; then the instance
// IAM resources run through TestTerraformBigQueryIAM's phases, each read
// back through the official client's InstanceIAM policy (stored, never
// enforced, ADR-0006); destroy removes the profile and the instance. All
// with egress to Google blocked.
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/CreateAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/GetAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/UpdateAppProfile, google.bigtable.admin.v2.BigtableInstanceAdmin/DeleteAppProfile
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/GetIamPolicy, google.bigtable.admin.v2.BigtableInstanceAdmin/SetIamPolicy
func TestTerraformBigtableAppProfileAndIAM(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	iac := bigtableInstanceAdmin(t, h)
	ctx := h.Context()
	const id, cluster, profile = "tf-bt-rest", "tf-bt-rest-c1", "tf-profile"
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	instance := fmt.Sprintf(`
resource "google_bigtable_instance" "i" {
  project             = %q
  name                = %q
  deletion_protection = false
  cluster {
    cluster_id   = %q
    zone         = "us-central1-b"
    num_nodes    = 1
    storage_type = "SSD"
  }
}
`, h.Project(), id, cluster)
	withProfile := func(description string, transactional bool) string {
		return instance + fmt.Sprintf(`
resource "google_bigtable_app_profile" "ap" {
  project         = google_bigtable_instance.i.project
  instance        = google_bigtable_instance.i.name
  app_profile_id  = %q
  description     = %q
  ignore_warnings = true
  single_cluster_routing {
    cluster_id                 = %q
    allow_transactional_writes = %t
  }
}
`, profile, description, cluster, transactional)
	}
	check := func(when, description string, transactional bool) {
		t.Helper()
		p, err := iac.GetAppProfile(ctx, id, profile)
		if err != nil {
			t.Fatalf("%s, GetAppProfile: %v", when, err)
		}
		r := p.GetSingleClusterRouting()
		if p.GetDescription() != description || r.GetClusterId() != cluster || r.GetAllowTransactionalWrites() != transactional {
			t.Errorf("%s, the app profile = %v", when, p)
		}
	}

	m.write(withProfile("from Terraform", true))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "from Terraform", true)
	m.planClean()

	m.write(withProfile("updated by Terraform", false))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the app profile was not updated in place:\n%s", lastLines(out, 20))
	}
	check("after the update", "updated by Terraform", false)
	m.planClean()

	// The instance's IAM, with the profile still there; the phases end
	// with destroy.
	targets := []tfIAMTarget{{
		kind: "google_bigtable_instance", label: "i",
		ref: map[string]string{"project": "google_bigtable_instance.i.project", "instance": "google_bigtable_instance.i.name"},
		read: func() map[string][]string {
			t.Helper()
			pol, err := iac.InstanceIAM(id).Policy(ctx)
			if err != nil {
				t.Fatalf("the instance's IAM policy: %v", err)
			}
			return policyMap(pol.InternalProto)
		},
	}}
	runTFIAMPhases(t, m, withProfile("updated by Terraform", false), [2]string{"roles/bigtable.user", "roles/bigtable.reader"}, targets)
	if _, err := iac.GetAppProfile(ctx, id, profile); status.Code(err) != codes.NotFound {
		t.Errorf("GetAppProfile after destroy: %v; want NotFound", err)
	}
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Errorf("InstanceInfo after destroy: %v; want NotFound", err)
	}
}
