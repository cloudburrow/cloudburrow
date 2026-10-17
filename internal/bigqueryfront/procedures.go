package bigqueryfront

// Stored procedures (#1217).
//
// The emulator reports CREATE PROCEDURE and CALL done and runs nothing
// (measured for #1201: a CALL of a procedure that inserts a row inserted
// none), and its routines.get and routines.list know no procedure. So the
// front keeps them itself and runs them with its script interpreter
// (scriptflow.go):
//
//   - CREATE [OR REPLACE] PROCEDURE [IF NOT EXISTS] name(args) [OPTIONS(...)]
//     BEGIN ... END stores the procedure, its arguments (IN, the default,
//     OUT or INOUT, each with its type), its OPTIONS' description and its
//     body, beside the functions the front knows (knownFunctions.procs),
//     in the same state file, so it survives a restart of the front. The
//     dataset must exist (404 notFound); a routine of the name that exists
//     fails without OR REPLACE or IF NOT EXISTS (409 duplicate).
//   - DROP PROCEDURE [IF EXISTS] name forgets it (404 without IF EXISTS
//     when there is none); so does routines.delete, and DROP SCHEMA
//     CASCADE of its dataset. DROP SCHEMA (RESTRICT) of a dataset holding
//     one fails resourceInUse.
//   - CALL name(args) runs its body as a script, with a scope of its own
//     holding the arguments: an IN argument is the caller's expression,
//     evaluated and cast to the argument's type; OUT and INOUT ones must
//     be the caller's variables, and the procedure's final values of them
//     are written back to those variables when it ends (by its end or a
//     RETURN). A procedure sees no variable of its caller's. An error in
//     its body that no handler of its own catches ends it and reaches the
//     caller's EXCEPTION handler, whose @@error.stack_trace holds a frame
//     for the statement in the procedure, with location
//     "project.dataset.procedure:line:column" and filename NULL, before the
//     CALL's own, and whose formatted_stack_trace reads "At
//     project.dataset.procedure[line:column]\nAt [line:column]", as the
//     procedural-language reference shows. The line and column in a
//     procedure are counted in its body, from BEGIN. "The maximum depth of
//     procedure calls is 50 frames" (same page): a CALL deeper fails.
//
// routines.list and routines.get show a procedure as routineType
// PROCEDURE, language SQL, with its arguments (mode and dataType) and its
// definitionBody, BEGIN ... END as written.
//
// Not carried out (501): a TEMP procedure, a procedure in another language
// (WITH CONNECTION, LANGUAGE), and CALL in a query sent as a dry run.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// procCallDepth is BigQuery's maximum depth of procedure calls.
const procCallDepth = 50

// procArg is one of a procedure's arguments.
type procArg struct {
	Mode string `json:"mode"` // IN, OUT or INOUT
	Name string `json:"name"`
	Type string `json:"type"` // as written
}

// storedProc is a procedure as the front keeps it: its Routine resource,
// and what CALL needs.
type storedProc struct {
	Routine map[string]any `json:"routine"`
	Args    []procArg      `json:"args"`
	Body    string         `json:"body"` // BEGIN ... END
}

// setProc records a procedure, path its dataset and name.
func (k *knownFunctions) setProc(project string, path []string, p storedProc) {
	b, err := json.Marshal(p)
	if k == nil || err != nil {
		return
	}
	key := project + "/" + path[0]
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.procs == nil {
		k.procs = map[string]map[string]json.RawMessage{}
	}
	if k.procs[key] == nil {
		k.procs[key] = map[string]json.RawMessage{}
	}
	k.procs[key][path[1]] = b
	k.saveLocked()
}

// dropProc forgets a procedure; it reports whether there was one.
func (k *knownFunctions) dropProc(project string, path []string) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.procs[project+"/"+path[0]][path[1]]; !ok {
		return false
	}
	delete(k.procs[project+"/"+path[0]], path[1])
	k.saveLocked()
	return true
}

// proc returns a procedure; routine IDs are case-sensitive.
func (k *knownFunctions) proc(project string, path []string) (storedProc, bool) {
	if k == nil {
		return storedProc{}, false
	}
	k.mu.Lock()
	b, ok := k.procs[project+"/"+path[0]][path[1]]
	k.mu.Unlock()
	var p storedProc
	if !ok || json.Unmarshal(b, &p) != nil {
		return storedProc{}, false
	}
	return p, true
}

// procsIn returns the procedures of a dataset, by name.
func (k *knownFunctions) procsIn(project, dataset string) map[string]storedProc {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]storedProc{}
	for n, b := range k.procs[project+"/"+dataset] {
		var p storedProc
		if json.Unmarshal(b, &p) == nil {
			out[n] = p
		}
	}
	return out
}

