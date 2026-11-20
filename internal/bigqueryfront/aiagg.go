package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// AI.AGG, BigQuery's aggregate generative AI function (#1777), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-agg
// documents it (read 2026-11-18):
//
//	AI.AGG([DISTINCT] INPUT, INSTRUCTION [, connection_id => …]
//	  [, endpoint => …] [, max_error_ratio => …] [, output_schema => …])
//	  -> STRING, or with output_schema a STRUCT of the schema's fields;
//	  one value per GROUP BY group (one for the whole input without one).
//
// INPUT is a STRING or a STRUCT of STRING and ARRAY<STRING> fields
// (concatenated, as AI.GENERATE's prompt), ObjectRef (or ObjectRefRuntime)
// values and STRUCT fields of them, sent as parts inside the row's tags
// (aggCall, #1983; "Rows can contain at most 10 images"); INSTRUCTION a STRING
// literal or a query parameter, named (@name) or positional (?), "as
// either a string or query parameter" (aggInstruction). The front
// evaluates a call before the engine runs the query, block by block, the
// deepest first:
//
//  1. It reads every group's rows with a probe of the call's block: the
//     block's select list (so GROUP BY ordinals and aliases keep their
//     meaning), FROM, WHERE, GROUP BY and WINDOW clauses, with one more
//     column, TO_JSON_STRING(ARRAY_AGG([DISTINCT] TO_JSON_STRING(INPUT)
//     ORDER BY …)), the group's inputs in a fixed order, and its
//     FARM_FINGERPRINT. HAVING, QUALIFY, ORDER BY and LIMIT are left out,
//     so every group is answered whatever they keep.
//  2. It answers each group on the instance's local Vertex AI endpoint
//     (never Google) by the documented multi-level aggregation: "it batches
//     the input, aggregates each batch, and aggregates the batch results",
//     skipped when the group fits one call. A batch is as many rows as fit
//     the local model's context (aiAggBatchBytes); the batches' answers are
//     combined the same way until one call holds them all, and that last
//     call is the one output_schema constrains.
//  3. It writes the answers into the query as CASE FARM_FINGERPRINT(that
//     same aggregate) WHEN … THEN answer END, which the engine evaluates
//     in the call's own group.
//
// Failures, as documented: a failed call drops its rows ("returns partial
// results") and counts them as failed; the query fails with BigQuery's
// "Query failed because AI functions exceeded their allowed error ratio"
// when the failed rows' ratio exceeds max_error_ratio (default 1.0) or
// when every row failed. Output is capped at 10,000 tokens per group;
// the local model's whole context is 4,096 tokens (measured below), so
// the cap the front sends is aiAggOutputTokens.
//
// 501, naming why: an OVER clause; AI.AGG outside a SELECT block; an
// instruction that is neither a STRING
// literal nor a query parameter.
//
// UNVERIFIED (re-read 2026-11-18: the page above documents none of these): the
// words of the prompts (aggPrompt; Google publishes no prompt); the
// batch size, which is the local model's and not Gemini's; that NULL
// inputs are ignored, as other aggregates ignore them, and that a group
// of only NULLs, or an empty input, is NULL with no call (the page's
// examples filter NULLs out and say nothing); what DISTINCT does beyond
// dropping duplicate inputs (the syntax shows it, the page does not
// describe it); that a NULL instruction is a 400 (the page lists "an
// internal error instead of an invalid-query error" as a known issue);
// the wording of the argument errors; that the endpoint BigQuery
// "chooses" when none is given is AI.GENERATE's default, which needs an
// alias locally; and that the error ratio counts a failed group's rows.

// The local model's budget (gemma-4-E2B-it on LiteRT-LM, measured
// 2026-11-18 with cloudburrow_generate: "Input token ids are too long.
// Exceeding the maximum number of tokens allowed: 5312 >= 4096" for a
// 13,134-byte prompt, 2.47 bytes a token).
const (
	aiAggContextTokens = 4096
	aiAggBytesPerToken = 2 // below the measured 2.47, so a batch fits
	aiAggOutputTokens  = 1024
	aiAggPartialTokens = 512 // a batch's answer, short enough to combine
	aiAggPromptTokens  = 256 // the chat template and aggPrompt's words
)

