package iameval

import (
	"slices"
	"strings"
	"testing"

	_ "cloud.google.com/go/bigquery/datatransfer/apiv1/datatransferpb"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestRound12DataTransferTable (#1879): every Data Transfer method has a
// row naming request fields it has and a permission Google's index names.
func TestRound12DataTransferTable(t *testing.T) {
	names := iamroles.Names()
	const svc = "google.cloud.bigquery.datatransfer.v1.DataTransferService"
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(svc)
	if err != nil {
		t.Fatal(err)
	}
	ms := d.(protoreflect.ServiceDescriptor).Methods()
	for i := 0; i < ms.Len(); i++ {
		full := "/" + svc + "/" + string(ms.Get(i).Name())
		m, ok := DataTransfer.Methods[full]
		if !ok {
			t.Errorf("%s has no row", full)
			continue
		}
		for _, p := range append([]string{m.Field}, m.ActAs...) {
			if !hasPath(ms.Get(i).Input(), strings.Split(p, ".")) {
				t.Errorf("%s: request has no string field %s", full, p)
			}
		}
		if _, ok := slices.BinarySearch(names, m.Permission); !ok {
			t.Errorf("%s: %s is not in Google's permission index", full, m.Permission)
		}
	}
	if len(DataTransfer.Methods) != ms.Len() {
		t.Errorf("%d rows for %d methods", len(DataTransfer.Methods), ms.Len())
	}
}
