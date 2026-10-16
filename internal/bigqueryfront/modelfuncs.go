package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The ML table functions over the models the front keeps (#1465,
// models.go), each written into the query as a subquery before the
// engine runs it, as INFORMATION_SCHEMA views are (infoschemaviews.go):
//
//   - ML.PREDICT(MODEL m, (query) | TABLE t [, STRUCT(threshold)]): the
//     input's rows with predicted_<label> (and, for a logistic model,
//     predicted_<label>_probs) added, computed by the engine from the
//     model's weights as SQL, so every row of the input is predicted where
//     it is read;
//   - ML.EVALUATE(MODEL m [, input [, STRUCT(threshold)]]): the metrics on
//     the input's rows, computed by the front, or with no input those
//     computed at training (on the evaluation rows when the data was
//     split);
//   - ML.WEIGHTS(MODEL m [, STRUCT(standardize)]), ML.FEATURE_INFO(MODEL
//     m) and ML.TRAINING_INFO(MODEL m): what training learned.
//
// Any other ML function of a MODEL is 501, naming it.

// mlFunctionsServed are the ML functions of a model CloudBurrow answers.
var mlFunctionsServed = map[string]bool{"PREDICT": true, "EVALUATE": true, "WEIGHTS": true, "FEATURE_INFO": true, "TRAINING_INFO": true,
	"CONFUSION_MATRIX": true, "ROC_CURVE": true, "CENTROIDS": true, "PRINCIPAL_COMPONENTS": true, "PRINCIPAL_COMPONENT_INFO": true}

// mlCall is one ML.<fn>(MODEL ...) call in a query's text.
type mlCall struct {
	fn    string
	path  []string
	input string // the input query, or ""
	struc map[string]string
	// thresholds are ML.ROC_CURVE's GENERATE_ARRAY argument.
	thresholds []float64
	pos, end   int
}

// findMLCalls finds the ML function calls of a MODEL in sql. msg is a
// syntax error.
func findMLCalls(sql string) ([]mlCall, string) {
	if !strings.Contains(strings.ToUpper(sql), "ML") {
		return nil, ""
	}
	toks, ok := lex(sql)
	if !ok {
		return nil, ""
	}
	var calls []mlCall
	for i := 0; i+4 < len(toks); i++ {
		if !toks[i].is("ML") || !toks[i+1].punct(".") || toks[i+2].kind != tokWord || !toks[i+3].punct("(") || !toks[i+4].is("MODEL") {
			continue
		}
		end := matchParen(toks, i+3)
		if end < 0 {
			return nil, "Syntax error: unclosed ML." + toks[i+2].text
		}
		c := mlCall{fn: strings.ToUpper(toks[i+2].text), pos: toks[i].pos, end: toks[end].end}
		path, n := readModelPath(toks[i+5 : end])
		if n == 0 {
			return nil, "Syntax error: expected a model name after MODEL"
		}
		c.path = path
		// The other arguments, split at top-level commas.
		var args [][]token
		rest := toks[i+5+n : end]
		if len(rest) > 0 {
			if !rest[0].punct(",") {
				return nil, "Syntax error: unexpected " + rest[0].text + " in ML." + c.fn
			}
			depth, start := 0, 1
			for k := 1; k <= len(rest); k++ {
				if k == len(rest) || depth == 0 && rest[k].punct(",") {
					args = append(args, rest[start:k])
					start = k + 1
					continue
				}
				if rest[k].punct("(") || rest[k].punct("[") {
					depth++
				}
				if rest[k].punct(")") || rest[k].punct("]") {
					depth--
				}
			}
		}
		for _, a := range args {
			switch {
			case len(a) == 0:
				return nil, "Syntax error: empty argument in ML." + c.fn
			case a[0].is("STRUCT"):
				m, msg := mlStruct(a)
				if msg != "" {
					return nil, msg
				}
				c.struc = m
			case a[0].punct("("):
				c.input = strings.TrimSpace(sql[a[0].end:a[len(a)-1].pos])
			case a[0].is("GENERATE_ARRAY"):
				th, msg := mlGenerateArray(a)
				if msg != "" {
					return nil, msg
				}
				c.thresholds = th
			case a[0].is("TABLE"):
				p, n := readModelPath(a[1:])
				if n == 0 || n+1 != len(a) {
					return nil, "Syntax error: expected a table name after TABLE"
				}
				c.input = "SELECT * FROM " + quotePath(p)
			default:
				return nil, "Syntax error: unexpected " + a[0].text + " in ML." + c.fn
			}
		}
		calls = append(calls, c)
		// Skip past the call: an input query holds no ML call of a model
		// the front would need to write first (it is written with the call).
		for i+1 < len(toks) && toks[i+1].pos < c.end {
			i++
		}
	}
	return calls, ""
}

// mlGenerateArray reads GENERATE_ARRAY(start, end[, step]) of number
// literals into its values.
func mlGenerateArray(a []token) ([]float64, string) {
	bad := "Syntax error: GENERATE_ARRAY takes number literals (start, end[, step])"
	if len(a) < 4 || !a[1].punct("(") || !a[len(a)-1].punct(")") {
		return nil, bad
	}
	var nums []float64
	cur := ""
	for _, t := range a[2 : len(a)-1] {
		if t.punct(",") {
			v, err := strconv.ParseFloat(cur, 64)
			if err != nil {
				return nil, bad
			}
			nums, cur = append(nums, v), ""
			continue
		}
		cur += t.text
	}
	v, err := strconv.ParseFloat(cur, 64)
	if err != nil {
		return nil, bad
	}
	nums = append(nums, v)
	if len(nums) == 2 {
		nums = append(nums, 1)
	}
	if len(nums) != 3 || nums[2] == 0 {
		return nil, bad
	}
	var out []float64
	for x, i := nums[0], 0; (nums[2] > 0 && x <= nums[1]+1e-12) || (nums[2] < 0 && x >= nums[1]-1e-12); i++ {
		out = append(out, math.Round(x*1e12)/1e12)
		x = nums[0] + float64(i+1)*nums[2]
		if i > 10000 {
			return nil, "GENERATE_ARRAY gives too many thresholds"
		}
	}
	return out, ""
}

// mlStruct reads STRUCT(value AS name, ...) into name -> value.
func mlStruct(a []token) (map[string]string, string) {
	if len(a) < 3 || !a[1].punct("(") || !a[len(a)-1].punct(")") {
		return nil, "Syntax error in STRUCT argument"
	}
	m := map[string]string{}
	inner := a[2 : len(a)-1]
	for k := 0; k < len(inner); {
		j := k
		for j < len(inner) && !inner[j].is("AS") {
			j++
		}
		if j+1 >= len(inner) || j == k {
			return nil, "Syntax error in STRUCT argument: each field needs AS name"
		}
		v := ""
		for _, t := range inner[k:j] {
			v += t.text
		}
		m[strings.ToLower(inner[j+1].text)] = v
		k = j + 2
		if k < len(inner) && inner[k].punct(",") {
			k++
		}
	}
	return m, ""
}

// mlFunctions answers q when it calls ML functions of models, and reports
// whether it did.
func (f front) mlFunctions(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.models == nil {
		return false
	}
	calls, msg := findMLCalls(q.Query)
	if msg != "" {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: msg})
		return true
	}
	if len(calls) == 0 {
		return false
	}
	var b strings.Builder
	last := 0
	for _, c := range calls {
		if !mlFunctionsServed[c.fn] {
			writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: ML."+c.fn+
				". CloudBurrow answers ML.PREDICT, ML.EVALUATE, ML.WEIGHTS, ML.FEATURE_INFO, ML.TRAINING_INFO, ML.CONFUSION_MATRIX, "+
				"ML.ROC_CURVE, ML.CENTROIDS, ML.PRINCIPAL_COMPONENTS and ML.PRINCIPAL_COMPONENT_INFO. Nothing was run.")
			return true
		}
		project, dataset, id, ok := f.resolveModel(c.path, defaultDatasetOf(q))
		if !ok {
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery",
				Message: "Model name " + strings.Join(c.path, ".") + " missing dataset while no default dataset is set in the request."})
			return true
		}
		md, ok := f.models.get(project, dataset, id)
		if !ok {
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusNotFound, rowError{Reason: "notFound",
				Message: fmt.Sprintf("Not found: Model %s:%s.%s", project, dataset, id)})
			return true
		}
		sub, code, why := f.mlSubquery(r, q, md, c)
		if code != 0 {
			reason := map[int]string{400: "invalidQuery", 501: "notImplemented"}[code]
			if code == 501 {
				writeError(w, code, reason, why)
				return true
			}
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: why})
			return true
		}
		b.WriteString(q.Query[last:c.pos])
		b.WriteString(sub)
		last = c.end
	}
	b.WriteString(q.Query[last:])
	sql := b.String()
	if !setQueryText(r, insert, sql) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	client := jobText{query: q.Query}
	q.Query = sql
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
	return true
}

