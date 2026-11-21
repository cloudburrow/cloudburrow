package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// The TensorFlow runtime of imported TensorFlow models (#1775).
//
// BigQuery runs an imported SavedModel with TensorFlow itself
// (bigqueryml-syntax-create-tensorflow: "Only core TensorFlow operations
// are supported"; it names no TensorFlow Lite limit), so a model whose
// graph needs TensorFlow operators TensorFlow Lite has no builtin for
// (Flex, SELECT_TF_OPS: tf.strings.split's StringSplitV2, say) or that
// TFLiteConverter cannot otherwise lower is imported all the same, and
// predicts on TensorFlow: the CLI's converter (internal/tfconvert) keeps a
// long-lived predictor container of Google's pinned tensorflow/tensorflow
// image per model, on its --internal Docker network (no route out), and
// ML.PREDICT sends it the query's rows in batches (tfRuntimeBatch rows a
// request; the predictor stacks rows of the same shapes into one batch).
// The model's columns are its SavedModel's serving_default's, after the
// converter's tf.train.Example and SparseTensor rewrites, as for a model
// that converts (modeltensorflow.go); its bundle (the SavedModel and its
// signature) is kept with the model.
//
// ML.EXPLAIN_PREDICT: the create-tensorflow page lists the functions that
// do not take a TensorFlow model, and ML.EXPLAIN_PREDICT is not one; it
// and troubleshoot-queries#tensorflow_worker_oom name ML.EXPLAIN_PREDICT
// among the "resource-intensive functions" an imported TensorFlow model
// runs. BigQuery's documented attribution method for a differentiable
// model is Integrated Gradients, with INTEGRATED_GRADIENTS_NUM_STEPS ("the
// number of steps to sample between the example being explained and its
// baseline ... used to approximate the integral in integrated gradients
// attribution methods", Google's BigQuery DataFrames reference of
// explain_predict; 15 by default, as BigQuery's DNN models documented it),
// and its output columns are the explain-predict page's
// top_feature_attributions, baseline_prediction_value, prediction_value
// and approximation_error. So it runs on TensorFlow: Integrated Gradients
// (TensorFlow's own tutorial: the trapezoidal Riemann sum over the steps)
// from a baseline of zeros of each floating-point input, of the model's
// first floating-point output by name (else its first integer or BOOL
// one), its predicted element when the output is
// an ARRAY (its largest), each input column one feature, the sum of its
// elements' attributions. TOP_K_FEATURES is 5 by default.
//
// UNVERIFIED (docs re-read 2026-11-21: bigqueryml-syntax-create-tensorflow,
// bigqueryml-syntax-explain-predict, xai-overview, troubleshoot-queries),
// as Google does not say for imported TensorFlow models: the baseline
// (zeros), which output and element are explained, that integer, BOOL and
// STRING inputs (no gradient) are held at their values and given no
// attribution, approximation_error as |sum of attributions −
// (prediction − baseline)| / |prediction − baseline| (Integrated
// Gradients' completeness gap), and that the model's own output columns
// lead the row, as in ML.PREDICT.

const (
	tfRuntimeTensorFlow = "tensorflow"
	tfRuntimeBatch      = 512
	tfDefaultIGSteps    = 15
)

// tfExplainColumns are ML.EXPLAIN_PREDICT's columns after the model's
// outputs; tfExplainNames their names.
const tfExplainColumns = "top_feature_attributions ARRAY<STRUCT<feature STRING, attribution FLOAT64>>, baseline_prediction_value FLOAT64, " +
	"prediction_value FLOAT64, approximation_error FLOAT64"

var tfExplainNames = []string{"top_feature_attributions", "baseline_prediction_value", "prediction_value", "approximation_error"}

// tfExplainOptions are ML.EXPLAIN_PREDICT's options of a TensorFlow model.
type tfExplainOptions struct {
	topK, steps int
	output      string
}

