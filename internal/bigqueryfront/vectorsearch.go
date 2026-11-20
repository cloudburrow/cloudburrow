package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// VECTOR_SEARCH (#1778), the table-valued function
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/search_functions#vector_search
// documents (read 2026-11-18):
//
//	VECTOR_SEARCH({TABLE base_table | (base_table_query)}, column_to_search,
//	  {TABLE query_table | (query_table_query)}
//	  [, query_column_to_search => STRING] [, top_k => INT64]
//	  [, distance_type => STRING] [, options => JSON STRING])
//
// and its single-query form, VECTOR_SEARCH(base, column_to_search,
// query_value => ... [, top_k] [, distance_type] [, options]). The
// column_to_search is an ARRAY<FLOAT64> column, or a STRING column with
// autonomous embedding generation (autoembed.go), whose generated
// embeddings are searched, rows without one skipped, and a STRING query
// value or query column embedded at run time with the base table's model
// and task type (RETRIEVAL_QUERY for RETRIEVAL_DOCUMENT), as AI.SEARCH.
// query_column_to_search (named, or positional after the query table, as
// the page's examples give it) defaults to column_to_search's name, or the query
// table's one ARRAY<FLOAT64> (or, for a STRING column, STRING) column.
// The rows are query (the query table's columns, batch form only), base
// and distance, top_k (10; a negative one every row) per query row.
//
// With no usable vector index the search is brute force, which is what
// BigQuery does: "If you don't have a vector index, BigQuery uses brute
// force, which returns exact results"; with one it probes the index's
// lists (vectorindex.go, #1952), as options' use_brute_force and
// fraction_lists_to_search say. lexical_search_columns and
// lexical_search_query_value make it a hybrid search (hybridsearch.go).
//
// An ObjectRef column with autonomous embedding generation, an ObjectRef
// query value and an ObjectRef query column are served as STRING ones are,
// each ObjectRef "embedded at runtime using the same connection and
// endpoint" (#1983, search_functions#objectref-search). UNVERIFIED (re-read 2026-11-18; the page quotes no error
// text): the refusals' words; DOT_PRODUCT's distance as the negative dot
// product; a base row with an empty (or NULL) array being skipped; a
// query row whose array's length differs from a base row's failing the
// query.

// vectorSearchCall is one VECTOR_SEARCH call of a query.
type vectorSearchCall struct {
	start, end int
	table      []string // TABLE t, or the base query's table
	query      string   // the base query, or ""
	fromAt     int
	column     string
	queryTable []string // TABLE q
	querySQL   string   // (query_table_query), or ""
	named      map[string]string
}

// parseSearchBase reads the base table argument of AI.SEARCH and
// VECTOR_SEARCH: TABLE t, or a (SELECT ... FROM t [WHERE ...]) query.
func parseSearchBase(text string, base []token, fn string) (table []string, query string, fromAt int, msg string) {
	switch {
	case base[0].is("TABLE"):
		p, n := readModelPath(base[1:])
		if n == 0 || n+1 != len(base) {
			return nil, "", 0, "Syntax error: expected a table name after TABLE"
		}
		return p, "", 0, ""
	case base[0].punct("(") && matchParen(base, 0) == len(base)-1:
		inner := base[1 : len(base)-1]
		if len(inner) == 0 || !inner[0].is("SELECT") {
			return nil, "", 0, "The base_table_query of " + fn + " must be a SELECT ... FROM ... [WHERE ...] query"
		}
		idepth := tokenDepths(inner)
		from := -1
		for k, t := range inner {
			if idepth[k] == 0 && (t.is("JOIN") || t.is("GROUP") || t.is("ORDER") || t.is("LIMIT") || t.is("HAVING") || t.is("UNION") || t.is("WITH") || t.is("QUALIFY") || t.punct(",") && from >= 0) {
				return nil, "", 0, "The base_table_query of " + fn + " can use only SELECT, FROM and WHERE"
			}
			if idepth[k] == 0 && t.is("FROM") && from < 0 {
				from = k
			}
		}
		if from < 0 {
			return nil, "", 0, "The base_table_query of " + fn + " must read a table: SELECT ... FROM ... [WHERE ...]"
		}
		p, n := readModelPath(inner[from+1:])
		if n == 0 {
			return nil, "", 0, "The base_table_query of " + fn + " must read one table"
		}
		return p, text[inner[0].pos:inner[len(inner)-1].end], inner[from].pos - inner[0].pos, ""
	}
	return nil, "", 0, "The first argument of " + fn + " must be TABLE base_table or (base_table_query)"
}

