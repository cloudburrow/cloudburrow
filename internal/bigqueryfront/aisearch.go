package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// AI.SEARCH (#1778), the table-valued function
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-search
// documents (Preview, read 2026-11-18):
//
//	AI.SEARCH({TABLE base_table | (base_table_query)}, column_to_search, query_value
//	  [, mode => 'AUTO' | 'VECTOR' | 'HYBRID'] [, top_k => INT64] [, distance_type => STRING]
//	  [, rerank => BOOL] [, options => JSON STRING])
//
// over a table with autonomous embedding generation (autoembed.go):
// column_to_search is the source column, query_value a STRING literal or
// query parameter "embedded at runtime using the base table's connection
// and endpoint", with RETRIEVAL_QUERY when the column's task_type is
// RETRIEVAL_DOCUMENT and the column's task_type otherwise. The rows are
// base (the base table's columns, or the query's) and distance, nearest
// first, top_k of them (10; a negative one all). "Rows with missing
// embeddings are skipped", and "if embedding fails, the whole query fails".
//
// The front embeds the query value on the local embedding model and writes
// the call into the query as a subquery that computes each row's distance
// from its generated embedding: brute force, which is what BigQuery does
// with no vector index ("If no usable index exists, brute force is used
// regardless"), or through the column's vector index (vectorindex.go,
// #1952), as options' use_brute_force and fraction_lists_to_search say.
// mode HYBRID, and AUTO over a hybrid index, is hybrid search
// (hybridsearch.go); rerank => TRUE reranks on the local model
// (rerank.go).
//
// An ObjectRef source column (autoembed.go) and an ObjectRef query value
// ("A string literal or ObjectRef value", #1983) are served: the value is
// embedded by :embedContent; rerank refuses ObjectRefs ("ObjectRef values
// aren't supported"), and HYBRID an ObjectRef value (UNVERIFIED: the
// lexical half needs text; the page states no rule). UNVERIFIED (re-read 2026-11-18, the page quotes no error text,
// and gives no formula): the refusals' words; DOT_PRODUCT's distance as
// the negative dot product, as VECTOR_SEARCH's order (nearest first)
// implies; a top_k of 0 giving no rows; a NULL query value failing 400.

// aiSearchCall is one AI.SEARCH call of a query.
type aiSearchCall struct {
	start, end int
	table      []string // TABLE t, or the base query's table
	query      string   // the base query, or ""
	column     string
	value      string // the query value's SQL
	named      map[string]string
	fromAt     int // the base query's FROM offset in query
}

// findAISearch finds text's AI.SEARCH calls.
func findAISearch(text string) ([]aiSearchCall, int, string) {
	if !strings.Contains(strings.ToUpper(text), "SEARCH") {
		return nil, 0, ""
	}
	toks, ok := lex(text)
	if !ok {
		return nil, 0, ""
	}
	depth := tokenDepths(toks)
	var calls []aiSearchCall
	for i := 0; i+3 < len(toks); i++ {
		if !toks[i].is("AI") || !toks[i+1].punct(".") || !toks[i+2].is("SEARCH") || !toks[i+3].punct("(") || i > 0 && toks[i-1].punct(".") {
			continue
		}
		end := matchParen(toks, i+3)
		if end < 0 {
			return nil, 400, "Syntax error: unclosed AI.SEARCH("
		}
		c := aiSearchCall{start: toks[i].pos, end: toks[end].end, named: map[string]string{}}
		args := splitCallArgs(toks, depth, i+3, end)
		var pos [][]token
		for _, a := range args {
			if len(a) == 0 {
				return nil, 400, "Syntax error: an empty argument of AI.SEARCH"
			}
			if len(a) > 3 && a[0].kind == tokWord && a[1].punct("=") && a[2].punct(">") {
				name := strings.ToLower(a[0].text)
				switch name {
				case "mode", "top_k", "distance_type", "rerank", "options":
				default:
					return nil, 400, "Unknown argument " + name + " of AI.SEARCH"
				}
				if _, dup := c.named[name]; dup {
					return nil, 400, "AI.SEARCH was given its " + name + " argument twice"
				}
				c.named[name] = strings.TrimSpace(text[a[3].pos:a[len(a)-1].end])
				continue
			}
			if len(c.named) > 0 {
				return nil, 400, "AI.SEARCH takes its base table, column_to_search and query_value first; name the others (top_k => ...)"
			}
			pos = append(pos, a)
		}
		if len(pos) != 3 {
			return nil, 400, "AI.SEARCH takes a base table, column_to_search and query_value"
		}
		var msg string
		if c.table, c.query, c.fromAt, msg = parseSearchBase(text, pos[0], "AI.SEARCH"); msg != "" {
			return nil, 400, msg
		}
		col, ok := stringLiteral(pos[1][0])
		if len(pos[1]) != 1 || !ok {
			return nil, 400, "The column_to_search argument of AI.SEARCH must be a STRING literal"
		}
		c.column = col
		c.value = strings.TrimSpace(text[pos[2][0].pos:pos[2][len(pos[2])-1].end])
		calls = append(calls, c)
		i = end
	}
	return calls, 0, ""
}

