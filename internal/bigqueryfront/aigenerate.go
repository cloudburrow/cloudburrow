package bigqueryfront

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// BigQuery's scalar generative AI functions, evaluated row by row in the
// front before the engine runs the query (#1765, #1771, #1772), as these
// reference pages document them (read 2026-11-17, under
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/):
//
//	AI.GENERATE([prompt =>] PROMPT [, endpoint] [, model_params]
//	  [, output_schema] [, connection_id] [, request_type])
//	  -> STRUCT<result STRING, full_response JSON, status STRING>, or with
//	     output_schema STRUCT<the schema's fields..., full_response, status>
//	     (bigqueryml-syntax-ai-generate);
//	AI.GENERATE_BOOL / AI.GENERATE_INT / AI.GENERATE_DOUBLE(the same but
//	  output_schema) -> STRUCT<result BOOL / INT64 / FLOAT64,
//	  full_response JSON, status STRING> (bigqueryml-syntax-ai-generate-bool,
//	  -int, -double);
//	AI.IF([prompt =>] PROMPT [, examples] [, connection_id] [, endpoint]
//	  [, embeddings] [, optimization_mode] [, max_error_ratio]) -> BOOL;
//	AI.SCORE([prompt =>] PROMPT [, connection_id] [, endpoint]
//	  [, max_error_ratio]) -> FLOAT64;
//	AI.CLASSIFY([input =>] INPUT, [categories =>] CATEGORIES [, examples]
//	  [, connection_id] [, endpoint] [, output_mode] [, embeddings]
//	  [, optimization_mode] [, max_error_ratio]) -> STRING, or
//	  ARRAY<STRING> with output_mode;
//	AI.EMBED([content =>] CONTENT, {endpoint | model} [, task_type]
//	  [, title] [, model_params] [, connection_id])
//	  -> STRUCT<result ARRAY<FLOAT64>, status STRING>;
//	AI.SIMILARITY(content1 => C1, content2 => C2 [, {endpoint | model}]
//	  [, model_params] [, connection_id]) -> FLOAT64, their embeddings'
//	  cosine similarity.
//
// The prompt is a STRING, or a STRUCT of STRING and ARRAY<STRING> fields
// concatenated in order. The front reads the distinct prompts of the
// call's query block (its WITH clause and FROM clause, with
// TO_JSON_STRING of the prompt), sends each to the instance's local
// Vertex AI endpoint (never Google), and puts the answers in the query as
// a literal table the call is looked up in. The endpoint is resolved by
// the local endpoint's own rule (a gemini-* name answers only through an
// explicitly configured alias; otherwise its NOT_FOUND is the row's
// status, never a substitution). Generation runs on the local LiteRT-LM
// runtime (`up --local-ai-model`), embeddings on the local embedding
// model (`up --local-ai-embeddings`).
//
// Structured answers (output_schema, the BOOL/INT64/FLOAT64 functions and
// the managed functions): the local runtime has no working constrained
// decoding (aischema.go), so the front appends what it asks for to the
// prompt in words and parses the answer; one that does not fit is the
// row's failure: a NULL result and a status (AI.GENERATE_*), or a NULL
// (AI.IF, AI.SCORE, AI.CLASSIFY, which count it against max_error_ratio:
// "Query failed because AI functions exceeded their allowed error ratio"
// when it is exceeded, or when every row fails).
//
// Permissions: with connection_id, the connection's (connectionauth.go);
// without, the caller's own Vertex AI User role on the project ("the
// query runs with the caller's Google Account user credentials, which
// requires the Agent Platform User role"). AI.EMBED's and AI.SIMILARITY's
// model => 'embeddinggemma-300m' runs in BigQuery and takes neither.
//
// model_params of the generating functions sends its generationConfig as
// the request's, which the local endpoint applies or refuses (#1797).
//
// 501, naming why: model_params fields other than generationConfig, and of
// the embedding functions other than outputDimensionality; embeddings
// (optimized mode's distilled model); ObjectRef prompt fields (object
// tables); and a call whose prompts the front cannot read (outside a
// SELECT block, or inside its FROM clause, such as a JOIN's ON).
//
// UNVERIFIED (re-read 2026-11-17: the pages above, which describe no
// NULL prompt and quote no error text but the error ratio's): a NULL
// prompt's row is NULL; the wording of the argument errors; the words
// added to the prompt (aiInstruction); that the error ratio counts
// distinct prompts rather than rows; the endpoint the managed functions
// take when none is given (BigQuery "dynamically chooses" one; here it
// is AI.GENERATE's default, which needs an alias locally); that
// AI.SIMILARITY without endpoint or model is an error (the page names
// no default); and that max_error_ratio is refused with the default
// optimization_mode (the pages: "not supported when optimization_mode is
// MINIMIZE_COST", which is the default).

// aiGenerateDefaultEndpoint is the endpoint AI.GENERATE takes when none is
// given (the page above).
const aiGenerateDefaultEndpoint = "gemini-3.5-flash-lite"

// aiSpec is one scalar AI function's arguments.
type aiSpec struct {
	positional []string // the arguments that may be given unnamed, in order
	named      []string // the others
	key        []string // the arguments the rows' values are read from
}

