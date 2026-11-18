package bigqueryfront

import (
	"fmt"
	"strings"
	"testing"
)

// TestFindAIGenerate (#1765, #1771, #1772): the calls, their arguments and
// the prompts' probe over the call's block.
func TestFindAIGenerate(t *testing.T) {
	sql := "WITH t AS (SELECT 'a' AS s) SELECT id, AI.GENERATE(CONCAT('Say ', s), endpoint => 'm', connection_id => 'us.c').result AS r " +
		"FROM t WHERE id > 1 ORDER BY id"
	toks, _ := lex(sql)
	depth := tokenDepths(toks)
	calls, code, msg := findAICalls(sql, toks, depth)
	if code != 0 || len(calls) != 1 {
		t.Fatalf("%d %s %v", code, msg, calls)
	}
	c := calls[0]
	if c.fn != "GENERATE" || c.args["prompt"] != "CONCAT('Say ', s)" || c.args["endpoint"] != "'m'" || c.args["connection_id"] != "'us.c'" {
		t.Errorf("%+v", c)
	}
	probe := fmt.Sprintf(blockSource(sql, toks, depth, c.block), c.keyExpr())
	if want := "WITH t AS (SELECT 'a' AS s) SELECT DISTINCT TO_JSON_STRING(CONCAT('Say ', s)) AS _cbai_k FROM t "; probe != want {
		t.Errorf("probe %q, want %q", probe, want)
	}
	if !strings.Contains(sql[c.start:c.end], "AI.GENERATE(") || !strings.HasSuffix(sql[c.start:c.end], "'us.c')") {
		t.Errorf("span %q", sql[c.start:c.end])
	}
	for in, want := range map[string]int{
		"SELECT AI.GENERATE(prompt => 'x')":                                       0,
		"SELECT AI.GENERATE('x', 'y')":                                            400,
		"SELECT AI.GENERATE(endpoint => 'm')":                                     400,
		"SELECT AI.GENERATE('x', prompt => 'y')":                                  400,
		"SELECT (SELECT AI.GENERATE(s) FROM t) AS a":                              0,
		"SELECT AI.GENERATE('x', bogus => 1)":                                     400,
		"SELECT AI.GENERATE_BOOL('x', output_schema => 'a INT64')":                400,
		"SELECT AI.IF('x', endpoint => 'm')":                                      0,
		"SELECT AI.CLASSIFY('x', ['a', 'b'])":                                     0,
		"SELECT AI.IF('x', examples => ARRAY<STRUCT<STRING, BOOL>>[('a', TRUE)])": 0,
		"SELECT AI.CLASSIFY('x')":                                                 400,
		"SELECT AI.SIMILARITY('a', 'b')":                                          400,
		"SELECT AI.SIMILARITY(content1 => 'a', content2 => 'b')":                  0,
		"SELECT AI.EMBED('a', endpoint => 'e', endpoint => 'f')":                  400,
		"SELECT AI.SCORE(endpoint => 'm', prompt => 'x')":                         0,
	} {
		toks, _ := lex(in)
		if _, code, msg := findAICalls(in, toks, tokenDepths(toks)); code != want {
			t.Errorf("%s: %d %s, want %d", in, code, msg, want)
		}
	}
}

func TestPromptText(t *testing.T) {
	for in, want := range map[string]string{
		`"hi"`: "hi", `{"a":"Describe ","b":"cats"}`: "Describe cats", `{"a":"x","b":["y","z"]}`: "xyz",
	} {
		if got, ok, code, _ := promptText(in); !ok || code != 0 || got != want {
			t.Errorf("%s: %q %v %d", in, got, ok, code)
		}
	}
	if _, ok, code, _ := promptText("null"); ok || code != 0 {
		t.Error("a NULL prompt")
	}
	if _, _, code, _ := promptText(`{"a":"x","b":{"uri":"gs://b/o"}}`); code != 501 {
		t.Errorf("an ObjectRef: %d", code)
	}
	if _, _, code, _ := promptText(`{"a":1}`); code != 400 {
		t.Errorf("an INT64 field: %d", code)
	}
}

