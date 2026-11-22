package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
	"github.com/cloudburrow/cloudburrow/internal/tflitemodel"
)

// Imported TensorFlow Lite models (#1755): CREATE MODEL … OPTIONS
// (MODEL_TYPE='TENSORFLOW_LITE', MODEL_PATH='gs://…' [, KMS_KEY_NAME]), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tflite
// documents it (read 2026-11-17):
//
//   - "TensorFlow Lite models must be in .tflite format" and "must be
//     stored in Cloud Storage": the .tflite file MODEL_PATH names (or the
//     one such file of a folder, gs://bucket/path/to/tflite_model/*) is
//     read from the instance's own Cloud Storage and kept with the model;
//   - "Models are limited to 450 MB in size";
//   - "You can only use TensorFlow Lite models with the ML.PREDICT
//     function";
//   - the type table: the integer types read and write INT64, FLOAT16,
//     FLOAT32 and FLOAT64 FLOAT64, BOOL BOOL and STRING STRING;
//     COMPLEX64, COMPLEX128, RESOURCE and VARIANT are unsupported;
//   - "Sparse tensors are not supported"; "SentencePiece operators are
//     not supported"; only TensorFlow core (and TensorFlow Text)
//     operations.
//
// ML.PREDICT (bigqueryml-syntax-predict): "the input must be convertible
// to the type expected by the model" and "the output is the output of the
// TensorFlow Lite model's predict method"; the input's columns are passed
// through. The model's inputs and outputs are its serving_default
// SignatureDef's (as LiteRT's signature runner names them), or its
// subgraph's tensors' names when it has none. As for ONNX (modelonnx.go),
// a port whose dimensions after the batch dimension are all 1 is a value
// of a row, one with more an ARRAY of the row's values, row-major; a
// fixed first dimension of more than one is part of the row's ARRAY too
// (#1775), as each row runs the whole tensor.
//
// How it runs: on internal/tflitemodel, a pure-Go interpreter of LiteRT's
// builtin operators checked against LiteRT's own output, so every build
// (and the CGO-less in-cluster front) runs it: float, quantized (int8,
// uint8; #1766) and STRING tensors (#1766), and control flow (WHILE and
// IF over the model's other subgraphs, #1775). An operator it does not
// run is 501, naming what is missing.
//
// UNVERIFIED (docs re-read 2026-11-17: bigqueryml-syntax-create-tflite), chosen here as Google does not say: each row
// runs as a batch of one; a NULL input is 400 (a TensorFlow Lite tensor
// has no missing value); the create-tflite page's "You can only use an
// imported TensorFlow Lite model with an object table" is not enforced,
// as the ML.PREDICT page takes any query whose columns convert. An image
// STRUCT column (ML.DECODE_IMAGE's over an object table, mlimage.go,
// #1798) is reshaped to an image input's tensor (tfliteImageTensor).

// maxTFLiteModelBytes is BigQuery's "Models are limited to 450 MB in size".
const maxTFLiteModelBytes = 450 << 20

// tfliteImport is what an imported TensorFlow Lite model keeps.
type tfliteImport struct {
	ModelPath string          `json:"modelPath"`
	URI       string          `json:"uri"`
	SHA256    string          `json:"sha256"`
	Size      int64           `json:"size"`
	Inputs    []tflitePortCol `json:"inputs"`
	Outputs   []tflitePortCol `json:"outputs"`
	// Bundle marks an imported TensorFlow model kept as the converter's
	// bundle (internal/tfconvert's bundle.go: the SavedModel, and its
	// TensorFlow Lite flatbuffer when there is one), SHA256 the bundle's;
	// a model imported before it keeps the flatbuffer alone.
	Bundle bool `json:"bundle,omitempty"`
	// Runtime is "tensorflow" for a model TensorFlow Lite's builtins do
	// not take, which predicts on TensorFlow itself (modeltfruntime.go),
	// with Fallback saying why; "" runs internal/tflitemodel.
	Runtime  string `json:"runtime,omitempty"`
	Fallback string `json:"fallback,omitempty"`
}

