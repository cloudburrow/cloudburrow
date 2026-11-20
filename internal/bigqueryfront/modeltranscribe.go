package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
//   - Without SPEECH_RECOGNIZER, recognition_config is required: it
//     configures the default recognizer. With it (a recognizer created on
//     the instance's local Speech-to-Text API,
//     internal/service/vertexai/speechrecognizers.go, checked to exist at
//     CREATE MODEL), recognition_config is optional and, when given,
//     overrides the recognizer's default_recognition_config field by
//     field, as the page's example "override the recognizer's default
//     configuration" does. Either way the model must be chirp, as the page
//     says, and the config is checked as the recognize route checks it
//     (internal/speechconfig): what has no local equivalent is refused 501
//     before anything is read.
//   - The page's locations alone: asia-southeast1, europe-west4,
//     us-central1, US and EU; the query runs in the model's location.
//   - A file longer than 30 minutes is its row's error, as the page's
//     "INVALID_ARGUMENT: The audio file cannot be processed in time."
//
// UNVERIFIED (re-read 2026-11-19: the page shows them only in its sample
// output, truncated, or not at all): whether recognition_config may be
// given with SPEECH_RECOGNIZER (the page says "If you specify
// SPEECH_RECOGNIZER, you can't specify a RECOGNITION_CONFIG value", yet its
// example does, to override the recognizer's configuration; CloudBurrow
// follows the example); ml_transcribe_result's shape, written as the sample's is,
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
	effective := raw
	if rec := md.Remote.Recognizer; rec != "" {
		merged, code, msg := f.recognizerConfig(r.Context(), rec, raw)
		if code != 0 {
			return "", code, msg
		}
		effective = merged
	} else if raw == "" {
		return "", 400, fname + " requires recognition_config when the remote model specifies no SPEECH_RECOGNIZER: " +
			"recognition_config => (JSON '{\"model\": \"chirp\", \"language_codes\": [...], \"auto_decoding_config\": {}}')"
	}
	cfg, code, err := speechconfig.Parse([]byte(effective))
	switch {
	case code == 501:
		return "", 501, err.Error() + ". Nothing was run."
	case code != 0:
		return "", 400, "Invalid recognition_config: " + err.Error()
	case cfg.Model != "chirp":
		return "", 400, "Invalid recognition_config: only the chirp transcription model can be used in the speech recognizer or recognition_config, not " + strconv.Quote(cfg.Model)
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
			rec := md.Remote.Recognizer
			if rec == "" {
				rec = "projects/" + md.Project + "/locations/" + loc + "/recognizers/_"
			}
			transcripts, res, status = f.transcribe(r.Context(), rec, raw, o.data)
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
func (f front) transcribe(ctx context.Context, recognizer, config string, data []byte) (string, []byte, string) {
	msg := map[string]any{"content": base64.StdEncoding.EncodeToString(data)}
	if config != "" {
		msg["config"] = json.RawMessage(config)
	}
	body, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	u := strings.TrimSuffix(f.vertexai, "/") + "/v2/" + recognizer + ":recognize"
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

// recognizerPattern is SPEECH_RECOGNIZER's documented format.
var recognizerPattern = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/recognizers/[^/]+$`)

// speechRecognizer checks a SPEECH_RECOGNIZER at CREATE MODEL: its format,
// and that the recognizer exists, active, on the instance's local
// Speech-to-Text API.
func (f front) speechRecognizer(ctx context.Context, name string) (int, string) {
	if !recognizerPattern.MatchString(name) || strings.HasSuffix(name, "/recognizers/_") {
		return 400, "Invalid SPEECH_RECOGNIZER " + strconv.Quote(name) + ": expected projects/PROJECT_NUMBER/locations/LOCATION/recognizers/RECOGNIZER_ID"
	}
	_, code, msg := f.getRecognizer(ctx, name)
	return code, msg
}

// getRecognizer reads a recognizer from the local Speech-to-Text API: its
// JSON, or 400 and why it cannot be used.
func (f front) getRecognizer(ctx context.Context, name string) (map[string]any, int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(f.vertexai, "/")+"/v2/"+name, nil)
	if err != nil {
		return nil, 500, err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 503, "A retryable error occurred: the local Speech-to-Text endpoint did not answer: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	var rec map[string]any
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, 400, "The speech recognizer " + name + " doesn't exist"
	case resp.StatusCode != http.StatusOK || json.Unmarshal(b, &rec) != nil:
		return nil, 500, fmt.Sprintf("Cannot read the speech recognizer %s: the local Speech-to-Text endpoint answered %d", name, resp.StatusCode)
	case rec["state"] == "DELETED":
		return nil, 400, "The speech recognizer " + name + " is deleted"
	}
	return rec, 0, ""
}

// recognizerConfig is a recognizer's default_recognition_config with
// recognition_config, if any, merged over it field by field, as JSON.
func (f front) recognizerConfig(ctx context.Context, name, raw string) (string, int, string) {
	rec, code, msg := f.getRecognizer(ctx, name)
	if code != 0 {
		return "", code, msg
	}
	base, _ := rec["defaultRecognitionConfig"].(map[string]any)
	if base == nil {
		base = map[string]any{}
	}
	if raw != "" {
		var over any
		if json.Unmarshal([]byte(raw), &over) != nil {
			return "", 400, "Invalid recognition_config: it is not a JSON object"
		}
		m, ok := speechconfig.CamelKeys(over).(map[string]any)
		if !ok {
			return "", 400, "Invalid recognition_config: it is not a JSON object"
		}
		mergeJSONObjects(base, m)
	}
	b, _ := json.Marshal(base)
	return string(b), 0, ""
}

// mergeJSONObjects sets each field of src over dst, merging objects.
func mergeJSONObjects(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeJSONObjects(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
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