// isCreateProcedure reports whether the parser is at CREATE [OR REPLACE]
// [TEMP] PROCEDURE.
func (p *scriptParser) isCreateProcedure() bool {
	if !p.at("CREATE") {
		return false
	}
	for k := 1; ; k++ {
		t, ok := p.peek(k)
		switch {
		case !ok:
			return false
		case t.is("PROCEDURE"):
			return true
		case t.is("OR") || t.is("REPLACE") || t.is("TEMP") || t.is("TEMPORARY"):
		default:
			return false
		}
	}
}

// createProcedure reads CREATE ... PROCEDURE ... BEGIN ... END: its
// header in toks, its body in body, the body's text span in procSpan.
func (p *scriptParser) createProcedure() scriptNode {
	n := scriptNode{kind: nodeCreateProc}
	start := p.i
	depth := 0
	for p.i < len(p.toks) {
		t := p.toks[p.i]
		switch {
		case t.punct("(") || t.punct("["):
			depth++
		case t.punct(")") || t.punct("]"):
			depth--
		}
		if depth == 0 && t.is("BEGIN") {
			break
		}
		if depth == 0 && t.punct(";") {
			p.fail("expected BEGIN in CREATE PROCEDURE")
			return n
		}
		p.i++
	}
	if p.i >= len(p.toks) {
		p.fail("expected BEGIN in CREATE PROCEDURE")
		return n
	}
	n.toks = p.toks[start:p.i]
	begin := p.i
	block := p.node()
	if p.err != "" || block.kind != nodeBlock {
		p.fail("expected BEGIN ... END in CREATE PROCEDURE")
		return n
	}
	end := p.i - 1
	for end > begin && p.toks[end].punct(";") {
		end--
	}
	n.procSpan = [2]int{p.toks[begin].pos, p.toks[end].end}
	n.body = []scriptNode{block}
	return n
}

// procHeader is a CREATE PROCEDURE's header, read.
type procHeader struct {
	replace, ifNotExists bool
	path                 []string
	args                 []procArg
	description          string
	unsupported          string
}

// readProcHeader reads CREATE [OR REPLACE] PROCEDURE [IF NOT EXISTS]
// name(args) [OPTIONS(...)], from s.sql.
func readProcHeader(sql string, t []token) (procHeader, string) {
	var h procHeader
	i := 1
	for i < len(t) && !t[i].is("PROCEDURE") {
		switch {
		case t[i].is("REPLACE"):
			h.replace = true
		case t[i].is("TEMP") || t[i].is("TEMPORARY"):
			h.unsupported = "a TEMP procedure"
		}
		i++
	}
	i++
	if i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		h.ifNotExists = true
		i += 3
	}
	h.path, i = path(t, i)
	if len(h.path) == 0 || i >= len(t) || !t[i].punct("(") {
		return h, "Syntax error: expected the procedure's name and arguments"
	}
	end := closing(t, i)
	if end < 0 {
		return h, "Syntax error: expected ) after the procedure's arguments"
	}
	for _, a := range splitTop(t[i+1 : end]) {
		if len(a) == 0 {
			continue
		}
		arg := procArg{Mode: "IN"}
		if a[0].is("IN") || a[0].is("OUT") || a[0].is("INOUT") {
			arg.Mode = strings.ToUpper(a[0].text)
			a = a[1:]
		}
		if len(a) < 2 || a[0].kind != tokWord && a[0].kind != tokQuoted {
			return h, "Syntax error: expected an argument's name and type"
		}
		arg.Name = a[0].text
		arg.Type = sql[a[1].pos:a[len(a)-1].end]
		if a[1].is("ANY") {
			h.unsupported = "an argument of ANY TYPE"
		}
		h.args = append(h.args, arg)
	}
	for j := end + 1; j < len(t); j++ {
		switch {
		case t[j].is("OPTIONS") && j+1 < len(t) && t[j+1].punct("("):
			e := closing(t, j+1)
			if e < 0 {
				return h, "Syntax error in OPTIONS"
			}
			def := map[string]any{}
			readRoutineOptions(t[j+2:e], def)
			h.description, _ = def["description"].(string)
			j = e
		case t[j].is("WITH") || t[j].is("LANGUAGE"):
			h.unsupported = "a procedure in another language than SQL"
		}
	}
	return h, ""
}

// splitTop splits t at its top-level commas.
func splitTop(t []token) [][]token {
	var out [][]token
	depth, start := 0, 0
	for i, x := range t {
		switch {
		case x.punct("(") || x.punct("[") || x.punct("<"):
			depth++
		case x.punct(")") || x.punct("]") || x.punct(">"):
			depth--
		case depth == 0 && x.punct(","):
			out = append(out, t[start:i])
			start = i + 1
		}
	}
	return append(out, t[start:])
}

