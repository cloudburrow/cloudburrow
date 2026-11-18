package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ML.UNDERSTAND_TEXT over a CLOUD_AI_NATURAL_LANGUAGE_V1 remote model
// (#1799), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-understand-text
// documents it (read 2026-11-18, "Last updated 2026-11-10"):
//
//	ML.UNDERSTAND_TEXT(MODEL m, { TABLE t | (query) },
//	  STRUCT('OPTION_NAME' AS nlu_option
//	    [, FLATTEN_JSON_OUTPUT AS flatten_json_output]
//	    [, ENCODING_TYPE AS encoding_type]))
//
// The input's text_content column (STRING) is sent, one distinct text at a
// time, to the instance's local Vertex AI endpoint's Natural Language v1
// route (internal/service/vertexai/language.go), never to Google. The
// options are the page's five: ANALYZE_ENTITIES, ANALYZE_ENTITY_SENTIMENT,
// ANALYZE_SENTIMENT and CLASSIFY_TEXT, answered by the local Gemma model,
// and ANALYZE_SYNTAX, which has no local tagger and parser and is 501.
// encoding_type is NONE, UTF8, UTF16 or UTF32, refused with CLASSIFY_TEXT
// as the page says.
//
// The output is ml_understand_text_result (JSON, the response with
// snake_case keys, as the tutorial bigquery/docs/understand-text shows
// document_sentiment and begin_offset), or with flatten_json_output TRUE
// the page's columns for the option (entities, language, sentiment,
// sentences, tokens, categories); then ml_understand_text_status (STRING,
// empty on success, else the row's error) and the input's columns. A NULL
// text_content row has no result and no status.
//
// UNVERIFIED (re-read 2026-11-18): the order of the flattened columns (the
// page's list order is used); that sentiment is the response's
// document_sentiment; the texts of the errors; that option names are
// matched case-insensitively (the page's examples write classify_text).

// understandOptions are the page's nlu_option values, and the flattened
// columns of each.
var understandOptions = map[string][]string{
	"ANALYZE_ENTITIES":         {"entities", "language"},
	"ANALYZE_ENTITY_SENTIMENT": {"entities", "language"},
	"ANALYZE_SENTIMENT":        {"language", "sentiment", "sentences"},
	"ANALYZE_SYNTAX":           {"language", "sentences", "tokens"},
	"CLASSIFY_TEXT":            {"categories"},
}

// understandVerbs are the Natural Language v1 methods of the options.
var understandVerbs = map[string]string{
	"ANALYZE_ENTITIES": "analyzeEntities", "ANALYZE_ENTITY_SENTIMENT": "analyzeEntitySentiment",
	"ANALYZE_SENTIMENT": "analyzeSentiment", "ANALYZE_SYNTAX": "analyzeSyntax", "CLASSIFY_TEXT": "classifyText",
}

