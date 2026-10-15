package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
)

// Scripting control flow (#1201).
//
// The emulator does not run the statements inside IF, LOOP, WHILE, REPEAT,
// FOR or a script CASE, and reports the script done (measured, checkDDL),
// and it never runs a BEGIN ... EXCEPTION block's handler. So the front
// carries such a script out itself
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language):
//
//   - It reads the script into its statements and blocks: IF / ELSEIF /
//     ELSE, LOOP, WHILE, REPEAT ... UNTIL, FOR ... IN, CASE (simple and
//     searched), BEGIN ... END with or without EXCEPTION WHEN ERROR THEN,
//     BREAK / LEAVE, CONTINUE / ITERATE (with a label or not), DECLARE and
//     SET.
//   - It keeps the script's variables itself, each as a literal of its
//     type, in the scope of the block that declares it. A DECLARE's
//     DEFAULT and a SET's value, a condition, a CASE's operands and a FOR's
//     query are run on the emulator as queries, with each variable
//     replaced by its literal (the words variableRefs finds); a FOR's
//     variable is a STRUCT of the row's columns.
//   - Each other statement is sent, so rewritten, in its turn, as a query
//     of its own through the front's own checks (runQuery). A statement
//     that fails ends the script with its error, unless a BEGIN ...
//     EXCEPTION block it is in handles it: the handler then runs, with
//     @@error.message, @@error.statement_text, @@error.stack_trace and
//     @@error.formatted_stack_trace. What the statements before it did is
//     kept, as in BigQuery.
//   - The script's answer is its last statement's result. A query (SELECT
//     or WITH) last run is sent as the client's own request, so a query
//     job's results are its rows; a last statement that changes data was
//     already run, and is jobs.query's answer, while a query job is then
//     recorded with a statement that does nothing (failBeforeRun's).
//
// RAISE USING MESSAGE fails the script with its message, which a handler
// may catch; a bare RAISE in a handler raises its error again. A script
// with a RAISE and no block is carried out so too.
//
// What it cannot do faithfully is 501, before anything runs: a
// CALL, a transaction, a FOR or SET of a type it cannot write as a literal
// (RANGE), EXECUTE IMMEDIATE inside a block, and a script of over
// scriptStepLimit steps (BigQuery's own limit is the job's time, 6 hours).

// scriptStepLimit bounds the statements and evaluations a script the front
// carries out may run: CloudBurrow's own bound, so a loop that never ends
// fails rather than running for ever.
const scriptStepLimit = 10000

// hasControlFlow reports whether sql is a script with a control-flow
// block or an EXCEPTION handler, which the front carries out (runScript).
func hasControlFlow(sql string) bool {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "IF") && !strings.Contains(upper, "LOOP") && !strings.Contains(upper, "WHILE") &&
		!strings.Contains(upper, "REPEAT") && !strings.Contains(upper, "FOR") && !strings.Contains(upper, "CASE") &&
		!strings.Contains(upper, "EXCEPTION") && !strings.Contains(upper, "RAISE") {
		return false
	}
	toks, ok := lex(sql)
	if !ok {
		return false
	}
	for _, stmt := range splitStatements(toks) {
		if body, block, handler := stripControlFlow(stmt); block != "" || handler || len(body) > 0 && body[0].is("RAISE") {
			return true
		}
	}
	return false
}

type scriptNodeKind int

const (
	nodeStmt scriptNodeKind = iota
	nodeDeclare
	nodeSet
	nodeIf
	nodeLoop
	nodeWhile
	nodeRepeat
	nodeFor
	nodeCase
	nodeBlock
	nodeBreak
	nodeContinue
)

// scriptNode is one statement of a script: a plain statement, its text in
// toks; or a block, with its parts.
type scriptNode struct {
	kind  scriptNodeKind
	label string
	toks  []token // the statement (nodeStmt, nodeDeclare, nodeSet); the condition or query
	// conds and bodies: IF's and CASE's branches, in order; els the ELSE.
	conds  [][]token
	bodies [][]scriptNode
	els    []scriptNode
	hasEls bool
	body   []scriptNode
	// handler is a BEGIN block's EXCEPTION WHEN ERROR THEN statements.
	handler    []scriptNode
	hasHandler bool
	forVar     string
	caseExpr   []token
	target     string // BREAK's or CONTINUE's label
}

type scriptParser struct {
	toks []token
	i    int
	err  string
}

func (p *scriptParser) peek(k int) (token, bool) {
	if p.i+k < len(p.toks) {
		return p.toks[p.i+k], true
	}
	return token{}, false
}

func (p *scriptParser) at(words ...string) bool {
	for k, w := range words {
		t, ok := p.peek(k)
		if !ok || !t.is(w) {
			return false
		}
	}
	return true
}

func (p *scriptParser) fail(msg string) {
	if p.err == "" {
		p.err = msg
	}
}

