//go:build compat

package compat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// TestBigQueryInformationSchemaViews (#1274): a dataset's
// INFORMATION_SCHEMA.VIEWS, through jobs.query and jobs.insert, has one row
// per view, made by CREATE VIEW or tables.insert, with BigQuery's columns:
// its project, dataset, name and query, check_option NULL and
// use_standard_sql YES; a table is not in it; a WHERE and an alias work; a
// dataset that does not exist is 404. Measured first: the engine failed it
// 400 "Table not found".
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryInformationSchemaViews(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	if err := bqRun(ctx, c, "CREATE TABLE "+path("t")+" (a INT64)", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "CREATE VIEW "+path("v_ddl")+" AS SELECT a FROM "+path("t"), false); err != nil {
		t.Fatal(err)
	}
	const apiQuery = "SELECT 1 AS one"
	if err := ds.Table("v_api").Create(ctx, &bigquery.TableMetadata{ViewQuery: apiQuery}); err != nil {
		t.Fatal(err)
	}
	read := func(sql string, insert bool) ([][]bigquery.Value, error) {
		qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		q := c.Query(sql)
		var it *bigquery.RowIterator
		var err error
		if insert {
			job, err := q.Run(qctx)
			if err != nil {
				return nil, err
			}
			if it, err = job.Read(qctx); err != nil {
				return nil, err
			}
		} else if it, err = q.Read(qctx); err != nil {
			return nil, err
		}
		var rows [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return rows, nil
			}
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
	}
	for _, insert := range []bool{false, true} {
		rows, err := read("SELECT table_catalog, table_schema, table_name, view_definition, check_option, use_standard_sql FROM "+
			ds.DatasetID+".INFORMATION_SCHEMA.VIEWS ORDER BY table_name", insert)
		if err != nil {
			t.Fatalf("insert=%v: %v", insert, err)
		}
		if len(rows) != 2 {
			t.Fatalf("insert=%v: %d rows, want the two views: %v", insert, len(rows), rows)
		}
		for i, want := range []struct{ name, query string }{
			{"v_api", apiQuery}, {"v_ddl", "SELECT a FROM " + path("t")},
		} {
			r := rows[i]
			if r[0] != project || r[1] != ds.DatasetID || r[2] != want.name || r[3] != want.query || r[4] != nil || r[5] != "YES" {
				t.Errorf("insert=%v: row %d = %v, want %s with %q", insert, i, r, want.name, want.query)
			}
		}
		rows, err = read("SELECT v.table_name FROM `"+project+"."+ds.DatasetID+".INFORMATION_SCHEMA.VIEWS` v "+
			"WHERE v.table_name = 'v_ddl'", insert)
		if err != nil || len(rows) != 1 || rows[0][0] != "v_ddl" {
			t.Errorf("insert=%v: an aliased, filtered read gave %v, %v", insert, rows, err)
		}
		_, err = read("SELECT * FROM no_such_dataset_1274.INFORMATION_SCHEMA.VIEWS", insert)
		var e *googleapi.Error
		if !errors.As(err, &e) || e.Code != http.StatusNotFound || !strings.Contains(e.Message, "no_such_dataset_1274") {
			t.Errorf("insert=%v: a dataset that does not exist gave %v, want 404", insert, err)
		}
	}
}
