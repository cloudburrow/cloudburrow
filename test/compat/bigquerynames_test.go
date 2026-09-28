//go:build compat

package compat

import (
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bqstorage "cloud.google.com/go/bigquery/storage/apiv1"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestBigQueryStorageReadRefusesATableIDAnotherDatasetHas (#1032), through
// the official Storage Read client: a read session of a table whose ID
// another dataset has too is UNIMPLEMENTED, never the other table's rows;
// so is ReadRows of a session made before another dataset made a table of
// its ID; a table whose ID is its own is read. Measured first against the
// pinned emulator, with one.t (s STRING, three rows) made first and two.t
// (a INT64, two rows) after: ReadRows of two.t failed "strconv.ParseInt:
// parsing "one"", reading one.t's rows, as the emulator reads a session's
// rows by the bare table ID.
func TestBigQueryStorageReadRefusesATableIDAnotherDatasetHas(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	one, two := twoDatasets(t, h, c)
	for _, s := range []struct {
		ds   *bigquery.Dataset
		id   string
		rows string
	}{{one, "same", "('one'),('two'),('three')"}, {two, "same", "('four'),('five')"}, {one, "own", "('six')"}, {one, "later", "('seven')"}} {
		if err := s.ds.Table(s.id).Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType}}}); err != nil {
			t.Fatalf("create %s.%s: %v", s.ds.DatasetID, s.id, err)
		}
		if err := bqRun(ctx, c, "INSERT INTO "+s.ds.DatasetID+"."+s.id+" (s) VALUES "+s.rows, false); err != nil {
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
	session := func(ds *bigquery.Dataset, id string) (*storagepb.ReadSession, error) {
		return rc.CreateReadSession(ctx, &storagepb.CreateReadSessionRequest{
			Parent: "projects/" + project,
			ReadSession: &storagepb.ReadSession{
				Table:      "projects/" + project + "/datasets/" + ds.DatasetID + "/tables/" + id,
				DataFormat: storagepb.DataFormat_ARROW,
			},
			MaxStreamCount: 1,
		})
	}
	rows := func(s *storagepb.ReadSession) (int64, error) {
		var n int64
		for _, st := range s.GetStreams() {
			stream, err := rc.ReadRows(ctx, &storagepb.ReadRowsRequest{ReadStream: st.Name})
			if err != nil {
				return n, err
			}
			for {
				resp, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return n, err
				}
				n += resp.RowCount
			}
		}
		return n, nil
	}
	unimplemented := func(what string, err error) {
		t.Helper()
		if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "#1032") {
			t.Errorf("%s: %v, want UNIMPLEMENTED", what, err)
		}
	}

	s, err := session(one, "own")
	if err != nil {
		t.Fatalf("a session of %s.own: %v", one.DatasetID, err)
	}
	if n, err := rows(s); err != nil || n != 1 {
		t.Errorf("%s.own streamed %d rows (%v), want its 1", one.DatasetID, n, err)
	}
	for _, ds := range []*bigquery.Dataset{one, two} {
		s, err := session(ds, "same")
		if err == nil {
			n, rerr := rows(s)
			t.Errorf("a session of %s.same was made and streamed %d rows (%v), want UNIMPLEMENTED", ds.DatasetID, n, rerr)
			continue
		}
		unimplemented("a session of "+ds.DatasetID+".same", err)
	}

	s, err = session(one, "later")
	if err != nil {
		t.Fatalf("a session of %s.later: %v", one.DatasetID, err)
	}
	if err := two.Table("later").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "s", Type: bigquery.StringFieldType}}}); err != nil {
		t.Fatalf("create %s.later: %v", two.DatasetID, err)
	}
	_, err = rows(s)
	unimplemented("ReadRows of "+one.DatasetID+".later after "+two.DatasetID+".later was made", err)
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
