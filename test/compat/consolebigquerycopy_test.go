//go:build compat

package compat

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestConsoleBigQueryCopyTablesAndAvroExport (#782), in the emulators
// shard, through the console API, each outcome read back with the official
// clients. Copy table with More source tables copies two tables of one
// schema into a new one in one copy job, which then holds every row of
// both; Copy table alone copies one; a source named twice, or the
// destination named as a source, is refused before anything is sent.
// Export to Cloud Storage in Avro with DEFLATE writes an Avro object
// container file.
func TestConsoleBigQueryCopyTablesAndAvroExport(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	sc := instanceStorage(t, h)
	ds := validationDataset(t, h, c)
	d := ds.DatasetID
	schema := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "name", Type: bigquery.StringFieldType}}
	for name, rows := range map[string][]string{"a": {"1", "2"}, "b": {"3"}} {
		if err := ds.Table(name).Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := bqRun(ctx, c, "INSERT INTO "+d+"."+name+" (id, name) SELECT CAST(x AS INT64), CONCAT('n', x) FROM UNNEST(["+strings.Join(quoteAll(rows), ",")+"]) AS x", false); err != nil {
			t.Fatalf("insert into %s: %v", name, err)
		}
	}

	_, fields, _ := consoleJobResult(t, addr, project, []string{d, "a"}, "copytable", map[string]string{
		"datasetId": d, "tableId": "both", "writeDisposition": "WRITE_EMPTY", "moreSources": d + ".b\n"})
	if fields["State"] != "Succeeded" {
		t.Errorf("the multi-source copy answered %v", fields)
	}
	if got := strings.Join(readRowsSorted(t, ctx, ds.Table("both")), " "); got != `[1,"n1"] [2,"n2"] [3,"n3"]` {
		t.Errorf("the multi-source copy's destination holds %s", got)
	}
	_, fields, _ = consoleJobResult(t, addr, project, []string{d, "b"}, "copytable", map[string]string{
		"datasetId": d, "tableId": "onlyb", "writeDisposition": "WRITE_EMPTY"})
	if got := strings.Join(readRowsSorted(t, ctx, ds.Table("onlyb")), " "); fields["State"] != "Succeeded" || got != `[3,"n3"]` {
		t.Errorf("the copy of b answered %v and holds %s", fields, got)
	}
	for _, more := range []string{d + ".b\n" + d + ".b", d + ".dst"} {
		code, out := consoleAct(t, addr, "bigquery", project, []string{d, "a"}, "copytable", map[string]string{
			"datasetId": d, "tableId": "dst", "writeDisposition": "WRITE_EMPTY", "moreSources": more})
		if code == http.StatusOK {
			t.Errorf("Copy table with More source tables %q = %d: %s, want a refusal", more, code, out)
		}
	}
	if _, err := ds.Table("dst").Metadata(ctx); err == nil {
		t.Error("a refused copy made its destination")
	}

	bucketName := strings.ReplaceAll(h.Project(), "_", "-") + "-bq-avro"
	bucket := sc.Bucket(bucketName)
	if err := bucket.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = bucket.Object("both.avro").Delete(context.Background())
		_ = bucket.Delete(context.Background())
	})
	_, fields, _ = consoleJobResult(t, addr, project, []string{d, "both"}, "export", map[string]string{
		"uri": "gs://" + bucketName + "/both.avro", "format": "AVRO", "compression": "DEFLATE",
		"fieldDelimiter": ",", "header": "true"})
	if fields["State"] != "Succeeded" || fields["Files written"] != "1" {
		t.Errorf("the Avro export answered %v", fields)
	}
	r, err := bucket.Object("both.avro").NewReader(ctx)
	if err != nil {
		t.Fatalf("the Avro export's object: %v", err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.HasPrefix(b, []byte("Obj\x01")) || !bytes.Contains(b, []byte("deflate")) {
		t.Errorf("the Avro export is not a deflate Avro container file: %q", b[:min(len(b), 64)])
	}
}

// TestConsoleBigQueryCloneAndSnapshotTables (#1002), through the console
// API, each outcome read back with the official client: Copy table with
// Operation CLONE makes a writable clone with a cloneDefinition, SNAPSHOT a
// read-only table snapshot, and RESTORE of the snapshot a standard table
// with its rows; CLONE with More source tables is refused before anything
// is sent.
func TestConsoleBigQueryCloneAndSnapshotTables(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	ds := validationDataset(t, h, c)
	d := ds.DatasetID
	schema := bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}, {Name: "name", Type: bigquery.StringFieldType}}
	if err := ds.Table("a").Create(ctx, &bigquery.TableMetadata{Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if err := bqRun(ctx, c, "INSERT INTO "+d+".a VALUES (1, 'n1'), (2, 'n2')", false); err != nil {
		t.Fatal(err)
	}
	copyAs := func(src, dst, op string) {
		t.Helper()
		_, fields, _ := consoleJobResult(t, addr, project, []string{d, src}, "copytable", map[string]string{
			"datasetId": d, "tableId": dst, "operationType": op, "writeDisposition": "WRITE_EMPTY"})
		if fields["State"] != "Succeeded" {
			t.Errorf("%s of %s answered %v", op, src, fields)
		}
	}
	copyAs("a", "cl", "CLONE")
	md, err := ds.Table("cl").Metadata(ctx)
	if err != nil || md.Type != bigquery.RegularTable || md.CloneDefinition == nil ||
		md.CloneDefinition.BaseTableReference == nil || md.CloneDefinition.BaseTableReference.TableID != "a" {
		t.Errorf("the clone: %+v, %v", md, err)
	}
	copyAs("a", "sn", "SNAPSHOT")
	if md, err := ds.Table("sn").Metadata(ctx); err != nil || md.Type != bigquery.Snapshot {
		t.Errorf("the snapshot: %+v, %v", md, err)
	}
	copyAs("sn", "re", "RESTORE")
	if md, err := ds.Table("re").Metadata(ctx); err != nil || md.Type != bigquery.RegularTable {
		t.Errorf("the restored table: %+v, %v", md, err)
	}
	for _, table := range []string{"cl", "sn", "re"} {
		if got := strings.Join(readRowsSorted(t, ctx, ds.Table(table)), " "); got != `[1,"n1"] [2,"n2"]` {
			t.Errorf("%s holds %s", table, got)
		}
	}
	code, out := consoleAct(t, addr, "bigquery", project, []string{d, "a"}, "copytable", map[string]string{
		"datasetId": d, "tableId": "many", "operationType": "CLONE", "moreSources": d + ".cl"})
	if code == http.StatusOK {
		t.Errorf("CLONE with More source tables = %d: %s, want a refusal", code, out)
	}
	if _, err := ds.Table("many").Metadata(ctx); err == nil {
		t.Error("a refused clone made its destination")
	}
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = "'" + s + "'"
	}
	return out
}
