package bigqueryfront

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Where the dry run of a script stops (#1456).
//
// BigQuery documents how far the dry run of a multi-statement query
// validates it (https://docs.cloud.google.com/bigquery/docs/multi-statement-queries,
// "Dry-run a multi-statement query", docs re-read 2026-10-17): for "DDL
// statements: the dry run validates the first DDL statement and then
// stops. All subsequent statements are skipped", and for "FOR, IF and
// WHILE control statements: the dry run stops immediately. Condition
// expressions, bodies of the control statement, and all subsequent
// statements are not validated."
//
// So a dry run of a script with an ALTER TABLE, or with an ALTER PROJECT
// or ALTER ORGANIZATION inside an IF, FOR or WHILE, which checkDDL and
// alterProjectOptions refused 501 before this, is answered by validating
// the statements before the stop as the dry run of a script of them alone
// (the emulator's, scriptdryrun.go), and, when the stop is an ALTER TABLE,
// that statement as a lone one's dry run checks it (altertabledry.go).
// What follows the stop is not read. The job's statementType is SCRIPT,
// as scriptdryrun.go answers a script's. The cases the docs leave out
// (LOOP, REPEAT, CASE, a BEGIN block, CALL, EXECUTE IMMEDIATE, a labelled
// statement, and CREATE TEMP TABLE, whose dry run "isn't supported") are
// served as before.

// dryRunStop reads sql, a script, up to where its dry run stops: prefix is
// the text validated before the stop, alter the ALTER TABLE the dry run
// stops at (nil when it stops at a control statement or at another DDL
// statement, which prefix then ends with). ok is false when the script is
// not one this answers (above), or nothing after the stop needs it: only
// a script that reaches an ALTER TABLE or a nested ALTER PROJECT is.
func dryRunStop(sql string) (prefix string, alter []token, ok bool) {
	toks, lexed := lex(sql)
	if !lexed {
		return "", nil, false
	}
	p := &scriptParser{toks: toks}
	nodes := p.block()
	if p.err != "" || len(nodes) < 2 {
		return "", nil, false
	}
	end := 0 // the end of the last statement validated
	rest := -1
	for _, n := range nodes {
		if n.label != "" {
			return "", nil, false
		}
		switch n.kind {
		case nodeIf, nodeWhile, nodeFor:
			rest = end
		case nodeStmt, nodeDeclare, nodeSet:
			if len(n.toks) == 0 {
				return "", nil, false
			}
			first := n.toks[0]
			switch {
			case first.is("EXECUTE"):
				return "", nil, false
			case isAlterTable(n.toks):
				alter, rest = n.toks, n.toks[len(n.toks)-1].end
			case first.is("CREATE") || first.is("DROP") || first.is("ALTER") || first.is("UNDROP") ||
				first.is("TRUNCATE"):
				if _, temp := createsTemp(n.toks); temp {
					return "", nil, false
				}
				end = n.toks[len(n.toks)-1].end
				rest = end
			default:
				end = n.toks[len(n.toks)-1].end
			}
		default:
			return "", nil, false
		}
		if rest >= 0 {
			break
		}
	}
	if rest < 0 {
		return "", nil, false
	}
	if alter == nil && !altersAfter(sql[rest:]) {
		return "", nil, false
	}
	return strings.TrimSpace(sql[:end]), alter, true
}

// altersAfter reports whether text has an ALTER TABLE, ALTER PROJECT or
// ALTER ORGANIZATION anywhere, at any depth.
func altersAfter(text string) bool {
	toks, ok := lex(text)
	if !ok {
		return false
	}
	for i := 0; i+1 < len(toks); i++ {
		if toks[i].is("ALTER") && (toks[i+1].is("TABLE") || toks[i+1].is("PROJECT") || toks[i+1].is("ORGANIZATION")) {
			return true
		}
	}
	return false
}

// dryRunUntilStop answers q, a dry run, when dryRunStop reads it, and
// reports whether it did.
func (f front) dryRunUntilStop(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	prefix, alter, ok := dryRunStop(q.Query)
	if !ok {
		return false
	}
	client := jobText{query: q.Query}
	text := prefix
	if text == "" {
		text = "SELECT 1" // nothing before the ALTER TABLE: a statement that reads nothing
	}
	if !setQueryText(r, insert, text) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	q2 := q
	q2.Query = text
	rec := newRecorder()
	f.runQuery(rec, r, q2, insert)
	if rec.status != http.StatusOK && rec.status != 0 {
		rec.copyTo(w)
		return true
	}
	var resp map[string]any
	if json.Unmarshal(rec.body.Bytes(), &resp) != nil {
		rec.copyTo(w)
		return true
	}
	if msg, failed := queryFailure(rec, resp); failed {
		writeError(w, http.StatusBadRequest, "invalidQuery", msg)
		return true
	}
	if alter != nil {
		s := &scriptRun{f: f, r: r, q: q, sql: q.Query, scopes: []map[string]scriptVar{{}}, start: scriptNow(), plain: true, dry: true}
		flow := s.alterTable(alter)
		if s.notImpl != "" {
			writeError(w, http.StatusNotImplemented, "notImplemented", s.notImpl)
			return true
		}
		if flow != nil && flow.failed {
			if len(flow.body) == 0 {
				writeError(w, flow.status, "invalidQuery", flow.msg)
				return true
			}
			writeRaw(w, flow.status, flow.body)
			return true
		}
	}
	b := rec.body.Bytes()
	if prefix == "" {
		b = dryDDLAnswer(b, "SCRIPT")
	} else {
		// A dry run gives no result: the statistics of a job (jobs.insert),
		// or no rows or schema (jobs.query).
		if st, ok := resp["statistics"].(map[string]any); ok {
			qs, _ := st["query"].(map[string]any)
			if qs == nil {
				qs = map[string]any{}
				st["query"] = qs
			}
			qs["statementType"] = "SCRIPT"
			delete(qs, "schema")
		} else {
			for _, k := range []string{"schema", "rows", "totalRows", "pageToken"} {
				delete(resp, k)
			}
		}
		if out, err := json.Marshal(resp); err == nil {
			b = out
		}
	}
	rec.body.Reset()
	rec.body.Write(b)
	f.clientText(w, rec, client)
	return true
}
