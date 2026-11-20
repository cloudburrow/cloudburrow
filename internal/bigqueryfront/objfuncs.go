package bigqueryfront

import (
	"encoding/json"
	"net/http"
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
		// OBJ.MAKE_REF first (it is SQL alone), then the deepest call,
		// so that a probe of a block's rows holds no OBJ call.
		var c objCall
		for _, k := range calls {
			switch {
			case k.inner:
			case c.fn == "", k.fn == "MAKE_REF" && c.fn != "MAKE_REF", (k.fn == "MAKE_REF") == (c.fn == "MAKE_REF") && k.depth > c.depth:
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
		return "", 501, "Not implemented here: OBJ.LIST, the table function over a Cloud Storage listing: list the objects with an object table " +
			"(CREATE EXTERNAL TABLE ... OPTIONS(object_metadata = 'SIMPLE', uris = [...])), whose ref column is each object's ObjectRef. Nothing was run."
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
	if c.fn == "GET_ACCESS_URL" {
		mode, ok := unquoteString(c.pos[1])
		if !ok {
			return "", 501, "Not implemented here: an OBJ.GET_ACCESS_URL mode that is not a STRING literal ('r' or 'rw'). Nothing was run."
		}
		switch strings.ToLower(mode) {
		case "r":
		case "rw":
			write = true
		default:
			return "", 400, "OBJ.GET_ACCESS_URL's mode must be 'r' or 'rw', not " + c.pos[1]
		}
		if len(c.pos) == 3 {
			ms, ok := intervalLiteralMs(lexOrNone(c.pos[2]))
			if !ok {
				return "", 501, "Not implemented here: an OBJ.GET_ACCESS_URL duration that is not an INTERVAL literal. Nothing was run."
			}
			dur = time.Duration(ms) * time.Millisecond
			if dur < 30*time.Minute || dur > 6*time.Hour {
				return "", 400, "OBJ.GET_ACCESS_URL's duration must be between 30 minutes and 6 hours"
			}
		}
	}
	if c.block < 0 {
		return "", 501, "Not implemented here: " + name + " outside a SELECT query block. Nothing was run."
	}
	arg := c.pos[0]
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
		var refs []objRef
		if strings.HasPrefix(strings.TrimSpace(key), "[") {
			if c.fn == "GET_READ_URL" {
				return "", 400, "No matching signature for function OBJ.GET_READ_URL for argument types: ARRAY"
			}
			isArray = true
			var items []json.RawMessage
			if json.Unmarshal([]byte(key), &items) != nil {
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
			o, ok := parseObjRef([]byte(key))
			if !ok {
				return "", 400, name + " takes an ObjectRef or ARRAY<ObjectRef>, not " + key
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
