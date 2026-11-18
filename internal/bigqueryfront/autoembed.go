package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Autonomous embedding generation (#1778), as
// https://docs.cloud.google.com/bigquery/docs/autonomous-embedding-generation
// documents it (read 2026-11-18):
//
//	col STRUCT<result ARRAY<FLOAT64>, status STRING>
//	  GENERATED ALWAYS AS (AI.EMBED(src, connection_id => C, endpoint => E | model => M [, task_type => T]))
//	  STORED OPTIONS (asynchronous = TRUE)
//
// in CREATE TABLE's column list or ALTER TABLE ADD COLUMN. "BigQuery
// maintains an embedding column based on a STRING or ObjectRef source
// column": when rows are added or their source changes, "a background job
// starts shortly after" and writes each row's embedding; a NULL source's
// result is NULL with status "NULL value is not supported for embedding
// generation", a failed one's result NULL with the failure as its status,
// and a good one's status ''.
//
// The engine makes no generated column, so the front cuts the clause from
// the statement it sends (the column is made as a plain STRUCT), keeps the
// generation (tableSetting.Embedding, by the table's creation time, so a
// copy, clone or snapshot, which is a new table, has none, as BigQuery's
// "copies data only") and writes the column's generatedColumn into the
// table's schema, which tables.get and INFORMATION_SCHEMA.COLUMNS read
// back (is_generated ALWAYS, generation_expression the AI.EMBED call with
// its default task_type, is_stored YES).
//
// The background job is the front's embedGenerator: every two seconds,
// and at once after a statement that makes a generated column, it reads
// each such table's distinct (source, embedding) pairs and writes, with an
// UPDATE of the engine's own, the embedding of every source whose rows do
// not hold the one the front last wrote for it, on the local embedding
// model (--local-ai-embeddings), never Google. Rows written by any path
// (DML, tabledata.insertAll, loads, the Storage Write API) are seen, since
// the table is read, not the writes.
//
// "You can't write directly to the generated column": an INSERT that
// names it or gives every column (no column list), an UPDATE or MERGE that
// sets it, and a tabledata.insertAll row that has it are refused 400. The
// source column can't be dropped or renamed, nor either column's type
// changed, while the embedding column exists (400). The refusals' words
// are CloudBurrow's (UNVERIFIED, re-read 2026-11-18: the page quotes none).
//
// Not served (named, 501): an ObjectRef source (multimodal embeddings);
// a statement that makes one inside a script. UNVERIFIED (re-read 2026-11-18, the
// page does not say): whether a failed row is retried (here it is not,
// until its source changes); the exact text of generation_expression;
// that the default task_type is written into it for every model but
// gemini-embedding-2. The gc_ background jobs BigQuery lists in
// INFORMATION_SCHEMA.JOBS, async_generation_status' blocking errors and a
// BACKGROUND reservation are not kept: the front's own writes are not
// jobs.

// genEmbedding is a table's generated embedding column.
type genEmbedding struct {
	Project string `json:"project"`
	Dataset string `json:"dataset"`
	Table   string `json:"table"`
	Column  string `json:"column"`
	Source  string `json:"source"`
	// Expression is the AI.EMBED call as generation_expression gives it.
	Expression string `json:"expression"`
	Model      string `json:"model"`
	Endpoint   string `json:"endpoint"`
	Location   string `json:"location"`
	TaskType   string `json:"taskType,omitempty"`
	Title      string `json:"title,omitempty"`
	Dims       int    `json:"dims,omitempty"`
	// Connection is the connection_id, PROJECT.LOCATION.ID, or "" for
	// model => 'embeddinggemma-300m'.
	Connection string `json:"connection,omitempty"`
	// renaming is set while an ALTER TABLE RENAME COLUMN of the column
	// runs, so the background job does not take the column for dropped.
	renaming bool
}

// embedNullStatus is the status of a NULL source's row, as the page quotes
// it.
const embedNullStatus = "NULL value is not supported for embedding generation"

// embeddingColumnType is the generated column's type, normalized
// (normalType).
const embeddingColumnType = "STRUCT<RESULTARRAY<FLOAT64>,STATUSSTRING>"

func normalType(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// generatedDef is one generated column of a statement.
type generatedDef struct {
	column, typ, expr string
	cut               [2]int // the clause's span, cut from the statement
}

// generatedStatement is a CREATE TABLE or ALTER TABLE ADD COLUMN with
// generated columns.
type generatedStatement struct {
	alter      bool
	path       []string
	ifNotExist bool
	cols       []generatedDef
	// columns are a CREATE TABLE's column names and types.
	columns map[string]string
	text    string // the statement with the clauses cut
}

// parseGeneratedColumns reads sql's GENERATED ALWAYS AS clauses. ok is
// false when it has none; code and msg refuse it.
func parseGeneratedColumns(sql string) (st generatedStatement, ok bool, code int, msg string) {
	if !strings.Contains(strings.ToUpper(sql), "GENERATED") {
		return st, false, 0, ""
	}
	toks, lexed := lex(sql)
	if !lexed {
		return st, false, 0, ""
	}
	var gens []int
	for i := 0; i+3 < len(toks); i++ {
		if toks[i].is("GENERATED") && toks[i+1].is("ALWAYS") && toks[i+2].is("AS") && toks[i+3].punct("(") {
			gens = append(gens, i)
		}
	}
	if len(gens) == 0 {
		return st, false, 0, ""
	}
	for _, t := range toks {
		if t.punct(";") && t.end != len(strings.TrimRight(sql, " \t\r\n;")) && t.pos < len(strings.TrimRight(sql, " \t\r\n;")) {
			return st, true, 501, "Not implemented here: a generated column made inside a script; run the CREATE TABLE or ALTER TABLE statement on its own. Nothing was run."
		}
	}
	i := 0
	if !toks[0].is("CREATE") && !toks[0].is("ALTER") {
		return st, true, 400, "GENERATED ALWAYS AS is only valid in a column definition of CREATE TABLE or ALTER TABLE ADD COLUMN"
	}
	if toks[0].is("ALTER") {
		st.alter = true
		i = 1
		if !toks[i].is("TABLE") {
			return st, true, 400, "GENERATED ALWAYS AS is only valid in a column definition of CREATE TABLE or ALTER TABLE ADD COLUMN"
		}
		i++
		if toks[i].is("IF") && toks[i+1].is("EXISTS") {
			i += 2
		}
	} else {
		i = 1
		if toks[i].is("OR") && toks[i+1].is("REPLACE") {
			i += 2
		}
		if !toks[i].is("TABLE") {
			return st, true, 400, "GENERATED ALWAYS AS is only valid in a column definition of CREATE TABLE or ALTER TABLE ADD COLUMN"
		}
		i++
		if toks[i].is("IF") && toks[i+1].is("NOT") && toks[i+2].is("EXISTS") {
			st.ifNotExist = true
			i += 3
		}
	}
	path, n := readModelPath(toks[i:])
	if n == 0 {
		return st, true, 400, "Syntax error: expected a table name"
	}
	st.path = path
	i += n
	depth := tokenDepths(toks)
	var b strings.Builder
	last := 0
	for _, g := range gens {
		// The column's definition starts after the previous top-level
		// comma, the column list's (, or ADD COLUMN [IF NOT EXISTS].
		start := -1
		angle := 0 // the < > of STRUCT and ARRAY types
		for k := g - 1; k > i; k-- {
			if toks[k].punct(">") && !toks[k-1].punct("=") {
				angle++
			}
			if toks[k].punct("<") {
				angle--
			}
			if angle > 0 {
				continue
			}
			if st.alter && toks[k].is("COLUMN") && toks[k-1].is("ADD") {
				start = k + 1
				if toks[start].is("IF") && toks[start+1].is("NOT") && toks[start+2].is("EXISTS") {
					start += 3
				}
				break
			}
			if !st.alter && depth[k] == depth[g] && (toks[k].punct(",") || toks[k].punct("(")) {
				start = k + 1
				break
			}
			if !st.alter && depth[k] < depth[g] {
				start = k + 1
				break
			}
		}
		if start < 0 || start >= g-1 {
			return st, true, 400, "Syntax error: a generated column needs a name and a type"
		}
		col := toks[start].text
		typ := strings.TrimSpace(sql[toks[start+1].pos:toks[g].pos])
		end := matchParen(toks, g+3)
		if end < 0 {
			return st, true, 400, "Syntax error: unclosed GENERATED ALWAYS AS ("
		}
		expr := strings.TrimSpace(sql[toks[g+3].end:toks[end].pos])
		k := end + 1
		stored := k < len(toks) && toks[k].is("STORED")
		if stored {
			k++
		}
		async := false
		if k+1 < len(toks) && toks[k].is("OPTIONS") && toks[k+1].punct("(") {
			oe := matchParen(toks, k+1)
			if oe < 0 {
				return st, true, 400, "Syntax error: unclosed OPTIONS ("
			}
			for o := k + 2; o+2 <= oe; o++ {
				if toks[o].is("asynchronous") && toks[o+1].punct("=") {
					async = toks[o+2].is("TRUE")
				}
			}
			k = oe + 1
		}
		if !stored || !async {
			return st, true, 400, "A generated column of " + col + " must be STORED with OPTIONS (asynchronous = TRUE): BigQuery generates only embedding columns, asynchronously"
		}
		if normalType(typ) != embeddingColumnType {
			return st, true, 400, "The generated column " + col + " must be of type STRUCT<result ARRAY<FLOAT64>, status STRING>, not " + typ
		}
		st.cols = append(st.cols, generatedDef{column: col, typ: typ, expr: expr, cut: [2]int{toks[g].pos, toks[k-1].end}})
		b.WriteString(sql[last:toks[g].pos])
		last = toks[k-1].end
	}
	b.WriteString(sql[last:])
	st.text = b.String()
	if len(st.cols) > 1 {
		return st, true, 400, "A table can have at most one generated embedding column"
	}
	if !st.alter {
		// The column list's names and types, to check the source.
		st.columns = map[string]string{}
		open := i
		if open < len(toks) && toks[open].punct("(") {
			cl := matchParen(toks, open)
			s := open + 1
			angle := 0
			for k := open + 1; k <= cl && cl > 0; k++ {
				if toks[k].punct("<") {
					angle++
				}
				if toks[k].punct(">") && !toks[k-1].punct("=") {
					angle--
				}
				if k == cl || toks[k].punct(",") && depth[k] == depth[open]+1 && angle == 0 {
					if k > s+1 {
						st.columns[strings.ToLower(toks[s].text)] = normalType(sql[toks[s+1].pos:toks[k-1].end])
					}
					s = k + 1
				}
			}
		}
	}
	return st, true, 0, ""
}

// resolveGenerated checks a generated column's AI.EMBED call, source the
// type of its source column by name (normalType), and gives its
// generation.
func (f front) resolveGenerated(r *http.Request, q queryOptions, project string, d generatedDef, source func(string) (string, bool)) (genEmbedding, int, string) {
	g := genEmbedding{Column: d.column}
	sql := "SELECT " + d.expr
	toks, ok := lex(sql)
	if !ok {
		return g, 400, "Syntax error in the generation expression of " + d.column
	}
	calls, code, msg := findAICalls(sql, toks, tokenDepths(toks))
	if code != 0 {
		return g, code, msg
	}
	if len(calls) != 1 || calls[0].fn != "EMBED" || calls[0].start != len("SELECT ") || calls[0].end != len(sql) {
		return g, 400, "The generation expression of " + d.column + " must be an AI.EMBED call: BigQuery generates only embedding columns"
	}
	c := calls[0]
	src := c.args["content"]
	ctoks, _ := lex(src)
	if len(ctoks) != 1 || ctoks[0].kind != tokWord && ctoks[0].kind != tokQuoted {
		return g, 400, "The content of AI.EMBED in a generated column must be a column of the table, not " + src
	}
	typ, found := source(ctoks[0].text)
	if !found {
		return g, 400, "Unrecognized name: " + ctoks[0].text + " (the source column of " + d.column + ")"
	}
	switch {
	case typ == "STRING":
	case strings.Contains(typ, "URI") && strings.Contains(typ, "AUTHORIZER"): // ObjectRef
		return g, 501, "Not implemented here: an ObjectRef source column of a generated embedding column (multimodal embeddings, #1798). Nothing was run."
	default:
		return g, 400, "The source column " + ctoks[0].text + " of a generated embedding column must be a STRING or ObjectRef, not " + typ
	}
	if _, ok := c.args["endpoint"]; ok {
		if _, ok := c.args["connection_id"]; !ok {
			return g, 400, "AI.EMBED in a generated column requires a connection_id with its endpoint"
		}
	}
	a, code, msg := f.resolveAICall(r, q, project, c)
	if code != 0 {
		return g, code, msg
	}
	g.Source = ctoks[0].text
	g.Model, g.Endpoint, g.Location = a.rm.Model, a.rm.Endpoint, a.rm.Location
	g.Title, g.Dims = a.title, a.dims
	g.TaskType = a.taskType
	expr := d.expr
	if g.TaskType == "" && !strings.HasPrefix(strings.ToLower(g.Model), "gemini-embedding-2") {
		// "If omitted and the model supports it, defaults to
		// RETRIEVAL_DOCUMENT", which generation_expression shows.
		g.TaskType = "RETRIEVAL_DOCUMENT"
		cl := strings.LastIndex(expr, ")")
		expr = strings.TrimRight(expr[:cl], " \t\r\n") + ", task_type => 'RETRIEVAL_DOCUMENT')"
	}
	g.Expression = expr
	if s, ok := unquoteString(c.args["connection_id"]); ok {
		parts := strings.Split(s, ".")
		if len(parts) == 2 {
			parts = append([]string{project}, parts...)
		}
		g.Connection = strings.Join(parts, ".")
	}
	return g, 0, ""
}

// tableSchemaFields is a table's top-level fields by tables.get, and its
// creationTime and type; found is false when there is no such table.
func (f front) tableSchemaFields(r *http.Request, project, dataset, table string) (fields []map[string]any, meta map[string]any, found bool) {
	st, b := f.inProject(project).get(r, "/datasets/"+url.PathEscape(dataset)+"/tables/"+url.PathEscape(table))
	if st != http.StatusOK || json.Unmarshal(b, &meta) != nil {
		return nil, nil, false
	}
	schema, _ := meta["schema"].(map[string]any)
	return fieldsOf(schema), meta, true
}

// restFieldType is a REST field's type as normalType gives a DDL type.
func restFieldType(fl map[string]any) string {
	t := strings.ToUpper(str(fl, "type"))
	switch t {
	case "RECORD", "STRUCT":
		var parts []string
		for _, sub := range fieldsOf(map[string]any{"fields": fl["fields"]}) {
			parts = append(parts, strings.ToUpper(str(sub, "name"))+restFieldType(sub))
		}
		t = "STRUCT<" + strings.Join(parts, ",") + ">"
	case "FLOAT":
		t = "FLOAT64"
	case "INTEGER":
		t = "INT64"
	case "BOOLEAN":
		t = "BOOL"
	}
	if str(fl, "mode") == "REPEATED" {
		return "ARRAY<" + t + ">"
	}
	return t
}

// serveGeneratedColumns serves a statement that makes a generated column,
// refuses one that writes one, and reports whether it answered.
func (f front) serveGeneratedColumns(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.tables == nil {
		return false
	}
	f.tables.gen.start(f, r)
	st, ok, code, msg := parseGeneratedColumns(q.Query)
	if !ok {
		if code, msg := f.refuseGeneratedWrites(r, q); code != 0 {
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: "invalidQuery", Message: msg})
			return true
		}
		return f.renameEmbeddingColumn(w, r, q, insert)
	}
	if code == 501 {
		writeError(w, code, "notImplemented", msg)
		return true
	}
	if code != 0 {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: "invalidQuery", Message: msg})
		return true
	}
	project, dataset, table, ok := f.resolveModel(st.path, q)
	if !ok {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", 400, rowError{Reason: "invalidQuery",
			Message: "Table name " + strings.Join(st.path, ".") + " missing dataset while no default dataset is set in the request."})
		return true
	}
	before, beforeMeta, existed := f.tableSchemaFields(r, project, dataset, table)
	source := func(name string) (string, bool) {
		if !st.alter {
			t, ok := st.columns[strings.ToLower(name)]
			return t, ok
		}
		for _, fl := range before {
			if strings.EqualFold(str(fl, "name"), name) {
				return restFieldType(fl), true
			}
		}
		return "", false
	}
	if st.alter {
		if !existed {
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", 404, rowError{Reason: "notFound", Message: "Not found: Table " + project + ":" + dataset + "." + table})
			return true
		}
		if f.tables.get(streamKey(project, dataset, table), str(beforeMeta, "creationTime")).Embedding != nil {
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", 400, rowError{Reason: "invalidQuery", Message: "A table can have at most one generated embedding column"})
			return true
		}
	}
	g, code, msg := f.resolveGenerated(r, q, project, st.cols[0], source)
	if code == 501 {
		writeError(w, code, "notImplemented", msg)
		return true
	}
	if code != 0 {
		reason := map[int]string{403: "accessDenied", 404: "notFound"}[code]
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: cmpOr(reason, "invalidQuery"), Message: msg})
		return true
	}
	g.Project, g.Dataset, g.Table = project, dataset, table
	if !setQueryText(r, insert, st.text) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	client := jobText{query: q.Query}
	q2 := q
	q2.Query = st.text
	rec := newRecorder()
	f.runQuery(rec, r, q2, insert)
	if !dryRun(r, insert) {
		f.keepGenerated(r, g, existed, str(beforeMeta, "creationTime"), st)
	}
	f.clientText(w, rec, client)
	return true
}

