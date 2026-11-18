package bigqueryfront

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel"
	"github.com/cloudburrow/cloudburrow/internal/xgbmodel"
)

// Imported XGBoost models (#1711): CREATE MODEL … [INPUT(…) OUTPUT(…)]
// OPTIONS (MODEL_TYPE='XGBOOST', MODEL_PATH='gs://…' [, KMS_KEY_NAME]), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-xgboost
// documents it (re-read 2026-11-17):
//
//   - "BigQuery ML uses XGBoost 3.1.0", and models must be "BST, UBJ, or
//     JSON": the model is read from MODEL_PATH (a .bst, .ubj or .json file,
//     or the one such file of a folder, gs://bucket/path/to/xgboost_model/*)
//     from the instance's own Cloud Storage, kept with the model, and run by
//     internal/xgbmodel, a pure-Go reader of XGBoost's documented model
//     document that matches XGBoost 3.1.0's predict (its tests);
//   - "Models are limited to 250 MB in size";
//   - the INPUT and OUTPUT clause "is optional only if the model file
//     specifies both feature_names and feature_types"; "If feature_names
//     is populated, input field names must match"; input types must be
//     numeric and "The output type must be FLOAT64"; "BigQuery XGBoost
//     models only support a single scalar or array output";
//   - "Categorical features using XGBoost's built-in support are treated as
//     integer inputs";
//   - only ML.PREDICT and ML.FEATURE_IMPORTANCE work with them.
//
// ML.PREDICT: "the output is the output of the XGBoost model's predict
// method", the input "must be convertible to the type expected by the
// model", and the input's columns are passed through, as for ONNX
// (modelonnx.go), whose distinct-key join this reuses. ML.FEATURE_IMPORTANCE
// is XGBoost's get_score: importance_weight, importance_gain (mean) and
// importance_cover (mean), as for a BOOSTED_TREE model (modeltree.go).
//
// UNVERIFIED (2026-11-17), chosen here as Google does not say: a NULL
// input is XGBoost's missing value (NaN); with no OUTPUT clause the output
// column is predicted_label (ML.PREDICT's "predicted_<label_column_name>"
// for a model whose file names no label); a multi:softprob or
// multi-target model's output is ARRAY<FLOAT64>; feature_types "int" and
// "i" and "c" read INT64, the others FLOAT64; ML.FEATURE_IMPORTANCE of a
// gblinear model is 400 (XGBoost gives it only coefficients).

// maxXGBModelBytes is BigQuery's "Models are limited to 250 MB in size".
const maxXGBModelBytes = 250 << 20

// xgbImport is what an imported XGBoost model keeps.
type xgbImport struct {
	ModelPath string `json:"modelPath"`
	URI       string `json:"uri"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	// Inputs are the model's features, in the model's feature order;
	// Output its one output column.
	Inputs []importColumn `json:"inputs"`
	Output importColumn   `json:"output"`
}

// importColumn is a column of an imported model's INPUT or OUTPUT.
type importColumn struct {
	Name string `json:"name"`
	// Type is INT64, FLOAT64, NUMERIC, BIGNUMERIC, BOOL or ARRAY<FLOAT64>.
	Type string `json:"type"`
}

// parseImportColumns reads an INPUT or OUTPUT clause's field list.
func parseImportColumns(toks []token, what string) ([]importColumn, int, string) {
	var out []importColumn
	for i := 0; i < len(toks); {
		j := i
		depth := 0
		for j < len(toks) && (depth > 0 || !toks[j].punct(",")) {
			if toks[j].punct("<") {
				depth++
			} else if toks[j].punct(">") {
				depth--
			}
			j++
		}
		f := toks[i:j]
		i = j + 1
		if len(f) < 2 || (f[0].kind != tokWord && f[0].kind != tokQuoted) {
			return nil, 400, "Syntax error in " + what + ": expected field_name field_type"
		}
		name := f[0].text
		if f[0].kind == tokQuoted {
			name = strings.Trim(name, "`")
		}
		var typ strings.Builder
		for _, t := range f[1:] {
			typ.WriteString(strings.ToUpper(t.text))
		}
		out = append(out, importColumn{Name: name, Type: bqColumnType(typ.String())})
	}
	if len(out) == 0 {
		return nil, 400, "Syntax error: " + what + " lists no fields"
	}
	return out, 0, ""
}

