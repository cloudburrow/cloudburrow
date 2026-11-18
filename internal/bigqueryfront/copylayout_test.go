package bigqueryfront

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLayoutMismatch(t *testing.T) {
	l := func(s string) map[string]json.RawMessage { return layoutOf([]byte(s)) }
	day := l(`{"timePartitioning":{"type":"DAY","field":"d","expirationMs":"86400000"},"clustering":{"fields":["n"]}}`)
	dayOther := l(`{"timePartitioning":{"field":"D","expirationMs":"1"},"clustering":{"fields":["N"]}}`)
	month := l(`{"timePartitioning":{"type":"MONTH","field":"d"}}`)
	ingest := l(`{"timePartitioning":{"type":"DAY"}}`)
	rng := l(`{"rangePartitioning":{"field":"n","range":{"start":"0","end":"10","interval":"1"}}}`)
	none := l(`{"numRows":"3"}`)
	ref := func(id string) tableRef { return tableRef{DatasetID: "ds", TableID: id} }
	for _, c := range []struct {
		name       string
		srcs       []map[string]json.RawMessage
		dest       map[string]json.RawMessage
		destExists bool
		want       string
	}{
		{"new destination", []map[string]json.RawMessage{day}, nil, false, ""},
		{"same spec, other expiration and case", []map[string]json.RawMessage{day}, dayOther, true, ""},
		{"other unit", []map[string]json.RawMessage{day}, month, true, "Expects partitioning specification time(type:MONTH"},
		{"time into range", []map[string]json.RawMessage{day}, rng, true, "Incompatible table partitioning"},
		{"partitioned into unpartitioned", []map[string]json.RawMessage{month}, none, true, ""},
		{"clustered into plain", []map[string]json.RawMessage{day}, none, true, ""},
		{"unpartitioned into ingestion", []map[string]json.RawMessage{none}, ingest, true, ""},
		{"unpartitioned into column", []map[string]json.RawMessage{none}, month, true, "only supported for ingestion-time"},
		{"clustering differs", []map[string]json.RawMessage{day}, l(`{"timePartitioning":{"field":"d"}}`), true, "clustering specification"},
		{"mixture", []map[string]json.RawMessage{day, none}, nil, false, "mixture"},
		{"sources differ", []map[string]json.RawMessage{day, month}, nil, false, "identical schemas"},
		{"sources match", []map[string]json.RawMessage{day, dayOther}, nil, false, ""},
	} {
		srcs := make([]tableRef, len(c.srcs))
		for i := range srcs {
			srcs[i] = ref("s")
		}
		got := layoutMismatch("p", srcs, c.srcs, ref("d"), c.dest, c.destExists)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
