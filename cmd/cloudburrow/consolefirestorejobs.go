package main

import (
	"context"
	"sort"
	"strings"
	"time"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// importExportJobs is the Import/Export tab's table (#1845): the database's
// export and import operations, newest first, from the Admin API's
// ListOperations, as Google's page lists its recent imports and exports
// (https://docs.cloud.google.com/firestore/native/docs/manage-data/export-import).
func (p firestoreDatabasesProvider) importExportJobs(ctx context.Context, c *admin.FirestoreAdminClient, db string) console.Listing {
	list := console.Listing{Columns: []string{"Type", "Status", "Start time", "End time", "Location", "Collection IDs"},
		NameColumn: "Operation", Noun: "imports and exports"}
	it := c.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: db + "/operations"})
	var jobs []transferRow
	for {
		op, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			list.Unavailable = "listing operations: " + err.Error()
			return list
		}
		j, ok := transferJob(op)
		if !ok {
			continue
		}
		jobs = append(jobs, j)
	}
	sort.SliceStable(jobs, func(i, k int) bool { return jobs[i].start.After(jobs[k].start) })
	for _, j := range jobs {
		list.Items = append(list.Items, j.res)
	}
	list.Total = len(list.Items)
	return list
}

type transferRow struct {
	start time.Time
	res   console.Resource
}

// transferJob is an export's or an import's row; ok is false for any other
// operation (an index build, a bulk delete).
func transferJob(op *longrunningpb.Operation) (transferRow, bool) {
	var (
		kind, where string
		ids         []string
		state       adminpb.OperationState
		start, end  *timestamppb.Timestamp
	)
	switch {
	case op.GetMetadata().MessageIs(&adminpb.ExportDocumentsMetadata{}):
		m := &adminpb.ExportDocumentsMetadata{}
		if op.GetMetadata().UnmarshalTo(m) != nil {
			return transferRow{}, false
		}
		kind, where, ids, state, start, end = "Export", m.GetOutputUriPrefix(), m.GetCollectionIds(), m.GetOperationState(), m.GetStartTime(), m.GetEndTime()
	case op.GetMetadata().MessageIs(&adminpb.ImportDocumentsMetadata{}):
		m := &adminpb.ImportDocumentsMetadata{}
		if op.GetMetadata().UnmarshalTo(m) != nil {
			return transferRow{}, false
		}
		kind, where, ids, state, start, end = "Import", m.GetInputUriPrefix(), m.GetCollectionIds(), m.GetOperationState(), m.GetStartTime(), m.GetEndTime()
	default:
		return transferRow{}, false
	}
	status := enumWord(state, "")
	switch {
	case op.GetDone() && op.GetError() != nil:
		status = "Failed: " + op.GetError().GetMessage()
	case op.GetDone() && state == adminpb.OperationState_OPERATION_STATE_UNSPECIFIED:
		status = "Successful"
	}
	collections := "All collections"
	if len(ids) > 0 {
		collections = strings.Join(ids, ", ")
	}
	stamp := func(t *timestamppb.Timestamp) string {
		if t == nil {
			return ""
		}
		return t.AsTime().Format("2006-01-02 15:04:05 MST")
	}
	return transferRow{start: start.AsTime(), res: console.Resource{Name: op.GetName()[strings.LastIndex(op.GetName(), "/")+1:],
		Status: status, Fields: map[string]string{
			"Type": kind, "Status": status, "Start time": stamp(start), "End time": stamp(end),
			"Location": where, "Collection IDs": collections,
		}}}, true
}
