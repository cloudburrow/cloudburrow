package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// The vocabulary preprocessing functions (#1548), as BigQuery documents
// them
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-label-encoder,
// -one-hot-encoder, -multi-hot-encoder, -bag-of-words, -tf-idf, read
// 2026-10-17):
//
//   - ML.LABEL_ENCODER(x [, top_k [, frequency_threshold]]) OVER (): x's
//     index in the vocabulary, from 1, or 0 for NULL and a category not in
//     it;
//   - ML.ONE_HOT_ENCODER(x [, drop [, top_k [, frequency_threshold]]])
//     OVER (): [STRUCT(index, value)], that index and 1.0, or 0.0 for the
//     category drop => 'most_frequent' drops (dummy encoding);
//   - ML.MULTI_HOT_ENCODER(array [, top_k [, frequency_threshold]])
//     OVER (): one STRUCT(index, 1.0) for each index of the array's
//     elements, in the order they first appear;
//   - ML.BAG_OF_WORDS(tokens [, top_k [, frequency_threshold]]) OVER ():
//     STRUCT(index, count) for each index of the document's tokens, by
//     index, the unknown term 0 for every token not in the vocabulary
//     (NULL among them);
//   - ML.TF_IDF(tokens [, top_k [, frequency_threshold]]) OVER (): the
//     same with count / document size * log(1 + documents / (1 + the
//     documents of the term)); the unknown term's inverse document
//     frequency is the mean of the vocabulary's, which gives BigQuery's
//     documented example.
//
// The vocabulary is the top_k (32,000) categories or terms of the window
// (OVER () or OVER (PARTITION BY ...)) that are in at least
// frequency_threshold (5) rows, the most frequent first and, at the same
// frequency, the alphabetically first, as the documented examples choose;
// it is indexed in alphabetical order from 1. A row's array counts once
// for each term it has. In a TRANSFORM the vocabulary (and the category
// dropped, and the inverse document frequencies) is learned over the
// training rows and kept, as constants, in the model's TRANSFORM.
//
// UNVERIFIED: BigQuery documents no defaults for ML.BAG_OF_WORDS and
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-bag-of-words
// ML.TF_IDF; CloudBurrow uses the encoders' (32,000 and 5). The output of
// a NULL array is an empty array.
//
// In a query each call is written as calls of the SQL functions
// mlVocabFunctions, which the front makes in the project's resultsDataset,
// with the window's ARRAY_AGG (or ARRAY_CONCAT_AGG) as their argument: a
// window function cannot be read inside a subquery, but it can be a
// function's argument.

// mlVocab are the vocabulary functions, and their arguments after the
// first: drop for ONE_HOT_ENCODER, then top_k and frequency_threshold.
var mlVocab = map[string]bool{"LABEL_ENCODER": false, "ONE_HOT_ENCODER": true, "MULTI_HOT_ENCODER": false,
	"BAG_OF_WORDS": false, "TF_IDF": false}

// mlVocabPrefix begins the names of the vocabulary SQL functions; its
// number goes up when one of them changes, since they are made only when
// missing.
const mlVocabPrefix = "_cbml2_"

// mlVocabFn is the path of the vocabulary SQL function name.
func mlVocabFn(name string) string {
	return "`" + resultsDataset + "`.`" + mlVocabPrefix + name + "`"
}

// mlIndexSQL is the vocabulary index of e (a STRING) in voc, from 1, or 0.
func mlIndexSQL(voc, e string) string {
	return "IFNULL((SELECT _cbml_o + 1 FROM UNNEST(" + voc + ") AS _cbml_w WITH OFFSET _cbml_o WHERE _cbml_w = " + e + "), 0)"
}

// mlIndicesSQL are the indices of x's elements in voc, each once, in the
// order they first appear.
func mlIndicesSQL(voc, x string) string {
	return "ARRAY(SELECT _cbml_i FROM (SELECT _cbml_i, MIN(_cbml_p) AS _cbml_q FROM (SELECT " + mlIndexSQL(voc, "_cbml_e") +
		" AS _cbml_i, _cbml_p FROM UNNEST(" + x + ") AS _cbml_e WITH OFFSET _cbml_p) GROUP BY _cbml_i) ORDER BY _cbml_q)"
}

