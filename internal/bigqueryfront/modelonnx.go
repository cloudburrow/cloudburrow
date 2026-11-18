package bigqueryfront

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
	"github.com/cloudburrow/cloudburrow/internal/onnxrunner"
)

// Imported ONNX models (#1711): CREATE MODEL … OPTIONS (MODEL_TYPE='ONNX',
// MODEL_PATH='gs://…' [, KMS_KEY_NAME]), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-onnx
// documents it (re-read 2026-11-17):
//
//   - the model is read from MODEL_PATH, a Cloud Storage URI of a .onnx file
//     or a folder of one (gs://bucket/path/to/onnx_model/*), from the
//     instance's own Cloud Storage (the front's --storage, never Google's),
//     and kept with the model, as BigQuery keeps its copy: changing or
//     deleting the object afterwards changes nothing;
//   - "Maximum model size is 450 MB";
//   - "Input and output columns must be the ONNX Tensor type", of the
//     element types the page's table maps to BigQuery types (the integer
//     types to INT64, FLOAT16, BFLOAT16, FLOAT and DOUBLE to FLOAT64, BOOL
//     and STRING); COMPLEX64 and COMPLEX128 are unsupported, and so are
//     map, opaque, sequence, optional and sparse tensor values, refused in
//     the page's words: "ONNX model output 'output_probability' has
//     unsupported ONNX type: ONNX_TYPE_SEQUENCE.";
//   - "Imported models work only with the ML.PREDICT function".
//
// ML.PREDICT (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-predict):
// "the input column names in the table must match the column names in the
// model", "the input must be convertible to the type expected by the
// model", "For ONNX models, the output is the output of the ONNX model's
// predict method", and the input's columns are passed through. The
// sklearn tutorial (making-predictions-with-sklearn-models-in-onnx-format)
// shows inputs of FloatTensorType([None, 1]) read from FLOAT64 columns and
// outputs label and probabilities, a value and an array: an input or
// output whose dimensions after the batch dimension are all 1 is a value
// of a row, one with more an ARRAY of the row's values (row-major), and
// the batch dimension is the input's rows.
//
// How it runs: the front reads the input's distinct feature values (as
// TO_JSON_STRING of a STRUCT of the model's input columns), runs the model
// on ONNX Runtime (internal/onnxmodel, the `-tags onnx` build's), and
// writes ML.PREDICT as the input LEFT JOINed with the predictions, on the
// same key, so every input row is answered where it is read.
//
// Where it runs (#1711): the front runs in the cluster, from the storage
// image's CGO-less Linux cloudburrow-storage binary, which cannot load ONNX
// Runtime. The CLI of the opt-in onnx build (make build-onnx) serves an
// ONNX runner (internal/onnxrunner) on --port-onnx, published to the
// cluster through cloudburrow-host as Resource Manager is (#1392), and
// `up` gives the front its address with --onnx. The front sends the
// runner a model's bytes when it is created (and again whenever the
// runner no longer holds them), and runs it there. Without --onnx, as
// from the default build, every MODEL_TYPE='ONNX' is refused 501
// (onnxRefusal), naming the build that runs it.

// maxONNXModelBytes is BigQuery's "Maximum model size is 450 MB".
const maxONNXModelBytes = 450 << 20

// onnxImport is what an imported ONNX model keeps.
type onnxImport struct {
	// ModelPath is the MODEL_PATH option; URI the object read.
	ModelPath string           `json:"modelPath"`
	URI       string           `json:"uri"`
	SHA256    string           `json:"sha256"`
	Size      int64            `json:"size"`
	Inputs    []onnxmodel.Port `json:"inputs"`
	Outputs   []onnxmodel.Port `json:"outputs"`
}

