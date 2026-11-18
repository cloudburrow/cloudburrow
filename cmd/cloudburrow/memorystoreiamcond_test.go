package main

import (
	"slices"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/genproto/googleapis/type/expr"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Memorystore's IAM auth evaluates a project binding's condition per
// cluster or instance (#1745): request.time and the resource's effective
// tags (its own and the project's) are evaluated; a resource.* part grants
// nothing, as Google's Memorystore recognizes no resource attributes.
func TestMemorystoreIAMConditionsPerInstance(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.EnsureExists("p1"); err != nil {
		t.Fatal(err)
	}
	tk, err := reg.CreateTagKey(&rmpb.TagKey{Parent: resourcemanager.OrganizationName, ShortName: "env"}, false)
	if err != nil {
		t.Fatal(err)
	}
	tv, err := reg.CreateTagValue(&rmpb.TagValue{Parent: tk.GetName(), ShortName: "prod"}, false)
	if err != nil {
		t.Fatal(err)
	}
	prod := "projects/p1/locations/us-central1/instances/prod"
	if _, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: "//memorystore.googleapis.com/" + prod, TagValue: tv.GetName()}, false); err != nil {
		t.Fatal(err)
	}
	cond := func(title, e string) *expr.Expr { return &expr.Expr{Title: title, Expression: e} }
	role := "roles/memorystore.dbConnectionUser"
	if _, err := reg.SetIAMPolicy("projects/p1", &iampb.SetIamPolicyRequest{Resource: "projects/p1", Policy: &iampb.Policy{Version: 3, Bindings: []*iampb.Binding{
		{Role: role, Members: []string{"user:tagged@example.com"}, Condition: cond("prod", `resource.matchTag("`+tk.GetNamespacedName()+`", "prod")`)},
		{Role: role, Members: []string{"user:named@example.com"}, Condition: cond("by name", `resource.name == "`+prod+`"`)},
		{Role: role, Members: []string{"user:timed@example.com"}, Condition: cond("until 2099", `request.time < timestamp("2099-01-01T00:00:00Z")`)},
	}}}); err != nil {
		t.Fatal(err)
	}
	src := memorystoreIAM(nil, func() *resourcemanager.Registry { return reg })
	got := src.Members("p1", prod, role)
	if !slices.Contains(got, "user:tagged@example.com") || !slices.Contains(got, "user:timed@example.com") || slices.Contains(got, "user:named@example.com") {
		t.Errorf("members on the tagged instance = %v; want the tag and time conditions, not resource.name", got)
	}
	other := src.Members("p1", "projects/p1/locations/us-central1/instances/dev", role)
	if slices.Contains(other, "user:tagged@example.com") || !slices.Contains(other, "user:timed@example.com") {
		t.Errorf("members on an untagged instance = %v", other)
	}
}
