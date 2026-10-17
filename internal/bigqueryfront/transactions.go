package bigqueryfront

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Multi-statement transactions in a script the front carries out (#1201,
// https://cloud.google.com/bigquery/docs/transactions).
//
// BigQuery: a transaction runs from BEGIN TRANSACTION to COMMIT
// TRANSACTION or ROLLBACK TRANSACTION, or to the end of the query, which
// rolls it back; its statements see its own writes; transactions "cannot be
// nested"; an error inside one goes to the query's exception handler, where
// the transaction is still open, or, with none, fails the query and rolls
// the transaction back. Its statements may be queries, DML, and the CREATE
// TEMP TABLE and DROP TABLE of a TEMP table; variables and control flow
// work inside it as anywhere in the script.
//
// The emulator has no sessions, and runs each request in one SQLite
// transaction, which jobs.query rolls back whole when the request fails
// (measured, #935). So the front keeps the transaction itself, from the
// BEGIN TRANSACTION the script runs to the COMMIT or ROLLBACK it runs, in
// whatever block either is (txnState). The interpreter (scriptflow.go)
// carries out DECLARE, SET, blocks, CALL and EXECUTE IMMEDIATE inside it as
// it does elsewhere; what changes is how a statement is sent (sendInTxn):
//
//   - A statement that changes data (INSERT, UPDATE, DELETE, MERGE,
//     TRUNCATE TABLE) or makes a TEMP table is first run as a probe: a
//     request of the transaction's earlier such statements, then the
//     statement, then a statement that fails with probeSentinel, so the
//     emulator rolls the whole request back. A probe that fails with another
//     error is the statement's failure; else the statement is kept, to be
//     applied at COMMIT.
//   - A query that reads a table is a probe too, as a scalar subquery that
//     reads every column of every row before the probe fails, so that its
//     errors are seen; its rows are not the script's answer. A query that
//     reads no table is sent as it is.
//   - DROP TABLE of a TEMP table the transaction made is kept by the front
//     alone: a TEMP table lasts only for its request, and the emulator
//     cannot drop one inside a script (replacesTemp). A later statement of
//     the script that names it is 501, as the probes would still see it.
//   - At COMMIT, the kept statements are sent as one request, BEGIN
//     TRANSACTION; ...; COMMIT TRANSACTION, which the emulator runs in one
//     SQLite transaction: all of it is kept, or none. At ROLLBACK, or at the
//     end of the script, nothing is sent.
//
// The DML is applied as one atomic request at COMMIT, which is a serial
// order of the transaction among other requests, as BigQuery's snapshot
// isolation allows.
//
// 501:
//   - an expression the interpreter evaluates (a DECLARE's DEFAULT, a SET,
//     a condition, a FOR's query, EXECUTE IMMEDIATE ... INTO) that reads a
//     table, after the transaction changed data or made a TEMP table: its
//     value would have to come back from a request that is rolled back;
//   - a TEMP table the transaction made and did not drop, once a statement
//     after the transaction names it; making a TEMP table of a name the
//     script made before;
//   - DDL other than the above, a TEMP function, or CREATE or DROP
//     PROCEDURE inside a transaction, and a transaction inside a procedure;
//   - COMMIT or ROLLBACK with no transaction open, which BigQuery's
//     documentation does not describe.

// probeSentinel is the error a probe ends with, to be rolled back.
const probeSentinel = "cloudburrow: transaction probe, rolled back"

// txnState is the transaction a script has open.
type txnState struct {
	// kept: the statements to apply at COMMIT, in order; dml whether one
	// changes data.
	kept []string
	dml  bool
	// temps: the TEMP tables it made, lower-case, true until dropped.
	temps map[string]bool
}

type txnKey struct{}

// txnRequest reports whether r is a transaction's probe or COMMIT, whose
// failure the emulator rolls back whole, as BigQuery rolls the transaction
// back: serveQuery answers it with the emulator's error.
func txnRequest(r *http.Request) bool {
	v, _ := r.Context().Value(txnKey{}).(bool)
	return v
}

func isBeginTransaction(t []token) bool {
	return len(t) > 1 && t[0].is("BEGIN") && (t[1].is("TRANSACTION") || t[1].is("TRAN"))
}

func isTxnNode(n scriptNode, word string) bool {
	if n.kind != nodeStmt || len(n.toks) == 0 {
		return false
	}
	if word == "BEGIN" {
		return isBeginTransaction(n.toks)
	}
	return n.toks[0].is(word)
}

// txnStatementKind is "query" or "dml" for a statement a transaction may
// hold, else "".
func txnStatementKind(t []token) string {
	if len(t) == 0 {
		return ""
	}
	for _, x := range t {
		if x.is("TEMP") || x.is("TEMPORARY") {
			return ""
		}
	}
	switch {
	case t[0].is("SELECT") || t[0].is("WITH") || t[0].punct("("):
		return "query"
	case t[0].is("INSERT") || t[0].is("UPDATE") || t[0].is("DELETE") || t[0].is("MERGE") || t[0].is("TRUNCATE"):
		return "dml"
	}
	return ""
}

// dropsTable returns the name a DROP TABLE [IF EXISTS] name statement
// drops, if it is one name (tempName), and whether it has IF EXISTS.
func dropsTable(t []token) (name string, ifExists, ok bool) {
	if len(t) < 3 || !t[0].is("DROP") || !t[1].is("TABLE") {
		return "", false, false
	}
	i := 2
	if i+1 < len(t) && t[i].is("IF") && t[i+1].is("EXISTS") {
		i, ifExists = i+2, true
	}
	parts, n := path(t, i)
	if n != len(t) && !(n == len(t)-1 && t[n].punct(";")) {
		return "", false, false
	}
	name, ok = tempName(parts)
	return strings.ToLower(name), ifExists, ok
}

// unsupportedInTxn returns why a statement cannot be in a transaction
// here, or "" (above).
func unsupportedInTxn(n scriptNode) string {
	switch n.kind {
	case nodeCreateProc, nodeDropProc:
		return "CREATE PROCEDURE or DROP PROCEDURE inside a transaction."
	case nodeStmt:
	default:
		return ""
	}
	t := n.toks
	if len(t) == 0 {
		return ""
	}
	if _, ok := createsTemp(t); ok {
		return ""
	}
	if _, _, ok := dropsTable(t); ok {
		return ""
	}
	if schemaDDLInTxn(t) {
		return "" // fails in its turn (sendInTxn), #1562
	}
	switch {
	case t[0].is("CREATE"):
		for _, x := range t[1:min(len(t), 6)] {
			if x.is("FUNCTION") {
				return "a CREATE FUNCTION inside a transaction. A TEMP function would last only for the request that made it, and the front sends the transaction's statements as requests of their own."
			}
		}
		return "DDL other than CREATE TEMP TABLE, or DROP TABLE of a TEMP table, inside a transaction."
	case t[0].is("ALTER") || t[0].is("DROP") || t[0].is("GRANT") || t[0].is("REVOKE") || t[0].is("EXPORT") || t[0].is("LOAD"):
		return "DDL other than CREATE TEMP TABLE, or DROP TABLE of a TEMP table, inside a transaction."
	}
	return ""
}

// unsupportedTransactions returns why the transactions in nodes, a list of
// statements, cannot be carried out, or "": what follows a BEGIN
// TRANSACTION in its list, up to its COMMIT or ROLLBACK, and the blocks in
// that, is checked (unsupportedInTxn). top is unused: a transaction may end
// in any block.
func unsupportedTransactions(nodes []scriptNode, top bool) string {
	_ = top
	var inside func([]scriptNode) string
	inside = func(ns []scriptNode) string {
		for _, n := range ns {
			if msg := unsupportedInTxn(n); msg != "" {
				return msg
			}
			for _, b := range append([][]scriptNode{n.body, n.els, n.handler}, n.bodies...) {
				if msg := inside(b); msg != "" {
					return msg
				}
			}
		}
		return ""
	}
	for i := 0; i < len(nodes); i++ {
		if !isTxnNode(nodes[i], "BEGIN") {
			continue
		}
		j := i + 1
		for ; j < len(nodes); j++ {
			if isTxnNode(nodes[j], "COMMIT") || isTxnNode(nodes[j], "ROLLBACK") {
				break
			}
		}
		if msg := inside(nodes[i+1 : j]); msg != "" {
			return msg
		}
		i = j
	}
	return ""
}

// hasTransaction reports whether nodes, or a block in them, has a
// transaction statement.
func hasTransaction(nodes []scriptNode) bool {
	for _, n := range nodes {
		if n.kind == nodeStmt && isTransaction(n.toks) {
			return true
		}
		for _, b := range append([][]scriptNode{n.body, n.els, n.handler}, n.bodies...) {
			if hasTransaction(b) {
				return true
			}
		}
	}
	return false
}

// txnFail is a failed flow with a BigQuery error body.
func txnFail(msg, text string, pos int) *scriptFlow {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": http.StatusBadRequest, "message": msg,
		"errors": []map[string]string{{"message": msg, "domain": "global", "reason": "invalidQuery"}}, "status": "INVALID_ARGUMENT"}})
	return &scriptFlow{failed: true, status: http.StatusBadRequest, body: body, msg: msg, text: text, pos: pos, hasPos: true}
}