var aiScalars = map[string]aiSpec{
	"GENERATE":        {[]string{"prompt"}, []string{"endpoint", "model_params", "output_schema", "connection_id", "request_type"}, []string{"prompt"}},
	"GENERATE_BOOL":   {[]string{"prompt"}, []string{"endpoint", "model_params", "connection_id", "request_type"}, []string{"prompt"}},
	"GENERATE_INT":    {[]string{"prompt"}, []string{"endpoint", "model_params", "connection_id", "request_type"}, []string{"prompt"}},
	"GENERATE_DOUBLE": {[]string{"prompt"}, []string{"endpoint", "model_params", "connection_id", "request_type"}, []string{"prompt"}},
	"IF": {[]string{"prompt"}, []string{"examples", "connection_id", "endpoint", "embeddings", "optimization_mode", "max_error_ratio"},
		[]string{"prompt"}},
	"SCORE": {[]string{"prompt"}, []string{"connection_id", "endpoint", "max_error_ratio"}, []string{"prompt"}},
	"CLASSIFY": {[]string{"input", "categories"}, []string{"examples", "connection_id", "endpoint", "output_mode", "embeddings",
		"optimization_mode", "max_error_ratio"}, []string{"input"}},
	"EMBED":      {[]string{"content"}, []string{"endpoint", "model", "task_type", "title", "model_params", "connection_id"}, []string{"content"}},
	"SIMILARITY": {nil, []string{"content1", "content2", "endpoint", "model", "model_params", "connection_id"}, []string{"content1", "content2"}},
}

// aiCall is one scalar AI function call: its tokens' span and its
// arguments' SQL text by name.
type aiCall struct {
	fn         string
	start, end int // byte offsets of AI ... )
	args       map[string]string
	block      int // index of the block's SELECT token
}

// name is the call's function name.
func (c aiCall) name() string { return "AI." + c.fn }

// keyExpr is the SQL expression the call's rows are told apart by.
func (c aiCall) keyExpr() string {
	keys := aiScalars[c.fn].key
	if len(keys) == 1 {
		return c.args[keys[0]]
	}
	var fs []string
	for i, k := range keys {
		fs = append(fs, c.args[k]+" AS _"+strconv.Itoa(i))
	}
	return "STRUCT(" + strings.Join(fs, ", ") + ")"
}

// depths are each token's paren depth.
func tokenDepths(toks []token) []int {
	d := make([]int, len(toks))
	n := 0
	for i, t := range toks {
		if t.punct(")") {
			n--
		}
		d[i] = n
		if t.punct("(") {
			n++
		}
	}
	return d
}

// findAICalls finds the scalar AI function calls of sql.
func findAICalls(sql string, toks []token, depth []int) ([]aiCall, int, string) {
	var calls []aiCall
	for i := 0; i+3 < len(toks); i++ {
		if !toks[i].is("AI") || !toks[i+1].punct(".") || toks[i+2].kind != tokWord || !toks[i+3].punct("(") || i > 0 && toks[i-1].punct(".") {
			continue
		}
		fn := strings.ToUpper(toks[i+2].text)
		spec, ok := aiScalars[fn]
		if !ok {
			continue
		}
		name := "AI." + fn
		end := matchParen(toks, i+3)
		if end < 0 {
			return nil, 400, "Syntax error: unclosed " + name + "("
		}
		c := aiCall{fn: fn, start: toks[i].pos, end: toks[end].end, args: map[string]string{}, block: -1}
		args := splitCallArgs(toks, depth, i+3, end)
		npos := 0
		for _, a := range args {
			if len(a) == 0 {
				return nil, 400, "Syntax error: an empty argument of " + name
			}
			text := strings.TrimSpace(sql[a[0].pos:a[len(a)-1].end])
			if len(a) > 2 && a[0].kind == tokWord && a[1].punct("=") && a[2].punct(">") {
				arg := strings.ToLower(a[0].text)
				if len(a) == 3 {
					return nil, 400, "Syntax error: no value for the " + arg + " argument of " + name
				}
				if !slices.Contains(spec.positional, arg) && !slices.Contains(spec.named, arg) {
					return nil, 400, "Unknown argument " + arg + " of " + name
				}
				if _, dup := c.args[arg]; dup {
					return nil, 400, name + " was given its " + arg + " argument twice"
				}
				c.args[arg] = strings.TrimSpace(sql[a[3].pos:a[len(a)-1].end])
				continue
			}
			if npos >= len(spec.positional) || len(c.args) > npos {
				if len(spec.positional) == 0 {
					return nil, 400, name + " takes named arguments only (content1 => ..., content2 => ...)"
				}
				return nil, 400, name + " takes " + strings.Join(spec.positional, " and ") + " as its only positional arguments, first; name the others (endpoint => ...)"
			}
			c.args[spec.positional[npos]] = text
			npos++
		}
		for _, k := range append(slices.Clone(spec.key), map[string][]string{"CLASSIFY": {"categories"}}[fn]...) {
			if c.args[k] == "" {
				return nil, 400, name + " requires its " + k + " argument"
			}
		}
		c.block = enclosingSelect(toks, depth, i)
		if c.block < 0 {
			return nil, 501, "Not implemented here: " + name + " outside a SELECT query block. Nothing was run."
		}
		calls = append(calls, c)
		i = end
	}
	return calls, 0, ""
}

