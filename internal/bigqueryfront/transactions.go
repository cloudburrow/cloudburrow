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
// nested"; an error inside one goes to the query's exception handler, or,
// with none, fails the query and rolls the transaction back.
//
// The emulator has no sessions, and runs each request in one SQLite
// transaction, which jobs.query rolls back whole when the request fails
// (measured, #935). It reads only the rows of a script's last statement: a
// SELECT 1/0 before the last statement did not fail the script (measured).
// So the front carries out the subset it can make exact, a transaction whose
// statements are queries and DML (SELECT, WITH, INSERT, UPDATE, DELETE,
// MERGE, TRUNCATE TABLE), in the same list of statements as its BEGIN and
// its COMMIT or ROLLBACK:
//
//   - Each statement is first run as a probe, a request of the
//     transaction's earlier DML, then the statement, then a statement that
//     fails with probeSentinel, so the emulator rolls the whole request back.
//     A query is run as a scalar subquery that reads every column of every
//     row before the probe fails, so its errors are seen. A probe that fails
//     with another error is the statement's failure: the transaction fails
//     there, with nothing of it kept.
//   - At COMMIT, every DML statement is sent as one request, BEGIN
//     TRANSACTION; ...; COMMIT TRANSACTION, which the emulator runs in one
//     SQLite transaction: all of it is kept, or none. At ROLLBACK, or at the
//     end of the script, nothing is sent.
//
// The transaction's queries' rows are not used: a COMMIT follows them, and
// the script's answer is its last statement's. The DML is applied as one
// atomic request at COMMIT, which is a serial order of the transaction
// among other requests, as BigQuery's snapshot isolation allows.
//
// After a statement of a transaction failed, BigQuery's transaction is
// still open in the exception handler. Here nothing of it was kept, so in
// that state ROLLBACK TRANSACTION ends it, a query that reads no table runs
// (SELECT @@error.message), and COMMIT, or any statement that reads or
// writes a table, is 501: BigQuery would see the transaction's writes.
//
// 501 before anything runs: a DECLARE, SET, control-flow block, CALL,
// EXECUTE IMMEDIATE, TEMP table or function, or DDL inside a transaction; a
// COMMIT with no BEGIN TRANSACTION before it in its list; a transaction
// with no COMMIT or ROLLBACK in its list other than at the script's top
// level. A ROLLBACK with no transaction open is 501 when it runs: BigQuery's
// documentation does not say what it does.

// probeSentinel is the error a probe ends with, to be rolled back.
const probeSentinel = "cloudburrow: transaction probe, rolled back"

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

