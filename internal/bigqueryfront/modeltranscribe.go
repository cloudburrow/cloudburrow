package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/speechconfig"
)

// ML.TRANSCRIBE over a CLOUD_AI_SPEECH_TO_TEXT_V2 remote model (#1799), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-transcribe
// documents it (read 2026-11-18):
//
//	ML.TRANSCRIBE(MODEL m, TABLE object_table
//	  [, RECOGNITION_CONFIG => (JSON 'recognition_config')])
//
// Each audio object of the object table is read at its row's generation and
// sent, as a Speech-to-Text v2 RecognizeRequest on the default recognizer,
// to the instance's local Vertex AI endpoint, whose recognize route
// transcribes it with whisper.cpp and the Whisper base model
// (internal/service/vertexai/speech.go); never to Google. The output is
// transcripts (STRING), ml_transcribe_result (JSON, the
// RecognizeResponse) and ml_transcribe_status (STRING, empty on success,
// else the row's error), then the object table's columns.
//
//   - The model has no recognizer (SPEECH_RECOGNIZER is refused 501 at
//     CREATE MODEL: CloudBurrow serves no recognizers), so
//     recognition_config is required, and its model must be chirp, as the
//     page says of the default recognizer. The rest of the config is
//     checked as the recognize route checks it (internal/speechconfig):
//     what has no local equivalent is refused 501 before anything is read.
//   - The page's locations alone: asia-southeast1, europe-west4,
//     us-central1, US and EU; the query runs in the model's location.
//   - A file longer than 30 minutes is its row's error, as the page's
//     "INVALID_ARGUMENT: The audio file cannot be processed in time."
//
// UNVERIFIED (re-read 2026-11-18: the page shows them only in its sample
// output, truncated, or not at all): ml_transcribe_result's shape, written as the sample's is,
// with snake_case keys and durations as {"seconds", "nanos"} objects;
// transcripts as the results' first alternatives joined with a space; the
// texts of the errors.

// transcribeLocations are ML.TRANSCRIBE's documented locations.
var transcribeLocations = map[string]bool{"asia-southeast1": true, "europe-west4": true, "us-central1": true, "us": true, "eu": true}

// recognitionConfigText reads recognition_config's value: (JSON '...'),
// JSON '...' or a string literal.
func recognitionConfigText(v string) (string, bool) {
	toks, ok := lex(v)
	if !ok {
		return "", false
	}
	for len(toks) >= 2 && toks[0].punct("(") && toks[len(toks)-1].punct(")") {
		toks = toks[1 : len(toks)-1]
	}
	if len(toks) == 2 && toks[0].is("JSON") {
		toks = toks[1:]
	}
	if len(toks) != 1 || toks[0].kind != tokString {
		return "", false
	}
	return unquoteString(toks[0].text)
}