// tflitePortCol is how a model port reads or writes a column.
type tflitePortCol struct {
	Name string `json:"name"`
	// Type is the element's BigQuery type: INT64, FLOAT64, BOOL or STRING.
	Type  string `json:"type"`
	Array bool   `json:"array,omitempty"`
	// Shape is the port's, -1 where the model leaves it free.
	Shape []int `json:"shape"`
	// image is set for a query whose column is an image STRUCT.
	image bool
	// Sparse marks an imported TensorFlow model's SparseTensor input (or
	// VarLenFeature, #1775): the converter made it a dense [batch, n]
	// input of the row's values, and BigQuery takes "A NULL, the
	// associated BigQuery ML type, or an ARRAY of the associated BigQuery
	// ML type" for it. SparseDims is the SparseTensor's dense shape after
	// the batch, -1 where free.
	Sparse     bool  `json:"sparse,omitempty"`
	SparseDims []int `json:"sparseDims,omitempty"`
}

func (c tflitePortCol) sqlType() string {
	if c.Array {
		return "ARRAY<" + c.Type + ">"
	}
	return c.Type
}

// tfliteColumnOf maps a port to a column, or why it cannot: code 400 for
// a type BigQuery does not support, 501 for one CloudBurrow does not run.
func tfliteColumnOf(p tflitemodel.Port, what string) (tflitePortCol, int, string) {
	c := tflitePortCol{Name: p.Name, Shape: p.Shape}
	switch {
	case p.Type.IsInt():
		c.Type = "INT64"
	case p.Type.IsFloat():
		c.Type = "FLOAT64"
	case p.Type == tflitemodel.Bool:
		c.Type = "BOOL"
	case p.Type == tflitemodel.String:
		c.Type = "STRING" // #1766
	default:
		return c, 400, fmt.Sprintf("TensorFlow Lite model %s '%s' has unsupported TensorFlow Lite type: %s. BigQuery ML supports the integer types, "+
			"FLOAT16, FLOAT32, FLOAT64, BOOL and STRING.", what, p.Name, p.Type)
	}
	free := 0
	for k, d := range p.Shape {
		if k == 0 {
			// A fixed first dimension of more than one is not a batch a
			// row fills once: the row holds all its values (#1775).
			if d > 1 {
				c.Array = true
			}
			continue
		}
		if d < 0 {
			free++
			c.Array = true
		} else if d != 1 {
			c.Array = true
		}
	}
	if free > 1 {
		return c, 501, fmt.Sprintf("Not implemented here: TensorFlow Lite model %s '%s' has shape %v, with more than one free dimension after the batch: "+
			"CloudBurrow cannot tell its shape from a row's ARRAY. Nothing was run.", what, p.Name, p.Shape)
	}
	return c, 0, ""
}

// tfliteRefusal answers a model the interpreter cannot load.
func tfliteRefusal(uri string, err error) (int, string) {
	if tflitemodel.IsUnsupported(err) {
		return 501, "Not implemented here: the TensorFlow Lite model " + uri + ": " + err.Error() + ". Nothing was run."
	}
	return 400, "Failed to load the TensorFlow Lite model " + uri + ": " + err.Error()
}

// importTFLite reads and checks the model MODEL_PATH names.
func (f front) importTFLite(r *http.Request, d modelDDL) (*tfliteImport, []byte, int, string) {
	o, data, code, msg := f.readImportedModel(r.Context(), d.onnxPath, "TensorFlow Lite", []string{".tflite"}, maxTFLiteModelBytes, "450 MB")
	if code != 0 {
		return nil, nil, code, msg
	}
	imp, code, msg := tflitePorts(o.uri(), data, "TensorFlow Lite")
	if code != 0 {
		return nil, nil, code, msg
	}
	return imp, data, 0, ""
}

