package bigqueryfront

import (
	"bytes"
	"testing"

	"github.com/apache/arrow/go/v15/arrow"
	"github.com/apache/arrow/go/v15/arrow/array"
	"github.com/apache/arrow/go/v15/arrow/ipc"
	"github.com/apache/arrow/go/v15/arrow/memory"
)

// emulatorArrow writes vals as the emulator writes a batch: a whole IPC
// stream, schema, batch and end-of-stream marker (none: the empty batch
// of its session's schema).
func emulatorArrow(t *testing.T, vals ...int64) []byte {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{{Name: "a", Type: arrow.PrimitiveTypes.Int64}}, nil)
	b := array.NewRecordBuilder(memory.NewGoAllocator(), schema)
	defer b.Release()
	b.Field(0).(*array.Int64Builder).AppendValues(vals, nil)
	rec := b.NewRecord()
	defer rec.Release()
	var buf bytes.Buffer
	w := ipc.NewWriter(&buf, ipc.WithSchema(schema))
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readAsTheGoClient reads batch after schema, as the Go client's
// ArrowRecordBatch does, and returns the values.
func readAsTheGoClient(t *testing.T, schema, batch []byte) []int64 {
	t.Helper()
	r, err := ipc.NewReader(bytes.NewReader(append(append([]byte{}, schema...), batch...)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer r.Release()
	var out []int64
	for r.Next() {
		out = append(out, r.Record().Column(0).(*array.Int64).Int64Values()...)
	}
	return out
}

// TestArrowIsFramedAsBigQueryFramesIt (#1046): the emulator's session
// schema and batches, each a whole IPC stream, are sent as the schema's
// message and the batch's, which a client reads one after the other; what
// is framed so already is sent as it is.
func TestArrowIsFramedAsBigQueryFramesIt(t *testing.T) {
	sessionSchema := emulatorArrow(t)
	batch := emulatorArrow(t, 1, 2, 3)
	if got := readAsTheGoClient(t, sessionSchema, batch); len(got) != 0 {
		t.Fatalf("the emulator's framing read %v; the test expects it to read no rows, as measured", got)
	}
	schema, rows := arrowSchemaMessage(sessionSchema), arrowBatchMessages(batch)
	if got := readAsTheGoClient(t, schema, rows); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Errorf("read after the schema: %v, want [1 2 3]", got)
	}
	if !bytes.Equal(arrowSchemaMessage(schema), schema) || !bytes.Equal(arrowBatchMessages(rows), rows) {
		t.Error("framing BigQuery's framing again changed it")
	}
	if got := arrowBatchMessages([]byte("not arrow")); string(got) != "not arrow" {
		t.Errorf("a text that is not Arrow: %q", got)
	}

}