// enclosingSelect is the index of the SELECT of the query block holding
// toks[i]: the nearest SELECT before it at the lowest depth seen walking
// back, or -1.
func enclosingSelect(toks []token, depth []int, i int) int {
	m := depth[i]
	for k := i - 1; k >= 0; k-- {
		m = min(m, depth[k])
		if toks[k].is("SELECT") && depth[k] == m {
			return k
		}
	}
	return -1
}

// splitCallArgs splits the arguments of the call whose parens are toks[open]
// and toks[end] at its top-level commas.
func splitCallArgs(toks []token, depth []int, open, end int) [][]token {
	var args [][]token
	k0 := open + 1
	brackets := 0 // [ ] of ARRAY literals and < > of their types, which are not parens
	for k := k0; k <= end; k++ {
		switch {
		case toks[k].punct("["):
			brackets++
		case toks[k].punct("]"):
			brackets--
		case toks[k].punct("<") && (toks[k-1].is("ARRAY") || toks[k-1].is("STRUCT")):
			brackets++
		case toks[k].punct(">") && brackets > 0 && !toks[k-1].punct("="):
			brackets--
		}
		if k == end || toks[k].punct(",") && depth[k] == depth[open]+1 && brackets == 0 {
			args = append(args, toks[k0:k])
			k0 = k + 1
		}
	}
	return args
}

// blockSource is the text the prompts of a block are read over: the
// query's WITH clause, and FROM … up to the block's next clause.
func blockSource(sql string, toks []token, depth []int, sel int) string {
	d := depth[sel]
	from := -1
	for k := sel + 1; k < len(toks) && depth[k] >= d; k++ {
		if depth[k] == d && toks[k].is("FROM") {
			from = k
			break
		}
		if depth[k] == d && (toks[k].is("UNION") || toks[k].is("INTERSECT") || toks[k].punct(";")) {
			break
		}
	}
	src := ""
	if from >= 0 {
		end := len(sql)
		for k := from + 1; k < len(toks); k++ {
			if depth[k] < d || toks[k].punct(";") {
				end = toks[k].pos
				break
			}
			if depth[k] == d {
				for _, w := range []string{"WHERE", "GROUP", "HAVING", "QUALIFY", "WINDOW", "ORDER", "LIMIT", "UNION", "INTERSECT", "EXCEPT"} {
					if toks[k].is(w) {
						end = toks[k].pos
						break
					}
				}
				if end != len(sql) {
					break
				}
			}
		}
		src = " " + sql[toks[from].pos:end]
	}
	with := ""
	if len(toks) > 0 && toks[0].is("WITH") {
		for k := 1; k < len(toks); k++ {
			if depth[k] == 0 && toks[k].is("SELECT") {
				with = sql[:toks[k].pos]
				break
			}
		}
	}
	if src == "" {
		// A query with no FROM clause takes no DISTINCT: one row.
		return with + "SELECT TO_JSON_STRING(%s) AS _cbai_k"
	}
	return with + "SELECT DISTINCT TO_JSON_STRING(%s) AS _cbai_k" + src
}

// promptText is the prompt TO_JSON_STRING wrote: a string, or a STRUCT's
// STRING and ARRAY<STRING> fields concatenated in order. ok is false for
// a NULL prompt.
func promptText(k string) (string, bool, int, string) {
	dec := json.NewDecoder(strings.NewReader(k))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return "", false, 500, "cloudburrow: unreadable prompt " + k
	}
	switch v := tok.(type) {
	case nil:
		return "", false, 0, ""
	case string:
		return v, true, 0, ""
	case json.Delim:
		if v != '{' {
			break
		}
		var b strings.Builder
		for dec.More() {
			if _, err := dec.Token(); err != nil { // the field's name
				return "", false, 500, "cloudburrow: unreadable prompt " + k
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return "", false, 500, "cloudburrow: unreadable prompt " + k
			}
			raw = bytes.TrimSpace(raw)
			switch {
			case string(raw) == "null":
				// CONCAT of a NULL is NULL.
				return "", false, 0, ""
			case raw[0] == '"':
				var s string
				_ = json.Unmarshal(raw, &s)
				b.WriteString(s)
			case raw[0] == '[':
				var ss []*string
				if json.Unmarshal(raw, &ss) != nil {
					return "", false, 501, "Not implemented here: an AI.GENERATE prompt field that is an ARRAY of ObjectRef values (object tables, multimodal prompts, #1765). Nothing was run."
				}
				for _, s := range ss {
					if s != nil {
						b.WriteString(*s)
					}
				}
			case raw[0] == '{':
				return "", false, 501, "Not implemented here: an AI.GENERATE prompt field that is an ObjectRef (object tables, multimodal prompts, #1765). Nothing was run."
			default:
				return "", false, 400, "AI.GENERATE's prompt STRUCT fields must be STRING, ARRAY<STRING>, ObjectRef or ARRAY<ObjectRef>"
			}
		}
		return b.String(), true, 0, ""
	}
	return "", false, 400, "AI.GENERATE's prompt must be a STRING or a STRUCT"
}

// aiResolved is a call's checked arguments.
type aiResolved struct {
	rm           *remoteModel
	schema       []aiField // output_schema
	examples     [][]json.RawMessage
	categories   [][2]string // name, description
	multi        bool        // output_mode => 'multi'
	arrayOut     bool        // output_mode given
	maxErrRatio  float64
	taskType     string
	title        string
	dims         int
	embedNoCheck bool           // model => 'embeddinggemma-300m': runs in BigQuery
	genCfg       map[string]any // model_params' generationConfig (#1797)
}