// categoryWeightsType is ML.WEIGHTS' category_weights type.
const categoryWeightsType = "ARRAY<STRUCT<category STRING, weight FLOAT64>>"

// sqlFloat is a FLOAT64 literal, or NULL.
func sqlFloat(v *float64) string {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return "NULL"
	}
	s := strconv.FormatFloat(*v, 'g', 17, 64)
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	if strings.HasPrefix(s, "-") {
		return "(" + s + ")"
	}
	return s
}

func sqlF(v float64) string { return sqlFloat(&v) }

// sqlIdent quotes a column name.
func sqlIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "\\`") + "`" }

// sqlLabel is a label value as a literal of the label's type.
func sqlLabel(v, typ string) string {
	switch typ {
	case "STRING":
		return sqlString(v)
	case "BOOL":
		return strings.ToUpper(v)
	}
	return v
}

// rowsSubquery is a subquery of rows of the row type columns.
func mlRowsSubquery(columns string, rows []string) string {
	return "(SELECT * FROM UNNEST(ARRAY<" + columns + ">[" + strings.Join(rows, ", ") + "]))"
}

// threshold is a call's threshold option (0.5 by default).
func (c mlCall) threshold() (float64, string) {
	v, ok := c.struc["threshold"]
	if !ok {
		return 0.5, ""
	}
	t, err := strconv.ParseFloat(v, 64)
	if err != nil || t < 0 || t > 1 {
		return 0, "Invalid threshold " + v + ": it must be between 0 and 1"
	}
	return t, ""
}