// onnxRunner runs imported ONNX models: *onnxrunner.Client, the runner of
// the onnx build's CLI.
type onnxRunner interface {
	// Load sends the model, whose hex SHA-256 is sum, and returns its
	// inputs and outputs.
	Load(ctx context.Context, sum string, model []byte) (inputs, outputs []onnxmodel.Port, err error)
	// Run runs the model whose hex SHA-256 is sum, asking model for its
	// bytes when the runner no longer holds them.
	Run(ctx context.Context, sum string, model func() ([]byte, error), inputs []onnxmodel.Port, in []onnxmodel.Tensor,
		outputs []onnxmodel.Port) ([]onnxmodel.Tensor, error)
}

// onnxRefusal is the 501 of an ONNX model on an instance with no ONNX
// runner: one started from the default build.
func onnxRefusal() string {
	return "Not implemented here: model_type ONNX: an imported ONNX model runs on ONNX Runtime, which only CloudBurrow's " +
		"opt-in onnx build runs: build it with `make build-onnx` and start the instance with bin/cloudburrow-onnx up, " +
		"ONNXRUNTIME_LIB set to libonnxruntime (docs/compatibility.md, BQML, #1711). Nothing was run."
}

// onnxRunnerFailed is the answer to a runner's failure that is not the
// model's: code and message.
func onnxRunnerFailed(err error) (int, string) {
	if onnxrunner.IsUnavailable(err) {
		return 501, "Not implemented here: model_type ONNX: the instance's ONNX runner cannot run ONNX Runtime: " + err.Error() +
			" (#1711). Nothing was run."
	}
	return 500, "cloudburrow: " + err.Error()
}

// parseONNXOptions reads an ONNX model's OPTIONS list into d.
func parseONNXOptions(toks []token, d *modelDDL, typ string) (int, string) {
	d.opts = mlOptions{ModelType: typ}
	for i := 0; i < len(toks); {
		if toks[i].kind != tokWord || i+2 >= len(toks) || !toks[i+1].punct("=") {
			return 400, "Syntax error in OPTIONS"
		}
		name := strings.ToLower(toks[i].text)
		j := i + 2
		for j < len(toks) && !toks[j].punct(",") {
			j++
		}
		val := toks[i+2 : j]
		i = j + 1
		var s string
		ok := len(val) == 1 && val[0].kind == tokString
		if ok {
			s, ok = unquoteString(val[0].text)
		}
		switch name {
		case "model_type":
		case "model_path":
			if !ok || s == "" {
				return 400, "Invalid value for option model_path: it is a STRING, a Cloud Storage URI"
			}
			d.onnxPath = s
		case "kms_key_name":
			if !ok {
				return 400, "Invalid value for option kms_key_name"
			}
			return 501, "Not implemented here: kms_key_name on an imported " + typ + " model: CloudBurrow keeps the model unencrypted; " +
				"leave the option out. Nothing was run."
		default:
			return 400, "Option " + name + " is not supported for model_type " + typ + ": an imported " + typ + " model takes model_type, model_path and kms_key_name"
		}
	}
	if d.onnxPath == "" {
		return 400, "CREATE MODEL of model_type " + typ + " requires the model_path option"
	}
	if !strings.HasPrefix(d.onnxPath, "gs://") {
		return 400, "Invalid model_path " + d.onnxPath + ": it must be a Cloud Storage URI, gs://bucket/path/to/model_file or gs://bucket/path/to/model_folder/*"
	}
	return 0, ""
}

// onnxColumn is how an ONNX tensor reads or writes a BigQuery column.
type onnxColumn struct {
	typ   string // INT64, FLOAT64, BOOL or STRING
	array bool
	width int64 // an array's values per row, or -1 when the graph leaves it free
}

func (c onnxColumn) sqlType() string {
	if c.array {
		return "ARRAY<" + c.typ + ">"
	}
	return c.typ
}