func (p *scriptParser) expect(words ...string) {
	if !p.at(words...) {
		p.fail("expected " + strings.Join(words, " "))
		return
	}
	p.i += len(words)
}

func (p *scriptParser) semicolon() {
	if t, ok := p.peek(0); ok && t.punct(";") {
		p.i++
	}
}

// until returns the tokens from here to the first kw at the top level
// (outside brackets and CASE ... END expressions), and moves past kw.
func (p *scriptParser) until(kw ...string) []token {
	depth, cases := 0, 0
	for j := p.i; j < len(p.toks); j++ {
		t := p.toks[j]
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			depth--
		case depth == 0 && t.is("CASE"):
			cases++
		case depth == 0 && cases > 0 && t.is("END"):
			cases--
		case depth == 0 && cases == 0 && t.is(kw[0]):
			match := true
			for k := 1; k < len(kw); k++ {
				if j+k >= len(p.toks) || !p.toks[j+k].is(kw[k]) {
					match = false
				}
			}
			if match {
				out := p.toks[p.i:j]
				p.i = j + len(kw)
				return out
			}
		}
	}
	p.fail("expected " + strings.Join(kw, " "))
	p.i = len(p.toks)
	return nil
}

// statement returns the tokens to the next top-level ";".
func (p *scriptParser) statement() []token {
	depth := 0
	for j := p.i; j < len(p.toks); j++ {
		t := p.toks[j]
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			depth--
		case depth == 0 && t.punct(";"):
			out := p.toks[p.i:j]
			p.i = j + 1
			return out
		}
	}
	out := p.toks[p.i:]
	p.i = len(p.toks)
	return out
}

// block reads statements until one that starts with a word of stop.
func (p *scriptParser) block(stop ...string) []scriptNode {
	var out []scriptNode
	for p.err == "" && p.i < len(p.toks) {
		t, _ := p.peek(0)
		if t.punct(";") {
			p.i++
			continue
		}
		for _, s := range stop {
			if t.is(s) {
				return out
			}
		}
		out = append(out, p.node())
	}
	return out
}

func (p *scriptParser) node() scriptNode {
	var n scriptNode
	if t, ok := p.peek(0); ok && (t.kind == tokWord || t.kind == tokQuoted) {
		if c, ok := p.peek(1); ok && c.punct(":") {
			n.label = strings.ToLower(t.text)
			p.i += 2
		}
	}
	t, _ := p.peek(0)
	switch {
	case t.is("IF"):
		p.i++
		n.kind = nodeIf
		n.conds = append(n.conds, p.until("THEN"))
		n.bodies = append(n.bodies, p.block("ELSEIF", "ELSE", "END"))
		for p.err == "" && p.at("ELSEIF") {
			p.i++
			n.conds = append(n.conds, p.until("THEN"))
			n.bodies = append(n.bodies, p.block("ELSEIF", "ELSE", "END"))
		}
		if p.at("ELSE") {
			p.i++
			n.hasEls = true
			n.els = p.block("END")
		}
		p.expect("END", "IF")
	case t.is("LOOP"):
		p.i++
		n.kind = nodeLoop
		n.body = p.block("END")
		p.expect("END", "LOOP")
	case t.is("WHILE"):
		p.i++
		n.kind = nodeWhile
		n.toks = p.until("DO")
		n.body = p.block("END")
		p.expect("END", "WHILE")
	case t.is("REPEAT"):
		p.i++
		n.kind = nodeRepeat
		n.body = p.block("UNTIL")
		p.expect("UNTIL")
		n.toks = p.until("END", "REPEAT")
	case t.is("FOR"):
		p.i++
		n.kind = nodeFor
		v, ok := p.peek(0)
		if !ok || v.kind != tokWord && v.kind != tokQuoted {
			p.fail("expected FOR's variable")
			return n
		}
		n.forVar = v.text
		p.i++
		p.expect("IN")
		n.toks = p.until("DO")
		n.body = p.block("END")
		p.expect("END", "FOR")
	case t.is("CASE"):
		p.i++
		n.kind = nodeCase
		if !p.at("WHEN") {
			n.caseExpr = p.until("WHEN")
			p.i--
		}
		for p.err == "" && p.at("WHEN") {
			p.i++
			n.conds = append(n.conds, p.until("THEN"))
			n.bodies = append(n.bodies, p.block("WHEN", "ELSE", "END"))
		}
		if p.at("ELSE") {
			p.i++
			n.hasEls = true
			n.els = p.block("END")
		}
		p.expect("END", "CASE")
	case t.is("BEGIN") && !p.at("BEGIN", "TRANSACTION") && !p.at("BEGIN", "TRAN") && !(p.i+1 < len(p.toks) && p.toks[p.i+1].punct(";")) && p.i+1 < len(p.toks):
		p.i++
		n.kind = nodeBlock
		n.body = p.block("EXCEPTION", "END")
		if p.at("EXCEPTION") {
			p.i++
			p.expect("WHEN", "ERROR", "THEN")
			n.hasHandler = true
			n.handler = p.block("END")
		}
		p.expect("END")
		// BigQuery allows the label after END.
		if l, ok := p.peek(0); ok && n.label != "" && (l.kind == tokWord || l.kind == tokQuoted) && strings.EqualFold(l.text, n.label) {
			p.i++
		}
	case t.is("BREAK") || t.is("LEAVE") || t.is("CONTINUE") || t.is("ITERATE"):
		p.i++
		n.kind = nodeBreak
		if t.is("CONTINUE") || t.is("ITERATE") {
			n.kind = nodeContinue
		}
		if l, ok := p.peek(0); ok && (l.kind == tokWord || l.kind == tokQuoted) {
			n.target = strings.ToLower(l.text)
			p.i++
		}
	case t.is("DECLARE"):
		n.kind = nodeDeclare
		n.toks = p.statement()
		return n
	case t.is("SET"):
		n.kind = nodeSet
		n.toks = p.statement()
		return n
	default:
		n.kind = nodeStmt
		n.toks = p.statement()
		return n
	}
	if n.kind == nodeLoop || n.kind == nodeWhile || n.kind == nodeRepeat || n.kind == nodeFor {
		// A label may follow END LOOP and the like.
		if l, ok := p.peek(0); ok && n.label != "" && (l.kind == tokWord || l.kind == tokQuoted) && strings.EqualFold(l.text, n.label) {
			p.i++
		}
	}
	p.semicolon()
	return n
}

