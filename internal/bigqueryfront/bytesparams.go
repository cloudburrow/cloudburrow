package bigqueryfront

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// BYTES query parameters (#1078).
//
// BigQuery takes a BYTES parameter's value as base64 ("BYTES ... base64
// encoded", QueryParameterValue.value), and the parameter is BYTES in the
// query. The emulator hands its engine the value as a Go string (its
// source, internal/contentdata/repository.go, queryParameterValueToGoValue
// and coerceScalarParameterValue, which keep a BYTES value's text), and
// the engine types a string as STRING. Measured through the front with the
// official Go client, on the pinned image: INSERT INTO ds.t (id, b) VALUES
// (1, @p), with @p the BYTES ff 00 61, failed 400 "Value has type STRING
// which cannot be inserted into column b, which has type BYTES", SELECT
// TO_HEX(@p) failed "No matching signature for function TO_HEX ... STRING",
// an ARRAY<BYTES> parameter read back as its base64 texts, and SELECT @p.x
// of a STRUCT with a BYTES field failed "Cannot access field x on a value
// with type STRING". FROM_BASE64 of the same text as a STRING parameter
// read back ff0061, and inserted it as those bytes (measured).
//
// So the front sends each BYTES parameter as a STRING of its base64 text,
// and each reference to it in the query as FROM_BASE64 of it; an
// ARRAY<BYTES> parameter as an ARRAY<STRING>, referred to as the array of
// its elements' FROM_BASE64, in their order (NULL when the array is). A
// query with positional parameters is sent with named ones, @cloudburrow_p
// and the position, as a named parameter can be referred to twice. The
// references are found by the lexer the front reads queries with (lex), so
// text in a string literal or a comment is not changed. jobs.insert's
// answer, jobs.get and jobs.list show the client's text and parameters
// (jobTexts). A value that is not base64 is refused, 400 invalidQuery, as
// BigQuery refuses it.
//
// STRUCT parameters (#1082). The emulator types a STRUCT parameter, and
// an ARRAY of STRUCTs, as one STRING, the JSON text of its value, whatever
// its fields (measured through the front with the official Go client:
// SELECT @p of a STRUCT<X STRING, Y INT64> read back the STRING
// {"X":"/wBh","Y":"2"}, and SELECT @p.X failed "Cannot access field X on
// a value with type STRING"). Its engine types a STRUCT built in the query
// as the STRUCT it is (measured: STRUCT<X BYTES, Y INT64>(FROM_BASE64(@a),
// @b) read back a RECORD of the bytes ff 00 61 and 2, and was inserted
// into a RECORD column so; ARRAY<STRUCT<...>>[...] and ARRAY<...>[] an
// ARRAY of them, CAST(NULL AS STRUCT<...>) a NULL of the type; nested
// STRUCTs and ARRAYs of STRUCTs so too). So the front sends a parameter
// whose type holds a STRUCT as that expression, each reference to it
// replaced by it: every scalar of its value (a leaf) is sent as a
// parameter of its own, @cloudburrow_s<position>_<n>, and the STRUCTs and
// ARRAYs around them as typed constructors, in the value's order, a NULL
// STRUCT or leaf as a typed NULL, and an ARRAY given no values as the
// empty ARRAY of its type (the Go client sends an empty slice so). A leaf
// is sent as its own type where the emulator types a scalar parameter so
// (measured: INT64, FLOAT64, BOOL, STRING, TIMESTAMP read back as sent);
// BYTES as the STRING of its base64 in FROM_BASE64; NUMERIC, BIGNUMERIC,
// DATE, DATETIME, TIME and INTERVAL, which the emulator types STRING (a
// DATETIME TIMESTAMP, measured), as the STRING of its value in CAST(... AS
// type), which read back each as sent (measured); GEOGRAPHY in
// ST_GEOGFROMTEXT and JSON in PARSE_JSON (a CAST of a STRING to either is
// refused, measured). A RANGE, or a type the front does not know, inside
// a STRUCT is 501, naming it, before anything runs.
//
// Top-level scalars (#1108). The emulator types a top-level NUMERIC,
// BIGNUMERIC, DATE, TIME, GEOGRAPHY, JSON and INTERVAL parameter STRING,
// and a DATETIME one TIMESTAMP (measured through the front with the
// official Go client: SELECT @p read back a STRING of the text, and a
// TIMESTAMP), and refused a positional NUMERIC ("strconv.ParseInt:
// parsing ... invalid syntax", measured). So such a parameter is sent as
// a BYTES one is: as the STRING of its value, each reference to it in its
// conversion (convSQL: CAST(... AS type), ST_GEOGFROMTEXT, PARSE_JSON),
// an ARRAY of them as an ARRAY<STRING> read back element by element, and
// positional parameters as named ones. The emulator reads a NULL element
// of an ARRAY<STRING> parameter as '' (measured: a NULL DATE element
// failed "failed to convert  to time.Time"), so an ARRAY with a NULL
// element is built as a STRUCT's ARRAY is, each element a parameter of
// its own and a NULL one a typed NULL.