// checkSearchOptions checks the options JSON of AI.SEARCH or VECTOR_SEARCH:
// fraction_lists_to_search (0 < f < 1), use_brute_force (a boolean), not
// both, and AI.SEARCH's reranker only with rerank.
func checkSearchOptions(v string, rerank bool, fn string) string {
	raw, ok := jsonArgument(v)
	var opts map[string]json.RawMessage
	if !ok || json.Unmarshal([]byte(raw), &opts) != nil {
		return "The options argument of " + fn + " must be a JSON object STRING literal"
	}
	_, fraction := opts["fraction_lists_to_search"]
	brute := false
	for k, x := range opts {
		switch k {
		case "fraction_lists_to_search":
			fl, ok := aiJSONFloat(x)
			if !ok || fl <= 0 || fl >= 1 {
				return "fraction_lists_to_search must be greater than 0.0 and less than 1.0"
			}
		case "use_brute_force":
			if json.Unmarshal(x, &brute) != nil {
				return "use_brute_force must be a JSON boolean"
			}
		case "reranker":
			if fn != "AI.SEARCH" {
				return "Unknown " + fn + " option " + k
			}
			if !rerank {
				return "options.reranker is only valid when rerank is TRUE"
			}
		default:
			return "Unknown " + fn + " option " + k
		}
	}
	if brute && fraction {
		return "fraction_lists_to_search can't be combined with use_brute_force"
	}
	return ""
}

// searchDistance is the distance_type distance between the embeddings e
// (a base row's) and v, both ARRAY<FLOAT64> expressions.
func searchDistance(distance, e, v string) string {
	dot := "(SELECT SUM(x * " + v + "[OFFSET(i)]) FROM UNNEST(" + e + ") AS x WITH OFFSET i)"
	switch distance {
	case "COSINE":
		return "(1 - " + dot + " / (SQRT((SELECT SUM(x * x) FROM UNNEST(" + e + ") AS x)) * SQRT((SELECT SUM(y * y) FROM UNNEST(" + v + ") AS y))))"
	case "DOT_PRODUCT":
		return "(-" + dot + ")"
	}
	return "SQRT((SELECT SUM(POW(x - " + v + "[OFFSET(i)], 2)) FROM UNNEST(" + e + ") AS x WITH OFFSET i))"
}