// onnxColumnOf maps p to a column, or why it cannot: the page's message
// for a type that is not a tensor.
func onnxColumnOf(p onnxmodel.Port, what string) (onnxColumn, string) {
	if p.Kind != onnxmodel.KindTensor {
		return onnxColumn{}, fmt.Sprintf("ONNX model %s '%s' has unsupported ONNX type: %s.", what, p.Name, p.Kind)
	}
	var c onnxColumn
	switch {
	case onnxmodel.IsInt(p.Elem):
		c.typ = "INT64"
	case onnxmodel.IsFloat(p.Elem):
		c.typ = "FLOAT64"
	case p.Elem == "BOOL":
		c.typ = "BOOL"
	case p.Elem == "STRING":
		c.typ = "STRING"
	default:
		return c, fmt.Sprintf("ONNX model %s '%s' has unsupported ONNX tensor element type: %s. BigQuery ML supports the integer types, "+
			"FLOAT16, BFLOAT16, FLOAT, DOUBLE, BOOL and STRING.", what, p.Name, p.Elem)
	}
	c.width = 1
	for _, d := range p.Dims[min(1, len(p.Dims)):] {
		if d < 0 {
			c.width = -1
			c.array = true
			break
		}
		c.width *= d
		if d != 1 {
			c.array = true
		}
	}
	return c, ""
}

// outputNames are the columns ML.PREDICT writes the outputs to.
func (o *onnxImport) outputNames() []string {
	var names []string
	for _, p := range o.Outputs {
		names = append(names, p.Name)
	}
	return columnNames(names)
}

// importONNX reads and checks the model MODEL_PATH names. code and msg
// are a refusal.
func (f front) importONNX(ctx context.Context, path string) (*onnxImport, []byte, int, string) {
	if f.onnx == nil {
		return nil, nil, 501, onnxRefusal()
	}
	o, data, code, msg := f.readImportedModel(ctx, path, "ONNX", []string{".onnx"}, maxONNXModelBytes, "450 MB")
	if code != 0 {
		return nil, nil, code, msg
	}
	sum := sha256.Sum256(data)
	in, out, err := f.onnx.Load(ctx, hex.EncodeToString(sum[:]), data)
	if onnxrunner.IsModelError(err) {
		return nil, nil, 400, "Failed to load the ONNX model " + o.uri() + ": " + err.Error()
	}
	if err != nil {
		code, msg := onnxRunnerFailed(err)
		return nil, nil, code, msg
	}
	for _, p := range in {
		if _, why := onnxColumnOf(p, "input"); why != "" {
			return nil, nil, 400, why
		}
	}
	for _, p := range out {
		if _, why := onnxColumnOf(p, "output"); why != "" {
			return nil, nil, 400, why
		}
	}
	return &onnxImport{URI: o.uri(), SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)), Inputs: in, Outputs: out}, data, 0, ""
}

