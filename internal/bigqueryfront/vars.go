package bigqueryfront

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// renameVariables gives each script variable a script's DECLAREs make a
// name of its own, unique to this request, and returns the text and the
// names it gave, each mapped to the name as the client wrote it (#933).
//
// The emulator's SQL engine (goccy/googlesqlite v0.3.1, in the pinned
// image) keeps a script's variables after the script, on the one
// connection every query of the instance runs on, and replaces each bare
// word that names one, in every later query, with its value: measured,
// after `DECLARE zz INT64 DEFAULT 5; SELECT zz`, a separate `SELECT zz AS
// v` returned 5, and after `DECLARE w INT64 DEFAULT 9`, `SELECT 1 AS w`
// failed "Syntax error: Unexpected integer literal "9"". BigQuery scopes a
// variable to its script: the later query fails "Unrecognized name: zz",
// and the alias is an alias. A variable of a unique name is still kept by
// the engine, but no later query names it.
//
// The words renamed are the ones the engine would replace with the value
// (its applyScriptVariables and substituteScriptVariables): the name after
// DECLARE in a statement that starts with it (only the first of a list, as
// the engine records only that one), and after that statement, each bare
// word equal to it, ignoring case, that does not follow a "." and is not
// followed by "(" (spaces or tabs between), outside strings and quoted
// names. So the engine replaces the same words with the same value, and
// the script runs as it did.
func renameVariables(sql string) (string, map[string]string) {
	if !strings.Contains(strings.ToUpper(sql), "DECLARE") {
		return sql, nil
	}
	toks, ok := lex(sql)
	if !ok {
		return sql, nil
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	tag := hex.EncodeToString(suffix)
	vars := map[string]string{}  // lower-case name → unique name
	names := map[string]string{} // unique name → name as declared
	type edit struct {
		pos, end int
		text     string
	}
	var edits []edit
	refs := func(stmt []token) {
		for _, t := range stmt {
			if t.kind != tokWord || !engineIdent(t.text) {
				continue
			}
			u, ok := vars[strings.ToLower(t.text)]
			if !ok || t.pos > 0 && sql[t.pos-1] == '.' {
				continue
			}
			k := t.end
			for k < len(sql) && (sql[k] == ' ' || sql[k] == '\t') {
				k++
			}
			if k < len(sql) && sql[k] == '(' {
				continue
			}
			edits = append(edits, edit{t.pos, t.end, u})
		}
	}
	for _, stmt := range splitStatements(toks) {
		if len(stmt) > 1 && stmt[0].is("DECLARE") && stmt[1].kind == tokWord && engineIdent(stmt[1].text) &&
			(stmt[1].pos == 0 || sql[stmt[1].pos-1] != '.') {
			lower := strings.ToLower(stmt[1].text)
			u, ok := vars[lower]
			if !ok {
				u = "cbvar_" + tag + "_" + lower
				names[u] = stmt[1].text
			}
			edits = append(edits, edit{stmt[1].pos, stmt[1].end, u})
			// The DEFAULT expression is read with the variables declared
			// before this one.
			refs(stmt[2:])
			vars[lower] = u
			continue
		}
		refs(stmt)
	}
	if len(edits) == 0 {
		return sql, nil
	}
	var b strings.Builder
	last := 0
	for _, e := range edits {
		b.WriteString(sql[last:e.pos])
		b.WriteString(e.text)
		last = e.end
	}
	b.WriteString(sql[last:])
	return b.String(), names
}

// engineIdent reports whether the engine reads word as one identifier: an
// ASCII letter or underscore, then ASCII letters, digits and underscores.
func engineIdent(word string) bool {
	for i := 0; i < len(word); i++ {
		c := word[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return word != ""
}

// unname puts back the names renameVariables gave in an answer: an error
// that names a variable names it as the client wrote it.
func unname(b []byte, names map[string]string) []byte {
	for u, orig := range names {
		b = bytes.ReplaceAll(b, []byte(u), []byte(orig))
	}
	return b
}