// queryParameterString is the STRING value of the named (or the n-th
// positional) query parameter of q.
func queryParameterString(q queryOptions, ref string) (*string, bool) {
	var params []struct {
		Name  string `json:"name"`
		Value struct {
			Value *string `json:"value"`
		} `json:"parameterValue"`
		Type struct {
			Type string `json:"type"`
		} `json:"parameterType"`
	}
	if json.Unmarshal(q.QueryParameters, &params) != nil {
		return nil, false
	}
	name := strings.TrimPrefix(ref, "@")
	for _, p := range params {
		if strings.EqualFold(p.Name, name) {
			return p.Value.Value, p.Type.Type == "STRING"
		}
	}
	return nil, false
}

// rewriteAISearch evaluates text's AI.SEARCH calls into subqueries.
func (f front) rewriteAISearch(r *http.Request, q queryOptions, text string, stats *[]map[string]any) (string, int, string) {
	calls, code, msg := findAISearch(text)
	if code != 0 || len(calls) == 0 {
		return text, code, msg
	}
	var b strings.Builder
	last := 0
	for _, c := range calls {
		sub, code, msg := f.aiSearchSubquery(r, q, c, stats)
		if code != 0 {
			return "", code, msg
		}
		b.WriteString(text[last:c.start])
		b.WriteString(sub)
		last = c.end
	}
	b.WriteString(text[last:])
	return b.String(), 0, ""
}

