package bigqueryfront

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
)

// TestParseONNXModel (#1711): CREATE MODEL of model_type ONNX takes
// model_path, and no query or TRANSFORM. Whether the instance can run it
// is asked when it is created (TestONNXWithoutARunner).
func TestParseONNXModel(t *testing.T) {
	ok := "CREATE OR REPLACE MODEL ds.m OPTIONS(model_type='ONNX', model_path='gs://b/models/*')"
	toks, _ := lex(ok)
	d, is, code, msg := parseModelDDL(toks, ok)
	if !is || code != 0 || d.onnxPath != "gs://b/models/*" || d.opts.ModelType != "ONNX" || !d.replace {
		t.Fatalf("%v %d %s %+v", is, code, msg, d)
	}
	for sql, want := range map[string]int{
		"CREATE MODEL ds.m OPTIONS(model_type='onnx')":                                               400,
		"CREATE MODEL ds.m OPTIONS(model_type='onnx', model_path='/tmp/m.onnx')":                     400,
		"CREATE MODEL ds.m OPTIONS(model_type='onnx', model_path='gs://b/m.onnx') AS SELECT 1":       400,
		"CREATE MODEL ds.m OPTIONS(model_type='onnx', model_path='gs://b/m.onnx', l2_reg=0.1)":       400,
		"CREATE MODEL ds.m OPTIONS(model_type='onnx', model_path='gs://b/m.onnx', kms_key_name='k')": 501,
		"CREATE MODEL ds.m TRANSFORM(x) OPTIONS(model_type='onnx', model_path='gs://b/m.onnx')":      400,
	} {
		toks, _ := lex(sql)
		if _, is, code, msg := parseModelDDL(toks, sql); !is || code != want {
			t.Errorf("%s: %v %d %s, want %d", sql, is, code, msg, want)
		}
	}
}

// TestONNXColumns: the page's type table, and a tensor's dimensions after
// the batch dimension making a value or an ARRAY of a row.
func TestONNXColumns(t *testing.T) {
	for _, c := range []struct {
		p    onnxmodel.Port
		want onnxColumn
		why  string
	}{
		{onnxmodel.Port{Name: "a", Kind: onnxmodel.KindTensor, Elem: "FLOAT", Dims: []int64{-1, 1}}, onnxColumn{typ: "FLOAT64", width: 1}, ""},
		{onnxmodel.Port{Name: "l", Kind: onnxmodel.KindTensor, Elem: "INT64", Dims: []int64{-1}}, onnxColumn{typ: "INT64", width: 1}, ""},
		{onnxmodel.Port{Name: "u", Kind: onnxmodel.KindTensor, Elem: "UINT8", Dims: []int64{-1, 2, 3}}, onnxColumn{typ: "INT64", array: true, width: 6}, ""},
		{onnxmodel.Port{Name: "h", Kind: onnxmodel.KindTensor, Elem: "FLOAT16", Dims: []int64{-1, -1}}, onnxColumn{typ: "FLOAT64", array: true, width: -1}, ""},
		{onnxmodel.Port{Name: "s", Kind: onnxmodel.KindTensor, Elem: "STRING", Dims: []int64{-1, 1}}, onnxColumn{typ: "STRING", width: 1}, ""},
		{onnxmodel.Port{Name: "b", Kind: onnxmodel.KindTensor, Elem: "BOOL"}, onnxColumn{typ: "BOOL", width: 1}, ""},
		{onnxmodel.Port{Name: "c", Kind: onnxmodel.KindTensor, Elem: "COMPLEX64", Dims: []int64{-1}}, onnxColumn{}, "unsupported ONNX tensor element type: COMPLEX64"},
		{onnxmodel.Port{Name: "output_probability", Kind: "ONNX_TYPE_SEQUENCE"}, onnxColumn{},
			"ONNX model output 'output_probability' has unsupported ONNX type: ONNX_TYPE_SEQUENCE."},
	} {
		got, why := onnxColumnOf(c.p, "output")
		if c.why != "" {
			if !strings.Contains(why, c.why) {
				t.Errorf("%s: %q, want %q", c.p.Name, why, c.why)
			}
			continue
		}
		if why != "" || got != c.want {
			t.Errorf("%s: %+v %q, want %+v", c.p.Name, got, why, c.want)
		}
	}
	for _, c := range []struct {
		t    string
		col  onnxColumn
		want bool
	}{
		{"FLOAT", onnxColumn{typ: "FLOAT64"}, true},
		{"INTEGER", onnxColumn{typ: "FLOAT64"}, true},
		{"NUMERIC", onnxColumn{typ: "FLOAT64"}, true},
		{"FLOAT", onnxColumn{typ: "INT64"}, false},
		{"ARRAY<FLOAT>", onnxColumn{typ: "FLOAT64", array: true}, true},
		{"FLOAT", onnxColumn{typ: "FLOAT64", array: true}, false},
		{"STRING", onnxColumn{typ: "STRING"}, true},
		{"BOOLEAN", onnxColumn{typ: "BOOL"}, true},
	} {
		if got := onnxConvertible(c.t, c.col); got != c.want {
			t.Errorf("%s → %+v: %v", c.t, c.col, got)
		}
	}
}

func TestONNXShape(t *testing.T) {
	for _, c := range []struct {
		dims      []int64
		rows, per int
		want      []int64
		bad       bool
	}{
		{[]int64{-1, 1}, 3, 1, []int64{3, 1}, false},
		{[]int64{-1, 4}, 2, 4, []int64{2, 4}, false},
		{[]int64{1, -1}, 1, 5, []int64{1, 5}, false},
		{[]int64{-1, 2, -1}, 1, 6, []int64{1, 2, 3}, false},
		{[]int64{}, 1, 1, []int64{}, false},
		{[]int64{4, 1}, 1, 1, nil, true},
		{[]int64{-1, -1, -1}, 1, 4, nil, true},
	} {
		got, why := onnxShape(c.dims, c.rows, c.per)
		if (why != "") != c.bad || !c.bad && !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v %d %d: %v %q", c.dims, c.rows, c.per, got, why)
		}
	}
}
