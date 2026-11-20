package bigqueryfront

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Vector indexes (#1952), as Google documents them (read 2026-11-19):
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_vector_index_statement,
// #alter_vector_index_rebuild_statement and #drop_vector_index_statement,
// https://docs.cloud.google.com/bigquery/docs/vector-index and the
// INFORMATION_SCHEMA.VECTOR_INDEXES, VECTOR_INDEX_COLUMNS and
// VECTOR_INDEX_OPTIONS pages.
//
//	CREATE [OR REPLACE] VECTOR INDEX [IF NOT EXISTS] index_name
//	  ON table_name(column_name) [STORING(stored_column_name, ...)]
//	  OPTIONS(index_type = 'IVF' | 'TREE_AH', distance_type = ...,
//	    ivf_options = '{"num_lists": N}', tree_ah_options = '{...}',
//	    lexical_search_columns = [...])
//	ALTER VECTOR INDEX [IF EXISTS] index_name ON table_name REBUILD
//	DROP VECTOR INDEX [IF EXISTS] index_name ON table_name
//
// The index is a real inverted file index, trained locally: "An IVF uses a
// k-means algorithm to cluster the vector data, and then partitions the
// vector data based on those clusters". The front trains the centroids
// with k-means (Lloyd's algorithm from k-means++ seeds, always EUCLIDEAN,
// as "The index creation itself always uses EUCLIDEAN distance for
// training") and assigns every embedding to its nearest centroid's list.
// The centroids and the lists are kept as two tables of the front's own
// dataset (resultsDataset), the lists keyed by
// FARM_FINGERPRINT(TO_JSON_STRING(embedding)). An indexed VECTOR_SEARCH
// or AI.SEARCH probes the fraction_lists_to_search nearest lists of each
// query vector and ranks only the rows of those lists, so its results are
// approximate as BigQuery's are; a row the index has not assigned yet is
// searched as brute force would ("unindexed" rows). TREE_AH is the same
// partitioning with about leaf_node_embedding_count vectors in each list
// (normalization_type L2 normalizes base and query vectors first, the
// candidates scored on the normalized vectors); it does not quantize the
// vectors, it scores every candidate exactly, which BigQuery does in its
// re-ranking step.
//
// An index of a table smaller than 10 MB (its documented logical bytes,
// logicalbytes.go) is not populated: index_status TEMPORARILY DISABLED,
// coverage_percentage 0, and searches run brute force, reporting
// BASE_TABLE_TOO_SMALL in statistics.query.vectorSearchStatistics. The
// front checks the table whenever it reads the index (a search, an
// INFORMATION_SCHEMA read): it trains the index once the table reaches
// 10 MB, and assigns changed rows to the trained lists (BigQuery's
// background refresh, done here before the read), so coverage_percentage
// is 100 once populated and last_refresh_time the last assignment.
//
// UNVERIFIED (re-read 2026-11-19, the pages above, which quote no error text): the refusals' words; the
// default num_lists (the front takes ceil(sqrt(n)), at most 5,000) and the
// default fraction_lists_to_search (the front probes 10% of the lists, at
// least one: "an appropriate value is picked"); disable_reason's text.
// ALTER VECTOR INDEX REBUILD retrains the index at once: BigQuery's
// needs a BACKGROUND reservation and swaps a shadow index in
// asynchronously, which an instance without reservations does not model.
// PARTITION BY (TreeAH Preview) is 501, named.

// vectorIndexMinBytes is the table size below which an index is not
// populated: "If you create a vector index on a table that is smaller than
// 10 MB, then the vector index isn't populated."
const vectorIndexMinBytes = 10 << 20

// vectorIndexDefaultFraction is the fraction of lists probed when
// fraction_lists_to_search is not given (UNVERIFIED, re-read 2026-11-19:
// https://docs.cloud.google.com/bigquery/docs/vector-index, above).
const vectorIndexDefaultFraction = 0.1

// vectorIndex is one table's vector index, kept in its tableSetting.
type vectorIndex struct {
	Name     string              `json:"name"`
	Column   string              `json:"column"`
	Embedded bool                `json:"embedded,omitempty"` // a generated STRUCT<result, status> column
	Stored   []string            `json:"stored,omitempty"`
	Type     string              `json:"type"`
	Distance string              `json:"distance"`
	NumLists int                 `json:"numLists,omitempty"`
	Leaf     int                 `json:"leaf,omitempty"`
	Norm     string              `json:"norm,omitempty"`
	Lexical  []string            `json:"lexical,omitempty"`
	DDL      string              `json:"ddl"`
	Options  []vectorIndexOption `json:"options,omitempty"`
	Created  int64               `json:"created"`
	Modified int64               `json:"modified"`
	// Refreshed is when the rows were last assigned to the lists, 0 while
	// the index is not populated; Built when the centroids were trained.
	Refreshed int64  `json:"refreshed,omitempty"`
	Built     int64  `json:"built,omitempty"`
	Disabled  int64  `json:"disabled,omitempty"`
	Lists     int    `json:"lists,omitempty"`
	Dims      int    `json:"dims,omitempty"`
	Indexed   int64  `json:"indexed,omitempty"`
	State     string `json:"state,omitempty"` // the table's rows as last assigned
	Bytes     int64  `json:"bytes,omitempty"` // the index tables' logical bytes
	Tables    string `json:"tables"`          // the index tables' name prefix
	// Alteration is the last ALTER VECTOR INDEX REBUILD.
	Alteration *vectorIndexAlteration `json:"alteration,omitempty"`
}

type vectorIndexOption struct {
	Name, Type, Value string
}

type vectorIndexAlteration struct {
	Start, End int64
	Coverage   int64
	DDL        string
}

// vecSQL is the SQL of a base row's embedding (an ARRAY<FLOAT64>).
func (x *vectorIndex) vecSQL(alias string) string {
	col := sqlIdent(x.Column)
	if alias != "" {
		col = alias + "." + col
	}
	if x.Embedded {
		return col + ".result"
	}
	return col
}

func (x *vectorIndex) listsTable() string {
	return quotePath([]string{resultsDataset, x.Tables + "_lists"})
}

func (x *vectorIndex) centroidsTable() string {
	return quotePath([]string{resultsDataset, x.Tables + "_centroids"})
}

// populated reports whether searches can use x.
func (x *vectorIndex) populated() bool { return x != nil && x.Refreshed > 0 && x.Lists > 0 }

// setVectorIndex keeps x (nil drops it) for the table made at created.
func (s *tableSettings) setVectorIndex(key, created string, x *vectorIndex) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]tableSetting{}
	}
	t := s.m[key]
	if t.Created != created {
		t = tableSetting{Created: created}
	}
	t.VectorIndex = x
	s.m[key] = t
	s.saveLocked()
}

