//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryColumnNameCharacterMap (#1326): a CSV load with autodetect
// into a new table under columnNameCharacterMap V1 or V2 renames each
// detected name to the map's rule, each character it does not take
// replaced with "_" (BigQuery's load reference: unsupported characters
// "are replaced with underscores"), as the official Go client's
// Loader.ColumnNameCharacterMap sends it. Table.Metadata reads the new
// names, the column types are kept, and the rows read back by the new
// names. V2 keeps a flexible name ("first name") and renames "a!"; V1
// renames both. A name still refused once replaced (V1's leading digit)
// and names that are the same once replaced stay 501, the table deleted.
// Measured first: the emulator kept "a!" under V2, which the front
// answered 501.
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.tables.get, bigquery.tabledata.list
func TestBigQueryColumnNameCharacterMap(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	load := func(table, data string, charMap bigquery.ColumnNameCharacterMap) (*bigquery.Job, error) {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.SourceFormat = bigquery.CSV
		src.AutoDetect = true
		l := ds.Table(table).LoaderFrom(src)
		l.ColumnNameCharacterMap = charMap
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return l.Run(lctx)
	}
	for _, tc := range []struct {
		table   string
		charMap bigquery.ColumnNameCharacterMap
		want    []string
	}{
		{"v2", bigquery.V2ColumnNameCharacterMap, []string{"first name", "a_", "n"}},
		{"v1", bigquery.V1ColumnNameCharacterMap, []string{"first_name", "a_", "n"}},
	} {
		job, err := load(tc.table, "first name,a!,n\nbob,x,3\nann,y,4\n", tc.charMap)
		if err != nil {
			t.Fatalf("%s: the load was refused: %v", tc.table, err)
		}
		if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
			t.Fatalf("%s: the load failed: %v %v", tc.table, err, st.Err())
		}
		again, err := c.JobFromID(ctx, job.ID())
		if err != nil {
			t.Fatal(err)
		}
		if st, err := again.Status(ctx); err != nil || st.Err() != nil {
			t.Errorf("%s: jobs.get reports %v %v, want the job done", tc.table, st, err)
		}
		md, err := ds.Table(tc.table).Metadata(ctx)
		if err != nil || len(md.Schema) != 3 {
			t.Fatalf("%s: Table.Metadata: %+v %v", tc.table, md, err)
		}
		for i, name := range tc.want {
			if md.Schema[i].Name != name {
				t.Errorf("%s: column %d is %q, want %q", tc.table, i, md.Schema[i].Name, name)
			}
		}
		if md.Schema[2].Type != bigquery.IntegerFieldType {
			t.Errorf("%s: n is %s, want INTEGER", tc.table, md.Schema[2].Type)
		}
		got := rows(t, ctx, c, fmt.Sprintf("SELECT `%s`, a_, n FROM `%s.%s` ORDER BY n", tc.want[0], ds.DatasetID, tc.table))
		if fmt.Sprint(got) != "[[bob x 3] [ann y 4]]" {
			t.Errorf("%s reads %v", tc.table, got)
		}
		if n := countRows(t, h, ds.Table(tc.table)); n != 2 {
			t.Errorf("%s: tabledata.list has %d rows, want 2", tc.table, n)
		}
	}

	for table, data := range map[string]string{
		"digit": "1a,b\n1,2\n",
		"same":  "a!,a?\n1,2\n",
	} {
		_, err := load(table, data, bigquery.V1ColumnNameCharacterMap)
		wantHTTPStatus(t, "a V1 load with a header it cannot rename ("+table+")", err, http.StatusNotImplemented)
		if tableExists(t, ctx, ds.Table(table)) {
			t.Errorf("the refused load left %s", table)
		}
	}
}
