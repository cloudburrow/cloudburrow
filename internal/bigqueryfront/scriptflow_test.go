package bigqueryfront

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestScriptParser (#1201): a script's blocks are read into their parts.
func TestScriptParser(t *testing.T) {
	sql := "DECLARE i INT64 DEFAULT 0; outer_loop: LOOP SET i = i + 1; IF i > 3 THEN LEAVE outer_loop; ELSEIF i = 2 THEN CONTINUE; ELSE SELECT CASE WHEN i = 1 THEN 'a' END; END IF; END LOOP outer_loop; " +
		"WHILE i < 9 DO SET i = i + 1; END WHILE; REPEAT SET i = i - 1; UNTIL i < 0 END REPEAT; FOR r IN (SELECT 1 AS n) DO SELECT r.n; END FOR; " +
		"CASE i WHEN 1 THEN SELECT 1; ELSE SELECT 2; END CASE; BEGIN SELECT 1; EXCEPTION WHEN ERROR THEN SELECT @@error.message; END; RETURN"
	toks, ok := lex(sql)
	if !ok {
		t.Fatal("lex")
	}
	p := &scriptParser{toks: toks}
	nodes := p.block()
	if p.err != "" {
		t.Fatal(p.err)
	}
	var kinds []scriptNodeKind
	for _, n := range nodes {
		kinds = append(kinds, n.kind)
	}
	want := []scriptNodeKind{nodeDeclare, nodeLoop, nodeWhile, nodeRepeat, nodeFor, nodeCase, nodeBlock, nodeStmt}
	if len(kinds) != len(want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds %v, want %v", kinds, want)
		}
	}
	loop := nodes[1]
	if loop.label != "outer_loop" || len(loop.body) != 2 || loop.body[1].kind != nodeIf || len(loop.body[1].conds) != 2 || !loop.body[1].hasEls {
		t.Errorf("the LOOP: %+v", loop)
	}
	if b := loop.body[1].bodies[0]; len(b) != 1 || b[0].kind != nodeBreak || b[0].target != "outer_loop" {
		t.Errorf("LEAVE: %+v", b)
	}
	if nodes[4].forVar != "r" || nodes[5].caseExpr == nil || !nodes[6].hasHandler || len(nodes[6].handler) != 1 {
		t.Errorf("FOR, CASE or the handler: %+v %+v %+v", nodes[4], nodes[5], nodes[6])
	}
	for _, bad := range []string{"IF TRUE THEN SELECT 1;", "WHILE TRUE DO SELECT 1; END LOOP", "LOOP SELECT 1;"} {
		toks, _ := lex(bad)
		p := &scriptParser{toks: toks}
		p.block()
		if p.err == "" {
			t.Errorf("%q: no syntax error", bad)
		}
	}
}

// TestScriptLiterals (#1201): a value a query gave is kept as a literal of
// its type.
func TestScriptLiterals(t *testing.T) {
	for _, c := range []struct {
		fl        field
		v         any
		typ, want string
	}{
		{field{Type: "INTEGER"}, "5", "INT64", "CAST('5' AS INT64)"},
		{field{Type: "STRING"}, "it's", "STRING", `'it\'s'`},
		{field{Type: "BOOLEAN"}, "true", "BOOL", "TRUE"},
		{field{Type: "TIMESTAMP"}, "1.5E9", "TIMESTAMP", "TIMESTAMP_MICROS(1500000000000000)"},
		{field{Type: "BYTES"}, "YWJj", "BYTES", "FROM_BASE64('YWJj')"},
		{field{Type: "INTEGER"}, nil, "INT64", "CAST(NULL AS INT64)"},
		{field{Type: "INTEGER", Mode: "REPEATED"}, []any{map[string]any{"v": "1"}}, "ARRAY<INT64>", "ARRAY<INT64>[CAST('1' AS INT64)]"},
		{field{Type: "RECORD", Fields: []field{{Name: "n", Type: "INTEGER"}}}, map[string]any{"f": []any{map[string]any{"v": "2"}}},
			"STRUCT<`n` INT64>", "STRUCT<`n` INT64>(CAST('2' AS INT64))"},
	} {
		got, msg := literalOf(c.fl, c.v)
		if msg != "" || got.typ != c.typ || got.lit != c.want {
			t.Errorf("%+v %v: %+v %s, want %s %s", c.fl, c.v, got, msg, c.typ, c.want)
		}
	}
	if _, msg := literalOf(field{Type: "RANGE"}, "x"); !strings.Contains(msg, "Not implemented") {
		t.Errorf("RANGE: %s", msg)
	}
}

