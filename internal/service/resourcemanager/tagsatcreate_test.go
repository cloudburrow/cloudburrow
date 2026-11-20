package resourcemanager

import (
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Tags given at a resource's creation (#1961) name keys and values by ID,
// by namespaced name or, for a value, by short name; an unknown key or
// value, a value of another key and a key named twice are INVALID_ARGUMENT
// with nothing bound; validateOnly binds nothing; the bindings are
// effective tags, and UnbindTags removes them.
func TestBindTagsAtCreation(t *testing.T) {
	reg := New(store.NewMemory())
	if _, err := reg.Create(Project{ProjectID: "tag-project"}); err != nil {
		t.Fatal(err)
	}
	mk := func(key, value string) (*rmpb.TagKey, *rmpb.TagValue) {
		k, err := reg.CreateTagKey(&rmpb.TagKey{Parent: OrganizationName, ShortName: key}, false)
		if err != nil {
			t.Fatal(err)
		}
		v, err := reg.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: value}, false)
		if err != nil {
			t.Fatal(err)
		}
		return k, v
	}
	env, prod := mk("env", "prod")
	cc, mkt := mk("cost", "marketing")
	_, team := mk("team", "core")
	const res = "//secretmanager.googleapis.com/projects/tag-project/secrets/s"
	for name, tags := range map[string]map[string]string{
		"unknown key":   {"tagKeys/999": "x"},
		"unknown value": {env.GetName(): "staging"},
		"another key's": {env.GetName(): mkt.GetName()},
		"key twice":     {env.GetName(): prod.GetName(), env.GetNamespacedName(): "prod"},
	} {
		if err := reg.BindTags(res, tags, false); err == nil || apierror.From(err).Code != codes.InvalidArgument {
			t.Errorf("%s: BindTags = %v; want INVALID_ARGUMENT", name, err)
		}
	}
	tags := map[string]string{env.GetName(): prod.GetName(), cc.GetNamespacedName(): "marketing", team.GetParent(): team.GetNamespacedName()}
	if err := reg.BindTags(res, tags, true); err != nil {
		t.Fatalf("BindTags validateOnly: %v", err)
	}
	if b, _ := reg.TagBindings(res); len(b) != 0 {
		t.Fatalf("validateOnly bound %v", b)
	}
	if err := reg.BindTags(res, tags, false); err != nil {
		t.Fatalf("BindTags: %v", err)
	}
	eff, err := reg.EffectiveTags(res)
	if err != nil || len(eff) != 3 {
		t.Fatalf("EffectiveTags = %v, %v; want the 3 values bound", eff, err)
	}
	if err := reg.UnbindTags(res); err != nil {
		t.Fatal(err)
	}
	if b, _ := reg.TagBindings(res); len(b) != 0 {
		t.Errorf("after UnbindTags the bindings are %v", b)
	}
}
