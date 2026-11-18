package bigqueryfront

import (
	"encoding/json"
	"math"
	"net/http"
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
// regardless"); CloudBurrow makes no vector index (CREATE VECTOR INDEX
// fails), so mode AUTO is semantic search, and options' use_brute_force
// and fraction_lists_to_search are checked and change nothing.
//
// 501, named: mode => 'HYBRID' (it needs a hybrid search index, which is
// not served) and rerank => TRUE (the Discovery Engine ranking API, not
// served locally). UNVERIFIED (re-read 2026-11-18, the page quotes no error text,
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
		base := pos[0]
		switch {
		case base[0].is("TABLE"):
			p, n := readModelPath(base[1:])
			if n == 0 || n+1 != len(base) {
				return nil, 400, "Syntax error: expected a table name after TABLE"
			}
			c.table = p
		case base[0].punct("(") && matchParen(base, 0) == len(base)-1:
			inner := base[1 : len(base)-1]
			if len(inner) == 0 || !inner[0].is("SELECT") {
				return nil, 400, "The base_table_query of AI.SEARCH must be a SELECT ... FROM ... [WHERE ...] query"
			}
			idepth := tokenDepths(inner)
			from := -1
			for k, t := range inner {
				if idepth[k] == 0 && (t.is("JOIN") || t.is("GROUP") || t.is("ORDER") || t.is("LIMIT") || t.is("HAVING") || t.is("UNION") || t.is("WITH") || t.is("QUALIFY") || t.punct(",") && from >= 0) {
					return nil, 400, "The base_table_query of AI.SEARCH can use only SELECT, FROM and WHERE"
				}
				if idepth[k] == 0 && t.is("FROM") && from < 0 {
					from = k
				}
			}
			if from < 0 {
				return nil, 400, "The base_table_query of AI.SEARCH must read a table: SELECT ... FROM ... [WHERE ...]"
			}
			p, n := readModelPath(inner[from+1:])
			if n == 0 {
				return nil, 400, "The base_table_query of AI.SEARCH must read one table"
			}
			c.table = p
			c.query = text[inner[0].pos:inner[len(inner)-1].end]
			c.fromAt = inner[from].pos - inner[0].pos
		default:
			return nil, 400, "The first argument of AI.SEARCH must be TABLE base_table or (base_table_query)"
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
func (f front) rewriteAISearch(r *http.Request, q queryOptions, text string) (string, int, string) {
	calls, code, msg := findAISearch(text)
	if code != 0 || len(calls) == 0 {
		return text, code, msg
	}
	var b strings.Builder
	last := 0
	for _, c := range calls {
		sub, code, msg := f.aiSearchSubquery(r, q, c)
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
func (f front) aiSearchSubquery(r *http.Request, q queryOptions, c aiSearchCall) (string, int, string) {
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
	if mode == "HYBRID" {
		return "", 501, "Not implemented here: AI.SEARCH with mode => 'HYBRID': hybrid search needs a hybrid search index, which CloudBurrow does not make " +
			"(mode => 'AUTO' runs semantic search where there is none). Nothing was run."
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
		raw, ok := jsonArgument(v)
		var opts map[string]json.RawMessage
		if !ok || json.Unmarshal([]byte(raw), &opts) != nil {
			return "", 400, "The options argument of AI.SEARCH must be a JSON object STRING literal"
		}
		_, fraction := opts["fraction_lists_to_search"]
		brute := false
		for k, x := range opts {
			switch k {
			case "fraction_lists_to_search":
				fl, ok := aiJSONFloat(x)
				if !ok || fl <= 0 || fl >= 1 {
					return "", 400, "fraction_lists_to_search must be greater than 0.0 and less than 1.0"
				}
			case "use_brute_force":
				if json.Unmarshal(x, &brute) != nil {
					return "", 400, "use_brute_force must be a JSON boolean"
				}
			case "reranker":
				if !rerank {
					return "", 400, "options.reranker is only valid when rerank is TRUE"
				}
			default:
				return "", 400, "Unknown AI.SEARCH option " + k
			}
		}
		if brute && fraction {
			return "", 400, "fraction_lists_to_search can't be combined with use_brute_force"
		}
	}
	if rerank {
		return "", 501, "Not implemented here: AI.SEARCH with rerank => TRUE: reranking calls the Discovery Engine ranking API, which CloudBurrow does not serve locally. Nothing was run."
	}
	// The query value.
	var value *string
	if s, ok := unquoteString(c.value); ok {
		value = &s
	} else if strings.HasPrefix(c.value, "@") {
		v, isString := queryParameterString(q, c.value)
		if !isString {
			return "", 400, "The query_value of AI.SEARCH must be a STRING literal or a STRING query parameter"
		}
		value = v
	} else {
		return "", 400, "The query_value of AI.SEARCH must be a STRING literal or a STRING query parameter (ObjectRef query values are not served, #1798)"
	}
	if value == nil {
		return "", 400, "The query_value of AI.SEARCH can't be NULL"
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
	vec, status := f.embedVector(r.Context(), project, a, *value)
	if status != "" {
		return "", 400, "AI.SEARCH could not embed its query_value: " + status
	}
	lits := make([]string, len(vec))
	var norm float64
	for i, x := range vec {
		lits[i] = sqlF(x)
		norm += x * x
	}
	v := "ARRAY<FLOAT64>[" + strings.Join(lits, ", ") + "]"
	emb := "b._cb_search_embedding.result"
	dot := "(SELECT SUM(x * " + v + "[OFFSET(i)]) FROM UNNEST(" + emb + ") AS x WITH OFFSET i)"
	var dist string
	switch distance {
	case "EUCLIDEAN":
		dist = "SQRT((SELECT SUM(POW(x - " + v + "[OFFSET(i)], 2)) FROM UNNEST(" + emb + ") AS x WITH OFFSET i))"
	case "COSINE":
		dist = "(1 - " + dot + " / (SQRT((SELECT SUM(x * x) FROM UNNEST(" + emb + ") AS x)) * " + sqlF(math.Sqrt(norm)) + "))"
	case "DOT_PRODUCT":
		dist = "(-" + dot + ")"
	}
	// The base rows, with the embedding beside them.
	ref := quotePath([]string{project, dataset, table})
	baseSQL := "SELECT *, " + sqlIdent(e.Column) + " AS _cb_search_embedding FROM " + ref
	var names []string
	if c.query == "" {
		for _, fl := range fields {
			names = append(names, str(fl, "name"))
		}
	} else {
		probe := q
		if !strings.Contains(c.query, "@") && !strings.Contains(c.query, "?") {
			probe.QueryParameters, probe.ParameterMode = nil, ""
		}
		cols, _, code, msg := f.queryAllRows(r, probe, "SELECT * FROM ("+c.query+") LIMIT 0")
		if code != 0 {
			return "", code, msg
		}
		for _, col := range cols {
			names = append(names, col.Name)
		}
		baseSQL = c.query[:c.fromAt] + ", " + sqlIdent(e.Column) + " AS _cb_search_embedding " + c.query[c.fromAt:]
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
	sub := "(SELECT base, distance FROM (SELECT STRUCT(" + strings.Join(cols, ", ") + ") AS base, " + dist + " AS distance FROM (" + baseSQL + ") AS b " +
		"WHERE " + emb + " IS NOT NULL AND ARRAY_LENGTH(" + emb + ") = " + strconv.Itoa(len(vec)) + paramRef + ") ORDER BY distance"
	if topK >= 0 {
		sub += " LIMIT " + strconv.FormatInt(topK, 10)
	}
	return sub + ")", 0, ""
}
