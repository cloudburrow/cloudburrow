//go:build compat

package compat

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// covers: bigquery.datasets.insert, bigquery.datasets.patch, bigquery.tables.insert, bigquery.routines.insert
//
// TestOrgPolicyCustomConstraintsBigQuery (#1895), with the official
// BigQuery client: the page's example, DENY on bigquery.googleapis.com/
// Dataset resource.datasetReference.datasetId.startsWith('test'),
// refuses creating such a dataset, 412 with the page's message
// `Operation denied by custom org policies:
// ["customConstraints/custom.x": "..."]`; a DENY on
// defaultTableExpirationMs (CREATE and UPDATE) refuses lowering it below
// an hour; DENY constraints on bigquery.googleapis.com/Table and Routine
// read their references and refuse a tmp table and a routine named bad.
func TestOrgPolicyCustomConstraintsBigQuery(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	endpoint := h.Endpoint(EnvBigQuery)
	shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocq-")
	bq, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bq.Close() })
	ids := customName(h, "enforceDatasetId")
	idesc := "All new datasets can't begin with 'test'."
	expiry := customName(h, "bqexpiry")
	edesc := "Table expirations of an hour at least."
	tmp := customName(h, "bqtmp")
	tdesc := "No tmp tables."
	fn := customName(h, "bqroutine")
	fdesc := "No routine named bad."
	createCustom(t, h, c, ids, "bigquery.googleapis.com/Dataset", "resource.datasetReference.datasetId.startsWith('test')", ccDeny, idesc, ccCreate)
	createCustom(t, h, c, expiry, "bigquery.googleapis.com/Dataset",
		"has(resource.defaultTableExpirationMs) && resource.defaultTableExpirationMs < 3600000", ccDeny, edesc, ccCreate, ccUpdate)
	createCustom(t, h, c, tmp, "bigquery.googleapis.com/Table", "resource.tableReference.tableId.startsWith('tmp')", ccDeny, tdesc, ccCreate)
	createCustom(t, h, c, fn, "bigquery.googleapis.com/Routine", "resource.routineReference.routineId == 'bad'", ccDeny, fdesc, ccCreate)
	for _, n := range []string{ids, expiry, tmp, fn} {
		setOrgPolicy(t, h, c, "projects/"+project, n, opEnforce())
	}
	bqMessage := func(short, desc string) string {
		return `Operation denied by custom org policies: ["customConstraints/` + short + `": "` + desc + `"]`
	}
	want412 := func(what string, err error, msg string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != 412 || ge.Message != msg {
			t.Errorf("%s = %v, want 412 %s", what, err, msg)
		}
	}
	want412("a test dataset", bq.Dataset("test_cc").Create(ctx, &bigquery.DatasetMetadata{Location: "US"}), bqMessage(ids, idesc))
	want412("a dataset with a minute's expiration", bq.Dataset("cc_short").Create(ctx, &bigquery.DatasetMetadata{Location: "US",
		DefaultTableExpiration: time.Minute}), bqMessage(expiry, edesc))
	ds := bq.Dataset("cc_ok")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Location: "US", DefaultTableExpiration: 2 * time.Hour}); err != nil {
		t.Fatalf("an allowed dataset: %v", err)
	}
	t.Cleanup(func() { _ = ds.DeleteWithContents(context.Background()) })
	_, err = ds.Update(ctx, bigquery.DatasetMetadataToUpdate{DefaultTableExpiration: time.Minute}, "")
	want412("lowering the expiration", err, bqMessage(expiry, edesc))
	if _, err := ds.Update(ctx, bigquery.DatasetMetadataToUpdate{Description: "kept"}, ""); err != nil {
		t.Errorf("an update of the description: %v", err)
	}
	schema := bigquery.Schema{{Name: "x", Type: bigquery.IntegerFieldType}}
	want412("a tmp table", ds.Table("tmp_rows").Create(ctx, &bigquery.TableMetadata{Schema: schema}), bqMessage(tmp, tdesc))
	if err := ds.Table("rows").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Errorf("an allowed table: %v", err)
	}
	body := &bigquery.RoutineMetadata{Type: "SCALAR_FUNCTION", Language: "SQL", Body: "x * 2",
		Arguments: []*bigquery.RoutineArgument{{Name: "x", DataType: &bigquery.StandardSQLDataType{TypeKind: "INT64"}}}}
	want412("a routine named bad", ds.Routine("bad").Create(ctx, body), bqMessage(fn, fdesc))
	if err := ds.Routine("good").Create(ctx, body); err != nil {
		t.Errorf("an allowed routine: %v", err)
	}
}
