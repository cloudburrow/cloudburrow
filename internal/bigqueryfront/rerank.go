package bigqueryfront

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// AI.SEARCH's rerank => TRUE (#1952), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-search
// documents it (read 2026-11-19): "AI.SEARCH retrieves an initial set of
// candidates from the vector search or hybrid search stage and then
// evaluates query_value and each candidate row's column_to_search text
// together to score and reorder the candidates by relevance". The
// candidates are options.reranker.num_candidates rows (top_k * 5 by
// default; negative, every row), those whose column_to_search is NULL or
// '' left out; the output gains rerank_score, "in the range of 0.0 to
// 1.0", the rows "sorted by rerank_score in descending order", top_k of
// them.
//
// BigQuery calls the Discovery Engine ranking API, whose models
// (semantic-ranker-default@latest) Google does not publish. Everything
// stays local here, so the score is the instance's local embedding model's
// (the base table's endpoint): the query embedded with task type
// RETRIEVAL_QUERY and each candidate's text with RETRIEVAL_DOCUMENT, their
// cosine similarity mapped from [-1, 1] to [0, 1]. The order is a
// relevance order, as the API's; its scores are not Google's ranker's.
//
// UNVERIFIED (re-read 2026-11-19, the page above): the refusals' words; which reranker model names are taken
// (any semantic-ranker-* name, as the ranking page lists them).

// rerankCandidates reads options.reranker of AI.SEARCH: the number of
// candidates to rerank. The column to search must be STRING.
func rerankCandidates(options string, topK int64, fields []map[string]any, column string) (int64, int, string) {
	for _, fl := range fields {
		if strings.EqualFold(str(fl, "name"), column) && !strings.EqualFold(str(fl, "type"), "STRING") {
			return 0, 400, "With rerank => TRUE, the column_to_search of AI.SEARCH must be a STRING column; " + column + " is not (ObjectRef values aren't supported)"
		}
	}
	n := topK * 5
	if topK < 0 {
		n = -1
	}
	raw, ok := jsonArgument(options)
	if !ok || options == "" {
		return n, 0, ""
	}
	var opts struct {
		Reranker *map[string]json.RawMessage `json:"reranker"`
	}
	if json.Unmarshal([]byte(raw), &opts) != nil || opts.Reranker == nil {
		return n, 0, ""
	}
	for k, v := range *opts.Reranker {
		switch k {
		case "num_candidates":
			var c int64
			if json.Unmarshal(v, &c) != nil {
				return 0, 400, "options.reranker.num_candidates must be a JSON integer"
			}
			switch {
			case topK >= 0 && c >= 0 && c < topK:
				return 0, 400, "options.reranker.num_candidates must be at least top_k (" + strconv.FormatInt(topK, 10) + "), or negative to rerank every row"
			case topK < 0 && c >= 0:
				return 0, 400, "options.reranker.num_candidates must be negative or omitted when top_k is negative"
			case c > 10000000:
				return 0, 400, "options.reranker.num_candidates can't exceed 10,000,000"
			}
			n = c
		case "model":
			var m string
			if json.Unmarshal(v, &m) != nil || !strings.HasPrefix(m, "semantic-ranker-") {
				return 0, 400, "options.reranker.model must be a ranking model name, such as semantic-ranker-default@latest"
			}
		default:
			return 0, 400, "Unknown options.reranker field " + k + ": num_candidates and model are supported"
		}
	}
	return n, 0, ""
}

// rerankSQL is AI.SEARCH's reranked rows: the candidates sub (rows base,
// distance and _cb_rr_text), scored against value on the local embedding
// model a names, top_k of them by rerank_score.
func (f front) rerankSQL(r *http.Request, q queryOptions, project string, a aiResolved, value, sub string, topK int64) (string, int, string) {
	_, rows, code, msg := f.queryAllRows(r, probeOptions(q, sub), "SELECT DISTINCT _cb_rr_text FROM "+sub+" AS _cb_rr WHERE _cb_rr_text IS NOT NULL AND _cb_rr_text != ''")
	if code != 0 {
		return "", code, msg
	}
	qa, da := a, a
	qa.taskType, da.taskType = "RETRIEVAL_QUERY", "RETRIEVAL_DOCUMENT"
	qv, status := f.embedVector(r.Context(), project, qa, value)
	if status != "" {
		return "", 400, "AI.SEARCH could not rerank: embedding the query_value failed: " + status
	}
	var pairs []string
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		dv, status := f.embedVector(r.Context(), project, da, *row[0])
		if status != "" {
			return "", 400, "AI.SEARCH could not rerank: embedding a candidate failed: " + status
		}
		pairs = append(pairs, "STRUCT("+sqlString(*row[0])+" AS t, "+sqlF(rerankScore(qv, dv))+" AS s)")
	}
	score := "CAST(NULL AS FLOAT64)"
	if len(pairs) > 0 {
		score = "(SELECT _cb_rm.s FROM UNNEST(ARRAY<STRUCT<t STRING, s FLOAT64>>[" + strings.Join(pairs, ", ") + "]) AS _cb_rm WHERE _cb_rm.t = _cb_rr._cb_rr_text)"
	}
	out := "(SELECT base, distance, " + score + " AS rerank_score FROM " + sub + " AS _cb_rr WHERE _cb_rr._cb_rr_text IS NOT NULL AND _cb_rr._cb_rr_text != '' ORDER BY rerank_score DESC"
	if topK >= 0 {
		out += " LIMIT " + strconv.FormatInt(topK, 10)
	}
	return out + ")", 0, ""
}

// rerankScore is the cosine similarity of q and d, mapped to [0, 1].
func rerankScore(q, d []float64) float64 {
	var dot, nq, nd float64
	for i := range q {
		if i >= len(d) {
			break
		}
		dot += q[i] * d[i]
		nq += q[i] * q[i]
		nd += d[i] * d[i]
	}
	if nq == 0 || nd == 0 {
		return 0
	}
	return math.Max(0, math.Min(1, (dot/math.Sqrt(nq*nd)+1)/2))
}
