package bigqueryfront

import (
	"fmt"
	"strings"
	"testing"
)

// TestFindAIGenerate (#1765): the calls, their arguments and the prompts'
// probe over the call's block.
func TestFindAIGenerate(t *testing.T) {
	sql := "WITH t AS (SELECT 'a' AS s) SELECT id, AI.GENERATE(CONCAT('Say ', s), endpoint => 'm', connection_id => 'us.c').result AS r " +
		"FROM t WHERE id > 1 ORDER BY id"
	toks, _ := lex(sql)
	depth := tokenDepths(toks)
	calls, code, msg := findAIGenerate(sql, toks, depth)
	if code != 0 || len(calls) != 1 {
		t.Fatalf("%d %s %v", code, msg, calls)
	}
	c := calls[0]
	if c.prompt != "CONCAT('Say ', s)" || c.named["endpoint"] != "'m'" || c.named["connection_id"] != "'us.c'" {
		t.Errorf("%+v", c)
	}
	probe := fmt.Sprintf(blockSource(sql, toks, depth, c.block), c.prompt)
	if want := "WITH t AS (SELECT 'a' AS s) SELECT DISTINCT TO_JSON_STRING(CONCAT('Say ', s)) AS _cbai_k FROM t "; probe != want {
		t.Errorf("probe %q, want %q", probe, want)
	}
	if !strings.Contains(sql[c.start:c.end], "AI.GENERATE(") || !strings.HasSuffix(sql[c.start:c.end], "'us.c')") {
		t.Errorf("span %q", sql[c.start:c.end])
	}
	for in, want := range map[string]int{
		"SELECT AI.GENERATE(prompt => 'x')":          0,
		"SELECT AI.GENERATE('x', 'y')":               400,
		"SELECT AI.GENERATE(endpoint => 'm')":        400,
		"SELECT AI.GENERATE('x', prompt => 'y')":     400,
		"SELECT (SELECT AI.GENERATE(s) FROM t) AS a": 0,
	} {
		toks, _ := lex(in)
		if _, code, msg := findAIGenerate(in, toks, tokenDepths(toks)); code != want {
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