// scriptVar is a variable's value, as a literal of its type.
type scriptVar struct {
	typ, lit string
}

// scriptFlow is a control signal: a BREAK or CONTINUE on its way to its
// loop, or a failed statement on its way to a handler.
type scriptFlow struct {
	brk, cont bool
	label     string
	// failure: the failed statement's status and body, its message and
	// text.
	failed bool
	status int
	body   []byte
	msg    string
	text   string
}

type scriptRun struct {
	f      front
	r      *http.Request
	q      queryOptions
	sql    string
	scopes []map[string]scriptVar
	steps  int
	// last is the response to the last statement run, internally; pending
	// a query (SELECT or WITH) whose turn it is, kept to be sent last.
	last      *recorder
	lastQuery string
	pending   string
	inHandler []map[string]scriptVar // @@error in the handler running
	guarded   int                    // depth of EXCEPTION blocks around the statement running
	notImpl   string
}

// runScript carries out a script with control flow (above).
func (f front) runScript(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) {
	toks, ok := lex(q.Query)
	if !ok {
		f.serveQuery(w, r, q, insert)
		return
	}
	p := &scriptParser{toks: toks}
	nodes := p.block()
	if p.err != "" {
		// BigQuery refuses a script it cannot parse before it runs.
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest,
			rowError{Reason: "invalidQuery", Message: "Syntax error in the script: " + p.err})
		return
	}
	if msg := unsupportedInScript(nodes); msg != "" {
		writeError(w, http.StatusNotImplemented, "notImplemented", msg)
		return
	}
	// Every statement is checked before anything runs, as BigQuery checks
	// the whole script, whether or not its branch would run (checkDDL).
	if v := checkStatements(q.Query, nodes); v.code != 0 {
		if v.code == http.StatusBadRequest {
			f.failBeforeRun(w, r, q, insert, "_cloudburrow", v.code, rowError{Reason: v.reason, Message: v.msg})
			return
		}
		writeError(w, v.code, v.reason, v.msg)
		return
	}
	s := &scriptRun{f: f, r: r, q: q, sql: q.Query, scopes: []map[string]scriptVar{{}}}
	flow := s.run(nodes)
	if s.notImpl != "" {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusNotImplemented, rowError{Reason: "notImplemented", Message: s.notImpl})
		return
	}
	if flow != nil && flow.failed {
		if !insert {
			writeRaw(w, flow.status, flow.body)
			return
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: flow.msg})
		return
	}
	client := jobText{query: q.Query}
	if insert && s.pending == "" && s.lastQuery != "" {
		s.pending = s.lastQuery
	}
	if s.pending != "" {
		// The last statement was a query: it is the client's request.
		if !setQueryText(r, insert, s.pending) {
			writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
			return
		}
		rec := newRecorder()
		q2 := q
		q2.Query = s.pending
		f.runQuery(rec, r, q2, insert)
		f.clientText(w, rec, client)
		return
	}
	if !insert && s.last != nil {
		f.clientText(w, s.last, client)
		return
	}
	// No statement, or one that changed data, ran last: the job is
	// recorded with a statement that does nothing.
	if !setQueryText(r, insert, "DROP TABLE IF EXISTS "+quotePath([]string{resultsDataset, scratchTable()})) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	f.forward(w, r, client)
}

