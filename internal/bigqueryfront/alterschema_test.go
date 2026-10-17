package bigqueryfront

import (
	"encoding/json"
	"testing"
)

// TestAlterSchemaPatch (#1522): ALTER SCHEMA … SET OPTIONS is the
// datasets.patch of the options it sets: a value as CREATE SCHEMA reads it,
// NULL clearing the field, and labels replacing the dataset's.
func TestAlterSchemaPatch(t *testing.T) {
	sql := "ALTER SCHEMA ds SET OPTIONS (max_time_travel_hours = 72, description = NULL, labels = [('a', 'x')])"
	toks, ok := lex(sql)
	if !ok {
		t.Fatal("lex")
	}
	synth := append(append([]token{}, toks[0:3]...), toks[4:]...) // without SET
	d, msg, bad := schemaOptions(synth, "ds")
	if msg != "" || bad != "" {
		t.Fatalf("schemaOptions: %q %q", msg, bad)
	}
	patch, ok := alterPatch(toks[6:], d, map[string]string{"a": "old", "b": "gone"})
	if !ok {
		t.Fatal("alterPatch refused the options")
	}
	got, _ := json.Marshal(patch)
	if want := `{"description":null,"labels":{"a":"x","b":null},"maxTimeTravelHours":"72"}`; string(got) != want {
		t.Errorf("patch %s, want %s", got, want)
	}
}