// tflitePorts loads a TensorFlow Lite flatbuffer and maps its ports to
// columns; kind names the model in a refusal ("TensorFlow Lite", or
// "TensorFlow" for a converted SavedModel).
func tflitePorts(uri string, data []byte, kind string) (*tfliteImport, int, string) {
	m, err := tflitemodel.Load(data)
	if err != nil {
		code, msg := tfliteRefusal(uri, err)
		return nil, code, strings.Replace(msg, "TensorFlow Lite model", kind+" model", 1)
	}
	imp := &tfliteImport{URI: uri, SHA256: tfliteSum(data), Size: int64(len(data))}
	var sparse map[string][]int
	if kind == "TensorFlow" {
		// The TensorFlow converter's SparseTensor inputs (#1775).
		if b, ok, err := tflitemodel.Metadata(data, tensorflowSparseMetadata); err == nil && ok {
			if err := json.Unmarshal(b, &sparse); err != nil {
				return nil, 500, "cloudburrow: the converted model's " + tensorflowSparseMetadata + " metadata is unreadable: " + err.Error()
			}
		}
	}
	for k, ports := range [][]tflitemodel.Port{m.Inputs, m.Outputs} {
		what := []string{"input", "output"}[k]
		for _, p := range ports {
			c, code, msg := tfliteColumnOf(p, what)
			if code != 0 {
				return nil, code, strings.Replace(msg, "TensorFlow Lite model", kind+" model", 1)
			}
			if dims, ok := sparse[p.Name]; ok && k == 0 {
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
		return nil, 400, "Failed to load the " + kind + " model " + uri + ": it has no inputs or no outputs"
	}
	return imp, 0, ""
}

// tfliteResource fills a model resource's columns and import run.
func (md bqModel) tfliteResource(res map[string]any) {
	field := func(c tflitePortCol) map[string]any {
		t := map[string]any{"typeKind": c.Type}
		if c.Array {
			t = map[string]any{"typeKind": "ARRAY", "arrayElementType": map[string]string{"typeKind": c.Type}}
		}
		return map[string]any{"name": c.Name, "type": t}
	}
	var feats, labels []map[string]any
	for _, c := range md.TFLite.Inputs {
		feats = append(feats, field(c))
	}
	for _, c := range md.TFLite.Outputs {
		labels = append(labels, field(c))
	}
	res["featureColumns"] = feats
	res["labelColumns"] = labels
	res["trainingRuns"] = []map[string]any{{
		"trainingOptions": map[string]any{"modelUri": md.TFLite.ModelPath},
		"startTime":       timeRFC3339(md.Created),
	}}
}

// tfliteTensor is a row's JSON value of column c as the port's tensor,
// with a batch of one.
func tfliteTensor(c tflitePortCol, raw json.RawMessage) (tflitemodel.Tensor, string) {
	if c.image {
		return tfliteImageTensor(c, raw)
	}
	shape, flat, strs, why := tfliteFlat(c, raw)
	if why != "" {
		return tflitemodel.Tensor{}, why
	}
	if c.Type == "STRING" {
		return tflitemodel.Tensor{Type: tflitemodel.String, Shape: shape, S: strs}, ""
	}
	t := tflitemodel.Tensor{Type: tflitemodel.Float64, Shape: shape}
	for _, x := range flat {
		t.F = append(t.F, float32(x))
	}
	if c.Type != "FLOAT64" {
		t.Type = tflitemodel.Int64
		t.F = nil
		for _, x := range flat {
			t.I = append(t.I, int64(x))
		}
	}
	return t, ""
}

// tfliteFlat is a row's JSON value of column c as the port's shape, with a
// batch of one, and its values: strs for a STRING port, flat otherwise.
func tfliteFlat(c tflitePortCol, raw json.RawMessage) ([]int, []float64, []string, string) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, nil, nil, err.Error()
	}
	if c.Sparse {
		// "A NULL, the associated BigQuery ML type, or an ARRAY of the
		// associated BigQuery ML type": none, one or the ARRAY's values.
		switch e := v.(type) {
		case nil:
			v = []any{}
		case []any:
			for _, y := range e {
				if _, nested := y.([]any); nested || y == nil {
					return nil, nil, nil, "is not a NULL, a value or an ARRAY of values for the model's SparseTensor input"
				}
			}
		default:
			v = []any{e}
		}
	}
	var flat []float64
	var strs []string
	var walk func(any) string
	walk = func(x any) string {
		switch e := x.(type) {
		case nil:
			return "is NULL; a TensorFlow Lite model's input has no missing value"
		case string:
			if c.Type == "STRING" {
				strs, flat = append(strs, e), append(flat, 0)
				return ""
			}
			n, err := strconv.ParseFloat(e, 64)
			if err != nil {
				return "holds " + strconv.Quote(e) + ", which is not a number"
			}
			flat = append(flat, n)
		case float64:
			flat = append(flat, e)
		case bool:
			flat = append(flat, map[bool]float64{true: 1}[e])
		case []any:
			for _, y := range e {
				if why := walk(y); why != "" {
					return why
				}
			}
		default:
			return "is not a number"
		}
		return ""
	}
	if why := walk(v); why != "" {
		return nil, nil, nil, why
	}
	shape := append([]int{}, c.Shape...)
	if c.Sparse {
		if len(c.SparseDims) == 1 && c.SparseDims[0] >= 0 && len(flat) > c.SparseDims[0] {
			return nil, nil, nil, fmt.Sprintf("has %d values; the model's SparseTensor input of dense shape [batch, %d] holds at most %d a row",
				len(flat), c.SparseDims[0], c.SparseDims[0])
		}
		shape = []int{1, len(flat)}
	}
	known := 1
	free := -1
	for k, d := range shape {
		switch {
		case k == 0 && d < 0:
			shape[k] = 1
		case d < 0:
			free = k
		default:
			known *= d
		}
	}
	if free >= 0 && known > 0 {
		shape[free] = len(flat) / known
	}
	if len(shape) == 0 {
		shape = nil
	}
	n := 1
	for _, d := range shape {
		n *= d
	}
	if n != len(flat) {
		return nil, nil, nil, fmt.Sprintf("has %d values; the model's input of shape %v takes %d a row", len(flat), c.Shape, n)
	}
	if c.Type == "STRING" && len(strs) != len(flat) {
		return nil, nil, nil, "is not a STRING"
	}
	if c.Type != "STRING" {
		strs = nil
	}
	return shape, flat, strs, ""
}