// unsupportedInScript returns the 501 message for a statement the front
// cannot carry out faithfully inside a script it runs, or "".
func unsupportedInScript(nodes []scriptNode) string {
	var walk func([]scriptNode) string
	walk = func(ns []scriptNode) string {
		for _, n := range ns {
			if n.kind == nodeStmt && len(n.toks) > 0 {
				t := n.toks[0]
				switch {
				case t.is("CALL"):
					return "CALL inside a script with control flow. CloudBurrow carries such a script out itself, statement by statement, and does not run procedures."
				case t.is("COMMIT") || t.is("ROLLBACK") || t.is("BEGIN"):
					return "a transaction inside a script with control flow. CloudBurrow carries such a script out itself, one statement at a time, each in a transaction of its own, so it cannot roll several back together."
				case t.is("EXECUTE"):
					return "EXECUTE IMMEDIATE inside a script with control flow. CloudBurrow carries such a script out itself, and runs EXECUTE IMMEDIATE only in a script without control flow."
				}
			}
			for _, b := range append([][]scriptNode{n.body, n.els, n.handler}, n.bodies...) {
				if msg := walk(b); msg != "" {
					return msg
				}
			}
		}
		return ""
	}
	if msg := walk(nodes); msg != "" {
		return "Not implemented here: " + msg + " Nothing was run."
	}
	return ""
}

// checkStatements is checkDDL's verdict on each statement of nodes, the
// first refusal.
func checkStatements(sql string, nodes []scriptNode) ddlVerdict {
	for _, n := range nodes {
		if n.kind == nodeStmt && len(n.toks) > 0 && !n.toks[0].is("RAISE") {
			if v := checkDDL(sql[n.toks[0].pos:n.toks[len(n.toks)-1].end]); v.code != 0 {
				return v
			}
		}
		for _, b := range append([][]scriptNode{n.body, n.els, n.handler}, n.bodies...) {
			if v := checkStatements(sql, b); v.code != 0 {
				return v
			}
		}
	}
	return ddlVerdict{}
}

func (s *scriptRun) step() bool {
	s.steps++
	if s.steps > scriptStepLimit && s.notImpl == "" {
		s.notImpl = fmt.Sprintf("Not implemented here: the script ran over %d statements and evaluations, CloudBurrow's own "+
			"bound for a script with control flow, which it carries out itself (BigQuery's bound is the job's 6 hours). "+
			"What its statements did before that was kept.", scriptStepLimit)
	}
	return s.notImpl == ""
}

func (s *scriptRun) lookupVar(name string) (scriptVar, bool) {
	name = strings.ToLower(name)
	for i := len(s.scopes) - 1; i >= 0; i-- {
		if v, ok := s.scopes[i][name]; ok {
			return v, true
		}
	}
	return scriptVar{}, false
}

func (s *scriptRun) setVar(name string, v scriptVar) bool {
	name = strings.ToLower(name)
	for i := len(s.scopes) - 1; i >= 0; i-- {
		if _, ok := s.scopes[i][name]; ok {
			s.scopes[i][name] = v
			return true
		}
	}
	return false
}

// run runs nodes in order; a non-nil flow ends them early.
func (s *scriptRun) run(nodes []scriptNode) *scriptFlow {
	for _, n := range nodes {
		if !s.step() {
			return &scriptFlow{}
		}
		if flow := s.runNode(n); flow != nil {
			return flow
		}
		if s.notImpl != "" {
			return &scriptFlow{}
		}
	}
	return nil
}