// aiAggBatchBytes is how many bytes of rows (and instruction) one call
// takes; a variable so tests can batch small inputs.
var aiAggBatchBytes = (aiAggContextTokens - aiAggOutputTokens - aiAggPromptTokens) * aiAggBytesPerToken

// aiAggCall is one AI.AGG call.
type aiAggCall struct {
	start, end int               // byte offsets of AI ... )
	distinct   bool              // AI.AGG(DISTINCT …)
	args       map[string]string // input, instruction and the named arguments' SQL text
	block      int               // index of the block's SELECT token
}

var aiAggNamed = []string{"connection_id", "endpoint", "max_error_ratio", "output_schema"}

// findAIAggCalls finds the AI.AGG calls of sql (not one inside another).
func findAIAggCalls(sql string, toks []token, depth []int) ([]aiAggCall, int, string) {
	var calls []aiAggCall
	for i := 0; i+3 < len(toks); i++ {
		if !toks[i].is("AI") || !toks[i+1].punct(".") || !toks[i+2].is("AGG") || !toks[i+3].punct("(") || i > 0 && toks[i-1].punct(".") {
			continue
		}
		end := matchParen(toks, i+3)
		if end < 0 {
			return nil, 400, "Syntax error: unclosed AI.AGG("
		}
		c := aiAggCall{start: toks[i].pos, end: toks[end].end, args: map[string]string{}}
		npos := 0
		positional := []string{"input", "instruction"}
		for _, a := range splitCallArgs(toks, depth, i+3, end) {
			if len(a) == 0 {
				return nil, 400, "Syntax error: an empty argument of AI.AGG"
			}
			if len(a) > 2 && a[0].kind == tokWord && a[1].punct("=") && a[2].punct(">") {
				arg := strings.ToLower(a[0].text)
				if len(a) == 3 {
					return nil, 400, "Syntax error: no value for the " + arg + " argument of AI.AGG"
				}
				if !slices.Contains(positional, arg) && !slices.Contains(aiAggNamed, arg) {
					return nil, 400, "Unknown argument " + arg + " of AI.AGG"
				}
				if _, dup := c.args[arg]; dup {
					return nil, 400, "AI.AGG was given its " + arg + " argument twice"
				}
				c.args[arg] = strings.TrimSpace(sql[a[3].pos:a[len(a)-1].end])
				continue
			}
			if npos >= len(positional) || len(c.args) > npos {
				return nil, 400, "AI.AGG takes input and instruction as its only positional arguments, first; name the others (endpoint => ...)"
			}
			if npos == 0 && a[0].is("DISTINCT") {
				if len(a) == 1 {
					return nil, 400, "Syntax error: DISTINCT with no input in AI.AGG"
				}
				c.distinct, a = true, a[1:]
			}
			c.args[positional[npos]] = strings.TrimSpace(sql[a[0].pos:a[len(a)-1].end])
			npos++
		}
		for _, k := range positional {
			if c.args[k] == "" {
				return nil, 400, "AI.AGG requires its " + k + " argument"
			}
		}
		if end+1 < len(toks) && toks[end+1].is("OVER") {
			return nil, 501, "Not implemented here: AI.AGG with an OVER clause (as an analytic function). Nothing was run."
		}
		c.block = enclosingSelect(toks, depth, i)
		if c.block < 0 {
			return nil, 501, "Not implemented here: AI.AGG outside a SELECT query block. Nothing was run."
		}
		calls = append(calls, c)
		i = end
	}
	return calls, 0, ""
}

// contentExpr is the aggregate the call's group is told apart by: its
// inputs as JSON, in a fixed order.
func (c aiAggCall) contentExpr() string {
	in := "TO_JSON_STRING(" + c.args["input"] + ")"
	d := ""
	if c.distinct {
		d = "DISTINCT "
	}
	return "TO_JSON_STRING(ARRAY_AGG(" + d + in + " ORDER BY " + in + "))"
}

// aggType is the call's value's type.
func aggType(a aiResolved) string {
	if a.schema != nil {
		return aiType{kind: "STRUCT", fields: a.schema}.sql()
	}
	return "STRING"
}

