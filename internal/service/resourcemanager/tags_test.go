package resourcemanager

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// A lien restricting resourcemanager.projects.delete refuses the delete;
// another restriction does not; removing the lien allows it.
func TestLienBlocksProjectDelete(t *testing.T) {
	reg := New(store.NewMemory())
	if _, err := reg.Create(Project{ProjectID: "lien-project"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateLien(Lien{Parent: "projects/lien-project", Restrictions: []string{"resourcemanager.projects.update"}, Reason: "r", Origin: "o"}); err != nil {
		t.Fatal(err)
	}
	l, err := reg.CreateLien(Lien{Parent: V3Name("lien-project"), Restrictions: []string{ProjectDeleteRestriction}, Reason: "r", Origin: "o"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Delete("lien-project"); err == nil || apierror.From(err).Code != codes.FailedPrecondition {
		t.Fatalf("Delete with a lien = %v; want FAILED_PRECONDITION", err)
	}
	if got, _ := reg.Liens("projects/lien-project"); len(got) != 2 {
		t.Errorf("Liens = %v; want 2, by ID or number", got)
	}
	if err := reg.DeleteLien(l.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Delete("lien-project"); err != nil {
		t.Fatalf("Delete after the lien is removed: %v", err)
	}
	if _, err := reg.CreateLien(Lien{Parent: "projects/lien-project", Restrictions: []string{ProjectDeleteRestriction}, Reason: "r", Origin: "o"}); err == nil {
		t.Error("a lien on a project pending deletion was accepted")
	}
}

// Binding names round-trip however often the parent was escaped, and the
// REST paths reach them.
func TestTagBindingNamesAndREST(t *testing.T) {
	reg, srv, _ := startAPI(t)
	if _, err := reg.Create(Project{ProjectID: "tagged-project"}); err != nil {
		t.Fatal(err)
	}
	k, err := reg.CreateTagKey(&rmpb.TagKey{Parent: OrganizationName, ShortName: "env"}, false)
	if err != nil {
		t.Fatal(err)
	}
	v, err := reg.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: "prod"}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: CRMPrefix + "projects/tagged-project", TagValue: v.GetName()}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{b.GetName(), "tagBindings/" + url.PathEscape(url.PathEscape(b.GetParent())) + "/" + v.GetName()} {
		p, val, err := ParseTagBindingName(name)
		if err != nil || p != b.GetParent() || val != v.GetName() {
			t.Errorf("ParseTagBindingName(%q) = %q, %q, %v", name, p, val, err)
		}
	}
	// By number, the bindings made by ID are found.
	if got, err := reg.TagBindings(CRMPrefix + V3Name("tagged-project")); err != nil || len(got) != 1 {
		t.Errorf("TagBindings by number = %v, %v", got, err)
	}

	c, err := crmv3.NewService(context.Background(), option.WithEndpoint("http://"+srv.Addr()+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	if l, err := c.TagBindings.List().Parent(b.GetParent()).Do(); err != nil || len(l.TagBindings) != 1 {
		t.Fatalf("REST TagBindings.List = %+v, %v", l, err)
	}
	if _, err := c.TagBindings.Delete(b.GetName()).Do(); err != nil {
		t.Fatalf("REST TagBindings.Delete(%s): %v", b.GetName(), err)
	}
	if got, _ := reg.TagBindings(b.GetParent()); len(got) != 0 {
		t.Errorf("after delete, bindings = %v", got)
	}
	if e, err := c.EffectiveTags.List().Parent(b.GetParent()).Do(); err != nil || len(e.EffectiveTags) != 0 {
		t.Errorf("REST EffectiveTags.List = %+v, %v", e, err)
	}
	if k2, err := c.TagKeys.GetNamespaced().Name(OrganizationNumber + "/env").Do(); err != nil || k2.Name != k.GetName() {
		t.Errorf("REST TagKeys.GetNamespaced = %+v, %v", k2, err)
	}
	// Tag key and value IAM (#1486): stored, and gone with the tag.
	for _, res := range []string{k.GetName(), v.GetName()} {
		set := &crmv3.SetIamPolicyRequest{Policy: &crmv3.Policy{Bindings: []*crmv3.Binding{{Role: "roles/resourcemanager.tagUser", Members: []string{"user:ada@example.com"}}}}}
		var err error
		var got *crmv3.Policy
		if res == k.GetName() {
			_, err = c.TagKeys.SetIamPolicy(res, set).Do()
			if err == nil {
				got, err = c.TagKeys.GetIamPolicy(res, &crmv3.GetIamPolicyRequest{}).Do()
			}
		} else {
			_, err = c.TagValues.SetIamPolicy(res, set).Do()
			if err == nil {
				got, err = c.TagValues.GetIamPolicy(res, &crmv3.GetIamPolicyRequest{}).Do()
			}
		}
		if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:ada@example.com" {
			t.Errorf("REST IAM of %s = %+v, %v", res, got, err)
		}
	}
	if perms, err := c.TagKeys.TestIamPermissions(k.GetName(), &crmv3.TestIamPermissionsRequest{
		Permissions: []string{"resourcemanager.tagKeys.get", "storage.buckets.get"}}).Do(); err != nil || len(perms.Permissions) != 1 {
		t.Errorf("REST TagKeys.TestIamPermissions = %+v, %v", perms, err)
	}
	if _, err := c.TagValues.Delete(v.GetName()).Do(); err != nil {
		t.Errorf("REST TagValues.Delete: %v", err)
	}
	if _, err := c.TagKeys.Delete(k.GetName()).Do(); err != nil {
		t.Errorf("REST TagKeys.Delete: %v", err)
	}
	if _, err := c.TagKeys.GetIamPolicy(k.GetName(), &crmv3.GetIamPolicyRequest{}).Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("REST TagKeys.GetIamPolicy of a deleted key = %v; want 404", err)
	}
	if _, err := reg.loadIAMLocked(k.GetName()); err != nil {
		t.Errorf("load: %v", err)
	} else if p, _ := reg.loadIAMLocked(k.GetName()); p != nil {
		t.Errorf("a deleted key's policy is kept: %+v", p)
	}
}

// A regional resource of another service (#1350) carries its own bindings
// and inherits its project's; a name with no project here inherits nothing.
func TestEffectiveTagsOfAnotherServicesResource(t *testing.T) {
	reg := New(store.NewMemory())
	if _, err := reg.Create(Project{ProjectID: "run-project"}); err != nil {
		t.Fatal(err)
	}
	mk := func(key, val string) *rmpb.TagValue {
		k, err := reg.CreateTagKey(&rmpb.TagKey{Parent: OrganizationName, ShortName: key}, false)
		if err != nil {
			t.Fatal(err)
		}
		v, err := reg.CreateTagValue(&rmpb.TagValue{Parent: k.GetName(), ShortName: val}, false)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	env, team := mk("env", "prod"), mk("team", "a")
	svc := "//run.googleapis.com/projects/run-project/locations/us-central1/services/s"
	if _, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: CRMPrefix + "projects/run-project", TagValue: env.GetName()}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: svc, TagValue: team.GetName()}, false); err != nil {
		t.Fatal(err)
	}
	got, err := reg.EffectiveTags(svc)
	if err != nil || len(got) != 2 {
		t.Fatalf("EffectiveTags = %v, %v; want 2", got, err)
	}
	if got[0].GetTagValue() != team.GetName() || got[0].GetInherited() || got[1].GetTagValue() != env.GetName() || !got[1].GetInherited() {
		t.Errorf("EffectiveTags = %v; want team direct, env inherited", got)
	}
	if got, err := reg.EffectiveTags("//storage.googleapis.com/projects/_/buckets/b"); err != nil || len(got) != 0 {
		t.Errorf("bucket EffectiveTags = %v, %v; want none", got, err)
	}
}