// findVectorSearch finds text's VECTOR_SEARCH calls.
func findVectorSearch(text string) ([]vectorSearchCall, int, string) {
	if !strings.Contains(strings.ToUpper(text), "VECTOR_SEARCH") {
		return nil, 0, ""
	}
	toks, ok := lex(text)
	if !ok {
		return nil, 0, ""
	}
	depth := tokenDepths(toks)
	var calls []vectorSearchCall
	for i := 0; i+1 < len(toks); i++ {
		if !toks[i].is("VECTOR_SEARCH") || !toks[i+1].punct("(") || i > 0 && toks[i-1].punct(".") {
			continue
		}
		end := matchParen(toks, i+1)
		if end < 0 {
			return nil, 400, "Syntax error: unclosed VECTOR_SEARCH("
		}
		c := vectorSearchCall{start: toks[i].pos, end: toks[end].end, named: map[string]string{}}
		var pos [][]token
		for _, a := range splitCallArgs(toks, depth, i+1, end) {
			if len(a) == 0 {
				return nil, 400, "Syntax error: an empty argument of VECTOR_SEARCH"
			}
			if len(a) > 3 && a[0].kind == tokWord && a[1].punct("=") && a[2].punct(">") {
				name := strings.ToLower(a[0].text)
				switch name {
				case "query_column_to_search", "top_k", "distance_type", "options", "query_value", "lexical_search_columns", "lexical_search_query_value":
				default:
					return nil, 400, "Unknown argument " + name + " of VECTOR_SEARCH"
				}
				if _, dup := c.named[name]; dup {
					return nil, 400, "VECTOR_SEARCH was given its " + name + " argument twice"
				}
				c.named[name] = strings.TrimSpace(text[a[3].pos:a[len(a)-1].end])
				continue
			}
			if len(c.named) > 0 {
				return nil, 400, "VECTOR_SEARCH takes its base table, column_to_search and query table first; name the others (top_k => ...)"
			}
			pos = append(pos, a)
		}
		_, single := c.named["query_value"]
		switch {
		case single && len(pos) != 2:
			return nil, 400, "VECTOR_SEARCH with query_value takes a base table and column_to_search, and no query table"
		case !single && len(pos) != 3 && len(pos) != 4:
			return nil, 400, "VECTOR_SEARCH takes a base table, column_to_search and a query table (or query_value => ...)"
		case len(pos) == 4:
			// query_column_to_search, positional as the page's examples give it.
			if _, dup := c.named["query_column_to_search"]; dup || len(pos[3]) != 1 {
				return nil, 400, "VECTOR_SEARCH was given its query_column_to_search argument twice, or not as a STRING literal"
			}
			c.named["query_column_to_search"] = text[pos[3][0].pos:pos[3][0].end]
		}
		var msg string
		if c.table, c.query, c.fromAt, msg = parseSearchBase(text, pos[0], "VECTOR_SEARCH"); msg != "" {
			return nil, 400, msg
		}
		col, ok := stringLiteral(pos[1][0])
		if len(pos[1]) != 1 || !ok {
			return nil, 400, "The column_to_search argument of VECTOR_SEARCH must be a STRING literal"
		}
		c.column = col
		if !single {
			qt := pos[2]
			switch {
			case qt[0].is("TABLE"):
				p, n := readModelPath(qt[1:])
				if n == 0 || n+1 != len(qt) {
					return nil, 400, "Syntax error: expected a table name after TABLE"
				}
				c.queryTable = p
			case qt[0].punct("(") && matchParen(qt, 0) == len(qt)-1 && len(qt) > 2 && (qt[1].is("SELECT") || qt[1].is("WITH")):
				c.querySQL = text[qt[1].pos:qt[len(qt)-2].end]
			default:
				return nil, 400, "The third argument of VECTOR_SEARCH must be TABLE query_table or (query_table_query)"
			}
		}
		calls = append(calls, c)
		i = end
	}
	return calls, 0, ""
}

