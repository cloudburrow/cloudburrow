package bigqueryfront

import (
	"path/filepath"
	"testing"
)

// TestProcedureParse (#1217): CREATE PROCEDURE is read with its body, a
// block of its own, and its header's arguments, modes and OPTIONS; CALL
// and DROP PROCEDURE are statements of their own.
func TestProcedureParse(t *testing.T) {
	sql := "CREATE OR REPLACE PROCEDURE ds.p(x INT64, OUT y STRING, INOUT z ARRAY<STRUCT<a INT64, b STRING>>) " +
		"OPTIONS(description = 'adds') BEGIN IF x > 0 THEN SET y = 'pos'; END IF; BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN RETURN; END; END; " +
		"CALL ds.p(1, v, w); DROP PROCEDURE IF EXISTS ds.p"
	toks, _ := lex(sql)
	p := &scriptParser{toks: toks}
	nodes := p.block()
	if p.err != "" {
		t.Fatal(p.err)
	}
	if len(nodes) != 3 || nodes[0].kind != nodeCreateProc || nodes[1].kind != nodeCall || nodes[2].kind != nodeDropProc {
		t.Fatalf("nodes %+v", nodes)
	}
	body := sql[nodes[0].procSpan[0]:nodes[0].procSpan[1]]
	if want := "BEGIN IF x > 0 THEN SET y = 'pos'; END IF; BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN RETURN; END; END"; body != want {
		t.Errorf("body %q, want %q", body, want)
	}
	h, msg := readProcHeader(sql, nodes[0].toks)
	if msg != "" || !h.replace || h.ifNotExists || len(h.path) != 2 || h.description != "adds" || len(h.args) != 3 {
		t.Fatalf("header %+v %s", h, msg)
	}
	want := []procArg{{"IN", "x", "INT64"}, {"OUT", "y", "STRING"}, {"INOUT", "z", "ARRAY<STRUCT<a INT64, b STRING>>"}}
	for i := range want {
		if h.args[i] != want[i] {
			t.Errorf("argument %d: %+v, want %+v", i, h.args[i], want[i])
		}
	}
	if !hasControlFlow("CALL ds.p()") || !hasControlFlow("DROP PROCEDURE ds.p") || hasControlFlow("SELECT 'CALL'") {
		t.Error("procedure statements are not carried out by the interpreter")
	}
	for _, bad := range []string{"CREATE PROCEDURE ds.p() SELECT 1", "CREATE PROCEDURE ds.p() BEGIN SELECT 1;"} {
		toks, _ := lex(bad)
		p := &scriptParser{toks: toks}
		p.block()
		if p.err == "" {
			t.Errorf("%q: no syntax error", bad)
		}
	}
}

// TestProceduresOutliveAFrontRestart (#1217): the procedures are kept in
// the front's state file, a restart of the emulator empties them, and
// DROP SCHEMA's forget drops a dataset's.
func TestProceduresOutliveAFrontRestart(t *testing.T) {
	path := functionsStateFile(filepath.Join(t.TempDir(), "front"))
	k := &knownFunctions{started: 1}
	k.keep(path, nil)
	k.setProc("p", []string{"ds", "P"}, storedProc{Routine: map[string]any{"routineType": "PROCEDURE"}, Body: "BEGIN END"})
	again := &knownFunctions{}
	again.keep(path, nil)
	got, ok := again.proc("p", []string{"ds", "P"})
	if !ok || got.Body != "BEGIN END" || got.Routine["routineType"] != "PROCEDURE" {
		t.Fatalf("restored %+v %v", got, ok)
	}
	if _, ok := again.proc("p", []string{"ds", "p"}); ok {
		t.Error("a procedure's name is case-sensitive")
	}
	if len(again.procsIn("p", "ds")) != 1 {
		t.Error("procsIn")
	}
	again.forget("p", "ds")
	if _, ok := again.proc("p", []string{"ds", "P"}); ok {
		t.Error("forget kept the procedure")
	}
	again.setProc("p", []string{"ds", "Q"}, storedProc{})
	again.reset()
	third := &knownFunctions{}
	third.keep(path, nil)
	if len(third.procsIn("p", "ds")) != 0 {
		t.Error("the emulator's restart kept the procedures")
	}
	if third.dropProc("p", []string{"x", "y"}) {
		t.Error("dropProc of nothing")
	}
}

// TestProcedureStackTrace (#1217): a failure in a procedure gives its
// frame, with location, before the caller's.
func TestProcedureStackTrace(t *testing.T) {
	s := &scriptRun{sql: "SELECT 1;\n  CALL ds.p()"}
	flow := &scriptFlow{failed: true, msg: "x", pos: 12, hasPos: true, frames: []stackFrame{{2, 3, "pr.ds.p"}}}
	v := s.errorVars(flow)
	if got, want := v["formatted_stack_trace"].lit, sqlString("At pr.ds.p[2:3]\nAt [2:3]"); got != want {
		t.Errorf("formatted %s, want %s", got, want)
	}
	if got, want := v["stack_trace"].lit, "[STRUCT<line INT64, column INT64, filename STRING, location STRING>(2, 3, NULL, 'pr.ds.p:2:3'), "+
		"STRUCT<line INT64, column INT64, filename STRING, location STRING>(2, 3, NULL, NULL)]"; got != want {
		t.Errorf("stack %s, want %s", got, want)
	}
}
