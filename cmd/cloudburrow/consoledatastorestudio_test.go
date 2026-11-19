package main

import (
	"testing"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

// TestDatastoreStudioPieces (#1846): Import/Export's Namespace and Kind
// choices as an entity filter, GQL aggregations told from queries, and a
// Datastore mode database's row on the Firestore Databases page opening
// the Datastore page in it.
func TestDatastoreStudioPieces(t *testing.T) {
	f := datastoreEntityFilter(map[string]string{"namespace": datastoreAllNamespaces, "kind": datastoreAllKinds})
	if len(f.GetNamespaceIds()) != 0 || len(f.GetKinds()) != 0 {
		t.Errorf("All Namespaces, All Kinds = %v, want no filter", f)
	}
	f = datastoreEntityFilter(map[string]string{"namespace": "(default)", "kind": "Task"})
	if len(f.GetNamespaceIds()) != 1 || f.GetNamespaceIds()[0] != "" || len(f.GetKinds()) != 1 || f.GetKinds()[0] != "Task" {
		t.Errorf("(default), Task = %v", f)
	}
	for q, agg := range map[string]bool{
		"SELECT COUNT(*) FROM Task":                    true,
		"select sum(x) as s from Task":                 true,
		"AGGREGATE COUNT(*) OVER (SELECT * FROM Task)": true,
		"SELECT * FROM Task WHERE n = 1":               false,
		"SELECT count FROM Task":                       false,
	} {
		if got := datastoreGQLAggregation.MatchString(q); got != agg {
			t.Errorf("%q is an aggregation: %v, want %v", q, got, agg)
		}
	}
	acts := databaseRowActions("p", &adminpb.Database{Name: "projects/p/databases/d1", Type: adminpb.Database_DATASTORE_MODE})
	if len(acts) != 2 || acts[0].Label != "Datastore Studio" || acts[0].Href != "/datastore?database=d1&project=p" {
		t.Errorf("a Datastore mode database's row actions = %+v", acts)
	}
	if acts := databaseRowActions("p", &adminpb.Database{Name: "projects/p/databases/n1", Type: adminpb.Database_FIRESTORE_NATIVE}); len(acts) != 1 {
		t.Errorf("a Native database's row actions = %+v", acts)
	}
}