// tfliteLiteral is a row's output value as SQL.
func tfliteLiteral(c tflitePortCol, t tflitemodel.Tensor) string {
	lit := func(i int) string {
		switch c.Type {
		case "INT64":
			if t.I != nil {
				return strconv.FormatInt(t.I[i], 10)
			}
			return strconv.FormatInt(int64(t.F[i]), 10)
		case "BOOL":
			if t.B != nil && t.B[i] {
				return "TRUE"
			}
			return "FALSE"
		case "STRING":
			if t.S != nil {
				return sqlString(t.S[i])
			}
			return "NULL"
		}
		v := float64(t.F[i])
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
	if !c.Array {
		if t.Len() == 0 {
			return "NULL"
		}
		return lit(0)
	}
	var vs []string
	for i := range t.Len() {
		vs = append(vs, lit(i))
	}
	return "ARRAY<" + c.Type + ">[" + strings.Join(vs, ", ") + "]"
}

// tfliteSubquery is the subquery of ML.PREDICT of an imported TensorFlow
// Lite model.
func (f front) tfliteSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	imp := md.TFLite
	inputs := append([]tflitePortCol(nil), imp.Inputs...) // marked per query (image)
	explain := false
	switch {
	case c.fn == "PREDICT":
	case md.Type == "TENSORFLOW" && tensorflowUnsupportedFns[c.fn]:
		// modeltensorflow.go, #1775: the functions the create-tensorflow
		// page names.
		return "", 400, "ML." + c.fn + " is not supported for model type TENSORFLOW"
	case md.Type == "TENSORFLOW" && c.fn == "EXPLAIN_PREDICT":
		// Integrated Gradients on the TensorFlow runtime (modeltfruntime.go,
		// #1775).
		explain = true
	case md.Type == "TENSORFLOW":
		return "", 501, "Not implemented here: ML." + c.fn + " of an imported TensorFlow model; CloudBurrow runs ML.PREDICT of it. Nothing was run."
	default:
		return "", 400, "ML." + c.fn + " is not supported for model type TENSORFLOW_LITE: you can only use TensorFlow Lite models with the ML.PREDICT function"
	}
	if c.input == "" {
		return "", 400, "ML.PREDICT requires an input: a query in parentheses or TABLE name"
	}
	var xo tfExplainOptions
	switch {
	case explain:
		if c.input == "" {
			return "", 400, "ML.EXPLAIN_PREDICT requires an input: a query in parentheses or TABLE name"
		}
		var code int
		var msg string
		if xo, code, msg = tfExplainOpts(md, c); code != 0 {
			return "", code, msg
		}
	case len(c.struc) > 0:
		return "", 400, "ML.PREDICT of an imported " + map[bool]string{true: "TensorFlow", false: "TensorFlow Lite"}[md.Type == "TENSORFLOW"] + " model takes no STRUCT of options"
	}
	data, err := f.models.blob(imp.SHA256)
	if err != nil {
		return "", 500, "cloudburrow: the model's file is missing from the state directory: " + err.Error()
	}
	bundle := data
	var m *tflitemodel.Model
	if imp.Bundle {
		b, err := tfconvert.ReadBundle(data)
		if err != nil {
			return "", 500, "cloudburrow: could not read the model's converted bundle: " + err.Error()
		}
		data = b.TFLite
	}
	if imp.Runtime == "" && !explain {
		if m, err = tflitemodel.Load(data); err != nil {
			return "", 500, "cloudburrow: could not load the model's TensorFlow Lite file: " + err.Error()
		}
	}
	cols, _, code, msg := f.queryAllRows(r, q, "SELECT * FROM ("+c.input+") LIMIT 0")
	if code != 0 {
		return "", code, msg
	}
	byName := map[string]mlColumn{}
	for _, col := range cols {
		byName[strings.ToLower(col.Name)] = col
	}
	var refs []string
	for i, in := range inputs {
		col, ok := byName[strings.ToLower(in.Name)]
		if !ok {
			return "", 400, "Invalid table-valued function ML." + c.fn + " Column " + in.Name + " is not found in the input data to the " + c.fn + " function."
		}
		ct := bqColumnType(col.Type)
		elem, isArray := strings.CutPrefix(ct, "ARRAY<")
		elem = strings.TrimSuffix(elem, ">")
		okType := false
		switch {
		case ct == "RECORD" && in.Array && in.Type != "STRING" && in.Type != "BOOL":
			// An image STRUCT (ML.DECODE_IMAGE's, mlimage.go, #1798),
			// reshaped to the input's tensor; checked by value.
			inputs[i].image = true
			refs = append(refs, "_cbml_in."+sqlIdent(col.Name)+" AS c"+strconv.Itoa(i))
			continue
		}
		// A SparseTensor input takes a value or an ARRAY (#1775).
		shapeOK := isArray == in.Array || in.Sparse
		switch elem {
		case "INT64", "FLOAT64", "NUMERIC", "BIGNUMERIC", "BOOL":
			okType = shapeOK && in.Type != "STRING"
		case "STRING":
			okType = shapeOK && in.Type == "STRING" // #1766
		}
		if !okType {
			return "", 400, fmt.Sprintf("Invalid table-valued function ML.%s Column %s of type %s cannot be converted to the type the model's input %s expects, %s",
				c.fn, col.Name, ct, in.Name, in.sqlType())
		}
		ref := "_cbml_in." + sqlIdent(col.Name)
		if !isArray && in.Type != "STRING" {
			ref = "CAST(" + ref + " AS FLOAT64)"
		}
		refs = append(refs, ref+" AS c"+strconv.Itoa(i))
	}
	key := "TO_JSON_STRING(STRUCT(" + strings.Join(refs, ", ") + "))"
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT DISTINCT "+key+" AS k FROM ("+c.input+") AS _cbml_in")
	if code != 0 {
		return "", code, msg
	}
	var lits, outTypes []string
	for _, o := range imp.Outputs {
		outTypes = append(outTypes, sqlIdent(o.Name)+" "+o.sqlType())
	}
	if explain || imp.Runtime == tfRuntimeTensorFlow {
		// The TensorFlow runtime (modeltfruntime.go): every row in batches.
		var code int
		var msg string
		if lits, code, msg = f.tfRuntimeRows(r, imp, inputs, rows, bundle, explain, xo); code != 0 {
			return "", code, msg
		}
		if explain {
			outTypes = append(outTypes, tfExplainColumns)
		}
		rows = nil
	}
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		var vals map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*row[0]), &vals); err != nil {
			return "", 500, "cloudburrow: could not read the input's values: " + err.Error()
		}
		var ins []tflitemodel.Tensor
		for i, in := range inputs {
			t, why := tfliteTensor(in, vals["c"+strconv.Itoa(i)])
			if why != "" {
				return "", 400, "ML.PREDICT input column " + in.Name + " " + why
			}
			ins = append(ins, t)
		}
		outs, err := m.Run(ins)
		if tflitemodel.IsUnsupported(err) {
			return "", 501, "Not implemented here: the TensorFlow Lite model " + imp.URI + ": " + err.Error() + ". Nothing was run."
		}
		if err != nil {
			return "", 400, "The TensorFlow Lite model failed to run: " + err.Error()
		}
		parts := []string{sqlString(*row[0])}
		for k, o := range imp.Outputs {
			parts = append(parts, tfliteLiteral(o, outs[k]))
		}
		lits = append(lits, "STRUCT("+strings.Join(parts, ", ")+")")
	}
	var sel, except []string
	outNames := []string{}
	for _, o := range imp.Outputs {
		outNames = append(outNames, o.Name)
	}
	if explain {
		outNames = append(outNames, tfExplainNames...)
	}
	for _, o := range outNames {
		name := sqlIdent(o)
		sel = append(sel, "_cbml_p."+name+" AS "+name)
		if col, ok := byName[strings.ToLower(o)]; ok {
			except = append(except, sqlIdent(col.Name))
		}
	}
	in := "_cbml_in.*"
	if len(except) > 0 {
		in = "_cbml_in.* EXCEPT (" + strings.Join(except, ", ") + ")"
	}
	return "(SELECT " + strings.Join(sel, ", ") + ", " + in + " FROM (" + c.input + ") AS _cbml_in LEFT JOIN UNNEST(ARRAY<STRUCT<_cbml_k STRING, " +
		strings.Join(outTypes, ", ") + ">>[" + strings.Join(lits, ", ") + "]) AS _cbml_p ON " + key + " = _cbml_p._cbml_k)", 0, ""
}