func (s *scriptRun) runNode(n scriptNode) *scriptFlow {
	switch n.kind {
	case nodeStmt:
		if len(n.toks) == 1 && n.toks[0].is("RETURN") {
			return &scriptFlow{brk: true, label: "\x00return"}
		}
		if n.toks[0].is("RAISE") {
			return s.raise(n.toks)
		}
		return s.statement(n.toks)
	case nodeDeclare:
		return s.declare(n.toks)
	case nodeSet:
		return s.set(n.toks)
	case nodeBreak:
		return &scriptFlow{brk: true, label: n.target}
	case nodeContinue:
		return &scriptFlow{cont: true, label: n.target}
	case nodeIf:
		for i, c := range n.conds {
			ok, flow := s.condition(c)
			if flow != nil {
				return flow
			}
			if ok {
				return s.scoped(n.bodies[i])
			}
		}
		if n.hasEls {
			return s.scoped(n.els)
		}
		return nil
	case nodeCase:
		for i, c := range n.conds {
			text := s.text(c)
			if n.caseExpr != nil {
				text = "(" + s.text(n.caseExpr) + ") = (" + text + ")"
			}
			ok, flow := s.conditionText(text)
			if flow != nil {
				return flow
			}
			if ok {
				return s.scoped(n.bodies[i])
			}
		}
		if n.hasEls {
			return s.scoped(n.els)
		}
		return nil
	case nodeBlock:
		if !n.hasHandler {
			return s.scoped(n.body)
		}
		s.guarded++
		flow := s.scoped(n.body)
		s.guarded--
		if flow != nil && flow.failed && s.notImpl == "" {
			s.pending = ""
			s.scopes = append(s.scopes, map[string]scriptVar{"@@error": {}})
			errVars := map[string]scriptVar{
				"message":               {"STRING", sqlString(flow.msg)},
				"statement_text":        {"STRING", sqlString(flow.text)},
				"stack_trace":           {"ARRAY<STRUCT<line INT64, column INT64, filename STRING, location STRING>>", "CAST([] AS ARRAY<STRUCT<line INT64, column INT64, filename STRING, location STRING>>)"},
				"formatted_stack_trace": {"STRING", sqlString("")},
			}
			errVars["\x00raw"] = scriptVar{lit: flow.msg}
			s.inHandler = append(s.inHandler, errVars)
			out := s.run(n.handler)
			s.inHandler = s.inHandler[:len(s.inHandler)-1]
			s.scopes = s.scopes[:len(s.scopes)-1]
			return out
		}
		return flow
	case nodeLoop, nodeWhile, nodeRepeat:
		for {
			if n.kind == nodeWhile {
				ok, flow := s.condition(n.toks)
				if flow != nil {
					return flow
				}
				if !ok {
					return nil
				}
			}
			flow := s.scoped(n.body)
			if done, out := loopFlow(flow, n.label); done {
				return out
			}
			if !s.step() {
				return &scriptFlow{}
			}
			if n.kind == nodeRepeat {
				ok, flow := s.condition(n.toks)
				if flow != nil {
					return flow
				}
				if ok {
					return nil
				}
			}
		}
	case nodeFor:
		rows, flow := s.forRows(n.toks)
		if flow != nil {
			return flow
		}
		for _, row := range rows {
			s.scopes = append(s.scopes, map[string]scriptVar{strings.ToLower(n.forVar): row})
			flow := s.scoped(n.body)
			s.scopes = s.scopes[:len(s.scopes)-1]
			if done, out := loopFlow(flow, n.label); done {
				return out
			}
			if !s.step() {
				return &scriptFlow{}
			}
		}
		return nil
	}
	return nil
}

// loopFlow reads a loop body's flow: done when the loop ends, with the
// flow to pass on.
func loopFlow(flow *scriptFlow, label string) (bool, *scriptFlow) {
	switch {
	case flow == nil:
		return false, nil
	case flow.brk && (flow.label == "" || flow.label == label):
		return true, nil
	case flow.cont && (flow.label == "" || flow.label == label):
		return false, nil
	}
	return true, flow
}

func (s *scriptRun) scoped(nodes []scriptNode) *scriptFlow {
	s.scopes = append(s.scopes, map[string]scriptVar{})
	defer func() { s.scopes = s.scopes[:len(s.scopes)-1] }()
	return s.run(nodes)
}

// text returns the source text of toks, with each variable replaced by its
// literal, and @@error's fields in a handler.
func (s *scriptRun) text(toks []token) string {
	if len(toks) == 0 {
		return ""
	}
	src := s.sql
	vars := map[string]string{}
	for i := range s.scopes {
		for k, v := range s.scopes[i] {
			if !strings.HasPrefix(k, "@@") {
				vars[k] = "(" + v.lit + ")"
			}
		}
	}
	var edits []varEdit
	if len(vars) > 0 {
		refs, msg := variableRefs(src, toks, vars, s.f.variableColumns(s.r, s.q, ddlVerdict{}))
		if msg != "" && s.notImpl == "" {
			s.notImpl = msg
		}
		edits = refs
	}
	if len(s.inHandler) > 0 {
		errVars := s.inHandler[len(s.inHandler)-1]
		for i := 0; i+4 < len(toks); i++ {
			if toks[i].punct("@") && toks[i+1].punct("@") && toks[i+2].is("error") && toks[i+3].punct(".") {
				if v, ok := errVars[strings.ToLower(toks[i+4].text)]; ok {
					edits = append(edits, varEdit{toks[i].pos, toks[i+4].end, "(" + v.lit + ")"})
				}
			}
		}
	}
	start, end := toks[0].pos, toks[len(toks)-1].end
	var b strings.Builder
	last := start
	sortEdits(edits)
	for _, e := range edits {
		if e.pos < last {
			continue
		}
		b.WriteString(src[last:e.pos])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(src[last:end])
	return b.String()
}

func sortEdits(edits []varEdit) {
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && edits[j].pos < edits[j-1].pos; j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}
}

// flush runs the pending query, if any.
func (s *scriptRun) flush() *scriptFlow {
	if s.pending == "" {
		return nil
	}
	text := s.pending
	s.pending = ""
	rec, flow := s.send(text)
	if flow != nil {
		return flow
	}
	s.last = rec
	return nil
}

