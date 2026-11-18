package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The output schema of AI.GENERATE's output_schema argument, AI.AGG's
// and AI.GENERATE_TABLE's output_schema option (#1771), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-ai-generate
// and bigqueryml-syntax-generate-table document it (read 2026-11-17): a
// STRING of comma-separated fields, each a name, a type (STRING, INT64,
// FLOAT64, BOOL, ARRAY or STRUCT), an optional NOT NULL and an optional
// OPTIONS clause holding a description and an `enum` of allowed values.
//
// Google sends the schema to Gemini as a response schema, so its output
// is constrained. The local LiteRT-LM runtime's litert_lm_main accepts
// --constraint_regex and ignores it (measured 2026-11-17 on the pinned
// 02e50307 build: `(true|false)` and `-?[0-9]+` left the answers
// unchanged; upstream's litert_lm_main.cc never reads the flag, and only
// litert_lm_advanced_main's --use_session path applies it, without the
// model's chat template). So the front asks for the schema in words
// appended to the prompt (aiSchemaInstruction), parses the model's text
// and, when it does not fit, gives the row a NULL value and a status
// saying why (aiParseStatus), as BigQuery reports a row's failure.
//
// UNVERIFIED (re-read 2026-11-17: the two pages above, which document none
// of these): the words of the
// instruction (Google's response schema is not text the model sees); the
// status of an answer that does not parse (with constrained decoding,
// Google's never fails to); the error texts of a schema that does not
// parse.

// aiType is an output schema's type.
type aiType struct {
	kind   string // STRING, INT64, FLOAT64, BOOL, ARRAY or STRUCT
	elem   *aiType
	fields []aiField
}

// aiField is one field of an output schema.
type aiField struct {
	name    string
	typ     aiType
	notNull bool
	desc    string
	enum    []string
}

// sql is the type's GoogleSQL name.
func (t aiType) sql() string {
	switch t.kind {
	case "ARRAY":
		return "ARRAY<" + t.elem.sql() + ">"
	case "STRUCT":
		var fs []string
		for _, f := range t.fields {
			fs = append(fs, sqlIdent(f.name)+" "+f.typ.sql())
		}
		return "STRUCT<" + strings.Join(fs, ", ") + ">"
	}
	return t.kind
}

// parseOutputSchema reads an output_schema STRING.
func parseOutputSchema(s string) ([]aiField, string) {
	toks, ok := lex(s)
	if !ok || len(toks) == 0 {
		return nil, "Invalid output_schema: " + strconv.Quote(s)
	}
	p := &schemaParser{toks: toks}
	fields, msg := p.fields(func() bool { return p.i >= len(p.toks) })
	if msg != "" {
		return nil, "Invalid output_schema " + strconv.Quote(s) + ": " + msg
	}
	if p.i < len(p.toks) {
		return nil, "Invalid output_schema " + strconv.Quote(s) + ": unexpected " + p.toks[p.i].text
	}
	return fields, ""
}

type schemaParser struct {
	toks []token
	i    int
}

func (p *schemaParser) peek() token {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return token{}
}

func (p *schemaParser) fields(done func() bool) ([]aiField, string) {
	var out []aiField
	seen := map[string]bool{}
	for {
		t := p.peek()
		if t.kind != tokWord && t.kind != tokQuoted || t.text == "" {
			return nil, "expected a field name"
		}
		p.i++
		f := aiField{name: t.text}
		if seen[strings.ToLower(f.name)] {
			return nil, "duplicate field " + f.name
		}
		seen[strings.ToLower(f.name)] = true
		typ, msg := p.typ()
		if msg != "" {
			return nil, msg
		}
		f.typ = typ
		if p.peek().is("NOT") {
			p.i++
			if !p.peek().is("NULL") {
				return nil, "expected NULL after NOT"
			}
			p.i++
			f.notNull = true
		}
		if p.peek().is("OPTIONS") {
			p.i++
			if msg := p.options(&f); msg != "" {
				return nil, msg
			}
		}
		out = append(out, f)
		if done() {
			return out, ""
		}
		if !p.peek().punct(",") {
			return nil, "expected , after field " + f.name
		}
		p.i++
	}
}