// readImportedModel reads the object an imported model's MODEL_PATH names
// from the instance's Cloud Storage: a file of one of exts, or the one
// such file a folder's wildcard (gs://bucket/path/to/model_folder/*)
// matches, of at most limit bytes (limitText as the page writes it).
func (f front) readImportedModel(ctx context.Context, path, kind string, exts []string, limit int64, limitText string) (gsObject, []byte, int, string) {
	if f.storage == nil {
		return gsObject{}, nil, 404, "Not found: URI " + path + ": CloudBurrow was given no Cloud Storage to read it from; is Cloud Storage enabled? Nothing was run."
	}
	rest := strings.TrimPrefix(path, "gs://")
	bucket, name, _ := strings.Cut(rest, "/")
	if bucket == "" || name == "" {
		return gsObject{}, nil, 400, "Invalid model_path " + path + ": it must be gs://bucket/path/to/model" + exts[0] + " or gs://bucket/path/to/model_folder/*"
	}
	type object struct {
		Name string      `json:"name"`
		Size json.Number `json:"size"`
	}
	var obj object
	switch strings.Count(name, "*") {
	case 0:
		if !hasExt(name, exts) {
			return gsObject{}, nil, 400, "Invalid model_path " + path + ": an imported " + kind + " model must be a " + strings.Join(exts, ", ") + " file"
		}
		st, err := f.storage.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o/"+url.PathEscape(name), &obj)
		if err != nil {
			return gsObject{}, nil, 404, f.storage.unreachable(path, err).Error()
		}
		if st != http.StatusOK {
			return gsObject{}, nil, 404, fmt.Sprintf("Not found: URI %s (Cloud Storage answered HTTP %d). Nothing was run.", path, st)
		}
	case 1:
		prefix, suffix, _ := strings.Cut(name, "*")
		var found []object
		token := ""
		for {
			q := url.Values{"prefix": {prefix}}
			if token != "" {
				q.Set("pageToken", token)
			}
			var list struct {
				Items         []object `json:"items"`
				NextPageToken string   `json:"nextPageToken"`
			}
			st, err := f.storage.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o?"+q.Encode(), &list)
			if err != nil {
				return gsObject{}, nil, 404, f.storage.unreachable(path, err).Error()
			}
			if st != http.StatusOK {
				return gsObject{}, nil, 404, fmt.Sprintf("Not found: URI %s (Cloud Storage answered HTTP %d). Nothing was run.", path, st)
			}
			for _, it := range list.Items {
				if strings.HasSuffix(it.Name, suffix) && hasExt(it.Name, exts) {
					found = append(found, it)
				}
			}
			if token = list.NextPageToken; token == "" {
				break
			}
		}
		switch len(found) {
		case 0:
			return gsObject{}, nil, 404, "Not found: URI " + path + ": it matches no " + strings.Join(exts, ", ") + " file. Nothing was run."
		case 1:
			obj = found[0]
		default:
			return gsObject{}, nil, 400, fmt.Sprintf("Invalid model_path %s: it matches %d %s files (%s, %s, ...); an imported %s model is one file",
				path, len(found), strings.Join(exts, ", "), found[0].Name, found[1].Name, kind)
		}
	default:
		return gsObject{}, nil, 400, "Invalid model_path " + path + ": only one * wildcard is supported in a Cloud Storage URI"
	}
	o := gsObject{bucket, obj.Name}
	if size, err := obj.Size.Int64(); err == nil && size > limit {
		return gsObject{}, nil, 400, fmt.Sprintf("The %s model %s is %d bytes; the maximum model size is %s", kind, o.uri(), size, limitText)
	}
	rc, err := f.storage.open(ctx, o)
	if err != nil {
		return gsObject{}, nil, 404, strings.Replace(err.Error(), "Nothing was loaded.", "Nothing was run.", 1)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return gsObject{}, nil, 500, "cloudburrow: could not read " + o.uri() + ": " + err.Error()
	}
	if int64(len(data)) > limit {
		return gsObject{}, nil, 400, fmt.Sprintf("The %s model %s is larger than the maximum model size, %s", kind, o.uri(), limitText)
	}
	return o, data, 0, ""
}

// hasExt reports whether name ends in one of exts, ignoring case.
func hasExt(name string, exts []string) bool {
	for _, e := range exts {
		if strings.HasSuffix(strings.ToLower(name), e) {
			return true
		}
	}
	return false
}

// onnxResource fills a model resource's columns and its import's
// training run: featureColumns are the model's inputs and labelColumns its
// outputs, as StandardSqlFields, and the run's trainingOptions.modelUri is
// "Google Cloud Storage URI from which the model was imported" (the REST
// reference's TrainingOptions).
func (md bqModel) onnxResource(res map[string]any) {
	field := func(name string, c onnxColumn) map[string]any {
		t := map[string]any{"typeKind": c.typ}
		if c.array {
			t = map[string]any{"typeKind": "ARRAY", "arrayElementType": map[string]string{"typeKind": c.typ}}
		}
		return map[string]any{"name": name, "type": t}
	}
	var feats, labels []map[string]any
	for _, p := range md.ONNX.Inputs {
		c, _ := onnxColumnOf(p, "input")
		feats = append(feats, field(p.Name, c))
	}
	for i, p := range md.ONNX.Outputs {
		c, _ := onnxColumnOf(p, "output")
		labels = append(labels, field(md.ONNX.outputNames()[i], c))
	}
	res["featureColumns"] = feats
	res["labelColumns"] = labels
	res["trainingRuns"] = []map[string]any{{
		"trainingOptions": map[string]any{"modelUri": md.ONNX.ModelPath},
		"startTime":       timeRFC3339(md.Created),
	}}
}

