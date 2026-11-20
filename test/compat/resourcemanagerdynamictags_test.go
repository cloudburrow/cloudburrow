//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// covers: google.cloud.resourcemanager.v3.TagKeys/CreateTagKey, google.cloud.resourcemanager.v3.TagKeys/GetTagKey, google.cloud.resourcemanager.v3.TagKeys/ListTagKeys, google.cloud.resourcemanager.v3.TagKeys/UpdateTagKey, google.cloud.resourcemanager.v3.TagKeys/DeleteTagKey, google.cloud.resourcemanager.v3.TagValues/CreateTagValue, google.cloud.resourcemanager.v3.TagBindings/CreateTagBinding, google.cloud.resourcemanager.v3.TagBindings/ListTagBindings, google.cloud.resourcemanager.v3.TagBindings/DeleteTagBinding, google.cloud.resourcemanager.v3.TagBindings/ListEffectiveTags
//
// TestResourceManagerDynamicTagValues (#2023), through google.golang.org/
// api/cloudresourcemanager/v3, the client whose TagKey has
// AllowedValuesRegex: a key of dynamic values keeps its regex on create,
// get, list and patch; an invalid RE2 regex is INVALID_ARGUMENT; the key
// takes no predefined value (FAILED_PRECONDITION); a value is attached by
// its namespaced name, refused when it does not match the whole regex,
// bound as tagBindings/{parent}/tagKeys/{id} and inherited in
// ListEffectiveTags; one value per key on a resource; the key is not
// deleted while a dynamic binding exists.
func TestResourceManagerDynamicTagValues(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	endpoint := h.Endpoint(EnvResourceManager)
	p, f := tagsProject(t, h, "dy-")
	v3, err := crmv3.NewService(ctx, option.WithEndpoint("http://"+endpoint+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	code := func(err error) int {
		var ge *googleapi.Error
		if errors.As(err, &ge) {
			return ge.Code
		}
		return 0
	}

	short := v1ID(h, "dyn-")
	if _, err := v3.TagKeys.Create(&crmv3.TagKey{Parent: localOrg, ShortName: short, AllowedValuesRegex: "env-(["}).Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Fatalf("Create with an invalid regex = %v; want 400", err)
	}
	op, err := v3.TagKeys.Create(&crmv3.TagKey{Parent: localOrg, ShortName: short, Description: "dynamic",
		AllowedValuesRegex: "env-[a-z]+"}).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("Create = %v, %v", op, err)
	}
	var created crmv3.TagKey
	if err := unmarshalOpResponse(op, &created); err != nil || created.AllowedValuesRegex != "env-[a-z]+" || !strings.HasPrefix(created.Name, "tagKeys/") {
		t.Fatalf("Create's response = %+v, %v", created, err)
	}
	t.Cleanup(func() { _, _ = v3.TagKeys.Delete(created.Name).Do() })
	if got, err := v3.TagKeys.Get(created.Name).Context(ctx).Do(); err != nil || got.AllowedValuesRegex != "env-[a-z]+" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	listed, err := v3.TagKeys.List().Parent(localOrg).Context(ctx).Do()
	found := false
	for _, k := range listed.TagKeys {
		found = found || k.Name == created.Name && k.AllowedValuesRegex == "env-[a-z]+"
	}
	if err != nil || !found {
		t.Fatalf("List = %+v, %v; want %s with its regex", listed, err, created.Name)
	}
	if _, err := v3.TagValues.Create(&crmv3.TagValue{Parent: created.Name, ShortName: "fixed"}).Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("a predefined value under a dynamic key = %v; want 400 FAILED_PRECONDITION", err)
	}

	ns := "100000000001/" + short
	folder := crm + f.GetName()
	if _, err := v3.TagBindings.Create(&crmv3.TagBinding{Parent: folder, TagValueNamespacedName: ns + "/prod"}).Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("a value that does not match = %v; want 400", err)
	}
	if _, err := v3.TagBindings.Create(&crmv3.TagBinding{Parent: folder, TagValueNamespacedName: ns + "/xenv-prod"}).Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("a value matching only part of the regex = %v; want 400", err)
	}
	bop, err := v3.TagBindings.Create(&crmv3.TagBinding{Parent: folder, TagValueNamespacedName: ns + "/env-prod"}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	var b crmv3.TagBinding
	if err := unmarshalOpResponse(bop, &b); err != nil || !strings.HasSuffix(b.Name, "/"+created.Name) || b.TagValue != "" || b.TagValueNamespacedName != ns+"/env-prod" {
		t.Fatalf("binding = %+v, %v; want one named by %s, with no tagValue", b, err, created.Name)
	}
	if _, err := v3.TagBindings.Create(&crmv3.TagBinding{Parent: folder, TagValueNamespacedName: ns + "/env-dev"}).Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("a second value of the key on the folder = %v; want 400 FAILED_PRECONDITION", err)
	}
	if bl, err := v3.TagBindings.List().Parent(folder).Context(ctx).Do(); err != nil || len(bl.TagBindings) != 1 || bl.TagBindings[0].Name != b.Name {
		t.Errorf("ListTagBindings = %+v, %v", bl, err)
	}
	eff, err := v3.EffectiveTags.List().Parent(crm + p.GetName()).Context(ctx).Do()
	inherited := false
	if err == nil {
		for _, e := range eff.EffectiveTags {
			inherited = inherited || e.NamespacedTagValue == ns+"/env-prod" && e.Inherited && e.TagKey == created.Name && e.TagValue == ""
		}
	}
	if !inherited {
		t.Errorf("ListEffectiveTags on the project = %+v, %v; want %s/env-prod inherited", eff, err, ns)
	}

	if _, err := v3.TagKeys.Delete(created.Name).Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("Delete with a dynamic binding = %v; want 400 FAILED_PRECONDITION", err)
	}
	pop, err := v3.TagKeys.Patch(created.Name, &crmv3.TagKey{AllowedValuesRegex: ".*"}).UpdateMask("allowedValuesRegex").Context(ctx).Do()
	var patched crmv3.TagKey
	if err != nil || unmarshalOpResponse(pop, &patched) != nil || patched.AllowedValuesRegex != ".*" || patched.Description != "dynamic" {
		t.Fatalf("Patch the regex = %+v, %v", patched, err)
	}
	if _, err := v3.TagBindings.Create(&crmv3.TagBinding{Parent: crm + p.GetName(), TagValueNamespacedName: ns + "/anything_at.all"}).Context(ctx).Do(); err != nil {
		t.Errorf("a value under .* = %v", err)
	}
	if _, err := v3.TagBindings.Delete(b.Name).Context(ctx).Do(); err != nil {
		t.Fatalf("Delete binding %s = %v", b.Name, err)
	}
	pb, err := v3.TagBindings.List().Parent(crm + p.GetName()).Context(ctx).Do()
	if err != nil || len(pb.TagBindings) != 1 {
		t.Fatalf("project bindings = %+v, %v", pb, err)
	}
	if _, err := v3.TagBindings.Delete(pb.TagBindings[0].Name).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := v3.TagKeys.Delete(created.Name).Context(ctx).Do(); err != nil {
		t.Errorf("Delete once unbound = %v", err)
	}
}

// unmarshalOpResponse decodes a done operation's response.
func unmarshalOpResponse(op *crmv3.Operation, v any) error {
	if op == nil || !op.Done || op.Response == nil {
		return errors.New("operation not done or without a response")
	}
	return json.Unmarshal(op.Response, v)
}