// beginTransaction carries out BEGIN TRANSACTION.
func (s *scriptRun) beginTransaction(n scriptNode) *scriptFlow {
	if s.txn != nil {
		return txnFail("Transaction cannot be nested: BEGIN TRANSACTION inside a transaction.", s.text(n.toks), n.toks[0].pos)
	}
	if flow := s.flush(); flow != nil {
		return flow
	}
	s.txn = &txnState{temps: map[string]bool{}}
	s.last, s.lastQuery = nil, ""
	return nil
}

// endTransaction carries out a COMMIT or ROLLBACK.
func (s *scriptRun) endTransaction(n scriptNode) *scriptFlow {
	if s.txn == nil {
		s.notImpl = "Not implemented here: " + strings.ToUpper(n.toks[0].text) + " TRANSACTION with no transaction open, which BigQuery's documentation does not describe. What the statements before it did was kept."
		return &scriptFlow{}
	}
	txn := s.txn
	s.txn = nil
	// COMMIT and ROLLBACK have no rows: the script's last statement does
	// not answer with the transaction's.
	s.last, s.lastQuery = nil, ""
	for name, live := range txn.temps {
		if live {
			// BigQuery keeps it for the rest of the script; here it
			// lasted only for the transaction's requests.
			if s.goneTemps == nil {
				s.goneTemps = map[string]bool{}
			}
			s.goneTemps[name] = true
		}
	}
	if !isTxnNode(n, "COMMIT") || !txn.dml {
		return nil
	}
	if _, flow := s.sendTxn("BEGIN TRANSACTION;\n" + strings.Join(txn.kept, ";\n") + ";\nCOMMIT TRANSACTION"); flow != nil {
		if flow.failed {
			flow.text, flow.pos, flow.hasPos = s.text(n.toks), n.toks[0].pos, true
		}
		return flow
	}
	s.f.noteTransaction(s.r, s.q, txn.kept) // #1503, changehistorytxn.go
	return nil
}

