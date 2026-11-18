package main

import (
	"slices"
	"testing"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Memorystore's IAM auth and Firestore with MongoDB compatibility's IAM
// check evaluate a custom role by its permissions (#1743): a role holding
// redis.clusters.connect lets its members connect to a Redis cluster, not
// to a Valkey instance, and the Firestore payload gives a custom role's
// datastore permissions; a deleted role grants nothing.
func TestPermissionChecksEvaluateCustomRoles(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "cr-project"}); err != nil {
		t.Fatal(err)
	}
	role, err := reg.CreateRole("projects/cr-project", "connector", &adminpb.Role{
		IncludedPermissions: []string{"redis.clusters.connect", "datastore.entities.get"}})
	if err != nil {
		t.Fatal(err)
	}
	const member = "serviceAccount:sa@cr-project.iam.gserviceaccount.com"
	if _, err := reg.SetIAMPolicy("projects/cr-project", &iampb.SetIamPolicyRequest{Resource: "projects/cr-project", Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: role.GetName(), Members: []string{member}}}}}); err != nil {
		t.Fatal(err)
	}
	src := memorystoreIAM(nil, func() *resourcemanager.Registry { return reg })
	if got := src.Members("cr-project", "projects/cr-project/locations/us-central1/clusters/c", "roles/redis.dbConnectionUser"); !slices.Contains(got, member) {
		t.Errorf("Redis connect members = %v, want %s", got, member)
	}
	if got := src.Members("cr-project", "projects/cr-project/locations/us-central1/clusters/c", "roles/memorystore.dbConnectionUser"); slices.Contains(got, member) {
		t.Errorf("Valkey connect members = %v: a role without memorystore.instances.connect grants it", got)
	}
	if _, err := reg.DeleteRole(role.GetName(), nil); err != nil {
		t.Fatal(err)
	}
	if got := src.Members("cr-project", "projects/cr-project/locations/us-central1/clusters/c", "roles/redis.dbConnectionUser"); slices.Contains(got, member) {
		t.Errorf("a deleted role still grants the connection: %v", got)
	}
}
