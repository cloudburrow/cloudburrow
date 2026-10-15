//go:build compat

package compat

import (
	"strings"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
)

// regionalTagValues creates two organization tag keys, each with one
// value, for a regional resource and its project to carry.
func regionalTagValues(t *testing.T, h *Harness, c tagClients) (env, team *rmpb.TagValue) {
	t.Helper()
	ctx := h.Context()
	mk := func(prefix string) *rmpb.TagValue {
		kop, err := c.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: v1ID(h, prefix)}})
		if err != nil {
			t.Fatal(err)
		}
		k, err := kop.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		vop, err := c.values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: k.GetName(), ShortName: "v"}})
		if err != nil {
			t.Fatal(err)
		}
		v, err := vop.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if op, err := c.values.DeleteTagValue(ctx, &rmpb.DeleteTagValueRequest{Name: v.GetName()}); err == nil {
				_, _ = op.Wait(ctx)
			}
			if op, err := c.keys.DeleteTagKey(ctx, &rmpb.DeleteTagKeyRequest{Name: k.GetName()}); err == nil {
				_, _ = op.Wait(ctx)
			}
		})
		return v
	}
	return mk("renv-"), mk("rteam-")
}

// covers: google.cloud.resourcemanager.v3.TagBindings/CreateTagBinding, google.cloud.resourcemanager.v3.TagBindings/ListTagBindings, google.cloud.resourcemanager.v3.TagBindings/ListEffectiveTags, google.cloud.resourcemanager.v3.TagBindings/DeleteTagBinding
//
// TestResourceManagerRegionalTagBindings (#1350): the official client binds
// a value to a regional Cloud Run service by its full resource name, lists
// the binding, and reads the service's effective tags: its own value, and
// its project's inherited, as Google documents tag inheritance.
func TestResourceManagerRegionalTagBindings(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := newTagClients(t, h)
	p, _ := tagsProject(t, h, "rt-")
	env, team := regionalTagValues(t, h, c)
	svc := "//run.googleapis.com/" + p.GetName() + "/locations/us-central1/services/tagged"

	bind := func(parent, value string) *rmpb.TagBinding {
		op, err := c.bindings.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{Parent: parent, TagValue: value}})
		if err != nil {
			t.Fatalf("CreateTagBinding(%s): %v", parent, err)
		}
		b, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateTagBinding(%s): %v", parent, err)
		}
		t.Cleanup(func() {
			if op, err := c.bindings.DeleteTagBinding(ctx, &rmpb.DeleteTagBindingRequest{Name: b.GetName()}); err == nil {
				_ = op.Wait(ctx)
			}
		})
		return b
	}
	bind(crm+p.GetName(), env.GetName())
	b := bind(svc, team.GetName())
	if b.GetParent() != svc || b.GetTagValue() != team.GetName() {
		t.Errorf("binding = %v", b)
	}

	var bound []string
	for it := c.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: svc}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		bound = append(bound, x.GetTagValue())
	}
	if len(bound) != 1 || bound[0] != team.GetName() {
		t.Errorf("ListTagBindings(%s) = %v; want only %s", svc, bound, team.GetName())
	}

	effective := map[string]bool{}
	for it := c.bindings.ListEffectiveTags(ctx, &rmpb.ListEffectiveTagsRequest{Parent: svc}); ; {
		e, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		effective[e.GetTagValue()] = e.GetInherited()
	}
	if inh, ok := effective[team.GetName()]; !ok || inh {
		t.Errorf("effective tags %v: want %s direct", effective, team.GetName())
	}
	if inh, ok := effective[env.GetName()]; !ok || !inh {
		t.Errorf("effective tags %v: want %s inherited from %s", effective, env.GetName(), p.GetName())
	}

	op, err := c.bindings.DeleteTagBinding(ctx, &rmpb.DeleteTagBindingRequest{Name: b.GetName()})
	if err == nil {
		err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("DeleteTagBinding(%s): %v", b.GetName(), err)
	}
}

// TestGcloudRegionalTagBindings (#1350): the pinned gcloud, configured by
// `cloudburrow gcloud-setup` alone, creates, lists and deletes a tag binding
// on a regional Cloud Run service and lists its effective tags. Given
// --location, gcloud calls {location}-{host} of the cloudresourcemanager
// override, which on loopback is cloudresourcemanager.localhost, so the
// call still reaches the instance; every other host is refused.
func TestGcloudRegionalTagBindings(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := newTagClients(t, h)
	g := newGcloudSession(t, h)
	env, team := regionalTagValues(t, h, c)
	project := "projects/" + g.project
	svc := "//run.googleapis.com/" + project + "/locations/us-central1/services/gcloud-tagged"
	loc := "--location=us-central1"

	g.must("resource-manager", "tags", "bindings", "create", "--tag-value="+env.GetName(), "--parent="+crm+project)
	t.Cleanup(func() {
		_, _ = g.run(nil, "resource-manager", "tags", "bindings", "delete", "--tag-value="+env.GetName(), "--parent="+crm+project)
	})
	g.must("resource-manager", "tags", "bindings", "create", "--tag-value="+team.GetName(), "--parent="+svc, loc)
	t.Cleanup(func() {
		_, _ = g.run(nil, "resource-manager", "tags", "bindings", "delete", "--tag-value="+team.GetName(), "--parent="+svc, loc)
	})
	if out := g.must("resource-manager", "tags", "bindings", "list", "--parent="+svc, loc, "--format=value(tagValue)"); strings.TrimSpace(out) != team.GetName() {
		t.Errorf("bindings list = %q; want %s", out, team.GetName())
	}
	out := g.must("resource-manager", "tags", "bindings", "list", "--parent="+svc, loc, "--effective", "--format=value(tagValue,inherited)")
	if !strings.Contains(out, team.GetName()) || !strings.Contains(out, env.GetName()+"\tTrue") {
		t.Errorf("bindings list --effective = %q; want %s and %s inherited", out, team.GetName(), env.GetName())
	}
	g.must("resource-manager", "tags", "bindings", "delete", "--tag-value="+team.GetName(), "--parent="+svc, loc)
	if out := g.must("resource-manager", "tags", "bindings", "list", "--parent="+svc, loc, "--format=value(tagValue)"); strings.TrimSpace(out) != "" {
		t.Errorf("bindings list after delete = %q; want none", out)
	}
}
