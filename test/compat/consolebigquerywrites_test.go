//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
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
// Plain DML, alone or in a script, is refused before anything is sent, with
// the reason and #1008, and changes no row; a lone SELECT is sent back to
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

	refused("read-write", "INSERT INTO orders (id, region, amount) VALUES (9, 'x', 1)", "#1008")
	refused("read-write", "DECLARE x INT64 DEFAULT 1;\nDELETE FROM orders WHERE id >= x", "DELETE is DML")
	refused("read-write", "BEGIN\n  UPDATE orders SET region = 'z' WHERE true;\nEND", "UPDATE is DML")
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
// does not change, on a table and on a view; removing a label and clearing
// the description are refused, naming #1009, and change nothing; a form
// saved unchanged says so; it offers no field to add (#1013). Insert rows
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
	if got := strings.Join(consoleWriteDetail(t, addr, "bigquery", project, ds.DatasetID, "people").actionIDs(), ","); got != "insertrows,edittable,load,export,deletetable" {
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
	refused(tp, "edittable", map[string]string{"description": "edited", "labels": `{"team":"ops"}`}, "removing a label (env)")
	refused(tp, "edittable", map[string]string{"description": "", "labels": `{"team":"ops","env":"dev"}`}, "(#1009)")
	refused(tp, "edittable", map[string]string{"description": "edited", "labels": `{"team":"ops","env":"dev"}`}, "nothing to change")
	if desc, l := labels("people"); desc != "edited" || len(l) != 2 || l["env"] != "dev" {
		t.Errorf("refused edits changed the table to %q, %v", desc, l)
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
