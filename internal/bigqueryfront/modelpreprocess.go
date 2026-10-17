package bigqueryfront

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The BigQuery ML functions that take no MODEL (#1504,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-preprocessing-functions):
// the engine knows none of them, so the front writes each call into the
// query as the SQL that computes it, before the engine runs it, in a
// query of its own and in a TRANSFORM clause (modeltransform.go).
//
// Row-local, written as SQL of the call's own arguments:
//
//   - ML.BUCKETIZE(x, split_points [, exclude_boundaries]): 'bin_<n>',
//     n = 1 + the number of split points <= x (bin_1 below the first);
//     exclude_boundaries drops the first and the last split point;
//   - ML.POLYNOMIAL_EXPAND(STRUCT(...) [, degree]): a STRUCT of every
//     product of up to degree (2) fields, named by joining the fields'
//     names with _, as FLOAT64;
//   - ML.FEATURE_CROSS(STRUCT(...) [, degree]): a STRUCT of every
//     combination of 2 to degree (2) of the STRING fields, the values
//     joined with _;
//   - ML.DISTANCE(a, b [, 'EUCLIDEAN' | 'MANHATTAN' | 'COSINE']),
//     ML.LP_NORM(a, p) and ML.NORMALIZER(a [, p]) (a / its p-norm, p 2 by
//     default) of numeric arrays.
//
// Analytic, x's statistics over the window (OVER (...)) in a query, and in
// a TRANSFORM over the training rows, learned at CREATE MODEL and kept as
// constants in the model's TRANSFORM so prediction applies the same ones:
//
//   - ML.STANDARD_SCALER(x): (x - mean) / sample standard deviation;
//   - ML.MIN_MAX_SCALER(x): (x - min) / (max - min), capped to [0, 1];
//   - ML.MAX_ABS_SCALER(x): x / max |x|;
//   - ML.IMPUTER(x, 'mean'): x, or the mean where it is NULL.
//
// UNVERIFIED: what BigQuery gives where a scaler divides by zero (a
// constant column, or ML.NORMALIZER of a zero array); CloudBurrow gives
// NULL. ML.BUCKETIZE of NULL is NULL.
// The order of ML.POLYNOMIAL_EXPAND's fields is the input fields, then the
// products degree by degree.
//
// Refused 501 with the reason: the other preprocessing functions (their
// quantiles are approximate, their vocabularies or hashing Google's), the
// output_format argument of ML.BUCKETIZE, and IMPUTER strategies other than
// mean. Any other ML.<name>(...) of no MODEL is left to the engine.

// mlPreprocessUnserved are the ML functions of no model CloudBurrow
// refuses, and why.
var mlPreprocessUnserved = map[string]string{
	"QUANTILE_BUCKETIZE": "its quantiles are BigQuery's approximate ones",
	"ROBUST_SCALER":      "its quantiles are BigQuery's approximate ones",
	"ONE_HOT_ENCODER":    "its vocabulary (top_k, frequency_threshold) is learned over the rows, which CloudBurrow does not carry out yet",
	"MULTI_HOT_ENCODER":  "its vocabulary (top_k, frequency_threshold) is learned over the rows, which CloudBurrow does not carry out yet",
	"LABEL_ENCODER":      "its vocabulary (top_k, frequency_threshold) is learned over the rows, which CloudBurrow does not carry out yet",
	"HASH_BUCKETIZE":     "its hashing is BigQuery's",
	"NGRAMS":             "CloudBurrow does not carry it out yet",
	"BAG_OF_WORDS":       "CloudBurrow does not carry it out yet",
	"TF_IDF":             "CloudBurrow does not carry it out yet",
}

// mlStats are a column's statistics a TRANSFORM learned.
type mlStats struct {
	mean, sd, min, max, maxAbs *float64
}

// mlLearn computes the statistics of an expression over the training rows.
type mlLearn func(expr string) (mlStats, int, string)

// rewritePreprocess writes each ML function call of no model in sql as
// the SQL that computes it. With learn, analytic calls (which must then
// be OVER ()) use the statistics it learns, as constants.
func rewritePreprocess(sql string, learn mlLearn) (string, int, string) {
	if !strings.Contains(strings.ToUpper(sql), "ML") {
		return sql, 0, ""
	}
	toks, ok := lex(sql)
	if !ok {
		return sql, 0, ""
	}
	var b strings.Builder
	last := 0
	for i := 0; i+3 < len(toks); i++ {
		if !toks[i].is("ML") || !toks[i+1].punct(".") || toks[i+2].kind != tokWord || !toks[i+3].punct("(") ||
			i+4 < len(toks) && toks[i+4].is("MODEL") || i > 0 && toks[i-1].punct(".") {
			continue
		}
		fn := strings.ToUpper(toks[i+2].text)
		if _, refused := mlPreprocessUnserved[fn]; !mlPreprocessServed[fn] && !refused {
			// Not a preprocessing function BigQuery documents (a function
			// of a dataset named ML, say): the engine's to answer.
			continue
		}
		end := matchParen(toks, i+3)
		if end < 0 {
			return "", 400, "Syntax error: unclosed ML." + fn
		}
		var args []string
		for _, a := range splitTopLevel(toks[i+4 : end]) {
			if len(a) == 0 {
				return "", 400, "Syntax error: empty argument in ML." + fn
			}
			s, c, m := rewritePreprocess(sql[a[0].pos:a[len(a)-1].end], learn)
			if c != 0 {
				return "", c, m
			}
			args = append(args, s)
		}
		stop := end
		window := ""
		if end+2 < len(toks) && toks[end+1].is("OVER") && toks[end+2].punct("(") {
			we := matchParen(toks, end+2)
			if we < 0 {
				return "", 400, "Syntax error: unclosed OVER in ML." + fn
			}
			window = sql[toks[end+2].pos:toks[we].end]
			stop = we
		}
		out, c, m := mlPreprocessSQL(fn, args, window, learn)
		if c != 0 {
			return "", c, m
		}
		b.WriteString(sql[last:toks[i].pos])
		b.WriteString(out)
		last = toks[stop].end
		i = stop
	}
	if last == 0 {
		return sql, 0, ""
	}
	b.WriteString(sql[last:])
	return b.String(), 0, ""
}

// splitTopLevel splits toks at its top-level commas.
func splitTopLevel(toks []token) [][]token {
	if len(toks) == 0 {
		return nil
	}
	var out [][]token
	depth, start := 0, 0
	for k := 0; k <= len(toks); k++ {
		if k == len(toks) || depth == 0 && toks[k].punct(",") {
			out = append(out, toks[start:k])
			start = k + 1
			continue
		}
		if toks[k].punct("(") || toks[k].punct("[") {
			depth++
		}
		if toks[k].punct(")") || toks[k].punct("]") {
			depth--
		}
	}
	return out
}

// mlPreprocessServed are the ML functions of no model CloudBurrow writes
// as SQL.
var mlPreprocessServed = map[string]bool{"BUCKETIZE": true, "POLYNOMIAL_EXPAND": true, "FEATURE_CROSS": true, "DISTANCE": true, "LP_NORM": true,
	"NORMALIZER": true, "STANDARD_SCALER": true, "MIN_MAX_SCALER": true, "MAX_ABS_SCALER": true, "IMPUTER": true}

var mlAnalytic = map[string]bool{"STANDARD_SCALER": true, "MIN_MAX_SCALER": true, "MAX_ABS_SCALER": true, "IMPUTER": true}

// mlPreprocessSQL is the SQL of one call of fn, its arguments rewritten.
func mlPreprocessSQL(fn string, args []string, window string, learn mlLearn) (string, int, string) {
	if why, ok := mlPreprocessUnserved[fn]; ok {
		return "", 501, "Not implemented here: ML." + fn + ": " + why + ". CloudBurrow answers ML.BUCKETIZE, ML.POLYNOMIAL_EXPAND, " +
			"ML.FEATURE_CROSS, ML.STANDARD_SCALER, ML.MIN_MAX_SCALER, ML.MAX_ABS_SCALER, ML.IMPUTER (mean), ML.DISTANCE, ML.LP_NORM and ML.NORMALIZER. Nothing was run."
	}
	nargs := func(lo, hi int) (int, string) {
		if len(args) < lo || len(args) > hi {
			return 400, fmt.Sprintf("ML.%s takes %d to %d arguments; it was given %d", fn, lo, hi, len(args))
		}
		return 0, ""
	}
	if mlAnalytic[fn] {
		if window == "" {
			return "", 400, "ML." + fn + " is an analytic function: it requires an OVER clause"
		}
	} else if window != "" {
		return "", 400, "ML." + fn + " is not an analytic function: it takes no OVER clause"
	}
	switch fn {
	case "BUCKETIZE":
		if c, m := nargs(2, 4); c != 0 {
			return "", c, m
		}
		if len(args) == 4 {
			if s, ok := unquoteString(strings.TrimSpace(args[3])); !ok || !strings.EqualFold(s, "bucket_names") {
				return "", 501, "Not implemented here: ML.BUCKETIZE output_format " + args[3] + "; CloudBurrow gives bucket_names. Nothing was run."
			}
		}
		x, splits := "("+args[0]+")", "("+args[1]+")"
		where := x + " >= _cbml_s"
		if len(args) >= 3 {
			switch strings.ToUpper(strings.TrimSpace(args[2])) {
			case "TRUE":
				where = "_cbml_o > 0 AND _cbml_o < ARRAY_LENGTH(" + splits + ") - 1 AND " + where
			case "FALSE":
			default:
				return "", 501, "Not implemented here: ML.BUCKETIZE exclude_boundaries other than TRUE or FALSE. Nothing was run."
			}
		}
		return fmt.Sprintf("IF(%s IS NULL, CAST(NULL AS STRING), CONCAT('bin_', CAST(1 + (SELECT COUNT(1) FROM UNNEST(%s) AS _cbml_s WITH OFFSET _cbml_o WHERE %s) AS STRING)))",
			x, splits, where), 0, ""
	case "POLYNOMIAL_EXPAND", "FEATURE_CROSS":
		if c, m := nargs(1, 2); c != 0 {
			return "", c, m
		}
		names, exprs, c, m := structFields(fn, args[0])
		if c != 0 {
			return "", c, m
		}
		lo, hi := 1, 4
		if fn == "FEATURE_CROSS" {
			lo = 2
		}
		degree := 2
		if len(args) == 2 {
			d, err := strconv.Atoi(strings.TrimSpace(args[1]))
			if err != nil {
				return "", 501, "Not implemented here: ML." + fn + " of a degree that is not an integer literal. Nothing was run."
			}
			if d < lo || d > hi {
				return "", 400, fmt.Sprintf("ML.%s degree must be in [%d, %d]", fn, lo, hi)
			}
			degree = d
		}
		var fields []string
		for k := lo; k <= degree; k++ {
			if fn == "POLYNOMIAL_EXPAND" {
				for _, comb := range combinations(len(names), k, true) {
					var ns, fs []string
					for _, j := range comb {
						ns = append(ns, names[j])
						fs = append(fs, "CAST("+exprs[j]+" AS FLOAT64)")
					}
					fields = append(fields, strings.Join(fs, " * ")+" AS "+sqlIdent(strings.Join(ns, "_")))
				}
				continue
			}
			for _, comb := range combinations(len(names), k, false) {
				var ns, fs []string
				for _, j := range comb {
					ns = append(ns, names[j])
					fs = append(fs, "CAST("+exprs[j]+" AS STRING)")
				}
				fields = append(fields, "CONCAT("+strings.Join(fs, ", '_', ")+") AS "+sqlIdent(strings.Join(ns, "_")))
			}
		}
		if len(fields) == 0 {
			return "", 400, "ML." + fn + " needs at least 2 features to cross"
		}
		return "STRUCT(" + strings.Join(fields, ", ") + ")", 0, ""
	case "DISTANCE":
		if c, m := nargs(2, 3); c != 0 {
			return "", c, m
		}
		typ := "EUCLIDEAN"
		if len(args) == 3 {
			s, ok := unquoteString(strings.TrimSpace(args[2]))
			if !ok {
				return "", 501, "Not implemented here: ML.DISTANCE of a distance type that is not a string literal. Nothing was run."
			}
			typ = strings.ToUpper(s)
		}
		a, bv := "("+args[0]+")", "("+args[1]+")"
		pair := func(agg string) string {
			return fmt.Sprintf("(SELECT %s FROM UNNEST(%s) AS _cbml_a WITH OFFSET _cbml_o JOIN UNNEST(%s) AS _cbml_b WITH OFFSET _cbml_p ON _cbml_o = _cbml_p)", agg, a, bv)
		}
		var body string
		switch typ {
		case "EUCLIDEAN":
			body = "SQRT(" + pair("SUM(POW(CAST(_cbml_a AS FLOAT64) - CAST(_cbml_b AS FLOAT64), 2))") + ")"
		case "MANHATTAN":
			body = pair("SUM(ABS(CAST(_cbml_a AS FLOAT64) - CAST(_cbml_b AS FLOAT64)))")
		case "COSINE":
			body = "(1 - " + pair("SUM(CAST(_cbml_a AS FLOAT64) * CAST(_cbml_b AS FLOAT64)) / (SQRT(SUM(POW(CAST(_cbml_a AS FLOAT64), 2))) * SQRT(SUM(POW(CAST(_cbml_b AS FLOAT64), 2))))") + ")"
		default:
			return "", 400, "Invalid distance type " + typ + " for ML.DISTANCE: it must be EUCLIDEAN, MANHATTAN or COSINE"
		}
		return fmt.Sprintf("IF(%s IS NULL OR %s IS NULL, CAST(NULL AS FLOAT64), IF(ARRAY_LENGTH(%s) != ARRAY_LENGTH(%s), ERROR('ML.DISTANCE: the two arrays must have the same length'), %s))",
			a, bv, a, bv, body), 0, ""
	case "NORMALIZER":
		if c, m := nargs(1, 2); c != 0 {
			return "", c, m
		}
		p := 2.0
		if len(args) == 2 {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(args[1], " ", "")), 64)
			if err != nil || v < 1 {
				return "", 501, "Not implemented here: ML.NORMALIZER of a p that is not a number literal of at least 1 (0 and infinity are not carried out). Nothing was run."
			}
			p = v
		}
		a := "(" + args[0] + ")"
		norm := fmt.Sprintf("(SELECT POW(SUM(POW(ABS(CAST(_cbml_b AS FLOAT64)), %s)), 1 / %s) FROM UNNEST(%s) AS _cbml_b)", sqlF(p), sqlF(p), a)
		return fmt.Sprintf("IF(%s IS NULL, NULL, ARRAY(SELECT SAFE_DIVIDE(CAST(_cbml_a AS FLOAT64), %s) FROM UNNEST(%s) AS _cbml_a WITH OFFSET _cbml_o ORDER BY _cbml_o))",
			a, norm, a), 0, ""
	case "LP_NORM":
		if c, m := nargs(2, 2); c != 0 {
			return "", c, m
		}
		p, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(args[1], " ", "")), 64)
		if err != nil || p <= 0 {
			return "", 501, "Not implemented here: ML.LP_NORM of a degree that is not a positive number literal. Nothing was run."
		}
		return fmt.Sprintf("(SELECT POW(SUM(POW(ABS(CAST(_cbml_a AS FLOAT64)), %s)), 1 / %s) FROM UNNEST(%s) AS _cbml_a)", sqlF(p), sqlF(p), "("+args[0]+")"), 0, ""
	case "STANDARD_SCALER", "MIN_MAX_SCALER", "MAX_ABS_SCALER", "IMPUTER":
		if fn == "IMPUTER" {
			if c, m := nargs(2, 2); c != 0 {
				return "", c, m
			}
			s, ok := unquoteString(strings.TrimSpace(args[1]))
			if !ok || !strings.EqualFold(s, "mean") {
				return "", 501, "Not implemented here: ML.IMPUTER strategy " + args[1] + "; CloudBurrow imputes the mean. Nothing was run."
			}
		} else if c, m := nargs(1, 1); c != 0 {
			return "", c, m
		}
		x := "CAST((" + args[0] + ") AS FLOAT64)"
		if learn != nil {
			if strings.Join(strings.Fields(window), "") != "()" {
				return "", 400, "ML." + fn + " in a TRANSFORM must be OVER ()"
			}
			st, c, m := learn(args[0])
			if c != 0 {
				return "", c, m
			}
			lit := func(v *float64) string {
				if v == nil {
					return "CAST(NULL AS FLOAT64)"
				}
				return sqlF(*v)
			}
			switch fn {
			case "STANDARD_SCALER":
				return fmt.Sprintf("SAFE_DIVIDE(%s - %s, %s)", x, lit(st.mean), lit(st.sd)), 0, ""
			case "MIN_MAX_SCALER":
				rng := "CAST(NULL AS FLOAT64)"
				if st.min != nil && st.max != nil {
					rng = sqlF(*st.max - *st.min)
				}
				return fmt.Sprintf("GREATEST(0.0, LEAST(1.0, SAFE_DIVIDE(%s - %s, %s)))", x, lit(st.min), rng), 0, ""
			case "MAX_ABS_SCALER":
				return fmt.Sprintf("SAFE_DIVIDE(%s, %s)", x, lit(st.maxAbs)), 0, ""
			default:
				return fmt.Sprintf("IFNULL(%s, %s)", x, lit(st.mean)), 0, ""
			}
		}
		over := " OVER " + window
		switch fn {
		case "STANDARD_SCALER":
			return fmt.Sprintf("SAFE_DIVIDE(%s - AVG(%s)%s, STDDEV_SAMP(%s)%s)", x, x, over, x, over), 0, ""
		case "MIN_MAX_SCALER":
			return fmt.Sprintf("GREATEST(0.0, LEAST(1.0, SAFE_DIVIDE(%s - MIN(%s)%s, MAX(%s)%s - MIN(%s)%s)))", x, x, over, x, over, x, over), 0, ""
		case "MAX_ABS_SCALER":
			return fmt.Sprintf("SAFE_DIVIDE(%s, MAX(ABS(%s))%s)", x, x, over), 0, ""
		default:
			return fmt.Sprintf("IFNULL(%s, AVG(%s)%s)", x, x, over), 0, ""
		}
	}
	return "", 501, "Not implemented here: ML." + fn + ". CloudBurrow answers ML.BUCKETIZE, ML.POLYNOMIAL_EXPAND, ML.FEATURE_CROSS, " +
		"ML.STANDARD_SCALER, ML.MIN_MAX_SCALER, ML.MAX_ABS_SCALER, ML.IMPUTER (mean), ML.DISTANCE, ML.LP_NORM and ML.NORMALIZER, and the ML functions of a MODEL. Nothing was run."
}

