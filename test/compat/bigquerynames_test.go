//go:build compat

package compat

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"github.com/apache/arrow/go/v15/arrow/ipc"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestBigQueryStorageReadOfATableIDAnotherDatasetHas (#1032, #1046),
// through the official Storage Read client: a read session of a table
// whose ID another dataset has too streams that table's own rows, as
// Arrow in the table's own schema, never the other table's; so does
// ReadRows of a session made before another dataset made a table of its
// ID, and a session after the table's schema changed. Through the official
// Go client with its Storage Read client (Client.EnableStorageReadClient),
// Table.Read and Query.Read of a query job with a destination table read
// the same so. Measured first
// against the pinned emulator, with one.same (s STRING, three rows) made
// first and two.same (a INT64, two rows) after: ReadRows of two.same failed
// "strconv.ParseInt: parsing "one"", reading one.same's rows, as the
// emulator reads a session's rows by the bare table ID; since #1032 the
// session was 501.
func TestBigQueryStorageReadOfATableIDAnotherDatasetHas(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	one, two := twoDatasets(t, h, c)
	strS := bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType}}
	for _, s := range []struct {
		ds     *bigquery.Dataset
		id     string
		schema bigquery.Schema
		rows   string
	}{
		{one, "same", strS, "(s) VALUES ('one'),('two'),('three')"},
		{two, "same", bigquery.Schema{{Name: "a", Type: bigquery.IntegerFieldType, Required: true}, {Name: "b", Type: bigquery.StringFieldType}},
			"(a, b) VALUES (4, 'four'),(5, NULL)"},
		{one, "own", strS, "(s) VALUES ('six')"},
		{one, "later", strS, "(s) VALUES ('seven')"},
	} {
		if err := s.ds.Table(s.id).Create(ctx, &bigquery.TableMetadata{Schema: s.schema}); err != nil {
			t.Fatalf("create %s.%s: %v", s.ds.DatasetID, s.id, err)
		}
		if err := bqRun(ctx, c, "INSERT INTO "+s.ds.DatasetID+"."+s.id+" "+s.rows, false); err != nil {
			t.Fatalf("insert into %s.%s: %v", s.ds.DatasetID, s.id, err)
		}
	}

	rc, err := bqstorage.NewBigQueryReadClient(ctx,
		option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewBigQueryReadClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	tablePath := func(ds *bigquery.Dataset, id string) string {
		return "projects/" + project + "/datasets/" + ds.DatasetID + "/tables/" + id
	}
	session := func(ds *bigquery.Dataset, id string) *storagepb.ReadSession {
		t.Helper()
		s, err := rc.CreateReadSession(h.Context(), &storagepb.CreateReadSessionRequest{
			Parent:         "projects/" + project,
			ReadSession:    &storagepb.ReadSession{Table: tablePath(ds, id), DataFormat: storagepb.DataFormat_ARROW},
			MaxStreamCount: 1,
		})
		if err != nil {
			t.Fatalf("a session of %s.%s: %v", ds.DatasetID, id, err)
		}
		if s.GetTable() != tablePath(ds, id) {
			t.Errorf("the session of %s.%s names %s", ds.DatasetID, id, s.GetTable())
		}
		return s
	}
	// rows reads every stream of s, and returns its Arrow schema and each
	// row's values joined by "|", sorted.
	rows := func(s *storagepb.ReadSession) (string, []string) {
		t.Helper()
		var schema string
		var out []string
		for _, st := range s.GetStreams() {
			stream, err := rc.ReadRows(h.Context(), &storagepb.ReadRowsRequest{ReadStream: st.Name})
			if err != nil {
				t.Fatalf("ReadRows of %s: %v", s.GetTable(), err)
			}
			for {
				resp, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("ReadRows of %s: %v", s.GetTable(), err)
				}
				// A batch is read after the session's schema, as
				// BigQuery frames them and the Go client reads them.
				r, err := ipc.NewReader(io.MultiReader(bytes.NewReader(s.GetArrowSchema().GetSerializedSchema()),
					bytes.NewReader(resp.GetArrowRecordBatch().GetSerializedRecordBatch())))
				if err != nil {
					t.Fatalf("decode the Arrow rows of %s: %v", s.GetTable(), err)
				}
				schema = r.Schema().String()
				for r.Next() {
					rec := r.Record()
					for i := 0; i < int(rec.NumRows()); i++ {
						var vals []string
						for j := 0; j < int(rec.NumCols()); j++ {
							vals = append(vals, rec.Column(j).ValueStr(i))
						}
						out = append(out, strings.Join(vals, "|"))
					}
				}
				r.Release()
			}
		}
		sort.Strings(out)
		return schema, out
	}
	check := func(ds *bigquery.Dataset, id string, s *storagepb.ReadSession, wantSchema string, want []string) {
		t.Helper()
		schema, got := rows(s)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s.%s streamed %q, want its own %q", ds.DatasetID, id, got, want)
		}
		if wantSchema != "" && schema != wantSchema {
			t.Errorf("%s.%s streamed the Arrow schema\n%s\nwant\n%s", ds.DatasetID, id, schema, wantSchema)
		}
	}
	nullableS := "schema:\n  fields: 1\n    - s: type=utf8, nullable"
	twoSchema := "schema:\n  fields: 2\n    - a: type=int64\n    - b: type=utf8, nullable"

	check(one, "own", session(one, "own"), nullableS, []string{"six"})
	check(two, "same", session(two, "same"), twoSchema, []string{"4|four", "5|(null)"})
	check(one, "same", session(one, "same"), nullableS, []string{"one", "three", "two"})
	// Again, as the view the front read it through is kept.
	check(two, "same", session(two, "same"), twoSchema, []string{"4|four", "5|(null)"})

	// A session made before another dataset made a table of its ID.
	s := session(one, "later")
	if err := two.Table("later").Create(ctx, &bigquery.TableMetadata{Schema: strS}); err != nil {
		t.Fatalf("create %s.later: %v", two.DatasetID, err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+two.DatasetID+".later (s) VALUES ('eight')", false); err != nil {
		t.Fatal(err)
	}
	check(one, "later", s, nullableS, []string{"seven"})
	check(two, "later", session(two, "later"), nullableS, []string{"eight"})

	// The table's schema changes (a column added, tables.patch).
	tbl := two.Table("same")
	md, err := tbl.Metadata(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tbl.Update(h.Context(), bigquery.TableMetadataToUpdate{
		Schema: append(md.Schema, &bigquery.FieldSchema{Name: "c", Type: bigquery.BooleanFieldType})}, md.ETag); err != nil {
		t.Fatalf("add a column to %s.same: %v", two.DatasetID, err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+two.DatasetID+".same (a, c) VALUES (6, true)", false); err != nil {
		t.Fatal(err)
	}
	check(two, "same", session(two, "same"), "schema:\n  fields: 3\n    - a: type=int64\n    - b: type=utf8, nullable\n    - c: type=bool, nullable",
		[]string{"4|four|(null)", "5|(null)|(null)", "6|(null)|true"})

	// The Go client's reads through the Storage Read API.
	sc, _ := bigqueryClient(t, h)
	if err := sc.EnableStorageReadClient(h.Context(),
		option.WithEndpoint(h.Endpoint(EnvBigQueryStorage)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))); err != nil {
		t.Fatalf("EnableStorageReadClient: %v", err)
	}
	accelerated := func(what string, it *bigquery.RowIterator, want []string) {
		t.Helper()
		if !it.IsAccelerated() {
			t.Errorf("%s was not read through the Storage Read API", what)
		}
		if got := readRows(t, it, what); !reflect.DeepEqual(got, want) {
			t.Errorf("%s read %q, want %q", what, got, want)
		}
	}
	accelerated("Table.Read of "+two.DatasetID+".same", sc.Dataset(two.DatasetID).Table("same").Read(h.Context()),
		[]string{"4|four|<nil>", "5|<nil>|<nil>", "6|<nil>|true"})
	accelerated("Table.Read of "+one.DatasetID+".same", sc.Dataset(one.DatasetID).Table("same").Read(h.Context()),
		[]string{"one", "three", "two"})
	// A query job's destination whose ID another dataset has too.
	for _, ds := range []*bigquery.Dataset{one, two} {
		if err := ds.Table("result").Create(ctx, &bigquery.TableMetadata{Schema: strS}); err != nil {
			t.Fatalf("create %s.result: %v", ds.DatasetID, err)
		}
	}
	q := sc.Query("SELECT s FROM " + one.DatasetID + ".same WHERE s != 'two'")
	q.Dst = sc.Dataset(two.DatasetID).Table("result")
	q.WriteDisposition = bigquery.WriteTruncate
	it, err := q.Read(h.Context())
	if err != nil {
		t.Fatalf("Query.Read into %s.result: %v", two.DatasetID, err)
	}
	accelerated("Query.Read into "+two.DatasetID+".result", it, []string{"one", "three"})
}

// TestBigQueryGoogleSQLTypeNamesReadBack (#1034), through the official Go
// client: a table made with the GoogleSQL type names INT64, BOOL, FLOAT64,
// STRUCT and DECIMAL, through tables.insert, a load job and tables.patch,
// reads back by the legacy names BigQuery reports (INTEGER, BOOLEAN,
// FLOAT, RECORD, NUMERIC) and is read by Table.Read. Measured first
// against the pinned emulator: tables.get read INT64 and BOOL back as
// sent, and Table.Read failed "unrecognized type: INT64".
func TestBigQueryGoogleSQLTypeNamesReadBack(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	aliases := bigquery.Schema{
		{Name: "n", Type: "INT64"}, {Name: "b", Type: "BOOL"}, {Name: "f", Type: "FLOAT64"}, {Name: "d", Type: "DECIMAL"},
		{Name: "r", Type: "STRUCT", Schema: bigquery.Schema{{Name: "x", Type: "INT64"}}},
	}
	want := []bigquery.FieldType{bigquery.IntegerFieldType, bigquery.BooleanFieldType, bigquery.FloatFieldType,
		bigquery.NumericFieldType, bigquery.RecordFieldType}
	types := func(tbl *bigquery.Table) []bigquery.FieldType {
		t.Helper()
		md, err := tbl.Metadata(ctx)
		if err != nil {
			t.Fatalf("metadata of %s: %v", tbl.TableID, err)
		}
		var out []bigquery.FieldType
		for _, f := range md.Schema {
			out = append(out, f.Type)
		}
		if len(md.Schema) == 5 && len(md.Schema[4].Schema) == 1 && md.Schema[4].Schema[0].Type != bigquery.IntegerFieldType {
			t.Errorf("%s: r.x reads %s, want INTEGER", tbl.TableID, md.Schema[4].Schema[0].Type)
		}
		return out
	}

	tbl := ds.Table("typed")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: aliases}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := types(tbl); !reflect.DeepEqual(got, want) {
		t.Errorf("tables.insert: the table reads %v, want %v", got, want)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+ds.DatasetID+".typed (n, b, f, d, r) VALUES (1, true, 0.5, 2.5, STRUCT(3))", false); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got, want := readTable(t, ctx, tbl, 0), []string{"1|true|0.5|5/2|[3]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read: %v, want %v", got, want)
	}

	// A load job's schema.
	src := bigquery.NewReaderSource(strings.NewReader(`{"n":1,"b":true,"f":0.5,"d":"2.5","r":{"x":3}}` + "\n"))
	src.SourceFormat = bigquery.JSON
	src.Schema = aliases
	loaded := ds.Table("loaded")
	job, err := loaded.LoaderFrom(src).Run(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st, err := job.Wait(ctx); err != nil || st.Err() != nil {
		t.Fatalf("load: %v %v", err, st.Err())
	}
	if got := types(loaded); !reflect.DeepEqual(got, want) {
		t.Errorf("a load: the table reads %v, want %v", got, want)
	}
	if got := readTable(t, ctx, loaded, 0); len(got) != 1 {
		t.Errorf("Table.Read of the loaded table: %v", got)
	}

	// tables.patch adding a column by its GoogleSQL name.
	md, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	added := append(md.Schema, &bigquery.FieldSchema{Name: "g", Type: "INT64"})
	md, err = tbl.Update(ctx, bigquery.TableMetadataToUpdate{Schema: added}, md.ETag)
	if err != nil {
		t.Fatalf("add a column: %v", err)
	}
	if got := md.Schema[len(md.Schema)-1].Type; got != bigquery.IntegerFieldType {
		t.Errorf("the added column reads %s, want INTEGER", got)
	}
	if got, want := readTable(t, ctx, tbl, 0), []string{"1|true|0.5|5/2|[3]|<nil>"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Table.Read after the patch: %v, want %v", got, want)
	}
}