// project is the script's project.
func (s *scriptRun) project() string { return projectOf(s.f.base) }

// failWith is a failed statement's flow, as BigQuery answers it.
func failWith(status int, reason, msg, text string) *scriptFlow {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": msg,
		"errors": []map[string]string{{"message": msg, "domain": "global", "reason": reason}}}})
	return &scriptFlow{failed: true, status: status, body: body, msg: msg, text: text}
}

// procPath is a procedure's dataset and name, by the script's default
// dataset when it names none.
func (s *scriptRun) procPath(parts []string, text string) ([]string, *scriptFlow) {
	ds, name, ok := tableOf(s.q, parts)
	if !ok {
		return nil, failWith(http.StatusBadRequest, "invalidQuery", "Procedure name "+strings.Join(parts, ".")+
			" is missing a dataset while no default dataset is set in the request.", text)
	}
	return []string{ds, name}, nil
}

// createProc carries out CREATE PROCEDURE.
func (s *scriptRun) createProc(n scriptNode) *scriptFlow {
	if flow := s.flush(); flow != nil {
		return flow
	}
	text := s.sql[n.toks[0].pos:n.procSpan[1]]
	h, msg := readProcHeader(s.sql, n.toks)
	if msg != "" {
		return failWith(http.StatusBadRequest, "invalidQuery", msg, text)
	}
	if h.unsupported != "" {
		s.notImpl = "Not implemented here: " + h.unsupported + ". CloudBurrow runs SQL procedures itself, and not this one. Nothing more was run."
		return &scriptFlow{}
	}
	p, flow := s.procPath(h.path, text)
	if flow != nil {
		return flow
	}
	project := s.project()
	if status, _ := s.f.get(s.r, "/datasets/"+url.PathEscape(p[0])); status == http.StatusNotFound {
		return failWith(http.StatusNotFound, "notFound", "Not found: Dataset "+project+":"+p[0], text)
	}
	_, exists := s.f.functions.proc(project, p)
	if !exists {
		_, exists = s.f.functions.defsIn(project, p[0])[strings.ToLower(p[1])]
		exists = exists && s.f.routineExists(s.r, p[0], p[1])
	}
	switch {
	case exists && h.ifNotExists:
		return s.assigned()
	case exists && !h.replace:
		return failWith(http.StatusConflict, "duplicate", "Already Exists: Routine "+project+":"+p[0]+"."+p[1], text)
	}
	now := strconv.FormatInt(s.f.now().UnixMilli(), 10)
	routine := map[string]any{
		"routineReference": map[string]any{"projectId": project, "datasetId": p[0], "routineId": p[1]},
		"routineType":      "PROCEDURE", "language": "SQL",
		"definitionBody": s.sql[n.procSpan[0]:n.procSpan[1]],
		"creationTime":   now, "lastModifiedTime": now, "etag": etagOf(now + p[1]),
	}
	if old, ok := s.f.functions.proc(project, p); ok {
		if c, ok := old.Routine["creationTime"]; ok {
			routine["creationTime"] = c
		}
	}
	if h.description != "" {
		routine["description"] = h.description
	}
	var args []map[string]any
	for _, a := range h.args {
		arg := map[string]any{"name": a.Name, "mode": a.Mode}
		if tt, ok := lex(a.Type); ok {
			if typ, _ := sqlTypeAt(tt, 0); typ != nil {
				arg["dataType"] = typ
			}
		}
		args = append(args, arg)
	}
	if len(args) > 0 {
		routine["arguments"] = args
	}
	s.f.functions.setProc(project, p, storedProc{Routine: routine, Args: h.args, Body: s.sql[n.procSpan[0]:n.procSpan[1]]})
	return s.assigned()
}

// dropProc carries out DROP PROCEDURE [IF EXISTS] name.
func (s *scriptRun) dropProc(toks []token) *scriptFlow {
	if flow := s.flush(); flow != nil {
		return flow
	}
	text := s.text(toks)
	i := 2
	ifExists := false
	if i+1 < len(toks) && toks[i].is("IF") && toks[i+1].is("EXISTS") {
		ifExists, i = true, i+2
	}
	parts, next := path(toks, i)
	if len(parts) == 0 || next != len(toks) {
		return failWith(http.StatusBadRequest, "invalidQuery", "Syntax error in DROP PROCEDURE", text)
	}
	p, flow := s.procPath(parts, text)
	if flow != nil {
		return flow
	}
	if !s.f.functions.dropProc(s.project(), p) && !ifExists {
		return failWith(http.StatusNotFound, "notFound", "Not found: Procedure "+s.project()+":"+p[0]+"."+p[1], text)
	}
	return s.assigned()
}