// vectorIndexStmt is a parsed vector index statement.
type vectorIndexStmt struct {
	kind              string // CREATE, ALTER or DROP
	replace, ifClause bool
	name              string
	table             []string
	column            string
	stored            []string
	options           []alterOption
	text              string
}

// parseVectorIndexStatement reads text as one vector index statement; ok
// is false when it is none. msg is a syntax error (400), or, with code
// 501, what is not served.
func parseVectorIndexStatement(text string) (st vectorIndexStmt, code int, msg string, ok bool) {
	u := strings.ToUpper(text)
	if !strings.Contains(u, "VECTOR") || !strings.Contains(u, "INDEX") {
		return st, 0, "", false
	}
	toks, lexed := lex(text)
	if !lexed || len(toks) < 3 {
		return st, 0, "", false
	}
	for len(toks) > 0 && toks[len(toks)-1].punct(";") {
		toks = toks[:len(toks)-1]
	}
	i := 0
	if !(toks[0].is("CREATE") || toks[0].is("ALTER") || toks[0].is("DROP")) {
		return st, 0, "", false
	}
	st.kind = strings.ToUpper(toks[0].text)
	st.text = strings.TrimSpace(text)
	i = 1
	if st.kind == "CREATE" && i+1 < len(toks) && toks[i].is("OR") && toks[i+1].is("REPLACE") {
		st.replace = true
		i += 2
	}
	if i+1 >= len(toks) || !toks[i].is("VECTOR") || !toks[i+1].is("INDEX") {
		return st, 0, "", false
	}
	for _, t := range toks {
		if t.punct(";") {
			return st, 501, "Not implemented here: a VECTOR INDEX statement in a multi-statement query; run it as a statement of its own. Nothing was run.", true
		}
	}
	i += 2
	syntax := func(what string) (vectorIndexStmt, int, string, bool) {
		return st, 400, "Syntax error: " + what, true
	}
	switch {
	case st.kind == "CREATE" && i+2 < len(toks) && toks[i].is("IF") && toks[i+1].is("NOT") && toks[i+2].is("EXISTS"):
		st.ifClause = true
		i += 3
		if st.replace {
			return syntax("OR REPLACE and IF NOT EXISTS cannot both be given to CREATE VECTOR INDEX")
		}
	case st.kind != "CREATE" && i+1 < len(toks) && toks[i].is("IF") && toks[i+1].is("EXISTS"):
		st.ifClause = true
		i += 2
	}
	if i >= len(toks) || !isName(toks[i]) {
		return syntax("expected an index name")
	}
	st.name = toks[i].text
	if strings.Contains(st.name, ".") || i+1 < len(toks) && toks[i+1].punct(".") {
		return syntax("the index name of a vector index takes no project or dataset: it is always the table's")
	}
	i++
	if i >= len(toks) || !toks[i].is("ON") {
		return syntax("expected ON table_name")
	}
	i++
	p, n := readModelPath(toks[i:])
	if n == 0 {
		return syntax("expected a table name after ON")
	}
	st.table = p
	i += n
	switch st.kind {
	case "DROP":
		if i != len(toks) {
			return syntax("unexpected " + toks[i].text + " after DROP VECTOR INDEX ... ON table_name")
		}
		return st, 0, "", true
	case "ALTER":
		if i != len(toks)-1 || !toks[i].is("REBUILD") {
			return syntax("ALTER VECTOR INDEX takes ON table_name REBUILD")
		}
		return st, 0, "", true
	}
	if i >= len(toks) || !toks[i].punct("(") {
		return syntax("expected (column_name) after the table name")
	}
	end := matchParen(toks, i)
	if end != i+2 || !isName(toks[i+1]) {
		return syntax("a vector index is on one column: ON table_name(column_name)")
	}
	st.column = toks[i+1].text
	i = end + 1
	if i < len(toks) && toks[i].is("STORING") {
		if i+1 >= len(toks) || !toks[i+1].punct("(") {
			return syntax("expected STORING(column, ...)")
		}
		e := matchParen(toks, i+1)
		if e < 0 {
			return syntax("unclosed STORING(")
		}
		for j := i + 2; j < e; j++ {
			if toks[j].punct(",") {
				continue
			}
			if !isName(toks[j]) || j+1 < e && !toks[j+1].punct(",") {
				return syntax("STORING takes top-level column names")
			}
			st.stored = append(st.stored, toks[j].text)
		}
		i = e + 1
	}
	if i < len(toks) && toks[i].is("PARTITION") {
		return st, 501, "Not implemented here: CREATE VECTOR INDEX ... PARTITION BY (a partitioned TreeAH index, Preview). Nothing was run.", true
	}
	if i >= len(toks) {
		return st, 400, "CREATE VECTOR INDEX requires OPTIONS(index_type = 'IVF' | 'TREE_AH')", true
	}
	opts, ok2 := readOptions(toks[i:])
	if !ok2 {
		return syntax("expected OPTIONS(name = value, ...) at " + toks[i].text)
	}
	st.options = opts
	return st, 0, "", true
}