// keepGenerated keeps g once the statement made its column: the table's
// (new) creation time with the generation, and generatedColumn in its
// schema.
func (f front) keepGenerated(r *http.Request, g genEmbedding, existed bool, createdBefore string, st generatedStatement) {
	var fields []map[string]any
	var meta map[string]any
	for try := 0; try < 50; try++ {
		var found bool
		fields, meta, found = f.tableSchemaFields(r, g.Project, g.Dataset, g.Table)
		if found && (st.alter || !existed || str(meta, "creationTime") != createdBefore) && slices.ContainsFunc(fields, func(fl map[string]any) bool {
			return strings.EqualFold(str(fl, "name"), g.Column)
		}) {
			break
		}
		fields = nil
		time.Sleep(100 * time.Millisecond)
	}
	if fields == nil {
		return // the statement failed, or did nothing (IF NOT EXISTS)
	}
	created := str(meta, "creationTime")
	f.tables.setEmbedding(streamKey(g.Project, g.Dataset, g.Table), created, &g)
	f.writeGeneratedSchema(r, g, meta)
	f.tables.gen.start(f, r)
	f.tables.gen.wakeUp()
}

// generatedColumnOf is the REST TableFieldSchema.generatedColumn of g.
func generatedColumnOf(g genEmbedding) map[string]any {
	return map[string]any{"generatedMode": "GENERATED_ALWAYS",
		"generatedExpressionInfo": map[string]any{"generationExpression": g.Expression, "asynchronous": true, "stored": true}}
}

