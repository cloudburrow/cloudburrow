//go:build compat

package compat

import (
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"cloud.google.com/go/bigquery/storage/managedwriter"
	"cloud.google.com/go/bigquery/storage/managedwriter/adapt"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Change data capture through the Storage Write API (#1999), as
// https://docs.cloud.google.com/bigquery/docs/change-data-capture
// documents it, through the official managed writer: the writer schema is
// the table's (adapt) with the _CHANGE_TYPE and _CHANGE_SEQUENCE_NUMBER
// pseudo-columns added, as Google's CDC samples make it.

// cdcSchema is a table's writer schema with the two pseudo-columns.
func cdcSchema(t *testing.T, h *Harness, tbl *bigquery.Table) (protoreflect.MessageDescriptor, *descriptorpb.DescriptorProto) {
	t.Helper()
	md, err := tbl.Metadata(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	ss, err := adapt.BQSchemaToStorageTableSchema(md.Schema)
	if err != nil {
		t.Fatal(err)
	}
	ss.Fields = append(ss.Fields,
		&storagepb.TableFieldSchema{Name: "_CHANGE_TYPE", Type: storagepb.TableFieldSchema_STRING, Mode: storagepb.TableFieldSchema_NULLABLE},
		&storagepb.TableFieldSchema{Name: "_CHANGE_SEQUENCE_NUMBER", Type: storagepb.TableFieldSchema_STRING, Mode: storagepb.TableFieldSchema_NULLABLE})
	d, err := adapt.StorageSchemaToProto2Descriptor(ss, "root")
	if err != nil {
		t.Fatal(err)
	}
	m := d.(protoreflect.MessageDescriptor)
	dp, err := adapt.NormalizeDescriptor(m)
	if err != nil {
		t.Fatal(err)
	}
	return m, dp
}

// cdcChange is one row of a change append: id, name, and the
// pseudo-columns ("" leaves one unset).
type cdcChange struct {
	id        int64
	name, typ string
	seq       string
}

func cdcRows(t *testing.T, m protoreflect.MessageDescriptor, changes ...cdcChange) [][]byte {
	t.Helper()
	var out [][]byte
	for _, c := range changes {
		msg := dynamicpb.NewMessage(m)
		f := func(n string) protoreflect.FieldDescriptor { return m.Fields().ByName(protoreflect.Name(n)) }
		msg.Set(f("id"), protoreflect.ValueOfInt64(c.id))
		if c.name != "" {
			msg.Set(f("name"), protoreflect.ValueOfString(c.name))
		}
		if c.typ != "" {
			msg.Set(f("_CHANGE_TYPE"), protoreflect.ValueOfString(c.typ))
		}
		if c.seq != "" {
			msg.Set(f("_CHANGE_SEQUENCE_NUMBER"), protoreflect.ValueOfString(c.seq))
		}
		b, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// cdcTableRows reads the table's (id, name) rows, by id.
func cdcTableRows(t *testing.T, h *Harness, c *bigquery.Client, table string) map[int64]string {
	t.Helper()
	it, err := c.Query("SELECT id, name FROM " + table + " ORDER BY id").Read(h.Context())
	if err != nil {
		t.Fatalf("query %s: %v", table, err)
	}
	out := map[int64]string{}
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		name, _ := row[1].(string)
		out[row[0].(int64)] = name
	}
}

func cdcTable(t *testing.T, h *Harness, ds *bigquery.Dataset, name string, key bool) *bigquery.Table {
	t.Helper()
	tbl := ds.Table(name)
	md := &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "name", Type: bigquery.StringFieldType}}}
	if key {
		md.TableConstraints = &bigquery.TableConstraints{PrimaryKey: &bigquery.PrimaryKey{Columns: []string{"id"}}}
		md.MaxStaleness = &bigquery.IntervalValue{Minutes: 15}
	}
	if err := tbl.Create(h.Context(), md); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return tbl
}

func cdcAppend(t *testing.T, h *Harness, mw *managedwriter.Client, tbl *bigquery.Table, dp *descriptorpb.DescriptorProto,
	typ managedwriter.StreamType, rows [][]byte) (*storagepb.AppendRowsResponse, error) {
	t.Helper()
	ms, err := mw.NewManagedStream(h.Context(), managedwriter.WithDestinationTable(writeTablePath(tbl)),
		managedwriter.WithType(typ), managedwriter.WithSchemaDescriptor(dp))
	if err != nil {
		return nil, err
	}
	defer ms.Close()
	res, err := ms.AppendRows(h.Context(), rows)
	if err != nil {
		return nil, err
	}
	if _, err := res.GetResult(h.Context()); err != nil {
		full, _ := res.FullResponse(h.Context())
		return full, err
	}
	return res.FullResponse(h.Context())
}

func TestBigQueryStorageWriteChangeDataCapture(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	ds, _ := twoDatasets(t, h, c)
	tbl := cdcTable(t, h, ds, "cdc", true)
	meta, err := tbl.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta.MaxStaleness == nil || meta.MaxStaleness.Minutes != 15 || meta.TableConstraints == nil ||
		!reflect.DeepEqual(meta.TableConstraints.PrimaryKey.Columns, []string{"id"}) {
		t.Fatalf("metadata: max_staleness %v, constraints %+v", meta.MaxStaleness, meta.TableConstraints)
	}
	m, dp := cdcSchema(t, h, tbl)
	mw := managedWriter(t, h, project)
	full := "`" + project + "." + ds.DatasetID + ".cdc`"

	// The first append inserts; the second updates, deletes, and leaves
	// the changes numbered below a key's highest without effect.
	if _, err := cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m,
		cdcChange{id: 1, name: "a", typ: "UPSERT", seq: "1"},
		cdcChange{id: 2, name: "b", typ: "UPSERT", seq: "1"},
		cdcChange{id: 3, name: "c", typ: "UPSERT"},
		cdcChange{id: 4, name: "x", typ: "UPSERT", seq: "FFF/ABC"},
		cdcChange{id: 6, name: "late", typ: "UPSERT", seq: "BA/FFFFFFFF"},
	)); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if got, want := cdcTableRows(t, h, c, full), map[int64]string{1: "a", 2: "b", 3: "c", 4: "x", 6: "late"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the upserts: %v, want %v", got, want)
	}
	if _, err := cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m,
		cdcChange{id: 1, name: "a2", typ: "UPSERT", seq: "2"},
		cdcChange{id: 1, name: "stale", typ: "UPSERT", seq: "1/F"},     // 1 < 2: no effect
		cdcChange{id: 2, typ: "DELETE", seq: "2"},                      // the row is deleted
		cdcChange{id: 2, name: "back", typ: "UPSERT", seq: "1"},        // below the DELETE's: no effect
		cdcChange{id: 4, name: "y", typ: "UPSERT", seq: "FFF/B"},       // B < ABC: no effect
		cdcChange{id: 5, name: "first", typ: "UPSERT", seq: "A"},       //
		cdcChange{id: 5, name: "second", typ: "UPSERT", seq: "a"},      // equal: the later wins, either case
		cdcChange{id: 6, name: "now", typ: "UPSERT", seq: "ABC"},       // ABC > BA: the first section decides
		cdcChange{id: 3, typ: "DELETE"},                                // no number: applied
		cdcChange{id: 99, typ: "DELETE", seq: "1"},                     // no such row
		cdcChange{id: 7, name: "seven", typ: "UPSERT", seq: "0/0/0/0"}, // four sections
	)); err != nil {
		t.Fatalf("second append: %v", err)
	}
	want := map[int64]string{1: "a2", 4: "x", 5: "second", 6: "now", 7: "seven"}
	if got := cdcTableRows(t, h, c, full); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the changes: %v, want %v", got, want)
	}
	// A later append below a deleted key's number: still no effect.
	if _, err := cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m,
		cdcChange{id: 2, name: "again", typ: "UPSERT", seq: "1"})); err != nil {
		t.Fatal(err)
	}
	if got := cdcTableRows(t, h, c, full); !reflect.DeepEqual(got, want) {
		t.Fatalf("after a late change: %v, want %v", got, want)
	}
	// "Pseudocolumns _CHANGE_TYPE and _CHANGE_SEQUENCE_NUMBER are not
	// queryable columns".
	if _, err := c.Query("SELECT _CHANGE_TYPE FROM " + full).Read(ctx); err == nil {
		t.Errorf("SELECT _CHANGE_TYPE was answered")
	}

	// The refusals.
	rowErr := func(what string, resp *storagepb.AppendRowsResponse, err error, text string) {
		t.Helper()
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want InvalidArgument", what, err)
			return
		}
		if text == "" {
			return
		}
		msgs := status.Convert(err).Message()
		for _, re := range resp.GetRowErrors() {
			msgs += " " + re.GetMessage()
		}
		if !strings.Contains(msgs, text) {
			t.Errorf("%s: %q, want it to say %q", what, msgs, text)
		}
	}
	resp, err := cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m, cdcChange{id: 8, name: "i", typ: "INSERT"}))
	rowErr("_CHANGE_TYPE INSERT", resp, err, "not a valid CHANGE_TYPE")
	resp, err = cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m, cdcChange{id: 8, name: "i"}))
	rowErr("no _CHANGE_TYPE", resp, err, "not a valid CHANGE_TYPE")
	for _, bad := range []string{"XYZ", "1/2/3/4/5", "12345678901234567", "1//2"} {
		resp, err = cdcAppend(t, h, mw, tbl, dp, managedwriter.DefaultStream, cdcRows(t, m, cdcChange{id: 8, name: "i", typ: "UPSERT", seq: bad}))
		rowErr("_CHANGE_SEQUENCE_NUMBER "+bad, resp, err, "_CHANGE_SEQUENCE_NUMBER")
	}
	// "You must use the Storage Write API (gRPC) in the default stream."
	resp, err = cdcAppend(t, h, mw, tbl, dp, managedwriter.CommittedStream, cdcRows(t, m, cdcChange{id: 8, name: "i", typ: "UPSERT"}))
	rowErr("a COMMITTED stream", resp, err, "_default stream")
	// "You must declare primary keys for the destination table."
	nokey := cdcTable(t, h, ds, "nokey", false)
	nm, ndp := cdcSchema(t, h, nokey)
	resp, err = cdcAppend(t, h, mw, nokey, ndp, managedwriter.DefaultStream, cdcRows(t, nm, cdcChange{id: 1, name: "a", typ: "UPSERT"}))
	rowErr("a table without a primary key", resp, err, "primary key")
	if got := cdcTableRows(t, h, c, "`"+project+"."+ds.DatasetID+".nokey`"); len(got) != 0 {
		t.Errorf("the table without a key has rows %v", got)
	}
	// Nothing refused was written.
	if got := cdcTableRows(t, h, c, full); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the refusals: %v, want %v", got, want)
	}
}