// onnxSubquery is the subquery of ML.PREDICT of an imported ONNX model.
func (f front) onnxSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	if c.fn != "PREDICT" {
		return "", 400, "ML." + c.fn + " is not supported for model type ONNX: imported models work only with the ML.PREDICT function"
	}
	if c.input == "" {
		return "", 400, "ML.PREDICT requires an input: a query in parentheses or TABLE name"
	}
	if len(c.struc) > 0 {
		return "", 400, "ML.PREDICT of an imported ONNX model takes no STRUCT of options"
	}
	if f.onnx == nil {
		return "", 501, onnxRefusal()
	}
	imp := md.ONNX
	data := func() ([]byte, error) {
		b, err := f.models.blob(imp.SHA256)
		if err != nil {
			return nil, fmt.Errorf("the model's ONNX file is missing from the state directory: %w", err)
		}
		return b, nil
	}
	cols, _, code, msg := f.queryAllRows(r, q, "SELECT * FROM ("+c.input+") LIMIT 0")
	if code != 0 {
		return "", code, msg
	}
	byName := map[string]mlColumn{}
	for _, col := range cols {
		byName[strings.ToLower(col.Name)] = col
	}
	ins := make([]onnxColumn, len(imp.Inputs))
	var refs []string
	for i, p := range imp.Inputs {
		ic, _ := onnxColumnOf(p, "input")
		ins[i] = ic
		col, ok := byName[strings.ToLower(p.Name)]
		if !ok {
			return "", 400, "Invalid table-valued function ML.PREDICT Column " + p.Name + " is not found in the input data to the PREDICT function."
		}
		if !onnxConvertible(col.Type, ic) {
			return "", 400, fmt.Sprintf("Invalid table-valued function ML.PREDICT Column %s of type %s cannot be converted to the type the model's input %s expects, %s (ONNX %s %v)",
				col.Name, bqColumnType(col.Type), p.Name, ic.sqlType(), p.Elem, p.Dims)
		}
		refs = append(refs, "_cbml_in."+sqlIdent(col.Name)+" AS c"+strconv.Itoa(i))
	}
	key := "TO_JSON_STRING(STRUCT(" + strings.Join(refs, ", ") + "))"
	if len(refs) == 0 {
		key = "''"
	}
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT DISTINCT "+key+" AS k FROM ("+c.input+") AS _cbml_in")
	if code != 0 {
		return "", code, msg
	}
	var keys []string
	for _, row := range rows {
		if len(row) == 1 && row[0] != nil {
			keys = append(keys, *row[0])
		}
	}
	preds, code, msg := runONNX(r.Context(), f.onnx, data, imp, ins, keys)
	if code != 0 {
		return "", code, msg
	}
	names := imp.outputNames()
	fields := []string{"_cbml_k STRING"}
	var sel, clash []string
	for i, p := range imp.Outputs {
		oc, _ := onnxColumnOf(p, "output")
		fields = append(fields, sqlIdent(names[i])+" "+oc.sqlType())
		sel = append(sel, "_cbml_p."+sqlIdent(names[i])+" AS "+sqlIdent(names[i]))
		if col, ok := byName[strings.ToLower(names[i])]; ok {
			clash = append(clash, sqlIdent(col.Name))
		}
	}
	lits := make([]string, len(keys))
	for k, key := range keys {
		lits[k] = "STRUCT(" + sqlString(key) + ", " + strings.Join(preds[k], ", ") + ")"
	}
	in := "_cbml_in.*"
	if len(clash) > 0 {
		in = "_cbml_in.* EXCEPT (" + strings.Join(clash, ", ") + ")"
	}
	return "(SELECT " + strings.Join(append(sel, in), ", ") + " FROM (" + c.input + ") AS _cbml_in LEFT JOIN UNNEST(ARRAY<STRUCT<" +
		strings.Join(fields, ", ") + ">>[" + strings.Join(lits, ", ") + "]) AS _cbml_p ON " + key + " = _cbml_p._cbml_k)", 0, ""
}

