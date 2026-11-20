package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The OBJ functions in a query (objref.go, #1980): OBJ.MAKE_REF is
// written as SQL; OBJ.FETCH_METADATA, OBJ.GET_ACCESS_URL and
// OBJ.GET_READ_URL are evaluated here for the distinct ObjectRefs of the
// call's query block, as the AI functions' prompts are (aigenerate.go),
// and looked up in a literal table. The innermost call is rewritten
// first, so a call may take another's value.

var objFunctions = map[string]bool{"MAKE_REF": true, "FETCH_METADATA": true, "GET_ACCESS_URL": true, "GET_READ_URL": true, "LIST": true}

// objCall is one OBJ function call.
type objCall struct {
	fn         string
	start, end int
	pos        []string
	named      map[string]string
	block      int
	inner      bool // an argument holds another OBJ call
	depth      int
}

// findObjCalls finds sql's OBJ function calls.
func findObjCalls(sql string, toks []token, depth []int) ([]objCall, int, string) {
	var calls []objCall
	for i := 0; i+3 < len(toks); i++ {
		if !toks[i].is("OBJ") || !toks[i+1].punct(".") || toks[i+2].kind != tokWord || !toks[i+3].punct("(") || i > 0 && toks[i-1].punct(".") {
			continue
		}
		fn := strings.ToUpper(toks[i+2].text)
		if !objFunctions[fn] {
			return nil, 400, "Function not found: OBJ." + toks[i+2].text
		}
		end := matchParen(toks, i+3)
		if end < 0 {
			return nil, 400, "Syntax error: unclosed OBJ." + fn + "("
		}
		c := objCall{fn: fn, start: toks[i].pos, end: toks[end].end, named: map[string]string{}, block: enclosingSelect(toks, depth, i), depth: depth[i]}
		for k := i + 4; k+2 < end; k++ {
			if toks[k].is("OBJ") && toks[k+1].punct(".") {
				c.inner = true
			}
		}
		for _, a := range splitCallArgs(toks, depth, i+3, end) {
			if len(a) == 0 {
				if end == i+4 {
					break
				}
				return nil, 400, "Syntax error: an empty argument of OBJ." + fn
			}
			if len(a) > 3 && a[0].kind == tokWord && a[1].punct("=") && a[2].punct(">") {
				c.named[strings.ToLower(a[0].text)] = strings.TrimSpace(sql[a[3].pos:a[len(a)-1].end])
				continue
			}
			if len(c.named) > 0 {
				return nil, 400, "OBJ." + fn + ": positional arguments come before named ones"
			}
			c.pos = append(c.pos, strings.TrimSpace(sql[a[0].pos:a[len(a)-1].end]))
		}
		calls = append(calls, c)
	}
	return calls, 0, ""
}

// rewriteObjFunctions evaluates text's OBJ function calls.
func (f front) rewriteObjFunctions(r *http.Request, q queryOptions, text string) (string, int, string) {
	if !strings.Contains(strings.ToUpper(text), "OBJ") {
		return text, 0, ""
	}
	for range 200 {
		toks, ok := lex(text)
		if !ok {
			return text, 0, ""
		}
		depth := tokenDepths(toks)
		calls, code, msg := findObjCalls(text, toks, depth)
		if code != 0 || len(calls) == 0 {
			return text, code, msg
		}
		// OBJ.MAKE_REF and OBJ.LIST first (each is SQL alone, needing no
		// probe), then the deepest call, so that a probe of a block's rows
		// holds no OBJ call.
		alone := func(fn string) bool { return fn == "MAKE_REF" || fn == "LIST" }
		var c objCall
		for _, k := range calls {
			switch {
			case k.inner:
			case c.fn == "", alone(k.fn) && !alone(c.fn), alone(k.fn) == alone(c.fn) && k.depth > c.depth:
				c = k
			}
		}
		if c.fn == "" {
			return "", 500, "cloudburrow: could not order the OBJ calls"
		}
		repl, code, msg := f.objCallSQL(r, q, text, toks, depth, c)
		if code != 0 {
			return "", code, msg
		}
		text = text[:c.start] + repl + text[c.end:]
	}
	return "", 501, "Not implemented here: more than 200 OBJ function calls in one query. Nothing was run."
}

// objCallSQL is the SQL c is replaced by.
func (f front) objCallSQL(r *http.Request, q queryOptions, text string, toks []token, depth []int, c objCall) (string, int, string) {
	name := "OBJ." + c.fn
	switch c.fn {
	case "LIST":
		return f.objListSQL(r, c)
	case "MAKE_REF":
		return f.makeRefSQL(r, q, text, toks, depth, c)
	}
	allowed := map[string][2]int{"FETCH_METADATA": {1, 1}, "GET_ACCESS_URL": {2, 3}, "GET_READ_URL": {1, 1}}[c.fn]
	if len(c.named) > 0 {
		return "", 400, name + " takes no named arguments"
	}
	if len(c.pos) < allowed[0] || len(c.pos) > allowed[1] {
		return "", 400, "No matching signature for function " + name + " with " + strconv.Itoa(len(c.pos)) + " arguments"
	}
	write, dur := false, signedURLExpiry
	// perRow: a mode or duration that is an expression rather than a
	// literal (#1983), read with each ref from the block's rows.
	perRow := false
	if c.fn == "GET_ACCESS_URL" {
		if mode, ok := unquoteString(c.pos[1]); ok {
			if write, ok = accessMode(mode); !ok {
				return "", 400, "OBJ.GET_ACCESS_URL's mode must be 'r' or 'rw', not " + c.pos[1]
			}
		} else {
			perRow = true
		}
		if len(c.pos) == 3 {
			if ms, ok := intervalLiteralMs(lexOrNone(c.pos[2])); ok {
				dur = time.Duration(ms) * time.Millisecond
				if !validAccessDuration(dur) {
					return "", 400, accessDurationError
				}
			} else {
				perRow = true
			}
		}
	}
	if c.block < 0 {
		return "", 501, "Not implemented here: " + name + " outside a SELECT query block. Nothing was run."
	}
	arg := c.pos[0]
	if perRow {
		// The ref, the mode and the duration in milliseconds, one key.
		d := "CAST(NULL AS INT64)"
		if len(c.pos) == 3 {
			d = "UNIX_MILLIS(TIMESTAMP '1970-01-01 00:00:00+00' + (" + c.pos[2] + "))"
		}
		arg = "STRUCT(" + c.pos[0] + " AS r, CAST(" + c.pos[1] + " AS STRING) AS m, " + d + " AS d)"
	}
	probe := blockProbe(text, toks, depth, c.block, arg)
	if strings.Contains(probe, text[c.start:c.end]) {
		return "", 501, "Not implemented here: " + name + " inside a FROM clause (such as a JOIN's ON condition): filter in WHERE instead. Nothing was run."
	}
	_, rows, code, msg := f.queryAllRows(r, q, probe)
	if code != 0 {
		return "", code, msg
	}
	outType := map[string]string{"FETCH_METADATA": objRefType, "GET_ACCESS_URL": "JSON", "GET_READ_URL": "STRUCT<url STRING, status STRING>"}[c.fn]
	isArray := false
	var lits []string
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil || *row[0] == "null" {
			continue
		}
		key := *row[0]
		refKey := key
		if perRow {
			var t struct {
				R json.RawMessage `json:"r"`
				M *string         `json:"m"`
				D *int64          `json:"d"`
			}
			if json.Unmarshal([]byte(key), &t) != nil {
				return "", 500, "cloudburrow: unreadable OBJ.GET_ACCESS_URL arguments " + key
			}
			if t.M == nil || string(bytes.TrimSpace(t.R)) == "null" || len(c.pos) == 3 && t.D == nil {
				// A NULL argument: a NULL result.
				continue
			}
			var ok bool
			if write, ok = accessMode(*t.M); !ok {
				return "", 400, "OBJ.GET_ACCESS_URL's mode must be 'r' or 'rw', not " + strconv.Quote(*t.M)
			}
			dur = signedURLExpiry
			if t.D != nil {
				dur = time.Duration(*t.D) * time.Millisecond
				if !validAccessDuration(dur) {
					return "", 400, accessDurationError
				}
			}
			refKey = string(t.R)
		}
		var refs []objRef
		if strings.HasPrefix(strings.TrimSpace(refKey), "[") {
			if c.fn == "GET_READ_URL" {
				return "", 400, "No matching signature for function OBJ.GET_READ_URL for argument types: ARRAY"
			}
			isArray = true
			var items []json.RawMessage
			if json.Unmarshal([]byte(refKey), &items) != nil {
				return "", 400, name + " takes an ObjectRef or ARRAY<ObjectRef>"
			}
			for _, it := range items {
				o, ok := parseObjRef(it)
				if !ok {
					return "", 400, name + " takes an ObjectRef or ARRAY<ObjectRef>"
				}
				refs = append(refs, o)
			}
		} else {
			o, ok := parseObjRef([]byte(refKey))
			if !ok {
				return "", 400, name + " takes an ObjectRef or ARRAY<ObjectRef>, not " + refKey
			}
			refs = []objRef{o}
		}
		var vals []string
		for _, o := range refs {
			switch c.fn {
			case "FETCH_METADATA":
				vals = append(vals, f.fetchMetadata(r, o).sql())
			case "GET_ACCESS_URL":
				vals = append(vals, "PARSE_JSON("+sqlString(f.accessURL(r, o, write, dur))+")")
			case "GET_READ_URL":
				urls, _, code, msg := f.signedRefURLs(r, o, false, readURLExpiry)
				if code != 0 {
					vals = append(vals, "STRUCT(CAST(NULL AS STRING), "+sqlString(msg)+")")
				} else {
					vals = append(vals, "STRUCT("+sqlString(urls["read_url"])+", CAST(NULL AS STRING))")
				}
			}
		}
		v := vals[0]
		if isArray {
			v = "ARRAY<" + outType + ">[" + strings.Join(vals, ", ") + "]"
		}
		lits = append(lits, "STRUCT("+sqlString(key)+", "+v+")")
	}
	if isArray {
		outType = "ARRAY<" + outType + ">"
	}
	return "(SELECT _cbobj.v FROM UNNEST(ARRAY<STRUCT<k STRING, v " + outType + ">>[" + strings.Join(lits, ", ") +
		"]) AS _cbobj WHERE _cbobj.k = TO_JSON_STRING(" + arg + "))", 0, ""
}

