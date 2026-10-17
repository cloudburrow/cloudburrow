package bigqueryfront

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ML.EXPLAIN_PREDICT and ML.GLOBAL_EXPLAIN (#1504,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-explain-predict,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-global-explain)
// for LINEAR_REG, LOGISTIC_REG, BOOSTED_TREE_* and RANDOM_FOREST_* models,
// with exact attributions, as BigQuery documents for these types:
//
//   - a linear model's are its exact Shapley values, each feature's weight
//     times its encoded value (a numeric feature's standardized value, a
//     categorical one's one-hot columns), against the baseline of the
//     intercept, as documented;
//   - a tree model's are TreeSHAP's (path-dependent, Lundberg et al. 2018,
//     as XGBoost computes them from each node's cover), against the
//     baseline of the trees' expected value over the training rows (the
//     starting score plus each tree's cover-weighted leaf mean).
//
// A classifier's are toward the predicted class (prediction_value its
// logit: the binary log-odds, negated for the negative class; a multiclass
// model's class score). A row's top_k_features (default 5) are ranked by
// absolute attribution, and approximation_error is |prediction_value −
// baseline − Σ attributions|, computed. ML.GLOBAL_EXPLAIN, for a model
// trained with enable_global_explain, gives each feature's mean absolute
// attribution over the evaluation rows (the training rows with no split),
// computed at training; with class_level_explain, each class's top 10
// toward that class.
//
// UNVERIFIED against Google, chosen here: a multiclass model's attribution
// toward its class score; class_level_explain over every row, its class
// column named after the label; the order of ML.GLOBAL_EXPLAIN's rows (by
// attribution, the largest first) and of ties (by feature name). Refused
// 501: a model with a TRANSFORM clause, and an input column of a type
// CloudBurrow cannot write back (ARRAY, STRUCT, GEOGRAPHY, JSON, RANGE).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-global-explain

// mlGlobal is one row of ML.GLOBAL_EXPLAIN.
type mlGlobal struct {
	Class       string  `json:"class,omitempty"`
	Feature     string  `json:"feature"`
	Attribution float64 `json:"attribution"`
}

// treeExpected is a tree's cover-weighted mean leaf value.
func treeExpected(tree []mlNode) float64 {
	var walk func(j int) float64
	walk = func(j int) float64 {
		nd := tree[j]
		if nd.Feat < 0 || nd.Cover == 0 {
			return nd.Value
		}
		return (tree[nd.Left].Cover*walk(nd.Left) + tree[nd.Right].Cover*walk(nd.Right)) / nd.Cover
	}
	return walk(0)
}

type shapPath struct {
	feat    int
	zero    float64
	one     float64
	pweight float64
}

// treeShap adds tree's TreeSHAP values for x (encoded, the intercept's 1
// first) to phi, one per encoded column.
func treeShap(tree []mlNode, x []float64, phi []float64) {
	extend := func(p []shapPath, pz, po float64, pi int) []shapPath {
		l := len(p)
		p = append(p, shapPath{pi, pz, po, 0})
		if l == 0 {
			p[0].pweight = 1
		}
		for i := l - 1; i >= 0; i-- {
			p[i+1].pweight += po * p[i].pweight * float64(i+1) / float64(l+1)
			p[i].pweight = pz * p[i].pweight * float64(l-i) / float64(l+1)
		}
		return p
	}
	unwind := func(p []shapPath, idx int) []shapPath {
		d := len(p) - 1
		one, zero := p[idx].one, p[idx].zero
		next := p[d].pweight
		for i := d - 1; i >= 0; i-- {
			if one != 0 {
				tmp := p[i].pweight
				p[i].pweight = next * float64(d+1) / (float64(i+1) * one)
				next = tmp - p[i].pweight*zero*float64(d-i)/float64(d+1)
			} else {
				p[i].pweight = p[i].pweight * float64(d+1) / (zero * float64(d-i))
			}
		}
		for i := idx; i < d; i++ {
			p[i].feat, p[i].zero, p[i].one = p[i+1].feat, p[i+1].zero, p[i+1].one
		}
		return p[:d]
	}
	unwoundSum := func(p []shapPath, idx int) float64 {
		d := len(p) - 1
		one, zero := p[idx].one, p[idx].zero
		next := p[d].pweight
		var total float64
		for i := d - 1; i >= 0; i-- {
			if one != 0 {
				tmp := next * float64(d+1) / (float64(i+1) * one)
				total += tmp
				next = p[i].pweight - tmp*zero*float64(d-i)/float64(d+1)
			} else if zero != 0 {
				total += p[i].pweight / zero / (float64(d-i) / float64(d+1))
			}
		}
		return total
	}
	var rec func(j int, parent []shapPath, pz, po float64, pi int)
	rec = func(j int, parent []shapPath, pz, po float64, pi int) {
		p := extend(append([]shapPath(nil), parent...), pz, po, pi)
		nd := tree[j]
		if nd.Feat < 0 {
			for i := 1; i < len(p); i++ {
				phi[p[i].feat] += unwoundSum(p, i) * (p[i].one - p[i].zero) * nd.Value
			}
			return
		}
		hot, cold := nd.Right, nd.Left
		if x[nd.Feat+1] < nd.Thr {
			hot, cold = nd.Left, nd.Right
		}
		iz, io := 1.0, 1.0
		for k := 1; k < len(p); k++ {
			if p[k].feat == nd.Feat {
				iz, io = p[k].zero, p[k].one
				p = unwind(p, k)
				break
			}
		}
		rec(hot, p, iz*tree[hot].Cover/nd.Cover, io, nd.Feat)
		rec(cold, p, iz*tree[cold].Cover/nd.Cover, 0, nd.Feat)
	}
	rec(0, nil, 1, 1, -1)
}

// explainScore is the baseline and each feature's attribution of score k
// (a regressor's value or binary model's log-odds: 0; a multiclass
// model's class) for x, encoded with the intercept's 1 first.
func (t mlTrained) explainScore(x []float64, k int) (float64, []float64) {
	var owner []int
	for f, ft := range t.Features {
		for j := 0; j < ft.dims(); j++ {
			owner = append(owner, f)
		}
	}
	attr := make([]float64, len(t.Features))
	if len(t.Trees) > 0 {
		base := t.BaseScore
		phi := make([]float64, len(x)-1)
		for j, tr := range t.Trees {
			if j < len(t.TreeClass) && t.TreeClass[j] != k {
				continue
			}
			base += treeExpected(tr)
			treeShap(tr, x, phi)
		}
		for j, v := range phi {
			attr[owner[j]] += v
		}
		return base, attr
	}
	w := t.weightVector()
	if t.multiclass() {
		w = t.ClassWeights[k]
	}
	for j := 1; j < len(x); j++ {
		attr[owner[j-1]] += w[j] * x[j]
	}
	return w[0], attr
}

// explainRow is a row's prediction and its explanation toward the
// predicted class: the class (or value), its probability, its logit, the
// baseline and attributions.
func (t mlTrained) explainRow(x []float64, threshold float64) (pred int, value, prob, logit, base float64, attr []float64) {
	switch {
	case t.multiclass():
		var z []float64
		if len(t.Trees) > 0 {
			z = t.treeMargins(x, len(t.Classes))
		} else {
			for _, w := range t.ClassWeights {
				z = append(z, dot(w, x))
			}
		}
		p := softmaxOf(z)
		pred = argmax(p)
		base, attr = t.explainScore(x, pred)
		return pred, 0, p[pred], z[pred], base, attr
	case len(t.Classes) == 2:
		base, attr = t.explainScore(x, 0)
		var z float64
		if len(t.Trees) > 0 {
			z = t.treeMargin(x)
		} else {
			z = dot(t.weightVector(), x)
		}
		p := sigmoid(z)
		if p >= threshold {
			return 1, 0, p, z, base, attr
		}
		for i := range attr {
			attr[i] = -attr[i]
		}
		return 0, 0, 1 - p, -z, -base, attr
	}
	base, attr = t.explainScore(x, 0)
	if len(t.Trees) > 0 {
		value = t.treeMargin(x)
	} else {
		value = dot(t.weightVector(), x)
	}
	return 0, value, 0, value, base, attr
}

// computeGlobal computes ML.GLOBAL_EXPLAIN's rows over the encoded rows
// X, when the model was trained with enable_global_explain.
func (t *mlTrained) computeGlobal(o mlOptions, X [][]float64) {
	if !o.GlobalExplain || len(X) == 0 {
		return
	}
	mean := make([]float64, len(t.Features))
	K := len(t.Classes)
	perClass := make([][]float64, K)
	for k := range perClass {
		perClass[k] = make([]float64, len(t.Features))
	}
	for _, x := range X {
		_, _, _, _, _, attr := t.explainRow(x, 0.5)
		for f, a := range attr {
			mean[f] += math.Abs(a) / float64(len(X))
		}
		for k := 0; k < K; k++ {
			idx := k
			if !t.multiclass() {
				idx = 0
			}
			_, a := t.explainScore(x, idx)
			for f, v := range a {
				perClass[k][f] += math.Abs(v) / float64(len(X))
			}
		}
	}
	rank := func(class string, vals []float64, top int) {
		var rows []mlGlobal
		for f, ft := range t.Features {
			rows = append(rows, mlGlobal{Class: class, Feature: ft.Name, Attribution: vals[f]})
		}
		sort.SliceStable(rows, func(a, b int) bool {
			if rows[a].Attribution != rows[b].Attribution {
				return rows[a].Attribution > rows[b].Attribution
			}
			return rows[a].Feature < rows[b].Feature
		})
		if top > 0 && len(rows) > top {
			rows = rows[:top]
		}
		t.Global = append(t.Global, rows...)
	}
	rank("", mean, 0)
	for k, c := range t.Classes {
		rank(c, perClass[k], 10)
	}
}

// sqlValueOf is a REST value of BigQuery type typ as a SQL literal, or
// false for a type CloudBurrow cannot write back.
func sqlValueOf(v *string, typ string) (string, bool) {
	typ = strings.ToUpper(typ)
	if v == nil {
		switch typ {
		case "INTEGER", "INT64", "FLOAT", "FLOAT64", "BOOLEAN", "BOOL", "STRING", "NUMERIC", "BIGNUMERIC", "DATE", "DATETIME", "TIME", "TIMESTAMP", "BYTES":
			return "CAST(NULL AS " + sqlTypeName(typ) + ")", true
		}
		return "", false
	}
	switch typ {
	case "INTEGER", "INT64":
		return "CAST(" + sqlString(*v) + " AS INT64)", true
	case "FLOAT", "FLOAT64":
		return "CAST(" + sqlString(*v) + " AS FLOAT64)", true
	case "BOOLEAN", "BOOL":
		return "CAST(" + sqlString(*v) + " AS BOOL)", true
	case "STRING":
		return sqlString(*v), true
	case "NUMERIC", "BIGNUMERIC", "DATE", "DATETIME", "TIME":
		return "CAST(" + sqlString(*v) + " AS " + typ + ")", true
	case "TIMESTAMP":
		f, err := strconv.ParseFloat(*v, 64)
		if err != nil {
			return "", false
		}
		return fmt.Sprintf("TIMESTAMP_MICROS(%d)", int64(math.Round(f*1e6))), true
	case "BYTES":
		return "FROM_BASE64(" + sqlString(*v) + ")", true
	}
	return "", false
}

// sqlTypeName is a REST type's SQL name.
func sqlTypeName(typ string) string {
	switch typ = strings.ToUpper(typ); typ {
	case "INTEGER":
		return "INT64"
	case "FLOAT":
		return "FLOAT64"
	case "BOOLEAN":
		return "BOOL"
	}
	return typ
}

// explainSubquery is ML.EXPLAIN_PREDICT's or ML.GLOBAL_EXPLAIN's
// subquery for model md.
func (f front) explainSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	t := md.Trained
	typ := md.Options.ModelType
	if isDNN(typ) {
		return "", 501, "Not implemented here: ML." + c.fn + " of a " + typ + " model: its integrated gradients attributions " +
			"(integrated_gradients_num_steps) are not carried out. ML.PREDICT, ML.EVALUATE and ML.TRAINING_INFO serve it. Nothing was run."
	}
	if typ != "LINEAR_REG" && typ != "LOGISTIC_REG" && !isTreeModel(typ) {
		return "", 400, "ML." + c.fn + " is not supported for model type " + typ
	}
	if c.fn == "GLOBAL_EXPLAIN" {
		if !md.Options.GlobalExplain {
			return "", 400, "ML.GLOBAL_EXPLAIN requires a model trained with enable_global_explain = TRUE"
		}
		classLevel := strings.EqualFold(c.struc["class_level_explain"], "TRUE")
		if classLevel && len(t.Classes) == 0 {
			return "", 400, "class_level_explain is only supported for classification models"
		}
		var rows []string
		for _, g := range t.Global {
			if (g.Class != "") != classLevel {
				continue
			}
			if classLevel {
				rows = append(rows, fmt.Sprintf("(%s, %s, %s)", sqlLabel(g.Class, t.LabelType), sqlString(g.Feature), sqlF(g.Attribution)))
			} else {
				rows = append(rows, fmt.Sprintf("(%s, %s)", sqlString(g.Feature), sqlF(g.Attribution)))
			}
		}
		if classLevel {
			return mlRowsSubquery("STRUCT<"+sqlIdent(t.Label)+" "+t.LabelType+", feature STRING, attribution FLOAT64>", rows), 0, ""
		}
		return mlRowsSubquery("STRUCT<feature STRING, attribution FLOAT64>", rows), 0, ""
	}
	if c.input == "" {
		return "", 400, "ML.EXPLAIN_PREDICT requires an input: a query in parentheses or TABLE name"
	}
	if len(md.Transform) > 0 {
		return "", 501, "Not implemented here: ML.EXPLAIN_PREDICT of a model with a TRANSFORM clause; CloudBurrow explains a model trained on its input's columns. Nothing was run."
	}
	topK := 5
	if v, ok := c.struc["top_k_features"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", 400, "Invalid top_k_features " + v + ": it must be a positive integer"
		}
		topK = n
	}
	thr, why := c.threshold()
	if why != "" {
		return "", 400, why
	}
	binary := len(t.Classes) == 2
	if _, ok := c.struc["threshold"]; ok && !binary {
		return "", 400, "The threshold option is only supported for binary classification models"
	}
	if strings.EqualFold(c.struc["approx_feature_contrib"], "TRUE") {
		return "", 501, "Not implemented here: approx_feature_contrib TRUE (XGBoost's approximation, Saabas); CloudBurrow computes exact TreeSHAP attributions. Nothing was run."
	}
	cols, rows, code, msg := f.queryAllRows(r, q, c.input)
	if code != 0 {
		return "", code, msg
	}
	classifier := len(t.Classes) > 0
	head := []string{sqlIdent("predicted_"+t.Label) + " " + map[bool]string{true: t.LabelType, false: "FLOAT64"}[classifier]}
	if classifier {
		head = append(head, "probability FLOAT64")
	}
	head = append(head, "top_feature_attributions ARRAY<STRUCT<feature STRING, attribution FLOAT64>>", "baseline_prediction_value FLOAT64",
		"prediction_value FLOAT64", "approximation_error FLOAT64")
	for _, col := range cols {
		ty := sqlTypeName(col.Type)
		if strings.EqualFold(col.Name, "predicted_"+t.Label) || strings.EqualFold(col.Name, "probability") {
			continue
		}
		head = append(head, sqlIdent(col.Name)+" "+ty)
	}
	var out []string
	for _, row := range rows {
		vals := map[string]*string{}
		for i, col := range cols {
			vals[strings.ToLower(col.Name)] = row[i]
		}
		x := t.encodeRow(vals)
		pred, value, prob, logit, base, attr := t.explainRow(x, thr)
		sum := base
		for _, a := range attr {
			sum += a
		}
		order := make([]int, len(attr))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return math.Abs(attr[order[a]]) > math.Abs(attr[order[b]]) })
		var items []string
		for _, i := range order[:min(topK, len(order))] {
			items = append(items, fmt.Sprintf("(%s, %s)", sqlString(t.Features[i].Name), sqlF(attr[i])))
		}
		fields := []string{sqlF(value)}
		if classifier {
			fields = []string{sqlLabel(t.Classes[pred], t.LabelType), sqlF(prob)}
		}
		fields = append(fields, "["+strings.Join(items, ", ")+"]", sqlF(base), sqlF(logit), sqlF(math.Abs(logit-sum)))
		for i, col := range cols {
			if strings.EqualFold(col.Name, "predicted_"+t.Label) || strings.EqualFold(col.Name, "probability") {
				continue
			}
			lit, ok := sqlValueOf(row[i], col.Type)
			if !ok {
				return "", 501, "Not implemented here: ML.EXPLAIN_PREDICT of an input column (" + col.Name + ") of type " + col.Type +
					"; CloudBurrow passes through columns of scalar types. Nothing was run."
			}
			fields = append(fields, lit)
		}
		out = append(out, "("+strings.Join(fields, ", ")+")")
	}
	return mlRowsSubquery("STRUCT<"+strings.Join(head, ", ")+">", out), 0, ""
}