// TestScriptRaise (#1201): RAISE fails the script with its message, which
// a handler catches; uncaught, it is the answer, 400.
func TestScriptRaise(t *testing.T) {
	emu := &scriptEmulator{run: func(q string) (int, string) {
		if strings.Contains(q, "CAST(('boom') AS STRING)") {
			return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"f0_","type":"STRING"}]},"rows":[{"f":[{"v":"boom"}]}]}`
		}
		return 200, `{"jobComplete":true}`
	}}
	code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "RAISE USING MESSAGE = 'boom'"))
	if msg, _ := got["error"].(map[string]any)["message"].(string); code != 400 || msg != "boom" {
		t.Errorf("RAISE: %d %v", code, got)
	}
	code, got = do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "BEGIN RAISE USING MESSAGE = 'boom'; EXCEPTION WHEN ERROR THEN SELECT @@error.message AS m; END"))
	if code != 200 || emu.log[len(emu.log)-1] != "QUERY SELECT ('boom') AS m" {
		t.Errorf("a caught RAISE: %d %v %q", code, got, emu.log)
	}
}

// TestScriptTimeLimit (#1201): a script fails once it has run for
// BigQuery's cumulative time limit for a multi-statement query, 24 hours,
// not after a count of steps of CloudBurrow's own.
func TestScriptTimeLimit(t *testing.T) {
	now := time.Unix(0, 0)
	defer func(f func() time.Time) { scriptNow = f }(scriptNow)
	scriptNow = func() time.Time { now = now.Add(time.Hour); return now }
	emu := &scriptEmulator{run: func(q string) (int, string) { return 200, `{"jobComplete":true}` }}
	code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "LOOP SELECT 1; END LOOP"))
	msg, _ := got["error"].(map[string]any)["message"].(string)
	if code != 400 || !strings.Contains(msg, "24 hours") {
		t.Errorf("a loop that never ends: %d %v", code, got)
	}
	if n := len(emu.log); n < 5 || n > 30 {
		t.Errorf("the loop ran %d statements in its 24 hours of one an hour", n)
	}
}

// TestScriptVariableLimits (#1201): a variable of over 1 MB fails the
// script, BigQuery's documented limit.
func TestScriptVariableLimits(t *testing.T) {
	s := &scriptRun{scopes: []map[string]scriptVar{{}}}
	if flow := s.assign("x", scriptVar{"STRING", "'" + strings.Repeat("a", scriptVarLimit) + "'"}, s.scopes[0], "SET x = ..."); flow == nil || !strings.Contains(flow.msg, "1 MB") {
		t.Errorf("a variable of over 1 MB: %+v", flow)
	}
	for i := 0; i < 10; i++ {
		flow := s.assign(fmt.Sprint("v", i), scriptVar{"STRING", strings.Repeat("a", scriptVarLimit-10)}, s.scopes[0], "DECLARE")
		if flow != nil {
			t.Fatalf("variable %d: %+v", i, flow)
		}
	}
	if flow := s.assign("v10", scriptVar{"STRING", strings.Repeat("a", scriptVarLimit-10)}, s.scopes[0], "DECLARE"); flow == nil || !strings.Contains(flow.msg, "10 MB") {
		t.Errorf("variables of over 10 MB: %+v", flow)
	}
}

// TestScriptStackTrace (#1201): @@error.stack_trace and
// @@error.formatted_stack_trace name the failed statement's line and
// column, in the forms of BigQuery's reference.
func TestScriptStackTrace(t *testing.T) {
	sql := "DECLARE x INT64;\nBEGIN\n  SELECT 1/0;\nEXCEPTION WHEN ERROR THEN SELECT 1; END"
	s := &scriptRun{sql: sql}
	vars := s.errorVars(&scriptFlow{failed: true, msg: "division by zero", pos: strings.Index(sql, "SELECT 1/0"), hasPos: true})
	if got := vars["formatted_stack_trace"].lit; got != "'At [3:3]'" {
		t.Errorf("formatted_stack_trace %s", got)
	}
	if got := vars["stack_trace"].lit; got != "[STRUCT<line INT64, column INT64, filename STRING, location STRING>(3, 3, NULL, NULL)]" {
		t.Errorf("stack_trace %s", got)
	}
	if l, c := lineColumn("é\nab", 3); l != 2 || c != 1 {
		t.Errorf("lineColumn %d %d", l, c)
	}
}

// TestScriptLastStatement (#1201): a last statement that changes data is
// the client's own request, so a query job keeps its DML counts; a script
// ending in a SET answers with no rows.
func TestScriptLastStatement(t *testing.T) {
	emu := &scriptEmulator{run: func(q string) (int, string) {
		if strings.HasSuffix(q, "AS BOOL)") {
			return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"f0_","type":"BOOLEAN"}]},"rows":[{"f":[{"v":"true"}]}]}`
		}
		if strings.HasPrefix(q, "SELECT CAST") {
			return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"f0_","type":"INTEGER"}]},"rows":[{"f":[{"v":"1"}]}]}`
		}
		return 200, `{"jobComplete":true}`
	}}
	do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "IF TRUE THEN INSERT INTO ds.t (n) VALUES (1); END IF"))
	if n := len(emu.log); n == 0 || !strings.Contains(emu.log[n-1], "INSERT INTO") {
		t.Errorf("the last DML: %q", emu.log)
	}
	emu.log = nil
	do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", "DECLARE x INT64; IF TRUE THEN SELECT 1 AS a; SET x = 1; END IF"))
	if n := len(emu.log); n == 0 || strings.Contains(emu.log[n-1], "AS a") {
		t.Errorf("a script ending in SET: %q", emu.log)
	}
}

// TestScriptAssert (#1253): ASSERT, alone or in a script, fails with
// "Assertion failed: " and its description, or its expression's text,
// when the expression is FALSE or NULL; a statement after it is not sent.
func TestScriptAssert(t *testing.T) {
	emu := &scriptEmulator{run: func(q string) (int, string) {
		cell := ""
		switch {
		case strings.HasPrefix(q, "SELECT (TRUE"):
			cell = `{"v":"true"}`
		case strings.HasPrefix(q, "SELECT (FALSE"):
			cell = `{"v":"false"}`
		case strings.HasPrefix(q, "SELECT (NULL"):
			cell = `{"v":null}`
		case strings.HasPrefix(q, "SELECT (1)"):
			return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"f0_","type":"INTEGER"}]},"rows":[{"f":[{"v":"1"}]}]}`
		default:
			return 200, `{"jobComplete":true}`
		}
		return 200, `{"jobComplete":true,"schema":{"fields":[{"name":"f0_","type":"BOOLEAN"}]},"rows":[{"f":[` + cell + `]}]}`
	}}
	for sql, want := range map[string]string{
		"ASSERT FALSE":                              "Assertion failed: FALSE",
		"ASSERT FALSE AS 'must hold'":               "Assertion failed: must hold",
		"ASSERT NULL AS \"is null\";":               "Assertion failed: is null",
		"ASSERT FALSE; INSERT INTO ds.t VALUES (1)": "Assertion failed: FALSE",
		"ASSERT TRUE":                               "",
		"ASSERT TRUE AS 'x'; SELECT 2":              "",
		"ASSERT 1":                                  "ASSERT expression must be of type BOOL, but is of type INT64",
		"ASSERT TRUE AS x":                          "ASSERT description must be a STRING literal",
	} {
		emu.log = nil
		code, got := do(t, Wrap(emu), "POST", base+"/queries", queryBody("/queries", sql))
		if want == "" {
			if code != 200 {
				t.Errorf("%q: %d %v, want 200", sql, code, got)
			}
			continue
		}
		e, _ := got["error"].(map[string]any)
		if code != 400 || e == nil || e["message"] != want || errorReason(got) != "invalidQuery" {
			t.Errorf("%q: %d %v, want 400 %q", sql, code, got, want)
		}
		for _, q := range emu.log {
			if strings.Contains(q, "INSERT") {
				t.Errorf("%q: sent %q after the failed ASSERT", sql, q)
			}
		}
	}
	if earlierQuery("ASSERT FALSE") != scriptSplits || earlierQuery("CREATE TEMP TABLE t AS SELECT 1 AS a; ASSERT TRUE") != scriptKeepsTemp {
		t.Errorf("earlierQuery does not carry out ASSERT")
	}
}