// encodedSQL is the SQL of each of the model's encoded columns (no
// intercept), as encodeFeatureMode computes them, ref naming a column.
func (t mlTrained) encodedSQL(ref func(string) string) []string {
	var out []string
	for _, ft := range t.Features {
		col := ref(ft.Name)
		if ft.numeric() {
			x := fmt.Sprintf("IFNULL(CAST(%s AS FLOAT64), %s)", col, sqlF(ft.Mean))
			switch {
			case t.Scaling == "raw":
				out = append(out, x)
			case t.Scaling == "center":
				out = append(out, fmt.Sprintf("(%s - %s)", x, sqlF(ft.Mean)))
			case ft.Stddev == 0:
				out = append(out, "0.0")
			default:
				out = append(out, fmt.Sprintf("((%s - %s) / %s)", x, sqlF(ft.Mean), sqlF(ft.Stddev)))
			}
			continue
		}
		for _, cat := range ft.Categories {
			out = append(out, fmt.Sprintf("IF(CAST(%s AS STRING) = %s, 1.0, 0.0)", col, sqlString(cat)))
		}
		if ft.NullSeen {
			out = append(out, fmt.Sprintf("IF(%s IS NULL, 1.0, 0.0)", col))
		}
	}
	return out
}

// dotSQL is w · (1, enc...), w's first entry the intercept.
func dotSQL(w []float64, enc []string) string {
	terms := []string{sqlF(w[0])}
	for j, e := range enc {
		if w[j+1] != 0 {
			terms = append(terms, sqlF(w[j+1])+" * "+e)
		}
	}
	return "(" + strings.Join(terms, " + ") + ")"
}

// mlInput is the FROM of a call's input, the model's TRANSFORM applied
// (modeltransform.go), how to name a model column in it, and the helper
// columns to leave out of the output. The TRANSFORM's items are columns
// of their own (_cbml_t<n>), not a STRUCT: the engine loses a FLOAT64
// STRUCT field's type, so that dividing it divides integers (measured).
func (md bqModel) mlInput(input string) (from string, ref func(string) string, helpers []string) {
	if len(md.Transform) == 0 {
		return "(" + input + ") AS _cbml_in", sqlIdent, nil
	}
	cols := map[string]string{}
	var sel []string
	for k, it := range md.predictTransform() {
		expr, name := transformItemName(it)
		h := fmt.Sprintf("_cbml_t%d", k)
		cols[strings.ToLower(name)] = h
		sel = append(sel, "("+expr+") AS "+h)
		helpers = append(helpers, h)
	}
	return "(SELECT *, " + strings.Join(sel, ", ") + " FROM (" + input + ")) AS _cbml_in",
		func(n string) string {
			if h, ok := cols[strings.ToLower(n)]; ok {
				return h
			}
			return sqlIdent(n)
		}, helpers
}

func except(cols []string) string {
	if len(cols) == 0 {
		return "*"
	}
	return "* EXCEPT (" + strings.Join(cols, ", ") + ")"
}