// tfliteImageTensor is an image STRUCT (mlimage.go, #1798) as the port's
// tensor: a batch of one of its height × width × channels, for an input
// of rank 4 whose fixed dimensions it must match, or its values flattened
// for one of rank 2.
func tfliteImageTensor(c tflitePortCol, raw json.RawMessage) (tflitemodel.Tensor, string) {
	m, why := imageFromJSON(raw, false, "ML.PREDICT")
	if why != "" {
		return tflitemodel.Tensor{}, why
	}
	if m == nil {
		return tflitemodel.Tensor{}, "is NULL; a TensorFlow Lite model's input has no missing value"
	}
	var shape []int
	switch len(c.Shape) {
	case 4:
		shape = []int{1, m.h, m.w, m.c}
		for k := 1; k < 4; k++ {
			if c.Shape[k] >= 0 && c.Shape[k] != shape[k] {
				return tflitemodel.Tensor{}, fmt.Sprintf("is an image of %d × %d × %d (height, width, channels); the model's input of shape %v takes %d × %d × %d: "+
					"resize it with ML.RESIZE_IMAGE", m.h, m.w, m.c, c.Shape, c.Shape[1], c.Shape[2], c.Shape[3])
			}
		}
	case 2:
		shape = []int{1, len(m.v)}
		if c.Shape[1] >= 0 && c.Shape[1] != len(m.v) {
			return tflitemodel.Tensor{}, fmt.Sprintf("is an image of %d values; the model's input of shape %v takes %d", len(m.v), c.Shape, c.Shape[1])
		}
	default:
		return tflitemodel.Tensor{}, fmt.Sprintf("is an image; the model's input of shape %v is not one of an image (batch, height, width, channels)", c.Shape)
	}
	t := tflitemodel.Tensor{Type: tflitemodel.Float64, Shape: shape}
	if c.Type == "INT64" {
		t.Type = tflitemodel.Int64
		for _, x := range m.v {
			t.I = append(t.I, int64(x))
		}
		return t, ""
	}
	for _, x := range m.v {
		t.F = append(t.F, float32(x))
	}
	return t, ""
}