// tfExplainOpts reads and checks ML.EXPLAIN_PREDICT's STRUCT.
func tfExplainOpts(md bqModel, c mlCall) (tfExplainOptions, int, string) {
	o := tfExplainOptions{topK: 5, steps: tfDefaultIGSteps}
	if !md.TFLite.Bundle {
		return o, 501, "Not implemented here: ML.EXPLAIN_PREDICT of an imported TensorFlow model CloudBurrow imported before it kept the SavedModel; " +
			"run CREATE OR REPLACE MODEL again to explain it. Nothing was run."
	}
	for k, v := range c.struc {
		switch k {
		case "top_k_features":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return o, 400, "Invalid top_k_features " + v + ": it must be a positive integer"
			}
			o.topK = n
		case "integrated_gradients_num_steps":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 1000 {
				return o, 400, "Invalid integrated_gradients_num_steps " + v + ": it must be an integer between 1 and 1000"
			}
			o.steps = n
		case "threshold":
			return o, 400, "The threshold option is only supported for binary classification models"
		case "approx_feature_contrib":
			return o, 400, "The approx_feature_contrib option is only supported for boosted tree and random forest models"
		default:
			return o, 400, "Invalid option " + k + " for ML.EXPLAIN_PREDICT of a TensorFlow model: it takes top_k_features and integrated_gradients_num_steps"
		}
	}
	outs := append([]tflitePortCol(nil), md.TFLite.Outputs...)
	sort.Slice(outs, func(i, j int) bool { return outs[i].Name < outs[j].Name })
	// The first floating-point output by name, else the first integer
	// or BOOL one.
	for _, want := range []string{"FLOAT64", "INT64", "BOOL"} {
		for _, p := range outs {
			if o.output == "" && p.Type == want {
				o.output = p.Name
			}
		}
	}
	if o.output == "" {
		return o, 400, "ML.EXPLAIN_PREDICT of the TensorFlow model needs a numeric output to attribute; its outputs are all STRING"
	}
	return o, 0, ""
}

// tfDType maps a TensorFlow dtype's name to the TensorFlow Lite type of
// the same BigQuery column (tfliteColumnOf), Resrc for one BigQuery does
// not support.
func tfDType(name string) tflitemodel.Type {
	switch name {
	case "float16", "bfloat16":
		return tflitemodel.Float16
	case "float32":
		return tflitemodel.Float32
	case "float64":
		return tflitemodel.Float64
	case "int8":
		return tflitemodel.Int8
	case "int16":
		return tflitemodel.Int16
	case "int32":
		return tflitemodel.Int32
	case "int64":
		return tflitemodel.Int64
	case "uint8":
		return tflitemodel.Uint8
	case "uint16":
		return tflitemodel.Uint16
	case "uint32":
		return tflitemodel.Uint32
	case "uint64":
		return tflitemodel.Uint64
	case "bool":
		return tflitemodel.Bool
	case "string":
		return tflitemodel.String
	case "complex64":
		return tflitemodel.Complex
	case "complex128":
		return tflitemodel.Cplx128
	case "variant":
		return tflitemodel.Variant
	}
	return tflitemodel.Resrc
}

// tfSignaturePorts maps a bundle's serving_default to columns, for a
// model that runs on TensorFlow.
func tfSignaturePorts(uri string, sig tfconvert.Signature) (*tfliteImport, int, string) {
	imp := &tfliteImport{URI: uri}
	for k, ports := range [][]tfconvert.Port{sig.Inputs, sig.Outputs} {
		what := []string{"input", "output"}[k]
		for _, p := range ports {
			if p.Shape == nil {
				return nil, 501, fmt.Sprintf("Not implemented here: the TensorFlow model %s %s '%s' has an unknown rank: CloudBurrow cannot tell its shape "+
					"from a row. Nothing was run.", uri, what, p.Name)
			}
			t := tfDType(p.DType)
			c, code, msg := tfliteColumnOf(tflitemodel.Port{Name: p.Name, Type: t, Shape: p.Shape}, what)
			if code != 0 {
				msg = strings.Replace(msg, "TensorFlow Lite model", "TensorFlow model", 1)
				msg = strings.Replace(msg, "unsupported TensorFlow Lite type: "+t.String(), "unsupported TensorFlow type: "+p.DType, 1)
				return nil, code, msg
			}
			if dims, ok := sig.Sparse[p.Name]; ok && k == 0 {
				c.Sparse, c.SparseDims, c.Array = true, dims, true
			}
			if k == 0 {
				imp.Inputs = append(imp.Inputs, c)
			} else {
				imp.Outputs = append(imp.Outputs, c)
			}
		}
	}
	if len(imp.Inputs) == 0 || len(imp.Outputs) == 0 {
		return nil, 400, "Failed to load the TensorFlow model " + uri + ": it has no inputs or no outputs"
	}
	return imp, 0, ""
}

// tfValue is a tensor on the predictor's wire: its shape and its values,
// flat and row-major.
type tfValue struct {
	Shape  []int `json:"shape"`
	Values []any `json:"values"`
}

