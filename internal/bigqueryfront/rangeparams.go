package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// RANGE query parameters (#1111).
//
// The emulator reads a RANGE parameter as a NULL INTEGER (measured through
// the front with the official Go client), and its engine reads a RANGE the
// query builds as BigQuery does (measured: RANGE(CAST(@a AS DATE),
// CAST(@b AS DATE)), RANGE(@t, CAST(NULL AS TIMESTAMP)), CAST(NULL AS
// RANGE<DATE>) and RANGE<DATE> '[UNBOUNDED, UNBOUNDED)' read back typed
// RANGE values, so did they in an ARRAY and a STRUCT). So a parameter whose
// type has a RANGE in it, at the top level, in an ARRAY or in a STRUCT, is
// sent as the expression that builds its value, each reference to it in
// the query replaced by it: every scalar of its value (a leaf) is sent as a
// parameter of its own type, @cloudburrow_s<position>_<n>, and the RANGEs,
// STRUCTs and ARRAYs around them as typed constructors, in the value's
// order: RANGE(start, end), an UNBOUNDED bound a typed NULL, both UNBOUNDED
// as that literal, as RANGE(NULL, NULL) fails in the emulator ("at least
// one of start or end must be non-NULL", measured); a NULL RANGE, STRUCT or
// leaf a typed NULL; an ARRAY given no values the empty ARRAY of its type.
// The bounds stay parameters rather than text in a RANGE literal, so a
// value is never read as SQL. A query with positional parameters is sent
// with named ones, @cloudburrow_p and the position, as a named parameter
// can be referred to twice. The references are found by the lexer the
// front reads queries with (lex), so text in a string literal or a comment
// is not changed. jobs.insert's answer, jobs.get and jobs.list show the
// client's text and parameters (jobTexts). A type the front does not know
// in such a parameter is 501, naming it, before anything runs.
//
// Every other parameter goes to the engine as the client sent it. The
// front used to send BYTES (#1078), NUMERIC, BIGNUMERIC, DATE, DATETIME,
// TIME, INTERVAL, GEOGRAPHY and JSON parameters (#1108) as STRINGs in a
// conversion, and STRUCTs (#1082) built from their leaves as RANGEs are,
// because the emulator typed them STRING; its engine now binds each as a
// value of its declared type, NULLs, ARRAYs' elements and STRUCTs' fields
// too (bigquery-emulator patches 0098 and 0100, googlesqlite 0097 and
// 0099; #1814, #1816), and refuses a BYTES value that is not base64, 400.