// mlLabelRows reads a classifier's or regressor's evaluation input:
// each row's label (a class index, or the value) and its prediction (the
// positive probability, the class probabilities, or the value).
func (f front) mlLabelRows(r *http.Request, q queryOptions, md bqModel, input string) (y []float64, p []float64, probs [][]float64, code int, msg string) {
	t := md.Trained
	sql := input
	if len(md.Transform) > 0 {
		sql = "SELECT " + strings.Join(md.trainTransform(), ", ") + " FROM (" + input + ")"
	}
	cols, rows, code, msg := f.queryAllRows(r, q, sql)
	if code != 0 {
		return nil, nil, nil, code, msg
	}
	li := -1
	for i, col := range cols {
		if strings.EqualFold(col.Name, t.Label) {
			li = i
		}
	}
	if li < 0 {
		return nil, nil, nil, 400, "The input data has no label column " + t.Label
	}
	for _, row := range rows {
		if row[li] == nil {
			continue
		}
		vals := map[string]*string{}
		for i, col := range cols {
			vals[strings.ToLower(col.Name)] = row[i]
		}
		if len(t.Classes) > 0 {
			k := -1
			for i, c := range t.Classes {
				if *row[li] == c {
					k = i
				}
			}
			if k < 0 {
				return nil, nil, nil, 400, "Label " + *row[li] + " of the input data is not one of the model's labels"
			}
			y = append(y, float64(k))
		} else {
			v, err := strconv.ParseFloat(*row[li], 64)
			if err != nil {
				return nil, nil, nil, 400, "Label value " + *row[li] + " is not numeric"
			}
			y = append(y, v)
		}
		if t.multiclass() {
			probs = append(probs, t.classProbs(vals))
		} else {
			p = append(p, t.predictRow(vals))
		}
	}
	if len(y) == 0 {
		return nil, nil, nil, 400, "The input data has no rows with a label"
	}
	return y, p, probs, 0, ""
}

// mlFeatureRows reads an unsupervised model's input rows, encoded.
func (f front) mlFeatureRows(r *http.Request, q queryOptions, md bqModel, input string) ([][]float64, int, string) {
	sql := input
	if len(md.Transform) > 0 {
		sql = "SELECT " + strings.Join(md.trainTransform(), ", ") + " FROM (" + input + ")"
	}
	cols, rows, code, msg := f.queryAllRows(r, q, sql)
	if code != 0 {
		return nil, code, msg
	}
	if len(rows) == 0 {
		return nil, 400, "The input data has no rows"
	}
	var X [][]float64
	for _, row := range rows {
		vals := map[string]*string{}
		for i, col := range cols {
			vals[strings.ToLower(col.Name)] = row[i]
		}
		X = append(X, md.Trained.encodeRow(vals)[1:])
	}
	return X, 0, ""
}