// tfRuntimeInput is a row's JSON value of input c on the wire.
func tfRuntimeInput(c tflitePortCol, raw json.RawMessage) (tfValue, string) {
	if c.image {
		t, why := tfliteImageTensor(c, raw)
		if why != "" {
			return tfValue{}, why
		}
		v := tfValue{Shape: t.Shape}
		for _, x := range t.F {
			v.Values = append(v.Values, float64(x))
		}
		for _, x := range t.I {
			v.Values = append(v.Values, x)
		}
		return v, ""
	}
	shape, flat, strs, why := tfliteFlat(c, raw)
	if why != "" {
		return tfValue{}, why
	}
	v := tfValue{Shape: shape, Values: []any{}}
	if shape == nil {
		v.Shape = []int{}
	}
	switch c.Type {
	case "STRING":
		for _, s := range strs {
			v.Values = append(v.Values, s)
		}
	case "INT64":
		for _, x := range flat {
			v.Values = append(v.Values, int64(x))
		}
	case "BOOL":
		for _, x := range flat {
			v.Values = append(v.Values, x != 0)
		}
	default:
		for _, x := range flat {
			v.Values = append(v.Values, x)
		}
	}
	return v, ""
}

// tfNumber reads a wire number: a json.Number, or "NaN", "Infinity" or
// "-Infinity".
func tfNumber(x any) (float64, bool) {
	switch e := x.(type) {
	case json.Number:
		f, err := e.Float64()
		return f, err == nil
	case string:
		switch e {
		case "NaN":
			return math.NaN(), true
		case "Infinity":
			return math.Inf(1), true
		case "-Infinity":
			return math.Inf(-1), true
		}
	case bool:
		return map[bool]float64{true: 1}[e], true
	}
	return 0, false
}

// tfFloatLiteral is v as a FLOAT64 SQL literal.
func tfFloatLiteral(v float64) string {
	switch {
	case math.IsNaN(v):
		return "CAST('NaN' AS FLOAT64)"
	case math.IsInf(v, 1):
		return "CAST('Infinity' AS FLOAT64)"
	case math.IsInf(v, -1):
		return "CAST('-Infinity' AS FLOAT64)"
	}
	return sqlF(v)
}

// tfRuntimeLiteral is an output's value on the wire as SQL.
func tfRuntimeLiteral(c tflitePortCol, v tfValue) (string, error) {
	lit := func(x any) (string, error) {
		switch c.Type {
		case "STRING":
			s, ok := x.(string)
			if !ok {
				return "", fmt.Errorf("output %s is not a STRING", c.Name)
			}
			return sqlString(s), nil
		case "BOOL":
			if b, ok := x.(bool); ok {
				return map[bool]string{true: "TRUE", false: "FALSE"}[b], nil
			}
			f, ok := tfNumber(x)
			if !ok {
				return "", fmt.Errorf("output %s is not a BOOL", c.Name)
			}
			return map[bool]string{true: "TRUE", false: "FALSE"}[f != 0], nil
		case "INT64":
			if n, ok := x.(json.Number); ok {
				if i, err := n.Int64(); err == nil {
					return strconv.FormatInt(i, 10), nil
				}
			}
			f, ok := tfNumber(x)
			if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
				return "", fmt.Errorf("output %s is not an integer", c.Name)
			}
			return strconv.FormatInt(int64(f), 10), nil
		}
		f, ok := tfNumber(x)
		if !ok {
			return "", fmt.Errorf("output %s is not a number", c.Name)
		}
		return tfFloatLiteral(f), nil
	}
	if !c.Array {
		if len(v.Values) == 0 {
			return "NULL", nil
		}
		return lit(v.Values[0])
	}
	var vs []string
	for _, x := range v.Values {
		s, err := lit(x)
		if err != nil {
			return "", err
		}
		vs = append(vs, s)
	}
	return "ARRAY<" + c.Type + ">[" + strings.Join(vs, ", ") + "]", nil
}