// aiSearchSubquery is c's rows as a subquery.
func (f front) aiSearchSubquery(r *http.Request, q queryOptions, c aiSearchCall, stats *[]map[string]any) (string, int, string) {
	project, dataset, table, ok := f.resolveModel(c.table, q)
	if !ok {
		return "", 400, "Table name " + strings.Join(c.table, ".") + " missing dataset while no default dataset is set in the request."
	}
	fields, meta, found := f.tableSchemaFields(r, project, dataset, table)
	if !found {
		return "", 404, "Not found: Table " + project + ":" + dataset + "." + table
	}
	if t := str(meta, "type"); t == "VIEW" || t == "MATERIALIZED_VIEW" {
		return "", 400, "The base table of AI.SEARCH can't be a view: " + dataset + "." + table
	}
	e := f.tables.get(streamKey(project, dataset, table), str(meta, "creationTime")).Embedding
	if e == nil {
		return "", 400, "The base table " + dataset + "." + table + " of AI.SEARCH must have autonomous embedding generation enabled: " +
			"a column GENERATED ALWAYS AS (AI.EMBED(...)) STORED OPTIONS (asynchronous = TRUE)"
	}
	if !strings.EqualFold(c.column, e.Source) {
		return "", 400, "The column_to_search " + c.column + " of AI.SEARCH must be the source column of a generated embedding column of " + dataset + "." + table +
			" (" + e.Source + ", embedded as " + e.Column + ")"
	}
	// The named arguments.
	mode := "AUTO"
	if v, ok := c.named["mode"]; ok {
		s, ok := unquoteString(v)
		mode = strings.ToUpper(s)
		if !ok || mode != "AUTO" && mode != "VECTOR" && mode != "HYBRID" {
			return "", 400, "mode must be 'AUTO', 'VECTOR' or 'HYBRID'"
		}
	}
	topK := int64(10)
	if v, ok := c.named["top_k"]; ok {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return "", 400, "top_k must be an INT64 literal"
		}
		topK = n
	}
	distance := "EUCLIDEAN"
	if v, ok := c.named["distance_type"]; ok {
		s, ok := unquoteString(v)
		distance = strings.ToUpper(s)
		if !ok || distance != "EUCLIDEAN" && distance != "COSINE" && distance != "DOT_PRODUCT" {
			return "", 400, "distance_type must be 'EUCLIDEAN', 'COSINE' or 'DOT_PRODUCT'"
		}
	}
	rerank := false
	if v, ok := c.named["rerank"]; ok {
		switch strings.ToUpper(strings.TrimSpace(v)) {
		case "TRUE":
			rerank = true
		case "FALSE":
		default:
			return "", 400, "rerank must be a BOOL literal"
		}
	}
	if v, ok := c.named["options"]; ok {
		if msg := checkSearchOptions(v, rerank, "AI.SEARCH"); msg != "" {
			return "", 400, msg
		}
	}
	numCandidates := topK * 5
	if rerank {
		var code int
		var msg string
		if numCandidates, code, msg = rerankCandidates(c.named["options"], topK, fields, c.column); code != 0 {
			return "", code, msg
		}
	}
	// The query value: "A string literal or ObjectRef value" (#1983).
	var value *string
	var valueRef *objRef
	if s, ok := unquoteString(c.value); ok {
		value = &s
	} else if strings.HasPrefix(c.value, "@") {
		v, isString := queryParameterString(q, c.value)
		if !isString {
			return "", 400, "The query_value of AI.SEARCH must be a STRING literal or a STRING query parameter"
		}
		value = v
	} else {
		ps, isVec, code, msg := f.searchValuePieces(r, q, c.value)
		if code != 0 {
			return "", code, msg
		}
		switch {
		case isVec:
			return "", 400, "The query_value of AI.SEARCH must be a STRING literal, a STRING query parameter or an ObjectRef"
		case ps == nil:
		case ps[0].ref != nil:
			valueRef = ps[0].ref
		default:
			value = &ps[0].text
		}
		if value == nil && valueRef == nil {
			return "", 400, "The query_value of AI.SEARCH can't be NULL"
		}
	}
	if value == nil && valueRef == nil {
		return "", 400, "The query_value of AI.SEARCH can't be NULL"
	}
	if rerank && (valueRef != nil || e.SourceRef) {
		// "column_to_search must be a STRING, and query_value must be a
		// non-empty STRING ... ObjectRef values aren't supported."
		return "", 400, "With rerank => TRUE, column_to_search and query_value of AI.SEARCH must be STRING: ObjectRef values aren't supported"
	}
	if rerank && *value == "" {
		return "", 400, "With rerank => TRUE, the query_value of AI.SEARCH must be a non-empty STRING"
	}
	if e.Connection != "" {
		parts := strings.Split(e.Connection, ".")
		cn, ok := f.connections.lookup(connectionProject(parts[0], f.numbers), parts[1], parts[2])
		if !ok {
			return "", 404, "Not found: Connection " + e.Connection
		}
		if code, msg := f.checkConnectionUse(r.Context(), parts[0], project, cn); code != 0 {
			return "", code, msg
		}
	}
	task := e.TaskType
	if task == "RETRIEVAL_DOCUMENT" {
		task = "RETRIEVAL_QUERY"
	}
	a := aiResolved{rm: &remoteModel{Model: e.Model, Endpoint: e.Endpoint, Location: e.Location, Embedding: true}, taskType: task, dims: e.Dims}
	var vec []float64
	var status string
	if valueRef != nil {
		vec, status = f.embedPieces(r, project, a, []promptPiece{{ref: valueRef}})
	} else {
		vec, status = f.embedVector(r.Context(), project, a, *value)
	}
	if status != "" {
		return "", 400, "AI.SEARCH could not embed its query_value: " + status
	}
	// The index, and whether the search is hybrid: mode HYBRID, or AUTO
	// over a hybrid index (a vector index with lexical_search_columns).
	use, code, msg := f.searchIndex(r, project, dataset, table, fields, meta, e.Column, c.named["options"])
	if code != 0 {
		return "", code, msg
	}
	var lexical *lexicalSearch
	if x := f.tables.get(streamKey(project, dataset, table), str(meta, "creationTime")).VectorIndex; mode != "VECTOR" {
		hybridIndex := x != nil && strings.EqualFold(x.Column, e.Column) && len(x.Lexical) > 0
		if mode == "HYBRID" && !hybridIndex {
			return "", 400, "AI.SEARCH with mode => 'HYBRID' needs a hybrid index: a vector index on " + e.Column + " of " + dataset + "." + table +
				" with the lexical_search_columns option"
		}
		if hybridIndex && valueRef != nil && mode == "HYBRID" {
			// The lexical half searches text: an ObjectRef has none
			// (UNVERIFIED, re-read 2026-11-20: the page states no rule; AUTO
			// searches vectors).
			return "", 400, "AI.SEARCH with mode => 'HYBRID' needs a STRING query_value for its lexical search, not an ObjectRef"
		}
		if hybridIndex && valueRef == nil {
			var quoted []string
			for _, l := range x.Lexical {
				quoted = append(quoted, sqlString(l))
			}
			var code int
			var msg string
			if lexical, code, msg = parseLexicalSearch(q, fields, "["+strings.Join(quoted, ", ")+"]", c.value, "AI.SEARCH"); code != 0 {
				return "", code, msg
			}
		}
	}
	*stats = append(*stats, use.stats())
	if _, given := c.named["distance_type"]; use.index != nil && !given {
		distance = use.index.Distance
	}
	v := floatArrayLiteral(vec)
	bvec, qv := "b._cb_vs_vec", v
	if use.index != nil && use.index.Norm == "L2" {
		// The query vector is the front's: normalized here, a literal,
		// not an expression the engine evaluates per element (#2042).
		// A zero vector stays SQL's, whose elements divide by NULL.
		bvec, qv = normalizedSQL(bvec), normalizedSQL(v)
		if nv := append([]float64(nil), vec...); slices.ContainsFunc(nv, func(x float64) bool { return x != 0 }) {
			normalize(nv)
			qv = floatArrayLiteral(nv)
		}
	}
	dist := searchDistance(distance, bvec, qv)
	// The base rows, with the embedding beside them; with an index, a
	// WHERE on columns it does not store is a post-filter.
	ref := quotePath([]string{project, dataset, table})
	embExpr := sqlIdent(e.Column) + ".result"
	baseSQL := "SELECT *, " + embExpr + " AS _cb_vs_vec FROM " + ref
	var names []string
	post := false
	if c.query == "" {
		for _, fl := range fields {
			names = append(names, str(fl, "name"))
		}
	} else {
		cols, _, code, msg := f.queryAllRows(r, probeOptions(q, c.query), "SELECT * FROM ("+c.query+") LIMIT 0")
		if code != 0 {
			return "", code, msg
		}
		for _, col := range cols {
			names = append(names, col.Name)
		}
		baseSQL = c.query[:c.fromAt] + ", " + embExpr + " AS _cb_vs_vec " + c.query[c.fromAt:]
		if wcols, where, ok := whereColumns(c.query, fields); ok && where > 0 && use.index != nil && !use.index.preFilter(wcols) {
			post = true
			baseSQL = c.query[:c.fromAt] + ", " + embExpr + " AS _cb_vs_vec, IFNULL((" + c.query[where+len("WHERE"):] + "), FALSE) AS _cb_vs_keep " +
				c.query[c.fromAt:where]
		}
	}
	if use.index != nil {
		listed := names
		if post {
			listed = append(append([]string(nil), names...), "_cb_vs_keep")
		}
		baseSQL = use.listedBase(baseSQL, listed)
	}
	// A query parameter stays referenced, for the engine, which takes
	// only the parameters a query names.
	paramRef := ""
	if strings.HasPrefix(c.value, "@") {
		paramRef = " AND (" + c.value + " IS NULL OR TRUE)"
	}
	var cols []string
	for _, n := range names {
		cols = append(cols, "b."+sqlIdent(n)+" AS "+sqlIdent(n))
	}
	from := "(" + baseSQL + ") AS b"
	cand := ""
	if use.index != nil {
		from += " CROSS JOIN (SELECT " + use.probeSQL(v) + " AS _cb_vs_probe) AS _cb_p"
		cand = "(b._cb_vs_list IS NULL OR b._cb_vs_list IN UNNEST(_cb_p._cb_vs_probe))"
	}
	present := "b._cb_vs_vec IS NOT NULL AND ARRAY_LENGTH(b._cb_vs_vec) = " + strconv.Itoa(len(vec)) + paramRef
	keep, carry, limit := "", "", topK
	if post {
		keep = ", b._cb_vs_keep AS _cb_vs_keep"
	}
	if rerank {
		keep += ", b." + sqlIdent(c.column) + " AS _cb_rr_text"
		carry, limit = ", _cb_rr_text", numCandidates
	}
	var sub string
	if lexical != nil {
		lexical.paramRef = ""
		sub = lexical.hybridSQL(from, present, cand, dist, cols, keep, carry, post, limit)
	} else {
		sub = semanticSQL(from, present, cand, dist, cols, keep, carry, post, limit)
	}
	if !rerank {
		return sub, 0, ""
	}
	return f.rerankSQL(r, q, project, a, *value, sub, topK)
}

// semanticSQL is a semantic search's rows (hybridSQL's arguments).
func semanticSQL(from, present, cand, dist string, cols []string, keep, carry string, post bool, topK int64) string {
	where := present
	if cand != "" {
		where += " AND " + cand
	}
	sub := "(SELECT base, distance" + carry + " FROM (SELECT STRUCT(" + strings.Join(cols, ", ") + ") AS base, " + dist + " AS distance" + keep + " FROM " + from + " " +
		"WHERE " + where + " ORDER BY distance"
	if topK >= 0 {
		sub += " LIMIT " + strconv.FormatInt(topK, 10)
	}
	sub += ")"
	if post {
		sub += " WHERE _cb_vs_keep"
	}
	return sub + ")"
}
