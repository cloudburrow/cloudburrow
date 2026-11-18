package bigqueryfront

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// AI.GENERATE (#1765), the scalar generative AI function, as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-generate
// documents it (read 2026-11-17):
//
//	AI.GENERATE([prompt =>] PROMPT [, endpoint => 'ENDPOINT']
//	  [, model_params => JSON] [, output_schema => 'SCHEMA']
//	  [, connection_id => 'CONNECTION'] [, request_type => 'TYPE'])
//
// returning STRUCT<result STRING, full_response JSON, status STRING> a
// row. The prompt is a STRING, or a STRUCT of STRING and ARRAY<STRING>
// fields concatenated in order.
//
// The front evaluates it row by row before the engine runs the query:
// it reads the distinct prompts of the call's query block (its WITH
// clause and FROM clause, with TO_JSON_STRING of the prompt), sends each
// to the instance's local Vertex AI endpoint (never Google), and puts the
// answers in the query as a literal table the call is looked up in. The
// endpoint is resolved by the local endpoint's own rule (a gemini-* name
// answers only through an explicitly configured alias; otherwise its
// NOT_FOUND is the row's status, never a substitution).
//
// Permissions: with connection_id, the connection's (connectionauth.go);
// without, the caller's own Vertex AI User role on the project ("the
// query runs with the caller's Google Account user credentials, which
// requires the Agent Platform User role").
//
// 501, naming why: output_schema and model_params (the local runtime has
// no constrained decoding and takes no generation parameters,
// docs/generation.md), ObjectRef prompt fields (object tables), and a
// call outside a SELECT block's select list, WHERE or later clauses
// whose prompts the front cannot read.
//
// UNVERIFIED (re-read 2026-11-17: the page above, which describes no NULL
// prompt and quotes no error text): a NULL prompt's row is a NULL struct;
// the wording of the argument errors.

// aiGenerateDefaultEndpoint is the endpoint AI.GENERATE takes when none is
// given (the page above).
const aiGenerateDefaultEndpoint = "gemini-3.5-flash-lite"

