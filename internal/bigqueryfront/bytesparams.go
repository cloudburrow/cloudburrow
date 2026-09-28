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
// BigQuery refuses it. A STRUCT parameter with a BYTES field, at any
// depth, is 501: the front does not rebuild a STRUCT.

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
	}
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
		if kind == "nested" {
			return q, false, http.StatusNotImplemented, "Not implemented here: the query parameter " + label + ", a STRUCT " +
				"with a BYTES field. The emulator behind CloudBurrow types a BYTES query parameter as STRING (measured, " +
				"#1078); CloudBurrow sends a BYTES or ARRAY<BYTES> parameter so that it is BYTES, but not one inside a " +
				"STRUCT. Nothing was run."
		}
		pval, _ := p["parameterValue"].(map[string]any)
		array := kind == "array"
		if array {
			ptype["arrayType"] = map[string]any{"type": "STRING"}
			elems, _ := pval["arrayValues"].([]any)
			for j, e := range elems {
				em, _ := e.(map[string]any)
				if msg := normalBase64(em, fmt.Sprintf("%s[%d]", label, j)); msg != "" {
					return q, false, http.StatusBadRequest, msg
				}
			}
		} else {
			ptype["type"] = "STRING"
			if msg := normalBase64(pval, label); msg != "" {
				return q, false, http.StatusBadRequest, msg
			}
		}
		found = append(found, byteParam{name: name, array: array})
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
			if p.array {
				b.WriteString("IF(" + ref + " IS NULL, NULL, ARRAY(SELECT FROM_BASE64(_cloudburrow_e) FROM UNNEST(" + ref +
					") AS _cloudburrow_e WITH OFFSET AS _cloudburrow_o ORDER BY _cloudburrow_o))")
			} else {
				b.WriteString("FROM_BASE64(" + ref + ")")
			}
			last = next.end
			i++
			break
		}
	}
	b.WriteString(q.Query[last:])
	raw, err := json.Marshal(params)
	if err != nil {
		return q, false, 0, ""
	}
	q.Query, q.QueryParameters = b.String(), raw
	return q, true, 0, ""
}

// bytesKind returns "scalar" for a BYTES parameter type, "array" for
// ARRAY<BYTES>, "nested" for a type with BYTES inside a STRUCT, or "".
func bytesKind(t map[string]any) string {
	typ, _ := t["type"].(string)
	switch strings.ToUpper(typ) {
	case "BYTES":
		return "scalar"
	case "ARRAY":
		elem, _ := t["arrayType"].(map[string]any)
		switch bytesKind(elem) {
		case "scalar":
			return "array"
		case "":
			return ""
		}
		return "nested"
	case "STRUCT":
		fields, _ := t["structTypes"].([]any)
		for _, f := range fields {
			fm, _ := f.(map[string]any)
			ft, _ := fm["type"].(map[string]any)
			if bytesKind(ft) != "" {
				return "nested"
			}
		}
	}
	return ""
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