// columnName is a class label as a column name: itself when it is one,
// else with each character a column name cannot hold made _, and _
// before a leading digit (BigQuery documents "0" becoming "_0"; the rest
// is UNVERIFIED), made unique with a _<n> suffix.
func columnNames(labels []string) []string {
	seen := map[string]bool{}
	out := make([]string, len(labels))
	for i, l := range labels {
		var b strings.Builder
		for _, r := range l {
			if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		n := b.String()
		if n == "" || n[0] >= '0' && n[0] <= '9' {
			n = "_" + n
		}
		base := n
		for k := 1; seen[strings.ToLower(n)]; k++ {
			n = base + "_" + strconv.Itoa(k)
		}
		seen[strings.ToLower(n)] = true
		out[i] = n
	}
	return out
}

// mlSubquery is the subquery that stands for call c of model md.
func (f front) mlSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	t := md.Trained
	typ := md.Options.ModelType
	logistic := typ == "LOGISTIC_REG"
	binary := logistic && !t.multiclass()
	unsupported := func() (string, int, string) {
		return "", 400, "ML." + c.fn + " is not supported for model type " + typ
	}
	needInput := func() (string, int, string) {
		return "", 400, "ML." + c.fn + " requires an input: a query in parentheses or TABLE name"
	}
	switch c.fn {
	case "PREDICT":
		if c.input == "" {
			return needInput()
		}
		from, ref, helpers := md.mlInput(c.input)
		enc := t.encodedSQL(ref)
		out := sqlIdent("predicted_" + t.Label)
		switch {
		case typ == "LINEAR_REG":
			return "(SELECT " + except(helpers) + ", " + dotSQL(t.weightVector(), enc) + " AS " + out + " FROM " + from + ")", 0, ""
		case binary:
			thr, why := c.threshold()
			if why != "" {
				return "", 400, why
			}
			neg, pos := sqlLabel(t.Classes[0], t.LabelType), sqlLabel(t.Classes[1], t.LabelType)
			probs := sqlIdent("predicted_" + t.Label + "_probs")
			return "(SELECT * EXCEPT (_cbml_z, _cbml_p), IF(_cbml_p >= " + sqlF(thr) + ", " + pos + ", " + neg + ") AS " + out +
				", [STRUCT(" + pos + " AS label, _cbml_p AS prob), STRUCT(" + neg + " AS label, 1 - _cbml_p AS prob)] AS " + probs +
				" FROM (SELECT *, IF(_cbml_z >= 0, 1 / (1 + EXP(-_cbml_z)), EXP(_cbml_z) / (1 + EXP(_cbml_z))) AS _cbml_p" +
				" FROM (SELECT " + except(helpers) + ", " + dotSQL(t.weightVector(), enc) + " AS _cbml_z FROM " + from + ")))", 0, ""
		case logistic:
			if _, ok := c.struc["threshold"]; ok {
				return "", 400, "The threshold option is only supported for binary classification models"
			}
			// Softmax over the class scores, the most probable class
			// predicted (the first on a tie); _probs lists the classes in
			// their sorted order (UNVERIFIED).
			K := len(t.Classes)
			var zs, helpersZ, ps, cases []string
			for k := 0; k < K; k++ {
				zs = append(zs, fmt.Sprintf("%s AS _cbml_z%d", dotSQL(t.ClassWeights[k], enc), k))
				helpersZ = append(helpersZ, fmt.Sprintf("_cbml_z%d", k))
			}
			mx := "GREATEST(" + strings.Join(helpersZ, ", ") + ")"
			var es []string
			for k := 0; k < K; k++ {
				es = append(es, fmt.Sprintf("EXP(_cbml_z%d - _cbml_m)", k))
			}
			sum := "(" + strings.Join(es, " + ") + ")"
			for k := 0; k < K; k++ {
				ps = append(ps, fmt.Sprintf("STRUCT(%s AS label, EXP(_cbml_z%d - _cbml_m) / %s AS prob)", sqlLabel(t.Classes[k], t.LabelType), k, sum))
				var conds []string
				for j := 0; j < K; j++ {
					if j < k {
						conds = append(conds, fmt.Sprintf("_cbml_z%d > _cbml_z%d", k, j))
					} else if j > k {
						conds = append(conds, fmt.Sprintf("_cbml_z%d >= _cbml_z%d", k, j))
					}
				}
				cases = append(cases, fmt.Sprintf("WHEN %s THEN %s", strings.Join(conds, " AND "), sqlLabel(t.Classes[k], t.LabelType)))
			}
			probs := sqlIdent("predicted_" + t.Label + "_probs")
			return "(SELECT * EXCEPT (" + strings.Join(helpersZ, ", ") + ", _cbml_m), CASE " + strings.Join(cases, " ") + " END AS " + out +
				", [" + strings.Join(ps, ", ") + "] AS " + probs +
				" FROM (SELECT *, " + mx + " AS _cbml_m FROM (SELECT " + except(helpers) + ", " + strings.Join(zs, ", ") + " FROM " + from + ")))", 0, ""
		case typ == "KMEANS":
			// The nearest centroid and the distances to the nearest
			// min(num_clusters, 5), nearest first.
			K := len(t.Centroids)
			var ds, dn, items []string
			for k, cen := range t.Centroids {
				ds = append(ds, distanceSQL(enc, cen, t.Distance)+fmt.Sprintf(" AS _cbml_d%d", k))
				dn = append(dn, fmt.Sprintf("_cbml_d%d", k))
				items = append(items, fmt.Sprintf("STRUCT(%d AS CENTROID_ID, _cbml_d%d AS DISTANCE)", k+1, k))
			}
			near := fmt.Sprintf("ARRAY(SELECT AS STRUCT * FROM UNNEST([%s]) ORDER BY DISTANCE, CENTROID_ID LIMIT %d)", strings.Join(items, ", "), min(K, 5))
			return "(SELECT _cbml_n[OFFSET(0)].CENTROID_ID AS centroid_id, _cbml_n AS nearest_centroids_distance, * EXCEPT (_cbml_n)" +
				" FROM (SELECT * EXCEPT (" + strings.Join(dn, ", ") + "), " + near + " AS _cbml_n" +
				" FROM (SELECT " + except(helpers) + ", " + strings.Join(ds, ", ") + " FROM " + from + ")))", 0, ""
		case typ == "PCA":
			var pcs []string
			for k, v := range t.Components {
				pcs = append(pcs, dotSQL(append([]float64{0}, v...), enc)+fmt.Sprintf(" AS principal_component_%d", k+1))
			}
			if strings.EqualFold(c.struc["keep_original_columns"], "TRUE") {
				return "(SELECT " + strings.Join(pcs, ", ") + ", " + except(helpers) + " FROM " + from + ")", 0, ""
			}
			return "(SELECT " + strings.Join(pcs, ", ") + " FROM " + from + ")", 0, ""
		}
	case "EVALUATE":
		metrics := t.Eval
		var names []string
		switch {
		case typ == "KMEANS":
			names = clusteringMetricNames
			if c.input != "" {
				X, code, msg := f.mlFeatureRows(r, q, md, c.input)
				if code != 0 {
					return "", code, msg
				}
				metrics = clusteringMetrics(X, t.Centroids, t.Distance)
			}
		case typ == "PCA":
			names = []string{"total_explained_variance_ratio"}
			if c.input != "" {
				return "", 501, "Not implemented here: ML.EVALUATE of a PCA model with an input: BigQuery does not document how it is " +
					"computed on new rows; without an input it gives the training's total_explained_variance_ratio. Nothing was run."
			}
		default:
			thr, why := c.threshold()
			if why != "" {
				return "", 400, why
			}
			if _, ok := c.struc["threshold"]; ok && !binary {
				return "", 400, "The threshold option is only supported for binary classification models"
			}
			if c.input != "" || (binary && thr != 0.5) {
				if c.input == "" {
					return "", 400, "The threshold option of ML.EVALUATE requires an input query or table"
				}
				y, p, probs, code, msg := f.mlLabelRows(r, q, md, c.input)
				if code != 0 {
					return "", code, msg
				}
				switch {
				case binary:
					metrics = classificationMetrics(y, p, thr)
				case logistic:
					metrics = multiclassMetrics(y, probs)
				default:
					metrics = regressionMetrics(y, p)
				}
			}
			names = regressionMetricNames
			if logistic {
				names = classificationMetricNames
			}
		}
		var cols, vals []string
		for _, n := range names {
			cols = append(cols, n+" FLOAT64")
			vals = append(vals, sqlFloat(metrics[n]))
		}
		if binary {
			thr, _ := c.threshold()
			cols = append([]string{"threshold FLOAT64"}, cols...)
			vals = append([]string{sqlF(thr)}, vals...)
		}
		return mlRowsSubquery("STRUCT<"+strings.Join(cols, ", ")+">", []string{"STRUCT(" + strings.Join(vals, ", ") + ")"}), 0, ""
	case "CONFUSION_MATRIX":
		if !logistic {
			return unsupported()
		}
		thr, why := c.threshold()
		if why != "" {
			return "", 400, why
		}
		if _, ok := c.struc["threshold"]; ok && (!binary || c.input == "") {
			if !binary {
				return "", 400, "The threshold option is only supported for binary classification models"
			}
			return "", 400, "The threshold option of ML.CONFUSION_MATRIX requires an input query or table"
		}
		y, pred := t.EvalY, t.EvalScore
		if c.input != "" {
			yy, p, probs, code, msg := f.mlLabelRows(r, q, md, c.input)
			if code != 0 {
				return "", code, msg
			}
			y, pred = yy, p
			if !binary {
				pred = nil
				for _, pr := range probs {
					pred = append(pred, float64(argmax(pr)))
				}
			}
		}
		K := len(t.Classes)
		counts := make([][]int, K)
		for k := range counts {
			counts[k] = make([]int, K)
		}
		for i := range y {
			pk := int(pred[i])
			if binary {
				pk = 0
				if pred[i] >= thr {
					pk = 1
				}
			}
			counts[int(y[i])][pk]++
		}
		names := columnNames(t.Classes)
		cols := []string{"expected_label " + t.LabelType}
		for _, n := range names {
			cols = append(cols, sqlIdent(n)+" INT64")
		}
		var rows []string
		for k := range t.Classes {
			vals := []string{sqlLabel(t.Classes[k], t.LabelType)}
			for j := range t.Classes {
				vals = append(vals, strconv.Itoa(counts[k][j]))
			}
			rows = append(rows, "("+strings.Join(vals, ", ")+")")
		}
		return mlRowsSubquery("STRUCT<"+strings.Join(cols, ", ")+">", rows), 0, ""
	case "ROC_CURVE":
		if !binary {
			return "", 400, "ML.ROC_CURVE is only supported for binary classification models"
		}
		y, p := t.EvalY, t.EvalScore
		if c.input != "" {
			yy, pp, _, code, msg := f.mlLabelRows(r, q, md, c.input)
			if code != 0 {
				return "", code, msg
			}
			y, p = yy, pp
		}
		thresholds := c.thresholds
		if thresholds == nil {
			// The percentile values of the predictions (documented), each
			// whole percentile's nearest-rank value, distinct (UNVERIFIED).
			s := append([]float64(nil), p...)
			sortFloats(s)
			seen := map[float64]bool{}
			for pc := 0; pc <= 100; pc++ {
				v := s[int(math.Round(float64(pc)/100*float64(len(s)-1)))]
				if !seen[v] {
					seen[v] = true
					thresholds = append(thresholds, v)
				}
			}
		}
		var rows []string
		for _, th := range thresholds {
			var tp, fp, tn, fn int
			for i := range y {
				switch pos := p[i] >= th; {
				case pos && y[i] == 1:
					tp++
				case pos:
					fp++
				case y[i] == 1:
					fn++
				default:
					tn++
				}
			}
			rec, fpr := math.NaN(), math.NaN()
			if tp+fn > 0 {
				rec = float64(tp) / float64(tp+fn)
			}
			if fp+tn > 0 {
				fpr = float64(fp) / float64(fp+tn)
			}
			rows = append(rows, fmt.Sprintf("(%s, %s, %s, %d, %d, %d, %d)", sqlF(th), sqlF(rec), sqlF(fpr), tp, fp, tn, fn))
		}
		return mlRowsSubquery("STRUCT<threshold FLOAT64, recall FLOAT64, false_positive_rate FLOAT64, true_positives INT64, "+
			"false_positives INT64, true_negatives INT64, false_negatives INT64>", rows), 0, ""
	case "CENTROIDS":
		if typ != "KMEANS" {
			return unsupported()
		}
		std := strings.EqualFold(c.struc["standardize"], "TRUE")
		var rows []string
		for k, cen := range t.Centroids {
			j := 0
			for _, ft := range t.Features {
				if ft.numeric() {
					v := cen[j]
					j++
					switch {
					case ft.Stddev == 0:
						if std {
							v = 0
						} else {
							v = ft.Mean
						}
					case std && t.Scaling == "raw":
						v = (v - ft.Mean) / ft.Stddev
					case !std && t.Scaling == "":
						v = v*ft.Stddev + ft.Mean
					}
					rows = append(rows, fmt.Sprintf("(%d, %s, %s, ARRAY<STRUCT<category STRING, value FLOAT64>>[], CAST(NULL AS STRING))", k+1, sqlString(ft.Name), sqlF(v)))
					continue
				}
				var cv []string
				for _, cat := range ft.Categories {
					cv = append(cv, fmt.Sprintf("(%s, %s)", sqlString(cat), sqlF(cen[j])))
					j++
				}
				if ft.NullSeen {
					cv = append(cv, fmt.Sprintf("(NULL, %s)", sqlF(cen[j])))
					j++
				}
				rows = append(rows, fmt.Sprintf("(%d, %s, NULL, ARRAY<STRUCT<category STRING, value FLOAT64>>[%s], CAST(NULL AS STRING))", k+1, sqlString(ft.Name), strings.Join(cv, ", ")))
			}
		}
		return mlRowsSubquery("STRUCT<centroid_id INT64, feature STRING, numerical_value FLOAT64, categorical_value ARRAY<STRUCT<category STRING, value FLOAT64>>, geography_value STRING>", rows), 0, ""
	case "PRINCIPAL_COMPONENTS":
		if typ != "PCA" {
			return unsupported()
		}
		var rows []string
		for k, v := range t.Components {
			for j, ft := range t.Features {
				rows = append(rows, fmt.Sprintf("(%d, %s, %s, ARRAY<STRUCT<category STRING, value FLOAT64>>[])", k+1, sqlString(ft.Name), sqlF(v[j])))
			}
		}
		return mlRowsSubquery("STRUCT<principal_component_id INT64, feature STRING, numerical_value FLOAT64, categorical_value ARRAY<STRUCT<category STRING, value FLOAT64>>>", rows), 0, ""
	case "PRINCIPAL_COMPONENT_INFO":
		if typ != "PCA" {
			return unsupported()
		}
		var rows []string
		var cum float64
		for k, e := range t.Eigenvalues {
			ratio := math.NaN()
			if t.TotalVariance > 0 {
				ratio = e / t.TotalVariance
				cum += ratio
			}
			c := cum
			if math.IsNaN(ratio) {
				c = math.NaN()
			}
			rows = append(rows, fmt.Sprintf("(%d, %s, %s, %s)", k+1, sqlF(e), sqlF(ratio), sqlF(c)))
		}
		return mlRowsSubquery("STRUCT<principal_component_id INT64, eigenvalue FLOAT64, explained_variance_ratio FLOAT64, cumulative_explained_variance_ratio FLOAT64>", rows), 0, ""
	case "WEIGHTS":
		if typ == "KMEANS" || typ == "PCA" {
			return unsupported()
		}
		if t.multiclass() {
			return "", 501, "Not implemented here: ML.WEIGHTS of a multiclass LOGISTIC_REG model: BigQuery does not document its output's " +
				"per-class layout in the detail a faithful result needs. ML.PREDICT, ML.EVALUATE and ML.CONFUSION_MATRIX serve it. Nothing was run."
		}
		std := strings.EqualFold(c.struc["standardize"], "TRUE")
		var rows []string
		intercept := t.Intercept
		for _, ft := range t.Features {
			if ft.numeric() {
				wt := ft.Weight
				if !std {
					if ft.Stddev == 0 {
						wt = 0
					} else {
						wt = ft.Weight / ft.Stddev
						intercept -= wt * ft.Mean
					}
				}
				rows = append(rows, fmt.Sprintf("(%s, %s, %s[])", sqlString(ft.Name), sqlF(wt), categoryWeightsType))
				continue
			}
			var cw []string
			for i, cat := range ft.Categories {
				cw = append(cw, fmt.Sprintf("(%s, %s)", sqlString(cat), sqlF(ft.CategoryWeights[i])))
			}
			if ft.NullSeen {
				cw = append(cw, fmt.Sprintf("(NULL, %s)", sqlF(ft.CategoryWeights[len(ft.CategoryWeights)-1])))
			}
			rows = append(rows, fmt.Sprintf("(%s, NULL, %s[%s])", sqlString(ft.Name), categoryWeightsType, strings.Join(cw, ", ")))
		}
		rows = append(rows, fmt.Sprintf("('__INTERCEPT__', %s, %s[])", sqlF(intercept), categoryWeightsType))
		return mlRowsSubquery("STRUCT<processed_input STRING, weight FLOAT64, category_weights ARRAY<STRUCT<category STRING, weight FLOAT64>>>", rows), 0, ""
	case "FEATURE_INFO":
		var rows []string
		for _, ft := range t.Features {
			if ft.numeric() {
				sd := "NULL"
				if t.Rows-int(ft.NullCount) > 1 {
					sd = sqlF(ft.Stddev)
				}
				rows = append(rows, fmt.Sprintf("(%s, %s, %s, %s, %s, %s, NULL, %d, 1)", sqlString(ft.Name), sqlF(ft.Min), sqlF(ft.Max),
					sqlF(ft.Mean), sqlF(ft.Median), sd, ft.NullCount))
				continue
			}
			rows = append(rows, fmt.Sprintf("(%s, NULL, NULL, NULL, NULL, NULL, %d, %d, %d)", sqlString(ft.Name), len(ft.Categories),
				ft.NullCount, ft.dims()))
		}
		return mlRowsSubquery("STRUCT<input STRING, min FLOAT64, max FLOAT64, mean FLOAT64, median FLOAT64, stddev FLOAT64, "+
			"category_count INT64, null_count INT64, dimension INT64>", rows), 0, ""
	case "TRAINING_INFO":
		if typ == "PCA" {
			return "", 501, "Not implemented here: ML.TRAINING_INFO of a PCA model: its decomposition has no iterations, and what " +
				"BigQuery reports for it is undocumented. Nothing was run."
		}
		var rows []string
		for _, it := range t.Iterations {
			lr := sqlF(it.LearnRate)
			if typ == "KMEANS" {
				lr = "NULL"
			}
			rows = append(rows, fmt.Sprintf("(0, %d, %s, %s, %s, %d)", it.Index, sqlF(it.Loss), sqlFloat(it.EvalLoss), lr, it.DurationMs))
		}
		return mlRowsSubquery("STRUCT<training_run INT64, iteration INT64, loss FLOAT64, eval_loss FLOAT64, learning_rate FLOAT64, duration_ms INT64>", rows), 0, ""
	}
	return "", 501, "Not implemented here: ML." + c.fn
}

// distanceSQL is the SQL of the distance from the encoded row enc to the
// centroid cen, as mlDistance computes it.
func distanceSQL(enc []string, cen []float64, typ string) string {
	if typ == "COSINE" {
		var ab, aa []string
		var bb float64
		for j, e := range enc {
			ab = append(ab, sqlF(cen[j])+" * "+e)
			aa = append(aa, e+" * "+e)
			bb += cen[j] * cen[j]
		}
		if bb == 0 {
			return "1.0"
		}
		return fmt.Sprintf("IFNULL(1 - SAFE_DIVIDE(%s, SQRT(%s) * %s), 1.0)", strings.Join(ab, " + "), strings.Join(aa, " + "), sqlF(math.Sqrt(bb)))
	}
	var sq []string
	for j, e := range enc {
		sq = append(sq, fmt.Sprintf("POW(%s - %s, 2)", e, sqlF(cen[j])))
	}
	return "SQRT(" + strings.Join(sq, " + ") + ")"
}

func sortFloats(s []float64) { sort.Float64s(s) }
