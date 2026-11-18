//go:build compat

package compat

import (
	"fmt"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
)

// TestTerraformTagsLocationTagBinding (#1578): a
// google_tags_location_tag_binding binds a tag value to a regional Cloud Run
// service's full resource name through `cloudburrow terraform`
// (tags_location_custom_endpoint points here) with egress blocked; the
// official TagBindings client lists it, the plan is clean, and destroy
// removes it.
func TestTerraformTagsLocationTagBinding(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := newTagClients(t, h)
	m := newTFModule(t)
	p, _ := tagsProject(t, h, "tflt-")
	_, team := regionalTagValues(t, h, c)
	svc := "//run.googleapis.com/" + p.GetName() + "/locations/us-central1/services/tf-tagged"
	list := func() []*rmpb.TagBinding {
		t.Helper()
		var out []*rmpb.TagBinding
		for it := c.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: svc}); ; {
			b, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, b)
		}
	}
	m.write(fmt.Sprintf(`
resource "google_tags_location_tag_binding" "b" {
  parent    = %q
  tag_value = %q
  location  = "us-central1"
}

output "name" {
  value = google_tags_location_tag_binding.b.name
}
`, svc, team.GetName()))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	got := list()
	if len(got) != 1 || got[0].GetTagValue() != team.GetName() || got[0].GetParent() != svc {
		t.Fatalf("after apply the service's bindings are %v", got)
	}
	if name := m.must("output", "-raw", "name"); name != got[0].GetName() {
		t.Errorf("the resource's name is %q; the client's %q", name, got[0].GetName())
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if got := list(); len(got) != 0 {
		t.Errorf("after destroy the service's bindings are %v", got)
	}
}