// serveVectorIndexStatement carries out st.
func (f front) serveVectorIndexStatement(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, st vectorIndexStmt) {
	failed := func(code int, msg string) {
		reason := map[int]string{400: "invalidQuery", 403: "accessDenied", 404: "notFound", 409: "duplicate"}[code]
		if code >= 500 {
			writeError(w, code, map[int]string{500: "internalError", 501: "notImplemented"}[code], msg)
			return
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: msg})
	}
	project, dataset, table, ok := f.resolveModel(st.table, q)
	if !ok {
		failed(400, "Table name "+strings.Join(st.table, ".")+" missing dataset while no default dataset is set in the request.")
		return
	}
	if project != projectOf(f.base) {
		failed(501, "Not implemented here: a vector index on another project's table ("+project+"); run the statement in the table's project. Nothing was run.")
		return
	}
	fields, meta, found := f.tableSchemaFields(r, project, dataset, table)
	if !found {
		failed(404, "Not found: Table "+project+":"+dataset+"."+table)
		return
	}
	key := streamKey(project, dataset, table)
	created := str(meta, "creationTime")
	cur := f.tables.get(key, created).VectorIndex
	done := func(op string) {
		if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{"_cloudburrow", scratchTable()})) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
		f.forward(w, r, jobText{query: q.Query, ddl: &ownDDL{target: tableRef{ProjectID: project, DatasetID: dataset, TableID: table},
			statementType: st.kind + "_VECTOR_INDEX", operation: op}})
	}
	switch st.kind {
	case "DROP":
		switch {
		case cur == nil && st.ifClause:
			done("SKIP")
		case cur == nil || !strings.EqualFold(cur.Name, st.name):
			failed(404, "Not found: Index "+st.name+" on table "+project+":"+dataset+"."+table)
		default:
			f.dropVectorIndexTables(r, cur)
			f.tables.setVectorIndex(key, created, nil)
			done("DROP")
		}
		return
	case "ALTER":
		switch {
		case cur == nil && st.ifClause:
			done("SKIP")
		case cur == nil || !strings.EqualFold(cur.Name, st.name):
			failed(404, "Not found: Index "+st.name+" on table "+project+":"+dataset+"."+table)
		default:
			x := *cur
			now := f.nowMillis()
			x.Refreshed, x.Lists, x.State = 0, 0, ""
			if code, msg := f.populateVectorIndex(r, project, dataset, table, fields, &x, true); code != 0 {
				failed(code, msg)
				return
			}
			x.Modified = now
			x.Alteration = &vectorIndexAlteration{Start: now, Coverage: x.coverage(), DDL: st.text}
			f.tables.setVectorIndex(key, created, &x)
			done("ALTER")
		}
		return
	}
	// CREATE.
	if t := str(meta, "type"); t != "TABLE" {
		failed(400, "A vector index can only be created on a standard table; "+dataset+"."+table+" is a "+strings.ReplaceAll(t, "_", " "))
		return
	}
	if cur != nil {
		switch {
		case !strings.EqualFold(cur.Name, st.name):
			failed(400, "Table "+project+":"+dataset+"."+table+" already has a vector index, "+cur.Name+"; a table can have only one vector index")
			return
		case st.ifClause:
			done("SKIP")
			return
		case !st.replace:
			failed(409, "Already Exists: Index "+st.name+" on table "+project+":"+dataset+"."+table)
			return
		}
	}
	x, code, msg := f.newVectorIndex(r, st, project, dataset, table, fields, meta)
	if code != 0 {
		failed(code, msg)
		return
	}
	if code, msg := f.populateVectorIndex(r, project, dataset, table, fields, x, true); code != 0 {
		failed(code, msg)
		return
	}
	if cur != nil && cur.Tables != x.Tables {
		f.dropVectorIndexTables(r, cur)
	}
	f.tables.setVectorIndex(key, created, x)
	f.sweepVectorIndexTables(r)
	if cur != nil {
		done("REPLACE")
		return
	}
	done("CREATE")
}