// structFields reads a STRUCT(expr [AS name], ...) argument into its
// fields' names and expressions.
func structFields(fn, arg string) (names, exprs []string, code int, msg string) {
	toks, ok := lex(arg)
	if !ok || len(toks) < 3 || !toks[0].is("STRUCT") || !toks[1].punct("(") || matchParen(toks, 1) != len(toks)-1 {
		return nil, nil, 501, "Not implemented here: ML." + fn + " of an argument that is not a STRUCT(...) literal. Nothing was run."
	}
	for _, f := range splitTopLevel(toks[2 : len(toks)-1]) {
		if len(f) == 0 {
			return nil, nil, 400, "Syntax error: empty field in ML." + fn
		}
		n := len(f)
		if n >= 3 && f[n-2].is("AS") && isName(f[n-1]) {
			names = append(names, f[n-1].text)
			exprs = append(exprs, "("+arg[f[0].pos:f[n-3].end]+")")
			continue
		}
		plain := true
		for k, t := range f {
			plain = plain && (k%2 == 0 && isName(t) || k%2 == 1 && t.punct("."))
		}
		if !plain || n%2 == 0 {
			return nil, nil, 400, "ML." + fn + " needs a name for every STRUCT field (expr AS name)"
		}
		names = append(names, f[n-1].text)
		exprs = append(exprs, arg[f[0].pos:f[n-1].end])
	}
	return names, exprs, 0, ""
}