// rangeParameters returns q with its parameters whose type has a RANGE
// sent as above, and whether it changed q; or, with code set, why it is
// refused.
func rangeParameters(q queryOptions) (out queryOptions, changed bool, code int, msg string) {
	if len(q.QueryParameters) == 0 || q.UseLegacySQL != nil && *q.UseLegacySQL {
		return q, false, 0, ""
	}
	var params []map[string]any
	dec := json.NewDecoder(bytes.NewReader(q.QueryParameters))
	dec.UseNumber()
	if dec.Decode(&params) != nil {
		return q, false, 0, ""
	}
	type rebuiltParam struct {
		name string
		// expr is the parameter's rebuilt value, which each reference is
		// replaced by.
		expr string
	}
	var leaves []any
	rebuilt := map[int]bool{}
	var found []rebuiltParam
	positional := strings.EqualFold(q.ParameterMode, "POSITIONAL")
	if q.ParameterMode == "" {
		positional = true
		for _, p := range params {
			if name, _ := p["name"].(string); name != "" {
				positional = false
			}
		}
	}
	for i, p := range params {
		name, _ := p["name"].(string)
		ptype, _ := p["parameterType"].(map[string]any)
		if !hasRange(ptype) {
			continue
		}
		label := name
		if label == "" {
			label = fmt.Sprintf("at position %d", i+1)
		}
		pval, _ := p["parameterValue"].(map[string]any)
		b := structBuilder{prefix: fmt.Sprintf("cloudburrow_s%d_", i+1)}
		expr, code, msg := b.value(ptype, pval, label)
		if code != 0 {
			return q, false, code, msg
		}
		leaves = append(leaves, b.params...)
		rebuilt[i] = true
		found = append(found, rebuiltParam{name: name, expr: expr})
		if positional {
			found[len(found)-1].name = fmt.Sprintf("cloudburrow_p%d", i+1)
		}
	}
	if len(found) == 0 {
		return q, false, 0, ""
	}
	toks, ok := lex(q.Query)
	if !ok {
		return q, false, 0, ""
	}
	var b strings.Builder
	last := 0
	if positional {
		// Every ? becomes @cloudburrow_p<position>, and the parameters are
		// named so.
		n := 0
		for _, t := range toks {
			if t.punct("?") {
				n++
				b.WriteString(q.Query[last:t.pos])
				fmt.Fprintf(&b, "@cloudburrow_p%d", n)
				last = t.end
			}
		}
		if n != len(params) {
			return q, false, 0, "" // the emulator's answer stands
		}
		b.WriteString(q.Query[last:])
		for i, p := range params {
			p["name"] = fmt.Sprintf("cloudburrow_p%d", i+1)
		}
		q.ParameterMode = "NAMED"
		q.Query, b, last = b.String(), strings.Builder{}, 0
		if toks, ok = lex(q.Query); !ok {
			return q, false, 0, ""
		}
	}
	for i := 0; i+1 < len(toks); i++ {
		t, next := toks[i], toks[i+1]
		if !t.punct("@") || next.pos != t.end || next.kind != tokWord && next.kind != tokQuoted ||
			i > 0 && toks[i-1].punct("@") && toks[i-1].end == t.pos {
			continue
		}
		for _, p := range found {
			if !strings.EqualFold(next.text, p.name) {
				continue
			}
			b.WriteString(q.Query[last:t.pos])
			b.WriteString("(" + p.expr + ")")
			last = next.end
			i++
			break
		}
	}
	b.WriteString(q.Query[last:])
	sent := make([]any, 0, len(params)+len(leaves))
	for i, p := range params {
		if !rebuilt[i] {
			sent = append(sent, p)
		}
	}
	sent = append(sent, leaves...)
	raw, err := json.Marshal(sent)
	if err != nil {
		return q, false, 0, ""
	}
	q.Query, q.QueryParameters = b.String(), raw
	return q, true, 0, ""
}

// hasRange reports whether t, a QueryParameterType, is a RANGE or has one
// in it, as an ARRAY's element type or a STRUCT's field, at any depth.
func hasRange(t map[string]any) bool {
	typ, _ := t["type"].(string)
	switch strings.ToUpper(typ) {
	case "RANGE":
		return true
	case "ARRAY":
		elem, _ := t["arrayType"].(map[string]any)
		return hasRange(elem)
	case "STRUCT":
		fields, _ := t["structTypes"].([]any)
		for _, f := range fields {
			fm, _ := f.(map[string]any)
			ft, _ := fm["type"].(map[string]any)
			if hasRange(ft) {
				return true
			}
		}
	}
	return false
}

// structBuilder writes a parameter's value as the expression the front
// sends in its place (above), and collects the parameters of its leaves,
// each named prefix and a number.
type structBuilder struct {
	prefix string
	params []any
}

// leafTypes are the scalar types a leaf can be, each sent as a parameter
// of its own type.
var leafTypes = map[string]bool{
	"INT64": true, "FLOAT64": true, "BOOL": true, "STRING": true, "TIMESTAMP": true, "BYTES": true, "NUMERIC": true,
	"BIGNUMERIC": true, "DATE": true, "DATETIME": true, "TIME": true, "INTERVAL": true, "GEOGRAPHY": true, "JSON": true,
}