// rewriteVectorSearch evaluates text's VECTOR_SEARCH calls into subqueries.
func (f front) rewriteVectorSearch(r *http.Request, q queryOptions, text string, stats *[]map[string]any) (string, int, string) {
	calls, code, msg := findVectorSearch(text)
	if code != 0 || len(calls) == 0 {
		return text, code, msg
	}
	var b strings.Builder
	last := 0
	for _, c := range calls {
		sub, code, msg := f.vectorSearchSubquery(r, q, c, stats)
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

// searchEmbedder embeds STRING query values for a base table with
// autonomous embedding generation.
func (f front) searchEmbedder(r *http.Request, project string, e *genEmbedding, fn string) (func([]promptPiece) ([]float64, int, string), int, string) {
	if e.Connection != "" {
		parts := strings.Split(e.Connection, ".")
		cn, ok := f.connections.lookup(connectionProject(parts[0], f.numbers), parts[1], parts[2])
		if !ok {
			return nil, 404, "Not found: Connection " + e.Connection
		}
		if code, msg := f.checkConnectionUse(r.Context(), parts[0], project, cn); code != 0 {
			return nil, code, msg
		}
	}
	task := e.TaskType
	if task == "RETRIEVAL_DOCUMENT" {
		task = "RETRIEVAL_QUERY"
	}
	a := aiResolved{rm: &remoteModel{Model: e.Model, Endpoint: e.Endpoint, Location: e.Location, Embedding: true}, taskType: task, dims: e.Dims}
	return func(ps []promptPiece) ([]float64, int, string) {
		// A STRING or an ObjectRef (#1983), "embedded at runtime using
		// the same connection and endpoint" as the base table's column.
		vec, status := f.embedPieces(r, project, a, ps)
		if status != "" {
			return nil, 400, fn + " could not embed its query value: " + status
		}
		return vec, 0, ""
	}, 0, ""
}

func floatArrayLiteral(vec []float64) string {
	lits := make([]string, len(vec))
	for i, x := range vec {
		lits[i] = sqlF(x)
	}
	return "ARRAY<FLOAT64>[" + strings.Join(lits, ", ") + "]"
}

func isFloatArray(ty string) bool {
	t := strings.ToUpper(strings.ReplaceAll(ty, " ", ""))
	return t == "ARRAY<FLOAT64>" || t == "ARRAY<FLOAT>"
}

// vectorSearchSubquery is c's rows as a subquery; stats gets how it used
// the table's vector index (vectorindex.go).
func (f front) vectorSearchSubquery(r *http.Request, q queryOptions, c vectorSearchCall, stats *[]map[string]any) (string, int, string) {
	lexCols, hybrid := c.named["lexical_search_columns"]
	if _, ok := c.named["lexical_search_query_value"]; ok && !hybrid {
		return "", 400, "lexical_search_query_value of VECTOR_SEARCH is only valid with lexical_search_columns"
	}
	project, dataset, table, ok := f.resolveModel(c.table, q)
	if !ok {
		return "", 400, "Table name " + strings.Join(c.table, ".") + " missing dataset while no default dataset is set in the request."
	}
	fields, meta, found := f.tableSchemaFields(r, project, dataset, table)
	if !found {
		return "", 404, "Not found: Table " + project + ":" + dataset + "." + table
	}
	if t := str(meta, "type"); t == "VIEW" || t == "MATERIALIZED_VIEW" {
		return "", 400, "The base table of VECTOR_SEARCH can't be a view: " + dataset + "." + table
	}
	// The column to search: ARRAY<FLOAT64>, or STRING with generation.
	var baseVec, indexColumn string
	var embed func([]promptPiece) ([]float64, int, string)
	colType := ""
	for _, fl := range fields {
		if strings.EqualFold(str(fl, "name"), c.column) {
			colType = restFieldType(fl)
			c.column = str(fl, "name")
		}
	}
	switch {
	case colType == "":
		return "", 400, "Column " + c.column + " not found in " + dataset + "." + table
	case isFloatArray(colType):
		baseVec, indexColumn = sqlIdent(c.column), c.column
	case strings.EqualFold(colType, "STRING") || strings.HasPrefix(strings.ToUpper(colType), "STRUCT"):
		// A STRING or ObjectRef column (#1983) with autonomous embedding
		// generation: "The table must have autonomous embedding generation
		// enabled on this column."
		e := f.tables.get(streamKey(project, dataset, table), str(meta, "creationTime")).Embedding
		if e == nil || !strings.EqualFold(e.Source, c.column) || e.SourceRef != strings.HasPrefix(strings.ToUpper(colType), "STRUCT") {
			return "", 400, "The " + map[bool]string{true: "ObjectRef", false: "STRING"}[strings.HasPrefix(strings.ToUpper(colType), "STRUCT")] +
				" column_to_search " + c.column + " of VECTOR_SEARCH must have autonomous embedding generation enabled on " + dataset + "." + table
		}
		baseVec, indexColumn = sqlIdent(e.Column)+".result", e.Column
		var code int
		var msg string
		if embed, code, msg = f.searchEmbedder(r, project, e, "VECTOR_SEARCH"); code != 0 {
			return "", code, msg
		}
	default:
		return "", 400, "The column_to_search " + c.column + " of VECTOR_SEARCH must be ARRAY<FLOAT64>, or a STRING or ObjectRef column with autonomous embedding generation; it is " + colType
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
	v, distanceGiven := c.named["distance_type"]
	if distanceGiven {
		s, ok := unquoteString(v)
		distance = strings.ToUpper(s)
		if !ok || distance != "EUCLIDEAN" && distance != "COSINE" && distance != "DOT_PRODUCT" {
			return "", 400, "distance_type must be 'EUCLIDEAN', 'COSINE' or 'DOT_PRODUCT'"
		}
	}
	if v, ok := c.named["options"]; ok {
		if msg := checkSearchOptions(v, false, "VECTOR_SEARCH"); msg != "" {
			return "", 400, msg
		}
	}
	var lexical *lexicalSearch
	if hybrid {
		if _, single := c.named["query_value"]; !single {
			return "", 400, "lexical_search_columns of VECTOR_SEARCH can be used only with query_value: hybrid search doesn't support batch queries"
		}
		lv, ok := c.named["lexical_search_query_value"]
		if !ok {
			return "", 400, "lexical_search_query_value of VECTOR_SEARCH must be given with lexical_search_columns"
		}
		var code int
		var msg string
		if lexical, code, msg = parseLexicalSearch(q, fields, lexCols, lv, "VECTOR_SEARCH"); code != 0 {
			return "", code, msg
		}
	}
	use, code, msg := f.searchIndex(r, project, dataset, table, fields, meta, indexColumn, c.named["options"])
	if code != 0 {
		return "", code, msg
	}
	*stats = append(*stats, use.stats())
	if use.index != nil && !distanceGiven {
		distance = use.index.Distance
	}
	// The base rows, with the embedding beside them; with an index, a
	// WHERE on columns it does not store is a post-filter (_cb_vs_keep).
	ref := quotePath([]string{project, dataset, table})
	baseSQL := "SELECT *, " + baseVec + " AS _cb_vs_vec FROM " + ref
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
		baseSQL = c.query[:c.fromAt] + ", " + baseVec + " AS _cb_vs_vec " + c.query[c.fromAt:]
		if wcols, where, ok := whereColumns(c.query, fields); ok && where > 0 && use.index != nil && !use.index.preFilter(wcols) {
			post = true
			baseSQL = c.query[:c.fromAt] + ", " + baseVec + " AS _cb_vs_vec, IFNULL((" + c.query[where+len("WHERE"):] + "), FALSE) AS _cb_vs_keep " +
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
	var baseCols []string
	for _, n := range names {
		baseCols = append(baseCols, "b."+sqlIdent(n)+" AS "+sqlIdent(n))
	}
	bvec := "b._cb_vs_vec"
	scored := func(qv string) string { return qv }
	if use.index != nil && use.index.Norm == "L2" {
		bvec = normalizedSQL(bvec)
		scored = normalizedSQL
	}
	present := "b._cb_vs_vec IS NOT NULL AND ARRAY_LENGTH(b._cb_vs_vec) > 0"
	mismatch := "ERROR('VECTOR_SEARCH: the query and base embeddings have different dimensions')"
	if v, single := c.named["query_value"]; single {
		var qv string
		var str *string
		var valueRef *objRef
		switch {
		case strings.HasPrefix(v, "@"):
			s, isString := queryParameterString(q, v)
			if isString {
				str = s
				if s == nil {
					return "", 400, "The query_value of VECTOR_SEARCH can't be NULL"
				}
			} else {
				qv = v
			}
		default:
			if s, ok := unquoteString(v); ok {
				str = &s
			} else if embed != nil {
				// An expression: an ObjectRef or STRING to embed (#1983),
				// or a vector.
				ps, isVec, code, msg := f.searchValuePieces(r, q, v)
				switch {
				case code != 0:
					return "", code, msg
				case isVec:
					qv = v
				case ps == nil:
					return "", 400, "The query_value of VECTOR_SEARCH can't be NULL"
				case ps[0].ref != nil:
					valueRef = ps[0].ref
				default:
					str = &ps[0].text
				}
			} else {
				qv = v
			}
		}
		if str != nil || valueRef != nil {
			if embed == nil {
				return "", 400, "A STRING query_value of VECTOR_SEARCH needs a STRING column_to_search with autonomous embedding generation; " + c.column + " is " + colType
			}
			ps := []promptPiece{{ref: valueRef}}
			if str != nil {
				ps = []promptPiece{{text: *str}}
			}
			vec, code, msg := embed(ps)
			if code != 0 {
				return "", code, msg
			}
			qv = floatArrayLiteral(vec)
		}
		paramRef := ""
		if strings.HasPrefix(v, "@") && str != nil {
			paramRef = " AND (" + v + " IS NULL OR TRUE)" // the engine takes only the parameters a query names
		}
		dist := "IF(ARRAY_LENGTH(b._cb_vs_vec) != ARRAY_LENGTH(" + qv + "), " + mismatch + ", " + searchDistance(distance, bvec, scored(qv)) + ")"
		from := "(" + baseSQL + ") AS b"
		cand := ""
		if use.index != nil {
			from += " CROSS JOIN (SELECT " + use.probeSQL(qv) + " AS _cb_vs_probe) AS _cb_p"
			cand = "(b._cb_vs_list IS NULL OR b._cb_vs_list IN UNNEST(_cb_p._cb_vs_probe))"
		}
		keep := ""
		if post {
			keep = ", b._cb_vs_keep AS _cb_vs_keep"
		}
		if lexical != nil {
			return lexical.hybridSQL(from, present+paramRef, cand, dist, baseCols, keep, "", post, topK), 0, ""
		}
		where := present + paramRef
		if cand != "" {
			where += " AND " + cand
		}
		sub := "(SELECT base, distance FROM (SELECT STRUCT(" + strings.Join(baseCols, ", ") + ") AS base, " + dist + " AS distance" + keep + " FROM " + from + " " +
			"WHERE " + where + " ORDER BY distance"
		if topK >= 0 {
			sub += " LIMIT " + strconv.FormatInt(topK, 10)
		}
		sub += ")"
		if post {
			sub += " WHERE _cb_vs_keep"
		}
		return sub + ")", 0, ""
	}
	// The batch form: the query table's rows, each with its embedding.
	qsql := c.querySQL
	if qsql == "" {
		qp, qd, qt, ok := f.resolveModel(c.queryTable, q)
		if !ok {
			return "", 400, "Table name " + strings.Join(c.queryTable, ".") + " missing dataset while no default dataset is set in the request."
		}
		qsql = "SELECT * FROM " + quotePath([]string{qp, qd, qt})
	}
	qcols, _, code, msg := f.queryAllRows(r, probeOptions(q, qsql), "SELECT * FROM ("+qsql+") LIMIT 0")
	if code != 0 {
		return "", code, msg
	}
	isRefType := func(ty string) bool {
		return strings.EqualFold(ty, "RECORD") || strings.HasPrefix(strings.ToUpper(ty), "STRUCT")
	}
	want := func(ty string) bool {
		return isFloatArray(ty) || embed != nil && (strings.EqualFold(ty, "STRING") || isRefType(ty))
	}
	qcol, qtype := "", ""
	if v, ok := c.named["query_column_to_search"]; ok {
		s, ok := unquoteString(v)
		if !ok {
			return "", 400, "query_column_to_search of VECTOR_SEARCH must be a STRING literal"
		}
		for _, col := range qcols {
			if strings.EqualFold(col.Name, s) {
				qcol, qtype = col.Name, col.Type
			}
		}
		if qcol == "" {
			return "", 400, "Column " + s + " not found in the query table of VECTOR_SEARCH"
		}
	} else {
		for _, col := range qcols {
			if strings.EqualFold(col.Name, c.column) {
				qcol, qtype = col.Name, col.Type
			}
		}
		if qcol == "" {
			var fits []mlColumn
			for _, col := range qcols {
				if want(col.Type) {
					fits = append(fits, col)
				}
			}
			if len(fits) != 1 {
				return "", 400, "The query table of VECTOR_SEARCH has no column " + c.column + "; name its column with query_column_to_search"
			}
			qcol, qtype = fits[0].Name, fits[0].Type
		}
	}
	if !want(qtype) {
		return "", 400, "The query column " + qcol + " of VECTOR_SEARCH must be ARRAY<FLOAT64>" + map[bool]string{true: ", STRING or ObjectRef", false: ""}[embed != nil] + "; it is " + qtype
	}
	qvec := "q0." + sqlIdent(qcol)
	if strings.EqualFold(qtype, "STRING") || isRefType(qtype) {
		// Each distinct query string, or ObjectRef (#1983, read as its
		// TO_JSON_STRING), is embedded here, and joined back.
		key := "q0." + sqlIdent(qcol)
		if isRefType(qtype) {
			key = "TO_JSON_STRING(" + key + ")"
		}
		_, rows, code, msg := f.queryAllRows(r, probeOptions(q, qsql), "SELECT DISTINCT "+key+" FROM ("+qsql+") AS q0")
		if code != 0 {
			return "", code, msg
		}
		var pairs []string
		for _, row := range rows {
			if len(row) == 0 || row[0] == nil || isRefType(qtype) && *row[0] == "null" {
				return "", 400, "VECTOR_SEARCH could not embed its query value: " + embedNullStatus
			}
			ps := []promptPiece{{text: *row[0]}}
			if isRefType(qtype) {
				o, ok := parseObjRef([]byte(*row[0]))
				if !ok {
					return "", 400, "The query column " + qcol + " of VECTOR_SEARCH must be ARRAY<FLOAT64>, STRING or ObjectRef; it is a STRUCT that is not an ObjectRef"
				}
				ps = []promptPiece{{ref: &o}}
			}
			vec, code, msg := embed(ps)
			if code != 0 {
				return "", code, msg
			}
			pairs = append(pairs, "STRUCT("+sqlString(*row[0])+" AS s, "+floatArrayLiteral(vec)+" AS e)")
		}
		qvec = "(SELECT m.e FROM UNNEST(ARRAY<STRUCT<s STRING, e ARRAY<FLOAT64>>>[" + strings.Join(pairs, ", ") + "]) AS m WHERE m.s = " + key + ")"
	}
	var queryCols []string
	for _, col := range qcols {
		queryCols = append(queryCols, "q."+sqlIdent(col.Name)+" AS "+sqlIdent(col.Name))
	}
	// The query vector, normalized for an L2 index's search, is a column
	// of the query rows, which their ROW_NUMBER keeps the engine from
	// folding into the scoring: an expression there is evaluated again
	// at each element of each base row's vector (measured, #2042: a
	// TREE_AH search of 11,000 rows took 39 s, its distances 14 times).
	qn := ""
	if use.index != nil && use.index.Norm == "L2" {
		qn = ", " + normalizedSQL(qvec) + " AS _cb_vs_qn"
		scored = func(string) string { return "q._cb_vs_qn" }
	}
	qrows := "SELECT q0.*, " + qvec + " AS _cb_vs_qvec" + qn + ", ROW_NUMBER() OVER () AS _cb_vs_qid FROM (" + qsql + ") AS q0"
	cand := ""
	if use.index != nil {
		probes, code, msg := f.batchProbes(r, probeOptions(q, qsql), "SELECT "+qvec+" AS v FROM ("+qsql+") AS q0", use)
		if code != 0 {
			return "", code, msg
		}
		qrows = "SELECT q1.*, " + probes + " AS _cb_vs_probe FROM (" + qrows + ") AS q1"
		cand = " AND (b._cb_vs_list IS NULL OR b._cb_vs_list IN UNNEST(q._cb_vs_probe))"
	}
	dist := "IF(ARRAY_LENGTH(b._cb_vs_vec) != ARRAY_LENGTH(q._cb_vs_qvec), " + mismatch + ", " + searchDistance(distance, bvec, scored("q._cb_vs_qvec")) + ")"
	keep := ""
	if post {
		keep = ", b._cb_vs_keep AS _cb_vs_keep"
	}
	// Each pair's distance is computed once, and ranked a level up.
	carry := ""
	if post {
		carry = ", _cb_vs_keep"
	}
	inner := "SELECT query, base, distance, ROW_NUMBER() OVER (PARTITION BY _cb_vs_qid ORDER BY distance) AS _cb_vs_rank" + carry + " FROM (" +
		"SELECT STRUCT(" + strings.Join(queryCols, ", ") + ") AS query, STRUCT(" + strings.Join(baseCols, ", ") + ") AS base, " + dist + " AS distance, " +
		"q._cb_vs_qid AS _cb_vs_qid" + keep + " " +
		"FROM (" + qrows + ") AS q " +
		"CROSS JOIN (" + baseSQL + ") AS b WHERE " + present + cand + ")"
	sub := "(SELECT query, base, distance FROM (" + inner + ")"
	var conds []string
	if topK >= 0 {
		conds = append(conds, "_cb_vs_rank <= "+strconv.FormatInt(topK, 10))
	}
	if post {
		conds = append(conds, "_cb_vs_keep")
	}
	if len(conds) > 0 {
		sub += " WHERE " + strings.Join(conds, " AND ")
	}
	return sub + ")", 0, ""
}

// probeOptions is q for a probe of sql, without q's parameters when sql
// names none (the engine takes only the parameters a query names).
func probeOptions(q queryOptions, sql string) queryOptions {
	if !strings.Contains(sql, "@") && !strings.Contains(sql, "?") {
		q.QueryParameters, q.ParameterMode = nil, ""
	}
	return q
}

// vectorIndexStatement is the 501 of a CREATE, ALTER or DROP VECTOR INDEX
// statement in a script (text), or "": the front carries out one that is
// a query of its own (vectorindex.go, #1952), and a script's would reach
// the engine, which has none.
func vectorIndexStatement(text string) string {
	u := strings.ToUpper(text)
	if !strings.Contains(u, "VECTOR") || !strings.Contains(u, "INDEX") {
		return ""
	}
	toks, ok := lex(text)
	if !ok {
		return ""
	}
	for i := 0; i+2 < len(toks); i++ {
		if !(toks[i].is("CREATE") || toks[i].is("ALTER") || toks[i].is("DROP")) || i > 0 && !toks[i-1].punct(";") {
			continue
		}
		j := i + 1
		if toks[i].is("CREATE") && j+1 < len(toks) && toks[j].is("OR") && toks[j+1].is("REPLACE") {
			j += 2
		}
		if j+1 < len(toks) && toks[j].is("VECTOR") && toks[j+1].is("INDEX") {
			return "Not implemented here: " + strings.ToUpper(toks[i].text) + " VECTOR INDEX in a multi-statement query; run it as a query of its own. Nothing was run."
		}
	}
	return ""
}

// searchValuePieces evaluates a search's query value expression that is
// neither a literal nor a query parameter (#1983): its one piece, a STRING
// or an ObjectRef (OBJ.MAKE_REF's, already SQL), nil for a NULL; isVec
// when it is neither, such as an ARRAY<FLOAT64>, which the caller reads
// itself.
func (f front) searchValuePieces(r *http.Request, q queryOptions, expr string) (ps []promptPiece, isVec bool, code int, msg string) {
	_, rows, code, msg := f.queryAllRows(r, probeOptions(q, expr), "SELECT TO_JSON_STRING("+expr+")")
	if code != 0 {
		return nil, false, code, msg
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] == nil {
		return nil, false, 0, ""
	}
	raw := json.RawMessage(*rows[0][0])
	ps, ok, isStruct := contentPieces(raw)
	switch {
	case ok:
		return ps, false, 0, ""
	case isStruct:
		return nil, false, 400, "A STRUCT query value is not an ObjectRef: an ObjectRef is STRUCT<uri STRING, version STRING, authorizer STRING, details JSON>"
	case string(bytes.TrimSpace(raw)) == "null":
		return nil, false, 0, ""
	}
	return nil, true, 0, ""
}