// aiOutput is a call's value's type: its fields (name TYPE), and whether
// it is a scalar (one field, v) rather than a STRUCT.
func (c aiCall) output(a aiResolved) ([]string, bool) {
	switch c.fn {
	case "GENERATE":
		if a.schema != nil {
			var fs []string
			for _, f := range a.schema {
				fs = append(fs, sqlIdent(f.name)+" "+f.typ.sql())
			}
			return append(fs, "full_response JSON", "status STRING"), false
		}
		return []string{"result STRING", "full_response JSON", "status STRING"}, false
	case "GENERATE_BOOL":
		return []string{"result BOOL", "full_response JSON", "status STRING"}, false
	case "GENERATE_INT":
		return []string{"result INT64", "full_response JSON", "status STRING"}, false
	case "GENERATE_DOUBLE":
		return []string{"result FLOAT64", "full_response JSON", "status STRING"}, false
	case "IF":
		return []string{"v BOOL"}, true
	case "SCORE", "SIMILARITY":
		return []string{"v FLOAT64"}, true
	case "CLASSIFY":
		if a.arrayOut {
			return []string{"v ARRAY<STRING>"}, true
		}
		return []string{"v STRING"}, true
	case "EMBED":
		return []string{"result ARRAY<FLOAT64>", "status STRING"}, false
	}
	return nil, false
}

// rewriteAIGenerate evaluates text's AI function calls and writes their
// answers into it: the scalar ones first, then AI.AGG (aiagg.go, #1777),
// whose groups' rows may hold the scalars' answers; text is returned as it
// was when it has none. The parameters returned are the ones the text
// holds then (rewriteAIAgg, #1810).
func (f front) rewriteAIGenerate(r *http.Request, q queryOptions, text string) (string, queryOptions, int, string) {
	text, code, msg := f.rewriteAIScalars(r, q, text)
	if code != 0 {
		return "", q, code, msg
	}
	return f.rewriteAIAgg(r, q, text)
}

// rewriteAIScalars evaluates text's scalar AI function calls.
func (f front) rewriteAIScalars(r *http.Request, q queryOptions, text string) (string, int, string) {
	if !strings.Contains(strings.ToUpper(text), "AI") {
		return text, 0, ""
	}
	toks, ok := lex(text)
	if !ok {
		return text, 0, ""
	}
	depth := tokenDepths(toks)
	calls, code, msg := findAICalls(text, toks, depth)
	if code != 0 || len(calls) == 0 {
		return text, code, msg
	}
	if f.vertexai == "" {
		return "", 501, "Not implemented here: " + calls[0].name() + " needs the instance's local Vertex AI endpoint, which this instance does not serve to BigQuery: " +
			"start it with --local-ai-model (generation) or --local-ai-embeddings (embeddings) (docs/generation.md). CloudBurrow never sends a call to Google. Nothing was run."
	}
	project := projectOf(f.base)
	var out strings.Builder
	last := 0
	for _, c := range calls {
		a, code, msg := f.resolveAICall(r, q, project, c)
		if code != 0 {
			return "", code, msg
		}
		probe := fmt.Sprintf(blockSource(text, toks, depth, c.block), c.keyExpr())
		if strings.Contains(probe, text[c.start:c.end]) {
			return "", 501, "Not implemented here: " + c.name() + " inside a FROM clause (such as a JOIN's ON condition), whose prompts the front cannot read " +
				"before the join runs: filter in WHERE instead. Nothing was run."
		}
		_, rows, code, msg := f.queryAllRows(r, q, probe)
		if code != 0 {
			return "", code, msg
		}
		var lits []string
		failed, total := 0, 0
		firstStatus := ""
		for _, row := range rows {
			if len(row) != 1 || row[0] == nil {
				continue
			}
			vals, status, ok, code, msg := f.aiRow(r, project, c, a, *row[0])
			if code != 0 {
				return "", code, msg
			}
			if !ok {
				continue
			}
			total++
			if status != "" {
				failed++
				if firstStatus == "" {
					firstStatus = status
				}
			}
			lits = append(lits, "STRUCT("+sqlString(*row[0])+", "+strings.Join(vals, ", ")+")")
		}
		if c.fn == "IF" || c.fn == "SCORE" || c.fn == "CLASSIFY" {
			if total > 0 && (failed == total || float64(failed)/float64(total) > a.maxErrRatio) {
				return "", 400, "Query failed because AI functions exceeded their allowed error ratio: " + c.name() + " failed for " +
					strconv.Itoa(failed) + " of " + strconv.Itoa(total) + " distinct prompts; the first: " + firstStatus
			}
		}
		fields, scalar := c.output(a)
		var sel []string
		for _, fd := range fields {
			sel = append(sel, "_cbai."+strings.Fields(fd)[0])
		}
		head := "(SELECT AS STRUCT " + strings.Join(sel, ", ")
		if scalar {
			head = "(SELECT _cbai.v"
		}
		out.WriteString(text[last:c.start])
		out.WriteString(head + " FROM UNNEST(ARRAY<STRUCT<_cbai_k STRING, " + strings.Join(fields, ", ") + ">>[" + strings.Join(lits, ", ") +
			"]) AS _cbai WHERE _cbai._cbai_k = TO_JSON_STRING(" + c.keyExpr() + "))")
		last = c.end
	}
	out.WriteString(text[last:])
	return out.String(), 0, ""
}

