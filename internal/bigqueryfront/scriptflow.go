package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
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
//   - The script's answer is its last statement's result: "jobs.getQueryResults
//     returns the query results for the last statement to execute"
//     (https://cloud.google.com/bigquery/docs/multi-statement-queries). A
//     query (SELECT or WITH) or a statement that changes data (INSERT,
//     UPDATE, DELETE, MERGE) is held back until the next statement or read
//     needs it run, so that when it is the last it is sent as the client's
//     own request: a query job's results are its rows, its DML row count
//     its own. One run inside an EXCEPTION block runs at once, to see its
//     failure. A script whose last statement is a DECLARE or a SET answers
//     with no rows, as that statement has none; one that ran nothing
//     answers so too.
//
// RAISE USING MESSAGE fails the script with its message, which a handler
// may catch; a bare RAISE in a handler raises its error again. A script
// with a RAISE and no block is carried out so too.
//
// EXECUTE IMMEDIATE runs its SQL, evaluated in its turn, with its USING
// values each evaluated once, and INTO sets the variables to the query's one
// row's columns (NULL with no row, an error with more than one row or a
// number of columns other than the variables'), each statement it runs a
// child job of its own (#1037); @@row_count is the last DML statement's
// count (rowCountEdits); SET (a, b)
// = expr sets each variable to a field of the STRUCT expr gives. In an
// EXCEPTION handler @@error.stack_trace holds the failed statement's line
// and column in the script, and @@error.formatted_stack_trace reads
// "At [line:column]", the forms of BigQuery's reference.
//
// BigQuery's documented limits (https://cloud.google.com/bigquery/quotas,
// "Multi-statement queries"; multi-statement-queries, "Variables") apply:
// the script fails once it has run for scriptTimeLimit, 24 hours, its
// cumulative time limit, and a variable of over 1 MB, or variables of over
// 10 MB together, fail it. It also stops once the client's request is gone.
//
// Transactions are transactions.go; a RANGE value is written as
// RANGE(lower, upper) (rangeLiteral). What it cannot do faithfully is 501.

// scriptTimeLimit is BigQuery's cumulative time limit for a multi-statement
// query; scriptNow is the clock it is measured with.
var (
	scriptTimeLimit = 24 * time.Hour
	scriptNow       = time.Now
)

// BigQuery's limits on a script's variables: 1 MB each, 10 MB in all.
const (
	scriptVarLimit  = 1 << 20
	scriptVarsLimit = 10 << 20
)

// hasControlFlow reports whether sql is a script with a control-flow
// block or an EXCEPTION handler, which the front carries out (runScript).
func hasControlFlow(sql string) bool {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "IF") && !strings.Contains(upper, "LOOP") && !strings.Contains(upper, "WHILE") &&
		!strings.Contains(upper, "REPEAT") && !strings.Contains(upper, "FOR") && !strings.Contains(upper, "CASE") &&
		!strings.Contains(upper, "EXCEPTION") && !strings.Contains(upper, "RAISE") && !strings.Contains(upper, "PROCEDURE") &&
		!strings.Contains(upper, "CALL") && !strings.Contains(upper, "ALTER") && !strings.Contains(upper, "TRAN") &&
		!strings.Contains(upper, "VIEW") && !strings.Contains(upper, "OPTIONS") {
		return false
	}
	toks, ok := lex(sql)
	if !ok {
		return false
	}
	// A script with a transaction, or with a
	// ROLLBACK, which the emulator refuses ("Statement not supported:
	// RollbackStatement", measured), is carried out by the front
	// (transactions.go). Another keeps going to the emulator as before.
	txn, rollback := false, false
	for _, stmt := range splitStatements(toks) {
		body, _, _ := stripControlFlow(stmt)
		if isTransaction(body) {
			txn = true
			rollback = rollback || body[0].is("ROLLBACK")
		}
	}
	if txn {
		p := &scriptParser{toks: toks}
		// One it cannot is 501 there (unsupportedInScript): the emulator
		// would ignore its BEGIN and COMMIT.
		if p.block(); rollback || p.err == "" {
			return true
		}
	}
	temp := tokensHaveTemp(toks)
	stmts := 0
	for _, stmt := range splitStatements(toks) {
		if len(stmt) > 0 && !(len(stmt) == 1 && stmt[0].punct(";")) {
			stmts++
		}
	}
	for _, stmt := range splitStatements(toks) {
		if body, block, handler := stripControlFlow(stmt); block != "" || handler || len(body) > 0 && body[0].is("RAISE") ||
			isProcedureStatement(body) || // #1217, procedures.go
			!temp && isAlterTable(body) || // #1227, altertable.go; with a TEMP table 501 (checkDDL, #1416)
			!temp && isViewColumnList(sql, body) || // #1323, viewcolumns.go
			!temp && stmts > 1 && hasDDLOptions(sql, body) || // #1390, ddloptions.go; a lone one is runQuery's
			!temp && stmts > 1 && isMVBody(sql, body) { // #1415, materializedviews.go
			return true
		}
	}
	return false
}

// earlierQuery's verdicts on a script.
const (
	scriptAsIs      = iota // sent to the emulator as it is
	scriptSplits           // carried out by the front, statement by statement (runScript)
	scriptKeepsTemp        // 501: a TEMP table keeps it in one request
)