// statement runs a plain statement in its turn. A query is kept pending,
// to be the client's request if it is the last, unless an EXCEPTION
// block must see its failure.
func (s *scriptRun) statement(toks []token) *scriptFlow {
	if len(toks) == 0 {
		return nil
	}
	text := s.text(toks)
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	if flow := s.flush(); flow != nil {
		return flow
	}
	query := toks[0].is("SELECT") || toks[0].is("WITH") || toks[0].punct("(")
	if query && s.guarded == 0 {
		s.pending, s.last, s.lastQuery = text, nil, ""
		return nil
	}
	rec, flow := s.send(text)
	if flow != nil {
		return flow
	}
	s.last, s.lastQuery = rec, ""
	if query {
		// Run here, in an EXCEPTION block, to see its failure: should it
		// be the last, a query job is given its rows by running it again.
		s.lastQuery = text
	}
	return nil
}

// send runs text as a query of its own through the front, as jobs.query.
func (s *scriptRun) send(text string) (*recorder, *scriptFlow) {
	legacy := false
	opts := queryOptions{Query: text, UseLegacySQL: &legacy, DefaultDataset: s.q.DefaultDataset,
		ParameterMode: s.q.ParameterMode, QueryParameters: s.q.QueryParameters}
	body, err := json.Marshal(opts)
	if err != nil {
		return nil, &scriptFlow{failed: true, status: http.StatusInternalServerError, msg: err.Error(), text: text}
	}
	req, err := s.f.queryRequest(s.r, body)
	if err != nil {
		return nil, &scriptFlow{failed: true, status: http.StatusInternalServerError, msg: err.Error(), text: text}
	}
	rec := newRecorder()
	s.f.runQuery(rec, req, opts, false)
	var resp map[string]any
	if json.Unmarshal(rec.body.Bytes(), &resp) == nil {
		project, id := jobRef(resp)
		if project == "" {
			project = projectOf(s.f.base)
		}
		if id != "" {
			s.f.records.markInternal(project, id)
		}
	}
	if msg, failed := queryFailure(rec, nil); failed {
		status := rec.status
		if status == http.StatusOK || status == 0 {
			status = http.StatusBadRequest
		}
		if status == http.StatusNotImplemented {
			s.notImpl = msg
			return nil, &scriptFlow{}
		}
		return nil, &scriptFlow{failed: true, status: status, body: rec.body.Bytes(), msg: msg, text: text}
	}
	return rec, nil
}

// queryRequest is a jobs.query request of the front's own, with r's Host
// and context.
func (f front) queryRequest(r *http.Request, body []byte) (*http.Request, error) {
	u := *r.URL
	p := f.base + "/queries"
	u.Path, u.RawPath, u.RawQuery = p, "", ""
	if unescaped, err := url.PathUnescape(p); err == nil {
		u.Path, u.RawPath = unescaped, p
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	req.Proto, req.ProtoMajor, req.ProtoMinor = "HTTP/1.1", 1, 1
	return req, nil
}

// evaluate runs SELECT expr and returns its one value as a literal, with
// its type; typ, when given, is the type it is cast to.
func (s *scriptRun) evaluate(expr, typ string) (scriptVar, *scriptFlow) {
	q := "SELECT (" + expr + ")"
	if typ != "" {
		q = "SELECT CAST((" + expr + ") AS " + typ + ")"
	}
	vars, flow := s.query(q, 1)
	if flow != nil {
		return scriptVar{}, flow
	}
	if len(vars) != 1 {
		return scriptVar{}, &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "The expression " + expr + " gave no value", text: expr}
	}
	return vars[0], nil
}

// query runs q and returns each row as a literal: the value of its one
// column (columns 1), or a STRUCT of its columns (columns 0).
//
// A pending query is left pending: q only reads, as it does.
func (s *scriptRun) query(q string, columns int) ([]scriptVar, *scriptFlow) {
	rec, flow := s.send(q)
	if flow != nil {
		return nil, flow
	}
	var res struct {
		Schema tableSchema `json:"schema"`
		Rows   []struct {
			F []struct{ V any } `json:"f"`
		} `json:"rows"`
		PageToken string `json:"pageToken"`
	}
	dec := json.NewDecoder(bytes.NewReader(rec.body.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return nil, &scriptFlow{failed: true, status: http.StatusInternalServerError, msg: err.Error(), text: q}
	}
	if res.PageToken != "" {
		s.notImpl = "Not implemented here: a FOR ... IN query of more rows than one page of results. CloudBurrow carries a script with control flow out itself, and reads one page of the query's rows. Nothing more was run."
		return nil, &scriptFlow{}
	}
	var out []scriptVar
	for _, row := range res.Rows {
		var vals []any
		for _, c := range row.F {
			vals = append(vals, c.V)
		}
		if columns == 1 {
			if len(res.Schema.Fields) != 1 || len(vals) != 1 {
				return nil, &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "The expression gave more than one column", text: q}
			}
			v, msg := literalOf(res.Schema.Fields[0], vals[0])
			if msg != "" {
				s.notImpl = msg
				return nil, &scriptFlow{}
			}
			out = append(out, v)
			continue
		}
		v, msg := literalOf(field{Type: "RECORD", Fields: res.Schema.Fields}, map[string]any{"f": wrapCells(vals)})
		if msg != "" {
			s.notImpl = msg
			return nil, &scriptFlow{}
		}
		out = append(out, v)
	}
	return out, nil
}