// combinations are the k-index combinations of 0..n-1 in lexicographic
// order, with repetition when rep.
func combinations(n, k int, rep bool) [][]int {
	var out [][]int
	var rec func(start int, cur []int)
	rec = func(start int, cur []int) {
		if len(cur) == k {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for j := start; j < n; j++ {
			next := j + 1
			if rep {
				next = j
			}
			rec(next, append(cur, j))
		}
	}
	rec(0, nil)
	return out
}

// learnTransform writes a TRANSFORM's ML function calls as SQL, the
// analytic ones with the statistics they learn over the training query's
// rows, and refuses any other window.
func (f front) learnTransform(r *http.Request, q queryOptions, items []string, query string) ([]string, int, string) {
	learn := func(expr string) (mlStats, int, string) {
		_, rows, c, m := f.queryAllRows(r, q, "SELECT AVG(_cbml_x), STDDEV_SAMP(_cbml_x), MIN(_cbml_x), MAX(_cbml_x), MAX(ABS(_cbml_x)) FROM (SELECT CAST(("+
			expr+") AS FLOAT64) AS _cbml_x FROM ("+query+"))")
		if c != 0 {
			return mlStats{}, c, m
		}
		if len(rows) != 1 || len(rows[0]) != 5 {
			return mlStats{}, 500, "cloudburrow: could not read a TRANSFORM's statistics"
		}
		v := make([]*float64, 5)
		for k, s := range rows[0] {
			if s == nil {
				continue
			}
			x, err := strconv.ParseFloat(*s, 64)
			if err != nil {
				return mlStats{}, 500, "cloudburrow: could not read a TRANSFORM's statistics: " + *s
			}
			v[k] = &x
		}
		return mlStats{mean: v[0], sd: v[1], min: v[2], max: v[3], maxAbs: v[4]}, 0, ""
	}
	var out []string
	for _, it := range items {
		s, c, m := rewritePreprocess(it, learn)
		if c != 0 {
			return nil, c, m
		}
		if toks, ok := lex(s); ok {
			for _, t := range toks {
				if t.is("OVER") {
					return nil, 501, "Not implemented here: a TRANSFORM item with a window (" + it + "): it is analytic over the training rows. " +
						"CloudBurrow carries out TRANSFORM items that read their own row, and ML.STANDARD_SCALER, ML.MIN_MAX_SCALER, " +
						"ML.MAX_ABS_SCALER and ML.IMPUTER (mean) OVER (). Nothing was run."
				}
			}
		}
		out = append(out, s)
	}
	return out, 0, ""
}
