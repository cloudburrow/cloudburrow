//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestConsoleBigQueryReadWriteEditorRunsDDLAndScripts.
//
// The BigQuery editor's Read-write mode (#994), each change read back through
// the official Go client. A dataset's page offers the mode, naming the
// project and the default dataset a write changes. Through jobs.query it runs
// CREATE SCHEMA with a description, CREATE TABLE … AS SELECT from the
// dataset's own table by its unqualified name, CREATE VIEW, CREATE OR REPLACE
// TABLE, a CREATE TABLE IF NOT EXISTS of a table that exists (which changes
// nothing), a script with DECLARE whose CREATE TABLE … AS SELECT reads the
// variable, and DROP TABLE; and no query leaves a job-named dataset behind.
// DML inside a script (after DECLARE, or in a BEGIN block) is refused before
// anything is sent, with the reason and #1028, and so is UPDATE … FROM, with
// the emulator's reason and #1027, and they change no row (a DML statement of
// its own is TestConsoleBigQueryReadWriteEditorRunsDML); a lone SELECT is sent back to
// Read-only; ALTER TABLE is refused in the API's own words (the front's 501),
// without the client library's envelope. Read-only mode still refuses a DDL
// statement and says where it runs.
func TestConsoleBigQueryReadWriteEditorRunsDDLAndScripts(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds, orders := seedOrders(t, h, c)
	created := "rw_" + ds.DatasetID
	t.Cleanup(func() { _ = c.Dataset(created).DeleteWithContents(ctx) })
	q := "?project=" + url.QueryEscape(project)

	var page struct {
		Query struct {
			Hint  string
			Write struct{ Label, Target, Run string }
		}
	}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery"+q+"&name="+ds.DatasetID, "", &page)
	if page.Query.Write.Label != "Read-write" || !strings.Contains(page.Query.Write.Target, ds.DatasetID) ||
		page.Query.Write.Run != "Run statement" {
		t.Errorf("the dataset's page offers the write mode %+v, want Read-write naming %s", page.Query.Write, ds.DatasetID)
	}

	run := func(mode, statement string) (int, string) {
		t.Helper()
		req := map[string]any{"Path": []string{ds.DatasetID}, "Statement": statement}
		if mode != "" {
			req["Mode"] = mode
		}
		body, _ := json.Marshal(req)
		return consoleDo(t, addr, http.MethodPost, "/api/query/bigquery"+q, string(body))
	}
	write := func(statement string) {
		t.Helper()
		code, out := run("read-write", statement)
		var got struct{ Message string }
		_ = json.Unmarshal([]byte(out), &got)
		if code != http.StatusOK || !strings.HasPrefix(got.Message, "The statement ran") {
			t.Fatalf("console read-write %q = %d: %s", statement, code, out)
		}
	}
	refused := func(mode, statement, want string) {
		t.Helper()
		code, out := run(mode, statement)
		if code != http.StatusBadRequest {
			t.Fatalf("console %s %q = %d, want 400: %s", mode, statement, code, out)
		}
		if msg := consoleError(t, out); !strings.Contains(msg, want) || strings.Contains(msg, "googleapi") {
			t.Errorf("console %s %q refused with %q, want the words %q", mode, statement, msg, want)
		}
	}
	columns := func(table string) string {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("the client cannot read %s: %v", table, err)
		}
		var cols []string
		for _, f := range md.Schema {
			cols = append(cols, f.Name+":"+string(f.Type))
		}
		return strings.Join(cols, ",")
	}
	before := datasetIDs(t, h, c)

	write("CREATE SCHEMA " + created + ` OPTIONS (description = "made in the editor")`)
	if md, err := c.Dataset(created).Metadata(ctx); err != nil || md.Description != "made in the editor" {
		t.Errorf("CREATE SCHEMA made %+v, %v; want the dataset with its description", md, err)
	}
	write("CREATE TABLE totals AS SELECT region, SUM(amount) AS total FROM orders GROUP BY region")
	if got := columns("totals"); got != "region:STRING,total:FLOAT" {
		t.Errorf("CREATE TABLE … AS SELECT made columns %s", got)
	}
	if n := countRows(t, h, ds.Table("totals")); n != 2 {
		t.Errorf("CREATE TABLE … AS SELECT made %d rows, want the 2 regions", n)
	}
	write("CREATE VIEW big_orders AS SELECT id, amount FROM `" + project + "." + ds.DatasetID + ".orders` WHERE amount > 2")
	// The view's rows, not its query's text, which the emulator rewrites
	// (#1014).
	if md, err := ds.Table("big_orders").Metadata(ctx); err != nil || md.Type != bigquery.ViewTable {
		t.Errorf("CREATE VIEW made %+v, %v; want a view", md, err)
	}
	vq := c.Query("SELECT COUNT(*) FROM big_orders")
	vq.DefaultDatasetID = ds.DatasetID
	if it, err := vq.Read(ctx); err != nil {
		t.Errorf("the view cannot be read: %v", err)
	} else {
		var row []bigquery.Value
		if err := it.Next(&row); err != nil || len(row) != 1 || row[0] != int64(3) {
			t.Errorf("the view counts %v (%v), want the 3 orders over 2", row, err)
		}
	}
	write("CREATE OR REPLACE TABLE totals AS SELECT 1 AS n")
	if got := columns("totals"); got != "n:INTEGER" {
		t.Errorf("CREATE OR REPLACE TABLE left columns %s, want n:INTEGER", got)
	}
	write("CREATE TABLE IF NOT EXISTS totals (x STRING)")
	if got := columns("totals"); got != "n:INTEGER" {
		t.Errorf("CREATE TABLE IF NOT EXISTS of an existing table changed it to %s", got)
	}
	write("DECLARE k INT64 DEFAULT 3;\nCREATE TABLE late AS SELECT id FROM orders WHERE id >= k;")
	if n := countRows(t, h, ds.Table("late")); n != 2 {
		t.Errorf("the script's CREATE TABLE … AS SELECT made %d rows, want ids 3 and 4", n)
	}
	write("DROP TABLE late")
	if _, err := ds.Table("late").Metadata(ctx); !isNotFound(err) {
		t.Errorf("after DROP TABLE the client reads %v, want NOT_FOUND", err)
	}

	refused("read-write", "DECLARE x INT64 DEFAULT 1;\nDELETE FROM orders WHERE id >= x", "(#1028)")
	refused("read-write", "BEGIN\n  UPDATE orders SET region = 'z' WHERE true;\nEND", "UPDATE is DML inside a script")
	refused("read-write", "UPDATE orders SET region = t.region FROM totals t WHERE true", "Update with joins not supported")
	if n := countRows(t, h, orders); n != 4 {
		t.Errorf("after refused DML the table has %d rows, want 4", n)
	}
	refused("read-write", "SELECT * FROM orders", "switch the editor to Read-only")
	refused("read-write", "ALTER TABLE orders ADD COLUMN note STRING", "Not implemented here: ALTER TABLE")
	refused("", "CREATE TABLE never AS SELECT 1 AS a", "Switch it to Read-write")
	if _, err := ds.Table("never").Metadata(ctx); !isNotFound(err) {
		t.Errorf("a CREATE refused in Read-only made the table: %v", err)
	}

	after := datasetIDs(t, h, c)
	for _, id := range after {
		if id != created && !slices.Contains(before, id) {
			t.Errorf("the editor's statements left dataset %s behind", id)
		}
	}
}