// mlEncodedSQL is the output of fn of x, given its vocabulary voc, the
// category dropped (ONE_HOT_ENCODER), the inverse document frequencies
// idf (TF_IDF), and indices, x's indices (mlIndicesSQL).
func mlEncodedSQL(fn, voc, drop, idf, x, indices string) string {
	idx := mlIndexSQL(voc, x)
	count := "(SELECT COUNT(*) FROM UNNEST(" + x + ") AS _cbml_e WHERE " + mlIndexSQL(voc, "_cbml_e") + " = _cbml_i)"
	switch fn {
	case "LABEL_ENCODER":
		return idx
	case "ONE_HOT_ENCODER":
		return "[STRUCT(" + idx + " AS index, IF(" + idx + " > 0 AND " + x + " = " + drop + ", 0.0, 1.0) AS value)]"
	case "MULTI_HOT_ENCODER":
		return "ARRAY(SELECT STRUCT(_cbml_i AS index, 1.0 AS value) FROM UNNEST(" + indices + ") AS _cbml_i WITH OFFSET _cbml_p ORDER BY _cbml_p)"
	case "BAG_OF_WORDS":
		return "ARRAY(SELECT STRUCT(_cbml_i AS index, CAST(" + count + " AS FLOAT64) AS value) FROM UNNEST(" + indices + ") AS _cbml_i ORDER BY _cbml_i)"
	}
	return "ARRAY(SELECT STRUCT(_cbml_i AS index, " + count + " / ARRAY_LENGTH(" + x + ") * IF(_cbml_i = 0, (SELECT AVG(_cbml_f) FROM UNNEST(" + idf +
		") AS _cbml_f), " + idf + "[OFFSET(_cbml_i - 1)]) AS value) FROM UNNEST(" + indices + ") AS _cbml_i ORDER BY _cbml_i)"
}

// mlVocabFunctions are the SQL functions a query's vocabulary functions
// are written as: vocab, the vocabulary of the terms vs (one for each row
// that has it); top, its most frequent; idf, the inverse document
// frequency of each term of voc, of n rows; and one for each function,
// of the vocabulary.
func mlVocabFunctions() []string {
	ranked := "(SELECT _cbml_v, _cbml_n, ROW_NUMBER() OVER (ORDER BY _cbml_n DESC, _cbml_v) AS _cbml_r FROM " +
		"(SELECT _cbml_v, COUNT(*) AS _cbml_n FROM UNNEST(vs) AS _cbml_v GROUP BY _cbml_v))"
	def := func(name, params, body string) string {
		return "CREATE FUNCTION IF NOT EXISTS " + mlVocabFn(name) + "(" + params + ") AS (" + body + ")"
	}
	out := []string{
		def("vocab", "vs ARRAY<STRING>, k INT64, t INT64", "ARRAY(SELECT _cbml_v FROM "+ranked+" WHERE _cbml_n >= t AND _cbml_r <= k ORDER BY _cbml_v)"),
		def("top", "vs ARRAY<STRING>, k INT64, t INT64", "(SELECT _cbml_v FROM "+ranked+" WHERE _cbml_n >= t AND _cbml_r <= k AND _cbml_r = 1)"),
		def("idf", "vs ARRAY<STRING>, n INT64, voc ARRAY<STRING>", "ARRAY(SELECT LN(1 + n / (1 + (SELECT COUNT(*) FROM UNNEST(vs) AS _cbml_w "+
			"WHERE _cbml_w = _cbml_v))) FROM UNNEST(voc) AS _cbml_v WITH OFFSET _cbml_o ORDER BY _cbml_o)"),
	}
	for _, fn := range []string{"LABEL_ENCODER", "ONE_HOT_ENCODER", "MULTI_HOT_ENCODER", "BAG_OF_WORDS", "TF_IDF"} {
		params := "voc ARRAY<STRING>, x ARRAY<STRING>"
		switch fn {
		case "LABEL_ENCODER":
			params = "voc ARRAY<STRING>, x STRING"
		case "ONE_HOT_ENCODER":
			params = "voc ARRAY<STRING>, drop_value STRING, x STRING"
		case "TF_IDF":
			params = "voc ARRAY<STRING>, idf ARRAY<FLOAT64>, x ARRAY<STRING>"
		}
		out = append(out, def(strings.ToLower(fn), params, mlEncodedSQL(fn, "voc", "drop_value", "idf", "x", mlIndicesSQL("voc", "x"))))
	}
	return out
}

// ensureMLVocab makes mlVocabFunctions in the request's project where
// they are missing, and returns the engine's error, or "".
func (f front) ensureMLVocab(r *http.Request) string {
	for _, s := range mlVocabFunctions() {
		if msg := f.runDML(r, s); msg != "" {
			return msg
		}
	}
	return ""
}

// mlVocabLearn reads one value, the JSON of expr, an aggregate, over the
// training rows.
type mlVocabLearn func(expr string) (string, int, string)