// writeGeneratedSchema writes g's generatedColumn into the table's schema
// on the engine, which keeps a field's properties (its Table is the API's).
func (f front) writeGeneratedSchema(r *http.Request, g genEmbedding, meta map[string]any) {
	schema, _ := meta["schema"].(map[string]any)
	fs, _ := schema["fields"].([]any)
	for _, x := range fs {
		if fl, ok := x.(map[string]any); ok && strings.EqualFold(str(fl, "name"), g.Column) {
			fl["generatedColumn"] = generatedColumnOf(g)
		}
	}
	body, err := json.Marshal(map[string]any{"schema": schema})
	if err != nil {
		return
	}
	f.inProject(g.Project).send(r, http.MethodPatch, "/datasets/"+url.PathEscape(g.Dataset)+"/tables/"+url.PathEscape(g.Table), body)
}

// setEmbedding keeps g, the generated column of the table made at created.
func (s *tableSettings) setEmbedding(key, created string, g *genEmbedding) {
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
	t.Embedding = g
	s.m[key] = t
	s.saveLocked()
}

// embeddings are every table's generated columns, by key, with the
// creation time they are kept for.
func (s *tableSettings) embeddings() map[string]tableSetting {
	out := map[string]tableSetting{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.m {
		if t.Embedding != nil {
			out[k] = t
		}
	}
	return out
}

// embeddingOf is the generated column of project.dataset.table as kept
// for any creation time (the write checks', confirmed by tables.get).
func (s *tableSettings) embeddingOf(key string) (tableSetting, bool) {
	if s == nil {
		return tableSetting{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.m[key]
	return t, ok && t.Embedding != nil
}

// refuseGeneratedWrites is the 400 of a statement that writes a generated
// column, or (0, "").
func (f front) refuseGeneratedWrites(r *http.Request, q queryOptions) (int, string) {
	if len(f.tables.embeddings()) == 0 {
		return 0, ""
	}
	toks, ok := lex(q.Query)
	if !ok {
		return 0, ""
	}
	for i := 0; i < len(toks); i++ {
		var target int
		switch {
		case toks[i].is("INSERT"):
			target = i + 1
			if target < len(toks) && toks[target].is("INTO") {
				target++
			}
		case toks[i].is("UPDATE"), toks[i].is("MERGE"):
			target = i + 1
			if target < len(toks) && toks[target].is("INTO") {
				target++
			}
		case toks[i].is("ALTER") && i+1 < len(toks) && toks[i+1].is("TABLE"):
			target = i + 2
			if target+1 < len(toks) && toks[target].is("IF") && toks[target+1].is("EXISTS") {
				target += 2
			}
		default:
			continue
		}
		path, n := readModelPath(toks[target:])
		if n == 0 {
			continue
		}
		project, dataset, table, ok := f.resolveModel(path, q)
		if !ok {
			continue
		}
		key := streamKey(project, dataset, table)
		t, ok := f.tables.embeddingOf(key)
		if !ok {
			continue
		}
		_, meta, found := f.tableSchemaFields(r, project, dataset, table)
		if !found || str(meta, "creationTime") != t.Created {
			continue
		}
		col, src := t.Embedding.Column, t.Embedding.Source
		stmtEnd := len(toks)
		for k := target + n; k < len(toks); k++ {
			if toks[k].punct(";") {
				stmtEnd = k
				break
			}
		}
		body := toks[target+n : stmtEnd]
		writes := "You can't write to the generated column " + col + " of " + dataset + "." + table + ": BigQuery generates its values"
		switch {
		case toks[i].is("INSERT"):
			k := 0
			if k < len(body) && body[k].is("AS") {
				k += 2
			}
			if k >= len(body) || !body[k].punct("(") || k+1 < len(body) && body[k+1].is("SELECT") {
				return 400, writes + "; give an INSERT column list without it"
			}
			end := matchParen(body, k)
			for _, t := range body[k:max(end, k)] {
				if (t.kind == tokWord || t.kind == tokQuoted) && strings.EqualFold(t.text, col) {
					return 400, writes
				}
			}
		case toks[i].is("UPDATE"), toks[i].is("MERGE"):
			for k := 0; k+1 < len(body); k++ {
				if (body[k].kind == tokWord || body[k].kind == tokQuoted) && strings.EqualFold(body[k].text, col) && body[k+1].punct("=") &&
					k > 0 && (body[k-1].is("SET") || body[k-1].punct(",") || body[k-1].punct(".")) {
					return 400, writes
				}
				if toks[i].is("MERGE") && body[k].is("INSERT") && body[k+1].punct("(") {
					end := matchParen(body, k+1)
					for _, t := range body[k+1 : max(end, k+1)] {
						if (t.kind == tokWord || t.kind == tokQuoted) && strings.EqualFold(t.text, col) {
							return 400, writes
						}
					}
				}
			}
		default: // ALTER TABLE
			for k := 0; k+1 < len(body); k++ {
				name := ""
				switch {
				case body[k].is("DROP") && body[k+1].is("COLUMN"), body[k].is("RENAME") && body[k+1].is("COLUMN"):
					j := k + 2
					if j+1 < len(body) && body[j].is("IF") && body[j+1].is("EXISTS") {
						j += 2
					}
					if j < len(body) && strings.EqualFold(body[j].text, src) {
						name = src
					}
				case body[k].is("ALTER") && body[k+1].is("COLUMN"):
					j := k + 2
					if j+1 < len(body) && body[j].is("IF") && body[j+1].is("EXISTS") {
						j += 2
					}
					if j+2 < len(body) && (strings.EqualFold(body[j].text, src) || strings.EqualFold(body[j].text, col)) &&
						body[j+1].is("SET") && (body[j+2].is("DATA") || body[j+2].is("DEFAULT")) {
						return 400, "You can't change the data type or default of " + body[j].text + ": " + col + " of " + dataset + "." + table +
							" is a generated embedding column of " + src
					}
				}
				if name != "" {
					return 400, "You can't drop or rename the column " + name + " of " + dataset + "." + table + ": it is the source of the generated embedding column " + col +
						"; drop the embedding column first"
				}
			}
		}
	}
	return 0, ""
}

// refuseGeneratedRows is the 400 of a tabledata.insertAll whose rows give
// a generated column of project.dataset.table, or "".
func (f front) refuseGeneratedRows(project, dataset, table string, rows []map[string]any) string {
	t, ok := f.tables.embeddingOf(streamKey(project, dataset, table))
	if !ok {
		return ""
	}
	for _, row := range rows {
		for k := range row {
			if strings.EqualFold(k, t.Embedding.Column) {
				return "You can't write to the generated column " + t.Embedding.Column + " of " + dataset + "." + table + ": BigQuery generates its values"
			}
		}
	}
	return ""
}

// embedGenerator is the background job above.
type embedGenerator struct {
	mu      sync.Mutex
	started bool
	wake    chan struct{}
	f       front
	r       *http.Request
	// written is the embedding JSON (TO_JSON_STRING) each table's rows
	// hold for each source the front wrote, by table key and creation
	// time, then source ("\x00" for NULL).
	written map[string]map[string]string
}

// start runs the generator, once, with f and a request like r for its
// reads and writes.
func (g *embedGenerator) start(f front, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started || len(f.tables.embeddings()) == 0 {
		return
	}
	g.started = true
	g.wake = make(chan struct{}, 1)
	g.f = f
	tmpl := r.Clone(context.Background())
	tmpl.Body = http.NoBody
	g.r = tmpl
	go g.loop()
}

func (g *embedGenerator) wakeUp() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.wake != nil {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
}

func (g *embedGenerator) loop() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-g.wake:
		}
		g.pass()
	}
}