// bqColumnType is a REST schema type as GoogleSQL names it.
func bqColumnType(t string) string {
	if inner, ok := strings.CutPrefix(t, "ARRAY<"); ok {
		return "ARRAY<" + bqColumnType(strings.TrimSuffix(inner, ">")) + ">"
	}
	switch strings.ToUpper(t) {
	case "INTEGER", "INT64":
		return "INT64"
	case "FLOAT", "FLOAT64":
		return "FLOAT64"
	case "BOOLEAN", "BOOL":
		return "BOOL"
	}
	return strings.ToUpper(t)
}

// onnxConvertible reports whether a column of REST type t converts to
// what input c takes: a number to a floating-point tensor, an INT64 to an
// integer one, a BOOL and a STRING to their own, a value to a tensor of
// one value a row and an ARRAY to one of more.
func onnxConvertible(t string, c onnxColumn) bool {
	t = bqColumnType(t)
	inner, isArray := strings.CutPrefix(t, "ARRAY<")
	if isArray != c.array {
		return false
	}
	inner = strings.TrimSuffix(inner, ">")
	switch c.typ {
	case "FLOAT64":
		return inner == "FLOAT64" || inner == "INT64" || inner == "NUMERIC" || inner == "BIGNUMERIC"
	default:
		return inner == c.typ
	}
}

// runONNX runs the model on each key's input values (a key is
// TO_JSON_STRING of the STRUCT c0, c1, ... of the inputs' columns) and
// returns each key's outputs as SQL literals.
func runONNX(ctx context.Context, runner onnxRunner, model func() ([]byte, error), imp *onnxImport, ins []onnxColumn,
	keys []string) ([][]string, int, string) {
	out := make([][]string, len(keys))
	if len(keys) == 0 {
		return out, 0, ""
	}
	// values[k][i] are key k's values of input i.
	values := make([][][]json.RawMessage, len(keys))
	for k, key := range keys {
		var m map[string]json.RawMessage
		if len(ins) > 0 {
			if err := json.Unmarshal([]byte(key), &m); err != nil {
				return nil, 500, "cloudburrow: could not read the input's values: " + err.Error()
			}
		}
		values[k] = make([][]json.RawMessage, len(ins))
		for i, ic := range ins {
			raw := m["c"+strconv.Itoa(i)]
			name := imp.Inputs[i].Name
			if len(raw) == 0 || string(raw) == "null" {
				return nil, 400, "ML.PREDICT input column " + name + " is NULL in a row; the ONNX model's input " + name + " takes a value in every row"
			}
			var vs []json.RawMessage
			if ic.array {
				if err := json.Unmarshal(raw, &vs); err != nil {
					return nil, 500, "cloudburrow: could not read column " + name + ": " + err.Error()
				}
				if ic.width >= 0 && int64(len(vs)) != ic.width {
					return nil, 400, fmt.Sprintf("ML.PREDICT input column %s has an array of %d values in a row; the ONNX model's input %s %v takes %d",
						name, len(vs), name, imp.Inputs[i].Dims, ic.width)
				}
			} else {
				vs = []json.RawMessage{raw}
			}
			values[k][i] = vs
		}
	}
	// A batch of every row when each input's first dimension is free and
	// the rest fixed; otherwise one row at a time.
	batch := true
	for i, p := range imp.Inputs {
		if len(p.Dims) == 0 || p.Dims[0] != -1 || ins[i].width < 0 {
			batch = false
		}
	}
	const batchRows = 1024
	for start := 0; start < len(keys); {
		end := start + 1
		if batch {
			end = min(len(keys), start+batchRows)
		}
		n := end - start
		tensors := make([]onnxmodel.Tensor, len(ins))
		for i, p := range imp.Inputs {
			t := onnxmodel.Tensor{}
			for k := start; k < end; k++ {
				for _, raw := range values[k][i] {
					if why := appendONNXValue(&t, p.Elem, raw); why != "" {
						return nil, 400, "ML.PREDICT input column " + p.Name + ": " + why
					}
				}
			}
			shape, why := onnxShape(p.Dims, n, len(values[start][i]))
			if why != "" {
				return nil, 501, "Not implemented here: the ONNX model's input " + p.Name + " " + why + ". Nothing was run."
			}
			t.Shape = shape
			tensors[i] = t
		}
		res, err := runner.Run(ctx, imp.SHA256, model, imp.Inputs, tensors, imp.Outputs)
		if onnxrunner.IsModelError(err) {
			return nil, 400, "The ONNX model failed to run: " + err.Error()
		}
		if err != nil {
			code, msg := onnxRunnerFailed(err)
			return nil, code, msg
		}
		for o, p := range imp.Outputs {
			oc, _ := onnxColumnOf(p, "output")
			vals := res[o]
			total := len(vals.F) + len(vals.I) + len(vals.B) + len(vals.S)
			if total%n != 0 || !oc.array && total != n || oc.array && oc.width >= 0 && int64(total/n) != oc.width {
				return nil, 400, fmt.Sprintf("The ONNX model's output %s has shape %v for %d rows, which is not the shape %v it declares", p.Name, vals.Shape, n, p.Dims)
			}
			per := total / n
			for k := 0; k < n; k++ {
				var lits []string
				for j := k * per; j < (k+1)*per; j++ {
					lits = append(lits, onnxLiteral(vals, oc.typ, j))
				}
				lit := lits[0]
				if oc.array {
					lit = "[" + strings.Join(lits, ", ") + "]"
				}
				out[start+k] = append(out[start+k], lit)
			}
		}
		start = end
	}
	return out, 0, ""
}