// resolveAICall checks a call's arguments and permissions.
func (f front) resolveAICall(r *http.Request, q queryOptions, project string, c aiCall) (aiResolved, int, string) {
	name := c.name()
	a := aiResolved{maxErrRatio: 1}
	embedding := c.fn == "EMBED" || c.fn == "SIMILARITY"
	endpoint := ""
	if !embedding {
		endpoint = aiGenerateDefaultEndpoint
	}
	loc := "us"
	var connection *bqConnection
	str := func(arg string) (string, bool) {
		s, ok := unquoteString(c.args[arg])
		return s, ok
	}
	optMode := "MINIMIZE_COST"
	_, hasRatio := c.args["max_error_ratio"]
	for arg, v := range c.args {
		switch arg {
		case "prompt", "input", "content", "content1", "content2", "instruction":
		case "endpoint":
			s, ok := str(arg)
			if !ok || strings.TrimSpace(s) == "" {
				return a, 400, "The endpoint argument of " + name + " must be a STRING literal"
			}
			endpoint = s
		case "model":
			s, ok := str(arg)
			if !ok || s != "embeddinggemma-300m" {
				return a, 400, "The model argument of " + name + " must be 'embeddinggemma-300m', the only model it supports"
			}
			for _, other := range []string{"endpoint", "title", "model_params", "connection_id"} {
				if _, given := c.args[other]; given {
					return a, 400, name + " takes model or " + other + ", not both"
				}
			}
			endpoint, a.embedNoCheck = s, true
		case "connection_id":
			s, ok := str(arg)
			if !ok {
				return a, 400, "The connection_id argument of " + name + " must be a STRING literal"
			}
			parts := strings.Split(s, ".")
			if len(parts) == 2 {
				parts = append([]string{project}, parts...)
			}
			if len(parts) != 3 {
				return a, 400, "Invalid connection_id " + s + ": expected `[PROJECT_ID].LOCATION.CONNECTION_ID`"
			}
			cn, ok := f.connections.lookup(connectionProject(parts[0], f.numbers), parts[1], parts[2])
			if !ok {
				return a, 404, "Not found: Connection " + s
			}
			if code, msg := f.checkConnectionUse(r.Context(), parts[0], project, cn); code != 0 {
				return a, code, msg
			}
			connection, loc = cn, cn.Location
		case "request_type":
			s, _ := str(arg)
			if s := strings.ToUpper(s); s != "SHARED" && s != "DEDICATED" && s != "UNSPECIFIED" {
				return a, 400, "request_type must be 'SHARED', 'DEDICATED' or 'UNSPECIFIED'"
			}
		case "output_schema":
			s, ok := str(arg)
			if !ok {
				return a, 400, "The output_schema argument of " + name + " must be a STRING literal"
			}
			fields, msg := parseOutputSchema(s)
			if msg != "" {
				return a, 400, msg
			}
			a.schema = fields
		case "model_params":
			if !embedding {
				// "must conform to the generateContent request body
				// format", any field but contents: its generationConfig
				// is sent as the request's, for the local endpoint to
				// apply or refuse (#1797); the other fields are 501.
				raw, ok := jsonArgument(v)
				var mp map[string]json.RawMessage
				if !ok || json.Unmarshal([]byte(raw), &mp) != nil {
					return a, 400, "The model_params argument of " + name + " must be a JSON literal object"
				}
				for k, x := range mp {
					if k != "generationConfig" && k != "generation_config" {
						return a, 501, "Not implemented here: model_params." + k + " of " + name + ": the local runtime applies generationConfig alone, " +
							"and CloudBurrow refuses a parameter rather than ignoring it. Nothing was run."
					}
					var gc map[string]any
					if json.Unmarshal(x, &gc) != nil {
						return a, 400, "model_params." + k + " of " + name + " must be a JSON object"
					}
					a.genCfg = gc
				}
				continue
			}
			raw, ok := jsonArgument(v)
			var mp map[string]json.RawMessage
			if !ok || json.Unmarshal([]byte(raw), &mp) != nil {
				return a, 400, "The model_params argument of " + name + " must be a JSON literal object"
			}
			for k, x := range mp {
				if k != "outputDimensionality" {
					return a, 501, "Not implemented here: model_params." + k + " of " + name + ": the local embedding model takes outputDimensionality alone, " +
						"and CloudBurrow refuses a parameter rather than ignoring it. Nothing was run."
				}
				n, ok := aiJSONInt(x)
				if !ok || n <= 0 {
					return a, 400, "outputDimensionality must be a positive integer"
				}
				a.dims = int(n)
			}
		case "task_type":
			s, ok := str(arg)
			valid := []string{"RETRIEVAL_QUERY", "RETRIEVAL_DOCUMENT", "SEMANTIC_SIMILARITY", "CLASSIFICATION", "CLUSTERING", "QUESTION_ANSWERING",
				"FACT_VERIFICATION", "CODE_RETRIEVAL_QUERY"}
			if !ok || !slices.Contains(valid, strings.ToUpper(s)) {
				return a, 400, "task_type must be one of " + strings.Join(valid, ", ")
			}
			a.taskType = strings.ToUpper(s)
		case "title":
			s, ok := str(arg)
			if !ok {
				return a, 400, "The title argument of " + name + " must be a STRING literal"
			}
			a.title = s
		case "examples", "categories":
			vals, code, msg := f.literalRows(r, q, v)
			if code != 0 {
				return a, code, msg
			}
			if arg == "examples" {
				for _, e := range vals {
					if len(e) != 2 {
						return a, 400, "The examples of " + name + " must be an ARRAY of STRUCTs of an input and its answer"
					}
				}
				a.examples = vals
				continue
			}
			for _, e := range vals {
				var nm, desc string
				if len(e) == 0 || json.Unmarshal(e[0], &nm) != nil || strings.TrimSpace(nm) == "" || len(e) > 2 ||
					len(e) == 2 && json.Unmarshal(e[1], &desc) != nil {
					return a, 400, "The categories of AI.CLASSIFY must be an ARRAY<STRING> or ARRAY<STRUCT<STRING, STRING>> of names and descriptions"
				}
				a.categories = append(a.categories, [2]string{nm, desc})
			}
			if len(a.categories) == 0 {
				return a, 400, "AI.CLASSIFY requires at least one category"
			}
		case "output_mode":
			s, _ := str(arg)
			switch strings.ToLower(s) {
			case "single":
			case "multi":
				a.multi = true
			default:
				return a, 400, "output_mode must be 'single' or 'multi'"
			}
			a.arrayOut = true
		case "embeddings":
			return a, 501, "Not implemented here: the embeddings argument of " + name + " (optimized mode, whose local distilled model BigQuery does not publish). " +
				"Without it every row is answered by the model. Nothing was run."
		case "optimization_mode":
			s, _ := str(arg)
			if s := strings.ToUpper(s); s != "MINIMIZE_COST" && s != "MAXIMIZE_QUALITY" {
				return a, 400, "optimization_mode must be 'MINIMIZE_COST' or 'MAXIMIZE_QUALITY'"
			}
			optMode = strings.ToUpper(s)
		case "max_error_ratio":
			x, err := strconv.ParseFloat(v, 64)
			if err != nil || x < 0 || x > 1 {
				return a, 400, "max_error_ratio must be a FLOAT64 between 0.0 and 1.0"
			}
			a.maxErrRatio = x
		}
	}
	if hasRatio && optMode == "MINIMIZE_COST" && (c.fn == "IF" || c.fn == "CLASSIFY") {
		return a, 400, "max_error_ratio is not supported when optimization_mode is MINIMIZE_COST: pass optimization_mode => 'MAXIMIZE_QUALITY'"
	}
	if a.title != "" && a.taskType != "RETRIEVAL_DOCUMENT" {
		return a, 400, "title can only be used with task_type RETRIEVAL_DOCUMENT"
	}
	if embedding && endpoint == "" {
		return a, 400, name + " requires an endpoint or model argument"
	}
	if strings.HasPrefix(strings.ToLower(endpointModel(endpoint)), "gemini-embedding-2") && (a.taskType != "" || a.title != "") {
		return a, 400, "task_type and title are not compatible with the gemini-embedding-2 endpoint"
	}
	if a.multi && c.fn == "CLASSIFY" {
		for _, e := range a.examples {
			var ss []string
			if json.Unmarshal(e[1], &ss) != nil {
				return a, 400, "With output_mode 'multi' the examples of AI.CLASSIFY must be ARRAY<STRUCT<STRING, ARRAY<STRING>>>"
			}
		}
	}
	if connection == nil && !a.embedNoCheck {
		// The caller's own credentials: its Vertex AI User role.
		proj, code, msg := f.projectBindings(r.Context(), project)
		if code != 0 {
			return a, code, msg
		}
		if !bindingsGrant(proj, f.members(r.Context(), "serviceAccount:"+callerIdentity(project)), permVertexPredict) {
			return a, 403, fmt.Sprintf("Access Denied: Project %s: The user %s does not have permission %s: "+
				"grant the Vertex AI user role (roles/aiplatform.user), or pass a connection_id.", project, callerIdentity(project), permVertexPredict)
		}
	}
	model := endpointModel(endpoint)
	if model == "" {
		return a, 400, "Invalid endpoint " + endpoint
	}
	a.rm = &remoteModel{Model: model, Endpoint: endpoint, Location: cmp.Or(loc, "us"), Embedding: embedding}
	return a, 0, ""
}

