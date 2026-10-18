package bigqueryfront

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MATRIX_FACTORIZATION models with explicit feedback (#1504,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-matrix-factorization),
// trained by the front by alternating least squares (ALS), as BigQuery
// documents it trains them, with the documented options and defaults:
// feedback_type EXPLICIT, user_col 'user', item_col 'item', rating_col
// 'rating', num_factors, l2_reg 1.0, max_iterations 20, early_stop TRUE
// with min_rel_progress 0.01. Each user u and item i has num_factors
// weights (p_u, q_i) and an intercept (b_u, c_i), and a rating is predicted
// as p_u·q_i + b_u + c_i. Each iteration solves every user's weights and
// intercept exactly for the items' (a ridge regression of l2_reg on
// the user's ratings), then every item's for the users'.
//
// ML.RECOMMEND (predicted_<rating_col> with the user and item: of every
// user and item, or of an input's users, items or pairs), ML.EVALUATE (the
// regression metrics), ML.WEIGHTS (processed_input, feature,
// factor_weights, intercept) and ML.TRAINING_INFO serve it.
//
// UNVERIFIED against Google, chosen here: the weights start from a fixed
// seed (so a model is the same each run, but not Google's draw); the
// training ratings' mean is held out of the regularization and folded into
// the item intercepts; the loss is the mean squared error; num_factors'
// default is ⌊log2 n⌋ of the n training rows, kept in [2, 200]; a row with
// a NULL user, item or rating is dropped; a user or item not seen in
// training is predicted NULL; factors are numbered from 1; the column
// order of ML.RECOMMEND. Refused 501: IMPLICIT feedback (its weighting by
// wals_alpha and ranking metrics), and a data split.
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-matrix-factorization

// mfTrained is a matrix factorization model's weights.
type mfTrained struct {
	UserCol    string      `json:"userCol"`
	ItemCol    string      `json:"itemCol"`
	UserType   string      `json:"userType"`
	ItemType   string      `json:"itemType"`
	Users      []string    `json:"users"`
	Items      []string    `json:"items"`
	UserF      [][]float64 `json:"userFactors"`
	ItemF      [][]float64 `json:"itemFactors"`
	UserB      []float64   `json:"userBias"`
	ItemB      []float64   `json:"itemBias"`
	NumFactors int         `json:"numFactors"`
}

// applyMFDefaults sets a matrix factorization model's documented
// defaults that differ from the other types'.
func applyMFDefaults(o *mlOptions) {
	if o.FeedbackType == "" {
		o.FeedbackType = "EXPLICIT"
	}
	if o.UserCol == "" {
		o.UserCol = "user"
	}
	if o.ItemCol == "" {
		o.ItemCol = "item"
	}
	if o.RatingCol == "" {
		o.RatingCol = "rating"
	}
	if !o.given("l2_reg") {
		o.L2 = 1
	}
}

// trainMF trains a matrix factorization model of opts on rows.
func trainMF(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	if opts.FeedbackType != "EXPLICIT" {
		return t, mlUnimplemented("feedback_type %s: implicit feedback's confidence weighting (wals_alpha) and ranking metrics are not carried out; "+
			"CloudBurrow trains EXPLICIT matrix factorization", opts.FeedbackType)
	}
	find := func(name string) int {
		for i, c := range cols {
			if strings.EqualFold(c.Name, name) {
				return i
			}
		}
		return -1
	}
	ui, ii, ri := find(opts.UserCol), find(opts.ItemCol), find(opts.RatingCol)
	for _, p := range []struct {
		idx  int
		name string
	}{{ui, opts.UserCol}, {ii, opts.ItemCol}, {ri, opts.RatingCol}} {
		if p.idx < 0 {
			return t, mlInvalid("Unable to identify the column '%s' in the input data", p.name)
		}
	}
	if len(cols) != 3 {
		return t, mlInvalid("A MATRIX_FACTORIZATION model takes exactly the user, item and rating columns; the input data has %d columns", len(cols))
	}
	ut, it := mlTypeOf(cols[ui].Type), mlTypeOf(cols[ii].Type)
	for _, ty := range []string{ut, it} {
		if ty != "INT64" && ty != "STRING" {
			return t, mlInvalid("The user and item columns of a MATRIX_FACTORIZATION model must be INT64 or STRING")
		}
	}
	switch mlTypeOf(cols[ri].Type) {
	case "INT64", "FLOAT64", "NUMERIC", "BIGNUMERIC":
	default:
		return t, mlInvalid("The rating column %s must be numeric", cols[ri].Name)
	}
	type obs struct {
		u, i int
		r    float64
	}
	uIdx, iIdx := map[string]int{}, map[string]int{}
	var users, items []string
	count := 0
	for _, row := range rows {
		if row[ui] == nil || row[ii] == nil || row[ri] == nil {
			continue
		}
		if _, err := strconv.ParseFloat(*row[ri], 64); err != nil {
			continue
		}
		if _, ok := uIdx[*row[ui]]; !ok {
			uIdx[*row[ui]] = 0
			users = append(users, *row[ui])
		}
		if _, ok := iIdx[*row[ii]]; !ok {
			iIdx[*row[ii]] = 0
			items = append(items, *row[ii])
		}
		count++
	}
	if count == 0 {
		return t, mlInvalid("Input data doesn't contain any rows")
	}
	if opts.SplitMethod != "NO_SPLIT" && !(opts.SplitMethod == "AUTO_SPLIT" && count < 500) {
		return t, mlUnimplemented("data_split_method %s of a MATRIX_FACTORIZATION model of %d rows: CloudBurrow trains it on every row "+
			"(NO_SPLIT, or AUTO_SPLIT under 500 rows)", opts.SplitMethod, count)
	}
	sortLabels(users, ut)
	sortLabels(items, it)
	for k, u := range users {
		uIdx[u] = k
	}
	for k, v := range items {
		iIdx[v] = k
	}
	data := make([]obs, count)
	n := 0
	var mean float64
	for _, row := range rows {
		if row[ui] == nil || row[ii] == nil || row[ri] == nil {
			continue
		}
		r, err := strconv.ParseFloat(*row[ri], 64)
		if err != nil {
			continue
		}
		data[n] = obs{u: uIdx[*row[ui]], i: iIdx[*row[ii]], r: r}
		mean += r
		n++
	}
	data = data[:n]
	mean /= float64(n)
	k := opts.NumFactors
	if k == 0 {
		k = int(math.Floor(math.Log2(float64(n))))
		k = max(2, min(k, 200))
	}
	rng := rand.New(rand.NewSource(1504))
	initF := func(m int) [][]float64 {
		out := make([][]float64, m)
		for a := range out {
			out[a] = make([]float64, k)
			for j := range out[a] {
				out[a][j] = (rng.Float64() - 0.5) / math.Sqrt(float64(k))
			}
		}
		return out
	}
	P, Q := initF(len(users)), initF(len(items))
	bu, ci := make([]float64, len(users)), make([]float64, len(items))
	byUser, byItem := make([][]int, len(users)), make([][]int, len(items))
	for x, o := range data {
		byUser[o.u] = append(byUser[o.u], x)
		byItem[o.i] = append(byItem[o.i], x)
	}
	loss := func() float64 {
		var s float64
		for _, o := range data {
			d := o.r - (mean + dot(P[o.u], Q[o.i]) + bu[o.u] + ci[o.i])
			s += d * d
		}
		return s / float64(len(data))
	}
	// solve fits one side's weights and intercept for the other's fixed:
	// a ridge regression of l2_reg on [factors, 1].
	solve := func(idx []int, other func(o obs) ([]float64, float64), target func(o obs) float64) ([]float64, float64) {
		d := k + 1
		A := make([][]float64, d)
		bv := make([]float64, d)
		for a := range A {
			A[a] = make([]float64, d)
			A[a][a] = opts.L2
		}
		for _, x := range idx {
			o := data[x]
			f, ob := other(o)
			z := append(append([]float64(nil), f...), 1)
			tg := target(o) - ob
			for a := 0; a < d; a++ {
				bv[a] += z[a] * tg
				for b := 0; b < d; b++ {
					A[a][b] += z[a] * z[b]
				}
			}
		}
		w := solveLinear(A, bv)
		return w[:k], w[k]
	}
	prev := loss()
	for iter := 0; iter < opts.MaxIterations; iter++ {
		start := time.Now()
		for u := range users {
			P[u], bu[u] = solve(byUser[u], func(o obs) ([]float64, float64) { return Q[o.i], ci[o.i] }, func(o obs) float64 { return o.r - mean })
		}
		for i := range items {
			Q[i], ci[i] = solve(byItem[i], func(o obs) ([]float64, float64) { return P[o.u], bu[o.u] }, func(o obs) float64 { return o.r - mean })
		}
		l := loss()
		t.Iterations = append(t.Iterations, mlIteration{Index: iter, Loss: l, DurationMs: time.Since(start).Milliseconds()})
		if opts.EarlyStop && prev > 0 && (prev-l)/prev < opts.MinRelProgress {
			break
		}
		if l == 0 {
			break
		}
		prev = l
	}
	// Newest first, as the other models keep them.
	for a, b := 0, len(t.Iterations)-1; a < b; a, b = a+1, b-1 {
		t.Iterations[a], t.Iterations[b] = t.Iterations[b], t.Iterations[a]
	}
	for i := range ci {
		ci[i] += mean
	}
	t.MF = &mfTrained{UserCol: cols[ui].Name, ItemCol: cols[ii].Name, UserType: ut, ItemType: it, Users: users, Items: items,
		UserF: P, ItemF: Q, UserB: bu, ItemB: ci, NumFactors: k}
	t.Label, t.LabelType = cols[ri].Name, "FLOAT64"
	t.Rows, t.EvalRows = n, n
	t.Features = []mlFeature{{Name: cols[ui].Name, Type: ut}, {Name: cols[ii].Name, Type: it}}
	y, p := make([]float64, n), make([]float64, n)
	for x, o := range data {
		y[x] = o.r
		p[x] = t.MF.predict(o.u, o.i)
	}
	t.Eval = regressionMetrics(y, p)
	return t, nil
}

func (m *mfTrained) predict(u, i int) float64 {
	return dot(m.UserF[u], m.ItemF[i]) + m.UserB[u] + m.ItemB[i]
}

// solveLinear solves A x = b by Gaussian elimination with partial
// pivoting; a singular system's free unknowns are 0.
func solveLinear(A [][]float64, b []float64) []float64 {
	n := len(b)
	M := make([][]float64, n)
	for i := range M {
		M[i] = append(append([]float64(nil), A[i]...), b[i])
	}
	for c := 0; c < n; c++ {
		p := c
		for r := c + 1; r < n; r++ {
			if math.Abs(M[r][c]) > math.Abs(M[p][c]) {
				p = r
			}
		}
		M[c], M[p] = M[p], M[c]
		if math.Abs(M[c][c]) < 1e-12 {
			continue
		}
		for r := 0; r < n; r++ {
			if r == c || M[r][c] == 0 {
				continue
			}
			f := M[r][c] / M[c][c]
			for j := c; j <= n; j++ {
				M[r][j] -= f * M[c][j]
			}
		}
	}
	x := make([]float64, n)
	for i := range x {
		if math.Abs(M[i][i]) >= 1e-12 {
			x[i] = M[i][n] / M[i][i]
		}
	}
	return x
}

// mfTables are the SQL of the users' and items' weights, as rows.
func (m *mfTrained) mfTables() (users, items string) {
	arr := func(f []float64) string {
		var s []string
		for _, v := range f {
			s = append(s, sqlF(v))
		}
		return "[" + strings.Join(s, ", ") + "]"
	}
	var ur, ir []string
	for a, u := range m.Users {
		ur = append(ur, fmt.Sprintf("(%s, %s, %s)", sqlLabel(u, m.UserType), arr(m.UserF[a]), sqlF(m.UserB[a])))
	}
	for a, v := range m.Items {
		ir = append(ir, fmt.Sprintf("(%s, %s, %s)", sqlLabel(v, m.ItemType), arr(m.ItemF[a]), sqlF(m.ItemB[a])))
	}
	return "UNNEST(ARRAY<STRUCT<_cbml_u " + m.UserType + ", _cbml_uf ARRAY<FLOAT64>, _cbml_ub FLOAT64>>[" + strings.Join(ur, ", ") + "])",
		"UNNEST(ARRAY<STRUCT<_cbml_i " + m.ItemType + ", _cbml_if ARRAY<FLOAT64>, _cbml_ib FLOAT64>>[" + strings.Join(ir, ", ") + "])"
}

// mfPredictSQL is a rating's SQL over the joined weights.
const mfPredictSQL = "(_cbml_ub + _cbml_ib + (SELECT SUM(_cbml_a * _cbml_if[OFFSET(_cbml_o)]) FROM UNNEST(_cbml_uf) AS _cbml_a WITH OFFSET _cbml_o))"

// mfSubquery is the subquery that stands for call c of a matrix
// factorization model.
func (f front) mfSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	t := md.Trained
	m := t.MF
	if m == nil {
		return "", 500, "cloudburrow: the model has no weights"
	}
	out := sqlIdent("predicted_" + t.Label)
	uc, ic := sqlIdent(m.UserCol), sqlIdent(m.ItemCol)
	switch c.fn {
	case "RECOMMEND":
		for k := range c.struc {
			if k != "trial_id" {
				return "", 400, "Unknown option " + k + " of ML.RECOMMEND"
			}
		}
		users, items := m.mfTables()
		if c.input == "" {
			return "(SELECT " + mfPredictSQL + " AS " + out + ", _cbml_u AS " + uc + ", _cbml_i AS " + ic + " FROM " + users + " CROSS JOIN " + items + ")", 0, ""
		}
		cols, _, code, msg := f.queryAllRows(r, q, "SELECT * FROM ("+c.input+") LIMIT 0")
		if code != 0 {
			return "", code, msg
		}
		hasU, hasI := false, false
		for _, col := range cols {
			hasU = hasU || strings.EqualFold(col.Name, m.UserCol)
			hasI = hasI || strings.EqualFold(col.Name, m.ItemCol)
		}
		in := "(" + c.input + ") AS _cbml_in"
		switch {
		case hasU && hasI:
			return "(SELECT " + mfPredictSQL + " AS " + out + ", _cbml_in.* FROM " + in + " LEFT JOIN " + users + " ON _cbml_in." + uc + " = _cbml_u LEFT JOIN " +
				items + " ON _cbml_in." + ic + " = _cbml_i)", 0, ""
		case hasU:
			return "(SELECT " + mfPredictSQL + " AS " + out + ", _cbml_in.*, _cbml_i AS " + ic + " FROM " + in + " JOIN " + users + " ON _cbml_in." + uc +
				" = _cbml_u CROSS JOIN " + items + ")", 0, ""
		case hasI:
			return "(SELECT " + mfPredictSQL + " AS " + out + ", _cbml_u AS " + uc + ", _cbml_in.* FROM " + in + " JOIN " + items + " ON _cbml_in." + ic +
				" = _cbml_i CROSS JOIN " + users + ")", 0, ""
		}
		return "", 400, "The input of ML.RECOMMEND must have the user column " + m.UserCol + " or the item column " + m.ItemCol
	case "EVALUATE":
		metrics := t.Eval
		if c.input != "" {
			cols, rows, code, msg := f.queryAllRows(r, q, c.input)
			if code != 0 {
				return "", code, msg
			}
			ui, ii, ri := -1, -1, -1
			for k, col := range cols {
				switch {
				case strings.EqualFold(col.Name, m.UserCol):
					ui = k
				case strings.EqualFold(col.Name, m.ItemCol):
					ii = k
				case strings.EqualFold(col.Name, t.Label):
					ri = k
				}
			}
			if ui < 0 || ii < 0 || ri < 0 {
				return "", 400, "The input of ML.EVALUATE must have the columns " + m.UserCol + ", " + m.ItemCol + " and " + t.Label
			}
			uIdx, iIdx := map[string]int{}, map[string]int{}
			for k, u := range m.Users {
				uIdx[u] = k
			}
			for k, v := range m.Items {
				iIdx[v] = k
			}
			var y, p []float64
			for _, row := range rows {
				if row[ui] == nil || row[ii] == nil || row[ri] == nil {
					continue
				}
				u, ok1 := uIdx[*row[ui]]
				i, ok2 := iIdx[*row[ii]]
				rv, err := strconv.ParseFloat(*row[ri], 64)
				if !ok1 || !ok2 || err != nil {
					continue
				}
				y, p = append(y, rv), append(p, m.predict(u, i))
			}
			metrics = map[string]*float64{}
			if len(y) > 0 {
				metrics = regressionMetrics(y, p)
			}
		}
		var cols, vals []string
		for _, n := range regressionMetricNames {
			cols = append(cols, n+" FLOAT64")
			vals = append(vals, sqlFloat(metrics[n]))
		}
		return mlRowsSubquery("STRUCT<"+strings.Join(cols, ", ")+">", []string{"STRUCT(" + strings.Join(vals, ", ") + ")"}), 0, ""
	case "WEIGHTS":
		fw := func(f []float64) string {
			var s []string
			for j, v := range f {
				s = append(s, fmt.Sprintf("(%d, %s)", j+1, sqlF(v)))
			}
			return "ARRAY<STRUCT<factor INT64, weight FLOAT64>>[" + strings.Join(s, ", ") + "]"
		}
		var rows []string
		for a, u := range m.Users {
			rows = append(rows, fmt.Sprintf("(%s, %s, %s, %s)", sqlString(m.UserCol), sqlString(u), fw(m.UserF[a]), sqlF(m.UserB[a])))
		}
		for a, v := range m.Items {
			rows = append(rows, fmt.Sprintf("(%s, %s, %s, %s)", sqlString(m.ItemCol), sqlString(v), fw(m.ItemF[a]), sqlF(m.ItemB[a])))
		}
		return mlRowsSubquery("STRUCT<processed_input STRING, feature STRING, factor_weights ARRAY<STRUCT<factor INT64, weight FLOAT64>>, intercept FLOAT64>", rows), 0, ""
	case "TRAINING_INFO":
		var rows []string
		for _, it := range t.Iterations {
			rows = append(rows, fmt.Sprintf("(0, %d, %s, NULL, NULL, %d)", it.Index, sqlF(it.Loss), it.DurationMs))
		}
		return mlRowsSubquery("STRUCT<training_run INT64, iteration INT64, loss FLOAT64, eval_loss FLOAT64, learning_rate FLOAT64, duration_ms INT64>", rows), 0, ""
	}
	return "", 400, "ML." + c.fn + " is not supported for model type MATRIX_FACTORIZATION"
}