// earlierQuery is the verdict on sql, a script with no control flow, when a
// statement before its last is a query (SELECT, WITH, a parenthesized
// query) (#1249).
//
// The emulator reads the rows of a script's last statement only: measured
// against the pinned image through the front, `INSERT INTO tx.t VALUES (5);
// SELECT 1/0; INSERT INTO tx.t VALUES (6)` answered done and kept both
// rows. BigQuery fails
// the script at that statement, keeping what the statements before it did
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/procedural-language).
// So such a script is carried out by the front, as a script with control
// flow is: each statement is a request of its own, whose error is seen, and
// the script fails at the first that fails. A TEMP table lasts only for the
// request that made it, so a script with one cannot be split: it is 501.
//
// The emulator's ASSERT does nothing at all, even ASSERT FALSE alone
// (measured, #1253), so a script with an ASSERT anywhere, or a lone ASSERT,
// is carried out by the front too, which checks it (assert).
// Any other script is sent on as it is.
func earlierQuery(sql string) int {
	upper := strings.ToUpper(sql)
	if !strings.Contains(upper, "SELECT") && !strings.Contains(upper, "ASSERT") {
		return scriptAsIs
	}
	toks, ok := lex(sql)
	if !ok {
		return scriptAsIs
	}
	var stmts [][]token
	for _, st := range splitStatements(toks) {
		if len(st) > 0 && !(len(st) == 1 && st[0].punct(";")) {
			stmts = append(stmts, st)
		}
	}
	found := false
	for i, st := range stmts {
		if st[0].is("ASSERT") || i < len(stmts)-1 && (st[0].is("SELECT") || st[0].is("WITH") || st[0].punct("(")) {
			found = true
		}
	}
	if !found {
		return scriptAsIs
	}
	if tokensHaveTemp(toks) {
		return scriptKeepsTemp
	}
	return scriptSplits
}

// hasTemp reports whether sql names TEMP or TEMPORARY: a TEMP table or
// function lasts only for the request that made it, so such a script is
// not run a statement at a time.
func hasTemp(sql string) bool {
	toks, ok := lex(sql)
	return !ok || tokensHaveTemp(toks)
}