// call carries out CALL name(args).
func (s *scriptRun) call(toks []token) *scriptFlow {
	if flow := s.flush(); flow != nil {
		return flow
	}
	stmt := s.text(toks)
	parts, i := path(toks, 1)
	if len(parts) == 2 && strings.EqualFold(parts[0], "BQ") && strings.EqualFold(parts[1], "REFRESH_MATERIALIZED_VIEW") {
		return s.runText(stmt) // #1415, materializedviews.go
	}
	if len(parts) == 0 || i >= len(toks) || !toks[i].punct("(") || closing(toks, i) != len(toks)-1 {
		return failWith(http.StatusBadRequest, "invalidQuery", "Syntax error in CALL", stmt)
	}
	p, flow := s.procPath(parts, stmt)
	if flow != nil {
		return flow
	}
	project := s.project()
	proc, ok := s.f.functions.proc(project, p)
	if !ok {
		return failWith(http.StatusNotFound, "notFound", "Not found: Procedure "+project+":"+p[0]+"."+p[1], stmt)
	}
	if s.depth >= procCallDepth {
		return failWith(http.StatusBadRequest, "invalidQuery",
			fmt.Sprintf("The maximum depth of procedure calls is %d frames; CALL %s.%s would exceed it.", procCallDepth, p[0], p[1]), stmt)
	}
	var given [][]token
	if i+1 < len(toks)-1 {
		given = splitTop(toks[i+1 : len(toks)-1])
	}
	if len(given) != len(proc.Args) {
		return failWith(http.StatusBadRequest, "invalidQuery", fmt.Sprintf("Procedure %s.%s expects %d arguments, but %d were given.",
			p[0], p[1], len(proc.Args), len(given)), stmt)
	}
	// The arguments, evaluated in the caller's scope.
	scope := map[string]scriptVar{}
	outs := map[string]string{} // the procedure's argument -> the caller's variable
	for k, a := range proc.Args {
		arg := given[k]
		name := strings.ToLower(a.Name)
		if a.Mode == "IN" {
			text := s.text(arg)
			if s.notImpl != "" {
				return &scriptFlow{}
			}
			s.place(arg)
			v, flow := s.evaluate(text, a.Type)
			if flow != nil {
				return flow
			}
			scope[name] = v
			continue
		}
		if len(arg) != 1 || arg[0].kind != tokWord && arg[0].kind != tokQuoted {
			return failWith(http.StatusBadRequest, "invalidQuery", fmt.Sprintf("Argument %s of procedure %s.%s is %s, "+
				"so it must be a variable.", a.Name, p[0], p[1], a.Mode), stmt)
		}
		v, ok := s.lookupVar(arg[0].text)
		if !ok {
			return failWith(http.StatusBadRequest, "invalidQuery", "Undeclared variable: "+arg[0].text, stmt)
		}
		if a.Mode == "OUT" {
			v = scriptVar{a.Type, "CAST(NULL AS " + a.Type + ")"}
		}
		scope[name] = v
		outs[name] = arg[0].text
	}
	body, ok := lex(proc.Body)
	if !ok {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: the procedure's body cannot be read", stmt)
	}
	parser := &scriptParser{toks: body}
	nodes := parser.block()
	if parser.err != "" {
		return failWith(http.StatusInternalServerError, "internalError", "cloudburrow: the procedure's body cannot be read: "+parser.err, stmt)
	}
	loc := project + "." + p[0] + "." + p[1]
	// The body runs in a frame of its own: its own text, its own scopes.
	// Its child jobs' frames are the body's, then this CALL's (#1569).
	sql, scopes, handlers, oldLoc, callers := s.sql, s.scopes, s.inHandler, s.loc, s.callers
	s.callers = s.framesOf(toks)
	s.sql, s.scopes, s.inHandler, s.loc = proc.Body, []map[string]scriptVar{scope}, nil, loc
	s.depth++
	out := s.run(nodes)
	s.depth--
	final := s.scopes[0]
	if out != nil && out.failed && out.hasPos {
		line, col := lineColumn(s.sql, out.pos)
		out.frames = append(out.frames, stackFrame{line, col, loc})
		out.hasPos = false
	}
	s.sql, s.scopes, s.inHandler, s.loc, s.callers = sql, scopes, handlers, oldLoc, callers
	switch {
	case out == nil, out.brk && out.label == "\x00return":
	default:
		return out
	}
	for name, callerVar := range outs {
		if flow := s.assign(callerVar, final[name], nil, stmt); flow != nil {
			return flow
		}
	}
	return nil
}

// stackFrame is one frame of @@error.stack_trace inside a procedure.
type stackFrame struct {
	line, col int
	loc       string
}
