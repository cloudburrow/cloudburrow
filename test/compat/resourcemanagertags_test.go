//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const crm = "//cloudresourcemanager.googleapis.com/"

type tagClients struct {
	keys     *resourcemanager.TagKeysClient
	values   *resourcemanager.TagValuesClient
	bindings *resourcemanager.TagBindingsClient
	holds    *resourcemanager.TagHoldsClient
}

func newTagClients(t *testing.T, h *Harness) tagClients {
	t.Helper()
	ctx := h.Context()
	var c tagClients
	var err error
	if c.keys, err = resourcemanager.NewTagKeysClient(ctx, rmOptions(h)...); err != nil {
		t.Fatal(err)
	}
	if c.values, err = resourcemanager.NewTagValuesClient(ctx, rmOptions(h)...); err != nil {
		t.Fatal(err)
	}
	if c.bindings, err = resourcemanager.NewTagBindingsClient(ctx, rmOptions(h)...); err != nil {
		t.Fatal(err)
	}
	if c.holds, err = resourcemanager.NewTagHoldsClient(ctx, rmOptions(h)...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.keys.Close(); _ = c.values.Close(); _ = c.bindings.Close(); _ = c.holds.Close() })
	return c
}

// tagsProject creates a project under a new folder under the organization,
// for bindings to be inherited through.
func tagsProject(t *testing.T, h *Harness, prefix string) (*rmpb.Project, *rmpb.Folder) {
	t.Helper()
	ctx := h.Context()
	fc, _, pc := foldersClients(t, h)
	fop, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, prefix)}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := fop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: v1ID(h, prefix), Parent: f.GetName()}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: p.GetName()})
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()})
	})
	return p, f
}

