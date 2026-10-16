package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
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
var mlFunctionsServed = map[string]bool{"PREDICT": true, "EVALUATE": true, "WEIGHTS": true, "FEATURE_INFO": true, "TRAINING_INFO": true}

// mlCall is one ML.<fn>(MODEL ...) call in a query's text.
type mlCall struct {
	fn       string
	path     []string
	input    string // the input query, or ""
	struc    map[string]string
	pos, end int
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
				". CloudBurrow answers ML.PREDICT, ML.EVALUATE, ML.WEIGHTS, ML.FEATURE_INFO and ML.TRAINING_INFO. Nothing was run.")
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

// linearSQL is the SQL of the model's linear value over its input's
// columns, as predictRow computes it.
func (t mlTrained) linearSQL() string {
	terms := []string{sqlF(t.Intercept)}
	for _, ft := range t.Features {
		if ft.numeric() {
			if ft.Stddev == 0 || ft.Weight == 0 {
				continue
			}
			terms = append(terms, fmt.Sprintf("%s * ((IFNULL(CAST(%s AS FLOAT64), %s) - %s) / %s)", sqlF(ft.Weight), sqlIdent(ft.Name),
				sqlF(ft.Mean), sqlF(ft.Mean), sqlF(ft.Stddev)))
			continue
		}
		var cs strings.Builder
		null := "0.0"
		if ft.NullSeen {
			null = sqlF(ft.CategoryWeights[len(ft.CategoryWeights)-1])
		}
		fmt.Fprintf(&cs, "(CASE WHEN %s IS NULL THEN %s ELSE CASE CAST(%s AS STRING)", sqlIdent(ft.Name), null, sqlIdent(ft.Name))
		for i, cat := range ft.Categories {
			fmt.Fprintf(&cs, " WHEN %s THEN %s", sqlString(cat), sqlF(ft.CategoryWeights[i]))
		}
		cs.WriteString(" ELSE 0.0 END END)")
		terms = append(terms, cs.String())
	}
	return "(" + strings.Join(terms, " + ") + ")"
}

// mlSubquery is the subquery that stands for call c of model md.
func (f front) mlSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	t := md.Trained
	logistic := md.Options.ModelType == "LOGISTIC_REG"
	switch c.fn {
	case "PREDICT":
		if c.input == "" {
			return "", 400, "ML.PREDICT requires an input: a query in parentheses or TABLE name"
		}
		out := sqlIdent("predicted_" + t.Label)
		if !logistic {
			return "(SELECT *, " + t.linearSQL() + " AS " + out + " FROM (" + c.input + ") AS _cbml_in)", 0, ""
		}
		thr, why := c.threshold()
		if why != "" {
			return "", 400, why
		}
		neg, pos := sqlLabel(t.Classes[0], t.LabelType), sqlLabel(t.Classes[1], t.LabelType)
		probs := sqlIdent("predicted_" + t.Label + "_probs")
		return "(SELECT * EXCEPT (_cbml_z, _cbml_p), IF(_cbml_p >= " + sqlF(thr) + ", " + pos + ", " + neg + ") AS " + out +
			", [STRUCT(" + pos + " AS label, _cbml_p AS prob), STRUCT(" + neg + " AS label, 1 - _cbml_p AS prob)] AS " + probs +
			" FROM (SELECT *, IF(_cbml_z >= 0, 1 / (1 + EXP(-_cbml_z)), EXP(_cbml_z) / (1 + EXP(_cbml_z))) AS _cbml_p" +
			" FROM (SELECT *, " + t.linearSQL() + " AS _cbml_z FROM (" + c.input + ") AS _cbml_in)))", 0, ""
	case "EVALUATE":
		metrics := t.Eval
		thr, why := c.threshold()
		if why != "" {
			return "", 400, why
		}
		if c.input != "" || (logistic && thr != 0.5) {
			if c.input == "" {
				return "", 501, "Not implemented here: ML.EVALUATE with a threshold and no input; CloudBurrow keeps the training " +
					"evaluation at 0.5 only. Give the input query. Nothing was run."
			}
			cols, rows, code, msg := f.queryAllRows(r, q, c.input)
			if code != 0 {
				return "", code, msg
			}
			li := -1
			for i, col := range cols {
				if strings.EqualFold(col.Name, t.Label) {
					li = i
				}
			}
			if li < 0 {
				return "", 400, "The input data of ML.EVALUATE has no label column " + t.Label
			}
			var y, p []float64
			for _, row := range rows {
				if row[li] == nil {
					continue
				}
				vals := map[string]*string{}
				for i, col := range cols {
					vals[strings.ToLower(col.Name)] = row[i]
				}
				if logistic {
					v := 0.0
					if *row[li] == t.Classes[1] {
						v = 1
					} else if *row[li] != t.Classes[0] {
						return "", 400, "Label " + *row[li] + " of the input data is not one of the model's labels"
					}
					y = append(y, v)
				} else {
					v, err := strconv.ParseFloat(*row[li], 64)
					if err != nil {
						return "", 400, "Label value " + *row[li] + " is not numeric"
					}
					y = append(y, v)
				}
				p = append(p, t.predictRow(vals))
			}
			if len(y) == 0 {
				return "", 400, "The input data of ML.EVALUATE has no rows with a label"
			}
			if logistic {
				metrics = classificationMetrics(y, p, thr)
			} else {
				metrics = regressionMetrics(y, p)
			}
		}
		names := regressionMetricNames
		if logistic {
			names = classificationMetricNames
		}
		var cols, vals []string
		for _, n := range names {
			cols = append(cols, n+" FLOAT64")
			vals = append(vals, sqlFloat(metrics[n]))
		}
		if logistic {
			cols = append([]string{"threshold FLOAT64"}, cols...)
			vals = append([]string{sqlF(thr)}, vals...)
		}
		return mlRowsSubquery("STRUCT<"+strings.Join(cols, ", ")+">", []string{"(" + strings.Join(vals, ", ") + ")"}), 0, ""
	case "WEIGHTS":
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
		var rows []string
		for _, it := range t.Iterations {
			rows = append(rows, fmt.Sprintf("(0, %d, %s, %s, %s, %d)", it.Index, sqlF(it.Loss), sqlFloat(it.EvalLoss), sqlF(it.LearnRate), it.DurationMs))
		}
		return mlRowsSubquery("STRUCT<training_run INT64, iteration INT64, loss FLOAT64, eval_loss FLOAT64, learning_rate FLOAT64, duration_ms INT64>", rows), 0, ""
	}
	return "", 501, "Not implemented here: ML." + c.fn
}