// parseInputOutput reads INPUT(…) OUTPUT(…) at stmt[i:], returning the
// index after them.
func parseInputOutput(stmt []token, i int, d *modelDDL) (int, int, string) {
	for _, clause := range []string{"INPUT", "OUTPUT"} {
		if i >= len(stmt) || !stmt[i].is(clause) || i+1 >= len(stmt) || !stmt[i+1].punct("(") {
			return i, 400, "Syntax error: the INPUT clause takes an OUTPUT clause after it: INPUT(field_name field_type, …) OUTPUT(field_name field_type, …)"
		}
		end := matchParen(stmt, i+1)
		if end < 0 {
			return i, 400, "Syntax error: unclosed " + clause
		}
		cols, c, m := parseImportColumns(stmt[i+2:end], clause)
		if c != 0 {
			return i, c, m
		}
		if clause == "INPUT" {
			d.importInputs = cols
		} else {
			d.importOutputs = cols
		}
		i = end + 1
	}
	return i, 0, ""
}

// importRefusal checks an imported model's statement around its options.
func importRefusal(d *modelDDL, typ string, trailing bool) (int, string) {
	switch {
	case d.transform != nil:
		return 400, "CREATE MODEL of model_type " + typ + " takes no TRANSFORM clause"
	case trailing:
		return 400, "CREATE MODEL of model_type " + typ + " takes no query: the model is imported from model_path"
	case d.importInputs != nil && typ != "XGBOOST":
		return 400, "CREATE MODEL of model_type " + typ + " takes no INPUT or OUTPUT clause"
	}
	return 0, ""
}

// xgbInputType is the column type of an XGBoost feature_types entry.
func xgbInputType(ft string) string {
	switch ft {
	case "int", "i", "c":
		return "INT64"
	}
	return "FLOAT64"
}