// mlVocabSQL is the SQL of one call of fn, a vocabulary function, its
// arguments rewritten. With learn (a TRANSFORM), the vocabulary is learned
// over the training rows and written as constants.
func mlVocabSQL(fn string, args []string, window string, learn mlVocabLearn, inTransform bool) (string, int, string) {
	if window == "" {
		return "", 400, "ML." + fn + " is an analytic function: it requires an OVER clause"
	}
	if toks, ok := lex(window); ok && len(toks) > 2 {
		inner := toks[1 : len(toks)-1]
		bad := !inner[0].is("PARTITION")
		for _, t := range inner {
			bad = bad || t.is("ORDER") || t.is("ROWS") || t.is("RANGE")
		}
		if bad {
			return "", 400, "ML." + fn + " takes OVER () or OVER (PARTITION BY ...): no ORDER BY or window frame"
		}
	}
	hasDrop := mlVocab[fn]
	max := 3
	if hasDrop {
		max = 4
	}
	if len(args) < 1 || len(args) > max {
		return "", 400, fmt.Sprintf("ML.%s takes 1 to %d arguments; it was given %d", fn, max, len(args))
	}
	rest := args[1:]
	drop := false
	if hasDrop && len(rest) > 0 {
		s, ok := unquoteString(strings.TrimSpace(rest[0]))
		if !ok {
			return "", 501, "Not implemented here: ML.ONE_HOT_ENCODER of a drop that is not a string literal. Nothing was run."
		}
		switch strings.ToLower(s) {
		case "none":
		case "most_frequent":
			drop = true
		default:
			return "", 400, "Invalid drop value " + s + " for ML.ONE_HOT_ENCODER: it must be 'none' or 'most_frequent'"
		}
		rest = rest[1:]
	}
	k, t := "32000", "5"
	for j, a := range rest {
		a = strings.TrimSpace(a)
		if v, err := strconv.ParseInt(a, 10, 64); err == nil {
			name := []string{"top_k", "frequency_threshold"}[j]
			if j == 0 && (v < 1 || v >= 1000000) {
				return "", 400, "ML." + fn + " " + name + " must be at least 1 and less than 1,000,000"
			}
			if j == 1 && v < 0 {
				return "", 400, "ML." + fn + " " + name + " must not be negative"
			}
		}
		if j == 0 {
			k = "(" + a + ")"
		} else {
			t = "(" + a + ")"
		}
	}
	x := "(" + args[0] + ")"
	vs := "ARRAY_AGG(" + x + " IGNORE NULLS)"
	if fn != "LABEL_ENCODER" && fn != "ONE_HOT_ENCODER" {
		vs = "ARRAY_CONCAT_AGG(ARRAY(SELECT DISTINCT _cbml_d FROM UNNEST(" + x + ") AS _cbml_d WHERE _cbml_d IS NOT NULL))"
	}
	if inTransform {
		if strings.Join(strings.Fields(window), "") != "()" {
			return "", 400, "ML." + fn + " in a TRANSFORM must be OVER ()"
		}
		if learn == nil {
			return "", 500, "cloudburrow: ML." + fn + " in a TRANSFORM: no training rows to learn its vocabulary from"
		}
		voc := mlVocabFn("vocab") + "(" + vs + ", " + k + ", " + t + ")"
		got, c, m := learn("TO_JSON_STRING(STRUCT(" + voc + " AS v, " + mlVocabFn("top") + "(" + vs + ", " + k + ", " + t + ") AS d, " +
			mlVocabFn("idf") + "(" + vs + ", COUNT(*), " + voc + ") AS i))")
		if c != 0 {
			return "", c, m
		}
		var learned struct {
			V []string   `json:"v"`
			D *string    `json:"d"`
			I []*float64 `json:"i"`
		}
		if err := json.Unmarshal([]byte(got), &learned); err != nil {
			return "", 500, "cloudburrow: could not read the vocabulary of ML." + fn + ": " + err.Error()
		}
		terms := make([]string, len(learned.V))
		for j, v := range learned.V {
			terms[j] = sqlString(v)
		}
		idfs := make([]string, len(learned.I))
		for j, v := range learned.I {
			idfs[j] = sqlFloat(v)
		}
		vocLit := "ARRAY<STRING>[" + strings.Join(terms, ", ") + "]"
		dropLit := "CAST(NULL AS STRING)"
		if drop && learned.D != nil {
			dropLit = sqlString(*learned.D)
		}
		idfLit := "ARRAY<FLOAT64>[" + strings.Join(idfs, ", ") + "]"
		return mlEncodedSQL(fn, vocLit, dropLit, idfLit, x, mlIndicesSQL(vocLit, x)), 0, ""
	}
	over := " OVER " + window
	voc := mlVocabFn("vocab") + "(" + vs + over + ", " + k + ", " + t + ")"
	switch fn {
	case "LABEL_ENCODER":
		return mlVocabFn("label_encoder") + "(" + voc + ", " + x + ")", 0, ""
	case "ONE_HOT_ENCODER":
		d := "CAST(NULL AS STRING)"
		if drop {
			d = mlVocabFn("top") + "(" + vs + over + ", " + k + ", " + t + ")"
		}
		return mlVocabFn("one_hot_encoder") + "(" + voc + ", " + d + ", " + x + ")", 0, ""
	case "TF_IDF":
		idf := mlVocabFn("idf") + "(" + vs + over + ", COUNT(*)" + over + ", " + voc + ")"
		return mlVocabFn("tf_idf") + "(" + voc + ", " + idf + ", " + x + ")", 0, ""
	}
	return mlVocabFn(strings.ToLower(fn)) + "(" + voc + ", " + x + ")", 0, ""
}