// blockProbe is the query of the distinct TO_JSON_STRING(expr) values of
// the block whose SELECT is toks[sel].
func blockProbe(text string, toks []token, depth []int, sel int, expr string) string {
	return strings.Replace(blockSource(text, toks, depth, sel), "TO_JSON_STRING(%s)", "TO_JSON_STRING("+expr+")", 1)
}

// lexOrNone is s's tokens, or none.
func lexOrNone(s string) []token {
	t, _ := lex(s)
	return t
}

// makeRefSQL is OBJ.MAKE_REF as SQL.
func (f front) makeRefSQL(r *http.Request, q queryOptions, text string, toks []token, depth []int, c objCall) (string, int, string) {
	for k := range c.named {
		if k != "version" && k != "details" {
			return "", 400, "Unknown argument " + k + " of OBJ.MAKE_REF"
		}
	}
	if len(c.pos) < 1 || len(c.pos) > 2 {
		return "", 400, "No matching signature for function OBJ.MAKE_REF: it takes (uri [, authorizer] [, version => ...] [, details => ...]), (objectref_json) or (objectref, authorizer)"
	}
	first := c.pos[0]
	kind := ""
	switch {
	case isStringLiteral(first):
		kind = "STRING"
	case hasPrefixFold(first, "JSON ") || hasPrefixFold(first, "JSON'") || hasPrefixFold(first, "PARSE_JSON("):
		kind = "JSON"
	default:
		// The argument's type, from a query of none of the block's rows.
		probe := "SELECT " + first + " AS _cbobj_t LIMIT 0"
		if c.block >= 0 {
			src := blockSource(text, toks, depth, c.block)
			probe = strings.Replace(strings.Replace(src, "SELECT DISTINCT TO_JSON_STRING(%s) AS _cbai_k", "SELECT "+first+" AS _cbobj_t", 1),
				"SELECT TO_JSON_STRING(%s) AS _cbai_k", "SELECT "+first+" AS _cbobj_t", 1) + " LIMIT 0"
		}
		cols, _, code, msg := f.queryAllRows(r, q, probe)
		if code != 0 {
			return "", code, msg
		}
		if len(cols) != 1 {
			return "", 500, "cloudburrow: could not read the type of OBJ.MAKE_REF's argument"
		}
		kind = strings.ToUpper(cols[0].Type)
	}
	auth := "CAST(NULL AS STRING)"
	if len(c.pos) == 2 {
		if s, ok := unquoteString(c.pos[1]); ok {
			n, bad := f.normalizeAuthorizer(s)
			if bad != "" {
				return "", 400, bad
			}
			auth = sqlString(n)
		} else {
			a := "CAST(" + c.pos[1] + " AS STRING)"
			auth = "IF(ARRAY_LENGTH(SPLIT(" + a + ", '.')) = 2, CONCAT(" + sqlString(projectOf(f.base)) + ", '.', " + a + "), " + a + ")"
		}
	}
	switch kind {
	case "STRING":
		u := "CAST(" + first + " AS STRING)"
		if s, ok := unquoteString(first); ok {
			if _, _, ok := (objRef{URI: s}).gcsObject(); !ok {
				return "", 400, "Invalid Cloud Storage URI " + first + " of OBJ.MAKE_REF: it is gs://BUCKET/OBJECT"
			}
		} else {
			u = "IF(" + u + " IS NULL OR REGEXP_CONTAINS(" + u + ", r'^gs://[^/]+/.+'), " + u + ", ERROR(CONCAT('Invalid Cloud Storage URI of OBJ.MAKE_REF: ', " + u + ")))"
		}
		version, details := "CAST(NULL AS STRING)", "CAST(NULL AS JSON)"
		if v, ok := c.named["version"]; ok {
			version = "CAST(" + v + " AS STRING)"
		}
		if d, ok := c.named["details"]; ok {
			details = "CAST(" + d + " AS JSON)"
		}
		return "STRUCT(" + u + " AS uri, " + version + " AS version, " + auth + " AS authorizer, " + details + " AS details)", 0, ""
	case "JSON":
		if len(c.pos) != 1 || len(c.named) > 0 {
			return "", 400, "OBJ.MAKE_REF(objectref_json) takes one JSON argument"
		}
		j := "(" + first + ")"
		u := "JSON_VALUE(" + j + ", '$.uri')"
		a := "JSON_VALUE(" + j + ", '$.authorizer')"
		return "STRUCT(IF(" + u + " IS NULL, ERROR('OBJ.MAKE_REF: the JSON has no uri'), " + u + ") AS uri, JSON_VALUE(" + j + ", '$.version') AS version, " +
			"IF(ARRAY_LENGTH(SPLIT(" + a + ", '.')) = 2, CONCAT(" + sqlString(projectOf(f.base)) + ", '.', " + a + "), " + a + ") AS authorizer, " +
			"JSON_QUERY(" + j + ", '$.details') AS details)", 0, ""
	case "RECORD", "STRUCT":
		if len(c.pos) != 2 || len(c.named) > 0 {
			return "", 400, "OBJ.MAKE_REF(objectref, authorizer) takes an ObjectRef and the new authorizer"
		}
		o := "(" + first + ")"
		return "STRUCT(" + o + ".uri AS uri, " + o + ".version AS version, " + auth + " AS authorizer, " + o + ".details AS details)", 0, ""
	}
	return "", 400, "No matching signature for function OBJ.MAKE_REF for argument types: " + kind
}