// jsonArgument reads a JSON '...' literal (or a STRING literal).
func jsonArgument(v string) (string, bool) {
	s := strings.TrimSpace(v)
	if len(s) > 4 && strings.EqualFold(s[:4], "JSON") {
		s = strings.TrimSpace(s[4:])
	}
	return unquoteString(s)
}

// literalRows evaluates an ARRAY argument (examples, categories) on the
// engine: each element's values in order (a STRING element is one value).
func (f front) literalRows(r *http.Request, q queryOptions, expr string) ([][]json.RawMessage, int, string) {
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT TO_JSON_STRING("+expr+")")
	if code != 0 {
		return nil, code, msg
	}
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] == nil {
		return nil, 400, "expected an ARRAY literal, not " + expr
	}
	var items []json.RawMessage
	if json.Unmarshal([]byte(*rows[0][0]), &items) != nil {
		return nil, 400, "expected an ARRAY literal, not " + expr
	}
	var out [][]json.RawMessage
	for _, it := range items {
		it = bytes.TrimSpace(it)
		if len(it) == 0 || it[0] != '{' {
			out = append(out, []json.RawMessage{it})
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(it))
		var vals []json.RawMessage
		_, _ = dec.Token()
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return nil, 500, "cloudburrow: unreadable element " + string(it)
			}
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				return nil, 500, "cloudburrow: unreadable element " + string(it)
			}
			vals = append(vals, raw)
		}
		out = append(out, vals)
	}
	return out, 0, ""
}

