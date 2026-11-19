package main

import (
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The Import/Export tab's jobs (#1845): an export and an import are rows
// with their location, collections and state; another operation is not.
func TestFirestoreImportExportJobRows(t *testing.T) {
	at := timestamppb.New(time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC))
	exp, _ := anypb.New(&adminpb.ExportDocumentsMetadata{OutputUriPrefix: "gs://b/x", CollectionIds: []string{"users"},
		OperationState: adminpb.OperationState_SUCCESSFUL, StartTime: at, EndTime: at})
	row, ok := transferJob(&longrunningpb.Operation{Name: "projects/p/databases/(default)/operations/op1", Metadata: exp, Done: true})
	if !ok || row.res.Name != "op1" || row.res.Fields["Type"] != "Export" || row.res.Fields["Location"] != "gs://b/x" ||
		row.res.Fields["Collection IDs"] != "users" || row.res.Fields["Status"] != "Successful" {
		t.Errorf("an export's row = %+v, %v", row.res, ok)
	}
	imp, _ := anypb.New(&adminpb.ImportDocumentsMetadata{InputUriPrefix: "gs://b/x", StartTime: at})
	if row, ok := transferJob(&longrunningpb.Operation{Name: "o/op2", Metadata: imp}); !ok || row.res.Fields["Type"] != "Import" ||
		row.res.Fields["Collection IDs"] != "All collections" {
		t.Errorf("an import's row = %+v, %v", row.res, ok)
	}
	idx, _ := anypb.New(&adminpb.IndexOperationMetadata{})
	if _, ok := transferJob(&longrunningpb.Operation{Name: "o/op3", Metadata: idx}); ok {
		t.Error("an index build is listed as an import or export")
	}
	if _, _, err := vectorIndex(map[string]string{"collectionGroup": "docs", "field": "embedding", "dimension": "4096"}); err == nil {
		t.Error("a vector index of 4096 dimensions is accepted; Firestore's limit is 2048")
	}
	in, cg, err := vectorIndex(map[string]string{"collectionGroup": "docs", "field": "embedding", "dimension": "3"})
	if err != nil || cg != "docs" || in.GetFields()[0].GetVectorConfig().GetDimension() != 3 || in.GetFields()[0].GetVectorConfig().GetFlat() == nil {
		t.Errorf("vectorIndex = %v, %q, %v", in, cg, err)
	}
}