func isStringLiteral(s string) bool {
	_, ok := unquoteString(s)
	return ok
}

func hasPrefixFold(s, p string) bool {
	return len(s) >= len(p) && strings.EqualFold(s[:len(p)], p)
}

// accessMode reads OBJ.GET_ACCESS_URL's mode: whether it is 'rw', and
// whether it is valid.
func accessMode(mode string) (write, ok bool) {
	switch strings.ToLower(mode) {
	case "r":
		return false, true
	case "rw":
		return true, true
	}
	return false, false
}

const accessDurationError = "OBJ.GET_ACCESS_URL's duration must be between 30 minutes and 6 hours"

// validAccessDuration reports whether d is a duration the page allows:
// "between 30 minutes and 6 hours".
func validAccessDuration(d time.Duration) bool {
	return d >= 30*time.Minute && d <= 6*time.Hour
}

// objListSQL is OBJ.LIST(uri [, authorizer]) (#1983), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/objectref_functions
// documents it: a table of the objects uri names (one * wildcard), with
// the columns uri, content_type, size, md5_hash, updated, metadata,
// generation and ref, each ref carrying the authorizer, or NULL for direct
// access. The listing is read from the instance's Cloud Storage when the
// query runs and written into it as a literal table. "Scalar subqueries
// and string functions such as CONCAT aren't supported" for uri, and "URIs
// that end in a slash or contain consecutive slashes return an error".
// Delegated access checks the caller's bigquery.objectRefs.read on the
// connection and its service account's storage.objects.list on the bucket,
// as an object table's; direct access, the caller's own (the instance
// grants it).
//
// UNVERIFIED (re-read 2026-11-20, the page above quotes no error text):
// the wording of the refusals; md5_hash written as Cloud Storage's base64,
// as object tables write it.
func (f front) objListSQL(r *http.Request, c objCall) (string, int, string) {
	if len(c.named) > 0 {
		return "", 400, "OBJ.LIST takes no named arguments"
	}
	if len(c.pos) < 1 || len(c.pos) > 2 {
		return "", 400, "No matching signature for function OBJ.LIST with " + strconv.Itoa(len(c.pos)) + " arguments: it takes (uri [, authorizer])"
	}
	uri, ok := unquoteString(c.pos[0])
	if !ok {
		return "", 400, "OBJ.LIST's uri must be a STRING literal: scalar subqueries and string functions such as CONCAT aren't supported"
	}
	rest, isGS := strings.CutPrefix(uri, "gs://")
	bucket, name, _ := strings.Cut(rest, "/")
	switch {
	case !isGS || bucket == "" || name == "":
		return "", 400, "Invalid Cloud Storage URI " + strconv.Quote(uri) + " of OBJ.LIST: it is gs://BUCKET/OBJECT, with at most one * wildcard"
	case strings.HasSuffix(uri, "/"):
		return "", 400, "Invalid Cloud Storage URI " + strconv.Quote(uri) + " of OBJ.LIST: a URI must not end in a slash"
	case strings.Contains(rest, "//"):
		return "", 400, "Invalid Cloud Storage URI " + strconv.Quote(uri) + " of OBJ.LIST: a URI must not contain consecutive slashes"
	case strings.Count(name, "*") > 1:
		return "", 400, "Invalid Cloud Storage URI " + strconv.Quote(uri) + " of OBJ.LIST: only one * wildcard is supported"
	}
	if f.storage == nil {
		return "", 501, "Not implemented here: OBJ.LIST reads the instance's Cloud Storage, which the BigQuery front was not given (enable Cloud Storage). Nothing was run."
	}
	authorizer := "CAST(NULL AS STRING)"
	auth := ""
	if len(c.pos) == 2 {
		a, ok := unquoteString(c.pos[1])
		if !ok {
			return "", 400, "OBJ.LIST's authorizer must be a STRING literal: a Cloud resource connection"
		}
		// The connection, the caller's use of it and its service
		// account's reading of the object, as an ObjectRef's (refAccess),
		// then its listing of the bucket.
		sa, cname, code, msg := f.refAccess(r, objRef{URI: "gs://" + bucket + "/" + strings.ReplaceAll(name, "*", "x"), Authorizer: &a}, false)
		if code != 0 {
			return "", code, msg
		}
		if code, msg := f.checkBucketAccess(r, projectOf(f.base), sa, bucket); code != 0 {
			return "", code, msg
		}
		auth = cname
		authorizer = sqlString(cname)
	}
	objs, status, err := f.storage.listObjects(r.Context(), uri)
	if err != nil {
		return "", 500, "cloudburrow: the instance's Cloud Storage did not answer listing " + uri + ": " + err.Error()
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		return "", 400, fmt.Sprintf("Cloud Storage answered HTTP %d listing %s", status, uri)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Name < objs[j].Name })
	var rows []string
	for _, o := range objs {
		row := objectRow(o, auth)
		rows = append(rows, objectRowSQL(row))
	}
	typ := "STRUCT<uri STRING, generation INT64, content_type STRING, size INT64, md5_hash STRING, updated TIMESTAMP, " +
		"metadata ARRAY<STRUCT<name STRING, value STRING>>, ref " + objRefType + ">"
	return "(SELECT _cbol.uri, _cbol.content_type, _cbol.size, _cbol.md5_hash, _cbol.updated, _cbol.metadata, _cbol.generation, " +
		"STRUCT(_cbol.ref.uri AS uri, _cbol.ref.version AS version, " + authorizer + " AS authorizer, _cbol.ref.details AS details) AS ref " +
		"FROM UNNEST(ARRAY<" + typ + ">[" + strings.Join(rows, ", ") + "]) AS _cbol)", 0, ""
}
