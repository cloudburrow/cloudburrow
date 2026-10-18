//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// changeRows reads sql, a query of CHANGES or APPENDS whose columns are
// product, inventory, _CHANGE_TYPE and (CHANGES) _CHANGE_IS_FOR_UPDATE,
// as "product inventory type [for-update]" lines, in their order.
func changeRows(t *testing.T, got [][]bigquery.Value) []string {
	t.Helper()
	var out []string
	for _, r := range got {
		parts := make([]string, len(r))
		for i, v := range r {
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, " "))
	}
	return out
}

// TestBigQueryChangeHistory (#1418): BigQuery's documented example of
// CHANGES (https://cloud.google.com/bigquery/docs/reference/standard-sql/time-series-functions#changes)
// on a table made with enable_change_history = TRUE: two inserts, a
// delete and an update read back as INSERT, INSERT, DELETE, and the
// update's DELETE (_CHANGE_IS_FOR_UPDATE) and UPDATE, each with the
// time of its statement, one time per statement; and its example of
// APPENDS: the rows as they were inserted, the deleted one too, with
// a column added since. Through jobs.query and jobs.insert.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryChangeHistory(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	for _, insert := range []bool{false, true} {
		name := fmt.Sprintf("produce_%v", insert)
		ref := "`" + ds.DatasetID + "." + name + "`"
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		read := func(sql string) [][]bigquery.Value {
			t.Helper()
			if !insert {
				return rows(t, ctx, c, sql)
			}
			job, err := c.Query(sql).Run(ctx)
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			it, err := job.Read(ctx)
			if err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			var out [][]bigquery.Value
			for {
				var r []bigquery.Value
				if err := it.Next(&r); err != nil {
					break
				}
				out = append(out, r)
			}
			return out
		}
		run("CREATE TABLE " + ref + " (product STRING, inventory INT64) OPTIONS(enable_change_history=true)")
		run("INSERT INTO " + ref + " VALUES ('bananas', 20), ('carrots', 30)")
		run("DELETE " + ref + " WHERE product = 'bananas'")
		run("UPDATE " + ref + " SET inventory = inventory - 10 WHERE product = 'carrots'")
		got := changeRows(t, read("SELECT product, inventory, _CHANGE_TYPE AS change_type, _CHANGE_IS_FOR_UPDATE AS change_is_for_update "+
			"FROM CHANGES(TABLE "+ref+", NULL, NULL) ORDER BY _CHANGE_TIMESTAMP, product, change_type"))
		want := []string{"bananas 20 INSERT false", "carrots 30 INSERT false", "bananas 20 DELETE false",
			"carrots 30 DELETE true", "carrots 20 UPDATE false"}
		if strings.Join(got, "; ") != strings.Join(want, "; ") {
			t.Errorf("insert %v: CHANGES\n got %v\nwant %v", insert, got, want)
		}
		times := read("SELECT _CHANGE_TYPE, COUNT(DISTINCT _CHANGE_TIMESTAMP), MIN(_CHANGE_TIMESTAMP) <= CURRENT_TIMESTAMP() " +
			"FROM CHANGES(TABLE " + ref + ", NULL, NULL) GROUP BY 1 ORDER BY 1")
		if fmt.Sprint(times) != "[[DELETE 2 true] [INSERT 1 true] [UPDATE 1 true]]" {
			t.Errorf("insert %v: change times by type %v, want one time per statement", insert, times)
		}
		// APPENDS: what was inserted, as it was; the documented example
		// adds a column, a row, and changes the rest.
		run("ALTER TABLE " + ref + " ADD COLUMN color STRING")
		run("INSERT INTO " + ref + " VALUES ('grapes', 40, 'purple')")
		got = changeRows(t, read("SELECT product, inventory, color, _CHANGE_TYPE FROM APPENDS(TABLE "+ref+
			", TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR), NULL) WHERE product = 'grapes'"))
		if strings.Join(got, "; ") != "grapes 40 purple INSERT" {
			t.Errorf("insert %v: APPENDS after ADD COLUMN %v", insert, got)
		}
	}
}