// importXGB reads and checks the model MODEL_PATH names, against the
// statement's INPUT and OUTPUT.
func (f front) importXGB(r *http.Request, d modelDDL) (*xgbImport, []byte, int, string) {
	o, data, code, msg := f.readImportedModel(r.Context(), d.onnxPath, "XGBoost", []string{".bst", ".ubj", ".json"}, maxXGBModelBytes, "250 MB")
	if code != 0 {
		return nil, nil, code, msg
	}
	m, err := xgbmodel.Load(data)
	if err != nil {
		return nil, nil, 400, "Failed to load the XGBoost model " + o.uri() + ": " + err.Error()
	}
	imp := &xgbImport{URI: o.uri(), Size: int64(len(data))}
	sum := sha256.Sum256(data)
	imp.SHA256 = hex.EncodeToString(sum[:])
	outType := "FLOAT64"
	if m.OutputWidth() > 1 {
		outType = "ARRAY<FLOAT64>"
	}
	if d.importInputs == nil {
		if len(m.FeatureNames) == 0 || len(m.FeatureTypes) != len(m.FeatureNames) {
			return nil, nil, 400, "The XGBoost model " + o.uri() + " does not specify both feature_names and feature_types: " +
				"give its columns in an INPUT(field_name field_type, …) OUTPUT(field_name field_type) clause"
		}
		for k, n := range m.FeatureNames {
			imp.Inputs = append(imp.Inputs, importColumn{Name: n, Type: xgbInputType(m.FeatureTypes[k])})
		}
		imp.Output = importColumn{Name: "predicted_label", Type: outType}
		return imp, data, 0, ""
	}
	if len(d.importInputs) != m.NumFeature {
		return nil, nil, 400, fmt.Sprintf("The INPUT clause has %d fields; the XGBoost model %s has %d features", len(d.importInputs), o.uri(), m.NumFeature)
	}
	byName := map[string]importColumn{}
	for _, c := range d.importInputs {
		switch c.Type {
		case "INT64", "FLOAT64", "NUMERIC", "BIGNUMERIC":
		default:
			return nil, nil, 400, "Invalid type " + c.Type + " of input field " + c.Name + ": an XGBoost model's inputs are numeric types (INT64, FLOAT64, NUMERIC, BIGNUMERIC)"
		}
		if _, dup := byName[strings.ToLower(c.Name)]; dup {
			return nil, nil, 400, "Duplicate input field " + c.Name
		}
		byName[strings.ToLower(c.Name)] = c
	}
	if len(m.FeatureNames) > 0 {
		for _, n := range m.FeatureNames {
			c, ok := byName[strings.ToLower(n)]
			if !ok {
				return nil, nil, 400, "The INPUT clause must name the XGBoost model's feature_names: it has no field " + n
			}
			imp.Inputs = append(imp.Inputs, c)
		}
	} else {
		imp.Inputs = d.importInputs
	}
	if len(d.importOutputs) != 1 {
		return nil, nil, 400, "BigQuery XGBoost models only support a single scalar or array output: the OUTPUT clause must have one field"
	}
	out := d.importOutputs[0]
	if out.Type != "FLOAT64" && out.Type != "ARRAY<FLOAT64>" {
		return nil, nil, 400, "Invalid type " + out.Type + " of output field " + out.Name + ": the output type must be FLOAT64"
	}
	if out.Type != outType {
		return nil, nil, 400, fmt.Sprintf("The output field %s is %s, but the XGBoost model's predict gives %d values a row (%s)", out.Name, out.Type, m.OutputWidth(), outType)
	}
	imp.Output = out
	return imp, data, 0, ""
}

// xgbResource fills a model resource's columns and import run.
func (md bqModel) xgbResource(res map[string]any) {
	field := func(c importColumn) map[string]any {
		t := map[string]any{"typeKind": c.Type}
		if inner, ok := strings.CutPrefix(c.Type, "ARRAY<"); ok {
			t = map[string]any{"typeKind": "ARRAY", "arrayElementType": map[string]string{"typeKind": strings.TrimSuffix(inner, ">")}}
		}
		return map[string]any{"name": c.Name, "type": t}
	}
	var feats []map[string]any
	for _, c := range md.XGB.Inputs {
		feats = append(feats, field(c))
	}
	res["featureColumns"] = feats
	res["labelColumns"] = []map[string]any{field(md.XGB.Output)}
	res["trainingRuns"] = []map[string]any{{
		"trainingOptions": map[string]any{"modelUri": md.XGB.ModelPath},
		"startTime":       timeRFC3339(md.Created),
	}}
}

// xgbLoad is the model's kept file, loaded.
func (f front) xgbLoad(imp *xgbImport) (*xgbmodel.Model, int, string) {
	data, err := f.models.blob(imp.SHA256)
	if err != nil {
		return nil, 500, "cloudburrow: the model's XGBoost file is missing from the state directory: " + err.Error()
	}
	m, err := xgbmodel.Load(data)
	if err != nil {
		return nil, 500, "cloudburrow: could not load the model's XGBoost file: " + err.Error()
	}
	return m, 0, ""
}