// blockLayout is where a query block's parts are, as token indices.
type blockLayout struct {
	listStart, listEnd int            // the select list
	clause             map[string]int // the first token of each clause at the block's depth
	end                int            // the first token after the block
}

var blockClauses = []string{"FROM", "WHERE", "GROUP", "HAVING", "QUALIFY", "WINDOW", "ORDER", "LIMIT"}

func layoutBlock(toks []token, depth []int, sel int) blockLayout {
	d := depth[sel]
	l := blockLayout{clause: map[string]int{}, end: len(toks)}
	for k := sel + 1; k < len(toks); k++ {
		if depth[k] < d {
			l.end = k
			break
		}
		if depth[k] != d {
			continue
		}
		t := toks[k]
		if t.punct(";") || t.is("UNION") || t.is("INTERSECT") ||
			t.is("EXCEPT") && k+1 < len(toks) && (toks[k+1].is("DISTINCT") || toks[k+1].is("ALL")) {
			l.end = k
			break
		}
		for _, w := range blockClauses {
			if _, seen := l.clause[w]; !seen && t.is(w) {
				l.clause[w] = k
			}
		}
	}
	l.listStart = sel + 1
	for l.listStart < l.end && (toks[l.listStart].is("ALL") || toks[l.listStart].is("DISTINCT")) {
		l.listStart++
	}
	if l.listStart+1 < l.end && toks[l.listStart].is("AS") && (toks[l.listStart+1].is("STRUCT") || toks[l.listStart+1].is("VALUE")) {
		l.listStart += 2
	}
	l.listEnd = l.end
	for _, w := range blockClauses {
		if k, ok := l.clause[w]; ok && k < l.listEnd {
			l.listEnd = k
		}
	}
	return l
}

// pos is the byte offset of token k (len(sql) past the last).
func tokPos(sql string, toks []token, k int) int {
	if k < len(toks) {
		return toks[k].pos
	}
	return len(sql)
}

// clauseEnd is where the clause starting at token from ends: at the next
// of the block's clauses in after, or the block's end.
func (l blockLayout) clauseEnd(from int, after ...string) int {
	end := l.end
	for _, w := range after {
		if k, ok := l.clause[w]; ok && k > from && k < end {
			end = k
		}
	}
	return end
}