// newVectorIndex checks st's column and options against the table.
func (f front) newVectorIndex(r *http.Request, st vectorIndexStmt, project, dataset, table string, fields []map[string]any, meta map[string]any) (*vectorIndex, int, string) {
	byName := map[string]map[string]any{}
	for _, fl := range fields {
		byName[strings.ToLower(str(fl, "name"))] = fl
	}
	x := &vectorIndex{Name: st.name, Distance: "EUCLIDEAN", Norm: "NONE"}
	fl := byName[strings.ToLower(st.column)]
	if fl == nil {
		return nil, 400, "Column " + st.column + " not found in table " + dataset + "." + table
	}
	x.Column = str(fl, "name")
	switch ty := strings.ReplaceAll(restFieldType(fl), " ", ""); {
	case isFloatArray(ty):
	case ty == "STRUCT<RESULTARRAY<FLOAT64>,STATUSSTRING>":
		x.Embedded = true
	default:
		return nil, 400, "The column " + x.Column + " of a vector index must be ARRAY<FLOAT64>, or a STRUCT<result ARRAY<FLOAT64>, status STRING> generated embedding column; it is " + restFieldType(fl)
	}
	seen := map[string]bool{}
	for _, s := range st.stored {
		sf := byName[strings.ToLower(s)]
		if sf == nil {
			return nil, 400, "Stored column " + s + " not found in table " + dataset + "." + table
		}
		if seen[strings.ToLower(s)] {
			return nil, 400, "Stored column " + s + " is given twice"
		}
		seen[strings.ToLower(s)] = true
		if strings.EqualFold(str(sf, "type"), "RANGE") {
			return nil, 400, "Stored column " + s + " can't be of type RANGE"
		}
		x.Stored = append(x.Stored, str(sf, "name"))
	}
	var ivf, tree string
	for _, o := range st.options {
		if len(o.val) == 0 {
			return nil, 400, "Syntax error: OPTIONS " + o.name + " has no value"
		}
		val := strings.TrimSpace(st.text[o.val[0].pos:o.val[len(o.val)-1].end])
		lit := func() (string, bool) {
			if len(o.val) != 1 {
				return "", false
			}
			return stringLiteral(o.val[0])
		}
		switch o.name {
		case "index_type":
			s, ok := lit()
			s = strings.ToUpper(s)
			if !ok || s != "IVF" && s != "TREE_AH" {
				return nil, 400, "index_type must be 'IVF' or 'TREE_AH'"
			}
			x.Type = s
			x.Options = append(x.Options, vectorIndexOption{"index_type", "STRING", strconv.Quote(s)})
		case "distance_type":
			s, ok := lit()
			s = strings.ToUpper(s)
			if !ok || s != "EUCLIDEAN" && s != "COSINE" && s != "DOT_PRODUCT" {
				return nil, 400, "distance_type must be 'EUCLIDEAN', 'COSINE' or 'DOT_PRODUCT'"
			}
			x.Distance = s
			x.Options = append(x.Options, vectorIndexOption{"distance_type", "STRING", strconv.Quote(s)})
		case "ivf_options", "tree_ah_options":
			s, ok := lit()
			var m map[string]json.RawMessage
			if !ok || json.Unmarshal([]byte(s), &m) != nil {
				return nil, 400, o.name + " must be a JSON object STRING"
			}
			if o.name == "ivf_options" {
				ivf = s
			} else {
				tree = s
			}
			x.Options = append(x.Options, vectorIndexOption{o.name, "STRING", strconv.Quote(s)})
		case "lexical_search_columns":
			if !o.val[0].punct("[") || !o.val[len(o.val)-1].punct("]") {
				return nil, 400, "lexical_search_columns must be an ARRAY of column names"
			}
			var names []string
			lex := map[string]bool{}
			for _, t := range o.val[1 : len(o.val)-1] {
				if t.punct(",") {
					continue
				}
				s, ok := stringLiteral(t)
				if !ok {
					return nil, 400, "lexical_search_columns must be an ARRAY of STRING column names"
				}
				lf := byName[strings.ToLower(s)]
				if lf == nil || !strings.EqualFold(str(lf, "type"), "STRING") || str(lf, "mode") == "REPEATED" {
					return nil, 400, "lexical_search_columns: " + s + " must be a STRING column of " + dataset + "." + table
				}
				if lex[strings.ToLower(s)] {
					return nil, 400, "lexical_search_columns must be unique: " + s + " is given twice"
				}
				lex[strings.ToLower(s)] = true
				x.Lexical = append(x.Lexical, str(lf, "name"))
				names = append(names, strconv.Quote(str(lf, "name")))
			}
			x.Options = append(x.Options, vectorIndexOption{"lexical_search_columns", "ARRAY<STRING>", "[" + strings.Join(names, ", ") + "]"})
		default:
			return nil, 400, "Unknown vector index option " + o.name + " (" + val + ")"
		}
	}
	if x.Type == "" {
		return nil, 400, "CREATE VECTOR INDEX requires the index_type option: 'IVF' or 'TREE_AH'"
	}
	if ivf != "" && x.Type != "IVF" {
		return nil, 400, "ivf_options can only be given with index_type 'IVF'"
	}
	if tree != "" && x.Type != "TREE_AH" {
		return nil, 400, "tree_ah_options can only be given with index_type 'TREE_AH'"
	}
	if ivf != "" {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(ivf), &m)
		for k, v := range m {
			if k != "num_lists" {
				return nil, 400, "Unknown ivf_options option " + k + ": the only supported option is num_lists"
			}
			var n int64
			if json.Unmarshal(v, &n) != nil || n < 1 || n > 5000 {
				return nil, 400, "ivf_options num_lists must be an INT64 from 1 to 5,000"
			}
			x.NumLists = int(n)
		}
	}
	if x.Type == "TREE_AH" {
		x.Leaf = 1000
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(tree), &m)
		for k, v := range m {
			switch k {
			case "leaf_node_embedding_count":
				var n int64
				if json.Unmarshal(v, &n) != nil || n < 500 {
					return nil, 400, "tree_ah_options leaf_node_embedding_count must be an INT64 of at least 500"
				}
				x.Leaf = int(n)
			case "normalization_type":
				var s string
				if json.Unmarshal(v, &s) != nil || strings.ToUpper(s) != "NONE" && strings.ToUpper(s) != "L2" {
					return nil, 400, "tree_ah_options normalization_type must be 'NONE' or 'L2'"
				}
				x.Norm = strings.ToUpper(s)
			default:
				return nil, 400, "Unknown tree_ah_options option " + k + ": leaf_node_embedding_count and normalization_type are supported"
			}
		}
	}
	now := f.nowMillis()
	x.Created, x.Modified = now, now
	sum := sha256.Sum256([]byte(streamKey(project, dataset, table) + "\x00" + str(meta, "creationTime") + "\x00" + x.Name + "\x00" + strconv.FormatInt(now, 10)))
	x.Tables = "cloudburrow_vector_index_" + hex.EncodeToString(sum[:8])
	x.DDL = vectorIndexDDL(x, project, dataset, table)
	return x, 0, ""
}

// vectorIndexDDL is x's DDL as VECTOR_INDEXES gives it: the page's
// example writes the options sorted by name.
func vectorIndexDDL(x *vectorIndex, project, dataset, table string) string {
	ddl := "CREATE VECTOR INDEX `" + x.Name + "` ON `" + project + "." + dataset + "." + table + "`(" + x.Column + ")"
	if len(x.Stored) > 0 {
		ddl += "\nSTORING(" + strings.Join(x.Stored, ", ") + ")"
	}
	opts := append([]vectorIndexOption(nil), x.Options...)
	sort.Slice(opts, func(i, j int) bool { return opts[i].Name < opts[j].Name })
	var parts []string
	for _, o := range opts {
		v := o.Value
		if o.Type == "STRING" {
			s, _ := strconv.Unquote(v)
			v = sqlString(s)
		}
		parts = append(parts, o.Name+" = "+v)
	}
	return ddl + "\nOPTIONS (" + strings.Join(parts, ", ") + ")"
}

func (x *vectorIndex) coverage() int64 {
	if !x.populated() {
		return 0
	}
	return 100
}

// dropVectorIndexTables deletes x's tables.
func (f front) dropVectorIndexTables(r *http.Request, x *vectorIndex) {
	_ = f.runDML(r, "DROP TABLE IF EXISTS "+x.listsTable())
	_ = f.runDML(r, "DROP TABLE IF EXISTS "+x.centroidsTable())
}

// vectorIndexState is the table's rows as the index sees them: their
// count and the XOR of their embeddings' fingerprints, and the table's
// logical bytes.
func (f front) vectorIndexState(r *http.Request, project, dataset, table string, fields []map[string]any, x *vectorIndex) (state string, size int64, code int, msg string) {
	var fs []field
	b, _ := json.Marshal(fields)
	_ = json.Unmarshal(b, &fs)
	ref := quotePath([]string{project, dataset, table})
	v := x.vecSQL("")
	_, rows, code, msg := f.queryAllRows(r, queryOptions{}, "SELECT COUNTIF("+v+" IS NOT NULL) AS n, IFNULL(BIT_XOR(FARM_FINGERPRINT(TO_JSON_STRING("+v+"))), 0) AS x, "+
		"IFNULL(SUM("+logicalBytesExpr(fs)+"), 0) AS b FROM "+ref)
	if code != 0 {
		return "", 0, code, msg
	}
	if len(rows) != 1 || len(rows[0]) != 3 || rows[0][0] == nil || rows[0][1] == nil || rows[0][2] == nil {
		return "", 0, 500, "cloudburrow: could not read the indexed table"
	}
	size, _ = strconv.ParseInt(*rows[0][2], 10, 64)
	return *rows[0][0] + ":" + *rows[0][1], size, 0, ""
}