// pass brings every generated column up to date.
func (g *embedGenerator) pass() {
	f, r := g.f, g.r
	tables := f.tables.embeddings()
	keys := make([]string, 0, len(tables))
	for k := range tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := tables[k]
		e := *t.Embedding
		if e.renaming {
			continue
		}
		fields, meta, found := f.tableSchemaFields(r, e.Project, e.Dataset, e.Table)
		hasColumn := slices.ContainsFunc(fields, func(fl map[string]any) bool { return strings.EqualFold(str(fl, "name"), e.Column) })
		if !found || str(meta, "creationTime") != t.Created || !hasColumn {
			// The table was dropped or made again, or the column dropped.
			if found && str(meta, "creationTime") == t.Created || !found {
				f.tables.setEmbedding(k, t.Created, nil)
			}
			continue
		}
		g.update(f, r, k+"\x00"+t.Created, e)
	}
}

// update writes the embeddings e's table lacks.
func (g *embedGenerator) update(f front, r *http.Request, wkey string, e genEmbedding) {
	f = f.inProject(e.Project)
	ref := quotePath([]string{e.Project, e.Dataset, e.Table})
	read := "SELECT s, e FROM (SELECT " + sqlIdent(e.Source) + " AS s, TO_JSON_STRING(" + sqlIdent(e.Column) + ") AS e FROM " + ref + ") GROUP BY s, e"
	_, rows, code, _ := f.queryAllRows(r, queryOptions{}, read)
	if code != 0 {
		return
	}
	g.mu.Lock()
	if g.written == nil {
		g.written = map[string]map[string]string{}
	}
	done := g.written[wkey]
	if done == nil {
		done = map[string]string{}
		g.written[wkey] = done
	}
	todo := map[string]*string{}
	for _, row := range rows {
		key := "\x00"
		if row[0] != nil {
			key = "s" + *row[0]
		}
		got := ""
		if row[1] != nil {
			got = *row[1]
		}
		if want, ok := done[key]; !ok || want != got {
			todo[key] = row[0]
		}
	}
	g.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	// A blocking error stalls the whole column, as BigQuery's does, and
	// is cleared by the next pass that can run.
	blocked := f.generationBlocked(r, e)
	setBlockingError(e, wkey, blocked)
	if blocked != "" {
		return
	}
	a := aiResolved{rm: &remoteModel{Model: e.Model, Endpoint: e.Endpoint, Location: e.Location, Embedding: true}, taskType: e.TaskType, title: e.Title, dims: e.Dims}
	ctx := r.Context()
	for key, s := range todo {
		var value, where string
		if s == nil {
			value = "STRUCT(CAST(NULL AS ARRAY<FLOAT64>) AS result, " + sqlString(embedNullStatus) + " AS status)"
			where = sqlIdent(e.Source) + " IS NULL"
		} else {
			v, status := f.embedVector(ctx, e.Project, a, *s)
			if status != "" {
				value = "STRUCT(CAST(NULL AS ARRAY<FLOAT64>) AS result, " + sqlString(status) + " AS status)"
			} else {
				lits := make([]string, len(v))
				for i, x := range v {
					lits[i] = sqlF(x)
				}
				value = "STRUCT(ARRAY<FLOAT64>[" + strings.Join(lits, ", ") + "] AS result, '' AS status)"
			}
			where = sqlIdent(e.Source) + " = " + sqlString(*s)
		}
		if _, _, code, _ := f.queryAllRows(r, queryOptions{}, "UPDATE "+ref+" SET "+sqlIdent(e.Column)+" = "+value+" WHERE "+where); code != 0 {
			continue
		}
		readBack := "SELECT TO_JSON_STRING(" + sqlIdent(e.Column) + ") FROM " + ref + " WHERE " + where + " LIMIT 1"
		if _, got, code, _ := f.queryAllRows(r, queryOptions{}, readBack); code == 0 && len(got) == 1 && got[0][0] != nil {
			g.mu.Lock()
			done[key] = *got[0][0]
			g.mu.Unlock()
		}
	}
}