// rewriteAIAgg evaluates text's AI.AGG calls and writes their answers in.
// It returns q with the parameters the rewritten text holds (#1810): the
// engine refuses a parameter the query does not reference ("expected 0
// arguments, got 1"), and an instruction's parameter is gone once its call
// is answered. Positional parameters are named first (namePositional), as
// the probes and the answers move the calls' ? around.
func (f front) rewriteAIAgg(r *http.Request, q queryOptions, text string) (string, queryOptions, int, string) {
	if !strings.Contains(strings.ToUpper(text), "AGG") {
		return text, q, 0, ""
	}
	project := projectOf(f.base)
	rewritten := false
	for {
		toks, ok := lex(text)
		if !ok {
			return text, q, 0, ""
		}
		depth := tokenDepths(toks)
		calls, code, msg := findAIAggCalls(text, toks, depth)
		if code != 0 {
			return "", q, code, msg
		}
		if len(calls) == 0 {
			if rewritten {
				q = usedParams(q, text)
			}
			return text, q, 0, ""
		}
		if !rewritten {
			rewritten = true
			if named, nq, ok := namePositional(q, text, toks); ok {
				text, q = named, nq
				continue
			}
		}
		if f.vertexai == "" {
			return "", q, 501, "Not implemented here: AI.AGG needs the instance's local Vertex AI endpoint, which this instance does not serve to BigQuery: " +
				"start it with --local-ai-model (docs/generation.md). CloudBurrow never sends a call to Google. Nothing was run."
		}
		resolved := make([]aiResolved, len(calls))
		types := make([]string, len(calls))
		deepest := 0
		for i, c := range calls {
			a, code, msg := f.resolveAICall(r, q, project, aiCall{fn: "AGG", start: c.start, end: c.end, args: c.args, block: c.block})
			if code != 0 {
				return "", q, code, msg
			}
			resolved[i], types[i] = a, aggType(a)
			if depth[c.block] > depth[calls[deepest].block] {
				deepest = i
			}
		}
		sel := calls[deepest].block
		var out strings.Builder
		last := 0
		for i, c := range calls {
			if c.block != sel {
				continue
			}
			probe, code, msg := aggProbe(text, toks, depth, calls, types, i)
			if code != 0 {
				return "", q, code, msg
			}
			a := resolved[i]
			instruction, code, msg := f.aggInstruction(r, q, c.args["instruction"])
			if code != 0 {
				return "", q, code, msg
			}
			_, rows, code, msg := f.queryAllRows(r, paramsOf(q, probe), probe)
			if code != 0 {
				return "", q, code, msg
			}
			var whens strings.Builder
			failed, total := 0, 0
			firstStatus := ""
			for _, row := range rows {
				if len(row) != 2 || row[0] == nil || row[1] == nil {
					continue
				}
				var keys []string
				if *row[0] != "null" && json.Unmarshal([]byte(*row[0]), &keys) != nil {
					return "", q, 500, "cloudburrow: unreadable AI.AGG group " + *row[0]
				}
				var inputs []aggItem
				tooMany := 0
				for _, k := range keys {
					ps, ok, code, msg := promptPieces(k)
					if code != 0 {
						return "", q, code, strings.ReplaceAll(msg, "AI.GENERATE", "AI.AGG")
					}
					if !ok {
						continue
					}
					if len(refPieces(ps)) > aiAggMaxObjects {
						// "Rows can contain at most 10 images": the row fails.
						tooMany++
						continue
					}
					it := aggItem{text: joinText(ps), rows: 1}
					if hasRef(ps) {
						it.pieces = ps
					}
					inputs = append(inputs, it)
				}
				total += len(inputs) + tooMany
				failed += tooMany
				if tooMany > 0 && firstStatus == "" {
					firstStatus = "A row of AI.AGG's input holds more than " + strconv.Itoa(aiAggMaxObjects) + " ObjectRefs: rows can contain at most 10 images, or 1 audio, video, or PDF file"
				}
				if len(inputs) == 0 {
					continue
				}
				lit, ok, nfail, status := f.aggGroup(r, project, a, instruction, inputs)
				failed += nfail
				if firstStatus == "" {
					firstStatus = status
				}
				if ok {
					whens.WriteString(" WHEN " + *row[1] + " THEN " + lit)
				}
			}
			if total > 0 && (failed == total || float64(failed)/float64(total) > a.maxErrRatio) {
				return "", q, 400, "Query failed because AI functions exceeded their allowed error ratio: AI.AGG failed for " +
					strconv.Itoa(failed) + " of " + strconv.Itoa(total) + " rows; the first: " + firstStatus
			}
			if whens.Len() == 0 {
				whens.WriteString(" WHEN 0 THEN CAST(NULL AS " + types[i] + ")")
			}
			out.WriteString(text[last:c.start])
			value := "(CASE FARM_FINGERPRINT(" + c.contentExpr() + ")" + whens.String() + " ELSE CAST(NULL AS " + types[i] + ") END)"
			out.WriteString(value)
			last = c.end
		}
		out.WriteString(text[last:])
		text = out.String()
	}
}