func (f front) transcribeSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall, fname string) (string, int, string) {
	for k := range c.struc {
		return "", 400, "Unknown option " + k + " for " + fname
	}
	raw := ""
	for k, v := range c.named {
		if k != "recognition_config" {
			return "", 400, "Unknown argument " + k + " of " + fname
		}
		s, ok := recognitionConfigText(v)
		if !ok {
			return "", 400, "recognition_config of " + fname + " must be a JSON literal: recognition_config => (JSON '{...}')"
		}
		raw = s
	}
	if raw == "" {
		return "", 400, fname + " requires recognition_config when the remote model specifies no SPEECH_RECOGNIZER: " +
			"recognition_config => (JSON '{\"model\": \"chirp\", \"language_codes\": [...], \"auto_decoding_config\": {}}')"
	}
	cfg, code, err := speechconfig.Parse([]byte(raw))
	switch {
	case code == 501:
		return "", 501, err.Error() + ". Nothing was run."
	case code != 0:
		return "", 400, "Invalid recognition_config: " + err.Error()
	case cfg.Model != "chirp":
		return "", 400, "Invalid recognition_config: only the chirp transcription model can be used with the default recognizer, not " + strconv.Quote(cfg.Model)
	}
	if !transcribeLocations[strings.ToLower(md.Remote.Location)] {
		return "", 400, fmt.Sprintf("%s is not supported in location %s: it runs in asia-southeast1, europe-west4, us-central1, US and EU", fname, md.Remote.Location)
	}
	src, objects, code, msg := f.serviceObjects(r, q, md, c, fname)
	if code != 0 {
		return "", code, msg
	}
	loc := strings.ToLower(md.Remote.Location)
	var lits []string
	for _, o := range objects {
		var res []byte
		transcripts, status := "", o.status
		if o.data != nil {
			transcripts, res, status = f.transcribe(r.Context(), md.Project, loc, raw, o.data)
		}
		t := "NULL"
		if res != nil {
			t = sqlString(transcripts)
		}
		lits = append(lits, "STRUCT("+sqlString(o.uri)+", "+t+", "+jsonLiteral(res)+", "+sqlString(status)+")")
	}
	return "(SELECT _cbml_p.transcripts AS transcripts, _cbml_p.ml_transcribe_result AS ml_transcribe_result, " +
		"_cbml_p.ml_transcribe_status AS ml_transcribe_status, _cbml_in.* FROM " + src + " AS _cbml_in LEFT JOIN " +
		"UNNEST(ARRAY<STRUCT<_cbml_k STRING, transcripts STRING, ml_transcribe_result JSON, ml_transcribe_status STRING>>[" +
		strings.Join(lits, ", ") + "]) AS _cbml_p ON _cbml_in.uri = _cbml_p._cbml_k)", 0, ""
}

// transcribe sends one audio file to the local endpoint's recognize route:
// the row's transcripts, its ml_transcribe_result and its status.
func (f front) transcribe(ctx context.Context, project, location, config string, data []byte) (string, []byte, string) {
	body, _ := json.Marshal(map[string]any{"config": json.RawMessage(config), "content": base64.StdEncoding.EncodeToString(data)})
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	u := strings.TrimSuffix(f.vertexai, "/") + "/v2/projects/" + project + "/locations/" + location + "/recognizers/_:recognize"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", nil, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, "A retryable error occurred: the local Speech-to-Text endpoint did not answer: " + err.Error()
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", nil, err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error.Message != "" {
			if e.Error.Status != "" {
				return "", nil, e.Error.Status + ": " + e.Error.Message
			}
			return "", nil, e.Error.Message
		}
		return "", nil, fmt.Sprintf("the local Speech-to-Text endpoint answered %d", resp.StatusCode)
	}
	var rr struct {
		Results []struct {
			Alternatives []struct {
				Transcript string `json:"transcript"`
			} `json:"alternatives"`
		} `json:"results"`
	}
	var generic any
	if json.Unmarshal(out, &rr) != nil || json.Unmarshal(out, &generic) != nil {
		return "", nil, "the local Speech-to-Text endpoint answered an unreadable response"
	}
	var parts []string
	for _, res := range rr.Results {
		if len(res.Alternatives) > 0 && res.Alternatives[0].Transcript != "" {
			parts = append(parts, res.Alternatives[0].Transcript)
		}
	}
	result, _ := json.Marshal(snakeResult(generic, ""))
	return strings.Join(parts, " "), result, ""
}

// durationKeys are the RecognizeResponse fields that are Durations.
var durationKeys = map[string]bool{"totalBilledDuration": true, "resultEndOffset": true, "startOffset": true, "endOffset": true}

// snakeResult rewrites a RecognizeResponse's JSON as ml_transcribe_result
// shows it: snake_case keys, and each Duration ("1.5s") as {"seconds": 1,
// "nanos": 500000000}, nanos omitted when zero.
func snakeResult(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[snakeCase(k)] = snakeResult(e, k)
		}
		return out
	case []any:
		for i := range x {
			x[i] = snakeResult(x[i], "")
		}
		return x
	case string:
		if durationKeys[key] && strings.HasSuffix(x, "s") {
			if d, err := time.ParseDuration(x); err == nil {
				m := map[string]any{"seconds": int64(d / time.Second)}
				if n := int64(d % time.Second); n != 0 {
					m["nanos"] = n
				}
				return m
			}
		}
	}
	return v
}