func wrapCells(vals []any) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = map[string]any{"v": v}
	}
	return out
}

// condition evaluates a condition: true only when it is TRUE.
func (s *scriptRun) condition(toks []token) (bool, *scriptFlow) {
	return s.conditionText(s.text(toks))
}

func (s *scriptRun) conditionText(text string) (bool, *scriptFlow) {
	if !s.step() || s.notImpl != "" {
		return false, &scriptFlow{}
	}
	v, flow := s.evaluate(text, "BOOL")
	if flow != nil {
		return false, flow
	}
	return v.lit == "TRUE", nil
}

func (s *scriptRun) forRows(toks []token) ([]scriptVar, *scriptFlow) {
	text := s.text(toks)
	if s.notImpl != "" {
		return nil, &scriptFlow{}
	}
	return s.query("SELECT * FROM "+text, 0)
}

// declare carries out DECLARE a, b [type] [DEFAULT expr].
func (s *scriptRun) declare(toks []token) *scriptFlow {
	names, rest := declaredNames(toks[1:])
	var typToks, def []token
	for i, t := range rest {
		if t.is("DEFAULT") {
			typToks, def = rest[:i], rest[i+1:]
			break
		}
	}
	if def == nil {
		typToks = rest
	}
	typ := ""
	if len(typToks) > 0 {
		typ = s.sql[typToks[0].pos:typToks[len(typToks)-1].end]
	}
	var v scriptVar
	if def != nil {
		text := s.text(def)
		if s.notImpl != "" {
			return &scriptFlow{}
		}
		var flow *scriptFlow
		if v, flow = s.evaluate(text, typ); flow != nil {
			return flow
		}
	} else {
		if typ == "" {
			return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "DECLARE needs a type or a DEFAULT", text: s.text(toks)}
		}
		v = scriptVar{typ, "CAST(NULL AS " + typ + ")"}
	}
	for _, n := range names {
		s.scopes[len(s.scopes)-1][strings.ToLower(n)] = v
	}
	return nil
}

// set carries out SET x = expr and SET (a, b) = (expr, expr) or a STRUCT.
func (s *scriptRun) set(toks []token) *scriptFlow {
	body := toks[1:]
	if len(body) > 0 && body[0].punct("(") {
		s.notImpl = "Not implemented here: SET (a, b) = ... inside a script with control flow, which CloudBurrow carries out itself. Set each variable with a SET of its own. What the statements before it did was kept."
		return &scriptFlow{}
	}
	if len(body) < 3 || !body[1].punct("=") {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Syntax error in SET", text: s.text(toks)}
	}
	if body[0].punct("@") {
		s.notImpl = "Not implemented here: SET of a system variable inside a script with control flow. Nothing more was run."
		return &scriptFlow{}
	}
	name := body[0].text
	old, ok := s.lookupVar(name)
	if !ok {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Undeclared variable: " + name, text: s.text(toks)}
	}
	text := s.text(body[2:])
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	v, flow := s.evaluate(text, old.typ)
	if flow != nil {
		return flow
	}
	s.setVar(name, v)
	return nil
}

// literalOf writes a REST value of a schema field as a GoogleSQL literal,
// with its type; msg says why it cannot.
func literalOf(fl field, v any) (scriptVar, string) {
	typ, msg := sqlTypeOf(fl)
	if msg != "" {
		return scriptVar{}, msg
	}
	if v == nil {
		return scriptVar{typ, "CAST(NULL AS " + typ + ")"}, ""
	}
	if strings.EqualFold(fl.Mode, "REPEATED") {
		cells, _ := v.([]any)
		elem := fl
		elem.Mode = "NULLABLE"
		var lits []string
		for _, c := range cells {
			m, _ := c.(map[string]any)
			e, msg := literalOf(elem, m["v"])
			if msg != "" {
				return scriptVar{}, msg
			}
			lits = append(lits, e.lit)
		}
		return scriptVar{typ, typ + "[" + strings.Join(lits, ", ") + "]"}, ""
	}
	t := strings.ToUpper(fl.Type)
	if t == "RECORD" || t == "STRUCT" {
		m, _ := v.(map[string]any)
		cells, _ := m["f"].([]any)
		if len(cells) != len(fl.Fields) {
			return scriptVar{}, "Not implemented here: a STRUCT value CloudBurrow could not read, in a script with control flow."
		}
		var lits []string
		for i, sub := range fl.Fields {
			c, _ := cells[i].(map[string]any)
			e, msg := literalOf(sub, c["v"])
			if msg != "" {
				return scriptVar{}, msg
			}
			lits = append(lits, e.lit)
		}
		return scriptVar{typ, typ + "(" + strings.Join(lits, ", ") + ")"}, ""
	}
	s := fmt.Sprint(v)
	switch t {
	case "STRING":
		return scriptVar{typ, sqlString(s)}, ""
	case "BOOLEAN", "BOOL":
		if strings.EqualFold(s, "true") {
			return scriptVar{typ, "TRUE"}, ""
		}
		return scriptVar{typ, "FALSE"}, ""
	case "BYTES":
		return scriptVar{typ, "FROM_BASE64(" + sqlString(s) + ")"}, ""
	case "JSON":
		return scriptVar{typ, "PARSE_JSON(" + sqlString(s) + ")"}, ""
	case "GEOGRAPHY":
		return scriptVar{typ, "ST_GEOGFROMTEXT(" + sqlString(s) + ")"}, ""
	case "TIMESTAMP":
		// Seconds since the epoch, as a decimal ("1.5E9" or "1577836800.5").
		r, ok := new(big.Rat).SetString(s)
		if !ok {
			return scriptVar{}, "Not implemented here: a TIMESTAMP value CloudBurrow could not read (" + s + "), in a script with control flow."
		}
		micros := new(big.Rat).Mul(r, big.NewRat(1000000, 1))
		n := new(big.Int).Quo(micros.Num(), micros.Denom())
		return scriptVar{typ, "TIMESTAMP_MICROS(" + n.String() + ")"}, ""
	}
	return scriptVar{typ, "CAST(" + sqlString(s) + " AS " + typ + ")"}, ""
}

