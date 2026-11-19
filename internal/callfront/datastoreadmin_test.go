package callfront

import (
	"net/http/httptest"
	"reflect"
	"testing"

	pb "cloud.google.com/go/datastore/apiv1/datastorepb"
)

func TestExportFoldersFollowGoogleLayout(t *testing.T) {
	names := func(f entityFilter) []string {
		var out []string
		for _, e := range exportFolders(f) {
			out = append(out, e.name)
		}
		return out
	}
	cases := []struct {
		f    entityFilter
		want []string
	}{
		{entityFilter{}, []string{"all_namespaces/all_kinds"}},
		{entityFilter{Kinds: []string{"Task"}}, []string{"all_namespaces/kind_Task"}},
		{entityFilter{Namespaces: []string{"", "ns"}}, []string{"default_namespace/all_kinds", "namespace_ns/all_kinds"}},
		{entityFilter{Kinds: []string{"A"}, Namespaces: []string{"ns"}}, []string{"namespace_ns/kind_A"}},
	}
	for _, c := range cases {
		if got := names(c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("exportFolders(%+v) = %v, want %v", c.f, got, c.want)
		}
	}
	key := func(ns, kind string) *pb.Entity {
		return &pb.Entity{Key: &pb.Key{PartitionId: &pb.PartitionId{NamespaceId: ns}, Path: []*pb.Key_PathElement{{Kind: kind}}}}
	}
	f := exportFolders(entityFilter{Kinds: []string{"A"}, Namespaces: []string{""}})[0]
	if !f.takes(key("", "A")) || f.takes(key("ns", "A")) || f.takes(key("", "B")) {
		t.Error("default_namespace/kind_A takes the wrong entities")
	}
}

func TestDatastoreAdminClaims(t *testing.T) {
	a := newDatastoreAdmin(newDatastoreDatabases("127.0.0.1:1", t.Logf), "")
	for path, want := range map[string]bool{
		"/v1/projects/p:export":                    true,
		"/v1/projects/p:import":                    true,
		"/v1/projects/p/indexes":                   true,
		"/v1/projects/p/indexes/CICAg":             true,
		"/v1/projects/p/operations":                true,
		"/v1/projects/p/operations/AS1:cancel":     true,
		"/v1/projects/p:runQuery":                  false,
		"/v1/projects/p:lookup":                    false,
		"/cloudburrow/datastore/databases/dump":    true,
		"/cloudburrow/datastore/databases/forget":  false,
		"/v1/projects/p/databases/d/documents/c/x": false,
	} {
		if got := a.Claims(httptest.NewRequest("POST", path, nil)); got != want {
			t.Errorf("Claims(%s) = %v, want %v", path, got, want)
		}
	}
	r := httptest.NewRequest("POST", "/google.datastore.admin.v1.DatastoreAdmin/ExportEntities", nil)
	r.Header.Set("Content-Type", "application/grpc")
	if !a.Claims(r) {
		t.Error("the gRPC ExportEntities is not claimed")
	}
	r = httptest.NewRequest("POST", "/google.datastore.v1.Datastore/RunQuery", nil)
	r.Header.Set("Content-Type", "application/grpc")
	if a.Claims(r) {
		t.Error("RunQuery is claimed")
	}
}