// bytesParameters returns q with its BYTES parameters sent as above, and
// whether it changed q; or, with code set, why it is refused.
func bytesParameters(q queryOptions) (out queryOptions, changed bool, code int, msg string) {
	if len(q.QueryParameters) == 0 || q.UseLegacySQL != nil && *q.UseLegacySQL {
		return q, false, 0, ""
	}
	var params []map[string]any
	dec := json.NewDecoder(bytes.NewReader(q.QueryParameters))
	dec.UseNumber()
	if dec.Decode(&params) != nil {
		return q, false, 0, ""
	}
	type byteParam struct {
		name  string
		array bool
		// typ is the GoogleSQL type of a scalar parameter, or of an
		// ARRAY's elements, that is sent as a STRING (convType).
		typ string
		// expr is a STRUCT parameter's rebuilt value, which each
		// reference is replaced by (#1082), or "".
		expr string
	}
	var leaves []any
	rebuilt := map[int]bool{}
	var found []byteParam
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
		kind := bytesKind(ptype)
		if kind == "" {
			continue
		}
		label := name
		if label == "" {
			label = fmt.Sprintf("at position %d", i+1)
		}
		pval, _ := p["parameterValue"].(map[string]any)
		if kind == "array" && nullElement(pval) {
			// The emulator reads a NULL element of an ARRAY<STRING>
			// parameter as '' (#1108), so an ARRAY with one is built as
			// a STRUCT's is.
			kind = "struct"
		}
		if kind == "struct" {
			b := structBuilder{prefix: fmt.Sprintf("cloudburrow_s%d_", i+1)}
			expr, code, msg := b.value(ptype, pval, label)
			if code != 0 {
				return q, false, code, msg
			}
			leaves = append(leaves, b.params...)
			rebuilt[i] = true
			found = append(found, byteParam{name: name, expr: expr})
			if positional {
				found[len(found)-1].name = fmt.Sprintf("cloudburrow_p%d", i+1)
			}
			continue
		}
		array := kind == "array"
		scalar := ptype
		if array {
			scalar, _ = ptype["arrayType"].(map[string]any)
		}
		typ := convType(scalar)
		if array {
			ptype["arrayType"] = map[string]any{"type": "STRING"}
			elems, _ := pval["arrayValues"].([]any)
			for j, e := range elems {
				em, _ := e.(map[string]any)
				if typ != "BYTES" {
					continue
				}
				if msg := normalBase64(em, fmt.Sprintf("%s[%d]", label, j)); msg != "" {
					return q, false, http.StatusBadRequest, msg
				}
			}
		} else {
			ptype["type"] = "STRING"
			if typ == "BYTES" {
				if msg := normalBase64(pval, label); msg != "" {
					return q, false, http.StatusBadRequest, msg
				}
			}
		}
		found = append(found, byteParam{name: name, array: array, typ: typ})
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
			ref := q.Query[t.pos:next.end]
			b.WriteString(q.Query[last:t.pos])
			if p.expr != "" {
				b.WriteString("(" + p.expr + ")")
			} else if p.array {
				b.WriteString("IF(" + ref + " IS NULL, NULL, ARRAY(SELECT " + convSQL(p.typ, "_cloudburrow_e") + " FROM UNNEST(" + ref +
					") AS _cloudburrow_e WITH OFFSET AS _cloudburrow_o ORDER BY _cloudburrow_o))")
			} else {
				b.WriteString(convSQL(p.typ, ref))
			}
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

// bytesKind returns "scalar" for the type of a parameter the front sends
// as a STRING (convType: BYTES, and since #1108 NUMERIC, BIGNUMERIC,
// DATE, DATETIME, TIME, INTERVAL, GEOGRAPHY and JSON), "array" for an
// ARRAY of one, "struct" for a STRUCT or an ARRAY of STRUCTs (whatever
// its fields, #1082), or "".
func bytesKind(t map[string]any) string {
	typ, _ := t["type"].(string)
	switch strings.ToUpper(typ) {
	case "ARRAY":
		elem, _ := t["arrayType"].(map[string]any)
		switch bytesKind(elem) {
		case "scalar":
			return "array"
		case "struct":
			return "struct"
		}
	case "STRUCT":
		return "struct"
	}
	if convType(t) != "" {
		return "scalar"
	}
	return ""
}

// nullElement reports whether an ARRAY parameter's value v has a NULL
// element.
func nullElement(v map[string]any) bool {
	elems, _ := v["arrayValues"].([]any)
	for _, e := range elems {
		em, _ := e.(map[string]any)
		if val, ok := em["value"]; !ok || val == nil {
			return true
		}
	}
	return false
}

// convType returns the GoogleSQL name of t, a scalar QueryParameterType,
// when the front sends a parameter of it as a STRING in a conversion
// (leafKinds), or "".
func convType(t map[string]any) string {
	typ, _ := t["type"].(string)
	typ = strings.ToUpper(typ)
	if n, ok := scalarTypeNames[typ]; ok {
		typ = n
	}
	if leafKinds[typ] == "" {
		return ""
	}
	return typ
}

// convSQL returns the expression that reads ref, a STRING, as a value of
// typ, a type convType names (leafKinds).
func convSQL(typ, ref string) string {
	switch how := leafKinds[typ]; how {
	case "bytes":
		return "FROM_BASE64(" + ref + ")"
	case "cast":
		return "CAST(" + ref + " AS " + typ + ")"
	default:
		return how + "(" + ref + ")"
	}
}

// structBuilder writes a STRUCT parameter's value as the expression the
// front sends in its place (above), and collects the parameters of its
// leaves, each named prefix and a number.
type structBuilder struct {
	prefix string
	params []any
}

// leafKinds are how a leaf of each scalar type is sent (above): "" as a
// parameter of its own type, "bytes" as FROM_BASE64 of a STRING, "cast"
// as a CAST of a STRING, or the function that reads it from a STRING.
var leafKinds = map[string]string{
	"INT64": "", "FLOAT64": "", "BOOL": "", "STRING": "", "TIMESTAMP": "",
	"BYTES": "bytes", "NUMERIC": "cast", "BIGNUMERIC": "cast", "DATE": "cast", "DATETIME": "cast", "TIME": "cast",
	"INTERVAL": "cast", "GEOGRAPHY": "ST_GEOGFROMTEXT", "JSON": "PARSE_JSON",
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
	}
	if _, ok := leafKinds[typ]; !ok {
		if typ == "" {
			return "", "a field with no type"
		}
		if typ == "RANGE" {
			// #1111: the emulator has no text of a RANGE value the front
			// can send (CAST(@x AS RANGE<DATE>) read back "unrecognized
			// type" in the Go client, measured).
			return "", "a RANGE field (the emulator reads no RANGE value CloudBurrow can send it: send the range's " +
				"start and end as fields of their own and build it in the query with RANGE(start, end))"
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
			why + " inside a STRUCT. The emulator behind CloudBurrow types a STRUCT query parameter as a STRING of its " +
			"JSON (measured, #1082); CloudBurrow sends a STRUCT parameter built from its values, but not with such a field. " +
			"Nothing was run."
	}
	kind, _ := t["type"].(string)
	switch strings.ToUpper(kind) {
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
	leaf := map[string]any{"value": val}
	sendAs := typ
	how := leafKinds[typ]
	if how != "" {
		sendAs = "STRING"
	}
	if how == "bytes" {
		if msg := normalBase64(leaf, label); msg != "" {
			return "", http.StatusBadRequest, msg
		}
	}
	name := fmt.Sprintf("%s%d", b.prefix, len(b.params)+1)
	b.params = append(b.params, map[string]any{"name": name, "parameterType": map[string]any{"type": sendAs},
		"parameterValue": leaf})
	ref := "@" + name
	if how == "" {
		return ref, 0, ""
	}
	return convSQL(typ, ref), 0, ""
}

// normalBase64 sets v's value, a QueryParameterValue's base64 text, as
// standard base64 (FROM_BASE64 reads it; a client may send it URL-safe),
// or returns why it is not base64. A NULL value is left as it is.
func normalBase64(v map[string]any, label string) string {
	s, ok := v["value"].(string)
	if !ok {
		return ""
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			v["value"] = base64.StdEncoding.EncodeToString(b)
			return ""
		}
	}
	return fmt.Sprintf("Invalid query parameter %s: the value of a BYTES parameter must be base64 encoded, and %q is not.",
		label, s)
}

// runBytesQuery runs q, whose BYTES parameters the front sends as client's
// were not (bytesParameters), and answers with the client's text and
// parameters in the job it names.
func (f front) runBytesQuery(w http.ResponseWriter, r *http.Request, client, q queryOptions, insert bool) {
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
