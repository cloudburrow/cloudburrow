package bigqueryfront

import (
	"strconv"
	"strings"
	"unicode"
)

// Hybrid search (#1952): VECTOR_SEARCH's single form with
// lexical_search_columns and lexical_search_query_value, and AI.SEARCH's
// mode => 'HYBRID' (or AUTO over a hybrid index: a vector index with the
// lexical_search_columns option), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/search_functions#hybrid-search
// and .../bigqueryml-syntax-ai-search document them (read 2026-11-19): "A
// hybrid search lets you use the VECTOR_SEARCH function to combine a
// semantic search with a lexical (keyword) search", over one or more
// STRING columns, and "Hybrid search doesn't support batch queries".
//
// The front ranks every base row twice: by its semantic distance (the
// rows an index probes, when one is used), and lexically, by BM25 (k1 1.2,
// b 0.75) of the query value's terms in the lexical columns' text,
// lower-cased and split at every character that is not a letter or a
// digit, the columns read as one document. The two rankings are fused by
// reciprocal rank fusion with k = 60, and distance is 1 minus the fused
// score, nearest first: the page's example gives distances of exactly
// that form (tiger 1 - (1/62 + 1/62), lion 1 - (1/61 + 1/64)).
//
// UNVERIFIED (re-read 2026-11-19, the pages above, which document no formula): BM25 and the tokenizer as
// the lexical score, and rows of equal lexical score ranked by their
// semantic distance. The page's example ranks wolf above tiger lexically
// and lion fourth, which no length-normalized score gives, so its
// distances are not reproduced exactly, though its two rows are.

// lexicalSearch is the lexical half of one hybrid search.
type lexicalSearch struct {
	columns  []string // the base table's STRING columns
	terms    []string // the query value's distinct terms
	paramRef string   // a query parameter the value is, kept referenced
}

