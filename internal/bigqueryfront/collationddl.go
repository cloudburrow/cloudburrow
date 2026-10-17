package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Collation specifications and where they go (#1508).
//
// BigQuery's collation reference
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/collation-concepts):
// "und:ci and empty collation are supported, but not other collation
// specifications". The engine would take any name and compare by code
// point; the front refuses the others, 400, wherever they are given: a
// query's COLLATE 'x' (a column, a table's or a schema's DEFAULT COLLATE)
// or COLLATE(expr, 'x'), a Table resource's defaultCollation or field
// collation, a Dataset resource's defaultCollation. BigQuery documents no
// error text; the message here is CloudBurrow's (UNVERIFIED wording).
//
// A STRING inside a STRUCT or an ARRAY can carry a collation in BigQuery
// ("A STRUCT can have STRING fields with different collation
// specifications"). The engine gives a collation only to a top-level
// STRING column (its analyzer has no nested column annotations), so such
// a collation is refused 501, Not implemented here, before anything is
// run or made, rather than kept in the schema and ignored.

// collationSpecRefused returns BigQuery's refusal of spec, or "" for the
// two it supports.
func collationSpecRefused(spec string) string {
	if spec == "" || spec == "und:ci" {
		return ""
	}
	return fmt.Sprintf("Invalid collation specification '%s': only 'und:ci' and '' are supported", spec)
}

// nestedCollationRefused is the 501 for a collation on a STRING inside a
// STRUCT or an ARRAY; what names it.
func nestedCollationRefused(what string) string {
	return "Not implemented here: a collation on " + what + ". BigQuery supports it; the engine behind " +
		"CloudBurrow compares only a top-level STRING column by its collation, so the collation would be " +
		"kept and ignored. Nothing was run."
}

// queryCollationRefusal returns the code, reason and message that refuse
// sql, a GoogleSQL query or script, for a collation (above), or code 0.
func queryCollationRefusal(sql string) (int, string, string) {
	if !strings.Contains(strings.ToUpper(sql), "COLLATE") {
		return 0, "", ""
	}
	t, ok := lex(sql)
	if !ok {
		return 0, "", ""
	}
	nest := 0 // open STRUCT< and ARRAY< of a type
	for i := 0; i < len(t); i++ {
		switch {
		case (t[i].is("STRUCT") || t[i].is("ARRAY")) && i+1 < len(t) && t[i+1].punct("<"):
			nest++
			i++
		case nest > 0 && t[i].punct(">"):
			nest--
		case t[i].is("COLLATE") && i+1 < len(t) && t[i+1].kind == tokString:
			if nest > 0 {
				return http.StatusNotImplemented, "notImplemented", nestedCollationRefused("a STRING inside a STRUCT or an ARRAY type")
			}
			if spec, ok := stringLiteral(t[i+1]); ok {
				if msg := collationSpecRefused(spec); msg != "" {
					return http.StatusBadRequest, "invalidQuery", msg
				}
			}
		case t[i].is("COLLATE") && i+1 < len(t) && t[i+1].punct("("): // the COLLATE function
			end := closing(t, i+1)
			if end < 0 {
				continue
			}
			last, depth := i+1, 0
			for j := i + 2; j < end; j++ {
				switch {
				case t[j].punct("(") || t[j].punct("["):
					depth++
				case t[j].punct(")") || t[j].punct("]"):
					depth--
				case depth == 0 && t[j].punct(","):
					last = j
				}
			}
			if last > i+1 && end == last+2 && t[last+1].kind == tokString {
				if spec, ok := stringLiteral(t[last+1]); ok {
					if msg := collationSpecRefused(spec); msg != "" {
						return http.StatusBadRequest, "invalidQuery", msg
					}
				}
			}
		}
	}
	return 0, "", ""
}

// collationField is a TableFieldSchema's part a collation check reads.
type collationField struct {
	Name      string           `json:"name"`
	Type      string           `json:"type"`
	Mode      string           `json:"mode"`
	Collation string           `json:"collation"`
	Fields    []collationField `json:"fields"`
}

// tableCollationRefusal returns the code, reason and message that refuse
// raw, a Table resource, for a collation (above), or code 0.
func tableCollationRefusal(raw []byte) (int, string, string) {
	if !bytes.Contains(raw, []byte("ollation")) {
		return 0, "", ""
	}
	var body struct {
		DefaultCollation string `json:"defaultCollation"`
		Schema           struct {
			Fields []collationField `json:"fields"`
		} `json:"schema"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return 0, "", ""
	}
	if msg := collationSpecRefused(body.DefaultCollation); msg != "" {
		return http.StatusBadRequest, "invalid", msg
	}
	var walk func(fields []collationField, nested bool, prefix string) (int, string, string)
	walk = func(fields []collationField, nested bool, prefix string) (int, string, string) {
		for _, f := range fields {
			if msg := collationSpecRefused(f.Collation); msg != "" {
				return http.StatusBadRequest, "invalid", msg
			}
			if f.Collation != "" && (nested || strings.EqualFold(f.Mode, "REPEATED")) {
				return http.StatusNotImplemented, "notImplemented", nestedCollationRefused("the field " + prefix + f.Name +
					", a STRING inside a RECORD or a REPEATED STRING")
			}
			if code, reason, msg := walk(f.Fields, true, prefix+f.Name+"."); code != 0 {
				return code, reason, msg
			}
		}
		return 0, "", ""
	}
	return walk(body.Schema.Fields, false, "")
}

// datasetCollationRefused returns the refusal of raw, a Dataset
// resource, for its defaultCollation (above), or "".
func datasetCollationRefused(raw []byte) string {
	if !bytes.Contains(raw, []byte("defaultCollation")) {
		return ""
	}
	var body struct {
		DefaultCollation string `json:"defaultCollation"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return collationSpecRefused(body.DefaultCollation)
}

// tableDefaultCollate returns the literal of a CREATE TABLE's own DEFAULT
// COLLATE (named true), and the byte offset after its column list, or
// after its name when it has none, where a DEFAULT COLLATE clause goes
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#create_table_statement:
// the column list, then DEFAULT COLLATE, then PARTITION BY, CLUSTER BY,
// OPTIONS and AS query). ok is false for a statement it cannot read.
func tableDefaultCollate(sql string) (spec string, named bool, at int, ok bool) {
	t, lexed := lex(sql)
	if !lexed {
		return "", false, 0, false
	}
	i := 0
	for i < len(t) && !t[i].is("TABLE") {
		i++
	}
	if i++; i+2 < len(t) && t[i].is("IF") && t[i+1].is("NOT") && t[i+2].is("EXISTS") {
		i += 3
	}
	parts, next := path(t, i)
	if len(parts) == 0 || next <= i {
		return "", false, 0, false
	}
	at = t[next-1].end
	if next < len(t) && t[next].punct("(") {
		end := closing(t, next)
		if end < 0 {
			return "", false, 0, false
		}
		at, next = t[end].end, end+1
	}
	depth := 0
	for j := next; j < len(t); j++ {
		switch {
		case t[j].punct("("):
			depth++
		case t[j].punct(")"):
			depth--
		case depth == 0 && t[j].is("AS"):
			return "", false, at, true // the query's own COLLATE is not the table's
		case depth == 0 && t[j].is("DEFAULT") && j+2 < len(t) && t[j+1].is("COLLATE"):
			s, isLit := stringLiteral(t[j+2])
			return s, isLit, at, isLit
		}
	}
	return "", false, at, true
}
