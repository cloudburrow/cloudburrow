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

// ML.TRANSLATE over a CLOUD_AI_TRANSLATE_V3 remote model (#1799), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-translate
// documents it (read 2026-11-18):
//
//	ML.TRANSLATE(MODEL m, { TABLE t | (query) },
//	  STRUCT('MODE_NAME' AS translate_mode [, 'TARGET_LANGUAGE_CODE' AS target_language_code]))
//
// The input's text_content column (STRING) is sent, one distinct text at a
// time, to the instance's local Vertex AI endpoint's Translation v3 route
// (internal/service/vertexai/translate.go): translateText for
// TRANSLATE_TEXT, detectLanguage for DETECT_LANGUAGE; never to Google. The
// output is ml_translate_result (JSON, the response, with snake_case keys
// as the page's sample shows) and ml_translate_status (STRING, empty on
// success, else the row's error), then the input's columns.
//
//   - Languages are detected by MediaPipe's Language Detector and texts
//     translated by the instance's local Gemma model with TranslateGemma's
//     prompt; with no local model, TRANSLATE_TEXT is 501 and
//     DETECT_LANGUAGE still runs.
//   - A NULL text_content row has no result and no status.
//
// UNVERIFIED (re-read 2026-11-18: the page shows TRANSLATE_TEXT's result in
// its sample alone): DETECT_LANGUAGE's result shape, written as
// DetectLanguageResponse in snake_case ({"languages": [{"confidence",
// "language_code"}]}); the texts of the errors; that modes and option names
// are matched case-insensitively (the page writes them both ways).

// translateModes are ML.TRANSLATE's documented modes.
var translateModes = map[string]bool{"TRANSLATE_TEXT": true, "DETECT_LANGUAGE": true}

func (f front) translateSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall, fname string) (string, int, string) {
	rm := md.Remote
	for k := range c.named {
		return "", 400, "Unknown argument " + k + " of " + fname
	}
	if c.input == "" {
		return "", 400, fname + " requires an input: a query in parentheses or TABLE name"
	}
	mode, target := "", ""
	for k, v := range c.struc {
		s, ok := unquoteString(v)
		switch k {
		case "translate_mode":
			if !ok {
				return "", 400, "translate_mode of " + fname + " must be a STRING literal"
			}
			mode = strings.ToUpper(s)
		case "target_language_code":
			if !ok {
				return "", 400, "target_language_code of " + fname + " must be a STRING literal"
			}
			target = s
		default:
			return "", 400, "Unknown option " + k + " for " + fname
		}
	}
	switch {
	case mode == "":
		return "", 400, fname + " requires STRUCT('TRANSLATE_TEXT' | 'DETECT_LANGUAGE' AS translate_mode)"
	case !translateModes[mode]:
		return "", 400, "Invalid translate_mode " + mode + " for " + fname + ": expected TRANSLATE_TEXT or DETECT_LANGUAGE"
	case mode == "TRANSLATE_TEXT" && target == "":
		return "", 400, fname + " requires target_language_code in TRANSLATE_TEXT mode"
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
	loc := strings.ToLower(rm.Location)
	var lits []string
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		res, status, code := f.translate(r.Context(), md.Project, loc, mode, target, *row[0])
		if code == 501 {
			return "", 501, "Not implemented here: " + fname + " in " + mode + " mode: " + status + ". Nothing was returned."
		}
		lits = append(lits, "STRUCT("+sqlString(*row[0])+", "+jsonLiteral(res)+", "+sqlString(status)+")")
	}
	inSel := "_cbml_in.*"
	var except []string
	for _, ic := range cols {
		if strings.EqualFold(ic.Name, "ml_translate_result") || strings.EqualFold(ic.Name, "ml_translate_status") {
			except = append(except, sqlIdent(ic.Name))
		}
	}
	if len(except) > 0 {
		inSel += " EXCEPT (" + strings.Join(except, ", ") + ")"
	}
	return "(SELECT _cbml_p.ml_translate_result AS ml_translate_result, _cbml_p.ml_translate_status AS ml_translate_status, " + inSel +
		" FROM (" + c.input + ") AS _cbml_in LEFT JOIN UNNEST(ARRAY<STRUCT<_cbml_k STRING, ml_translate_result JSON, ml_translate_status STRING>>[" +
		strings.Join(lits, ", ") + "]) AS _cbml_p ON " + key + " = _cbml_p._cbml_k)", 0, ""
}

// translate sends one text to the local endpoint's Translation route: the
// row's ml_translate_result and status, and 501 when the endpoint cannot
// serve the mode at all.
func (f front) translate(ctx context.Context, project, location, mode, target, text string) ([]byte, string, int) {
	verb := "detectLanguage"
	req := map[string]any{"content": text, "mimeType": "text/plain"}
	if mode == "TRANSLATE_TEXT" {
		verb = "translateText"
		req = map[string]any{"contents": []string{text}, "mimeType": "text/plain", "targetLanguageCode": target}
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	u := strings.TrimSuffix(f.vertexai, "/") + "/v3/projects/" + project + "/locations/" + location + ":" + verb
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err.Error(), 0
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return nil, "A retryable error occurred: the local Translation endpoint did not answer: " + err.Error(), 0
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
		msg := fmt.Sprintf("the local Translation endpoint answered %d", resp.StatusCode)
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
	if json.Unmarshal(out, &generic) != nil {
		return nil, "the local Translation endpoint answered an unreadable response", 0
	}
	if mode == "TRANSLATE_TEXT" {
		// The page's sample: TranslateTextResponse's repeated fields, empty
		// ones included.
		for _, k := range []string{"glossaryTranslations", "translationMemoryTranslations"} {
			if _, ok := generic[k]; !ok {
				generic[k] = []any{}
			}
		}
	}
	result, _ := json.Marshal(snakeResult(generic, ""))
	return result, "", 0
}