// namesTemp reports whether toks name one of names, as a word or a quoted
// identifier.
func namesTemp(toks []token, names map[string]bool, want bool) bool {
	for _, t := range toks {
		if t.kind != tokWord && t.kind != tokQuoted {
			continue
		}
		if v, ok := names[strings.ToLower(t.text)]; ok && v == want {
			return true
		}
	}
	return false
}

// readsTable reports whether a statement or expression may read a table:
// a FROM or JOIN that is not followed by a subquery or UNNEST, other than
// EXTRACT's, TRIM's and IS DISTINCT FROM's; a comma after a FROM's
// subquery; or TABLE.
func readsTable(toks []token) bool {
	var calls []string
	fromSub := map[int]bool{} // depths of the parentheses a FROM opened
	sub := func(i int) bool {
		return i+1 < len(toks) && (toks[i+1].punct("(") || toks[i+1].is("UNNEST"))
	}
	for i, t := range toks {
		switch {
		case t.punct("("):
			name := ""
			if i > 0 && toks[i-1].kind == tokWord {
				name = strings.ToUpper(toks[i-1].text)
			}
			if i > 0 && (toks[i-1].is("FROM") || toks[i-1].is("JOIN")) {
				fromSub[len(calls)] = true
			}
			calls = append(calls, name)
		case t.punct(")"):
			if len(calls) > 0 {
				calls = calls[:len(calls)-1]
			}
			if fromSub[len(calls)] {
				delete(fromSub, len(calls))
				if i+1 < len(toks) && toks[i+1].punct(",") {
					return true
				}
			}
		case t.is("TABLE"):
			return true
		case t.is("JOIN"):
			if !sub(i) {
				return true
			}
		case t.is("FROM"):
			if i > 0 && toks[i-1].is("DISTINCT") {
				continue
			}
			if len(calls) > 0 && (calls[len(calls)-1] == "EXTRACT" || calls[len(calls)-1] == "TRIM") {
				continue
			}
			if !sub(i) {
				return true
			}
		}
	}
	return false
}