// tfRuntimeRows runs the distinct input rows (each a TO_JSON_STRING of
// the inputs c0, c1, ...) on the TensorFlow runtime and answers each
// one's STRUCT literal: its key, its outputs and, for ML.EXPLAIN_PREDICT,
// tfExplainColumns.
func (f front) tfRuntimeRows(r *http.Request, imp *tfliteImport, inputs []tflitePortCol, rows [][]*string, bundle []byte, explain bool,
	xo tfExplainOptions) ([]string, int, string) {
	if f.tfconvert == nil {
		return nil, 501, "Not implemented here: the TensorFlow model " + imp.URI + " runs on TensorFlow, and this BigQuery front was given no " +
			"TensorFlow converter (--tfconvert), which `cloudburrow up` serves with BigQuery enabled. Nothing was run."
	}
	fn := "ML.PREDICT"
	if explain {
		fn = "ML.EXPLAIN_PREDICT"
	}
	var keys []string
	var feeds []map[string]tfValue
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		var vals map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*row[0]), &vals); err != nil {
			return nil, 500, "cloudburrow: could not read the input's values: " + err.Error()
		}
		feed := map[string]tfValue{}
		for i, in := range inputs {
			v, why := tfRuntimeInput(in, vals["c"+strconv.Itoa(i)])
			if why != "" {
				return nil, 400, fn + " input column " + in.Name + " " + why
			}
			feed[in.Name] = v
		}
		keys = append(keys, *row[0])
		feeds = append(feeds, feed)
	}
	floatIn := map[string]bool{}
	for _, in := range inputs {
		floatIn[in.Name] = in.Type == "FLOAT64"
	}
	var lits []string
	for lo := 0; lo < len(feeds); lo += tfRuntimeBatch {
		hi := min(lo+tfRuntimeBatch, len(feeds))
		req := map[string]any{"op": "predict", "rows": feeds[lo:hi]}
		if explain {
			req = map[string]any{"op": "explain", "rows": feeds[lo:hi], "output": xo.output, "steps": xo.steps}
		}
		body, _ := json.Marshal(req)
		ans, err := f.tfconvert.Run(r.Context(), imp.SHA256, func() ([]byte, error) { return bundle, nil }, body)
		if err != nil {
			var rf *tfconvert.Refusal
			if errors.As(err, &rf) && rf.Kind == "run" {
				return nil, 400, "The TensorFlow model failed to run: " + rf.Message
			}
			return nil, 501, "Not implemented here: the TensorFlow model " + imp.URI + ": CloudBurrow's TensorFlow runtime cannot run: " + err.Error() + ". Nothing was run."
		}
		type explained struct {
			Outputs      map[string]tfValue `json:"outputs"`
			Attributions map[string]any     `json:"attributions"`
			Prediction   any                `json:"prediction"`
			Baseline     any                `json:"baseline"`
		}
		var got struct {
			Rows []json.RawMessage `json:"rows"`
		}
		dec := json.NewDecoder(bytes.NewReader(ans))
		dec.UseNumber()
		if err := dec.Decode(&got); err != nil || len(got.Rows) != hi-lo {
			return nil, 500, fmt.Sprintf("cloudburrow: the TensorFlow runtime's answer is not one row per input row (%d for %d): %v", len(got.Rows), hi-lo, err)
		}
		for k, raw := range got.Rows {
			var x explained
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var err error
			if explain {
				err = dec.Decode(&x)
			} else {
				err = dec.Decode(&x.Outputs)
			}
			if err != nil {
				return nil, 500, "cloudburrow: could not read the TensorFlow runtime's answer: " + err.Error()
			}
			parts := []string{sqlString(keys[lo+k])}
			for _, o := range imp.Outputs {
				v, ok := x.Outputs[o.Name]
				if !ok {
					return nil, 500, "cloudburrow: the TensorFlow runtime answered no output " + o.Name
				}
				s, err := tfRuntimeLiteral(o, v)
				if err != nil {
					return nil, 500, "cloudburrow: the TensorFlow runtime's " + err.Error()
				}
				parts = append(parts, s)
			}
			if explain {
				parts = append(parts, tfExplainLiterals(inputs, floatIn, x.Attributions, x.Prediction, x.Baseline, xo.topK)...)
			}
			lits = append(lits, "STRUCT("+strings.Join(parts, ", ")+")")
		}
	}
	return lits, 0, ""
}

// tfExplainLiterals are a row's tfExplainColumns.
func tfExplainLiterals(inputs []tflitePortCol, floatIn map[string]bool, attr map[string]any, pred, base any, topK int) []string {
	type fa struct {
		name string
		v    float64
	}
	var as []fa
	sum := 0.0
	for _, in := range inputs {
		if !floatIn[in.Name] {
			continue
		}
		v, _ := tfNumber(attr[in.Name])
		as = append(as, fa{in.Name, v})
		sum += v
	}
	sort.SliceStable(as, func(i, j int) bool { return math.Abs(as[i].v) > math.Abs(as[j].v) })
	if len(as) > topK {
		as = as[:topK]
	}
	var items []string
	for _, a := range as {
		items = append(items, "STRUCT("+sqlString(a.name)+" AS feature, "+tfFloatLiteral(a.v)+" AS attribution)")
	}
	p, _ := tfNumber(pred)
	b, _ := tfNumber(base)
	gap := math.Abs(sum - (p - b))
	if d := math.Abs(p - b); d > 0 {
		gap /= d
	}
	return []string{"ARRAY<STRUCT<feature STRING, attribution FLOAT64>>[" + strings.Join(items, ", ") + "]", tfFloatLiteral(b), tfFloatLiteral(p), tfFloatLiteral(gap)}
}
