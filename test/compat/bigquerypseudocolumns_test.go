//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
)

// TestBigQueryEnginePseudoColumnsAreUnrecognized (#1458): the engine's
// marks of streamed and buffered rows, _PARTITION_STREAMED and
// _PARTITION_BUFFERED, are no columns of BigQuery's, which reserves every
// name starting with _PARTITION; a query that names one, through
// jobs.query or a query job, fails 400 invalidQuery "Unrecognized name",
// as a query naming any column that does not exist fails. The rows
// streamed into the table read as usual. Measured first: the query read
// the engine's marks.
//
// covers: bigquery.tabledata.insertAll, bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryEnginePseudoColumnsAreUnrecognized(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	tbl := ds.Table("pseudo")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{
		Schema:           bigquery.Schema{{Name: "d", Type: bigquery.DateFieldType}, {Name: "n", Type: bigquery.IntegerFieldType}},
		TimePartitioning: &bigquery.TimePartitioning{Type: bigquery.DayPartitioningType, Field: "d"},
	}); err != nil {
		t.Fatal(err)
	}
	type row struct {
		D bigquery.NullDate `bigquery:"d"`
		N int64             `bigquery:"n"`
	}
	if err := tbl.Inserter().Put(ctx, []*row{{N: 1}}); err != nil {
		t.Fatal(err)
	}
	p := "`" + ds.DatasetID + ".pseudo`"
	for _, insert := range []bool{false, true} {
		for _, sql := range []string{
			"SELECT n, _PARTITION_STREAMED FROM " + p,
			"SELECT n FROM " + p + " WHERE _partition_buffered IS NULL",
		} {
			err := bqRun(ctx, c, sql, insert)
			var e *googleapi.Error
			if err == nil || !strings.Contains(err.Error(), "Unrecognized name: _") ||
				!insert && (!errors.As(err, &e) || e.Code != http.StatusBadRequest) {
				t.Errorf("insert=%v: %s gave %v, want 400 Unrecognized name", insert, sql, err)
			}
		}
	}
	if got := queryRows(t, ctx, c, "", "", "SELECT n FROM "+p+" WHERE n = 1 AND '_PARTITION_STREAMED' != ''"); strings.Join(got, ",") != "1" {
		t.Errorf("rows %v, want the streamed row", got)
	}
}