// aiCall is one AI.GENERATE call: its tokens' span and its arguments.
type aiCall struct {
	start, end int // byte offsets of AI ... )
	prompt     string
	named      map[string]string
	block      int // index of the block's SELECT token
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

// findAIGenerate finds the AI.GENERATE calls of sql.
func findAIGenerate(sql string, toks []token, depth []int) ([]aiCall, int, string) {
	var calls []aiCall
	for i := 0; i+3 < len(toks); i++ {
		if !(toks[i].is("AI") && toks[i+1].punct(".") && toks[i+2].is("GENERATE") && toks[i+3].punct("(")) || i > 0 && toks[i-1].punct(".") {
			continue
		}
		end := matchParen(toks, i+3)
		if end < 0 {
			return nil, 400, "Syntax error: unclosed AI.GENERATE("
		}
		c := aiCall{start: toks[i].pos, end: toks[end].end, named: map[string]string{}, block: -1}
		var args [][]token
		k0 := i + 4
		for k := k0; k <= end; k++ {
			if k == end || toks[k].punct(",") && depth[k] == depth[i+3]+1 {
				args = append(args, toks[k0:k])
				k0 = k + 1
			}
		}
		for n, a := range args {
			if len(a) == 0 {
				return nil, 400, "Syntax error: an empty argument of AI.GENERATE"
			}
			text := strings.TrimSpace(sql[a[0].pos:a[len(a)-1].end])
			if len(a) > 2 && a[0].kind == tokWord && a[1].punct("=") && a[2].punct(">") {
				name := strings.ToLower(a[0].text)
				if len(a) == 3 {
					return nil, 400, "Syntax error: no value for the " + name + " argument of AI.GENERATE"
				}
				c.named[name] = strings.TrimSpace(sql[a[3].pos:a[len(a)-1].end])
				continue
			}
			if n != 0 {
				return nil, 400, "AI.GENERATE takes the prompt as its only positional argument; name the others (endpoint => ...)"
			}
			c.prompt = text
		}
		if p, ok := c.named["prompt"]; ok {
			if c.prompt != "" {
				return nil, 400, "AI.GENERATE was given its prompt twice"
			}
			c.prompt = p
			delete(c.named, "prompt")
		}
		if c.prompt == "" {
			return nil, 400, "AI.GENERATE requires a prompt as its first argument"
		}
		// The enclosing query block: the nearest SELECT before the call
		// at the lowest depth seen walking back.
		m := depth[i]
		for k := i - 1; k >= 0; k-- {
			m = min(m, depth[k])
			if toks[k].is("SELECT") && depth[k] == m {
				c.block = k
				break
			}
		}
		if c.block < 0 {
			return nil, 501, "Not implemented here: AI.GENERATE outside a SELECT query block. Nothing was run."
		}
		calls = append(calls, c)
		i = end
	}
	return calls, 0, ""
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

// rewriteAIGenerate evaluates text's AI.GENERATE calls and writes their
// answers into it; text is returned as it was when it has none.
func (f front) rewriteAIGenerate(r *http.Request, q queryOptions, text string) (string, int, string) {
	if !strings.Contains(strings.ToUpper(text), "GENERATE") {
		return text, 0, ""
	}
	toks, ok := lex(text)
	if !ok {
		return text, 0, ""
	}
	depth := tokenDepths(toks)
	calls, code, msg := findAIGenerate(text, toks, depth)
	if code != 0 || len(calls) == 0 {
		return text, code, msg
	}
	if f.vertexai == "" {
		return "", 501, "Not implemented here: AI.GENERATE needs the instance's local Vertex AI endpoint, which this instance does not serve to BigQuery: " +
			"start it with --local-ai-model (docs/generation.md). CloudBurrow never sends a call to Google. Nothing was run."
	}
	project := projectOf(f.base)
	var out strings.Builder
	last := 0
	for _, c := range calls {
		endpoint := aiGenerateDefaultEndpoint
		loc := "us"
		var connection *bqConnection
		for name, v := range c.named {
			switch name {
			case "endpoint":
				s, ok := unquoteString(v)
				if !ok || strings.TrimSpace(s) == "" {
					return "", 400, "The endpoint argument of AI.GENERATE must be a STRING literal"
				}
				endpoint = s
			case "connection_id":
				s, ok := unquoteString(v)
				if !ok {
					return "", 400, "The connection_id argument of AI.GENERATE must be a STRING literal"
				}
				parts := strings.Split(s, ".")
				if len(parts) == 2 {
					parts = append([]string{project}, parts...)
				}
				if len(parts) != 3 {
					return "", 400, "Invalid connection_id " + s + ": expected `[PROJECT_ID].LOCATION.CONNECTION_ID`"
				}
				cn, ok := f.connections.lookup(connectionProject(parts[0], f.numbers), parts[1], parts[2])
				if !ok {
					return "", 404, "Not found: Connection " + s
				}
				if code, msg := f.checkConnectionUse(r.Context(), parts[0], project, cn); code != 0 {
					return "", code, msg
				}
				connection, loc = cn, cn.Location
			case "request_type":
				s, _ := unquoteString(v)
				if s := strings.ToUpper(s); s != "SHARED" && s != "DEDICATED" && s != "UNSPECIFIED" {
					return "", 400, "request_type must be 'SHARED', 'DEDICATED' or 'UNSPECIFIED'"
				}
			case "output_schema":
				return "", 501, "Not implemented here: the output_schema argument of AI.GENERATE: the local LiteRT-LM runtime has no constrained decoding " +
					"(docs/generation.md), and CloudBurrow refuses an argument rather than ignoring it. Nothing was run."
			case "model_params":
				return "", 501, "Not implemented here: the model_params argument of AI.GENERATE: the local LiteRT-LM runtime takes no generation parameters " +
					"(docs/generation.md), and CloudBurrow refuses an argument rather than ignoring it. Nothing was run."
			default:
				return "", 400, "Unknown argument " + name + " of AI.GENERATE"
			}
		}
		if connection == nil {
			// The caller's own credentials: its Vertex AI User role.
			proj, code, msg := f.projectBindings(r.Context(), project)
			if code != 0 {
				return "", code, msg
			}
			if !bindingsGrant(proj, "serviceAccount:"+callerIdentity(project), permVertexPredict) {
				return "", 403, fmt.Sprintf("Access Denied: Project %s: The user %s does not have permission %s: "+
					"grant the Vertex AI user role (roles/aiplatform.user), or pass a connection_id.", project, callerIdentity(project), permVertexPredict)
			}
		}
		model := endpointModel(endpoint)
		if model == "" {
			return "", 400, "Invalid endpoint " + endpoint
		}
		rm := &remoteModel{Model: model, Endpoint: endpoint, Location: cmp.Or(loc, "us")}
		probe := fmt.Sprintf(blockSource(text, toks, depth, c.block), c.prompt)
		_, rows, code, msg := f.queryAllRows(r, q, probe)
		if code != 0 {
			return "", code, msg
		}
		var lits []string
		for _, row := range rows {
			if len(row) != 1 || row[0] == nil {
				continue
			}
			prompt, ok, code, msg := promptText(*row[0])
			if code != 0 {
				return "", code, msg
			}
			if !ok {
				continue
			}
			lits = append(lits, "STRUCT("+sqlString(*row[0])+", "+strings.Join(f.aiGenerateRow(r, project, rm, prompt), ", ")+")")
		}
		out.WriteString(text[last:c.start])
		out.WriteString("(SELECT AS STRUCT _cbai.result, _cbai.full_response, _cbai.status FROM UNNEST(ARRAY<STRUCT<k STRING, result STRING, " +
			"full_response JSON, status STRING>>[" + strings.Join(lits, ", ") + "]) AS _cbai WHERE _cbai.k = TO_JSON_STRING(" + c.prompt + "))")
		last = c.end
	}
	out.WriteString(text[last:])
	return out.String(), 0, ""
}

// aiGenerateRow is one prompt's result, full_response and status, as SQL
// literals: result NULL when the call failed, its reason the status.
func (f front) aiGenerateRow(r *http.Request, project string, rm *remoteModel, prompt string) []string {
	body := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": prompt}}}}}
	raw, status := f.vertexCall(r.Context(), project, rm, "generateContent", body)
	if status != "" {
		return []string{"CAST(NULL AS STRING)", jsonLiteral(raw), sqlString(status)}
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
		return []string{"CAST(NULL AS STRING)", jsonLiteral(raw), sqlString("The response had no candidates")}
	}
	var b strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return []string{sqlString(b.String()), jsonLiteral(raw), sqlString("")}
}