// populateVectorIndex brings x up to date with the table: below 10 MB it
// is not populated; otherwise it is trained (when it has no lists, or
// retrain) and its rows assigned, when they changed. x is changed in
// place; the caller keeps it.
func (f front) populateVectorIndex(r *http.Request, project, dataset, table string, fields []map[string]any, x *vectorIndex, retrain bool) (int, string) {
	state, size, code, msg := f.vectorIndexState(r, project, dataset, table, fields, x)
	if code != 0 {
		return code, msg
	}
	if size < vectorIndexMinBytes {
		if x.Refreshed != 0 || x.Disabled == 0 {
			x.Disabled = f.nowMillis()
		}
		x.Refreshed, x.Lists, x.State, x.Indexed, x.Bytes = 0, 0, "", 0, 0
		return 0, ""
	}
	if !retrain && x.populated() && x.State == state {
		return 0, ""
	}
	ref := quotePath([]string{project, dataset, table})
	v := x.vecSQL("")
	_, rows, code, msg := f.queryAllRows(r, queryOptions{}, "SELECT FARM_FINGERPRINT(TO_JSON_STRING("+v+")) AS fp, ANY_VALUE(TO_JSON_STRING("+v+")) AS j FROM "+ref+
		" WHERE "+v+" IS NOT NULL GROUP BY fp")
	if code != 0 {
		return code, msg
	}
	fps := make([]string, 0, len(rows))
	vecs := make([][]float64, 0, len(rows))
	dims := -1
	for _, row := range rows {
		if len(row) != 2 || row[0] == nil || row[1] == nil {
			continue
		}
		var raw []*float64
		if json.Unmarshal([]byte(*row[1]), &raw) != nil {
			return 500, "cloudburrow: could not read an embedding of the indexed column"
		}
		if len(raw) == 0 {
			continue
		}
		vec := make([]float64, len(raw))
		for i, e := range raw {
			if e == nil {
				return 400, "The vector index column " + x.Column + " has an array with a NULL element: all elements must be non-NULL"
			}
			vec[i] = *e
		}
		if dims >= 0 && len(vec) != dims {
			return 400, "The vector index column " + x.Column + " has arrays of different dimensions (" + strconv.Itoa(dims) + " and " + strconv.Itoa(len(vec)) + "): all must have the same"
		}
		dims = len(vec)
		if x.Norm == "L2" {
			normalize(vec)
		}
		fps = append(fps, *row[0])
		vecs = append(vecs, vec)
	}
	if len(vecs) == 0 {
		x.Refreshed, x.Lists, x.State, x.Indexed = 0, 0, "", 0
		return 0, ""
	}
	if x.Type == "TREE_AH" && dims < 2 {
		return 400, "A TREE_AH vector index needs arrays of at least 2 dimensions; " + x.Column + " has " + strconv.Itoa(dims)
	}
	var centroids [][]float64
	if !retrain && x.populated() && x.Dims == dims {
		var code int
		if centroids, code, msg = f.vectorIndexCentroids(r, x); code != 0 {
			return code, msg
		}
	}
	if centroids == nil {
		k := x.NumLists
		switch {
		case x.Type == "TREE_AH":
			k = (len(vecs) + x.Leaf - 1) / x.Leaf
		case k == 0:
			k = int(math.Ceil(math.Sqrt(float64(len(vecs)))))
		}
		k = max(1, min(k, 5000, len(vecs)))
		centroids = kMeans(vecs, k, 10)
		var rowsSQL []string
		for i, c := range centroids {
			rowsSQL = append(rowsSQL, "("+strconv.Itoa(i)+", "+floatArrayLiteral(c)+")")
		}
		if msg := f.writeIndexTable(r, x.centroidsTable(), "list INT64, centroid ARRAY<FLOAT64>", rowsSQL); msg != "" {
			return 500, "cloudburrow: could not write the vector index: " + msg
		}
		x.Built = f.nowMillis()
		x.Dims = dims
	}
	var rowsSQL []string
	for i, vec := range vecs {
		rowsSQL = append(rowsSQL, "("+fps[i]+", "+strconv.Itoa(nearestCentroid(centroids, vec))+")")
	}
	if msg := f.writeIndexTable(r, x.listsTable(), "fp INT64, list INT64", rowsSQL); msg != "" {
		return 500, "cloudburrow: could not write the vector index: " + msg
	}
	x.Lists = len(centroids)
	x.Indexed = int64(len(vecs))
	x.State = state
	x.Refreshed = f.nowMillis()
	x.Disabled = 0
	x.Bytes = int64(len(vecs))*16 + int64(len(centroids))*int64(8+8*dims)
	return 0, ""
}

// writeIndexTable replaces table with rows, in batches.
func (f front) writeIndexTable(r *http.Request, table, columns string, rows []string) string {
	// Dropped and made again: the engine's CREATE OR REPLACE TABLE of a
	// table that exists fails "table is already created" (measured).
	if msg := f.runDML(r, "DROP TABLE IF EXISTS "+table); msg != "" {
		return msg
	}
	if msg := f.runDML(r, "CREATE TABLE "+table+" ("+columns+")"); msg != "" {
		return msg
	}
	for len(rows) > 0 {
		n := jobsBatch(rows)
		if msg := f.runDML(r, "INSERT INTO "+table+" VALUES "+strings.Join(rows[:n], ", ")); msg != "" {
			return msg
		}
		rows = rows[n:]
	}
	return ""
}