// covers: google.cloud.resourcemanager.v3.TagKeys/CreateTagKey, google.cloud.resourcemanager.v3.TagKeys/GetTagKey, google.cloud.resourcemanager.v3.TagKeys/GetNamespacedTagKey, google.cloud.resourcemanager.v3.TagKeys/ListTagKeys, google.cloud.resourcemanager.v3.TagKeys/UpdateTagKey, google.cloud.resourcemanager.v3.TagKeys/DeleteTagKey, google.cloud.resourcemanager.v3.TagValues/CreateTagValue, google.cloud.resourcemanager.v3.TagValues/GetTagValue, google.cloud.resourcemanager.v3.TagValues/GetNamespacedTagValue, google.cloud.resourcemanager.v3.TagValues/ListTagValues, google.cloud.resourcemanager.v3.TagValues/UpdateTagValue, google.cloud.resourcemanager.v3.TagValues/DeleteTagValue, google.cloud.resourcemanager.v3.TagBindings/CreateTagBinding, google.cloud.resourcemanager.v3.TagBindings/ListTagBindings, google.cloud.resourcemanager.v3.TagBindings/DeleteTagBinding, google.cloud.resourcemanager.v3.TagBindings/ListEffectiveTags, google.cloud.resourcemanager.v3.TagHolds/CreateTagHold, google.cloud.resourcemanager.v3.TagHolds/ListTagHolds, google.cloud.resourcemanager.v3.TagHolds/DeleteTagHold
//
// TestResourceManagerV3Tags (#1295), through cloud.google.com/go/
// resourcemanager/apiv3 TagKeysClient, TagValuesClient, TagBindingsClient
// and TagHoldsClient: an organization key and a project key with namespaced
// names; values; Get, GetNamespaced, List and Update (description, with a
// stale etag ABORTED); a binding on a folder inherited by the project in it,
// and a project's own binding, in ListEffectiveTags; one value per key on a
// resource; a key with values and a value with bindings or holds are not
// deleted; tag IAM is UNIMPLEMENTED citing ADR-0006.
func TestResourceManagerV3Tags(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := newTagClients(t, h)
	p, f := tagsProject(t, h, "tg-")
	short := v1ID(h, "env-")

	kop, err := c.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: short, Description: "environment"}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := kop.Wait(ctx)
	if err != nil || !strings.HasPrefix(key.GetName(), "tagKeys/") || key.GetNamespacedName() != "100000000001/"+short || key.GetEtag() == "" {
		t.Fatalf("CreateTagKey = %v, %v", key, err)
	}
	if _, err := c.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: short}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("a second key %s = %v; want AlreadyExists", short, err)
	}
	pkop, err := c.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: p.GetName(), ShortName: "team"}})
	if err != nil {
		t.Fatal(err)
	}
	pkey, err := pkop.Wait(ctx)
	if err != nil || pkey.GetNamespacedName() != p.GetProjectId()+"/team" {
		t.Fatalf("project CreateTagKey = %v, %v", pkey, err)
	}
	if got, err := c.keys.GetTagKey(ctx, &rmpb.GetTagKeyRequest{Name: key.GetName()}); err != nil || got.GetShortName() != short {
		t.Errorf("GetTagKey = %v, %v", got, err)
	}
	if got, err := c.keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: key.GetNamespacedName()}); err != nil || got.GetName() != key.GetName() {
		t.Errorf("GetNamespacedTagKey = %v, %v", got, err)
	}
	listed := map[string]bool{}
	for it := c.keys.ListTagKeys(ctx, &rmpb.ListTagKeysRequest{Parent: p.GetName()}); ; {
		k, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		listed[k.GetName()] = true
	}
	if !listed[pkey.GetName()] || listed[key.GetName()] {
		t.Errorf("ListTagKeys(%s) = %v; want only the project's key", p.GetName(), listed)
	}
	uop, err := c.keys.UpdateTagKey(ctx, &rmpb.UpdateTagKeyRequest{TagKey: &rmpb.TagKey{Name: key.GetName(), Description: "deployment environment", Etag: key.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatal(err)
	}
	if up, err := uop.Wait(ctx); err != nil || up.GetDescription() != "deployment environment" || up.GetEtag() == key.GetEtag() {
		t.Errorf("UpdateTagKey = %v, %v", up, err)
	}
	if _, err := c.keys.UpdateTagKey(ctx, &rmpb.UpdateTagKeyRequest{TagKey: &rmpb.TagKey{Name: key.GetName(), Description: "x", Etag: key.GetEtag()}}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateTagKey with a stale etag = %v; want Aborted", err)
	}

	value := func(parent *rmpb.TagKey, short string) *rmpb.TagValue {
		t.Helper()
		op, err := c.values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: parent.GetName(), ShortName: short}})
		if err != nil {
			t.Fatal(err)
		}
		v, err := op.Wait(ctx)
		if err != nil || v.GetNamespacedName() != parent.GetNamespacedName()+"/"+short || !strings.HasPrefix(v.GetName(), "tagValues/") {
			t.Fatalf("CreateTagValue = %v, %v", v, err)
		}
		return v
	}
	prod, dev, red := value(key, "prod"), value(key, "dev"), value(pkey, "red")
	if got, err := c.values.GetNamespacedTagValue(ctx, &rmpb.GetNamespacedTagValueRequest{Name: prod.GetNamespacedName()}); err != nil || got.GetName() != prod.GetName() {
		t.Errorf("GetNamespacedTagValue = %v, %v", got, err)
	}
	if got, err := c.values.GetTagValue(ctx, &rmpb.GetTagValueRequest{Name: dev.GetName()}); err != nil || got.GetShortName() != "dev" {
		t.Errorf("GetTagValue = %v, %v", got, err)
	}
	n := 0
	for it := c.values.ListTagValues(ctx, &rmpb.ListTagValuesRequest{Parent: key.GetName(), PageSize: 1}); ; n++ {
		if _, err := it.Next(); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if n != 2 {
		t.Errorf("ListTagValues(%s) paged %d values; want 2", key.GetName(), n)
	}
	vop, err := c.values.UpdateTagValue(ctx, &rmpb.UpdateTagValueRequest{TagValue: &rmpb.TagValue{Name: dev.GetName(), Description: "development"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatal(err)
	}
	if up, err := vop.Wait(ctx); err != nil || up.GetDescription() != "development" {
		t.Errorf("UpdateTagValue = %v, %v", up, err)
	}

	bind := func(parent string, v *rmpb.TagValue) *rmpb.TagBinding {
		t.Helper()
		op, err := c.bindings.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{Parent: parent, TagValue: v.GetName()}})
		if err != nil {
			t.Fatalf("CreateTagBinding(%s, %s): %v", parent, v.GetName(), err)
		}
		b, err := op.Wait(ctx)
		if err != nil || b.GetTagValueNamespacedName() != v.GetNamespacedName() || !strings.HasPrefix(b.GetName(), "tagBindings/") {
			t.Fatalf("CreateTagBinding = %v, %v", b, err)
		}
		return b
	}
	folderRes, projectRes := crm+f.GetName(), crm+p.GetName()
	fb := bind(folderRes, prod)
	// The project's own value of key replaces the folder's; red is its own.
	pb := bind(projectRes, dev)
	// By namespaced name.
	op, err := c.bindings.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{Parent: projectRes, TagValueNamespacedName: red.GetNamespacedName()}})
	if err != nil {
		t.Fatal(err)
	}
	rb, err := op.Wait(ctx)
	if err != nil || rb.GetTagValue() != red.GetName() {
		t.Fatalf("CreateTagBinding(namespaced) = %v, %v", rb, err)
	}
	if _, err := c.bindings.CreateTagBinding(ctx, &rmpb.CreateTagBindingRequest{TagBinding: &rmpb.TagBinding{Parent: projectRes, TagValue: prod.GetName()}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a second value of one key on a project = %v; want FailedPrecondition", err)
	}
	var onProject []string
	for it := c.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: projectRes}); ; {
		b, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		onProject = append(onProject, b.GetTagValue())
	}
	if len(onProject) != 2 {
		t.Errorf("ListTagBindings(project) = %v; want %s and %s", onProject, dev.GetName(), red.GetName())
	}
	effective := func(parent string) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		for it := c.bindings.ListEffectiveTags(ctx, &rmpb.ListEffectiveTagsRequest{Parent: parent}); ; {
			e, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out[e.GetNamespacedTagValue()] = e.GetInherited()
		}
	}
	if got := effective(projectRes); len(got) != 2 || got[dev.GetNamespacedName()] || got[red.GetNamespacedName()] {
		t.Errorf("ListEffectiveTags(project) = %v; want %s and %s, neither inherited", got, dev.GetNamespacedName(), red.GetNamespacedName())
	}
	if _, err := c.bindings.DeleteTagBinding(ctx, &rmpb.DeleteTagBindingRequest{Name: pb.GetName()}); err != nil {
		t.Fatalf("DeleteTagBinding(%s): %v", pb.GetName(), err)
	}
	if got := effective(projectRes); len(got) != 2 || !got[prod.GetNamespacedName()] {
		t.Errorf("ListEffectiveTags(project) without its own value = %v; want %s inherited from %s", got, prod.GetNamespacedName(), f.GetName())
	}

	if _, err := c.keys.DeleteTagKey(ctx, &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteTagKey with values = %v; want FailedPrecondition", err)
	}
	if _, err := c.values.DeleteTagValue(ctx, &rmpb.DeleteTagValueRequest{Name: prod.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteTagValue while bound = %v; want FailedPrecondition", err)
	}

	hop, err := c.holds.CreateTagHold(ctx, &rmpb.CreateTagHoldRequest{Parent: dev.GetName(), TagHold: &rmpb.TagHold{Holder: "//compute.googleapis.com/projects/x/global/networks/n", Origin: "compat"}})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := hop.Wait(ctx)
	if err != nil || !strings.HasPrefix(hold.GetName(), dev.GetName()+"/tagHolds/") {
		t.Fatalf("CreateTagHold = %v, %v", hold, err)
	}
	if h, err := c.holds.ListTagHolds(ctx, &rmpb.ListTagHoldsRequest{Parent: dev.GetName(), Filter: `origin="compat"`}).Next(); err != nil || h.GetName() != hold.GetName() {
		t.Errorf("ListTagHolds = %v, %v", h, err)
	}
	if _, err := c.values.DeleteTagValue(ctx, &rmpb.DeleteTagValueRequest{Name: dev.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteTagValue while held = %v; want FailedPrecondition", err)
	}
	dhop, err := c.holds.DeleteTagHold(ctx, &rmpb.DeleteTagHoldRequest{Name: hold.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := dhop.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	for _, b := range []*rmpb.TagBinding{fb, rb} {
		dop, err := c.bindings.DeleteTagBinding(ctx, &rmpb.DeleteTagBindingRequest{Name: b.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if err := dop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []*rmpb.TagValue{prod, dev, red} {
		dop, err := c.values.DeleteTagValue(ctx, &rmpb.DeleteTagValueRequest{Name: v.GetName()})
		if err != nil {
			t.Fatalf("DeleteTagValue(%s): %v", v.GetName(), err)
		}
		if _, err := dop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []*rmpb.TagKey{key, pkey} {
		dop, err := c.keys.DeleteTagKey(ctx, &rmpb.DeleteTagKeyRequest{Name: k.GetName()})
		if err != nil {
			t.Fatalf("DeleteTagKey(%s): %v", k.GetName(), err)
		}
		if _, err := dop.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.keys.GetTagKey(ctx, &rmpb.GetTagKeyRequest{Name: key.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetTagKey after delete = %v; want NotFound", err)
	}
}

// covers: cloudresourcemanager.liens.create, cloudresourcemanager.liens.get, cloudresourcemanager.liens.list, cloudresourcemanager.liens.delete
//
// TestResourceManagerLiens (#1295): Liens, which Google publishes over JSON
// only (cloud.google.com/go/resourcemanager/apiv3 has no LiensClient), through
// google.golang.org/api/cloudresourcemanager/v3 and v1: create, get, list and
// delete on a project. A lien restricting resourcemanager.projects.delete
// makes DeleteProject FAILED_PRECONDITION through v3 gRPC and v1, and the
// delete succeeds once the lien is removed.
func TestResourceManagerLiens(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	_, _, pc := foldersClients(t, h)
	id := v1ID(h, "ln-")
	pop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v3, err := crmv3.NewService(ctx, option.WithEndpoint("http://"+h.Endpoint(EnvResourceManager)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	l, err := v3.Liens.Create(&crmv3.Lien{Parent: p.GetName(), Restrictions: []string{"resourcemanager.projects.delete"},
		Reason: "compat", Origin: "cloudburrow-compat"}).Context(ctx).Do()
	if err != nil || !strings.HasPrefix(l.Name, "liens/") || l.CreateTime == "" {
		t.Fatalf("Liens.Create = %+v, %v", l, err)
	}
	t.Cleanup(func() {
		_, _ = v3.Liens.Delete(l.Name).Context(context.Background()).Do()
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: p.GetName()})
	})
	if got, err := v3.Liens.Get(l.Name).Context(ctx).Do(); err != nil || got.Reason != "compat" || got.Parent != p.GetName() {
		t.Errorf("Liens.Get = %+v, %v", got, err)
	}
	// v1, as Terraform calls it, by project ID.
	v1 := rmV1(t, h)
	list, err := v1.Liens.List().Parent("projects/" + id).Context(ctx).Do()
	if err != nil || len(list.Liens) != 1 || list.Liens[0].Name != l.Name {
		t.Errorf("v1 Liens.List = %+v, %v", list, err)
	}
	if _, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: p.GetName()}); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "lien") {
		t.Errorf("DeleteProject with a lien = %v; want FailedPrecondition naming the lien", err)
	}
	var gerr *googleapi.Error
	if _, err := v1.Projects.Delete(id).Context(ctx).Do(); err == nil || !asGoogleAPIError(err, &gerr) || gerr.Code != http.StatusBadRequest {
		t.Errorf("v1 Projects.Delete with a lien = %v; want 400 FAILED_PRECONDITION", err)
	}
	if _, err := v3.Liens.Delete(l.Name).Context(ctx).Do(); err != nil {
		t.Fatalf("Liens.Delete: %v", err)
	}
	if _, err := v3.Liens.Get(l.Name).Context(ctx).Do(); !asGoogleAPIError(err, &gerr) || gerr.Code != http.StatusNotFound {
		t.Errorf("Liens.Get after delete = %v; want 404", err)
	}
	if _, err := v1.Liens.List().Parent("projects/" + strconv.FormatInt(projectNumberOf(t, v1, id), 10)).Context(ctx).Do(); err != nil {
		t.Errorf("v1 Liens.List by number: %v", err)
	}
	dop, err := pc.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: p.GetName()})
	if err != nil {
		t.Fatalf("DeleteProject after the lien is removed: %v", err)
	}
	if got, err := dop.Wait(ctx); err != nil || got.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("DeleteProject = %v, %v", got, err)
	}
}

func asGoogleAPIError(err error, out **googleapi.Error) bool {
	e, ok := err.(*googleapi.Error)
	if ok {
		*out = e
	}
	return ok
}

func projectNumberOf(t *testing.T, v1 *crmv1.Service, id string) int64 {
	t.Helper()
	p, err := v1.Projects.Get(id).Do()
	if err != nil {
		t.Fatal(err)
	}
	return p.ProjectNumber
}

// TestTerraformTagsAndLien (#1295): a google_tags_tag_key under the
// organization, a google_tags_tag_value, a google_tags_tag_binding on a
// google_project and a google_resource_manager_lien on it, through
// `cloudburrow terraform` (tags_custom_endpoint and
// resource_manager_custom_endpoint point here). They apply and plan clean; a
// changed key description applies in place and plans clean; destroy removes
// them all, the lien before the project, which it would otherwise protect.
func TestTerraformTagsAndLien(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	ctx := h.Context()
	c := newTagClients(t, h)
	short := v1ID(h, "tfk-")
	id := v1ID(h, "tft-")
	module := func(desc string) string {
		return fmt.Sprintf(`
resource "google_project" "p" {
  project_id      = %q
  name            = "Tagged"
  deletion_policy = "DELETE"
}

resource "google_tags_tag_key" "k" {
  parent      = %q
  short_name  = %q
  description = %q
}

resource "google_tags_tag_value" "v" {
  parent      = google_tags_tag_key.k.id
  short_name  = "prod"
  description = "production"
}

resource "google_tags_tag_binding" "b" {
  parent    = "//cloudresourcemanager.googleapis.com/projects/${google_project.p.number}"
  tag_value = google_tags_tag_value.v.id
}

resource "google_resource_manager_lien" "l" {
  parent       = "projects/${google_project.p.number}"
  restrictions = ["resourcemanager.projects.delete"]
  origin       = "terraform"
  reason       = "keep it"
}
`, id, localOrg, short, desc)
	}
	m.write(module("env"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	key, err := c.keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: "100000000001/" + short})
	if err != nil || key.GetDescription() != "env" {
		t.Fatalf("after apply the key reads %v, %v", key, err)
	}
	num := projectNumberOf(t, rmV1(t, h), id)
	b, err := c.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: fmt.Sprintf("%sprojects/%d", crm, num)}).Next()
	if err != nil || b.GetTagValueNamespacedName() != key.GetNamespacedName()+"/prod" {
		t.Errorf("after apply the project's binding is %v, %v", b, err)
	}
	if l, err := rmV1(t, h).Liens.List().Parent(fmt.Sprintf("projects/%d", num)).Context(ctx).Do(); err != nil || len(l.Liens) != 1 {
		t.Errorf("after apply the project's liens are %+v, %v", l, err)
	}
	m.planClean()

	m.write(module("environment"))
	m.apply()
	if got, err := c.keys.GetTagKey(ctx, &rmpb.GetTagKeyRequest{Name: key.GetName()}); err != nil || got.GetDescription() != "environment" {
		t.Errorf("after the update the key reads %v, %v; want the same key, described anew", got, err)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.keys.GetTagKey(ctx, &rmpb.GetTagKeyRequest{Name: key.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("after destroy the key reads %v; want NotFound", err)
	}
	if p, err := rmV1(t, h).Projects.Get(id).Context(ctx).Do(); err != nil || p.LifecycleState != "DELETE_REQUESTED" {
		t.Errorf("after destroy the project reads %+v, %v; want DELETE_REQUESTED", p, err)
	}
}
