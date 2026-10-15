package bigqueryfront

// routines.list, routines.get and routines.delete (#1177).
//
// The emulator has none of them: measured against the pinned image
// through the official Go client, routines.list answered 500 "unsupported
// bigquery.routines.list" (the client retried it to its deadline), and
// routines.get and routines.delete 404 "routine f is not found" for a
// function CREATE FUNCTION made and for one routines.insert had made a
// moment before (its handlers are stubs, and routines.insert makes the
// function in the SQL engine's catalog, not the metadata store the
// handlers read).
//
// So the front serves them from what it knows of each function's
// definition (knownFunctions.defs):
//
//   - routines.insert: the Routine the emulator answered, with the
//     creation and modification times the front gives it;
//   - CREATE [OR REPLACE] [TABLE] FUNCTION sent through the front (and
//     those of the emulator's jobs from before it started, scanJobs): the
//     routine the statement describes, read from its text (routineFromDDL):
//     its type, language, arguments, return type or table and body, as
//     the client wrote them; DROP [TABLE] FUNCTION forgets it.
//
// A routine the front knows of is listed and read only while the engine
// still has the function (functionKind), so one whose CREATE failed, or
// that something the front did not see took out, is not; one routines.get
// does not find is 404 notFound "Not found: Routine project:dataset.id",
// as BigQuery answers. routines.delete drops the function in the engine
// (DROP FUNCTION, or DROP TABLE FUNCTION for a table function) and
// answers 204, as BigQuery does. routines.list gives, as BigQuery does
// unless readMask asks for more, each routine's etag, reference, type,
// times and language (https://cloud.google.com/bigquery/docs/reference/rest/v2/routines/list).
//
// Not served: a function routines.insert made before the front started
// and a procedure (CREATE PROCEDURE), which the front does not know;
// routines.update, 501.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// routineRoute matches routines.get, routines.update and routines.delete.
var routineRoute = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/datasets/([^/]+)/routines/([^/]+)$`)

// setDef records a routine's definition (a bigquery Routine resource),
// path its dataset and name, and the name (add).
func (k *knownFunctions) setDef(project string, path []string, def map[string]any) {
	if k == nil {
		return
	}
	b, err := json.Marshal(def)
	if err != nil {
		return
	}
	key := project + "/" + path[0]
	k.mu.Lock()
	if k.defs == nil {
		k.defs = map[string]map[string]json.RawMessage{}
	}
	if k.defs[key] == nil {
		k.defs[key] = map[string]json.RawMessage{}
	}
	k.defs[key][strings.ToLower(path[1])] = b
	k.mu.Unlock()
	k.add(project, path)
}

// dropDef forgets a routine's definition.
func (k *knownFunctions) dropDef(project string, path []string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.defs[project+"/"+path[0]], strings.ToLower(path[1]))
	k.saveLocked()
}

// defsIn returns the definitions known in a dataset, by lower-case name.
func (k *knownFunctions) defsIn(project, dataset string) map[string]json.RawMessage {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]json.RawMessage{}
	for n, d := range k.defs[project+"/"+dataset] {
		out[n] = d
	}
	return out
}

// noteDefs records the definitions a query's CREATE FUNCTION statements
// give, and forgets those its DROP FUNCTION statements drop, in order.
func (k *knownFunctions) noteDefs(project string, q queryOptions, v ddlVerdict) {
	if k == nil {
		return
	}
	for _, s := range v.funcs {
		if s.temp {
			continue
		}
		full, ok := functionPath(q, s.path)
		if !ok {
			continue
		}
		p := full[len(full)-2:]
		if s.drop {
			k.dropDef(project, p)
			continue
		}
		if s.ifNotExists {
			if _, ok := k.defsIn(project, p[0])[strings.ToLower(p[1])]; ok {
				continue
			}
		}
		if s.pathEnd > s.end || s.end > len(q.Query) {
			continue
		}
		def := routineFromDDL(q.Query[s.pathEnd:s.end], s.table)
		now := strconv.FormatInt(time.Now().UnixMilli(), 10)
		def["routineReference"] = map[string]any{"projectId": project, "datasetId": p[0], "routineId": p[1]}
		def["creationTime"], def["lastModifiedTime"] = now, now
		def["etag"] = etagOf(now)
		k.setDef(project, p, def)
	}
}

// etagOf is an etag for a routine made at s.
func etagOf(s string) string { return strconv.Quote(strconv.FormatUint(hashString(s), 36)) }

func hashString(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * 1099511628211
	}
	return h
}

// routineFromDDL reads the part of a CREATE [TABLE] FUNCTION statement
// after its name: (arguments) [RETURNS type | RETURNS TABLE<columns>]
// [LANGUAGE lang] [OPTIONS(...)] AS body. A part it cannot read is left
// out.
func routineFromDDL(text string, table bool) map[string]any {
	def := map[string]any{"routineType": "SCALAR_FUNCTION", "language": "SQL"}
	if table {
		def["routineType"] = "TABLE_VALUED_FUNCTION"
	}
	t, ok := lex(text)
	if !ok || len(t) == 0 || !t[0].punct("(") {
		return def
	}
	i := 1
	var args []map[string]any
	for i < len(t) && !t[i].punct(")") {
		if t[i].kind != tokWord && t[i].kind != tokQuoted {
			return def
		}
		arg := map[string]any{"name": t[i].text}
		i++
		if i+1 < len(t) && t[i].is("ANY") && t[i+1].is("TYPE") {
			arg["argumentKind"] = "ANY_TYPE"
			i += 2
		} else {
			typ, next := sqlTypeAt(t, i)
			if typ == nil {
				return def
			}
			arg["argumentKind"], arg["dataType"] = "FIXED_TYPE", typ
			i = next
		}
		args = append(args, arg)
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	if len(args) > 0 {
		def["arguments"] = args
	}
	i++
	for i < len(t) {
		switch {
		case t[i].is("RETURNS") && i+1 < len(t) && t[i+1].is("TABLE"):
			cols, next := sqlFieldsAt(t, i+2)
			if cols == nil {
				return def
			}
			def["returnTableType"] = map[string]any{"columns": cols}
			i = next
		case t[i].is("RETURNS"):
			typ, next := sqlTypeAt(t, i+1)
			if typ == nil {
				return def
			}
			def["returnType"] = typ
			i = next
		case t[i].is("LANGUAGE") && i+1 < len(t):
			if strings.EqualFold(t[i+1].text, "js") {
				def["language"] = "JAVASCRIPT"
			} else {
				def["language"] = strings.ToUpper(t[i+1].text)
			}
			i += 2
		case t[i].is("OPTIONS") && i+1 < len(t) && t[i+1].punct("("):
			end := closing(t, i+1)
			if end < 0 {
				return def
			}
			readRoutineOptions(t[i+2:end], def)
			i = end + 1
		case t[i].is("AS"):
			def["definitionBody"] = routineBody(text, t[i+1:])
			return def
		default:
			i++ // DETERMINISTIC, NOT DETERMINISTIC, ...
		}
	}
	return def
}

// routineBody is a routine's definitionBody, from the tokens after AS: a
// SQL function's expression without the parentheses around it, a table
// function's query, a JavaScript function's code without its quotes.
func routineBody(text string, t []token) string {
	if len(t) == 0 {
		return ""
	}
	if len(t) == 1 && t[0].kind == tokString {
		return unquoteLiteral(t[0].text)
	}
	if t[0].punct("(") && closing(t, 0) == len(t)-1 {
		return strings.TrimSpace(text[t[0].end:t[len(t)-1].pos])
	}
	return strings.TrimSpace(text[t[0].pos:t[len(t)-1].end])
}

// readRoutineOptions reads description and library from OPTIONS(...).
func readRoutineOptions(t []token, def map[string]any) {
	for i := 0; i+2 < len(t); i++ {
		if !t[i+1].punct("=") {
			continue
		}
		switch {
		case t[i].is("description") && t[i+2].kind == tokString:
			def["description"] = unquoteLiteral(t[i+2].text)
		case t[i].is("library"):
			var libs []string
			for j := i + 2; j < len(t) && !t[j].is("description"); j++ {
				if t[j].kind == tokString {
					libs = append(libs, unquoteLiteral(t[j].text))
				}
			}
			if len(libs) > 0 {
				def["importedLibraries"] = libs
			}
		}
	}
}

// closing returns the index of the token that closes the bracket at
// t[open], or -1.
func closing(t []token, open int) int {
	depth := 0
	for i := open; i < len(t); i++ {
		switch {
		case t[i].punct("(") || t[i].punct("["):
			depth++
		case t[i].punct(")") || t[i].punct("]"):
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// sqlTypeAt reads a GoogleSQL type at t[i] as a StandardSqlDataType, and
// returns the index after it; nil for one it cannot read.
func sqlTypeAt(t []token, i int) (map[string]any, int) {
	if i >= len(t) || t[i].kind != tokWord {
		return nil, i
	}
	kind := strings.ToUpper(t[i].text)
	i++
	switch kind {
	case "ARRAY", "RANGE":
		if i >= len(t) || !t[i].punct("<") {
			return nil, i
		}
		elem, next := sqlTypeAt(t, i+1)
		if elem == nil || next >= len(t) || !t[next].punct(">") {
			return nil, i
		}
		if kind == "ARRAY" {
			return map[string]any{"typeKind": "ARRAY", "arrayElementType": elem}, next + 1
		}
		return map[string]any{"typeKind": "RANGE", "rangeElementType": elem}, next + 1
	case "STRUCT":
		fields, next := sqlFieldsAt(t, i)
		if fields == nil {
			return nil, i
		}
		return map[string]any{"typeKind": "STRUCT", "structType": map[string]any{"fields": fields}}, next
	}
	if alias, ok := typeAliases[kind]; ok {
		kind = alias
	}
	if i < len(t) && t[i].punct("(") { // STRING(10), NUMERIC(10, 2)
		if end := closing(t, i); end > 0 {
			i = end + 1
		}
	}
	return map[string]any{"typeKind": kind}, i
}

// sqlFieldsAt reads <name type, ...> at t[i] as StandardSqlFields, and
// returns the index after the closing >; nil for fields it cannot read.
func sqlFieldsAt(t []token, i int) ([]map[string]any, int) {
	if i >= len(t) || !t[i].punct("<") {
		return nil, i
	}
	i++
	fields := []map[string]any{}
	for i < len(t) && !t[i].punct(">") {
		f := map[string]any{}
		// A STRUCT field may have no name: STRUCT<INT64>.
		if (t[i].kind == tokWord || t[i].kind == tokQuoted) && i+1 < len(t) && !t[i+1].punct(",") && !t[i+1].punct(">") &&
			!t[i+1].punct("<") && !t[i+1].punct("(") {
			f["name"] = t[i].text
			i++
		}
		typ, next := sqlTypeAt(t, i)
		if typ == nil {
			return nil, i
		}
		f["type"] = typ
		fields = append(fields, f)
		i = next
		if i < len(t) && t[i].punct(",") {
			i++
		}
	}
	if i >= len(t) {
		return nil, i
	}
	return fields, i + 1
}

// typeAliases are the GoogleSQL names that stand for another type
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-types).
var typeAliases = map[string]string{
	"INT": "INT64", "SMALLINT": "INT64", "INTEGER": "INT64", "BIGINT": "INT64", "TINYINT": "INT64", "BYTEINT": "INT64",
	"DECIMAL": "NUMERIC", "BIGDECIMAL": "BIGNUMERIC", "BOOLEAN": "BOOL",
}

// unquoteLiteral is the value of a string literal: '…', "…", ”'…”',
// """…""", r'…' and the rest; escapes are read in all but raw ones.
func unquoteLiteral(s string) string {
	raw := false
	for len(s) > 0 && (s[0] == 'r' || s[0] == 'R' || s[0] == 'b' || s[0] == 'B') {
		raw = raw || s[0] == 'r' || s[0] == 'R'
		s = s[1:]
	}
	q := 1
	if len(s) >= 6 && (strings.HasPrefix(s, `"""`) || strings.HasPrefix(s, `'''`)) {
		q = 3
	}
	if len(s) < 2*q {
		return s
	}
	s = s[q : len(s)-q]
	if raw {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// routineExists reports whether the engine still has a known routine's
// function.
func (f front) routineExists(r *http.Request, dataset, name string) bool {
	exists, _ := f.functionKind(r, []string{dataset, name})
	return exists
}

// listFields are the fields routines.list gives without readMask.
var listFields = []string{"etag", "routineReference", "routineType", "creationTime", "lastModifiedTime", "language", "remoteFunctionOptions"}

// listRoutines serves routines.list.
func (f front) listRoutines(w http.ResponseWriter, r *http.Request, project, dataset string) {
	if status, got := f.get(r, "/datasets/"+url.PathEscape(dataset)); status != http.StatusOK {
		writeRaw(w, status, got)
		return
	}
	f.functionsIn(r, project, dataset) // the jobs from before the front started
	defs := f.functions.defsIn(project, dataset)
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	q := r.URL.Query()
	start := 0
	if tok := q.Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid page token: "+tok)
			return
		}
		start = n
	}
	max := 1000
	if m, err := strconv.Atoi(q.Get("maxResults")); err == nil && m > 0 && m < max {
		max = m
	}
	var mask []string
	if rm := q.Get("readMask"); rm != "" {
		for _, p := range strings.Split(rm, ",") {
			if p = strings.TrimSpace(p); p != "" {
				mask = append(mask, strings.SplitN(p, ".", 2)[0])
			}
		}
		mask = append(mask, "routineReference")
	} else {
		mask = listFields
	}
	var live []map[string]any
	for _, n := range names {
		var def map[string]any
		if json.Unmarshal(defs[n], &def) != nil {
			continue
		}
		ref, _ := def["routineReference"].(map[string]any)
		if id, _ := ref["routineId"].(string); id != "" && f.routineExists(r, dataset, id) {
			live = append(live, def)
		}
	}
	next := ""
	if start > len(live) {
		start = len(live)
	}
	page := live[start:]
	if len(page) > max {
		page, next = page[:max], strconv.Itoa(start+max)
	}
	routines := []map[string]any{}
	for _, def := range page {
		out := map[string]any{}
		for _, k := range mask {
			if v, ok := def[k]; ok {
				out[k] = v
			}
		}
		routines = append(routines, out)
	}
	resp := map[string]any{"routines": routines}
	if next != "" {
		resp["nextPageToken"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

// knownRoutine returns a routine's definition, when the front knows it and
// the engine has the function.
func (f front) knownRoutine(r *http.Request, project, dataset, id string) (map[string]any, bool) {
	f.functionsIn(r, project, dataset)
	b, ok := f.functions.defsIn(project, dataset)[strings.ToLower(id)]
	if !ok {
		return nil, false
	}
	var def map[string]any
	if json.Unmarshal(b, &def) != nil {
		return nil, false
	}
	ref, _ := def["routineReference"].(map[string]any)
	if got, _ := ref["routineId"].(string); got != id {
		return nil, false // routine IDs are case-sensitive
	}
	if !f.routineExists(r, dataset, id) {
		f.functions.dropDef(project, []string{dataset, id})
		return nil, false
	}
	return def, true
}

// routineNotFound answers as BigQuery does for a routine that does not exist.
func routineNotFound(w http.ResponseWriter, project, dataset, id string) {
	writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Routine %s:%s.%s", project, dataset, id))
}

// serveRoutine serves routines.get, routines.delete and routines.update.
func (f front) serveRoutine(w http.ResponseWriter, r *http.Request, project, dataset, id string) {
	switch r.Method {
	case http.MethodGet:
		def, ok := f.knownRoutine(r, project, dataset, id)
		if !ok {
			routineNotFound(w, project, dataset, id)
			return
		}
		writeJSON(w, http.StatusOK, def)
	case http.MethodDelete:
		def, ok := f.knownRoutine(r, project, dataset, id)
		if !ok {
			routineNotFound(w, project, dataset, id)
			return
		}
		table := def["routineType"] == "TABLE_VALUED_FUNCTION"
		f.sendDDL(r, dropFunction(table)+" IF EXISTS "+functionName([]string{dataset, id}))
		if f.routineExists(r, dataset, id) {
			writeError(w, http.StatusInternalServerError, "internalError",
				"The emulator behind CloudBurrow did not drop the function "+dataset+"."+id+".")
			return
		}
		f.functions.dropDef(project, []string{dataset, id})
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut, http.MethodPatch:
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: routines.update. "+
			"The emulator behind CloudBurrow has no routines.update; CREATE OR REPLACE FUNCTION, run as a query of its own, "+
			"replaces a function.")
	default:
		writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
	}
}
