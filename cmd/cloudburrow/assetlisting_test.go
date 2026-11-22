package main

import (
	"reflect"
	"testing"

	"cloud.google.com/go/bigquery"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

func TestRemoteParents(t *testing.T) {
	for name, want := range map[string][]string{
		"projects/p/topics/t":                {"projects/p/topics/t"},
		"projects/p/datasets/d/tables/t":     {"projects/p/datasets/d/tables/t", "projects/p/datasets/d"},
		"projects/p/instances/i/databases/d": {"projects/p/instances/i/databases/d", "projects/p/instances/i"},
	} {
		if got := remoteParents(name); !reflect.DeepEqual(got, want) {
			t.Errorf("remoteParents(%q) = %v; want %v", name, got, want)
		}
	}
}

func TestBigQueryAccessPolicy(t *testing.T) {
	got := bigqueryAccessPolicy("p", []*bigquery.AccessEntry{
		{Role: bigquery.OwnerRole, EntityType: bigquery.SpecialGroupEntity, Entity: "projectOwners"},
		{Role: bigquery.ReaderRole, EntityType: bigquery.UserEmailEntity, Entity: "ann@example.com"},
		{Role: bigquery.ReaderRole, EntityType: bigquery.GroupEmailEntity, Entity: "g@example.com"},
		{Role: bigquery.WriterRole, EntityType: bigquery.UserEmailEntity, Entity: "sa@p.iam.gserviceaccount.com"},
		{Role: "roles/bigquery.metadataViewer", EntityType: bigquery.IAMMemberEntity, Entity: "domain:example.com"},
		{EntityType: bigquery.ViewEntity},
	})
	want := &iampolicy.Stored{Bindings: []iampolicy.Binding{
		{Role: "roles/bigquery.dataOwner", Members: []string{"projectOwner:p"}},
		{Role: "roles/bigquery.dataViewer", Members: []string{"user:ann@example.com", "group:g@example.com"}},
		{Role: "roles/bigquery.dataEditor", Members: []string{"serviceAccount:sa@p.iam.gserviceaccount.com"}},
		{Role: "roles/bigquery.metadataViewer", Members: []string{"domain:example.com"}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bigqueryAccessPolicy = %+v; want %+v", got, want)
	}
}
