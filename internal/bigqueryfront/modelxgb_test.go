package bigqueryfront

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseXGBoostDDL (#1711): CREATE MODEL of an imported XGBoost model,
// with and without its INPUT and OUTPUT clauses, and the statement's
// refusals.
func TestParseXGBoostDDL(t *testing.T) {
	parse := func(sql string) (modelDDL, int, string) {
		t.Helper()
		toks, ok := lex(sql)
		if !ok {
			t.Fatalf("lex %q", sql)
		}
		d, _, code, msg := parseModelDDL(toks, sql)
		return d, code, msg
	}
	d, code, msg := parse("CREATE OR REPLACE MODEL `p.d.m` INPUT(f1 int64, f2 float64, `f 3` FLOAT64) OUTPUT(predicted_label float64) " +
		"OPTIONS (MODEL_TYPE = 'XGBOOST', MODEL_PATH = 'gs://b/xgboost-model/*')")
	if code != 0 {
		t.Fatal(code, msg)
	}
	if d.opts.ModelType != "XGBOOST" || d.onnxPath != "gs://b/xgboost-model/*" || !d.replace ||
		!reflect.DeepEqual(d.importInputs, []importColumn{{"f1", "INT64"}, {"f2", "FLOAT64"}, {"f 3", "FLOAT64"}}) ||
		!reflect.DeepEqual(d.importOutputs, []importColumn{{"predicted_label", "FLOAT64"}}) {
		t.Errorf("parsed %+v", d)
	}
	d, code, _ = parse("CREATE MODEL m INPUT(a FLOAT64) OUTPUT(p ARRAY<FLOAT64>) OPTIONS(model_type='xgboost', model_path='gs://b/m.json')")
	if code != 0 || !reflect.DeepEqual(d.importOutputs, []importColumn{{"p", "ARRAY<FLOAT64>"}}) {
		t.Errorf("array output: %d %+v", code, d.importOutputs)
	}
	if d, code, _ = parse("CREATE MODEL m OPTIONS(model_type='XGBOOST', model_path='gs://b/m.bst')"); code != 0 || d.importInputs != nil {
		t.Errorf("no INPUT: %d", code)
	}
	for _, tc := range []struct{ sql, want string }{
		{"CREATE MODEL m OPTIONS(model_type='XGBOOST')", "requires the model_path option"},
		{"CREATE MODEL m OPTIONS(model_type='XGBOOST', model_path='b/m.bst')", "Cloud Storage URI"},
		{"CREATE MODEL m OPTIONS(model_type='XGBOOST', model_path='gs://b/m.bst', l2_reg=1)", "not supported for model_type XGBOOST"},
		{"CREATE MODEL m OPTIONS(model_type='XGBOOST', model_path='gs://b/m.bst') AS SELECT 1 AS a", "takes no query"},
		{"CREATE MODEL m INPUT(a FLOAT64) OPTIONS(model_type='XGBOOST', model_path='gs://b/m.bst')", "OUTPUT clause"},
		{"CREATE MODEL m INPUT(a FLOAT64) OUTPUT(p FLOAT64) OPTIONS(model_type='ONNX', model_path='gs://b/m.onnx')", "takes no INPUT"},
		{"CREATE MODEL m INPUT(a FLOAT64) OUTPUT(p FLOAT64) OPTIONS(model_type='LINEAR_REG') AS SELECT 1 AS a, 2 AS label", "takes no INPUT"},
	} {
		if _, code, msg := parse(tc.sql); code != 400 || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: %d %q, want 400 %q", tc.sql, code, msg, tc.want)
		}
	}
}