// onnxShape is the shape of an input tensor of rows rows, each of per
// values, for an input of dims dims; why is a shape CloudBurrow cannot
// make.
func onnxShape(dims []int64, rows, per int) ([]int64, string) {
	if len(dims) == 0 {
		if rows != 1 || per != 1 {
			return nil, "is a scalar"
		}
		return []int64{}, ""
	}
	shape := append([]int64{}, dims...)
	if shape[0] == -1 {
		shape[0] = int64(rows)
	} else if shape[0] != int64(rows) {
		return nil, fmt.Sprintf("has a fixed first dimension of %d, which CloudBurrow does not read as the input's rows", shape[0])
	}
	free, known := -1, int64(1)
	for i := 1; i < len(shape); i++ {
		if shape[i] == -1 {
			if free >= 0 {
				return nil, fmt.Sprintf("has more than one free dimension after the first (%v)", dims)
			}
			free = i
			continue
		}
		known *= shape[i]
	}
	if free >= 0 {
		if known == 0 || int64(per)%known != 0 {
			return nil, fmt.Sprintf("of dimensions %v cannot hold %d values a row", dims, per)
		}
		shape[free] = int64(per) / known
	}
	return shape, ""
}

// appendONNXValue appends one value, raw as TO_JSON_STRING writes it, to t
// as an element of type elem.
func appendONNXValue(t *onnxmodel.Tensor, elem string, raw json.RawMessage) string {
	var s string
	str := json.Unmarshal(raw, &s) == nil
	switch {
	case onnxmodel.IsFloat(elem):
		text := string(raw)
		if str {
			text = s
		}
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			switch text {
			case "NaN":
				v = math.NaN()
			case "Infinity":
				v = math.Inf(1)
			case "-Infinity":
				v = math.Inf(-1)
			default:
				return "not a number: " + text
			}
		}
		t.F = append(t.F, v)
	case onnxmodel.IsInt(elem):
		text := string(raw)
		if str {
			text = s
		}
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return "not an INT64: " + text
		}
		lo, hi := intRange(elem)
		if v < lo || v > hi {
			return fmt.Sprintf("%d is out of range for the model's %s input", v, elem)
		}
		t.I = append(t.I, v)
	case elem == "BOOL":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return "not a BOOL: " + string(raw)
		}
		t.B = append(t.B, b)
	case elem == "STRING":
		if !str {
			return "not a STRING: " + string(raw)
		}
		t.S = append(t.S, s)
	default:
		return "unsupported element type " + elem
	}
	return ""
}