// xgbSubquery is the subquery of ML.PREDICT or ML.FEATURE_IMPORTANCE of an
// imported XGBoost model.
func (f front) xgbSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	imp := md.XGB
	switch c.fn {
	case "PREDICT":
	case "FEATURE_IMPORTANCE":
		m, code, msg := f.xgbLoad(imp)
		if code != 0 {
			return "", code, msg
		}
		imps := m.Importances()
		if imps == nil {
			return "", 400, "ML.FEATURE_IMPORTANCE is not supported for an XGBoost model of booster gblinear: it has no trees"
		}
		var rows []string
		for _, fi := range imps {
			rows = append(rows, fmt.Sprintf("(%s, %d, %s, %s)", sqlString(imp.Inputs[fi.Feature].Name), fi.Weight, sqlF(fi.Gain), sqlF(fi.Cover)))
		}
		return mlRowsSubquery("STRUCT<feature STRING, importance_weight INT64, importance_gain FLOAT64, importance_cover FLOAT64>", rows), 0, ""
	default:
		return "", 400, "ML." + c.fn + " is not supported for model type XGBOOST: imported XGBoost models work only with the ML.PREDICT and ML.FEATURE_IMPORTANCE functions"
	}
	if c.input == "" {
		return "", 400, "ML.PREDICT requires an input: a query in parentheses or TABLE name"
	}
	if len(c.struc) > 0 {
		return "", 400, "ML.PREDICT of an imported XGBoost model takes no STRUCT of options"
	}
	m, code, msg := f.xgbLoad(imp)
	if code != 0 {
		return "", code, msg
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
	for i, in := range imp.Inputs {
		col, ok := byName[strings.ToLower(in.Name)]
		if !ok {
			return "", 400, "Invalid table-valued function ML.PREDICT Column " + in.Name + " is not found in the input data to the PREDICT function."
		}
		switch bqColumnType(col.Type) {
		case "INT64", "FLOAT64", "NUMERIC", "BIGNUMERIC", "BOOL":
		default:
			return "", 400, fmt.Sprintf("Invalid table-valued function ML.PREDICT Column %s of type %s cannot be converted to the type the model's input %s expects, %s",
				col.Name, bqColumnType(col.Type), in.Name, in.Type)
		}
		refs = append(refs, "CAST(_cbml_in."+sqlIdent(col.Name)+" AS FLOAT64) AS c"+strconv.Itoa(i))
	}
	key := "TO_JSON_STRING(STRUCT(" + strings.Join(refs, ", ") + "))"
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT DISTINCT "+key+" AS k FROM ("+c.input+") AS _cbml_in")
	if code != 0 {
		return "", code, msg
	}
	out := imp.Output
	var lits []string
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		var vals map[string]json.RawMessage
		if err := json.Unmarshal([]byte(*row[0]), &vals); err != nil {
			return "", 500, "cloudburrow: could not read the input's values: " + err.Error()
		}
		x := make([]float64, len(imp.Inputs))
		for i := range x {
			raw := vals["c"+strconv.Itoa(i)]
			x[i] = math.NaN()
			if len(raw) > 0 && string(raw) != "null" {
				var t onnxmodel.Tensor
				if why := appendONNXValue(&t, "DOUBLE", raw); why != "" {
					return "", 400, "ML.PREDICT input column " + imp.Inputs[i].Name + ": " + why
				}
				x[i] = t.F[0]
			}
		}
		p, err := m.Predict(x)
		if err != nil {
			return "", 400, "The XGBoost model failed to run: " + err.Error()
		}
		var vs []string
		for _, v := range p {
			vs = append(vs, onnxLiteral(onnxmodel.Tensor{F: []float64{v}}, "FLOAT64", 0))
		}
		lit := vs[0]
		if out.Type != "FLOAT64" {
			lit = "[" + strings.Join(vs, ", ") + "]"
		}
		lits = append(lits, "STRUCT("+sqlString(*row[0])+", "+lit+")")
	}
	in := "_cbml_in.*"
	if col, ok := byName[strings.ToLower(out.Name)]; ok {
		in = "_cbml_in.* EXCEPT (" + sqlIdent(col.Name) + ")"
	}
	name := sqlIdent(out.Name)
	return "(SELECT _cbml_p." + name + " AS " + name + ", " + in + " FROM (" + c.input + ") AS _cbml_in LEFT JOIN UNNEST(ARRAY<STRUCT<_cbml_k STRING, " +
		name + " " + out.Type + ">>[" + strings.Join(lits, ", ") + "]) AS _cbml_p ON " + key + " = _cbml_p._cbml_k)", 0, ""
}