// goneTemp refuses text that names a TEMP table a transaction made and
// left (endTransaction).
func (s *scriptRun) goneTemp(toks []token) bool {
	if len(s.goneTemps) == 0 || !namesTemp(toks, s.goneTemps, true) {
		return false
	}
	if s.notImpl == "" {
		s.notImpl = "Not implemented here: a statement that names a TEMP table made inside a transaction, after the transaction. BigQuery keeps it for the rest of the script, but CloudBurrow keeps it only for the transaction's own requests. What the statements before it did was kept."
	}
	return true
}

// readInTxn checks q, a query whose value the interpreter needs, inside a
// transaction: it is sent as it is unless it reads what the transaction
// changed, which is 501 (above). ok is false when refused.
func (s *scriptRun) readInTxn(q string) bool {
	toks, _ := lex(q)
	if s.goneTemp(toks) {
		return false
	}
	if s.txn == nil || len(s.txn.kept) == 0 || !readsTable(toks) && !namesTemp(toks, s.txn.temps, true) && !namesTemp(toks, s.txn.temps, false) {
		return true
	}
	if s.notImpl == "" {
		s.notImpl = "Not implemented here: an expression (a DECLARE's DEFAULT, a SET, a condition, a FOR's query or EXECUTE IMMEDIATE ... INTO) that reads a table inside a transaction after it changed data. CloudBurrow sees the transaction's writes only in requests it rolls back, which return no rows. What the statements before the transaction did was kept."
	}
	return false
}

