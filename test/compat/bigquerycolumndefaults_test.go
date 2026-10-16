//go:build compat

package compat

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"cloud.google.com/go/bigquery/storage/managedwriter/adapt"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestBigQueryColumnDefaults (#1340): a column's default value as
// https://cloud.google.com/bigquery/docs/default-values documents it. A
// default given by tables.insert (defaultValueExpression), by CREATE TABLE
// … DEFAULT, by tables.patch and by ALTER COLUMN SET DEFAULT is read back
// in the table's schema, and applied by a DML INSERT that leaves the column
// out or gives DEFAULT (VALUES and INSERT … SELECT), by
// tabledata.insertAll and the Storage Write API for a row that does not
// give the column, and by a load whose data does not have it; a NULL given
// stays NULL, and changing a default leaves the rows the table has as they
// are. DROP DEFAULT and SET DEFAULT NULL clear it. Refused 400: ADD COLUMN
// … DEFAULT, and a tables.patch that adds a column with a default ("You
// cannot add a new column with a default value to an existing table").
//
// covers: bigquery.tables.insert, bigquery.tables.get, bigquery.tables.patch, bigquery.jobs.query, bigquery.jobs.insert, bigquery.tabledata.insertAll, bigquery.tabledata.list
func TestBigQueryColumnDefaults(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	rows := func(table string) []string {
		t.Helper()
		return queryRows(t, ctx, c, project, ds.DatasetID, "SELECT id, s FROM "+table)
	}
	want := func(what string, got []string, want ...string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rows %q, want %q", what, got, want)
		}
	}
	defaultOf := func(tbl *bigquery.Table, column string) string {
		t.Helper()
		md, err := tbl.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range md.Schema {
			if f.Name == column {
				return f.DefaultValueExpression
			}
		}
		t.Fatalf("%s has no column %s", tbl.TableID, column)
		return ""
	}

	// tables.insert: DML INSERT, insertAll.
	api := ds.Table("api")
	A := ds.DatasetID + ".api"
	if err := api.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType},
		{Name: "s", Type: bigquery.StringFieldType, DefaultValueExpression: "'dflt'"},
	}}); err != nil {
		t.Fatal(err)
	}
	if d := defaultOf(api, "s"); d != "'dflt'" {
		t.Errorf("tables.insert: defaultValueExpression %q, want 'dflt'", d)
	}
	for _, insert := range []bool{false, true} {
		for _, sql := range []string{"INSERT INTO " + A + " (id) VALUES (1)", "INSERT INTO " + A + " (id, s) VALUES (2, DEFAULT)",
			"INSERT INTO " + A + " (id) SELECT 3", "INSERT INTO " + A + " (id, s) VALUES (4, NULL)"} {
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		want("DML INSERT", rows("api"), "1|dflt", "2|dflt", "3|dflt", "4|<nil>")
		if err := bqRun(ctx, c, "DELETE FROM "+A+" WHERE true", insert); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.Inserter().Put(ctx, []*bigquery.ValuesSaver{
		{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}}, Row: []bigquery.Value{5}},
		{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "s", Type: bigquery.StringFieldType}},
			Row: []bigquery.Value{6, nil}},
		{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "s", Type: bigquery.StringFieldType}},
			Row: []bigquery.Value{7, "given"}},
	}); err != nil {
		t.Fatalf("insertAll: %v", err)
	}
	want("insertAll", rows("api"), "5|dflt", "6|<nil>", "7|given")

	// A load whose data does not have the column.
	src := bigquery.NewReaderSource(strings.NewReader(`{"id": 8}` + "\n" + `{"id": 9, "s": null}` + "\n"))
	src.SourceFormat = bigquery.JSON
	job, err := api.LoaderFrom(src).Run(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("load: %v, %v", err, st.Err())
	}
	want("load", rows("api"), "5|dflt", "6|<nil>", "7|given", "8|dflt", "9|<nil>")

	// The Storage Write API, with a write stream schema without s.
	md, err := api.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := adapt.BQSchemaToStorageTableSchema(md.Schema[:1])
	if err != nil {
		t.Fatal(err)
	}
	d, err := adapt.StorageSchemaToProto2Descriptor(ss, "root")
	if err != nil {
		t.Fatal(err)
	}
	m := d.(protoreflect.MessageDescriptor)
	dp, err := adapt.NormalizeDescriptor(m)
	if err != nil {
		t.Fatal(err)
	}
	mw := managedWriter(t, h, project)
	ms, err := mw.NewManagedStream(ctx, managedwriter.WithDestinationTable(writeTablePath(api)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	msg := dynamicpb.NewMessage(m)
	msg.Set(m.Fields().ByName("id"), protoreflect.ValueOfInt64(10))
	b, _ := proto.Marshal(msg)
	if _, err := appendResult(t, h, ms, [][]byte{b}); err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	ms.Close()
	want("Storage Write API", rows("api WHERE id = 10"), "10|dflt")
	// A field the write stream's schema has, unset in the row, is NULL.
	fm, full := writerSchema(t, h, api)
	ms, err = mw.NewManagedStream(ctx, managedwriter.WithDestinationTable(writeTablePath(api)),
		managedwriter.WithType(managedwriter.DefaultStream), managedwriter.WithSchemaDescriptor(full))
	if err != nil {
		t.Fatalf("NewManagedStream: %v", err)
	}
	msg = dynamicpb.NewMessage(fm)
	msg.Set(fm.Fields().ByName("id"), protoreflect.ValueOfInt64(12))
	b, _ = proto.Marshal(msg)
	if _, err := appendResult(t, h, ms, [][]byte{b}); err != nil {
		t.Fatalf("AppendRows: %v", err)
	}
	ms.Close()
	want("Storage Write API, s in the stream's schema", rows("api WHERE id = 12"), "12|<nil>")

	// tables.patch changes the default; the rows the table has stay.
	md, err = api.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	schema := md.Schema
	schema[1].DefaultValueExpression = "'patched'"
	if _, err := api.Update(ctx, bigquery.TableMetadataToUpdate{Schema: schema}, md.ETag); err != nil {
		t.Fatalf("tables.patch: %v", err)
	}
	if d := defaultOf(api, "s"); d != "'patched'" {
		t.Errorf("tables.patch: defaultValueExpression %q, want 'patched'", d)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+A+" (id) VALUES (11)", false); err != nil {
		t.Fatal(err)
	}
	want("after tables.patch", rows("api WHERE id IN (5, 6, 11)"), "11|patched", "5|dflt", "6|<nil>")
	added := append(bigquery.Schema{}, schema...)
	added = append(added, &bigquery.FieldSchema{Name: "n", Type: bigquery.IntegerFieldType, DefaultValueExpression: "7"})
	_, err = api.Update(ctx, bigquery.TableMetadataToUpdate{Schema: added}, "")
	wantHTTPStatus(t, "tables.patch adding a column with a default", err, http.StatusBadRequest)

	// CREATE TABLE … DEFAULT, ALTER COLUMN SET DEFAULT and DROP DEFAULT.
	for _, insert := range []bool{false, true} {
		name := "ddl_q"
		if insert {
			name = "ddl_j"
		}
		tbl := ds.Table(name)
		name = ds.DatasetID + "." + name
		run := func(sql string) {
			t.Helper()
			if err := bqRun(ctx, c, sql, insert); err != nil {
				t.Fatalf("%s (insert %v): %v", sql, insert, err)
			}
		}
		run("CREATE TABLE " + name + " (id INT64, s STRING DEFAULT 'x', t STRING)")
		if d := defaultOf(tbl, "s"); d != "'x'" {
			t.Errorf("CREATE TABLE (insert %v): defaultValueExpression %q, want 'x'", insert, d)
		}
		run("INSERT INTO " + name + " (id) VALUES (1)")
		run("ALTER TABLE " + name + " ALTER COLUMN s SET DEFAULT CONCAT('y', 'z'), ALTER COLUMN t SET DEFAULT 't'")
		if d := defaultOf(tbl, "s"); d != "CONCAT('y', 'z')" {
			t.Errorf("SET DEFAULT (insert %v): defaultValueExpression %q", insert, d)
		}
		run("INSERT INTO " + name + " (id) VALUES (2)")
		want("SET DEFAULT", rows(name), "1|x", "2|yz")
		run("ALTER TABLE " + name + " ALTER COLUMN s DROP DEFAULT, ALTER COLUMN t SET DEFAULT NULL")
		if d := defaultOf(tbl, "s") + defaultOf(tbl, "t"); d != "" {
			t.Errorf("DROP DEFAULT (insert %v): defaultValueExpression %q, want none", insert, d)
		}
		run("INSERT INTO " + name + " (id) VALUES (3)")
		want("DROP DEFAULT", rows(name), "1|x", "2|yz", "3|<nil>")
		for sql, code := range map[string]int{
			"ALTER TABLE " + name + " ADD COLUMN n INT64 DEFAULT 1":     http.StatusBadRequest,
			"ALTER TABLE " + name + " ALTER COLUMN nope SET DEFAULT 1":  http.StatusBadRequest,
			"ALTER TABLE " + name + " ALTER COLUMN s.x SET DEFAULT 'a'": http.StatusBadRequest, // a subfield, #1389
		} {
			wantHTTPStatus(t, sql, bqRun(ctx, c, sql, insert), code)
		}
	}
}
