//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	dfpb "cloud.google.com/go/dataform/apiv1/dataformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.dataform.v1.Dataform/CreateRepository, google.cloud.dataform.v1.Dataform/GetRepository, google.cloud.dataform.v1.Dataform/UpdateRepository, google.cloud.dataform.v1.Dataform/DeleteRepository
//
// TestTerraformDataformRepository (#2105): hashicorp/google's
// google_dataform_repository through `cloudburrow terraform`
// (dataform_custom_endpoint), egress to Google blocked: apply, read back by
// the official client, plan clean, an in-place update of its labels and
// workspace compilation overrides, and destroy.
func TestTerraformDataformRepository(t *testing.T) {
	h := New(t)
	c := dataformClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_DATAFORM
	ctx := h.Context()
	m := newTFModule(t)
	id := fmt.Sprintf("tf-df-%d", time.Now().UnixNano()%1e6)
	conf := func(team string) string {
		return fmt.Sprintf(`
resource "google_dataform_repository" "r" {
  project      = %[1]q
  region       = "us-central1"
  name         = %[2]q
  display_name = "Terraform repository"
  labels       = { team = %[3]q }
  workspace_compilation_overrides {
    default_database = %[1]q
    schema_suffix    = %[3]q
    table_prefix     = "tf"
  }
}
`, m.project, id, team)
	}
	m.write(conf("one"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + m.project + "/locations/us-central1/repositories/" + id
	r, err := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: name})
	if err != nil || r.GetLabels()["team"] != "one" || r.GetWorkspaceCompilationOverrides().GetSchemaSuffix() != "one" || r.GetDisplayName() != "Terraform repository" {
		t.Fatalf("the repository Terraform made: %+v, %v", r, err)
	}
	m.planClean()
	m.write(conf("two"))
	m.apply()
	if r, err := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: name}); err != nil || r.GetLabels()["team"] != "two" || r.GetWorkspaceCompilationOverrides().GetSchemaSuffix() != "two" {
		t.Fatalf("after the update: %+v, %v", r, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetRepository(ctx, &dfpb.GetRepositoryRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy: %v, want NotFound", err)
	}
}