// sendInTxn sends text, a statement of the open transaction (above). rec
// is nil for a statement run as a probe, which has no rows.
func (s *scriptRun) sendInTxn(text string) (*recorder, *scriptFlow) {
	txn := s.txn
	toks, _ := lex(text)
	for len(toks) > 0 && toks[len(toks)-1].punct(";") {
		toks = toks[:len(toks)-1]
	}
	if len(toks) == 0 {
		return nil, nil
	}
	refuse := func(msg string) (*recorder, *scriptFlow) {
		if s.notImpl == "" {
			s.notImpl = "Not implemented here: " + msg + " What the statements before the transaction did was kept."
		}
		return nil, &scriptFlow{}
	}
	if s.goneTemp(toks) {
		return nil, &scriptFlow{}
	}
	if name, ifExists, ok := dropsTable(toks); ok {
		live, made := txn.temps[name]
		switch {
		case made && live:
			txn.temps[name] = false
			return nil, nil
		case made && ifExists:
			return nil, nil
		}
		return refuse("DROP TABLE of a table other than a TEMP table the transaction made, inside a transaction.")
	}
	if namesTemp(toks, txn.temps, false) {
		return refuse("a statement that names a TEMP table the transaction dropped. CloudBurrow drops it only at the transaction's end.")
	}
	if schemaDDLInTxn(toks) {
		// #1562: BigQuery's transactions take only queries, DML and DDL of
		// TEMP entities; this statement fails the script there, as an
		// error inside the transaction (an exception handler sees it, and
		// with none the transaction is rolled back). UNVERIFIED: the
		// message, which BigQuery's documentation does not give.
		msg := strings.ToUpper(toks[0].text) + " SCHEMA is not supported inside a transaction: a transaction takes " +
			"queries, DML, and CREATE or DROP of TEMP tables and functions."
		flow := txnFail(msg, text, 0)
		flow.hasPos = false
		return nil, flow
	}
	name, temp := createsTemp(toks)
	kind := txnStatementKind(toks)
	switch {
	case temp:
		name = strings.ToLower(name)
		if _, ok := txn.temps[name]; ok || s.goneTemps[name] {
			return refuse("a CREATE TEMP TABLE of a name the script made a TEMP table of before.")
		}
		kind = "temp"
	case isTransaction(toks):
		return refuse("a transaction statement run by EXECUTE IMMEDIATE.")
	case kind == "":
		return refuse("a statement inside a transaction other than a query, DML, CREATE TEMP TABLE or DROP TABLE of a TEMP table.")
	case kind == "query" && !readsTable(toks) && !namesTemp(toks, txn.temps, true):
		// It reads nothing the transaction changed.
		return s.send(text)
	}
	probe := "BEGIN TRANSACTION;\n"
	for _, d := range txn.kept {
		probe += d + ";\n"
	}
	sentinel := "SELECT ERROR(" + sqlString(probeSentinel) + ")"
	if kind == "query" {
		probe += "SELECT IF((SELECT COUNT(TO_JSON_STRING(cloudburrow_row)) FROM (\n" + text + "\n) AS cloudburrow_row) >= 0, ERROR(" +
			sqlString(probeSentinel) + "), ERROR(" + sqlString(probeSentinel) + "))"
	} else {
		probe += text + ";\n" + sentinel
	}
	_, flow := s.sendTxn(probe)
	if flow == nil {
		// A probe never succeeds: it ends with an error.
		s.notImpl = "Not implemented here: a probe of a statement inside a transaction was not rolled back. Its writes may have been kept."
		return nil, &scriptFlow{}
	}
	if !flow.failed || !strings.Contains(flow.msg, probeSentinel) {
		// The statement failed: the transaction stays open, without it.
		if flow.failed {
			flow.text = text
		}
		return nil, flow
	}
	switch kind {
	case "dml":
		txn.kept, txn.dml = append(txn.kept, text), true
	case "temp":
		txn.kept, txn.temps[name] = append(txn.kept, text), true
	}
	return nil, nil
}

// sendTxn sends text as a transaction's probe or COMMIT (send).
func (s *scriptRun) sendTxn(text string) (*recorder, *scriptFlow) {
	prev := s.r
	s.r = s.r.WithContext(context.WithValue(s.r.Context(), txnKey{}, true))
	defer func() { s.r = prev }()
	return s.send(text)
}

// schemaDDLInTxn reports whether t is an UNDROP SCHEMA or an ALTER SCHEMA,
// which a transaction does not take (#1562,
// https://cloud.google.com/bigquery/docs/transactions: its statements are
// queries, DML, and DDL of temporary entities).
func schemaDDLInTxn(t []token) bool {
	return len(t) > 2 && (t[0].is("UNDROP") || t[0].is("ALTER")) && t[1].is("SCHEMA")
}