func (p *schemaParser) typ() (aiType, string) {
	t := p.peek()
	if t.kind != tokWord {
		return aiType{}, "expected a type"
	}
	p.i++
	switch k := strings.ToUpper(t.text); k {
	case "STRING", "INT64", "FLOAT64", "BOOL":
		return aiType{kind: k}, ""
	case "ARRAY":
		if !p.peek().punct("<") {
			return aiType{}, "expected < after ARRAY"
		}
		p.i++
		e, msg := p.typ()
		if msg != "" {
			return aiType{}, msg
		}
		if e.kind == "ARRAY" {
			return aiType{}, "an ARRAY of ARRAY is not a valid type"
		}
		if !p.peek().punct(">") {
			return aiType{}, "expected > closing ARRAY"
		}
		p.i++
		return aiType{kind: "ARRAY", elem: &e}, ""
	case "STRUCT":
		if !p.peek().punct("<") {
			return aiType{}, "expected < after STRUCT"
		}
		p.i++
		fs, msg := p.fields(func() bool { return p.peek().punct(">") })
		if msg != "" {
			return aiType{}, msg
		}
		p.i++
		return aiType{kind: "STRUCT", fields: fs}, ""
	default:
		return aiType{}, "unsupported type " + t.text + ": an output schema takes STRING, INT64, FLOAT64, BOOL, ARRAY and STRUCT"
	}
}

func (p *schemaParser) options(f *aiField) string {
	if !p.peek().punct("(") {
		return "expected ( after OPTIONS"
	}
	end := matchParen(p.toks, p.i)
	if end < 0 {
		return "unclosed OPTIONS("
	}
	p.i++
	for p.i < end {
		name := p.peek()
		if name.kind != tokWord && name.kind != tokQuoted {
			return "expected an option name"
		}
		p.i++
		if !p.peek().punct("=") {
			return "expected = after " + name.text
		}
		p.i++
		switch strings.ToLower(name.text) {
		case "description":
			s, ok := unquoteString(p.peek().text)
			if p.peek().kind != tokString || !ok {
				return "description must be a STRING literal"
			}
			f.desc = s
			p.i++
		case "enum":
			if !p.peek().punct("[") {
				return "enum must be an ARRAY of STRING literals"
			}
			p.i++
			for !p.peek().punct("]") {
				s, ok := unquoteString(p.peek().text)
				if p.peek().kind != tokString || !ok {
					return "enum must be an ARRAY of STRING literals"
				}
				f.enum = append(f.enum, s)
				p.i++
				if p.peek().punct(",") {
					p.i++
				}
			}
			p.i++
		default:
			return "unknown option " + name.text + ": a field takes description and enum"
		}
		if p.peek().punct(",") {
			p.i++
		}
	}
	p.i = end + 1
	return ""
}

// describeSchema writes the fields as words for the model.
func describeOutputSchema(fields []aiField) string {
	var b strings.Builder
	var walk func(fs []aiField, indent string)
	walk = func(fs []aiField, indent string) {
		for _, f := range fs {
			fmt.Fprintf(&b, "%s- %q: %s", indent, f.name, jsonTypeName(f.typ))
			if f.notNull {
				b.WriteString(", required")
			}
			if f.desc != "" {
				b.WriteString(", " + f.desc)
			}
			if len(f.enum) > 0 {
				b.WriteString(", one of: " + strings.Join(f.enum, ", "))
			}
			b.WriteString("\n")
			t := f.typ
			if t.kind == "ARRAY" {
				t = *t.elem
			}
			if t.kind == "STRUCT" {
				walk(t.fields, indent+"  ")
			}
		}
	}
	walk(fields, "")
	return b.String()
}

func jsonTypeName(t aiType) string {
	switch t.kind {
	case "STRING":
		return "string"
	case "INT64":
		return "integer"
	case "FLOAT64":
		return "number"
	case "BOOL":
		return "boolean (true or false)"
	case "ARRAY":
		return "array of " + jsonTypeName(*t.elem)
	}
	return "object with these keys"
}

// aiSchemaInstruction is what the front appends to a prompt with an
// output schema (UNVERIFIED, re-read 2026-11-17: see the top of this file).
func aiSchemaInstruction(fields []aiField) string {
	return "\n\nAnswer with only a JSON object, and nothing before or after it, with these keys:\n" + describeOutputSchema(fields)
}