// scalarTypeNames are the GoogleSQL names of a QueryParameterType's legacy
// scalar type names.
var scalarTypeNames = map[string]string{"INTEGER": "INT64", "FLOAT": "FLOAT64", "BOOLEAN": "BOOL", "BIGDECIMAL": "BIGNUMERIC",
	"DECIMAL": "NUMERIC"}

// typeSQL returns the GoogleSQL type of a QueryParameterType, or why the
// front does not rebuild it.
func typeSQL(t map[string]any) (string, string) {
	typ, _ := t["type"].(string)
	typ = strings.ToUpper(typ)
	if n, ok := scalarTypeNames[typ]; ok {
		typ = n
	}
	switch typ {
	case "ARRAY":
		elem, _ := t["arrayType"].(map[string]any)
		e, why := typeSQL(elem)
		if why != "" {
			return "", why
		}
		return "ARRAY<" + e + ">", ""
	case "STRUCT":
		fields, _ := t["structTypes"].([]any)
		if len(fields) == 0 {
			return "", "a STRUCT with no fields"
		}
		parts := make([]string, len(fields))
		for i, f := range fields {
			fm, _ := f.(map[string]any)
			ft, _ := fm["type"].(map[string]any)
			ftype, why := typeSQL(ft)
			if why != "" {
				return "", why
			}
			parts[i] = ftype
			if name, _ := fm["name"].(string); name != "" {
				parts[i] = quoteName(name) + " " + ftype
			}
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">", ""
	case "RANGE":
		elem, _ := t["rangeElementType"].(map[string]any)
		e, _ := elem["type"].(string)
		switch e = strings.ToUpper(e); e {
		case "DATE", "DATETIME", "TIMESTAMP":
			return "RANGE<" + e + ">", ""
		}
		return "", "a RANGE whose element type is not DATE, DATETIME or TIMESTAMP"
	}
	if !leafTypes[typ] {
		if typ == "" {
			return "", "a field with no type"
		}
		return "", "a " + typ + " field"
	}
	return typ, ""
}

// value returns the expression for a value v (a QueryParameterValue, nil
// for NULL) of type t, or the status and message to refuse the query
// with; label names the value in a message.
func (b *structBuilder) value(t, v map[string]any, label string) (string, int, string) {
	typ, why := typeSQL(t)
	if why != "" {
		return "", http.StatusNotImplemented, "Not implemented here: the query parameter " + label + ", whose type has " +
			why + ". The emulator behind CloudBurrow reads a RANGE query parameter as a NULL INTEGER (measured, #1111); " +
			"CloudBurrow sends such a parameter built from its values, but not with that type. Nothing was run."
	}
	kind, _ := t["type"].(string)
	switch strings.ToUpper(kind) {
	case "RANGE":
		rv, ok := v["rangeValue"].(map[string]any)
		if !ok {
			return "CAST(NULL AS " + typ + ")", 0, ""
		}
		elem, _ := t["rangeElementType"].(map[string]any)
		start, _ := rv["start"].(map[string]any)
		end, _ := rv["end"].(map[string]any)
		if boundValue(start) == nil && boundValue(end) == nil {
			// RANGE(NULL, NULL) fails in the emulator ("at least one of
			// start or end must be non-NULL", measured); BigQuery's
			// value is [UNBOUNDED, UNBOUNDED).
			return typ + " '[UNBOUNDED, UNBOUNDED)'", 0, ""
		}
		s, code, msg := b.value(elem, start, label+".start")
		if code != 0 {
			return "", code, msg
		}
		e, code, msg := b.value(elem, end, label+".end")
		if code != 0 {
			return "", code, msg
		}
		return "RANGE(" + s + ", " + e + ")", 0, ""
	case "STRUCT":
		sv, ok := v["structValues"].(map[string]any)
		if !ok {
			return "CAST(NULL AS " + typ + ")", 0, ""
		}
		fields, _ := t["structTypes"].([]any)
		parts := make([]string, len(fields))
		for i, f := range fields {
			fm, _ := f.(map[string]any)
			ft, _ := fm["type"].(map[string]any)
			name, _ := fm["name"].(string)
			fv, _ := sv[name].(map[string]any)
			e, code, msg := b.value(ft, fv, label+"."+name)
			if code != 0 {
				return "", code, msg
			}
			parts[i] = e
		}
		return typ + "(" + strings.Join(parts, ", ") + ")", 0, ""
	case "ARRAY":
		elem, _ := t["arrayType"].(map[string]any)
		elems, _ := v["arrayValues"].([]any)
		parts := make([]string, len(elems))
		for i, e := range elems {
			em, _ := e.(map[string]any)
			x, code, msg := b.value(elem, em, fmt.Sprintf("%s[%d]", label, i))
			if code != 0 {
				return "", code, msg
			}
			parts[i] = x
		}
		return typ + "[" + strings.Join(parts, ", ") + "]", 0, ""
	}
	val, ok := v["value"]
	if !ok || val == nil {
		return "CAST(NULL AS " + typ + ")", 0, ""
	}
	name := fmt.Sprintf("%s%d", b.prefix, len(b.params)+1)
	b.params = append(b.params, map[string]any{"name": name, "parameterType": map[string]any{"type": typ},
		"parameterValue": map[string]any{"value": val}})
	return "@" + name, 0, ""
}

// boundValue returns a RANGE bound's value (a QueryParameterValue), or nil
// when the bound is UNBOUNDED: no value, or a NULL one.
func boundValue(v map[string]any) any {
	if v == nil {
		return nil
	}
	return v["value"]
}

// runRangeQuery runs q, whose RANGE parameters the front sends as client's
// were not (rangeParameters), and answers with the client's text and
// parameters in the job it names.
func (f front) runRangeQuery(w http.ResponseWriter, r *http.Request, client, q queryOptions, insert bool) {
	if !setQueryParameters(r, insert, q) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	rec := newRecorder()
	f.runQuery(rec, r, q, insert)
	f.clientJob(w, rec, jobText{query: client.Query, params: client.QueryParameters, paramMode: client.ParameterMode,
		paramsSet: true})
}

// setQueryParameters sets the query text, parameterMode and
// queryParameters in r's body: jobs.query's, or (insert) a query job's
// configuration.query.
func setQueryParameters(r *http.Request, insert bool, q queryOptions) bool {
	b, err := readBody(r)
	if err != nil {
		return false
	}
	body, ok := decodeMap(b)
	if !ok {
		return false
	}
	target := body
	if insert {
		conf, _ := body["configuration"].(map[string]any)
		target, _ = conf["query"].(map[string]any)
		if target == nil {
			return false
		}
	}
	var params any
	if err := json.Unmarshal(q.QueryParameters, &params); err != nil {
		return false
	}
	target["query"], target["queryParameters"] = q.Query, params
	if q.ParameterMode != "" {
		target["parameterMode"] = q.ParameterMode
	}
	out, err := json.Marshal(body)
	if err != nil {
		return false
	}
	setBody(r, out)
	return true
}

// clientJob answers w with rec, the answer to a query the front sent
// otherwise than the client did, with the client's part of it, t, in the
// job it names, which jobs.get and jobs.list then show too (jobTexts); t
// is added to what the front already keeps of the job.
func (f front) clientJob(w http.ResponseWriter, rec *recorder, t jobText) {
	var resp map[string]any
	if f.texts == nil || json.Unmarshal(rec.body.Bytes(), &resp) != nil {
		rec.copyTo(w)
		return
	}
	project, id := jobRef(resp)
	if project == "" {
		project = projectOf(f.base)
	}
	if id != "" {
		kept, _ := f.texts.get(project, id)
		kept.merge(t)
		f.texts.add(project, id, kept)
	}
	if _, ok := resp["configuration"]; ok {
		t.patch(resp)
		if b, err := json.Marshal(resp); err == nil {
			rec.body.Reset()
			rec.body.Write(b)
		}
	}
	rec.copyTo(w)
}
