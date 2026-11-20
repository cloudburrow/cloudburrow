package main

import (
	"testing"

	"cloud.google.com/go/bigtable"
)

// TestBigtableRowKeySchemaFromTheForm (#1900): Create table's row key
// schema fields make the client's StructType, and a wrong one is refused.
func TestBigtableRowKeySchemaFromTheForm(t *testing.T) {
	st, err := bigtableRowKeySchema(map[string]string{"rowKeyEncoding": "DelimitedBytes", "rowKeyDelimiter": "#",
		"rowKeySegments": `[{"segment":"user_id","type":"BYTES","byteEncoding":"Default"},{"segment":"n","type":"INT64","byteEncoding":"OrderedCodeBytes"},{"segment":"at","type":"TIMESTAMP","byteEncoding":"Default"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if got := describeRowKeySchema(st); got != `user_id BYTES, n INT64, at TIMESTAMP (DelimitedBytes "#")` {
		t.Errorf("describeRowKeySchema = %s", got)
	}
	if _, ok := st.Fields[1].FieldType.(bigtable.Int64Type).Encoding.(bigtable.Int64OrderedCodeBytesEncoding); !ok {
		t.Errorf("segment n's encoding = %#v", st.Fields[1].FieldType)
	}
	for _, bad := range []map[string]string{
		{"rowKeyEncoding": "Singleton", "rowKeySegments": ""},
		{"rowKeyEncoding": "Singleton", "rowKeySegments": `[{"segment":"s","type":"STRING","byteEncoding":"BigEndianBytes"}]`},
		{"rowKeyEncoding": "Other", "rowKeySegments": `[{"segment":"s","type":"STRING"}]`},
	} {
		if _, err := bigtableRowKeySchema(bad); err == nil {
			t.Errorf("bigtableRowKeySchema(%v): no error", bad)
		}
	}
}