// lexicalTerms splits s as the lexical search does.
func lexicalTerms(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// parseLexicalSearch reads lexical_search_columns (cols, an ARRAY of
// column names) and the lexical query value (value, a STRING literal or
// query parameter) of a call of fn.
func parseLexicalSearch(q queryOptions, fields []map[string]any, cols, value, fn string) (*lexicalSearch, int, string) {
	l := &lexicalSearch{}
	toks, ok := lex(cols)
	if !ok || len(toks) < 2 || !toks[0].punct("[") || !toks[len(toks)-1].punct("]") {
		return nil, 400, "lexical_search_columns of " + fn + " must be an ARRAY<STRING> of column names"
	}
	byName := map[string]map[string]any{}
	for _, fl := range fields {
		byName[strings.ToLower(str(fl, "name"))] = fl
	}
	seen := map[string]bool{}
	for i, t := range toks[1 : len(toks)-1] {
		if i%2 == 1 {
			if !t.punct(",") {
				return nil, 400, "lexical_search_columns of " + fn + " must be an ARRAY<STRING> of column names"
			}
			continue
		}
		s, ok := stringLiteral(t)
		if !ok {
			return nil, 400, "lexical_search_columns of " + fn + " must be an ARRAY<STRING> of column names"
		}
		fl := byName[strings.ToLower(s)]
		if fl == nil || !strings.EqualFold(str(fl, "type"), "STRING") || str(fl, "mode") == "REPEATED" {
			return nil, 400, "lexical_search_columns of " + fn + ": " + s + " must be a STRING column of the base table"
		}
		if seen[strings.ToLower(s)] {
			return nil, 400, "lexical_search_columns of " + fn + " must be unique: " + s + " is given twice"
		}
		seen[strings.ToLower(s)] = true
		l.columns = append(l.columns, str(fl, "name"))
	}
	if len(l.columns) == 0 {
		return nil, 400, "lexical_search_columns of " + fn + " must name at least one column"
	}
	var text *string
	if strings.HasPrefix(value, "@") {
		v, isString := queryParameterString(q, value)
		if !isString {
			return nil, 400, "The lexical query value of " + fn + " must be a STRING literal or a STRING query parameter"
		}
		text, l.paramRef = v, " AND ("+value+" IS NULL OR TRUE)"
	} else if s, ok := unquoteString(value); ok {
		text = &s
	} else {
		return nil, 400, "The lexical query value of " + fn + " must be a STRING literal or a STRING query parameter"
	}
	if text == nil {
		return nil, 400, "The lexical query value of " + fn + " can't be NULL"
	}
	l.terms = lexicalTerms(*text)
	return l, 0, ""
}

// hybridSQL is the hybrid search's rows: from, the base rows as b (and
// any probe), present, which rows have an embedding, cand, which rows the
// index probes ("" for all), dist, a row's semantic distance, baseCols,
// the base STRUCT's fields, keep (columns of b to keep) and carry (those
// the rows give), post, the post-filter, and topK.
func (l *lexicalSearch) hybridSQL(from, present, cand, dist string, baseCols []string, keep, carry string, post bool, topK int64) string {
	if cand == "" {
		cand = "TRUE"
	}
	var parts []string
	for _, c := range l.columns {
		parts = append(parts, "IFNULL(b."+sqlIdent(c)+", '')")
	}
	doc := parts[0]
	if len(parts) > 1 {
		doc = "CONCAT(" + strings.Join(parts, ", ' ', ") + ")"
	}
	a := "SELECT STRUCT(" + strings.Join(baseCols, ", ") + ") AS base, " + dist + " AS _cb_hs_sem, " + cand + " AS _cb_hs_cand, " +
		"REGEXP_EXTRACT_ALL(LOWER(" + doc + "), r'[\\p{L}\\p{N}]+') AS _cb_hs_tok" + keep + " FROM " + from + " WHERE " + present + l.paramRef
	tf, df, score := []string{"ARRAY_LENGTH(_cb_hs_tok) AS _cb_hs_dl"}, []string{"COUNT(*) OVER () AS _cb_hs_n", "AVG(_cb_hs_dl) OVER () AS _cb_hs_avgdl"}, []string{"0"}
	for i, t := range l.terms {
		n := strconv.Itoa(i)
		tf = append(tf, "(SELECT COUNT(*) FROM UNNEST(_cb_hs_tok) AS _cb_hs_w WHERE _cb_hs_w = "+sqlString(t)+") AS _cb_hs_tf"+n)
		df = append(df, "COUNTIF(_cb_hs_tf"+n+" > 0) OVER () AS _cb_hs_df"+n)
		score = append(score, "IFNULL(LN(1 + (_cb_hs_n - _cb_hs_df"+n+" + 0.5) / (_cb_hs_df"+n+" + 0.5)) * _cb_hs_tf"+n+" * 2.2 / "+
			"(_cb_hs_tf"+n+" + 1.2 * (0.25 + 0.75 * _cb_hs_dl / NULLIF(_cb_hs_avgdl, 0))), 0)")
	}
	b := "SELECT *, " + strings.Join(tf, ", ") + " FROM (" + a + ")"
	c := "SELECT *, " + strings.Join(df, ", ") + " FROM (" + b + ")"
	d := "SELECT *, " + strings.Join(score, " + ") + " AS _cb_hs_lex FROM (" + c + ")"
	e := "SELECT *, IF(_cb_hs_cand, ROW_NUMBER() OVER (PARTITION BY _cb_hs_cand ORDER BY _cb_hs_sem), NULL) AS _cb_hs_rs, " +
		"ROW_NUMBER() OVER (ORDER BY _cb_hs_lex DESC, _cb_hs_sem) AS _cb_hs_rl FROM (" + d + ")"
	fused := "1 - (IFNULL(1 / (60 + _cb_hs_rs), 0) + 1 / (60 + _cb_hs_rl))"
	sel := "base, " + fused + " AS distance" + carry
	if post {
		sel += ", _cb_vs_keep"
	}
	sub := "SELECT " + sel + " FROM (" + e + ") ORDER BY distance"
	if topK >= 0 {
		sub += " LIMIT " + strconv.FormatInt(topK, 10)
	}
	if post {
		sub = "SELECT base, distance" + carry + " FROM (" + sub + ") WHERE _cb_vs_keep"
	}
	return "(" + sub + ")"
}