// TestBigQueryViewsNameTheirTablesWithADataset (#1035), through the
// official Go client: a view made through tables.insert (Table.Create
// with a ViewQuery) whose query names a table without a dataset is 400
// invalid, and nothing is made, as BigQuery requires "A reference inside
// of a view must be qualified with a dataset"; one naming dataset.table
// reads that table. Measured first against the pinned emulator, with
// one.t made first and two.t after: a view two.v of "SELECT * FROM t" was
// made, and read one.t's rows through Table.Read and a query.
func TestBigQueryViewsNameTheirTablesWithADataset(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	one, two := twoDatasets(t, h, c)
	for _, s := range []struct {
		ds   *bigquery.Dataset
		rows string
	}{{one, "('one')"}, {two, "('two')"}} {
		if err := s.ds.Table("t").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType}}}); err != nil {
			t.Fatalf("create %s.t: %v", s.ds.DatasetID, err)
		}
		if err := bqRun(ctx, c, "INSERT INTO "+s.ds.DatasetID+".t (s) VALUES "+s.rows, false); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	err := two.Table("v").Create(ctx, &bigquery.TableMetadata{ViewQuery: "SELECT * FROM t"})
	wantReason(t, "a view of SELECT * FROM t", err, http.StatusBadRequest, "invalid")
	if _, err := two.Table("v").Metadata(ctx); !isNotFound(err) {
		t.Errorf("the refused view was made: %v", err)
	}
	if err := two.Table("v").Create(ctx, &bigquery.TableMetadata{ViewQuery: "SELECT * FROM " + two.DatasetID + ".t"}); err != nil {
		t.Fatalf("a view of dataset.t: %v", err)
	}
	if got, want := queryRows(t, ctx, c, "", "", "SELECT * FROM "+two.DatasetID+".v"), []string{"two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the view reads %v, want %v", got, want)
	}
}