func (f front) understandSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall, fname string) (string, int, string) {
	rm := md.Remote
	for k := range c.named {
		return "", 400, "Unknown argument " + k + " of " + fname
	}
	if c.input == "" {
		return "", 400, fname + " requires an input: a query in parentheses or TABLE name"
	}
	option, encoding, flatten := "", "", false
	for k, v := range c.struc {
		s, ok := unquoteString(v)
		switch k {
		case "nlu_option":
			if !ok {
				return "", 400, "nlu_option of " + fname + " must be a STRING literal"
			}
			option = strings.ToUpper(s)
		case "encoding_type":
			if !ok {
				return "", 400, "encoding_type of " + fname + " must be a STRING literal"
			}
			encoding = strings.ToUpper(s)
		case "flatten_json_output":
			switch strings.ToUpper(strings.TrimSpace(v)) {
			case "TRUE":
				flatten = true
			case "FALSE":
			default:
				return "", 400, "flatten_json_output of " + fname + " must be a BOOL literal"
			}
		default:
			return "", 400, "Unknown option " + k + " for " + fname
		}
	}
	switch {
	case option == "":
		return "", 400, fname + " requires STRUCT('ANALYZE_ENTITIES' | 'ANALYZE_ENTITY_SENTIMENT' | 'ANALYZE_SENTIMENT' | 'ANALYZE_SYNTAX' | 'CLASSIFY_TEXT' AS nlu_option)"
	case understandOptions[option] == nil:
		return "", 400, "Invalid nlu_option " + option + " for " + fname + ": expected ANALYZE_ENTITIES, ANALYZE_ENTITY_SENTIMENT, ANALYZE_SENTIMENT, ANALYZE_SYNTAX or CLASSIFY_TEXT"
	case encoding != "" && encoding != "NONE" && encoding != "UTF8" && encoding != "UTF16" && encoding != "UTF32":
		return "", 400, "Invalid encoding_type " + encoding + " for " + fname + ": expected NONE, UTF8, UTF16 or UTF32"
	case encoding != "" && option == "CLASSIFY_TEXT":
		return "", 400, "encoding_type of " + fname + " cannot be used with CLASSIFY_TEXT"
	}
	cn, ok := f.remoteConnection(rm)
	if !ok {
		return "", 404, "Not found: Connection " + rm.Connection
	}
	if code, msg := f.checkConnectionUse(r.Context(), md.Project, md.Project, cn); code != 0 {
		return "", code, msg
	}
	cols, _, code, msg := f.queryAllRows(r, q, "SELECT * FROM ("+c.input+") LIMIT 0")
	if code != 0 {
		return "", code, msg
	}
	var found *mlColumn
	for i := range cols {
		if strings.EqualFold(cols[i].Name, "text_content") {
			found = &cols[i]
		}
	}
	if found == nil {
		return "", 400, "Invalid table-valued function " + fname + ": the input must have a column named text_content"
	}
	if !strings.EqualFold(found.Type, "STRING") {
		return "", 400, fmt.Sprintf("Invalid table-valued function %s: column %s must be of type STRING, not %s", fname, found.Name, found.Type)
	}
	key := "_cbml_in." + sqlIdent(found.Name)
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT DISTINCT "+key+" FROM ("+c.input+") AS _cbml_in WHERE "+key+" IS NOT NULL")
	if code != 0 {
		return "", code, msg
	}
	// The output's columns: the result (or the flattened fields), the status.
	type outCol struct{ name, typ string }
	var outs []outCol
	if flatten {
		for _, n := range understandOptions[option] {
			t := "JSON"
			if n == "language" {
				t = "STRING"
			}
			outs = append(outs, outCol{n, t})
		}
	} else {
		outs = []outCol{{"ml_understand_text_result", "JSON"}}
	}
	outs = append(outs, outCol{"ml_understand_text_status", "STRING"})
	var lits []string
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		res, status, code := f.understand(r.Context(), md.Project, option, encoding, *row[0])
		if code == 501 {
			return "", 501, "Not implemented here: " + fname + " with nlu_option " + option + ": " + status + ". Nothing was returned."
		}
		vals := []string{sqlString(*row[0])}
		if flatten {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(res, &m)
			for _, oc := range outs[:len(outs)-1] {
				v, ok := m[map[string]string{"sentiment": "document_sentiment"}[oc.name]]
				if !ok {
					v = m[oc.name]
				}
				switch {
				case oc.typ == "STRING":
					var s *string
					_ = json.Unmarshal(v, &s)
					if s == nil {
						vals = append(vals, "CAST(NULL AS STRING)")
					} else {
						vals = append(vals, sqlString(*s))
					}
				case len(v) == 0 || res == nil:
					vals = append(vals, jsonLiteral(nil))
				default:
					vals = append(vals, jsonLiteral(v))
				}
			}
		} else {
			vals = append(vals, jsonLiteral(res))
		}
		vals = append(vals, sqlString(status))
		lits = append(lits, "STRUCT("+strings.Join(vals, ", ")+")")
	}
	inSel := "_cbml_in.*"
	var except []string
	for _, ic := range cols {
		for _, oc := range outs {
			if strings.EqualFold(ic.Name, oc.name) {
				except = append(except, sqlIdent(ic.Name))
			}
		}
	}
	if len(except) > 0 {
		inSel += " EXCEPT (" + strings.Join(except, ", ") + ")"
	}
	var sel, fields []string
	for _, oc := range outs {
		sel = append(sel, "_cbml_p."+oc.name+" AS "+oc.name)
		fields = append(fields, oc.name+" "+oc.typ)
	}
	return "(SELECT " + strings.Join(sel, ", ") + ", " + inSel +
		" FROM (" + c.input + ") AS _cbml_in LEFT JOIN UNNEST(ARRAY<STRUCT<_cbml_k STRING, " + strings.Join(fields, ", ") + ">>[" +
		strings.Join(lits, ", ") + "]) AS _cbml_p ON " + key + " = _cbml_p._cbml_k)", 0, ""
}

// understand sends one text to the local endpoint's Natural Language
// route: the row's ml_understand_text_result (snake_case) and status, and
// 501 when the endpoint cannot serve the option at all.
func (f front) understand(ctx context.Context, project, option, encoding, text string) ([]byte, string, int) {
	req := map[string]any{"document": map[string]any{"type": "PLAIN_TEXT", "content": text}}
	if encoding != "" {
		req["encodingType"] = encoding
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	u := strings.TrimSuffix(f.vertexai, "/") + "/v1/documents:" + understandVerbs[option]
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err.Error(), 0
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return nil, "A retryable error occurred: the local Natural Language endpoint did not answer: " + err.Error(), 0
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err.Error(), 0
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		msg := fmt.Sprintf("the local Natural Language endpoint answered %d", resp.StatusCode)
		if json.Unmarshal(out, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
			if e.Error.Status != "" {
				msg = e.Error.Status + ": " + msg
			}
		}
		if resp.StatusCode == http.StatusNotImplemented {
			return nil, e.Error.Message, 501
		}
		return nil, msg, 0
	}
	var generic map[string]any
	d := json.NewDecoder(bytes.NewReader(out))
	d.UseNumber() // keep the API's float32 digits (0.51999998)
	if d.Decode(&generic) != nil {
		return nil, "the local Natural Language endpoint answered an unreadable response", 0
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(snakeResult(generic, ""))
	return bytes.TrimSpace(b.Bytes()), "", 0
}