// aiRow is one distinct key's value literals and failure status; ok is
// false for a key whose row is NULL (a NULL prompt).
func (f front) aiRow(r *http.Request, project string, c aiCall, a aiResolved, key string) ([]string, string, bool, int, string) {
	ctx := r.Context()
	if c.fn == "SIMILARITY" {
		var pair struct {
			A *string `json:"_0"`
			B *string `json:"_1"`
		}
		if json.Unmarshal([]byte(key), &pair) != nil {
			return nil, "", false, 501, "Not implemented here: AI.SIMILARITY of content that is not a STRING (ObjectRef or STRUCT multimodal content, #1772). Nothing was run."
		}
		if pair.A == nil || pair.B == nil {
			return nil, "", false, 0, ""
		}
		va, status := f.embedVector(ctx, project, a, *pair.A)
		if status == "" {
			var vb []float64
			vb, status = f.embedVector(ctx, project, a, *pair.B)
			if status == "" {
				if s, ok := cosine(va, vb); ok {
					return []string{sqlF(s)}, "", true, 0, ""
				}
				status = "the embeddings differ in length or are zero"
			}
		}
		return []string{"CAST(NULL AS FLOAT64)"}, status, true, 0, ""
	}
	prompt, ok, code, msg := promptText(key)
	if code != 0 || !ok {
		return nil, "", false, code, strings.ReplaceAll(msg, "AI.GENERATE", c.name())
	}
	if c.fn == "EMBED" {
		if strings.HasPrefix(strings.TrimSpace(key), "{") {
			return nil, "", false, 501, "Not implemented here: AI.EMBED of a STRUCT (gemini-embedding-2 multimodal content, #1772). Nothing was run."
		}
		v, status := f.embedVector(ctx, project, a, prompt)
		if status != "" {
			return []string{"CAST(NULL AS ARRAY<FLOAT64>)", sqlString(status)}, status, true, 0, ""
		}
		lits := make([]string, len(v))
		for i, x := range v {
			lits[i] = sqlF(x)
		}
		return []string{"ARRAY<FLOAT64>[" + strings.Join(lits, ", ") + "]", sqlString("")}, "", true, 0, ""
	}
	full := prompt + aiInstruction(c, a)
	if c.fn == "CLASSIFY" {
		full = classifyPrompt(prompt, a)
	}
	text, raw, status := f.aiGenerateText(r, project, a.rm, full, aiResponseSchema(c.fn, a.schema), a.genCfg)
	fields, _ := c.output(a)
	nulls := func() []string {
		var out []string
		for _, fd := range fields {
			if strings.HasPrefix(fd, "full_response ") || strings.HasPrefix(fd, "status ") {
				continue
			}
			out = append(out, "CAST(NULL AS "+strings.SplitN(fd, " ", 2)[1]+")")
		}
		return out
	}
	withMeta := func(vals []string, status string) []string {
		if strings.HasPrefix(c.fn, "GENERATE") {
			return append(vals, jsonLiteral(raw), sqlString(status))
		}
		return vals
	}
	if status != "" {
		return withMeta(nulls(), status), status, true, 0, ""
	}
	var vals []string
	var why string
	switch c.fn {
	case "GENERATE":
		if a.schema == nil {
			return withMeta([]string{sqlString(text)}, ""), "", true, 0, ""
		}
		vals, why = schemaLiterals(a.schema, text)
	case "GENERATE_BOOL", "IF":
		var v string
		v, why = parseBoolAnswer(text)
		vals = []string{v}
	case "GENERATE_INT":
		var v string
		v, why = parseIntAnswer(text)
		vals = []string{v}
	case "GENERATE_DOUBLE", "SCORE":
		var v string
		v, why = parseFloatAnswer(text)
		vals = []string{v}
	case "CLASSIFY":
		var v string
		v, why = parseCategories(text, a)
		vals = []string{v}
	}
	if why != "" {
		return withMeta(nulls(), why), why, true, 0, ""
	}
	return withMeta(vals, ""), "", true, 0, ""
}

// aiInstruction is what the front appends to a prompt to ask for the
// call's answer (UNVERIFIED, re-read 2026-11-17: the reference pages above
// publish none of Google's wording; it
// constrains Gemini with a response schema instead).
func aiInstruction(c aiCall, a aiResolved) string {
	examples := func(answer func(json.RawMessage) string) string {
		if len(a.examples) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString("\n\nExamples:")
		for _, e := range a.examples {
			var in string
			_ = json.Unmarshal(e[0], &in)
			fmt.Fprintf(&b, "\nInput: %s\nAnswer: %s", in, answer(e[1]))
		}
		return b.String()
	}
	raw := func(v json.RawMessage) string { return string(v) }
	switch c.fn {
	case "GENERATE":
		if a.schema != nil {
			return aiSchemaInstruction(a.schema)
		}
	case "GENERATE_BOOL":
		return "\n\nAnswer with only true or false."
	case "GENERATE_INT":
		return "\n\nAnswer with only an integer."
	case "GENERATE_DOUBLE":
		return "\n\nAnswer with only a number."
	case "IF":
		return examples(raw) + "\n\nIs this true for the input? Answer with only true or false."
	case "SCORE":
		return "\n\nAnswer with only the score, a number."
	}
	return ""
}