// unsupportedTransactions returns why the transactions in nodes, a list of
// statements (top at the script's top level), cannot be carried out, or "".
func unsupportedTransactions(nodes []scriptNode, top bool) string {
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		if isTxnNode(n, "COMMIT") {
			return "a COMMIT TRANSACTION with no BEGIN TRANSACTION before it in its block. CloudBurrow carries out a transaction only when its BEGIN and its COMMIT or ROLLBACK are statements of the same block."
		}
		if !isTxnNode(n, "BEGIN") {
			continue
		}
		j := i + 1
		for ; j < len(nodes); j++ {
			m := nodes[j]
			if isTxnNode(m, "COMMIT") || isTxnNode(m, "ROLLBACK") {
				break
			}
			if isTxnNode(m, "BEGIN") {
				continue // nested: an error when it runs
			}
			if m.kind != nodeStmt || txnStatementKind(m.toks) == "" {
				return "a statement inside a transaction other than a query or DML (SELECT, INSERT, UPDATE, DELETE, MERGE, TRUNCATE TABLE) on a table that is not TEMP. CloudBurrow carries out such a transaction itself, as statements whose writes it applies at COMMIT, and cannot run a DECLARE, SET, block, CALL, EXECUTE IMMEDIATE or TEMP table or function inside one."
			}
		}
		if j == len(nodes) && !top {
			return "a transaction with no COMMIT TRANSACTION or ROLLBACK TRANSACTION in its block. CloudBurrow carries out a transaction only when its BEGIN and its COMMIT or ROLLBACK are statements of the same block."
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

// transaction carries out the transaction whose BEGIN TRANSACTION is
// nodes[i] (above). It returns the index of its last node, the COMMIT or
// ROLLBACK, and a flow that ends the list early.
func (s *scriptRun) transaction(nodes []scriptNode, i int) (int, *scriptFlow) {
	begin := nodes[i]
	if s.txnOpen {
		return i, txnFail("Transaction cannot be nested: BEGIN TRANSACTION inside a transaction.", s.text(begin.toks), begin.toks[0].pos)
	}
	if flow := s.flush(); flow != nil {
		return i, flow
	}
	var dml []string
	j := i + 1
	commit := false
	for ; j < len(nodes); j++ {
		n := nodes[j]
		if isTxnNode(n, "COMMIT") || isTxnNode(n, "ROLLBACK") {
			commit = isTxnNode(n, "COMMIT")
			break
		}
		if !s.step() {
			return j, &scriptFlow{}
		}
		text := s.text(n.toks)
		if s.notImpl != "" {
			return j, &scriptFlow{}
		}
		pos := n.toks[0].pos
		if isTxnNode(n, "BEGIN") {
			s.txnOpen = true
			return j, txnFail("Transaction cannot be nested: BEGIN TRANSACTION inside a transaction.", text, pos)
		}
		kind := txnStatementKind(n.toks)
		probe := "BEGIN TRANSACTION;\n"
		for _, d := range dml {
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
			return j, &scriptFlow{}
		}
		if !flow.failed {
			return j, flow
		}
		if !strings.Contains(flow.msg, probeSentinel) {
			// The statement failed: nothing of the transaction was kept,
			// and the transaction stays open, for a handler to end.
			s.txnOpen = true
			flow.text, flow.pos, flow.hasPos = text, pos, true
			return j, flow
		}
		if kind == "dml" {
			dml = append(dml, text)
		}
	}
	if commit && len(dml) > 0 {
		if _, flow := s.sendTxn("BEGIN TRANSACTION;\n" + strings.Join(dml, ";\n") + ";\nCOMMIT TRANSACTION"); flow != nil {
			if flow.failed {
				end := nodes[j]
				flow.text, flow.pos, flow.hasPos = s.text(end.toks), end.toks[0].pos, true
			}
			return j, flow
		}
	}
	// COMMIT and ROLLBACK have no rows: the script's last statement does
	// not answer with the transaction's.
	s.last, s.lastQuery = nil, ""
	return j, nil
}

// endTransaction carries out a COMMIT or ROLLBACK that ends no
// transaction() started in its turn: the transaction a failed statement
// left open (above), or none.
func (s *scriptRun) endTransaction(n scriptNode) *scriptFlow {
	switch {
	case s.txnOpen && isTxnNode(n, "ROLLBACK"):
		if flow := s.flush(); flow != nil {
			return flow
		}
		s.txnOpen = false
		s.last, s.lastQuery = nil, ""
		return nil
	case s.txnOpen:
		s.notImpl = "Not implemented here: COMMIT TRANSACTION after a statement of the transaction failed. BigQuery would commit what the transaction's statements before the failure did, but CloudBurrow ran them only as probes it rolled back. What the statements before the transaction did was kept."
	default:
		s.notImpl = "Not implemented here: " + strings.ToUpper(n.toks[0].text) + " TRANSACTION with no transaction open, which BigQuery's documentation does not describe. What the statements before it did was kept."
	}
	return &scriptFlow{}
}

// readsInFailedTxn reports whether text, sent while a failed transaction
// is open, reads or writes a table, and refuses it (above).
func (s *scriptRun) readsInFailedTxn(text string) bool {
	if !s.txnOpen {
		return false
	}
	toks, _ := lex(text)
	bad := len(toks) > 0 && txnStatementKind(toks) == "dml"
	for _, t := range toks {
		if t.is("FROM") || t.is("TABLE") {
			bad = true
		}
	}
	if bad && s.notImpl == "" {
		s.notImpl = "Not implemented here: a statement that reads or writes a table after a statement of a transaction failed, before the transaction ends. BigQuery's transaction is still open there, and its statements would see its writes, but CloudBurrow ran them only as probes it rolled back. What the statements before the transaction did was kept."
	}
	return bad
}

// sendTxn sends text as a transaction's probe or COMMIT (send).
func (s *scriptRun) sendTxn(text string) (*recorder, *scriptFlow) {
	prev := s.r
	s.r = s.r.WithContext(context.WithValue(s.r.Context(), txnKey{}, true))
	defer func() { s.r = prev }()
	return s.send(text)
}