// vectorIndexCentroids reads x's trained centroids.
func (f front) vectorIndexCentroids(r *http.Request, x *vectorIndex) ([][]float64, int, string) {
	_, rows, code, msg := f.queryAllRows(r, queryOptions{}, "SELECT list, TO_JSON_STRING(centroid) FROM "+x.centroidsTable()+" ORDER BY list")
	if code != 0 {
		return nil, code, msg
	}
	var out [][]float64
	for _, row := range rows {
		var c []float64
		if len(row) != 2 || row[1] == nil || json.Unmarshal([]byte(*row[1]), &c) != nil {
			return nil, 500, "cloudburrow: could not read the vector index's centroids"
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, 0, ""
	}
	return out, 0, ""
}

func normalize(v []float64) {
	var n float64
	for _, x := range v {
		n += x * x
	}
	if n == 0 {
		return
	}
	n = math.Sqrt(n)
	for i := range v {
		v[i] /= n
	}
}

func sqDist(a, b []float64) float64 {
	var s float64
	for i := range a {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

func nearestCentroid(centroids [][]float64, v []float64) int {
	best, bd := 0, math.Inf(1)
	for i, c := range centroids {
		if d := sqDist(c, v); d < bd {
			best, bd = i, d
		}
	}
	return best
}

// kMeans clusters vecs into k centroids: k-means++ seeds (a fixed seed,
// so a rebuild of the same rows gives the same index), then iterations of
// Lloyd's algorithm. At most 50,000 vectors are sampled for training.
func kMeans(vecs [][]float64, k, iterations int) [][]float64 {
	rng := rand.New(rand.NewSource(1952))
	train := vecs
	if len(train) > 50000 {
		train = make([][]float64, 50000)
		for i, j := range rng.Perm(len(vecs))[:50000] {
			train[i] = vecs[j]
		}
	}
	dims := len(train[0])
	centroids := [][]float64{append([]float64(nil), train[rng.Intn(len(train))]...)}
	d2 := make([]float64, len(train))
	for i, v := range train {
		d2[i] = sqDist(v, centroids[0])
	}
	for len(centroids) < k {
		var sum float64
		for _, d := range d2 {
			sum += d
		}
		pick := 0
		if sum == 0 {
			pick = rng.Intn(len(train))
		} else {
			target := rng.Float64() * sum
			for i, d := range d2 {
				target -= d
				if target <= 0 {
					pick = i
					break
				}
			}
		}
		c := append([]float64(nil), train[pick]...)
		centroids = append(centroids, c)
		for i, v := range train {
			if d := sqDist(v, c); d < d2[i] {
				d2[i] = d
			}
		}
	}
	assign := make([]int, len(train))
	for it := 0; it < iterations; it++ {
		changed := false
		for i, v := range train {
			if n := nearestCentroid(centroids, v); n != assign[i] || it == 0 {
				changed = changed || n != assign[i]
				assign[i] = n
			}
		}
		sums := make([][]float64, k)
		counts := make([]int, k)
		for i := range sums {
			sums[i] = make([]float64, dims)
		}
		for i, v := range train {
			c := assign[i]
			counts[c]++
			for j, e := range v {
				sums[c][j] += e
			}
		}
		for c := range centroids {
			if counts[c] == 0 {
				continue // an empty list keeps its centroid
			}
			for j := range sums[c] {
				centroids[c][j] = sums[c][j] / float64(counts[c])
			}
		}
		if !changed && it > 0 {
			break
		}
	}
	return centroids
}

// vectorIndexUse is how one search uses the table's vector index.
type vectorIndexUse struct {
	index *vectorIndex // the populated index the search probes, or nil
	// probes is how many lists each query vector probes.
	probes int
	// reason is why the index is not used: an IndexUnusedReason code and
	// message, or "".
	code, message string
	table         tableRef
}

// searchIndex is how a search of column (the ARRAY<FLOAT64> column, or
// the generated embedding column) of the table uses its vector index,
// with the options (validated by checkSearchOptions) of the call. The
// index is brought up to date first.
func (f front) searchIndex(r *http.Request, project, dataset, table string, fields []map[string]any, meta map[string]any, column, options string) (vectorIndexUse, int, string) {
	use := vectorIndexUse{table: tableRef{ProjectID: project, DatasetID: dataset, TableID: table}}
	key := streamKey(project, dataset, table)
	created := str(meta, "creationTime")
	x := f.tables.get(key, created).VectorIndex
	if x == nil || !strings.EqualFold(x.Column, column) {
		use.code, use.message = "INDEX_CONFIG_NOT_AVAILABLE", "There is no vector index on the column "+column+" of the base table."
		return use, 0, ""
	}
	var opts struct {
		Fraction *float64 `json:"fraction_lists_to_search"`
		Brute    bool     `json:"use_brute_force"`
	}
	if raw, ok := jsonArgument(options); ok && options != "" {
		_ = json.Unmarshal([]byte(raw), &opts)
	}
	y := *x
	if code, msg := f.populateVectorIndex(r, project, dataset, table, fields, &y, false); code != 0 {
		return use, code, msg
	}
	if y.Refreshed != x.Refreshed || y.Disabled != x.Disabled || y.State != x.State {
		f.tables.setVectorIndex(key, created, &y)
	}
	switch {
	case opts.Brute:
		use.code, use.message = "INDEX_SUPPRESSED_BY_FUNCTION_OPTION", "The vector index was not used because use_brute_force option has been specified."
	case !y.populated():
		use.code, use.message = "BASE_TABLE_TOO_SMALL", "The vector index is not used: the base table is smaller than 10 MB, so the index is not populated."
	default:
		fraction := vectorIndexDefaultFraction
		if opts.Fraction != nil {
			fraction = *opts.Fraction
		}
		use.index = &y
		use.probes = max(1, min(y.Lists, int(math.Ceil(fraction*float64(y.Lists)))))
	}
	return use, 0, ""
}

// stats is the search's vectorSearchStatistics.
func (u vectorIndexUse) stats() map[string]any {
	if u.index != nil {
		return map[string]any{"indexUsageMode": "FULLY_USED"}
	}
	return map[string]any{"indexUsageMode": "UNUSED", "indexUnusedReasons": []any{map[string]any{
		"code": u.code, "message": u.message,
		"baseTable": map[string]any{"projectId": u.table.ProjectID, "datasetId": u.table.DatasetID, "tableId": u.table.TableID}}}}
}

// mergeVectorStats combines the vectorSearchStatistics of a query's
// searches: FULLY_USED when all used an index, UNUSED when none did.
func mergeVectorStats(all []map[string]any) json.RawMessage {
	if len(all) == 0 {
		return nil
	}
	used := 0
	var reasons []any
	for _, s := range all {
		if s["indexUsageMode"] == "FULLY_USED" {
			used++
		}
		if rs, ok := s["indexUnusedReasons"].([]any); ok {
			reasons = append(reasons, rs...)
		}
	}
	out := map[string]any{"indexUsageMode": "UNUSED"}
	switch {
	case used == len(all):
		out["indexUsageMode"] = "FULLY_USED"
	case used > 0:
		out["indexUsageMode"] = "PARTIALLY_USED"
	}
	if len(reasons) > 0 {
		out["indexUnusedReasons"] = reasons
	}
	b, _ := json.Marshal(out)
	return b
}

// normalizedSQL is v, an ARRAY<FLOAT64> expression, divided by its L2
// norm.
func normalizedSQL(v string) string {
	return "ARRAY(SELECT _cb_n_x / NULLIF(SQRT((SELECT SUM(_cb_n_y * _cb_n_y) FROM UNNEST(" + v + ") AS _cb_n_y)), 0) FROM UNNEST(" + v + ") AS _cb_n_x WITH OFFSET _cb_n_o ORDER BY _cb_n_o)"
}

// probeSQL is the lists a query vector qv probes: the use's nearest
// centroids by EUCLIDEAN distance.
func (u vectorIndexUse) probeSQL(qv string) string {
	if u.index.Norm == "L2" {
		qv = normalizedSQL(qv)
	}
	return "ARRAY(SELECT _cb_ct.list FROM " + u.index.centroidsTable() + " AS _cb_ct ORDER BY " +
		"(SELECT SUM(POW(_cb_cx - " + qv + "[OFFSET(_cb_ci)], 2)) FROM UNNEST(_cb_ct.centroid) AS _cb_cx WITH OFFSET _cb_ci), _cb_ct.list LIMIT " + strconv.Itoa(u.probes) + ")"
}

// listedBase is base, a base rows query of the columns cols whose
// embedding column is _cb_vs_vec, with each row's list (_cb_vs_list, NULL
// for a row the index has not assigned). The rows and the index's lists
// are put together and partitioned by fingerprint, which the engine
// sorts, rather than joined: it joins two tables by comparing every pair
// of rows (measured: an 11,000-row join did not finish in two minutes,
// the partition took two seconds).
func (u vectorIndexUse) listedBase(base string, cols []string) string {
	var out []string
	for _, c := range append(append([]string(nil), cols...), "_cb_vs_vec") {
		out = append(out, "_cb_u.r."+sqlIdent(c)+" AS "+sqlIdent(c))
	}
	return "SELECT " + strings.Join(out, ", ") + ", _cb_u._cb_l AS _cb_vs_list FROM (SELECT r, MAX(l) OVER (PARTITION BY fp) AS _cb_l, isb FROM (" +
		"SELECT (SELECT AS STRUCT _cb_b0.*) AS r, FARM_FINGERPRINT(TO_JSON_STRING(_cb_b0._cb_vs_vec)) AS fp, CAST(NULL AS INT64) AS l, TRUE AS isb FROM (" + base + ") AS _cb_b0 " +
		"UNION ALL SELECT NULL, fp, list, FALSE FROM " + u.index.listsTable() + ")) AS _cb_u WHERE _cb_u.isb"
}

// whereColumns are the base table's columns that a base_table_query's
// WHERE names (the words that are column names), and whether it has one.
func whereColumns(query string, fields []map[string]any) (cols []string, where int, ok bool) {
	toks, lexed := lex(query)
	if !lexed {
		return nil, -1, false
	}
	depth := tokenDepths(toks)
	where = -1
	for i, t := range toks {
		if depth[i] == 0 && t.is("WHERE") {
			where = i
			break
		}
	}
	if where < 0 {
		return nil, -1, true
	}
	names := map[string]string{}
	for _, fl := range fields {
		names[strings.ToLower(str(fl, "name"))] = str(fl, "name")
	}
	seen := map[string]bool{}
	for _, t := range toks[where+1:] {
		if t.kind != tokWord && t.kind != tokQuoted {
			continue
		}
		if n, ok := names[strings.ToLower(t.text)]; ok && !seen[n] {
			seen[n] = true
			cols = append(cols, n)
		}
	}
	return cols, toks[where].pos, true
}

// preFilter reports whether a WHERE on cols is applied before an indexed
// search (all of them stored in the index) or after it.
func (x *vectorIndex) preFilter(cols []string) bool {
	stored := map[string]bool{}
	for _, s := range x.Stored {
		stored[strings.ToLower(s)] = true
	}
	for _, c := range cols {
		if !stored[strings.ToLower(c)] {
			return false
		}
	}
	return true
}

// vectorIndexRows are the rows of VECTOR_INDEXES, VECTOR_INDEX_COLUMNS
// and VECTOR_INDEX_OPTIONS (view) of dataset.
func vectorIndexRowsOf(view string) func(front, *http.Request, string, *datasetMeta) (string, int, string) {
	columns := map[string]string{"VECTOR_INDEXES": vectorIndexesColumns, "VECTOR_INDEX_COLUMNS": indexColumnsColumns, "VECTOR_INDEX_OPTIONS": indexOptionsColumns}[view]
	return func(f front, r *http.Request, dataset string, m *datasetMeta) (string, int, string) {
		tables, st, msg := f.tablesOf(r, dataset, m)
		if st != 0 {
			return "", st, msg
		}
		project := projectOf(f.base)
		var all [][]string
		for _, t := range tables {
			table := str(t, "tableId")
			key := streamKey(project, dataset, table)
			created := str(t, "creationTime")
			x := f.tables.get(key, created).VectorIndex
			if x == nil {
				continue
			}
			lead := []string{sqlString(project), sqlString(dataset), sqlString(table), sqlString(x.Name)}
			switch view {
			case "VECTOR_INDEX_COLUMNS":
				all = append(all, append(lead, sqlString(x.Column), sqlString(x.Column)))
				continue
			case "VECTOR_INDEX_OPTIONS":
				for _, o := range x.Options {
					all = append(all, append(append([]string(nil), lead...), sqlString(o.Name), sqlString(o.Type), sqlString(o.Value)))
				}
				continue
			}
			if fields, _, found := f.tableSchemaFields(r, project, dataset, table); found {
				y := *x
				if code, _ := f.populateVectorIndex(r, project, dataset, table, fields, &y, false); code == 0 &&
					(y.Refreshed != x.Refreshed || y.Disabled != x.Disabled || y.State != x.State) {
					f.tables.setVectorIndex(key, created, &y)
					x = &y
				}
			}
			status, disabled, reason := "ACTIVE", "NULL", "NULL"
			if !x.populated() {
				status, disabled, reason = "TEMPORARILY DISABLED", msTimestamp(x.Disabled), sqlString("The indexed table is smaller than 10 MB.")
			}
			refreshed, built := "NULL", "NULL"
			if x.Refreshed > 0 {
				refreshed = msTimestamp(x.Refreshed)
			}
			if x.Built > 0 {
				built = msTimestamp(x.Built)
			}
			alteration := "NULL"
			if a := x.Alteration; a != nil {
				alteration = "STRUCT(CAST(NULL AS STRING) AS status, CAST(NULL AS STRUCT<reason STRING, location STRING, debug_info STRING, message STRING>) AS message, " +
					strconv.FormatInt(a.Coverage, 10) + " AS new_coverage_percentage, " + msTimestamp(a.Start) + " AS start_time, CAST(NULL AS TIMESTAMP) AS end_time, " +
					sqlString(a.DDL) + " AS ddl)"
			}
			unindexed := "0"
			if !x.populated() {
				unindexed = "NULL"
			}
			all = append(all, append(lead, sqlString(status), msTimestamp(x.Created), msTimestamp(x.Modified), refreshed, disabled, reason,
				sqlString(x.DDL), strconv.FormatInt(x.coverage(), 10), unindexed, strconv.FormatInt(x.Bytes, 10), strconv.FormatInt(x.Bytes, 10),
				alteration, built))
		}
		return rowsSubquery(columns, all), 0, ""
	}
}

func msTimestamp(ms int64) string {
	if ms == 0 {
		return "NULL"
	}
	return fmt.Sprintf("TIMESTAMP_MILLIS(%d)", ms)
}

// vectorIndexTables are the index table prefixes of every kept vector
// index, by table key, with the creation time each is kept for.
func (s *tableSettings) vectorIndexTables() map[string]tableSetting {
	out := map[string]tableSetting{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.m {
		if t.VectorIndex != nil {
			out[k] = t
		}
	}
	return out
}

// sweepVectorIndexTables deletes the index tables of indexes whose table
// is gone (deleted, or made again), which a table's deletion leaves; the
// front runs it when it makes an index.
func (f front) sweepVectorIndexTables(r *http.Request) {
	_, rows, code, _ := f.queryAllRows(r, queryOptions{}, "SELECT table_name FROM "+quotePath([]string{resultsDataset, "INFORMATION_SCHEMA", "TABLES"})+
		" WHERE STARTS_WITH(table_name, 'cloudburrow_vector_index_')")
	if code != 0 || len(rows) == 0 {
		return
	}
	live := map[string]bool{}
	for k, t := range f.tables.vectorIndexTables() {
		parts := strings.Split(k, "\x00")
		if len(parts) != 3 {
			continue
		}
		if _, meta, found := f.tableSchemaFields(r, parts[0], parts[1], parts[2]); found && str(meta, "creationTime") == t.Created {
			live[t.VectorIndex.Tables] = true
		} else {
			f.tables.setVectorIndex(k, t.Created, nil)
		}
	}
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		name := *row[0]
		prefix := strings.TrimSuffix(strings.TrimSuffix(name, "_lists"), "_centroids")
		if !live[prefix] {
			_ = f.runDML(r, "DROP TABLE IF EXISTS "+quotePath([]string{resultsDataset, name}))
		}
	}
}

// batchProbes is the SQL of the lists each query vector of a batch search
// probes, as an expression of q1._cb_vs_qvec: the front reads the
// distinct query vectors (vectors, a query of their column v), finds each
// one's nearest centroids, and writes them as constants by the vector's
// fingerprint. Written as a correlated subquery over the centroids, the
// probes made an 11,000-row batch search run past a minute (measured).
func (f front) batchProbes(r *http.Request, q queryOptions, vectors string, use vectorIndexUse) (string, int, string) {
	centroids, code, msg := f.vectorIndexCentroids(r, use.index)
	if code != 0 {
		return "", code, msg
	}
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT FARM_FINGERPRINT(TO_JSON_STRING(v)) AS fp, ANY_VALUE(TO_JSON_STRING(v)) AS j FROM ("+vectors+") WHERE v IS NOT NULL GROUP BY fp")
	if code != 0 {
		return "", code, msg
	}
	var entries []string
	for _, row := range rows {
		var v []float64
		if len(row) != 2 || row[0] == nil || row[1] == nil || json.Unmarshal([]byte(*row[1]), &v) != nil {
			continue
		}
		if len(v) != use.index.Dims {
			continue // the search fails on its dimensions
		}
		if use.index.Norm == "L2" {
			normalize(v)
		}
		order := make([]int, len(centroids))
		dist := make([]float64, len(centroids))
		for i, c := range centroids {
			order[i], dist[i] = i, sqDist(c, v)
		}
		sort.SliceStable(order, func(a, b int) bool { return dist[order[a]] < dist[order[b]] })
		var ids []string
		for _, i := range order[:min(use.probes, len(order))] {
			ids = append(ids, strconv.Itoa(i))
		}
		entries = append(entries, "("+*row[0]+", ["+strings.Join(ids, ", ")+"])")
	}
	if len(entries) == 0 {
		return "CAST([] AS ARRAY<INT64>)", 0, ""
	}
	return "(SELECT _cb_m.p FROM UNNEST(ARRAY<STRUCT<fp INT64, p ARRAY<INT64>>>[" + strings.Join(entries, ", ") + "]) AS _cb_m " +
		"WHERE _cb_m.fp = FARM_FINGERPRINT(TO_JSON_STRING(q1._cb_vs_qvec)))", 0, ""
}