// aggProbe is the query that reads the groups of calls[i]: its block's
// select list, FROM, WHERE, GROUP BY and WINDOW clauses with the group's
// inputs as one more column (contentExpr) and their fingerprint, every
// AI.AGG call in it a typed NULL.
func aggProbe(text string, toks []token, depth []int, calls []aiAggCall, types []string, i int) (string, int, string) {
	c := calls[i]
	l := layoutBlock(toks, depth, c.block)
	for _, w := range []string{"FROM", "WHERE", "GROUP"} {
		k, ok := l.clause[w]
		if !ok {
			continue
		}
		if end := l.clauseEnd(k, blockClauses...); c.start >= toks[k].pos && c.start < tokPos(text, toks, end) {
			return "", 400, "Aggregate function AI.AGG not allowed in " + map[string]string{"FROM": "FROM clause", "WHERE": "WHERE clause", "GROUP": "GROUP BY clause"}[w]
		}
	}
	excerpt := func(lo, hi int) string {
		var b strings.Builder
		at := lo
		for j, o := range calls {
			if o.start >= lo && o.end <= hi {
				b.WriteString(text[at:o.start] + "CAST(NULL AS " + types[j] + ")")
				at = o.end
			}
		}
		b.WriteString(text[at:hi])
		return b.String()
	}
	with := ""
	if toks[0].is("WITH") {
		for k := 1; k < len(toks); k++ {
			if depth[k] == 0 && toks[k].is("SELECT") {
				with = excerpt(0, toks[k].pos)
				break
			}
		}
	}
	list := strings.TrimSpace(excerpt(tokPos(text, toks, l.listStart), tokPos(text, toks, l.listEnd)))
	list = strings.TrimSpace(strings.TrimSuffix(list, ","))
	src := ""
	if from, ok := l.clause["FROM"]; ok {
		src = " " + strings.TrimSpace(excerpt(toks[from].pos, tokPos(text, toks, l.clauseEnd(from, "HAVING", "QUALIFY", "WINDOW", "ORDER", "LIMIT"))))
		if w, ok := l.clause["WINDOW"]; ok {
			src += " " + strings.TrimSpace(excerpt(toks[w].pos, tokPos(text, toks, l.clauseEnd(w, "ORDER", "LIMIT"))))
		}
	}
	return with + "SELECT DISTINCT _cbai_c, FARM_FINGERPRINT(_cbai_c) AS _cbai_fp FROM (SELECT " + list + ", " + c.contentExpr() + " AS _cbai_c" + src + ")", 0, ""
}

// aggInstruction is the instruction's value: a STRING literal, or a query
// parameter (@name; a positional ? is named by then, namePositional),
// read from the request's own parameters, whose declared type it must
// have (#1810): the engine types a parameter it reads alone by its value,
// a NULL as INT64, so it is not asked. Any other expression is 501.
func (f front) aggInstruction(r *http.Request, q queryOptions, expr string) (string, int, string) {
	if s, ok := unquoteString(expr); ok {
		return s, 0, ""
	}
	if strings.EqualFold(strings.TrimSpace(expr), "NULL") || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(expr)), "CAST(NULL") {
		return "", 400, "The instruction argument of AI.AGG must not be NULL"
	}
	name, ok := queryParameterName(expr)
	if !ok {
		return "", 501, "Not implemented here: an AI.AGG instruction that is neither a STRING literal nor a query parameter (" + expr + "). Nothing was run."
	}
	var ps []struct {
		Name string `json:"name"`
		Type struct {
			Type string `json:"type"`
		} `json:"parameterType"`
		Value *struct {
			Value *string `json:"value"`
		} `json:"parameterValue"`
	}
	if len(q.QueryParameters) > 0 && json.Unmarshal(q.QueryParameters, &ps) != nil {
		return "", 400, "Invalid query parameters"
	}
	for _, p := range ps {
		if !strings.EqualFold(p.Name, name) {
			continue
		}
		if !strings.EqualFold(p.Type.Type, "STRING") {
			return "", 400, "The instruction argument of AI.AGG must be a STRING, not " + p.Type.Type
		}
		if p.Value == nil || p.Value.Value == nil {
			return "", 400, "The instruction argument of AI.AGG must not be NULL"
		}
		return *p.Value.Value, 0, ""
	}
	return "", 400, "Query parameter '" + name + "' not found"
}

// queryParameterName is the name of the query parameter expr is, @name or
// @`name` (not @@name, a system variable); ok is false for any other
// expression.
func queryParameterName(expr string) (string, bool) {
	toks, ok := lex(expr)
	if !ok || len(toks) != 2 || !toks[0].punct("@") || toks[1].kind != tokWord && toks[1].kind != tokQuoted || toks[1].pos != toks[0].end {
		return "", false
	}
	return toks[1].text, true
}

