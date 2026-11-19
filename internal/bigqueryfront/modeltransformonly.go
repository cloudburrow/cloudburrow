package bigqueryfront

import (
	"net/http"
	"strings"
)

// TRANSFORM_ONLY models and ML.TRANSFORM (#1929):
//
//   - CREATE MODEL m TRANSFORM(select_list) OPTIONS(model_type =
//     'TRANSFORM_ONLY') AS query
//     (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-transform):
//     the TRANSFORM learnt over the query's rows, as any model's is
//     (modeltransform.go, modelpreprocess.go), and nothing trained; the
//     model takes ML.TRANSFORM and ML.FEATURE_INFO, whose rows are the
//     TRANSFORM's output columns' statistics over the training rows.
//   - ML.TRANSFORM(MODEL m, {TABLE t | (query)})
//     (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-transform),
//     of any model made with a TRANSFORM: "the columns specified in the
//     model's TRANSFORM clause", the statistics learnt at training applied.
//     The page's example also shows the input's columns the TRANSFORM does
//     not output after them (culmen_depth_mm, body_mass_g), in the
//     input's order, so CloudBurrow passes those through; as ML.PREDICT,
//     a label, data split or k-means init column the TRANSFORM names by
//     itself need not be in the input, and is one of those passed through
//     when it is (UNVERIFIED beyond the example, docs re-read 2026-11-19).
//     A model with no TRANSFORM is an error, as the page says (its text
//     UNVERIFIED).

// trainTransformOnly is a TRANSFORM_ONLY model of the TRANSFORM's output
// rows: every column a feature, for ML.FEATURE_INFO.
func trainTransformOnly(cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	var fts []mlFeature
	var fidx []int
	for i, c := range cols {
		ty := mlTypeOf(c.Type)
		if ty == "" {
			// "The output columns of select_list can be of any BigQuery
			// supported data type": counted as categories.
			ty = strings.ToUpper(c.Type)
		}
		fts = append(fts, mlFeature{Name: c.Name, Type: ty})
		fidx = append(fidx, i)
	}
	all := make([]int, len(rows))
	for i := range all {
		all[i] = i
	}
	if fe := learnFeatures(fts, fidx, rows, all); fe != nil {
		return t, fe
	}
	t.Features, t.Rows = fts, len(rows)
	return t, nil
}

// mlTransform is ML.TRANSFORM of model md.
func (f front) mlTransform(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	if len(md.Transform) == 0 {
		return "", 400, "ML.TRANSFORM requires a model created with a TRANSFORM clause; model " + md.ID + " has none"
	}
	if c.input == "" {
		return "", 400, "ML.TRANSFORM requires an input: a query in parentheses or TABLE name"
	}
	if len(c.struc) > 0 {
		return "", 400, "ML.TRANSFORM takes no STRUCT of options"
	}
	cols, _, code, msg := f.queryAllRows(r, q, "SELECT * FROM ("+c.input+") LIMIT 0")
	if code != 0 {
		return "", code, msg
	}
	items := md.predictTransform()
	out := map[string]bool{}
	for _, it := range items {
		_, name := transformItemName(it)
		out[strings.ToLower(name)] = true
	}
	sel := append([]string(nil), items...)
	for _, col := range cols {
		if !out[strings.ToLower(col.Name)] {
			sel = append(sel, sqlIdent(col.Name))
		}
	}
	return "(SELECT " + strings.Join(sel, ", ") + " FROM (" + c.input + "))", 0, ""
}

// expandMLTransform writes each ML.TRANSFORM(MODEL ...) call of sql as
// its subquery, the last first, so a call in another's input is written
// before it.
func (f front) expandMLTransform(r *http.Request, q queryOptions, sql string) (string, int, string) {
	if !strings.Contains(strings.ToUpper(sql), "TRANSFORM") {
		return sql, 0, ""
	}
	for n := 0; n < 100; n++ {
		toks, ok := lex(sql)
		if !ok {
			return sql, 0, ""
		}
		at := -1
		for i := 0; i+4 < len(toks); i++ {
			if toks[i].is("ML") && toks[i+1].punct(".") && toks[i+2].is("TRANSFORM") && toks[i+3].punct("(") && toks[i+4].is("MODEL") &&
				(i == 0 || !toks[i-1].punct(".")) {
				at = toks[i].pos
			}
		}
		if at < 0 {
			return sql, 0, ""
		}
		calls, msg := findMLCalls(sql[at:])
		if msg != "" {
			return "", 400, msg
		}
		if len(calls) == 0 || calls[0].pos != 0 {
			return "", 400, "Syntax error in ML.TRANSFORM"
		}
		c := calls[0]
		project, dataset, id, ok := f.resolveModel(c.path, q)
		if !ok {
			return "", 400, "Model name " + strings.Join(c.path, ".") + " missing dataset while no default dataset is set in the request."
		}
		md, ok := f.models.get(project, dataset, id)
		if !ok {
			return "", 404, "Not found: Model " + project + ":" + dataset + "." + id
		}
		sub, code, why := f.mlTransform(r, q, md, c)
		if code != 0 {
			return "", code, why
		}
		sql = sql[:at] + sub + sql[at+c.end:]
	}
	return "", 400, "Too many ML.TRANSFORM calls"
}