// TestConsoleBigQueryEditTableViewsAndInsertOptions.
//
// The BigQuery forms #994 adds, each read back through the official Go
// client. Create table takes BigQuery's flexible column names ("first name");
// with the VIEW table type it makes a view from its query (tables.insert with
// a view), refusing a view with a schema and a table with a view query. A
// view's page offers Edit table and Delete table; a table's, Insert rows,
// Edit table, the jobs and Delete table. Edit table (tables.patch, through
// Table.Update) changes the description and labels, keeping the labels it
// does not change, on a table and on a view; since #1025 it removes a label
// taken out of the form, clears an emptied description and removes every
// label; a form saved unchanged says so; it offers no field to add (#1013).
// Insert rows
// with Skip invalid rows inserts the valid rows and answers with the invalid
// one, numbered, in the API's words; with Ignore unknown values a field the
// table lacks is dropped.
//
// covers: bigquery.tables.patch
func TestConsoleBigQueryEditTableViewsAndInsertOptions(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds, _ := seedOrders(t, h, c)
	path := []string{ds.DatasetID}

	act := func(path []string, action string, values map[string]string) (int, string) {
		t.Helper()
		return consoleAct(t, addr, "bigquery", project, path, action, values)
	}
	ok := func(path []string, action string, values map[string]string) string {
		t.Helper()
		code, out := act(path, action, values)
		if code != http.StatusOK {
			t.Fatalf("console %s %v = %d: %s", action, values, code, out)
		}
		return out
	}
	refused := func(path []string, action string, values map[string]string, want string) {
		t.Helper()
		code, out := act(path, action, values)
		if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), want) {
			t.Errorf("console %s %v = %d %s, want a refusal saying %q", action, values, code, out, want)
		}
	}

	ok(path, "createtable", map[string]string{"tableId": "people", "tableType": "TABLE", "schema": `[{"name":"id","type":"INTEGER","mode":"REQUIRED"},` +
		`{"name":"first name","type":"STRING"}]`})
	people := ds.Table("people")
	schema := func() string {
		t.Helper()
		md, err := people.Metadata(ctx)
		if err != nil {
			t.Fatalf("the client cannot read people: %v", err)
		}
		var cols []string
		for _, f := range md.Schema {
			mode := ""
			switch {
			case f.Required:
				mode = " REQUIRED"
			case f.Repeated:
				mode = " REPEATED"
			}
			cols = append(cols, f.Name+":"+string(f.Type)+mode)
		}
		return strings.Join(cols, ",")
	}
	if got := schema(); got != "id:INTEGER REQUIRED,first name:STRING" {
		t.Errorf("the flexible column name reads back as %s", got)
	}

	viewQuery := "SELECT `first name` AS name FROM `" + project + "." + ds.DatasetID + ".people`"
	ok(path, "createtable", map[string]string{"tableId": "names", "tableType": "VIEW", "viewQuery": viewQuery,
		"description": "a view"})
	if md, err := ds.Table("names").Metadata(ctx); err != nil || md.Type != bigquery.ViewTable ||
		md.ViewQuery != viewQuery || md.Description != "a view" {
		t.Errorf("Create table as a view made %+v, %v", md, err)
	}
	refused(path, "createtable", map[string]string{"tableId": "v2", "tableType": "VIEW", "viewQuery": viewQuery,
		"schema": `[{"name":"x","type":"STRING"}]`}, "leave the schema empty")
	refused(path, "createtable", map[string]string{"tableId": "t2", "tableType": "TABLE", "viewQuery": viewQuery,
		"schema": `[{"name":"x","type":"STRING"}]`}, "choose VIEW")
	for _, id := range []string{"v2", "t2"} {
		if _, err := ds.Table(id).Metadata(ctx); !isNotFound(err) {
			t.Errorf("the refused %s was made: %v", id, err)
		}
	}
	if got := strings.Join(consoleWriteDetail(t, addr, "bigquery", project, ds.DatasetID, "names").actionIDs(), ","); got != "edittable,deletetable" {
		t.Errorf("a view's page offers %s", got)
	}
	if got := strings.Join(consoleWriteDetail(t, addr, "bigquery", project, ds.DatasetID, "people").actionIDs(), ","); got != "insertrows,edittable,load,loadfile,export,copytable,deletetable" {
		t.Errorf("a table's page offers %s", got)
	}

	tp := []string{ds.DatasetID, "people"}
	page := consoleWriteDetail(t, addr, "bigquery", project, tp...)
	for _, a := range page.Actions {
		if a.ID != "edittable" {
			continue
		}
		var names []string
		for _, f := range a.Fields {
			names = append(names, f.Name)
		}
		if got := strings.Join(names, ","); got != "description,labels" {
			t.Errorf("Edit table offers the fields %s, want description and labels", got)
		}
	}
	ok(tp, "edittable", map[string]string{"description": "edited", "labels": `{"team":"data","env":"dev"}`})
	labels := func(table string) (string, map[string]string) {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil {
			t.Fatalf("the client cannot read %s: %v", table, err)
		}
		return md.Description, md.Labels
	}
	if desc, l := labels("people"); desc != "edited" || len(l) != 2 || l["team"] != "data" || l["env"] != "dev" {
		t.Errorf("Edit table left description %q and labels %v", desc, l)
	}
	ok(tp, "edittable", map[string]string{"description": "edited", "labels": `{"team":"ops","env":"dev"}`})
	if _, l := labels("people"); len(l) != 2 || l["team"] != "ops" || l["env"] != "dev" {
		t.Errorf("changing one label left %v, want team ops and env dev", l)
	}
	refused(tp, "edittable", map[string]string{"description": "edited", "labels": `{"team":"ops","env":"dev"}`}, "nothing to change")
	if desc, l := labels("people"); desc != "edited" || len(l) != 2 || l["env"] != "dev" {
		t.Errorf("a refused edit changed the table to %q, %v", desc, l)
	}
	// Removing a label and clearing the description (#1025).
	ok(tp, "edittable", map[string]string{"description": "edited", "labels": `{"team":"ops"}`})
	if desc, l := labels("people"); desc != "edited" || len(l) != 1 || l["team"] != "ops" {
		t.Errorf("removing env left %q, %v; want team ops alone", desc, l)
	}
	ok(tp, "edittable", map[string]string{"description": "", "labels": `{"team":"ops"}`})
	if desc, l := labels("people"); desc != "" || len(l) != 1 {
		t.Errorf("clearing the description left %q, %v", desc, l)
	}
	ok(tp, "edittable", map[string]string{"description": "", "labels": ""})
	if _, l := labels("people"); len(l) != 0 {
		t.Errorf("removing every label left %v", l)
	}
	ok([]string{ds.DatasetID, "names"}, "edittable", map[string]string{"description": "a view of names", "labels": `{"kind":"view"}`})
	if desc, l := labels("names"); desc != "a view of names" || l["kind"] != "view" {
		t.Errorf("Edit table on a view left %q, %v", desc, l)
	}
	if got := schema(); got != "id:INTEGER REQUIRED,first name:STRING" {
		t.Errorf("Edit table changed the schema to %s", got)
	}

	out := ok(tp, "insertrows", map[string]string{"skipInvalidRows": "true",
		"rows": `{"id": 1, "first name": "ann"}` + "\n" + `{"first name": "no id"}` + "\n" + `{"id": 3}`})
	var result struct {
		Result struct {
			Note  string
			Items []struct {
				Name   string
				Fields map[string]string
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if r := result.Result; len(r.Items) != 1 || r.Items[0].Name != "2" ||
		!strings.Contains(r.Items[0].Fields["Skipped because"], "Missing required field: id.") ||
		r.Note != "Inserted 2 of 3 rows; 1 skipped as invalid." {
		t.Errorf("Insert rows with Skip invalid rows answered %+v", r)
	}
	if n := countRows(t, h, people); n != 2 {
		t.Errorf("Skip invalid rows left %d rows, want the 2 valid", n)
	}
	refused(tp, "insertrows", map[string]string{"rows": `{"id": 4, "nosuch": 1}`}, "no such field: nosuch")
	ok(tp, "insertrows", map[string]string{"ignoreUnknownValues": "true", "rows": `{"id": 4, "nosuch": 1}`})
	if n := countRows(t, h, people); n != 3 {
		t.Errorf("Ignore unknown values left %d rows, want 3", n)
	}
}

// TestConsoleBigQueryReadWriteEditorRunsDML (#1024).
//
// A DML statement of its own runs in the BigQuery editor's Read-write mode,
// through jobs.query, and the answer says what it changed, from the rows
// the front reports (#1008): an INSERT of two rows added 2, an UPDATE by an
// unqualified name modified 2, a DELETE removed 1, a MERGE from a subquery
// inserted 1 and updated 1, a DELETE of nothing removed 0, and TRUNCATE
// TABLE removed the rest; each change is read back through the official Go
// client. The hint names DML, #1028 and #1027. No statement leaves a
// job-named dataset behind.
//
// covers: bigquery.jobs.query
func TestConsoleBigQueryReadWriteEditorRunsDML(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds, orders := seedOrders(t, h, c)
	q := "?project=" + url.QueryEscape(project)

	var page struct {
		Query struct{ Write struct{ Hint string } }
	}
	consoleJSON(t, addr, http.MethodGet, "/api/detail/bigquery"+q+"&name="+ds.DatasetID+"&name=orders", "", &page)
	if hint := page.Query.Write.Hint; !strings.Contains(hint, "INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE") ||
		!strings.Contains(hint, "#1028") || !strings.Contains(hint, "#1027") {
		t.Errorf("the Read-write hint is %q; want DML and what is not run", hint)
	}
	before := datasetIDs(t, h, c)

	dml := func(statement, want string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{ds.DatasetID, "orders"}, "Statement": statement, "Mode": "read-write"})
		code, out := consoleDo(t, addr, http.MethodPost, "/api/query/bigquery"+q, string(body))
		var got struct{ Message string }
		_ = json.Unmarshal([]byte(out), &got)
		if code != http.StatusOK || !strings.HasPrefix(got.Message, want+" It ran as job ") {
			t.Fatalf("console read-write %q = %d: %s; want %q", statement, code, out, want)
		}
	}
	rows := func() map[int64]string {
		t.Helper()
		it := orders.Read(ctx)
		out := map[int64]string{}
		for {
			var o order
			if err := it.Next(&o); errors.Is(err, iterator.Done) {
				return out
			} else if err != nil {
				t.Fatalf("read orders: %v", err)
			}
			out[o.ID] = o.Region
		}
	}

	dml("INSERT INTO orders (id, region, amount) VALUES (5, 'ap', 2), (6, 'ap', 3)", "This statement added 2 rows.")
	if r := rows(); len(r) != 6 || r[5] != "ap" || r[6] != "ap" {
		t.Errorf("after the INSERT the client reads %v", r)
	}
	dml("UPDATE orders SET region = 'eu2' WHERE region = 'eu'", "This statement modified 2 rows.")
	if r := rows(); r[1] != "eu2" || r[2] != "eu2" || r[3] != "us" {
		t.Errorf("after the UPDATE the client reads %v", r)
	}
	dml("DELETE FROM orders WHERE id = 6", "This statement removed 1 row.")
	if r := rows(); len(r) != 5 || r[6] != "" {
		t.Errorf("after the DELETE the client reads %v", r)
	}
	dml("MERGE orders o USING (SELECT 5 AS id, 'sa' AS region UNION ALL SELECT 7, 'nz') s ON o.id = s.id "+
		"WHEN MATCHED THEN UPDATE SET region = s.region "+
		"WHEN NOT MATCHED THEN INSERT (id, region, amount) VALUES (s.id, s.region, 0)",
		"This statement modified 2 rows: 1 inserted, 1 updated, 0 deleted.")
	if r := rows(); len(r) != 6 || r[5] != "sa" || r[7] != "nz" {
		t.Errorf("after the MERGE the client reads %v", r)
	}
	dml("DELETE FROM orders WHERE id = 42", "This statement removed 0 rows.")
	dml("TRUNCATE TABLE orders", "This statement removed 6 rows.")
	if r := rows(); len(r) != 0 {
		t.Errorf("after TRUNCATE TABLE the client reads %v", r)
	}

	for _, id := range datasetIDs(t, h, c) {
		if !slices.Contains(before, id) {
			t.Errorf("the editor's DML left dataset %s behind", id)
		}
	}
}