// TestBigQueryCreateViewNamesItsTablesWithADataset (#1049), through the
// official Go client: a CREATE VIEW (or CREATE MATERIALIZED VIEW)
// statement whose query names a table without a dataset is refused 400
// invalid even with a default dataset, and nothing is made, as BigQuery's
// view reference says "The default dataset doesn't affect a view body";
// the view's own name is in the default dataset, and a query naming
// dataset.table reads that table. Before, since #1015, the front sent the
// view's query with its table names in the default dataset, so two.v of
// "SELECT * FROM t" was made of two.t.
func TestBigQueryCreateViewNamesItsTablesWithADataset(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	one, two := twoDatasets(t, h, c)
	for _, s := range []struct {
		ds   *bigquery.Dataset
		rows string
	}{{one, "('one')"}, {two, "('two')"}} {
		if err := s.ds.Table("t").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType}}}); err != nil {
			t.Fatalf("create %s.t: %v", s.ds.DatasetID, err)
		}
		if err := bqRun(ctx, c, "INSERT INTO "+s.ds.DatasetID+".t (s) VALUES "+s.rows, false); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	for _, sql := range []string{"CREATE VIEW v AS SELECT * FROM t", "CREATE OR REPLACE VIEW " + two.DatasetID + ".v AS SELECT s FROM t",
		"CREATE MATERIALIZED VIEW v AS SELECT * FROM t"} {
		for _, insert := range []bool{false, true} {
			err := runIn(ctx, c, project, two.DatasetID, sql, insert)
			if !insert {
				wantReason(t, sql+" through jobs.query", err, http.StatusBadRequest, "invalid")
			} else if errReason(err) != "invalid" {
				t.Errorf("%s as a query job: %v, want it failed invalid", sql, err)
			}
			if err == nil || !strings.Contains(err.Error(), `Table "t" must be qualified with a dataset`) {
				t.Errorf("%s: %v, want the table named", sql, err)
			}
		}
	}
	if _, err := two.Table("v").Metadata(ctx); !isNotFound(err) {
		t.Errorf("a refused view was made: %v", err)
	}
	if err := runIn(ctx, c, project, two.DatasetID, "CREATE VIEW v AS SELECT * FROM "+one.DatasetID+".t", false); err != nil {
		t.Fatalf("a view of dataset.t: %v", err)
	}
	if got, want := queryRows(t, ctx, c, "", "", "SELECT * FROM "+two.DatasetID+".v"), []string{"one"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the view %s.v reads %v, want %v", two.DatasetID, got, want)
	}
}