// sqlTypeOf writes a schema field's type in GoogleSQL.
func sqlTypeOf(fl field) (string, string) {
	var t string
	switch u := strings.ToUpper(fl.Type); u {
	case "INTEGER", "INT64":
		t = "INT64"
	case "FLOAT", "FLOAT64":
		t = "FLOAT64"
	case "BOOLEAN", "BOOL":
		t = "BOOL"
	case "STRING", "BYTES", "DATE", "DATETIME", "TIME", "TIMESTAMP", "NUMERIC", "BIGNUMERIC", "JSON", "GEOGRAPHY", "INTERVAL":
		t = u
	case "RECORD", "STRUCT":
		var parts []string
		for _, sub := range fl.Fields {
			st, msg := sqlTypeOf(sub)
			if msg != "" {
				return "", msg
			}
			parts = append(parts, "`"+strings.ReplaceAll(sub.Name, "`", "\\`")+"` "+st)
		}
		t = "STRUCT<" + strings.Join(parts, ", ") + ">"
	default:
		return "", "Not implemented here: a value of type " + fl.Type + " in a script variable, FOR or condition. CloudBurrow carries a script with control flow out itself, and writes each value as a literal, which it cannot for this type. What the statements before it did was kept."
	}
	if strings.EqualFold(fl.Mode, "REPEATED") {
		t = "ARRAY<" + t + ">"
	}
	return t, ""
}

// raise carries out RAISE [USING MESSAGE = expr]: the script fails with
// the message, which an EXCEPTION block may catch. A bare RAISE in a
// handler raises the error it handles again.
func (s *scriptRun) raise(toks []token) *scriptFlow {
	text := s.text(toks)
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	var msg string
	switch {
	case len(toks) == 1 && len(s.inHandler) > 0:
		msg = s.inHandler[len(s.inHandler)-1]["\x00raw"].lit
	case len(toks) > 4 && toks[1].is("USING") && toks[2].is("MESSAGE") && toks[3].punct("="):
		expr := s.text(toks[4:])
		if s.notImpl != "" {
			return &scriptFlow{}
		}
		v, flow := s.evaluate(expr, "STRING")
		if flow != nil {
			return flow
		}
		msg = unquoteRaised(v.lit)
	default:
		s.notImpl = "Not implemented here: RAISE without USING MESSAGE outside an EXCEPTION handler. What the statements before it did was kept."
		return &scriptFlow{}
	}
	if flow := s.flush(); flow != nil {
		return flow
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": http.StatusBadRequest, "message": msg,
		"errors": []map[string]string{{"message": msg, "domain": "global", "reason": "invalidQuery"}}, "status": "INVALID_ARGUMENT"}})
	return &scriptFlow{failed: true, status: http.StatusBadRequest, body: body, msg: msg, text: text}
}

// unquoteRaised reads back a STRING literal sqlString wrote; NULL is "".
func unquoteRaised(lit string) string {
	if !strings.HasPrefix(lit, "'") {
		return ""
	}
	var b strings.Builder
	in := lit[1 : len(lit)-1]
	for i := 0; i < len(in); i++ {
		c := in[i]
		if c == '\\' && i+1 < len(in) {
			i++
			if in[i] == 'U' && i+9 <= len(in) {
				var r rune
				fmt.Sscanf(in[i+1:i+9], "%08x", &r)
				b.WriteRune(r)
				i += 8
				continue
			}
			b.WriteByte(in[i])
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
