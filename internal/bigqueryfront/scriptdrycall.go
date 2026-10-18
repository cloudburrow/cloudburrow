package bigqueryfront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// A script's dry run that stops at a CALL or an EXECUTE IMMEDIATE (#1612,
// scriptdrystop.go has the docs).

// dryCallOrExecute checks stop's CALL or EXECUTE IMMEDIATE, if it has one,
// as the dry run validates it, the statements before it, stop.prefix, in
// scope; code 0 when it passes (or there is none).
func (f front) dryCallOrExecute(r *http.Request, q queryOptions, insert bool, orig []byte, stop dryStop) (code int, reason, msg string) {
	switch {
	case stop.call != nil:
		return f.dryCall(r, q, insert, orig, stop.prefix, stop.call)
	case stop.exec != nil:
		return f.dryExecute(r, q, insert, orig, stop.prefix, stop.exec)
	}
	return 0, "", ""
}

// dryCall checks CALL name(args), toks, as the dry run does: the procedure
// exists, with as many arguments as are given, each given one of a type
// its argument takes (an OUT or INOUT one a variable).
func (f front) dryCall(r *http.Request, q queryOptions, insert bool, orig []byte, prefix string, toks []token) (int, string, string) {
	sql := q.Query
	stmt := sql[toks[0].pos:toks[len(toks)-1].end]
	parts, i := path(toks, 1)
	if len(parts) == 0 || i >= len(toks) || !toks[i].punct("(") || closing(toks, i) != len(toks)-1 {
		return http.StatusBadRequest, "invalidQuery", "Syntax error in CALL"
	}
	s := &scriptRun{f: f, r: r, q: q, sql: sql, dry: true}
	p, flow := s.procPath(parts, stmt)
	if flow != nil {
		return flow.status, "invalidQuery", flow.msg
	}
	project := s.project()
	proc, ok := f.functions.proc(project, p)
	if !ok {
		return http.StatusNotFound, "notFound", "Not found: Procedure " + project + ":" + p[0] + "." + p[1]
	}
	var given [][]token
	if i+1 < len(toks)-1 {
		given = splitTop(toks[i+1 : len(toks)-1])
	}
	if len(given) != len(proc.Args) {
		return http.StatusBadRequest, "invalidQuery", fmt.Sprintf("Procedure %s.%s expects %d arguments, but %d were given.",
			p[0], p[1], len(proc.Args), len(given))
	}
	for k, a := range proc.Args {
		arg := given[k]
		if a.Mode != "IN" && (len(arg) != 1 || arg[0].kind != tokWord && arg[0].kind != tokQuoted) {
			return http.StatusBadRequest, "invalidQuery", fmt.Sprintf("Argument %s of procedure %s.%s is %s, "+
				"so it must be a variable.", a.Name, p[0], p[1], a.Mode)
		}
		if len(arg) == 0 {
			return http.StatusBadRequest, "invalidQuery", "Syntax error in CALL"
		}
		expr := sql[arg[0].pos:arg[len(arg)-1].end]
		check := "IF(FALSE, CAST(NULL AS " + a.Type + "), (" + expr + "))"
		if code, reason, msg := f.dryCheck(r, q, insert, orig, prefix, check); code != 0 {
			return code, reason, fmt.Sprintf("Argument %s of procedure %s.%s, %s %s, is not given a value of that type: %s",
				a.Name, p[0], p[1], a.Mode, a.Type, msg)
		}
	}
	return 0, "", ""
}

// dryExecute checks EXECUTE IMMEDIATE sql [INTO ...] [USING ...], toks, as
// the dry run does: sql, the query expression, is a STRING expression. The
// dynamic query is not read.
func (f front) dryExecute(r *http.Request, q queryOptions, insert bool, orig []byte, prefix string, toks []token) (int, string, string) {
	end, depth := len(toks), 0
	for j := 2; j < len(toks); j++ {
		switch {
		case toks[j].punct("(") || toks[j].punct("["):
			depth++
		case toks[j].punct(")") || toks[j].punct("]"):
			depth--
		case depth == 0 && (toks[j].is("INTO") || toks[j].is("USING")):
			end = j
		}
		if end < len(toks) {
			break
		}
	}
	if end <= 2 {
		return http.StatusBadRequest, "invalidQuery", "Syntax error: EXECUTE IMMEDIATE needs its SQL"
	}
	expr := q.Query[toks[2].pos:toks[end-1].end]
	if code, reason, msg := f.dryCheck(r, q, insert, orig, prefix, "IF(FALSE, CAST(NULL AS STRING), ("+expr+"))"); code != 0 {
		return code, reason, "The SQL of EXECUTE IMMEDIATE must be a STRING expression: " + msg
	}
	return 0, "", ""
}

// dryCheck runs SELECT expr, after prefix, as a dry run of the request
// whose body was orig, and returns its refusal (code 0 when it passes).
func (f front) dryCheck(r *http.Request, q queryOptions, insert bool, orig []byte, prefix, expr string) (int, string, string) {
	text := "SELECT " + expr
	if prefix != "" {
		text = prefix + ";\n" + text
	}
	setBody(r, orig) // the run before read it
	if !setQueryText(r, insert, text) {
		return http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query"
	}
	q2 := q
	q2.Query = text
	rec := newRecorder()
	f.runQuery(rec, r, q2, insert)
	var resp map[string]any
	_ = json.Unmarshal(rec.body.Bytes(), &resp)
	msg, failed := queryFailure(rec, resp)
	if !failed {
		return 0, "", ""
	}
	code, reason := http.StatusBadRequest, "invalidQuery"
	switch rec.status {
	case http.StatusNotFound:
		code, reason = http.StatusNotFound, "notFound"
	case http.StatusNotImplemented:
		code, reason = http.StatusNotImplemented, "notImplemented"
	}
	return code, reason, strings.TrimSpace(msg)
}