// jsonBody is the JSON object or value in a model's answer: the text with
// any Markdown code fence removed.
func jsonBody(text string) string {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if k := strings.IndexByte(s, '\n'); k >= 0 {
			s = s[k+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// schemaLiterals are the fields' SQL literals of a model's answer, or
// why it does not fit.
func schemaLiterals(fields []aiField, text string) ([]string, string) {
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(jsonBody(text)))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, "The model's response is not a JSON object"
	}
	return fieldLiterals(fields, obj)
}

func fieldLiterals(fields []aiField, obj map[string]json.RawMessage) ([]string, string) {
	lower := map[string]json.RawMessage{}
	for k, v := range obj {
		lower[strings.ToLower(k)] = v
	}
	var out []string
	for _, f := range fields {
		raw, ok := obj[f.name]
		if !ok {
			raw, ok = lower[strings.ToLower(f.name)]
		}
		if !ok || string(bytes.TrimSpace(raw)) == "null" {
			if f.notNull {
				return nil, "The model's response has no value for the NOT NULL field " + f.name
			}
			out = append(out, "CAST(NULL AS "+f.typ.sql()+")")
			continue
		}
		lit, msg := valueLiteral(f.typ, raw)
		if msg != "" {
			return nil, "Field " + f.name + ": " + msg
		}
		if len(f.enum) > 0 {
			var s string
			if json.Unmarshal(raw, &s) != nil || !aiContains(f.enum, s) {
				return nil, "Field " + f.name + ": " + string(raw) + " is not one of its enum values"
			}
		}
		out = append(out, lit)
	}
	return out, ""
}

func aiContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// valueLiteral is raw as a SQL literal of t.
func valueLiteral(t aiType, raw json.RawMessage) (string, string) {
	raw = bytes.TrimSpace(raw)
	if string(raw) == "null" {
		return "CAST(NULL AS " + t.sql() + ")", ""
	}
	switch t.kind {
	case "STRING":
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", "expected a string, got " + string(raw)
		}
		return sqlString(s), ""
	case "INT64":
		n, ok := aiJSONInt(raw)
		if !ok {
			return "", "expected an integer, got " + string(raw)
		}
		return strconv.FormatInt(n, 10), ""
	case "FLOAT64":
		v, ok := aiJSONFloat(raw)
		if !ok {
			return "", "expected a number, got " + string(raw)
		}
		return sqlF(v), ""
	case "BOOL":
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return "", "expected true or false, got " + string(raw)
		}
		return strings.ToUpper(strconv.FormatBool(b)), ""
	case "ARRAY":
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return "", "expected an array, got " + string(raw)
		}
		var lits []string
		for _, it := range items {
			if string(bytes.TrimSpace(it)) == "null" {
				return "", "an array element is null, which an ARRAY cannot hold"
			}
			l, msg := valueLiteral(*t.elem, it)
			if msg != "" {
				return "", msg
			}
			lits = append(lits, l)
		}
		return t.sql() + "[" + strings.Join(lits, ", ") + "]", ""
	case "STRUCT":
		var obj map[string]json.RawMessage
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&obj) != nil {
			return "", "expected an object, got " + string(raw)
		}
		lits, msg := fieldLiterals(t.fields, obj)
		if msg != "" {
			return "", msg
		}
		return t.sql() + "(" + strings.Join(lits, ", ") + ")", ""
	}
	return "", "unsupported type " + t.kind
}

func aiJSONInt(raw json.RawMessage) (int64, bool) {
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	if i, err := n.Int64(); err == nil {
		return i, true
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

func aiJSONFloat(raw json.RawMessage) (float64, bool) {
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

// scalarAnswer reads a one-value answer: the text with surrounding
// whitespace, quotes, Markdown emphasis and a final full stop removed.
func scalarAnswer(text string) string {
	s := jsonBody(text)
	s = strings.Trim(s, " \t\r\n*`\"'")
	s = strings.TrimSuffix(s, ".")
	return strings.TrimSpace(s)
}

// parseBoolAnswer, parseIntAnswer and parseFloatAnswer read a model's
// one-value answer as a SQL literal, or say why it is not one.
func parseBoolAnswer(text string) (string, string) {
	switch strings.ToLower(scalarAnswer(text)) {
	case "true":
		return "TRUE", ""
	case "false":
		return "FALSE", ""
	}
	return "", "The model's response is not a BOOL: " + strconv.Quote(strings.TrimSpace(text))
}

func parseIntAnswer(text string) (string, string) {
	s := scalarAnswer(text)
	if n, ok := aiJSONInt(json.RawMessage(s)); ok {
		return strconv.FormatInt(n, 10), ""
	}
	return "", "The model's response is not an INT64: " + strconv.Quote(strings.TrimSpace(text))
}

func parseFloatAnswer(text string) (string, string) {
	s := scalarAnswer(text)
	if v, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(v, 0) && !math.IsNaN(v) {
		return sqlF(v), ""
	}
	return "", "The model's response is not a FLOAT64: " + strconv.Quote(strings.TrimSpace(text))
}
