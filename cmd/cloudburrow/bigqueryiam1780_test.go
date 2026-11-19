package main

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	adminpb "cloud.google.com/go/iam/admin/apiv1/adminpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestBigQueryIAMPayload1780 (#1780): the front is given a custom role no
// hierarchy policy names (a table's own policy may), a project's effective
// tags, the tags bound to a dataset (its project named by number or ID),
// and every tag value by its namespaced name; the console's dataset page
// binds and unbinds a tag.
func TestBigQueryIAMPayload1780(t *testing.T) {
	reg := resourcemanager.New(store.NewMemory())
	p, err := reg.Create(resourcemanager.Project{ProjectID: "tag-proj", Parent: resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateRole("projects/tag-proj", "tableReader", &adminpb.Role{Title: "r", Stage: adminpb.Role_GA,
		IncludedPermissions: []string{"bigquery.tables.getData", "storage.objects.get"}}); err != nil {
		t.Fatal(err)
	}
	key, err := reg.CreateTagKey(&rmpb.TagKey{Parent: "projects/tag-proj", ShortName: "env"}, false)
	if err != nil {
		t.Fatal(err)
	}
	prod, err := reg.CreateTagValue(&rmpb.TagValue{Parent: key.GetName(), ShortName: "prod"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: resourcemanager.CRMPrefix + "projects/tag-proj", TagValue: prod.GetName()}, false); err != nil {
		t.Fatal(err)
	}
	if p.Number == 0 {
		t.Fatal("the project has no number")
	}
	ds := bigqueryfront.BigQueryTagPrefix + "projects/" + strconv.FormatInt(p.Number, 10) + "/datasets/d"
	if _, err := reg.CreateTagBinding(&rmpb.TagBinding{Parent: ds, TagValue: prod.GetName()}, false); err != nil {
		t.Fatal(err)
	}
	b := &bigqueryIAM{creds: &metadata.Credentials{}, registry: func() *resourcemanager.Registry { return reg }}
	m := b.payload(time.Now())
	if got := m.CustomRoles["projects/tag-proj/roles/tableReader"]; !slices.Equal(got, []string{"bigquery.tables.getData"}) {
		t.Errorf("custom role = %v, want [bigquery.tables.getData]", got)
	}
	want := key.GetNamespacedName()
	if tags := m.Tags["projects/tag-proj"]; len(tags) != 1 || tags[0].NamespacedKey != want || tags[0].ShortValue != "prod" {
		t.Errorf("the project's tags = %+v", tags)
	}
	if tags := m.Tags[bigqueryfront.BigQueryTagPrefix+"projects/tag-proj/datasets/d"]; len(tags) != 1 || tags[0].ValueID != prod.GetName() {
		t.Errorf("the dataset's tags = %+v (all: %v)", tags, m.Tags)
	}
	if v, ok := m.TagValues[prod.GetNamespacedName()]; !ok || v.KeyID != key.GetName() {
		t.Errorf("tag values = %v", m.TagValues)
	}

	// The console's dataset page.
	cp := bigqueryProvider{project: "tag-proj", registry: reg}
	path := []string{"other"}
	acts := cp.tagActionsAt(path)
	if len(acts) != 1 || acts[0].ID != tagBind {
		t.Fatalf("actions = %+v, want Bind tag", acts)
	}
	if ok, err := cp.tagActAt(context.Background(), path, tagBind, map[string]string{"value": prod.GetNamespacedName()}); !ok || err != nil {
		t.Fatalf("Bind tag: %v, %v", ok, err)
	}
	bs, err := reg.TagBindings(bigqueryfront.BigQueryTagPrefix + "projects/tag-proj/datasets/other")
	if err != nil || len(bs) != 1 {
		t.Fatalf("bindings after Bind tag: %v, %v", bs, err)
	}
	if acts := cp.tagActionsAt(path); len(acts) != 2 || acts[1].ID != tagUnbind {
		t.Fatalf("actions = %+v, want Bind tag and Unbind tag", acts)
	}
	if ok, err := cp.tagActAt(context.Background(), path, tagUnbind, map[string]string{"value": prod.GetNamespacedName()}); !ok || err != nil {
		t.Fatalf("Unbind tag: %v, %v", ok, err)
	}
}