// TestBigQueryChangeHistoryWritePaths (#1418): every way rows reach a
// table is in its change history: tabledata.insertAll, a load job (and a
// WRITE_TRUNCATE load, whose rows deleted are DELETEs), the Storage Write
// API, a query's destination table, MERGE and TRUNCATE TABLE. APPENDS
// reads any table, CHANGES one with enable_change_history, set here by
// ALTER TABLE SET OPTIONS.
//
// covers: bigquery.tabledata.insertAll, bigquery.jobs.insert, bigquery.jobs.query, bigquery.tables.insert
func TestBigQueryChangeHistoryWritePaths(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	tbl := ds.Table("paths")
	schema := bigquery.Schema{{Name: "product", Type: bigquery.StringFieldType}, {Name: "inventory", Type: bigquery.IntegerFieldType}}
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	ref := "`" + ds.DatasetID + ".paths`"
	if err := bqRun(ctx, c, "ALTER TABLE "+ref+" SET OPTIONS (enable_change_history = TRUE)", false); err != nil {
		t.Fatal(err)
	}
	type row struct {
		Product   string `bigquery:"product"`
		Inventory int64  `bigquery:"inventory"`
	}
	if err := tbl.Inserter().Put(ctx, []row{{"streamed", 1}}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}
	load := func(data string, wd bigquery.TableWriteDisposition) {
		t.Helper()
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.SourceFormat = bigquery.JSON
		l := tbl.LoaderFrom(src)
		l.WriteDisposition = wd
		job, err := l.Run(ctx)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
			t.Fatalf("load: %v, %v", err, st.Err())
		}
	}
	load(`{"product": "loaded", "inventory": 2}`+"\n", bigquery.WriteAppend)

	fm, dp := writerSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	ms, err := mw.NewManagedStream(ctx, managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	msg := dynamicpb.NewMessage(fm)
	msg.Set(fm.Fields().ByName("product"), protoreflect.ValueOfString("written"))
	msg.Set(fm.Fields().ByName("inventory"), protoreflect.ValueOfInt64(3))
	b, _ := proto.Marshal(msg)
	if _, err := appendResult(t, h, ms, [][]byte{b}); err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	ms.Close()

	q := c.Query("SELECT 'queried' AS product, 4 AS inventory")
	q.Dst = tbl
	q.WriteDisposition = bigquery.WriteAppend
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("query to a destination: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("query to a destination: %v, %v", err, st.Err())
	}
	all := func(fn string) []string {
		t.Helper()
		got := changeRows(t, rows(t, ctx, c, "SELECT product, inventory, _CHANGE_TYPE FROM "+fn+"(TABLE "+ref+", NULL, NULL)"))
		sort.Strings(got)
		return got
	}
	want := []string{"loaded 2 INSERT", "queried 4 INSERT", "streamed 1 INSERT", "written 3 INSERT"}
	if got := all("APPENDS"); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("APPENDS after insertAll, load, Storage Write API, query destination:\n got %v\nwant %v", got, want)
	}

	load(`{"product": "truncated", "inventory": 5}`+"\n", bigquery.WriteTruncate)
	got := all("CHANGES")
	want = append(want, "loaded 2 DELETE", "queried 4 DELETE", "streamed 1 DELETE", "truncated 5 INSERT", "written 3 DELETE")
	sort.Strings(want)
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("CHANGES after a WRITE_TRUNCATE load:\n got %v\nwant %v", got, want)
	}

	if err := bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".src` AS SELECT 'truncated' AS product, 6 AS inventory UNION ALL SELECT 'merged', 7", false); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "MERGE "+ref+" T USING `"+ds.DatasetID+".src` S ON T.product = S.product "+
		"WHEN MATCHED THEN UPDATE SET inventory = S.inventory WHEN NOT MATCHED THEN INSERT (product, inventory) VALUES (S.product, S.inventory)", true); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "TRUNCATE TABLE "+ref, false); err != nil {
		t.Fatal(err)
	}
	got = changeRows(t, rows(t, ctx, c, "SELECT product, inventory, _CHANGE_TYPE, _CHANGE_IS_FOR_UPDATE FROM CHANGES(TABLE "+ref+
		", NULL, NULL) WHERE product IN ('truncated', 'merged') ORDER BY _CHANGE_TIMESTAMP, product, _CHANGE_TYPE"))
	want = []string{"truncated 5 INSERT false", "merged 7 INSERT false", "truncated 5 DELETE true", "truncated 6 UPDATE false",
		"merged 7 DELETE false", "truncated 6 DELETE false"}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("CHANGES after MERGE and TRUNCATE TABLE:\n got %v\nwant %v", got, want)
	}
}

// TestBigQueryChangeHistoryRefusals (#1418): the documented limits.
// CHANGES of a table without enable_change_history, a start before the
// time travel window, a future end, and either function on a view fail
// 400 (wording UNVERIFIED); APPENDS reads a table without the option; a
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/change-history
// start before the table was made reads from its creation.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryChangeHistoryRefusals(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	ref := "`" + ds.DatasetID + ".plain`"
	for _, sql := range []string{
		"CREATE TABLE " + ref + " (product STRING, inventory INT64)",
		"INSERT INTO " + ref + " VALUES ('apples', 10)",
		"CREATE VIEW `" + ds.DatasetID + ".v` AS SELECT * FROM " + ref,
	} {
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if got := changeRows(t, rows(t, ctx, c, "SELECT product, inventory, _CHANGE_TYPE FROM APPENDS(TABLE "+ref+
		", TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 1 DAY), NULL)")); strings.Join(got, "; ") != "apples 10 INSERT" {
		t.Errorf("APPENDS of a table without change history: %v", got)
	}
	for _, insert := range []bool{false, true} {
		for _, tc := range []struct{ sql, want string }{
			{"SELECT * FROM CHANGES(TABLE " + ref + ", NULL, NULL)", "enable_change_history"},
			{"SELECT * FROM APPENDS(TABLE " + ref + ", TIMESTAMP_SUB(CURRENT_TIMESTAMP(), INTERVAL 8 DAY), NULL)", "time travel window"},
			{"SELECT * FROM APPENDS(TABLE " + ref + ", NULL, TIMESTAMP_ADD(CURRENT_TIMESTAMP(), INTERVAL 1 HOUR))", "future"},
			{"SELECT * FROM APPENDS(TABLE `" + ds.DatasetID + ".v`, NULL, NULL)", "regular table"},
		} {
			err := bqRun(ctx, c, tc.sql, insert)
			var e *googleapi.Error
			switch {
			case err == nil:
				t.Errorf("insert %v: %s: no error", insert, tc.sql)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("insert %v: %s: %v, want %q", insert, tc.sql, err, tc.want)
			case !insert && (!errors.As(err, &e) || e.Code != http.StatusBadRequest):
				t.Errorf("%s: %v, want 400", tc.sql, err)
			}
		}
	}
}
