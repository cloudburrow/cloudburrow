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
// as scriptdryrun.go answers a script's.
//
// The same section documents two more stops (#1612): "CALL statements:
// the dry run validates that the called procedure exists and has a
// signature matching the arguments provided. The content of the called
// procedure and all statements after the CALL statement are not
// validated", and "EXECUTE IMMEDIATE statements: the dry run validates the
// query expression, but does not evaluate the dynamic query itself. All
// statements following the EXECUTE IMMEDIATE statement are skipped". So a
// script reaching an ALTER TABLE after a CALL of a procedure the front
// keeps (procedures.go) stops at the CALL: the procedure must exist (404
// notFound), be given as many arguments as it has (400), an OUT or INOUT
// one a variable, and each argument must be of a type the argument's
// coerces to, which the emulator's dry run checks, with the statements
// before the CALL in scope, as IF(FALSE, CAST(NULL AS type), (argument))
// (no common type fails it). One after an EXECUTE IMMEDIATE stops there:
// its SQL expression, before INTO and USING, is checked so, as a STRING.
// The messages of those refusals are CloudBurrow's own, UNVERIFIED: the
// page gives none (docs re-read 2026-10-17,
// https://docs.cloud.google.com/bigquery/docs/multi-statement-queries).
// A CALL of a system procedure (BQ.*) is served as before.
//
// The cases the docs leave out (LOOP, REPEAT, CASE, a BEGIN block, a
// labelled statement, and CREATE TEMP TABLE, whose dry run "isn't
// supported") are served as before.

// dryStop is where dryRunStop found a script's dry run stops.
type dryStop struct {
	prefix string  // the text validated before the stop
	alter  []token // the ALTER TABLE the dry run stops at, validated
	call   []token // the CALL it stops at (#1612)
	exec   []token // the EXECUTE IMMEDIATE it stops at (#1612)
}

// dryRunStop reads sql, a script, up to where its dry run stops: prefix is
// the text validated before the stop, alter the ALTER TABLE the dry run
// stops at (nil when it stops at a control statement or at another DDL
// statement, which prefix then ends with), call or exec the CALL or
// EXECUTE IMMEDIATE it stops at. ok is false when the script is not one
// this answers (above), or nothing after the stop needs it: only a script
// that reaches an ALTER TABLE or a nested ALTER PROJECT is.
func dryRunStop(sql string) (stop dryStop, ok bool) {
	toks, lexed := lex(sql)
	if !lexed {
		return dryStop{}, false
	}
	p := &scriptParser{toks: toks}
	nodes := p.block()
	if p.err != "" || len(nodes) < 2 {
		return dryStop{}, false
	}
	var alter []token
	end := 0 // the end of the last statement validated
	rest := -1
	for _, n := range nodes {
		if n.label != "" {
			return dryStop{}, false
		}
		switch n.kind {
		case nodeIf, nodeWhile, nodeFor:
			rest = end
		case nodeCall:
			if len(n.toks) < 2 || n.toks[1].is("BQ") {
				return dryStop{}, false // a system procedure: as before
			}
			stop.call, rest = n.toks, n.toks[len(n.toks)-1].end
		case nodeStmt, nodeDeclare, nodeSet:
			if len(n.toks) == 0 {
				return dryStop{}, false
			}
			first := n.toks[0]
			switch {
			case first.is("EXECUTE"):
				if len(n.toks) < 3 || !n.toks[1].is("IMMEDIATE") {
					return dryStop{}, false
				}
				stop.exec, rest = n.toks, n.toks[len(n.toks)-1].end
			case isAlterTable(n.toks):
				alter, rest = n.toks, n.toks[len(n.toks)-1].end
			case first.is("CREATE") || first.is("DROP") || first.is("ALTER") || first.is("UNDROP") ||
				first.is("TRUNCATE"):
				if _, temp := createsTemp(n.toks); temp {
					return dryStop{}, false
				}
				end = n.toks[len(n.toks)-1].end
				rest = end
			default:
				end = n.toks[len(n.toks)-1].end
			}
		default:
			return dryStop{}, false
		}
		if rest >= 0 {
			break
		}
	}
	if rest < 0 {
		return dryStop{}, false
	}
	if alter == nil && !altersAfter(sql[rest:]) {
		return dryStop{}, false
	}
	stop.prefix, stop.alter = strings.TrimSpace(sql[:end]), alter
	return stop, true
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
	stop, ok := dryRunStop(q.Query)
	if !ok {
		return false
	}
	prefix, alter := stop.prefix, stop.alter
	client := jobText{query: q.Query}
	orig, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
		return true
	}
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
	if code, reason, msg := f.dryCallOrExecute(r, q, insert, orig, stop); code != 0 { // #1612, scriptdrycall.go
		writeError(w, code, reason, msg)
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