// mfResource sets a matrix factorization model's REST fields.
func (md bqModel) mfResource(res map[string]any) (opts, metrics map[string]any) {
	t, o := md.Trained, md.Options
	res["labelColumns"] = []map[string]any{{"name": "predicted_" + t.Label, "type": map[string]string{"typeKind": "FLOAT64"}}}
	k := 0
	if t.MF != nil {
		k = t.MF.NumFactors
	}
	opts = map[string]any{"numFactors": strconv.Itoa(k), "feedbackType": o.FeedbackType, "userColumn": o.UserCol, "itemColumn": o.ItemCol,
		"l2Regularization": o.L2, "maxIterations": strconv.Itoa(o.MaxIterations), "earlyStop": o.EarlyStop,
		"minRelativeProgress": o.MinRelProgress, "dataSplitMethod": o.SplitMethod}
	num := func(k string) any {
		if v := t.Eval[k]; v != nil {
			return *v
		}
		return nil
	}
	metrics = map[string]any{"regressionMetrics": map[string]any{"meanAbsoluteError": num("mean_absolute_error"),
		"meanSquaredError": num("mean_squared_error"), "meanSquaredLogError": num("mean_squared_log_error"),
		"medianAbsoluteError": num("median_absolute_error"), "rSquared": num("r2_score")}}
	return opts, metrics
}
