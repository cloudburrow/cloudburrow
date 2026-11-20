package pubsubfront

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// A change's row, encoded in the writer schema the front makes of the
// table (#1999), decodes to the values and pseudo-columns given.
func TestEncodeChange(t *testing.T) {
	cols := []bqField{{Name: "id", Type: "INTEGER"}, {Name: "ts", Type: "TIMESTAMP"}, {Name: "b", Type: "BYTES"},
		{Name: "weird-name", Type: "STRING"}, {Name: "rec", Type: "RECORD", Fields: []bqField{{Name: "x", Type: "FLOAT", Mode: "REPEATED"}}}}
	_, md, err := changeDescriptor(cols)
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"id": "7", "ts": "2026-01-02T03:04:05.000006Z", "b": "AAE=", "weird-name": "w",
		"rec": map[string]any{"x": []any{1.5, 2.0}}}
	b, err := encodeChange(md, cols, cdcRow{row: row, ch: &msgChange{typ: "UPSERT", seq: "A/1", hasSeq: true}})
	if err != nil {
		t.Fatal(err)
	}
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(b, m); err != nil {
		t.Fatal(err)
	}
	f := func(n string) protoreflect.Value { return m.Get(md.Fields().ByName(protoreflect.Name(n))) }
	if f("id").Int() != 7 || f("ts").Int() != 1767323045000006 || string(f("b").Bytes()) != "\x00\x01" || f("col_4").String() != "w" ||
		f("_CHANGE_TYPE").String() != "UPSERT" || f("_CHANGE_SEQUENCE_NUMBER").String() != "A/1" {
		t.Errorf("decoded %v", m)
	}
	if l := f("rec").Message().Get(md.Fields().ByName("rec").Message().Fields().ByName("x")).List(); l.Len() != 2 || l.Get(1).Float() != 2 {
		t.Errorf("rec = %v", f("rec"))
	}
}

func TestTakeChange(t *testing.T) {
	row := map[string]any{"id": 1, "_change_type": "DELETE"}
	ch, err := takeChange(row)
	if err != nil || ch == nil || ch.typ != "DELETE" || ch.hasSeq || len(row) != 1 {
		t.Errorf("takeChange = %+v, %v, row %v", ch, err, row)
	}
	if _, err := takeChange(map[string]any{"_CHANGE_SEQUENCE_NUMBER": "1"}); err == nil {
		t.Errorf("a sequence number without _CHANGE_TYPE is accepted")
	}
	if ch, err := takeChange(map[string]any{"id": 1}); ch != nil || err != nil {
		t.Errorf("a row without the pseudo-columns: %v, %v", ch, err)
	}
}
