package bigqueryfront

import (
	"strings"
	"testing"
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