func tokensHaveTemp(toks []token) bool {
	for _, t := range toks {
		if t.is("TEMP") || t.is("TEMPORARY") {
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
	nodeCreateProc // CREATE PROCEDURE (procedures.go)
	nodeDropProc
	nodeCall
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
	// procSpan is a CREATE PROCEDURE's body, BEGIN ... END, in the script.
	procSpan [2]int
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
	case p.isCreateProcedure():
		n2 := p.createProcedure()
		n2.label = n.label
		p.semicolon()
		return n2
	case t.is("DROP") && p.at("DROP", "PROCEDURE"):
		n.kind = nodeDropProc
		n.toks = p.statement()
		return n
	case t.is("CALL"):
		n.kind = nodeCall
		n.toks = p.statement()
		return n
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
	// pos is the failed statement's offset in the script; hasPos once
	// set, by the innermost statement.
	pos    int
	hasPos bool
	// frames are the procedures' frames the failure passed, innermost
	// first (procedures.go).
	frames []stackFrame
}

type scriptRun struct {
	f      front
	r      *http.Request
	q      queryOptions
	sql    string
	scopes []map[string]scriptVar
	steps  int
	// depth is the procedure calls the statement running is in; loc the
	// innermost procedure's project.dataset.name (procedures.go).
	depth int
	loc   string
	// last is the response to the last statement run, internally; pending
	// a query (SELECT or WITH) whose turn it is, kept to be sent last.
	last      *recorder
	lastQuery string
	pending   string
	inHandler []map[string]scriptVar // @@error in the handler running
	guarded   int                    // depth of EXCEPTION blocks around the statement running
	notImpl   string
	// pendingDML: pending is a statement that changes data, run before
	// anything else is sent.
	pendingDML bool
	start      time.Time
	// txn is the transaction open; goneTemps the TEMP tables
	// transactions made and left (transactions.go).
	txn       *txnState
	goneTemps map[string]bool
	// limit is the error a BigQuery limit failed the script with.
	limit string
	// plain: the script has no control flow, and is carried out here only
	// so its earlier queries' errors are seen (earlierQuery, #1249). Its
	// answer is then the one the emulator gave such a script: a DECLARE,
	// SET or EXECUTE IMMEDIATE ... INTO after the last query leaves that
	// query's rows the answer (#1037).
	plain bool
	// children are the jobs the script ran, its child jobs
	// (scriptjobs.go); expr counts the evaluations running, whose jobs
	// are EXPRESSION children.
	children *[]scriptChild
	expr     int
	// rowCount is @@row_count, the rows the last DML statement changed
	// (NULL before one, and after a statement of another kind); frames
	// place a statement EXECUTE IMMEDIATE ran, by its text, at the EXECUTE
	// IMMEDIATE; intoQuery: the query running is EXECUTE IMMEDIATE ...
	// INTO's, a STATEMENT child (execimmediate.go, #1037).
	rowCount  string
	frames    map[string]scriptChild
	intoQuery bool
	// dry: the run is a dry run's, which checks an ALTER TABLE and
	// changes nothing (altertabledry.go, #1416).
	dry bool
}

// runScript carries out a script with control flow (above), and records
// the jobs it ran as the children of its job (scriptjobs.go, #1353).
func (f front) runScript(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) {
	var kids []scriptChild
	rec := newRecorder()
	f.runScriptIn(rec, r, q, insert, &kids)
	f.records.adoptScript(w, rec, kids, insert, projectOf(f.base))
}

func (f front) runScriptIn(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, kids *[]scriptChild) {
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
	s := &scriptRun{f: f, r: r, q: q, sql: q.Query, scopes: []map[string]scriptVar{{}}, start: scriptNow(),
		plain: !hasControlFlow(q.Query), children: kids}
	flow := s.run(nodes)
	if s.txn != nil {
		// The query ended with a transaction open: rolled back, and its
		// probes have no rows.
		s.txn, s.pending, s.pendingDML = nil, "", false
		s.last, s.lastQuery = nil, ""
	}
	if s.limit != "" {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusBadRequest, rowError{Reason: "invalidQuery", Message: s.limit})
		return
	}
	if s.notImpl != "" {
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", http.StatusNotImplemented, rowError{Reason: "notImplemented", Message: s.notImpl})
		return
	}
	if flow != nil && flow.failed {
		if !insert {
			if len(flow.body) == 0 {
				// A failure of the front's own, with its message only.
				writeError(w, flow.status, "invalidQuery", flow.msg)
				return
			}
			writeRaw(w, flow.status, flow.body)
			return
		}
		// A query job fails as the statement did: a table not found is
		// 404 notFound (#1227), a statement the front refused with a
		// reason of BigQuery's (duplicate, resourceInUse) with it (#1459),
		// anything else 400 invalidQuery.
		status, reason := http.StatusBadRequest, "invalidQuery"
		var e struct {
			Error struct {
				Errors []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(flow.body, &e)
		if flow.status == http.StatusNotFound && len(e.Error.Errors) > 0 && e.Error.Errors[0].Reason == "notFound" {
			status, reason = http.StatusNotFound, "notFound"
		} else if len(e.Error.Errors) > 0 && (flow.status == http.StatusConflict || flow.status == http.StatusBadRequest) &&
			(e.Error.Errors[0].Reason == "duplicate" || e.Error.Errors[0].Reason == "resourceInUse") {
			status, reason = flow.status, e.Error.Errors[0].Reason
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", status, rowError{Reason: reason, Message: flow.msg})
		return
	}
	client := jobText{query: q.Query}
	if s.pending != "" && !s.pendingDML {
		// The last query is a child job too: run once more, as it only
		// reads (scriptjobs.go). Its failure is the client's request's.
		s.send(s.pending)
	}
	if insert && s.pending == "" && s.lastQuery != "" {
		s.pending = s.lastQuery
	}
	if s.pending != "" {
		// The last statement was a query: it is the client's request.
		if r.Context().Value(dpMemoKey{}) != nil {
			// Its noisy rows are its child job's (#1482, differentialprivacy.go).
			r = r.WithContext(context.WithValue(r.Context(), dpReplayKey{}, true))
		}
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
	// No statement, or a DECLARE or SET, ran last, or a statement that
	// changed data in an EXCEPTION block: the job is recorded with a
	// statement that does nothing.
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
			if n.kind == nodeCreateProc && hasTransaction(n.body) {
				return "a transaction in a procedure. CloudBurrow carries out a transaction only in the script that begins and ends it."
			}
			// Each block's transactions (transactions.go).
			for _, b := range append([][]scriptNode{n.body, n.els, n.handler}, n.bodies...) {
				if msg := unsupportedTransactions(b, false); msg != "" {
					return msg
				}
				if msg := walk(b); msg != "" {
					return msg
				}
			}
		}
		return ""
	}
	if msg := unsupportedTransactions(nodes, true); msg != "" { // transactions.go
		return "Not implemented here: " + msg + " Nothing was run."
	}
	if msg := walk(nodes); msg != "" {
		return "Not implemented here: " + msg + " Nothing was run."
	}
	return ""
}

// splitCures reports whether sql, a script of several statements that
// checkDDL refuses 501 or that has a MERGE from a subquery, is carried out
// by the front a statement at a time instead (#1459): it has no TEMP table
// or function (a session's, tempsession.go, has none), and no statement of
// it is refused when it runs alone (checkStatements). What the emulator
// cannot run inside a script, but can as a statement of its own (CREATE
// OR REPLACE TABLE of a table that exists, CREATE TABLE ... LIKE, COPY or
// CLONE, CREATE SCHEMA of a dataset that exists, DROP SCHEMA of a dataset
// the script names, CREATE FUNCTION of one that exists, DROP FUNCTION,
// MERGE from a subquery), is so run in its turn, each its own child job,
// as BigQuery runs each statement of a script. So is every other script of
// several statements that checkDDL passes (dml.go): sent whole, it would
// make no child jobs, and the emulator would roll it back whole through
// jobs.query when a statement failed.
func splitCures(sql string) bool {
	if !strings.Contains(sql, ";") {
		return false
	}
	toks, ok := lex(sql)
	if !ok || tokensHaveTemp(toks) {
		return false
	}
	n := 0
	for _, st := range splitStatements(toks) {
		body, _, _ := stripControlFlow(st)
		if isTransaction(body) {
			return false
		}
		if len(st) > 0 && !(len(st) == 1 && st[0].punct(";")) {
			n++
		}
	}
	if n < 2 {
		return false
	}
	p := &scriptParser{toks: toks}
	nodes := p.block()
	if p.err != "" || unsupportedInScript(nodes) != "" {
		return false
	}
	return checkStatements(sql, nodes).code == 0
}

// checkStatements is checkDDL's verdict on each statement of nodes, the
// first refusal.
func checkStatements(sql string, nodes []scriptNode) ddlVerdict {
	for _, n := range nodes {
		if n.kind == nodeStmt && isAlterTable(n.toks) {
			// Carried out by the front (#1227, altertable.go): its names
			// are checked, and what it does not carry out is 501.
			if msg := checkAlter(n.toks); msg != "" {
				return ddlVerdict{code: http.StatusBadRequest, reason: "invalidQuery", msg: msg}
			}
			if what := alterUnsupported(n.toks); what != "" {
				return ddlVerdict{code: http.StatusNotImplemented, reason: "notImplemented", msg: alterNotImplemented(what)}
			}
		} else if n.kind == nodeStmt && len(n.toks) > 0 && n.toks[0].is("ASSERT") {
			// Carried out by the front (#1253, assert).
			if _, _, _, msg := assertParts(n.toks); msg != "" {
				return ddlVerdict{code: http.StatusBadRequest, reason: "invalidQuery", msg: msg}
			}
		} else if n.kind == nodeStmt && isMVBody(sql, n.toks) {
			// Carried out in its turn by the front (#1415,
			// materializedviews.go): what it does not set is 501.
			if st, _ := parseMVStatement(sql[n.toks[0].pos:n.toks[len(n.toks)-1].end]); st.notImpl {
				return ddlVerdict{code: http.StatusNotImplemented, reason: "notImplemented", msg: "Not implemented here: " +
					st.msg + " in a script. Nothing was run."}
			}
		} else if vc, ok := scriptViewColumnList(sql, n); ok {
			// Carried out alone in its turn (#1323, viewcolumns.go): its
			// listed names are checked here, and the rest of it as a
			// CREATE VIEW without the list.
			if msg := vc.checkNames(); msg != "" {
				return ddlVerdict{code: http.StatusBadRequest, reason: "invalidQuery", msg: msg}
			}
			if v := checkDDL(vc.withoutList()); v.code != 0 {
				return v
			}
		} else if n.kind == nodeStmt && len(n.toks) > 0 && !n.toks[0].is("RAISE") {
			text := sql[n.toks[0].pos:n.toks[len(n.toks)-1].end]
			if like, ok := rewriteTableLikeCopy(text); ok {
				// Sent on alone as CREATE TABLE AS SELECT (#1239,
				// tablelikecopy.go), in its turn (#1459).
				text = like
			} else if _, unsupported, ok := parseCloneDDL(text); ok {
				// Carried out alone by the front (#1002, cloneddl.go).
				if unsupported != "" {
					return ddlVerdict{code: http.StatusNotImplemented, reason: "notImplemented",
						msg: "Not implemented here: " + unsupported + ". Nothing was run."}
				}
				text = ""
			}
			if v := checkDDL(text); text != "" && v.code != 0 {
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

// scriptViewColumnList reads n, a statement of the script sql, as a CREATE
// VIEW with a column list.
func scriptViewColumnList(sql string, n scriptNode) (viewColumnList, bool) {
	if n.kind != nodeStmt || len(n.toks) < 8 || !n.toks[0].is("CREATE") {
		return viewColumnList{}, false
	}
	return parseViewColumnList(sql[n.toks[0].pos:n.toks[len(n.toks)-1].end])
}

// step reports whether the script may go on: not past BigQuery's time
// limit, and its client still waiting. A stop is kept in notImpl, so that
// every caller ends, and in limit, the script's error.
func (s *scriptRun) step() bool {
	s.steps++
	if s.notImpl == "" {
		switch {
		case s.r.Context().Err() != nil:
			s.notImpl, s.limit = "stopped", "The request was cancelled."
		case scriptNow().Sub(s.start) > scriptTimeLimit:
			s.notImpl = "stopped"
			s.limit = fmt.Sprintf("Script exceeded the cumulative time limit for a multi-statement query, %s. "+
				"What its statements did before that was kept.", formatLimit(scriptTimeLimit))
		}
	}
	return s.notImpl == ""
}

func formatLimit(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d hours", d/time.Hour)
	}
	return d.String()
}

// assign sets name to v in scope (a DECLARE) or where it is declared
// (scope nil), within BigQuery's limits on variables' sizes.
func (s *scriptRun) assign(name string, v scriptVar, scope map[string]scriptVar, text string) *scriptFlow {
	fail := func(msg string) *scriptFlow {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": http.StatusBadRequest, "message": msg,
			"errors": []map[string]string{{"message": msg, "domain": "global", "reason": "invalidQuery"}}, "status": "INVALID_ARGUMENT"}})
		return &scriptFlow{failed: true, status: http.StatusBadRequest, body: body, msg: msg, text: text}
	}
	if len(v.lit) > scriptVarLimit {
		return fail(fmt.Sprintf("Variable %s exceeds the 1 MB size limit for a variable in a multi-statement query.", name))
	}
	name = strings.ToLower(name)
	if scope != nil {
		scope[name] = v
	} else if !s.setVar(name, v) {
		return fail("Undeclared variable: " + name)
	}
	total := 0
	for _, sc := range s.scopes {
		for k, x := range sc {
			if !strings.HasPrefix(k, "@@") {
				total += len(x.lit)
			}
		}
	}
	if total > scriptVarsLimit {
		return fail("The variables of the multi-statement query exceed the 10 MB size limit for all of its variables.")
	}
	return nil
}

// nodePos is the offset in the script of n's first token, or -1.
func nodePos(n scriptNode) int {
	switch {
	case len(n.toks) > 0:
		return n.toks[0].pos
	case len(n.caseExpr) > 0:
		return n.caseExpr[0].pos
	case len(n.conds) > 0 && len(n.conds[0]) > 0:
		return n.conds[0][0].pos
	}
	return -1
}

// lineColumn is the 1-based line and column, in characters, of offset pos
// in sql.
func lineColumn(sql string, pos int) (int, int) {
	if pos > len(sql) {
		pos = len(sql)
	}
	before := sql[:pos]
	line := strings.Count(before, "\n") + 1
	col := utf8.RuneCountInString(before[strings.LastIndex(before, "\n")+1:]) + 1
	return line, col
}

// errorVars are @@error's fields in a handler for flow, in BigQuery's
// forms: a stack of one frame, the failed statement's, whose filename and
// location are NULL in a script that is not a procedure.
func (s *scriptRun) errorVars(flow *scriptFlow) map[string]scriptVar {
	const frame = "STRUCT<line INT64, column INT64, filename STRING, location STRING>"
	frames := append([]stackFrame(nil), flow.frames...)
	if flow.hasPos {
		line, col := lineColumn(s.sql, flow.pos)
		frames = append(frames, stackFrame{line, col, s.loc})
	}
	stack := "CAST([] AS ARRAY<" + frame + ">)"
	var items, lines []string
	for _, fr := range frames {
		loc, at := "NULL", ""
		if fr.loc != "" {
			loc, at = sqlString(fmt.Sprintf("%s:%d:%d", fr.loc, fr.line, fr.col)), fr.loc
		}
		items = append(items, fmt.Sprintf("STRUCT<line INT64, column INT64, filename STRING, location STRING>(%d, %d, NULL, %s)", fr.line, fr.col, loc))
		lines = append(lines, fmt.Sprintf("At %s[%d:%d]", at, fr.line, fr.col))
	}
	if len(items) > 0 {
		stack = "[" + strings.Join(items, ", ") + "]"
	}
	formatted := strings.Join(lines, "\n")
	return map[string]scriptVar{
		"message":               {"STRING", sqlString(flow.msg)},
		"statement_text":        {"STRING", sqlString(flow.text)},
		"stack_trace":           {"ARRAY<" + frame + ">", stack},
		"formatted_stack_trace": {"STRING", sqlString(formatted)},
		"\x00raw":               {lit: flow.msg},
	}
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
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		if !s.step() {
			return &scriptFlow{}
		}
		if flow := s.readyRowCount(n.toks, n.caseExpr, n.conds); flow != nil { // #1037
			return flow
		}
		if flow := s.runNode(n); flow != nil {
			if flow.failed && !flow.hasPos {
				if pos := nodePos(n); pos >= 0 {
					flow.pos, flow.hasPos = pos, true
				}
			}
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
		if n.toks[0].is("ASSERT") {
			return s.assert(n.toks)
		}
		if isTxnNode(n, "BEGIN") {
			return s.beginTransaction(n) // transactions.go
		}
		if isTxnNode(n, "COMMIT") || isTxnNode(n, "ROLLBACK") {
			return s.endTransaction(n) // transactions.go
		}
		if isAlterTable(n.toks) {
			return s.alterTable(n.toks) // #1227, altertable.go
		}
		return s.statement(n.toks)
	case nodeDeclare:
		return s.declare(n.toks)
	case nodeSet:
		return s.set(n.toks)
	case nodeCreateProc:
		return s.createProc(n)
	case nodeDropProc:
		return s.dropProc(n.toks)
	case nodeCall:
		return s.call(n.toks)
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
		// What is held back runs before the block, whose handler is not
		// its.
		if flow := s.flush(); flow != nil {
			return flow
		}
		s.guarded++
		flow := s.scoped(n.body)
		s.guarded--
		if flow != nil && flow.failed && s.notImpl == "" {
			s.pending = ""
			s.scopes = append(s.scopes, map[string]scriptVar{"@@error": {}})
			s.inHandler = append(s.inHandler, s.errorVars(flow))
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
	edits = append(edits, s.rowCountEdits(toks)...) // execimmediate.go, #1037
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
	text, dml := s.pending, s.pendingDML
	s.pending, s.pendingDML = "", false
	rec, flow := s.send(text)
	if flow != nil {
		return flow
	}
	s.noteRowCount(text, rec)
	s.last = rec
	if s.plain && !dml {
		// A query job is given its rows by running it again, should it
		// stay the answer (plain).
		s.lastQuery = text
	}
	return nil
}

// statement runs a plain statement in its turn. A query is kept pending,
// to be the client's request if it is the last, unless an EXCEPTION
// block must see its failure.
func (s *scriptRun) statement(toks []token) *scriptFlow {
	if len(toks) == 0 {
		return nil
	}
	if toks[0].is("EXECUTE") {
		return s.executeImmediate(toks)
	}
	text := s.text(toks)
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	return s.runText(text)
}

// runText runs a statement's text in its turn (statement).
func (s *scriptRun) runText(text string) *scriptFlow {
	if flow := s.flush(); flow != nil {
		return flow
	}
	toks, _ := lex(text)
	if len(toks) == 0 {
		return nil
	}
	if s.goneTemp(toks) { // transactions.go
		return &scriptFlow{}
	}
	query := toks[0].is("SELECT") || toks[0].is("WITH") || toks[0].punct("(")
	dml := toks[0].is("INSERT") || toks[0].is("UPDATE") || toks[0].is("DELETE") || toks[0].is("MERGE")
	if s.txn != nil {
		// Inside a transaction (transactions.go): run in its turn; a
		// probe has no rows.
		rec, flow := s.sendInTxn(text)
		if flow != nil {
			return flow
		}
		s.noteRowCount(text, rec)
		s.last, s.lastQuery = rec, ""
		if query && rec != nil {
			s.lastQuery = text
		}
		return nil
	}
	if (query || dml) && s.guarded == 0 {
		s.pending, s.pendingDML, s.last, s.lastQuery = text, dml, nil, ""
		return nil
	}
	rec, flow := s.send(text)
	if flow != nil {
		return flow
	}
	s.noteRowCount(text, rec)
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
	if !txnRequest(s.r) && s.txn == nil && len(s.goneTemps) > 0 { // transactions.go
		if toks, _ := lex(text); s.goneTemp(toks) {
			return nil, &scriptFlow{}
		}
	}
	if s.pendingDML {
		// A statement that changes data runs before anything that may
		// read what it changed.
		if flow := s.flush(); flow != nil {
			return nil, flow
		}
	}
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
			if s.children != nil {
				*s.children = append(*s.children, s.child(project, id, text))
			}
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
		if _, id := jobRef(resp); id == "" {
			s.failedChild(text, rec, msg)
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
// column (columns 1), a STRUCT of its columns (columns 0), or each column's
// value in cols (columns -1). It reads every page of the rows.
//
// A pending query is left pending: q only reads, as it does.
func (s *scriptRun) query(q string, columns int) ([]scriptVar, *scriptFlow) {
	out, _, flow := s.queryRows(q, columns)
	return out, flow
}

// queryRows is query; with columns -1, the rows are each row's columns.
func (s *scriptRun) queryRows(q string, columns int) ([]scriptVar, [][]scriptVar, *scriptFlow) {
	out, rows, _, flow := s.queryAll(q, columns)
	return out, rows, flow
}

// queryColumns is queryRows of each row's columns, with the number of
// columns the query's schema has.
func (s *scriptRun) queryColumns(q string) ([]scriptVar, [][]scriptVar, int, *scriptFlow) {
	return s.queryAll(q, -1)
}

func (s *scriptRun) queryAll(q string, columns int) (out []scriptVar, rows [][]scriptVar, ncols int, flow *scriptFlow) {
	defer func() {
		if flow != nil {
			out, rows, ncols = nil, nil, 0
		}
	}()
	if s.txn != nil && !s.readInTxn(q) { // transactions.go
		return nil, nil, 0, &scriptFlow{}
	}
	s.expr++
	rec, flow := s.send(q)
	s.expr--
	if flow != nil {
		return nil, nil, 0, flow
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
		return nil, nil, 0, &scriptFlow{failed: true, status: http.StatusInternalServerError, msg: err.Error(), text: q}
	}
	allRows := res.Rows
	for res.PageToken != "" {
		var job map[string]any
		_ = json.Unmarshal(rec.body.Bytes(), &job)
		_, id := jobRef(job)
		token := res.PageToken
		res.Rows, res.PageToken = nil, ""
		page, flow := s.page(id, token, q)
		if flow != nil {
			return nil, nil, 0, flow
		}
		dec := json.NewDecoder(bytes.NewReader(page))
		dec.UseNumber()
		if err := dec.Decode(&res); err != nil {
			return nil, nil, 0, &scriptFlow{failed: true, status: http.StatusInternalServerError, msg: err.Error(), text: q}
		}
		allRows = append(allRows, res.Rows...)
	}
	for _, row := range allRows {
		var vals []any
		for _, c := range row.F {
			vals = append(vals, c.V)
		}
		if columns == -1 {
			var cols []scriptVar
			for i, fl := range res.Schema.Fields {
				if i >= len(vals) {
					break
				}
				v, msg := literalOf(fl, vals[i])
				if msg != "" {
					s.notImpl = msg
					return nil, nil, 0, &scriptFlow{}
				}
				cols = append(cols, v)
			}
			rows = append(rows, cols)
			continue
		}
		if columns == 1 {
			if len(res.Schema.Fields) != 1 || len(vals) != 1 {
				return nil, nil, 0, &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "The expression gave more than one column", text: q}
			}
			v, msg := literalOf(res.Schema.Fields[0], vals[0])
			if msg != "" {
				s.notImpl = msg
				return nil, nil, 0, &scriptFlow{}
			}
			out = append(out, v)
			continue
		}
		v, msg := literalOf(field{Type: "RECORD", Fields: res.Schema.Fields}, map[string]any{"f": wrapCells(vals)})
		if msg != "" {
			s.notImpl = msg
			return nil, nil, 0, &scriptFlow{}
		}
		out = append(out, v)
	}
	return out, rows, len(res.Schema.Fields), nil
}

// page reads the page of query job id's rows at token, from the engine.
func (s *scriptRun) page(id, token, q string) ([]byte, *scriptFlow) {
	if id == "" {
		return nil, &scriptFlow{failed: true, status: http.StatusInternalServerError, msg: "cloudburrow: a page of rows with no job", text: q}
	}
	u := *s.r.URL
	p := s.f.base + "/queries/" + url.PathEscape(id)
	u.Path, u.RawPath = p, ""
	if unescaped, err := url.PathUnescape(p); err == nil {
		u.Path, u.RawPath = unescaped, p
	}
	u.RawQuery = url.Values{"pageToken": {token}}.Encode()
	req, err := http.NewRequestWithContext(s.r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &scriptFlow{failed: true, status: http.StatusInternalServerError, msg: err.Error(), text: q}
	}
	req.Host, req.RemoteAddr = s.r.Host, s.r.RemoteAddr
	req.Header = s.r.Header.Clone()
	req.Header.Del("Content-Type")
	req.Header.Del("Content-Length")
	rec := newRecorder()
	s.f.next.ServeHTTP(rec, req)
	if rec.status != http.StatusOK && rec.status != 0 {
		msg, _ := queryFailure(rec, nil)
		return nil, &scriptFlow{failed: true, status: rec.status, body: rec.body.Bytes(), msg: msg, text: q}
	}
	return rec.body.Bytes(), nil
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
	if flow := s.readyRowCount(toks, nil, nil); flow != nil { // #1037
		return false, flow
	}
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
		if flow := s.assign(n, v, s.scopes[len(s.scopes)-1], s.text(toks)); flow != nil {
			return flow
		}
	}
	return s.assigned()
}

// assigned runs what is held back once a DECLARE or SET has run: the
// script's last statement is then the DECLARE or SET, which has no rows.
func (s *scriptRun) assigned() *scriptFlow {
	if flow := s.flush(); flow != nil {
		return flow
	}
	if s.plain {
		return nil
	}
	s.last, s.lastQuery = nil, ""
	return nil
}

// set carries out SET x = expr and SET (a, b) = (expr, expr) or a STRUCT.
func (s *scriptRun) set(toks []token) *scriptFlow {
	body := toks[1:]
	if len(body) > 0 && body[0].punct("(") {
		return s.setTuple(toks)
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
	if flow := s.assign(name, v, nil, s.text(toks)); flow != nil {
		return flow
	}
	return s.assigned()
}

// setTuple carries out SET (a, b) = expr, where expr is a STRUCT, such as
// (1, 'x') or a (SELECT AS STRUCT ...): each variable is set to its field.
func (s *scriptRun) setTuple(toks []token) *scriptFlow {
	body := toks[1:]
	closeAt := skipTo(body, 1, ")")
	if closeAt >= len(body) || closeAt+2 >= len(body) || !body[closeAt+1].punct("=") {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Syntax error in SET", text: s.text(toks)}
	}
	var names []string
	for _, t := range body[1:closeAt] {
		if !t.punct(",") {
			names = append(names, t.text)
		}
	}
	text := s.text(body[closeAt+2:])
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	return s.setColumns(names, "SELECT * FROM UNNEST([("+text+")])", s.text(toks), false)
}

// setColumns sets each of names to a column of the one row q gives, cast
// to the variable's type; NULL with no row, an error with more than one.
//
// into: the query is EXECUTE IMMEDIATE ... INTO's, a statement of the
// script (a STATEMENT child job), whose columns must be as many as the
// variables.
func (s *scriptRun) setColumns(names []string, q, stmt string, into bool) *scriptFlow {
	olds := make([]scriptVar, len(names))
	for i, n := range names {
		v, ok := s.lookupVar(n)
		if !ok {
			return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Undeclared variable: " + n, text: stmt}
		}
		olds[i] = v
	}
	s.intoQuery = into
	_, rows, cols, flow := s.queryColumns(q)
	s.intoQuery = false
	if flow != nil {
		return flow
	}
	if into {
		// BigQuery: "an error is thrown if you attempt to return more
		// than one row"; the columns are assigned "by position", which
		// needs one per variable (the wording of both errors UNVERIFIED).
		if len(rows) > 1 {
			return &scriptFlow{failed: true, status: http.StatusBadRequest,
				msg: "EXECUTE IMMEDIATE ... INTO: the query returned more than one row", text: stmt}
		}
		if cols != len(names) {
			return &scriptFlow{failed: true, status: http.StatusBadRequest,
				msg: fmt.Sprintf("EXECUTE IMMEDIATE ... INTO has %d variables, but the query returns %d columns", len(names), cols), text: stmt}
		}
	}
	if len(rows) > 1 {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "The query to set " + strings.Join(names, ", ") + " returned more than one row", text: stmt}
	}
	if len(rows) == 1 && len(rows[0]) != len(names) {
		return &scriptFlow{failed: true, status: http.StatusBadRequest,
			msg: fmt.Sprintf("SET of %d variables to a value of %d fields", len(names), len(rows[0])), text: stmt}
	}
	for i, n := range names {
		v := scriptVar{olds[i].typ, "CAST(NULL AS " + olds[i].typ + ")"}
		if len(rows) == 1 {
			var flow *scriptFlow
			if v, flow = s.evaluate(rows[0][i].lit, olds[i].typ); flow != nil {
				return flow
			}
		}
		if flow := s.assign(n, v, nil, stmt); flow != nil {
			return flow
		}
	}
	return s.assigned()
}

// executeImmediate carries out EXECUTE IMMEDIATE sql [INTO a, b] [USING
// v [AS name], ...]: sql and each USING value are evaluated in their turn,
// and the statement is run with each query parameter replaced by its
// value's literal (bindImmediate).
func (s *scriptRun) executeImmediate(toks []token) *scriptFlow {
	// The statement as written, for its errors: its parts are read with
	// the variables (s.text) each in its own scope, as INTO names
	// variables a query of the SQL may not.
	stmt := s.sql[toks[0].pos:toks[len(toks)-1].end]
	if len(toks) < 3 || !toks[1].is("IMMEDIATE") {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Syntax error: EXECUTE IMMEDIATE expected", text: stmt}
	}
	rest := toks[2:]
	intoAt, usingAt := -1, -1
	depth := 0
	for i, t := range rest {
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			depth--
		case depth == 0 && t.is("INTO") && intoAt < 0 && usingAt < 0:
			intoAt = i
		case depth == 0 && t.is("USING") && usingAt < 0:
			usingAt = i
		}
	}
	end := len(rest)
	if intoAt >= 0 {
		end = intoAt
	} else if usingAt >= 0 {
		end = usingAt
	}
	if end == 0 {
		return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Syntax error: EXECUTE IMMEDIATE needs its SQL", text: stmt}
	}
	sqlText := s.text(rest[:end])
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	// A string literal is its value; any other expression is evaluated,
	// an EXPRESSION child job.
	inner, ok := literalString(rest[:end])
	if !ok {
		v, flow := s.evaluate(sqlText, "STRING")
		if flow != nil {
			return flow
		}
		if !strings.HasPrefix(v.lit, "'") {
			return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "EXECUTE IMMEDIATE of a NULL statement", text: stmt}
		}
		inner = unquoteRaised(v.lit)
	}
	var items []usingItem
	if usingAt >= 0 {
		u := rest[usingAt+1:]
		for len(u) > 0 {
			e := skipTo(u, 0, ",")
			item := u[:e]
			var it usingItem
			if n := len(item); n >= 3 && item[n-2].is("AS") && (item[n-1].kind == tokWord || item[n-1].kind == tokQuoted) {
				it.name = item[n-1].text
				item = item[:n-2]
			}
			if len(item) == 0 {
				return &scriptFlow{failed: true, status: http.StatusBadRequest, msg: "Syntax error: an empty value in USING", text: stmt}
			}
			text := s.text(item)
			if s.notImpl != "" {
				return &scriptFlow{}
			}
			val, flow := s.evaluate(text, "")
			if flow != nil {
				return flow
			}
			it.expr = val.lit
			items = append(items, it)
			if e >= len(u) {
				break
			}
			u = u[e+1:]
		}
	}
	bound, code, msg := bindImmediate(inner, items)
	if code == http.StatusNotImplemented {
		s.notImpl = "Not implemented here: EXECUTE IMMEDIATE " + msg + " What the statements before it did was kept."
		return &scriptFlow{}
	}
	if code != 0 {
		return &scriptFlow{failed: true, status: code, msg: msg, text: stmt}
	}
	if sess := sessionOf(s.r); sess != nil { // #1459, tempsession.go: the TEMP tables it names
		bound = sess.rename(bound)
	}
	if intoAt >= 0 {
		intoEnd := len(rest)
		if usingAt > intoAt {
			intoEnd = usingAt
		}
		var names []string
		for _, t := range rest[intoAt+1 : intoEnd] {
			if t.kind == tokQuoted && !plainIdentifier(t.text) {
				// The front's run of a script does not read such a
				// variable (expandExecuteImmediate).
				s.notImpl = "Not implemented here: EXECUTE IMMEDIATE ... INTO " + t.text + ", a quoted name that must be " +
					"quoted, which CloudBurrow does not keep as a script variable. What the statements before it did was kept."
				return &scriptFlow{}
			}
			if !t.punct(",") {
				names = append(names, t.text)
			}
		}
		// BigQuery documents INTO of a query's row only: "If this returns
		// a value, the INTO clause must contain values of the same type",
		// assigned "by position". INTO of another statement, or of a
		// number of variables other than the query's columns, is not
		// documented (#1037).
		bt, _ := lex(bound)
		first := 0
		for first < len(bt) && bt[first].punct("(") {
			first++
		}
		if first >= len(bt) || !bt[first].is("SELECT") && !bt[first].is("WITH") {
			s.notImpl = "Not implemented here: EXECUTE IMMEDIATE ... INTO of " + statementText(bound, bt) + ", which is " +
				"not a query (SELECT or WITH); BigQuery documents INTO of a query's row only. What the statements before it did was kept."
			return &scriptFlow{}
		}
		s.frameAt(bound, toks)
		return s.setColumns(names, bound, stmt, true)
	}
	s.frameAt(bound, toks)
	return s.runText(bound)
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
	if t == "RANGE" {
		return rangeLiteral(typ, strings.ToUpper(fl.RangeElementType.Type), s)
	}
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

// rangeLiteral writes a RANGE's REST value, "[lower, upper)" with
// UNBOUNDED for a missing bound, as RANGE(lower, upper) (#1201). A
// TIMESTAMP bound reads as microseconds since the epoch (measured: the
// emulator gives RANGE(TIMESTAMP '2020-01-01 10:00:00.5', NULL) as
// "[1577872800500000, UNBOUNDED)"), a DATE or DATETIME bound as its text.
func rangeLiteral(typ, elem, s string) (scriptVar, string) {
	bad := "Not implemented here: a " + typ + " value CloudBurrow could not read (" + s + "), in a script with control flow. What the statements before it did was kept."
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, ")") {
		return scriptVar{}, bad
	}
	parts := strings.Split(s[1:len(s)-1], ",")
	if len(parts) != 2 {
		return scriptVar{}, bad
	}
	var bounds []string
	for _, b := range parts {
		b = strings.TrimSpace(b)
		switch {
		case strings.EqualFold(b, "UNBOUNDED"):
			bounds = append(bounds, "CAST(NULL AS "+elem+")")
		case elem == "TIMESTAMP":
			n, ok := new(big.Int).SetString(b, 10)
			if !ok {
				return scriptVar{}, bad
			}
			bounds = append(bounds, "TIMESTAMP_MICROS("+n.String()+")")
		default:
			bounds = append(bounds, "CAST("+sqlString(b)+" AS "+elem+")")
		}
	}
	return scriptVar{typ, "RANGE(" + bounds[0] + ", " + bounds[1] + ")"}, ""
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
	case "RANGE":
		// #1201: a RANGE<DATE>, RANGE<DATETIME> or RANGE<TIMESTAMP>.
		if fl.RangeElementType == nil {
			return "", "Not implemented here: a RANGE value with no element type in a script variable, FOR or condition. What the statements before it did was kept."
		}
		switch e := strings.ToUpper(fl.RangeElementType.Type); e {
		case "DATE", "DATETIME", "TIMESTAMP":
			t = "RANGE<" + e + ">"
		default:
			return "", "Not implemented here: a value of type RANGE<" + e + "> in a script variable, FOR or condition. What the statements before it did was kept."
		}
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

// assert carries out ASSERT expression [AS description] (#1253): the
// emulator's ASSERT does nothing. As BigQuery documents it
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/debugging-statements#assert),
// expression is run as the query SELECT expression; TRUE goes on, FALSE or
// NULL fails the script with "Assertion failed: " and the description, or
// the expression's text with none, and a runtime error in expression fails
// it with that error. expression must be a BOOL and description a STRING
// literal. A handler may catch the failure, as it may a RAISE.
func (s *scriptRun) assert(toks []token) *scriptFlow {
	stmt := s.sql[toks[0].pos:toks[len(toks)-1].end]
	body, desc, hasDesc, msg := assertParts(toks)
	if msg != "" {
		return assertFailure(stmt, msg)
	}
	// What is held back runs first, in its turn.
	if flow := s.flush(); flow != nil {
		return flow
	}
	if !s.step() || s.notImpl != "" {
		return &scriptFlow{}
	}
	expr := s.text(body)
	if s.notImpl != "" {
		return &scriptFlow{}
	}
	v, flow := s.evaluate(expr, "")
	if flow != nil {
		return flow
	}
	if v.typ != "BOOL" {
		return assertFailure(stmt, "ASSERT expression must be of type BOOL, but is of type "+v.typ)
	}
	if v.lit != "TRUE" {
		if !hasDesc {
			desc = s.sql[body[0].pos:body[len(body)-1].end]
		}
		return assertFailure(stmt, "Assertion failed: "+desc)
	}
	// It has no result: the answer stays as after a SET.
	return s.assigned()
}

// assertParts reads ASSERT expression [AS description]: expression's
// tokens and the description, or the error refusing the statement.
func assertParts(toks []token) (body []token, desc string, hasDesc bool, msg string) {
	body = toks[1:]
	if n := len(body); n >= 2 && body[n-2].is("AS") {
		lit, ok := stringLiteral(body[n-1])
		if !ok {
			return nil, "", false, "ASSERT description must be a STRING literal"
		}
		desc, hasDesc, body = lit, true, body[:n-2]
	}
	if len(body) == 0 {
		return nil, "", false, "Syntax error: ASSERT has no expression"
	}
	return body, desc, hasDesc, ""
}

// assertFailure is an ASSERT's failure with msg, a 400 invalidQuery.
func assertFailure(stmt, msg string) *scriptFlow {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": http.StatusBadRequest, "message": msg,
		"errors": []map[string]string{{"message": msg, "domain": "global", "reason": "invalidQuery"}}, "status": "INVALID_ARGUMENT"}})
	return &scriptFlow{failed: true, status: http.StatusBadRequest, body: body, msg: msg, text: stmt}
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
