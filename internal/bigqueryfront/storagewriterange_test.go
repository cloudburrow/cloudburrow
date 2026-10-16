package bigqueryfront

import (
	"reflect"
	"testing"

	"cloud.google.com/go/bigquery/storage/apiv1/storagepb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// rangeMessage is a RANGE message of start and end of type typ, as the Go
// client's adapt package makes one.
func rangeMessage(name string, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: []*descriptorpb.FieldDescriptorProto{
		{Name: proto.String("start"), Number: proto.Int32(1), Type: typ.Enum(), Label: opt},
		{Name: proto.String("end"), Number: proto.Int32(2), Type: typ.Enum(), Label: opt}}}
}

func rangeOf(elem string) *struct {
	Type string `json:"type"`
} {
	return &struct {
		Type string `json:"type"`
	}{elem}
}

// TestStorageWriteRanges (#1391): a proto RANGE message is written as
// insertAll's "[start, end)", a bound with no value UNBOUNDED.
func TestStorageWriteRanges(t *testing.T) {
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	msgT := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	fields := []field{{Name: "d", Type: "RANGE", RangeElementType: rangeOf("DATE")},
		{Name: "dt", Type: "RANGE", RangeElementType: rangeOf("DATETIME")},
		{Name: "ts", Type: "RANGE", RangeElementType: rangeOf("TIMESTAMP")}}
	dp := &descriptorpb.DescriptorProto{Name: proto.String("m"), Field: []*descriptorpb.FieldDescriptorProto{
		{Name: proto.String("d"), Number: proto.Int32(1), Type: msgT, TypeName: proto.String("rd"), Label: opt},
		{Name: proto.String("dt"), Number: proto.Int32(2), Type: msgT, TypeName: proto.String("rdt"), Label: opt},
		{Name: proto.String("ts"), Number: proto.Int32(3), Type: msgT, TypeName: proto.String("rts"), Label: opt},
	}, NestedType: []*descriptorpb.DescriptorProto{rangeMessage("rd", descriptorpb.FieldDescriptorProto_TYPE_INT32),
		rangeMessage("rdt", descriptorpb.FieldDescriptorProto_TYPE_INT64), rangeMessage("rts", descriptorpb.FieldDescriptorProto_TYPE_INT64)}}
	md := testMessage(t, dp)
	rc, err := newRowConv(md, fields, "")
	if err != nil {
		t.Fatal(err)
	}
	msg := dynamicpb.NewMessage(md)
	packed := int64(2020)<<46 | 1<<42 | 1<<37 | 3<<32 | 4<<26 | 5<<20 // 2020-01-01 03:04:05
	set := func(i int, start, end any) {
		f := md.Fields().Get(i)
		m := dynamicpb.NewMessage(f.Message())
		if start != nil {
			m.Set(f.Message().Fields().ByName("start"), protoreflect.ValueOf(start))
		}
		if end != nil {
			m.Set(f.Message().Fields().ByName("end"), protoreflect.ValueOf(end))
		}
		msg.Set(f, protoreflect.ValueOfMessage(m))
	}
	set(0, int32(18262), int32(18293))   // 2020-01-01, 2020-02-01
	set(1, packed, nil)                  // a packed DATETIME, UNBOUNDED
	set(2, nil, int64(1577836800000000)) // UNBOUNDED, 2020-01-01 UTC
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rc.row(b)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"d": "[2020-01-01, 2020-02-01)", "dt": "[2020-01-01 03:04:05.000000, UNBOUNDED)",
		"ts": "[UNBOUNDED, 2020-01-01 00:00:00.000000+00)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RANGEs: %v, want %v", got, want)
	}
	// No message: NULL.
	got, err = rc.row(nil)
	if err != nil || !reflect.DeepEqual(got, map[string]any{"d": nil, "dt": nil, "ts": nil}) {
		t.Errorf("no RANGEs: %v, %v", got, err)
	}
}

// TestStorageWriteMissingValues (#1391): DEFAULT_VALUE leaves a missing
// value of a column with a default out of the row, so the engine writes
// the default; NULL_VALUE in the map overrides the connection's default.
func TestStorageWriteMissingValues(t *testing.T) {
	fields := []field{{Name: "a", Type: "INT64", DefaultValueExpression: "7"}, {Name: "b", Type: "STRING", DefaultValueExpression: "'x'"},
		{Name: "c", Type: "STRING"}, {Name: "r", Type: "INT64", Mode: "REPEATED", DefaultValueExpression: "[1]"}}
	row := func() map[string]any { return map[string]any{"a": nil, "b": nil, "c": nil, "r": []any{}} }
	for name, tc := range map[string]struct {
		def  storagepb.AppendRowsRequest_MissingValueInterpretation
		m    map[string]storagepb.AppendRowsRequest_MissingValueInterpretation
		want map[string]any
	}{
		"none":         {0, nil, row()},
		"NULL_VALUE":   {storagepb.AppendRowsRequest_NULL_VALUE, nil, row()},
		"every column": {storagepb.AppendRowsRequest_DEFAULT_VALUE, nil, map[string]any{"c": nil, "r": []any{}}},
		"one column":   {0, map[string]storagepb.AppendRowsRequest_MissingValueInterpretation{"A": storagepb.AppendRowsRequest_DEFAULT_VALUE}, map[string]any{"b": nil, "c": nil, "r": []any{}}},
		"overridden": {storagepb.AppendRowsRequest_DEFAULT_VALUE, map[string]storagepb.AppendRowsRequest_MissingValueInterpretation{"b": storagepb.AppendRowsRequest_NULL_VALUE},
			map[string]any{"b": nil, "c": nil, "r": []any{}}},
	} {
		mv, err := missingInterpretations(tc.def, tc.m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := mv.apply(fields, row()); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	// A value given is kept.
	mv, _ := missingInterpretations(storagepb.AppendRowsRequest_DEFAULT_VALUE, nil)
	if got := mv.apply(fields, map[string]any{"a": "1"}); !reflect.DeepEqual(got, map[string]any{"a": "1"}) {
		t.Errorf("a value given: %v", got)
	}
	// A REQUIRED column with a default may be left out of a row.
	req := []field{{Name: "a", Type: "INT64", Mode: "REQUIRED", DefaultValueExpression: "7"}}
	if _, errs := checkRow(req, map[string]any{}, false); len(errs) != 0 {
		t.Errorf("a REQUIRED column with a default left out: %v", errs)
	}
	if _, errs := checkRow(req, map[string]any{"a": nil}, false); len(errs) != 1 {
		t.Errorf("a REQUIRED column with a default given NULL: %v", errs)
	}
}