// classifyPrompt is AI.CLASSIFY's prompt (UNVERIFIED, re-read 2026-11-17:
// bigqueryml-syntax-ai-classify publishes no prompt; as aiInstruction).
func classifyPrompt(input string, a aiResolved) string {
	var b strings.Builder
	if a.multi {
		b.WriteString("Classify the input into every one of these categories that applies (it may be none, one or several):\n")
	} else {
		b.WriteString("Classify the input into exactly one of these categories:\n")
	}
	for _, cat := range a.categories {
		b.WriteString("- " + cat[0])
		if cat[1] != "" {
			b.WriteString(": " + cat[1])
		}
		b.WriteString("\n")
	}
	for i, e := range a.examples {
		if i == 0 {
			b.WriteString("\nExamples:\n")
		}
		var in string
		_ = json.Unmarshal(e[0], &in)
		fmt.Fprintf(&b, "Input: %s\nAnswer: %s\n", in, jsonBody(string(e[1])))
	}
	b.WriteString("\nInput: " + input + "\n\n")
	if a.multi {
		b.WriteString("Answer with only a JSON array of the names of the categories that apply.")
	} else {
		b.WriteString("Answer with only the category's name.")
	}
	return b.String()
}

// parseCategories reads AI.CLASSIFY's answer as its SQL value.
func parseCategories(text string, a aiResolved) (string, string) {
	match := func(s string) (string, bool) {
		s = scalarAnswer(s)
		for _, cat := range a.categories {
			if strings.EqualFold(s, cat[0]) {
				return cat[0], true
			}
		}
		return "", false
	}
	if !a.multi {
		name, ok := match(text)
		if !ok {
			return "", "The model's response is not one of the categories: " + strconv.Quote(strings.TrimSpace(text))
		}
		if a.arrayOut {
			return "ARRAY<STRING>[" + sqlString(name) + "]", ""
		}
		return sqlString(name), ""
	}
	var got []string
	if json.Unmarshal([]byte(jsonBody(text)), &got) != nil {
		return "", "The model's response is not a JSON array of categories: " + strconv.Quote(strings.TrimSpace(text))
	}
	var lits []string
	seen := map[string]bool{}
	for _, g := range got {
		name, ok := match(g)
		if !ok {
			return "", "The model's response names a category that is not one of them: " + strconv.Quote(g)
		}
		if !seen[name] {
			seen[name] = true
			lits = append(lits, sqlString(name))
		}
	}
	return "ARRAY<STRING>[" + strings.Join(lits, ", ") + "]", ""
}

// aiGenerateText sends one prompt to the local generateContent: its text,
// the raw response and the failure status.
//
// schema, when not nil, is the Vertex AI responseSchema the answer is
// constrained to, as BigQuery sends Gemini one (aiResponseSchema, #1779).
//
// cfg, when not nil, is the request's generationConfig (model_params'); the
// schema's fields replace any of its own.
func (f front) aiGenerateText(r *http.Request, project string, rm *remoteModel, prompt string, schema, cfg map[string]any) (string, []byte, string) {
	gc := map[string]any{}
	for k, v := range cfg {
		gc[k] = v
	}
	if schema != nil {
		gc["responseMimeType"] = "application/json"
		gc["responseSchema"] = schema
	}
	body := generateBody(prompt, gc)
	raw, status := f.vertexCall(r.Context(), project, rm, "generateContent", body)
	if status != "" {
		return "", raw, status
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &resp) != nil || len(resp.Candidates) == 0 {
		return "", raw, "The response had no candidates"
	}
	var b strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String(), raw, ""
}

// cosine is two vectors' cosine similarity.
func cosine(a, b []float64) (float64, bool) {
	if len(a) != len(b) || len(a) == 0 {
		return 0, false
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0, false
	}
	return dot / math.Sqrt(na*nb), true
}

// embedVector is one content's embedding on the local embedding model, or
// the failure status.
func (f front) embedVector(ctx context.Context, project string, a aiResolved, content string) ([]float64, string) {
	inst := map[string]any{"content": content}
	if a.taskType != "" {
		inst["task_type"] = a.taskType
	}
	if a.title != "" {
		inst["title"] = a.title
	}
	body := map[string]any{"instances": []any{inst}}
	if a.dims > 0 {
		body["parameters"] = map[string]any{"outputDimensionality": a.dims}
	}
	out, status := f.vertexCall(ctx, project, a.rm, "predict", body)
	if status != "" {
		return nil, status
	}
	var resp struct {
		Predictions []struct {
			Embeddings struct {
				Values []float64 `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if json.Unmarshal(out, &resp) != nil || len(resp.Predictions) != 1 || len(resp.Predictions[0].Embeddings.Values) == 0 {
		return nil, "The response had no prediction"
	}
	return resp.Predictions[0].Embeddings.Values, ""
}