// intRange is the values an integer element type holds, as INT64s.
func intRange(elem string) (int64, int64) {
	switch elem {
	case "INT8":
		return math.MinInt8, math.MaxInt8
	case "UINT8":
		return 0, math.MaxUint8
	case "INT16":
		return math.MinInt16, math.MaxInt16
	case "UINT16":
		return 0, math.MaxUint16
	case "INT32":
		return math.MinInt32, math.MaxInt32
	case "UINT32":
		return 0, math.MaxUint32
	case "UINT64":
		return 0, math.MaxInt64
	}
	return math.MinInt64, math.MaxInt64
}

// onnxLiteral is output value j as a literal of type typ.
func onnxLiteral(t onnxmodel.Tensor, typ string, j int) string {
	switch typ {
	case "FLOAT64":
		v := t.F[j]
		switch {
		case math.IsNaN(v):
			return "CAST('NaN' AS FLOAT64)"
		case math.IsInf(v, 1):
			return "CAST('Infinity' AS FLOAT64)"
		case math.IsInf(v, -1):
			return "CAST('-Infinity' AS FLOAT64)"
		}
		return sqlF(v)
	case "INT64":
		return strconv.FormatInt(t.I[j], 10)
	case "BOOL":
		if t.B[j] {
			return "TRUE"
		}
		return "FALSE"
	}
	return sqlString(t.S[j])
}

// The models' ONNX files: each kept once, by its SHA-256, beside
// models.json (onnx-models/<sha256>.onnx), or in memory when the front
// keeps no state; removed when no model names it.

func (m *bqModels) onnxDir() string {
	if m.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(m.path), "onnx-models")
}

// keepBlob keeps an ONNX file.
func (m *bqModels) keepBlob(sum string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if dir := m.onnxDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		p := filepath.Join(dir, sum+".onnx")
		if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, data) {
			return nil
		}
		return os.WriteFile(p, data, 0o600)
	}
	if m.blobs == nil {
		m.blobs = map[string][]byte{}
	}
	m.blobs[sum] = data
	return nil
}

// blob is a kept ONNX file.
func (m *bqModels) blob(sum string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.blobs[sum]; ok {
		return b, nil
	}
	dir := m.onnxDir()
	if dir == "" {
		return nil, fs.ErrNotExist
	}
	return os.ReadFile(filepath.Join(dir, sum+".onnx"))
}

// blobSum is the SHA-256 of the imported model file md keeps (ONNX or,
// in the same store, XGBoost: modelxgb.go), or "".
func (md *bqModel) blobSum() string {
	switch {
	case md == nil:
	case md.ONNX != nil:
		return md.ONNX.SHA256
	case md.XGB != nil:
		return md.XGB.SHA256
	}
	return ""
}

// dropBlobLocked removes an ONNX file no model names any more.
func (m *bqModels) dropBlobLocked(old *bqModel) {
	sum := old.blobSum()
	if sum == "" {
		return
	}
	for _, md := range m.models {
		if md.blobSum() == sum {
			return
		}
	}
	delete(m.blobs, sum)
	if dir := m.onnxDir(); dir != "" {
		_ = os.Remove(filepath.Join(dir, sum+".onnx"))
	}
}

// timeRFC3339 is a time in milliseconds as RFC 3339.
func timeRFC3339(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano) }