func TestAICallKeys(t *testing.T) {
	sql := "SELECT AI.SIMILARITY(content1 => a, content2 => 'b') AS s, AI.IF(CONCAT('Is ', a), endpoint => 'm') FROM t WHERE AI.IF(a)"
	toks, _ := lex(sql)
	calls, code, msg := findAICalls(sql, toks, tokenDepths(toks))
	if code != 0 || len(calls) != 3 {
		t.Fatalf("%d %s %v", code, msg, calls)
	}
	if got := calls[0].keyExpr(); got != "STRUCT(a AS _0, 'b' AS _1)" {
		t.Errorf("SIMILARITY key %q", got)
	}
	if calls[1].fn != "IF" || calls[1].keyExpr() != "CONCAT('Is ', a)" || calls[2].keyExpr() != "a" {
		t.Errorf("%+v", calls)
	}
	// AI.IF in WHERE reads its prompts over the block's FROM.
	if probe := fmt.Sprintf(blockSource(sql, toks, tokenDepths(toks), calls[2].block), calls[2].keyExpr()); probe != "SELECT DISTINCT TO_JSON_STRING(a) AS _cbai_k FROM t " {
		t.Errorf("probe %q", probe)
	}
}

func TestOutputSchema(t *testing.T) {
	fields, msg := parseOutputSchema("name STRING NOT NULL OPTIONS(description = 'the name'), age INT64, " +
		"address STRUCT<street STRING, city STRING>, tags ARRAY<STRING>, w FLOAT64, ok BOOL, state STRING OPTIONS(`enum` = ['CA', 'NY'])")
	if msg != "" || len(fields) != 7 {
		t.Fatalf("%s %v", msg, fields)
	}
	if !fields[0].notNull || fields[0].desc != "the name" || fields[2].typ.sql() != "STRUCT<`street` STRING, `city` STRING>" ||
		fields[3].typ.sql() != "ARRAY<STRING>" || len(fields[6].enum) != 2 {
		t.Errorf("%+v", fields)
	}
	lits, why := schemaLiterals(fields, "```json\n{\"name\": \"Ann\", \"age\": 40, \"address\": {\"street\": \"1 Main\", \"city\": \"X\"}, "+
		"\"tags\": [\"a\"], \"w\": 1.5, \"ok\": true, \"state\": \"CA\"}\n```")
	want := []string{"'Ann'", "40", "STRUCT<`street` STRING, `city` STRING>('1 Main', 'X')", "ARRAY<STRING>['a']", "1.5", "TRUE", "'CA'"}
	if why != "" || strings.Join(lits, "|") != strings.Join(want, "|") {
		t.Errorf("%s %v", why, lits)
	}
	if _, why := schemaLiterals(fields, `{"age": 3}`); !strings.Contains(why, "NOT NULL") {
		t.Errorf("a missing NOT NULL field: %s", why)
	}
	if _, why := schemaLiterals(fields, `{"name": "a", "state": "TX"}`); !strings.Contains(why, "enum") {
		t.Errorf("an enum value: %s", why)
	}
	if lits, why := schemaLiterals(fields, `{"name": "a"}`); why != "" || lits[1] != "CAST(NULL AS INT64)" {
		t.Errorf("a missing field: %s %v", why, lits)
	}
	for _, bad := range []string{"", "a", "a DATE", "a STRING,", "a ARRAY<ARRAY<INT64>>", "a STRING, a INT64", "a STRING OPTIONS(foo = 1)"} {
		if _, msg := parseOutputSchema(bad); msg == "" {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestScalarAnswers(t *testing.T) {
	for in, want := range map[string]string{"true": "TRUE", " False.\n": "FALSE", "**true**": "TRUE"} {
		if got, why := parseBoolAnswer(in); got != want || why != "" {
			t.Errorf("%q: %s %s", in, got, why)
		}
	}
	if _, why := parseBoolAnswer("Yes, it is."); why == "" {
		t.Error("a sentence parsed as a BOOL")
	}
	if got, _ := parseIntAnswer("8\n"); got != "8" {
		t.Errorf("int: %s", got)
	}
	if _, why := parseIntAnswer("8.5"); why == "" {
		t.Error("8.5 parsed as an INT64")
	}
	if got, _ := parseFloatAnswer("7.5"); got != "7.5" {
		t.Errorf("float: %s", got)
	}
	a := aiResolved{categories: [][2]string{{"Billing", ""}, {"Service", "outages"}}}
	if got, why := parseCategories("billing", a); got != "'Billing'" || why != "" {
		t.Errorf("single: %s %s", got, why)
	}
	a.arrayOut, a.multi = true, true
	if got, why := parseCategories(`["Service", "billing"]`, a); got != "ARRAY<STRING>['Service', 'Billing']" || why != "" {
		t.Errorf("multi: %s %s", got, why)
	}
	if _, why := parseCategories(`["Other"]`, a); why == "" {
		t.Error("an unknown category")
	}
	if s, ok := cosine([]float64{1, 0}, []float64{1, 1}); !ok || s < 0.707 || s > 0.708 {
		t.Errorf("cosine %f", s)
	}
}