// namePositional names q's positional parameters, in order, cbai_p1,
// cbai_p2, … (the engine refuses a name that does not begin with a
// letter), and writes text's ? as @cbai_p1, …; ok is false (nothing
// to do) when q has no positional parameters. The client's job still shows
// its own text and parameters (mlFunctions).
func namePositional(q queryOptions, text string, toks []token) (string, queryOptions, bool) {
	if len(q.QueryParameters) == 0 || q.ParameterMode != "" && !strings.EqualFold(q.ParameterMode, "POSITIONAL") {
		return text, q, false
	}
	var ps []map[string]json.RawMessage
	if json.Unmarshal(q.QueryParameters, &ps) != nil {
		return text, q, false
	}
	for _, p := range ps {
		// No parameterMode, as the Go client sends: positional when no
		// parameter is named (bytesParameters).
		var name string
		if p == nil || q.ParameterMode == "" && json.Unmarshal(p["name"], &name) == nil && name != "" {
			return text, q, false
		}
	}
	for i, p := range ps {
		p["name"], _ = json.Marshal("cbai_p" + strconv.Itoa(i+1))
	}
	var b strings.Builder
	last, n := 0, 0
	for _, t := range toks {
		if t.punct("?") {
			n++
			b.WriteString(text[last:t.pos] + "@cbai_p" + strconv.Itoa(n))
			last = t.end
		}
	}
	b.WriteString(text[last:])
	raw, err := json.Marshal(ps)
	if err != nil {
		return text, q, false
	}
	q.QueryParameters, q.ParameterMode = raw, "NAMED"
	return b.String(), q, true
}

// usedParams is q with only the named parameters text references, as
// the query the engine runs: an empty list rather than none, so the
// request's own parameters are replaced (setQueryParameters).
func usedParams(q queryOptions, text string) queryOptions {
	if len(q.QueryParameters) == 0 {
		return q
	}
	mode := q.ParameterMode
	out := paramsOf(q, text)
	if out.QueryParameters == nil {
		out.QueryParameters, out.ParameterMode = json.RawMessage("[]"), mode
	}
	return out
}

// paramsOf is q with only the named query parameters sql uses: the engine
// refuses a query given a parameter it does not reference ("expected 0
// arguments, got 1"), and a probe holds only part of the query.
func paramsOf(q queryOptions, sql string) queryOptions {
	if len(q.QueryParameters) == 0 || strings.EqualFold(q.ParameterMode, "POSITIONAL") {
		return q
	}
	var ps []json.RawMessage
	if json.Unmarshal(q.QueryParameters, &ps) != nil {
		return q
	}
	toks, _ := lex(sql)
	var keep []json.RawMessage
	for _, p := range ps {
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(p, &named) != nil || named.Name == "" {
			return q
		}
		for k := 1; k < len(toks); k++ {
			// @name or @`name`, not @@name (a system variable)
			if (toks[k].kind == tokWord || toks[k].kind == tokQuoted) && strings.EqualFold(toks[k].text, named.Name) &&
				toks[k-1].punct("@") && (k < 2 || !toks[k-2].punct("@")) {
				keep = append(keep, p)
				break
			}
		}
	}
	out := q
	if len(keep) == 0 {
		out.QueryParameters, out.ParameterMode = nil, ""
		return out
	}
	out.QueryParameters, _ = json.Marshal(keep)
	return out
}

// aggItem is one input of an aggregation call: a row, or a batch's answer
// standing for the rows it read.
type aggItem struct {
	text   string
	rows   int
	pieces []promptPiece // a row with ObjectRefs (#1983): its pieces in order
}

// aiAggMaxObjects is the most ObjectRefs a row of AI.AGG's input holds:
// "Rows can contain at most 10 images, or 1 audio, video, or PDF file"
// (https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-agg).
const aiAggMaxObjects = 10

// aiAggRefBytes is what one ObjectRef counts for in a batch's budget: an
// image is about 256 of the local model's tokens.
const aiAggRefBytes = 1024

// size is it's share of a batch's budget.
func (it aggItem) size() int {
	return len(it.text) + len("<row></row>\n") + aiAggRefBytes*len(refPieces(it.pieces))
}

