package bigqueryfront

import (
	"strings"
)

// TRANSFORM clauses (#1481,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create#transform):
// CREATE MODEL m TRANSFORM(select_list) ... AS query trains on the
// columns of SELECT select_list FROM (query), and keeps the list, so
// ML.PREDICT, ML.EVALUATE and the other functions of an input apply it to
// their input rows. A * or * EXCEPT (...) item is expanded, at CREATE
// MODEL, into the query's columns. At prediction the items that are the
// label, the data split or the k-means init column by name are left out,
// so the input needs only the columns the other items read, and its other
// columns pass through to the output, as BigQuery documents.
//
// The ML preprocessing functions in an item are written as SQL
// (modelpreprocess.go), the analytic ones (ML.STANDARD_SCALER, ...
// OVER ()) with the statistics they learn over the training rows, kept in
// the model as constants. Refused 501 with the reason: the preprocessing
// functions CloudBurrow does not carry out, and any other window (OVER).

// transformItems reads TRANSFORM(...)'s select list (toks, inside the
// parentheses) into its items' text.
func transformItems(toks []token, sql string) ([]string, int, string) {
	var items []string
	depth, start := 0, 0
	for k := 0; k <= len(toks); k++ {
		if k == len(toks) || depth == 0 && toks[k].punct(",") {
			if k == start {
				return nil, 400, "Syntax error: empty item in TRANSFORM"
			}
			// ML functions and windows are the front's learnTransform's
			// (modelpreprocess.go).
			item := toks[start:k]
			items = append(items, strings.TrimSpace(sql[item[0].pos:item[len(item)-1].end]))
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
	return items, 0, ""
}

// expandTransform replaces each * or * EXCEPT (a, ...) item with the
// columns of cols it names.
func expandTransform(items []string, cols []mlColumn) ([]string, string) {
	var out []string
	for _, it := range items {
		if !strings.HasPrefix(it, "*") {
			out = append(out, it)
			continue
		}
		skip := map[string]bool{}
		rest := strings.TrimSpace(it[1:])
		if rest != "" {
			toks, ok := lex(rest)
			if !ok || len(toks) < 3 || !toks[0].is("EXCEPT") || !toks[1].punct("(") || !toks[len(toks)-1].punct(")") {
				return nil, "Not implemented here: TRANSFORM item " + it + "; CloudBurrow expands * and * EXCEPT (...) only. Nothing was run."
			}
			for _, t := range toks[2 : len(toks)-1] {
				if !t.punct(",") {
					skip[strings.ToLower(t.text)] = true
				}
			}
		}
		for _, c := range cols {
			if !skip[strings.ToLower(c.Name)] {
				out = append(out, sqlIdent(c.Name))
			}
		}
	}
	return out, ""
}

// trainTransform is the model's TRANSFORM list, as training applied it.
func (md bqModel) trainTransform() []string { return md.Transform }

// predictTransform is the TRANSFORM list without the items that are the
// label, data split or init column by name, which an input to predict
// need not have.
func (md bqModel) predictTransform() []string {
	drop := map[string]bool{strings.ToLower(md.Trained.Label): md.Trained.Label != ""}
	for _, c := range []string{md.Options.SplitCol, md.Options.KMeansInitCol} {
		if c != "" {
			drop[strings.ToLower(c)] = true
		}
	}
	var out []string
	for _, it := range md.Transform {
		expr, name := transformItemName(it)
		if drop[strings.ToLower(name)] && strings.EqualFold(strings.Trim(expr, "`"), name) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// transformItemName splits a TRANSFORM item into its expression and its
// output column's name: expr AS name, or a column by itself.
func transformItemName(it string) (expr, name string) {
	toks, ok := lex(it)
	if ok && len(toks) >= 3 && toks[len(toks)-2].is("AS") && (toks[len(toks)-1].kind == tokWord || toks[len(toks)-1].kind == tokQuoted) {
		return strings.TrimSpace(it[:toks[len(toks)-2].pos]), toks[len(toks)-1].text
	}
	name = strings.Trim(it, "`")
	if k := strings.LastIndex(name, "."); k >= 0 {
		name = name[k+1:]
	}
	return it, name
}

// transformInputs are the columns of cols, the training query's, that the
// TRANSFORM items read (a word of an item names one), but the label: the
// model's featureColumns (#1595, models.go).
func transformInputs(items []string, cols []mlColumn, label string) []mlColumn {
	read := map[string]bool{}
	for _, it := range items {
		toks, ok := lex(it)
		if !ok {
			continue
		}
		for _, t := range toks {
			if t.kind == tokWord || t.kind == tokQuoted {
				read[strings.ToLower(t.text)] = true
			}
		}
	}
	var out []mlColumn
	for _, c := range cols {
		if read[strings.ToLower(c.Name)] && !strings.EqualFold(c.Name, label) {
			out = append(out, c)
		}
	}
	return out
}