// renameEmbeddingColumn serves ALTER TABLE t RENAME COLUMN e TO n of a
// generated embedding column e, which keeps its generation under its new
// name: "you can still drop or rename the generated embedding column"
// (https://docs.cloud.google.com/bigquery/docs/autonomous-embedding-generation,
// read 2026-11-18), and reports whether it did.
func (f front) renameEmbeddingColumn(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if len(f.tables.embeddings()) == 0 || !strings.Contains(strings.ToUpper(q.Query), "RENAME") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok || len(toks) < 3 || !toks[0].is("ALTER") || !toks[1].is("TABLE") {
		return false
	}
	k := 2
	if k+1 < len(toks) && toks[k].is("IF") && toks[k+1].is("EXISTS") {
		k += 2
	}
	path, n := readModelPath(toks[k:])
	if n == 0 {
		return false
	}
	body := toks[k+n:]
	if len(body) > 0 && body[len(body)-1].punct(";") {
		body = body[:len(body)-1]
	}
	// One action: RENAME COLUMN [IF EXISTS] old TO new.
	j := 2
	if len(body) < 5 || !body[0].is("RENAME") || !body[1].is("COLUMN") {
		return false
	}
	if body[j].is("IF") && j+1 < len(body) && body[j+1].is("EXISTS") {
		j += 2
	}
	if j+2 >= len(body) || !body[j+1].is("TO") || j+3 != len(body) {
		return false
	}
	project, dataset, table, ok := f.resolveModel(path, q)
	if !ok {
		return false
	}
	key := streamKey(project, dataset, table)
	t, ok := f.tables.embeddingOf(key)
	if !ok || !strings.EqualFold(body[j].text, t.Embedding.Column) || dryRun(r, insert) {
		return false
	}
	_, meta, found := f.tableSchemaFields(r, project, dataset, table)
	if !found || str(meta, "creationTime") != t.Created {
		return false
	}
	old := *t.Embedding
	g := old
	g.Column, g.renaming = body[j+2].text, true
	f.tables.setEmbedding(key, t.Created, &g)
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	var fields []map[string]any
	for try := 0; try < 50; try++ {
		fields, meta, found = f.tableSchemaFields(r, project, dataset, table)
		if found && slices.ContainsFunc(fields, func(fl map[string]any) bool { return strings.EqualFold(str(fl, "name"), g.Column) }) {
			break
		}
		fields = nil
		if rec.status >= 400 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if fields == nil {
		f.tables.setEmbedding(key, t.Created, &old) // the rename failed
	} else {
		for _, fl := range fields {
			if strings.EqualFold(str(fl, "name"), g.Column) {
				g.Column = str(fl, "name")
			}
		}
		g.renaming = false
		f.tables.setEmbedding(key, str(meta, "creationTime"), &g)
		f.writeGeneratedSchema(r, g, meta)
		f.tables.gen.wakeUp()
	}
	rec.copyTo(w)
	return true
}

// refuseGeneratedLoad is the 400 of a load into d whose schema gives d's
// generated embedding column, or "": "You can't directly write to
// generated embedding columns" by bq load (the autonomous embedding
// generation page, read 2026-11-18).
func (f front) refuseGeneratedLoad(d tableRef, fields []field) string {
	project := cmpOr(d.ProjectID, projectOf(f.base))
	t, ok := f.tables.embeddingOf(streamKey(project, d.DatasetID, d.TableID))
	if !ok {
		return ""
	}
	for _, fl := range fields {
		if strings.EqualFold(fl.Name, t.Embedding.Column) {
			return "You can't write to the generated column " + t.Embedding.Column + " of " + d.DatasetID + "." + d.TableID + ": BigQuery generates its values; load without it"
		}
	}
	return ""
}

// refuseGeneratedCopy is the 400 of a WRITE_APPEND copy (bq copy -a) into
// a table with a generated embedding column from a source that has the
// column, or "": the page lists bq copy -a among the writes it refuses.
func (f front) refuseGeneratedCopy(r *http.Request, write string, srcs []tableRef, d tableRef) string {
	if write != "WRITE_APPEND" {
		return ""
	}
	project := cmpOr(d.ProjectID, projectOf(f.base))
	t, ok := f.tables.embeddingOf(streamKey(project, d.DatasetID, d.TableID))
	if !ok {
		return ""
	}
	for _, s := range srcs {
		fields, _, found := f.tableSchemaFields(r, cmpOr(s.ProjectID, project), s.DatasetID, strings.SplitN(s.TableID, "@", 2)[0])
		if found && slices.ContainsFunc(fields, func(fl map[string]any) bool { return strings.EqualFold(str(fl, "name"), t.Embedding.Column) }) {
			return "You can't write to the generated column " + t.Embedding.Column + " of " + d.DatasetID + "." + d.TableID + ": BigQuery generates its values"
		}
	}
	return ""
}

// blockingErrors are the generated columns' blocking errors, by table key
// and creation time (blockingKey): what INFORMATION_SCHEMA.COLUMNS'
// async_generation_status.blocking_error.message reads.
var blockingErrors sync.Map

func blockingKey(project, dataset, table, created string) string {
	return streamKey(project, dataset, table) + "\x00" + created
}

// setBlockingError keeps (or, for "", clears) e's blocking error; wkey is
// the table's key and creation time, as the generator keeps them.
func setBlockingError(e genEmbedding, wkey, msg string) {
	created := wkey[strings.LastIndex(wkey, "\x00")+1:]
	k := blockingKey(e.Project, e.Dataset, e.Table, created)
	if msg == "" {
		blockingErrors.Delete(k)
		return
	}
	blockingErrors.Store(k, msg)
}

// blockingErrorOf is the blocking error of the generated column of the
// table made at created, or "".
func blockingErrorOf(project, dataset, table, created string) string {
	if v, ok := blockingErrors.Load(blockingKey(project, dataset, table, created)); ok {
		return v.(string)
	}
	return ""
}

// generationBlocked is the blocking error that stalls e's generation, or
// "": of the ones the page lists ("Permission denied errors", "Not found
// errors", ...), the two that can arise locally: e's connection deleted,
// or its service account without the Vertex AI User role on the project,
// whose message the page shows ("<service_account> does not have the
// permission to access resources used by AI.EMBED. Please follow ...").
func (f front) generationBlocked(r *http.Request, e genEmbedding) string {
	if e.Connection == "" || f.connections == nil {
		return ""
	}
	parts := strings.Split(e.Connection, ".")
	if len(parts) != 3 {
		return ""
	}
	cn, ok := f.connections.lookup(connectionProject(parts[0], f.numbers), parts[1], parts[2])
	if !ok {
		return "Not found: Connection " + e.Connection
	}
	proj, code, _ := f.projectBindings(r.Context(), e.Project)
	if code != 0 {
		return "" // no Resource Manager to read the grants from: the calls' own checks apply
	}
	if sa := cn.serviceAccount(); sa == "" || !bindingsGrant(proj, "serviceAccount:"+sa, permVertexPredict) {
		return sa + " does not have the permission to access resources used by AI.EMBED. " +
			"Please follow https://docs.cloud.google.com/bigquery/docs/permissions-for-ai-functions to set up permissions."
	}
	return ""
}