// packAgg groups items into batches of at most budget bytes, in order; an
// item larger than the budget is a batch of its own.
func packAgg(items []aggItem, budget int) [][]aggItem {
	var out [][]aggItem
	var cur []aggItem
	size := 0
	for _, it := range items {
		n := it.size()
		if len(cur) > 0 && size+n > budget {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, it)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// aggPrompt is one aggregation call's prompt (UNVERIFIED, re-read 2026-11-18: the
// page publishes no prompt): the instruction and the rows, or, combining,
// the batches' answers.
func aggPrompt(instruction string, items []aggItem, combine bool) string {
	var b strings.Builder
	b.WriteString(instruction)
	tag := "row"
	if combine {
		tag = "part"
		b.WriteString("\n\nThe data was too large to read at once, so it was split into parts and the instruction was applied to each part. " +
			"Combine the results of the parts below, each between <part> and </part>, into one answer to the instruction for all of the data.\n")
	} else {
		b.WriteString("\n\nApply the instruction to all of the following rows taken together. Each row is between <row> and </row>.\n")
	}
	for _, it := range items {
		b.WriteString("<" + tag + ">" + it.text + "</" + tag + ">\n")
	}
	return b.String()
}

// aggGroup answers one group: its value's SQL literal and whether it has
// one, the rows that failed and the first failure's status.
func (f front) aggGroup(r *http.Request, project string, a aiResolved, instruction string, items []aggItem) (string, bool, int, string) {
	budget := max(aiAggBatchBytes-len(instruction), 1)
	failed := 0
	first := ""
	fail := func(n int, status string) {
		failed += n
		if first == "" {
			first = status
		}
	}
	combine := false
	for {
		batches := packAgg(items, budget)
		if combine && len(batches) > 1 && len(batches) == len(items) {
			// Every answer fills a batch alone: combine them in pairs, so
			// each level halves them.
			batches = nil
			for i := 0; i < len(items); i += 2 {
				batches = append(batches, items[i:min(i+2, len(items))])
			}
		}
		if len(batches) == 1 {
			n := 0
			for _, it := range items {
				n += it.rows
			}
			suffix := ""
			if a.schema != nil {
				suffix = aiSchemaInstruction(a.schema)
			}
			text, status := f.aggCall(r, project, a, instruction, items, combine, suffix, aiResponseSchema("GENERATE", a.schema), aiAggOutputTokens)
			if status != "" {
				fail(n, status)
				return "", false, failed, first
			}
			if a.schema == nil {
				return sqlString(text), true, failed, first
			}
			lits, why := schemaLiterals(a.schema, text)
			if why != "" {
				fail(n, why)
				return "", false, failed, first
			}
			return aggType(a) + "(" + strings.Join(lits, ", ") + ")", true, failed, first
		}
		var next []aggItem
		for _, b := range batches {
			n := 0
			for _, it := range b {
				n += it.rows
			}
			text, status := f.aggCall(r, project, a, instruction, b, combine, "", nil, aiAggPartialTokens)
			if status != "" {
				fail(n, status)
				continue
			}
			next = append(next, aggItem{text: text, rows: n})
		}
		if len(next) == 0 {
			return "", false, failed, first
		}
		items, combine = next, true
	}
}

// aggCall is one aggregation call over items: aggPrompt's text, or, when
// a row holds ObjectRefs (#1983), the same prompt as parts with each
// row's objects inside its <row> tags, which the local model reads when
// it reads that modality.
func (f front) aggCall(r *http.Request, project string, a aiResolved, instruction string, items []aggItem, combine bool, suffix string, schema map[string]any, maxTokens int) (string, string) {
	cfg := map[string]any{"maxOutputTokens": maxTokens}
	multimodal := false
	for _, it := range items {
		multimodal = multimodal || it.pieces != nil
	}
	if !multimodal {
		text, _, status := f.aiGenerateText(r, project, a.rm, aggPrompt(instruction, items, combine)+suffix, schema, cfg)
		return text, status
	}
	head := aggPrompt(instruction, nil, combine)
	ps := []promptPiece{{text: head}}
	for _, it := range items {
		if it.pieces == nil {
			ps = append(ps, promptPiece{text: "<row>" + it.text + "</row>\n"})
			continue
		}
		ps = append(ps, promptPiece{text: "<row>"})
		ps = append(ps, it.pieces...)
		ps = append(ps, promptPiece{text: "</row>\n"})
	}
	parts, status := f.pieceParts(r, ps, suffix)
	if status != "" {
		return "", status
	}
	text, _, status := f.aiGenerateParts(r, project, a.rm, parts, schema, cfg)
	return text, status
}
