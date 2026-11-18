package bigqueryfront

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Remote models (#1760): CREATE MODEL … REMOTE WITH CONNECTION … OPTIONS
// (ENDPOINT = …), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-remote-model
// documents it (read 2026-11-17), and the functions of one:
//
//   - ML.GENERATE_TEXT(MODEL m, {TABLE t | (query)} [, STRUCT(...)]): the
//     input's rows with ml_generate_text_result (JSON) and
//     ml_generate_text_status, or, with flatten_json_output,
//     ml_generate_text_llm_result, ml_generate_text_rai_result,
//     ml_generate_text_grounding_result and ml_generate_text_status
//     (bigqueryml-syntax-generate-text);
//   - ML.GENERATE_EMBEDDING(MODEL m, {TABLE t | (query)} [, STRUCT(...)]):
//     ml_generate_embedding_result (ARRAY<FLOAT64>),
//     ml_generate_embedding_statistics (JSON) and
//     ml_generate_embedding_status (bigqueryml-syntax-generate-embedding).
//
// Where it runs: never on Google. The ENDPOINT names a model (a bare name,
// or the last segment of a Vertex AI URL, which is never dialled), and the
// front sends each distinct prompt or content to the instance's own local
// Vertex AI endpoint (internal/service/vertexai), published to the cluster
// through cloudburrow-host and given to the front with --vertexai:
// generation on the local LiteRT-LM runtime (`up --local-ai-model`) and
// embeddings on the local embedding model (`--local-ai-embeddings`). The
// local endpoint's own rule holds: a model it does not serve, such as a
// gemini-* endpoint with no alias configured, is refused, never
// substituted; the refusal is the row's status, as a Vertex AI error is
// in BigQuery.
//
// The connection must exist in the dataset's location (the Connection API
// of connections.go), the caller must hold bigquery.connections.use on it
// and its service account roles/aiplatform.user on the project
// (connectionauth.go, #1765).
//
// The generation options (#1797) are sent as the request's
// generationConfig, with BigQuery's documented ranges and Gemini defaults
// (generationOptions): max_output_tokens [1,8192] default 1024,
// temperature [0.0,1.0] default 0, top_p [0.0,1.0] default 0.95,
// stop_sequences; top_k [1,40] for a non-Gemini endpoint (BigQuery
// documents none for Gemini). The local runtime applies them through
// cloudburrow_generate. Options it cannot honour are still refused 501
// rather than ignored (docs/generation.md): safety_settings,
// generation_config and model_params; ground_with_google_search is
// refused because it searches Google.
//
// UNVERIFIED (re-read 2026-11-17:
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-generate-text,
// which gives none of these texts): the
// wording of the errors for a missing prompt or content column and for
// an unknown option.

// remoteModel is what a remote model keeps.
type remoteModel struct {
	// Connection is projects/{number}/locations/{location}/connections/{id}.
	Connection string `json:"connection"`
	// Endpoint is the ENDPOINT option as written; Model the model it names.
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	// Embedding is true for an embedding model, which ML.GENERATE_EMBEDDING
	// takes, false for a generative one, which ML.GENERATE_TEXT takes.
	Embedding bool   `json:"embedding,omitempty"`
	Location  string `json:"location"`
	// ServiceType is REMOTE_SERVICE_TYPE, for a remote model over a Cloud
	// AI service (modelservice.go, #1773), with no endpoint; "" for an
	// LLM or embedding model.
	ServiceType string `json:"serviceType,omitempty"`
}

// remoteDDL is a CREATE MODEL … REMOTE WITH CONNECTION's parts.
type remoteDDL struct {
	connection []string // project, location, id; nil for DEFAULT
	isDefault  bool     // REMOTE WITH CONNECTION DEFAULT
	endpoint   string
	// serviceType is REMOTE_SERVICE_TYPE (modelservice.go, #1773).
	serviceType string
}

// parseRemote reads REMOTE WITH CONNECTION {DEFAULT | name} OPTIONS(...)
// from stmt[i], which is REMOTE, to the statement's end.
func parseRemote(stmt []token, i int, d *modelDDL) (int, string) {
	if i+2 >= len(stmt) || !stmt[i+1].is("WITH") || !stmt[i+2].is("CONNECTION") {
		return 400, "Syntax error: expected WITH CONNECTION after REMOTE"
	}
	i += 3
	rd := &remoteDDL{}
	if i < len(stmt) && stmt[i].is("DEFAULT") {
		// The project's default connection (connectionauth.go, #1765).
		rd.isDefault = true
		i++
	} else {
		path, n := readModelPath(stmt[i:])
		if n == 0 {
			return 400, "Syntax error: expected a connection name after WITH CONNECTION"
		}
		switch len(path) {
		case 3:
			rd.connection = path
		case 2:
			rd.connection = []string{"", path[0], path[1]}
		default:
			return 400, "Invalid connection name " + strings.Join(path, ".") + ": expected `project.location.connection_id`"
		}
		i += n
	}
	if i >= len(stmt) || !stmt[i].is("OPTIONS") || i+1 >= len(stmt) || !stmt[i+1].punct("(") {
		return 400, "CREATE MODEL with a remote connection requires OPTIONS(ENDPOINT = ...)"
	}
	end := matchParen(stmt, i+1)
	if end < 0 {
		return 400, "Syntax error: unclosed OPTIONS"
	}
	opts := stmt[i+2 : end]
	for k := 0; k < len(opts); {
		if k+2 >= len(opts) || opts[k].kind != tokWord || !opts[k+1].punct("=") {
			return 400, "Syntax error in OPTIONS"
		}
		name := strings.ToUpper(opts[k].text)
		switch name {
		case "ENDPOINT":
			v, ok := unquoteString(opts[k+2].text)
			if opts[k+2].kind != tokString || !ok {
				return 400, "The ENDPOINT option of a remote model must be a string literal"
			}
			rd.endpoint = v
		case "REMOTE_SERVICE_TYPE":
			v, ok := unquoteString(opts[k+2].text)
			if opts[k+2].kind != tokString || !ok {
				return 400, "The REMOTE_SERVICE_TYPE option of a remote model must be a string literal"
			}
			if code, msg := checkServiceType(v); code != 0 { // modelservice.go, #1773
				return code, msg
			}
			rd.serviceType = strings.ToUpper(v)
		case "DOCUMENT_PROCESSOR":
			// Only CLOUD_AI_DOCUMENT_V1's, which is refused (modelservice.go).
			return 501, "Not implemented here: the " + name + " option of a remote model: " + serviceTypeRefusals["CLOUD_AI_DOCUMENT_V1"] + " Nothing was run."
		case "SPEECH_RECOGNIZER":
			// CLOUD_AI_SPEECH_TO_TEXT_V2's (#1799): CloudBurrow serves no
			// Speech-to-Text API, so no recognizer exists to name.
			return 501, "Not implemented here: the SPEECH_RECOGNIZER option of a remote model: CloudBurrow serves no Speech-to-Text " +
				"API, so no recognizer can exist for it to name; create the model without it and pass ML.TRANSCRIBE a " +
				"recognition_config, which configures the default recognizer (#1799). Nothing was run."
		case "MAX_BATCHING_ROWS", "PROMPT_COL", "INPUT_LABEL_COLS",
			"MODEL_TYPE", "AI_SERVICE_TYPE":
			return 501, "Not implemented here: the " + name + " option of a remote model: CloudBurrow's remote models are LLM " +
				"and embedding models on the instance's local Vertex AI endpoint alone. Nothing was run."
		default:
			return 400, "Unknown option " + opts[k].text + " for a remote model"
		}
		k += 3
		if k < len(opts) && opts[k].punct(",") {
			k++
		}
	}
	if rd.endpoint != "" && rd.serviceType != "" {
		return 400, "A remote model takes either the ENDPOINT or the REMOTE_SERVICE_TYPE option, not both"
	}
	if rd.endpoint == "" && rd.serviceType == "" {
		return 400, "CREATE MODEL with a remote connection requires the ENDPOINT or REMOTE_SERVICE_TYPE option"
	}
	if end+1 < len(stmt) {
		return 400, "Syntax error: a remote model takes no AS query: unexpected " + stmt[end+1].text
	}
	d.remote = rd
	d.opts = mlOptions{}
	return 0, ""
}

// endpointModel is the model an ENDPOINT names: a bare model name, or
// the segment after models/ of a Vertex AI URL or resource name, with any
// :method dropped. The URL is never dialled.
func endpointModel(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if k := strings.LastIndex(e, "/models/"); k >= 0 {
		e = e[k+len("/models/"):]
	} else if strings.HasPrefix(e, "models/") {
		e = strings.TrimPrefix(e, "models/")
	}
	if k := strings.IndexAny(e, ":/?"); k >= 0 {
		e = e[:k]
	}
	return e
}

// isEmbeddingEndpoint reports whether a model is an embedding model, by
// Google's names (text-embedding-*, text-multilingual-embedding-*,
// gemini-embedding-*, multimodalembedding*) and the local catalogue's
// (embeddinggemma-*).
func isEmbeddingEndpoint(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "embedding")
}

// createRemote checks a remote model's connection and endpoint: the
// model to keep, or code and message.
func (f front) createRemote(ctx context.Context, d modelDDL, project, loc string) (*remoteModel, int, string) {
	rd := d.remote
	if f.vertexai == "" {
		return nil, 501, "Not implemented here: a remote model needs the instance's local Vertex AI endpoint, which this instance does not serve to BigQuery: " +
			"start it with --local-ai-model (generation) or --local-ai-embeddings (embeddings) (docs/generation.md, #1760). CloudBurrow never sends a remote model's calls to Google. Nothing was run."
	}
	if rd.isDefault {
		id := f.settings.effective(project, strings.ToLower(cmp.Or(loc, "us")), "default_cloud_resource_connection_id")
		rd.connection = []string{project, strings.ToLower(cmp.Or(loc, "us")), cmp.Or(id, defaultConnectionID)}
	}
	cp := rd.connection[0]
	if cp == "" {
		cp = project
	}
	number := connectionProject(cp, f.numbers)
	cn, ok := f.connections.lookup(number, rd.connection[1], rd.connection[2])
	if rd.isDefault {
		var code int
		var msg string
		if cn, code, msg = f.ensureDefaultConnection(ctx, project, number, rd.connection[1], rd.connection[2], cn); code != 0 {
			return nil, code, msg
		}
		ok = true
	}
	if !ok {
		return nil, 404, fmt.Sprintf("Not found: Connection %s.%s.%s", cp, rd.connection[1], rd.connection[2])
	}
	if !sameLocation(cn.Location, loc) {
		return nil, 400, fmt.Sprintf("Dataset location %s differs from connection location %s", loc, cn.Location)
	}
	if _, ok := cn.Fields["cloudResource"]; !ok {
		return nil, 400, fmt.Sprintf("The connection %s.%s.%s is not a Cloud resource connection: a remote model over Vertex AI needs a CLOUD_RESOURCE connection",
			cp, rd.connection[1], rd.connection[2])
	}
	model := endpointModel(rd.endpoint)
	if model == "" && rd.serviceType == "" {
		return nil, 400, "Invalid ENDPOINT " + strconv.Quote(rd.endpoint)
	}
	if code, msg := f.checkConnectionUse(ctx, cp, project, cn); code != 0 { // connectionauth.go, #1765
		return nil, code, msg
	}
	if rd.serviceType != "" {
		return &remoteModel{Connection: fmt.Sprintf("projects/%d/locations/%s/connections/%s", number, cn.Location, cn.ID),
			ServiceType: rd.serviceType, Location: cn.Location}, 0, ""
	}
	return &remoteModel{Connection: fmt.Sprintf("projects/%d/locations/%s/connections/%s", number, cn.Location, cn.ID),
		Endpoint: rd.endpoint, Model: model, Embedding: isEmbeddingEndpoint(model), Location: cn.Location}, 0, ""
}

// sameLocation compares two BigQuery locations, case-insensitively.
func sameLocation(a, b string) bool {
	if b == "" {
		b = "US"
	}
	return strings.EqualFold(a, b)
}

// remoteResource fills a remote model's resource.
func (md bqModel) remoteResource(res map[string]any) {
	delete(res, "modelType")
	if md.Remote.ServiceType != "" {
		// remoteServiceType, with no endpoint (modelservice.go, #1773).
		res["remoteModelInfo"] = map[string]any{"connection": md.Remote.Connection, "remoteServiceType": md.Remote.ServiceType}
		return
	}
	res["remoteModelInfo"] = map[string]any{"connection": md.Remote.Connection, "endpoint": md.Remote.Endpoint}
}

// vertexLocation is the Vertex AI region a BigQuery location's calls
// would go to: a multi-region's default region.
func vertexLocation(loc string) string {
	switch strings.ToUpper(loc) {
	case "", "US":
		return "us-central1"
	case "EU":
		return "europe-west4"
	}
	return strings.ToLower(loc)
}

// refusedGenerateOptions are ML.GENERATE_TEXT's options the local runtime
// cannot honour, and why.
var refusedGenerateOptions = map[string]string{
	"safety_settings":           "the local runtime has no safety filters to configure",
	"generation_config":         "CloudBurrow does not read the JSON form yet; the individual options (max_output_tokens, temperature, top_p, top_k, stop_sequences) are applied",
	"ground_with_google_search": "grounding searches Google, which CloudBurrow never calls",
	"model_params":              "CloudBurrow does not read the JSON form yet; the individual options (max_output_tokens, temperature, top_p, top_k, stop_sequences) are applied",
}

// generationOption reads one of ML.GENERATE_TEXT's and AI.GENERATE_TEXT's
// generation options into cfg (a Vertex AI generationConfig), with
// BigQuery's documented ranges (bigqueryml-syntax-generate-text,
// bigqueryml-syntax-ai-generate-text, re-read 2026-11-18). ok is false when
// k is not a generation option.
//
// UNVERIFIED (re-read 2026-11-18: the two pages above, which give the
// ranges and no error text): the wording of the range errors and of top_k
// refused for a Gemini endpoint.
func generationOption(fname, endpoint, k, v string, cfg map[string]any) (code int, msg string, ok bool) {
	num := func(lo, hi float64) (float64, bool) {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil && f >= lo && f <= hi
	}
	switch k {
	case "max_output_tokens":
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 1 || n > 8192 {
			return 400, fmt.Sprintf("Invalid value of max_output_tokens for %s: %s. It must be in the range [1, 8192].", fname, v), true
		}
		cfg["maxOutputTokens"] = n
	case "temperature":
		f, ok := num(0, 1)
		if !ok {
			return 400, fmt.Sprintf("Invalid value of temperature for %s: %s. It must be in the range [0.0, 1.0].", fname, v), true
		}
		cfg["temperature"] = f
	case "top_p":
		f, ok := num(0, 1)
		if !ok {
			return 400, fmt.Sprintf("Invalid value of top_p for %s: %s. It must be in the range [0.0, 1.0].", fname, v), true
		}
		cfg["topP"] = f
	case "top_k":
		if strings.HasPrefix(strings.ToLower(endpoint), "gemini") {
			return 400, fmt.Sprintf("top_k is not supported for %s with the Gemini model endpoint %s", fname, endpoint), true
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 1 || n > 40 {
			return 400, fmt.Sprintf("Invalid value of top_k for %s: %s. It must be in the range [1, 40].", fname, v), true
		}
		cfg["topK"] = n
	case "stop_sequences":
		a, ok := parseStringArray(v)
		if !ok {
			return 400, "stop_sequences must be an ARRAY<STRING> literal", true
		}
		var stops []string
		for _, s := range a {
			if s != "" {
				stops = append(stops, s)
			}
		}
		if len(stops) > 0 {
			cfg["stopSequences"] = stops
		}
	default:
		return 0, "", false
	}
	return 0, "", true
}

// defaultGenerationConfig is BigQuery's Gemini defaults, which it sends
// when an option is not given: a remote model's answer is capped at 1024
// tokens and greedy (temperature 0, "deterministic") unless asked
// otherwise.
func defaultGenerationConfig() map[string]any {
	return map[string]any{"maxOutputTokens": int64(1024), "temperature": 0.0, "topP": 0.95}
}

// remoteSubquery writes ML.GENERATE_TEXT or ML.GENERATE_EMBEDDING of a
// remote model.
func (f front) remoteSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	rm := md.Remote
	fname := "ML." + c.fn
	if c.ai {
		fname = "AI." + c.fn
	}
	if rm.ServiceType != "" {
		return f.serviceSubquery(r, q, md, c, fname) // modelservice.go, #1773
	}
	if t := map[string]string{"ANNOTATE_IMAGE": "CLOUD_AI_VISION_V1", "TRANSCRIBE": "CLOUD_AI_SPEECH_TO_TEXT_V2"}[c.fn]; t != "" {
		return "", 400, fmt.Sprintf("Function %s is not supported for the remote model with endpoint %s: it takes a remote model whose REMOTE_SERVICE_TYPE is %s", fname, rm.Endpoint, t)
	}
	switch {
	case (c.fn == "GENERATE_TEXT" || c.fn == "GENERATE_TABLE") && rm.Embedding, c.fn == "GENERATE_EMBEDDING" && !rm.Embedding:
		return "", 400, fmt.Sprintf("Function %s is not supported for the remote model with endpoint %s", fname, rm.Endpoint)
	case c.fn != "GENERATE_TEXT" && c.fn != "GENERATE_EMBEDDING" && c.fn != "GENERATE_TABLE":
		return "", 400, fmt.Sprintf("Function %s is not supported for the remote model with endpoint %s", fname, rm.Endpoint)
	case c.input == "":
		return "", 400, fname + " requires an input: a query in parentheses or TABLE name"
	}
	if c.table != nil {
		// Multimodal input (#1798): an object table's objects sent to the
		// model with the prompt (bigqueryml-syntax-generate-text, "TABLE
		// object_table").
		if ds, table, ok := tableOf(q, c.table); ok && (len(c.table) < 3 || c.table[0] == projectOf(f.base)) && f.isObjectTable(r, ds, table) {
			return "", 501, "Not implemented here: " + fname + " over an object table (a multimodal prompt): the instance's local generative runtime " +
				"(LiteRT-LM, `up --local-ai-model`) runs text models and reads text only; its generateContent refuses inlineData and fileData parts, " +
				"so the objects cannot be sent to it (#1798). Nothing was run."
		}
	}
	if f.vertexai == "" {
		return "", 501, "Not implemented here: ML." + c.fn + ": this instance does not serve its local Vertex AI endpoint to BigQuery (#1760). Nothing was run."
	}
	flatten := c.fn == "GENERATE_EMBEDDING"
	var taskType string
	dims := 0
	genCfg := defaultGenerationConfig()
	var schema []aiField
	if c.fn == "GENERATE_TABLE" {
		v, ok := c.struc["output_schema"]
		s, isStr := unquoteString(v)
		if !ok || !isStr {
			return "", 400, "AI.GENERATE_TABLE requires the output_schema option, a STRING literal"
		}
		var msg string
		if schema, msg = parseOutputSchema(s); msg != "" {
			return "", 400, msg
		}
	}
	for k, v := range c.struc {
		if c.fn == "GENERATE_TEXT" || c.fn == "GENERATE_TABLE" {
			if code, msg, ok := generationOption(fname, rm.Endpoint, k, v, genCfg); ok {
				if code != 0 {
					return "", code, msg
				}
				continue
			}
		}
		switch {
		case c.fn == "GENERATE_TABLE" && k == "output_schema":
		case k == "flatten_json_output" && !c.ai:
			b, ok := map[string]bool{"TRUE": true, "FALSE": false}[strings.ToUpper(v)]
			if !ok {
				return "", 400, "flatten_json_output must be a BOOL literal"
			}
			flatten = b
		case (c.fn == "GENERATE_TEXT" || c.fn == "GENERATE_TABLE") && k == "request_type":
			s, _ := unquoteString(v)
			if s := strings.ToUpper(s); s != "DEDICATED" && s != "SHARED" && s != "UNSPECIFIED" {
				return "", 400, "request_type must be 'DEDICATED', 'SHARED' or 'UNSPECIFIED'"
			}
		case (c.fn == "GENERATE_TEXT" || c.fn == "GENERATE_TABLE") && refusedGenerateOptions[k] != "":
			return "", 501, "Not implemented here: the " + k + " option of " + fname + ": " + refusedGenerateOptions[k] +
				", and CloudBurrow refuses an option rather than ignoring it (docs/generation.md). Nothing was run."
		case c.fn == "GENERATE_EMBEDDING" && k == "task_type":
			s, ok := unquoteString(v)
			if !ok {
				return "", 400, "task_type must be a STRING literal"
			}
			taskType = s
		case c.fn == "GENERATE_EMBEDDING" && k == "output_dimensionality":
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return "", 400, "output_dimensionality must be a positive INT64 literal"
			}
			dims = n
		default:
			return "", 400, "Unknown option " + k + " for " + fname
		}
	}
	// The connection's permissions, before any row is sent (#1765).
	cn, ok := f.remoteConnection(rm)
	if !ok {
		return "", 404, "Not found: Connection " + rm.Connection
	}
	if code, msg := f.checkConnectionUse(r.Context(), md.Project, md.Project, cn); code != 0 {
		return "", code, msg
	}
	col := "prompt"
	if c.fn == "GENERATE_EMBEDDING" {
		col = "content"
	}
	cols, _, code, msg := f.queryAllRows(r, q, "SELECT * FROM ("+c.input+") LIMIT 0")
	if code != 0 {
		return "", code, msg
	}
	var found *mlColumn
	for i := range cols {
		if strings.EqualFold(cols[i].Name, col) {
			found = &cols[i]
		}
	}
	if found == nil {
		return "", 400, fmt.Sprintf("Invalid table-valued function %s: the input must have a column named %s", fname, col)
	}
	if !strings.EqualFold(found.Type, "STRING") {
		return "", 400, fmt.Sprintf("Invalid table-valued function %s: column %s must be of type STRING, not %s", fname, found.Name, found.Type)
	}
	key := "_cbml_in." + sqlIdent(found.Name)
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT DISTINCT "+key+" FROM ("+c.input+") AS _cbml_in WHERE "+key+" IS NOT NULL")
	if code != 0 {
		return "", code, msg
	}
	project := projectOf(f.base)
	var outCols []string // name and type
	if c.fn == "GENERATE_TABLE" {
		// The schema's columns, full_response and status (#1771).
		for _, fd := range schema {
			outCols = append(outCols, sqlIdent(fd.name)+" "+fd.typ.sql())
		}
		outCols = append(outCols, "full_response JSON", "status STRING")
	} else if c.ai && c.fn == "GENERATE_EMBEDDING" {
		// AI.GENERATE_EMBEDDING's columns, a text embedding model's (#1772).
		outCols = []string{"embedding ARRAY<FLOAT64>", "statistics JSON", "status STRING"}
	} else if c.ai {
		// AI.GENERATE_TEXT's columns, a Gemini model's (#1765).
		outCols = []string{"result STRING", "rai_result JSON", "grounding_result JSON", "statistics JSON", "full_response JSON", "status STRING"}
	} else if c.fn == "GENERATE_TEXT" && flatten {
		outCols = []string{"ml_generate_text_llm_result STRING", "ml_generate_text_rai_result JSON", "ml_generate_text_grounding_result JSON", "ml_generate_text_status STRING"}
	} else if c.fn == "GENERATE_TEXT" {
		outCols = []string{"ml_generate_text_result JSON", "ml_generate_text_status STRING"}
	} else if flatten {
		outCols = []string{"ml_generate_embedding_result ARRAY<FLOAT64>", "ml_generate_embedding_statistics JSON", "ml_generate_embedding_status STRING"}
	} else {
		outCols = []string{"ml_generate_embedding_result JSON", "ml_generate_embedding_status STRING"}
	}
	var lits []string
	for _, row := range rows {
		if len(row) != 1 || row[0] == nil {
			continue
		}
		in := *row[0]
		var parts []string
		if c.fn == "GENERATE_TABLE" {
			parts = f.generateTableRow(r, project, rm, schema, in, genCfg)
		} else if c.ai && c.fn == "GENERATE_EMBEDDING" {
			parts = f.embedRow(r.Context(), project, rm, in, true, taskType, dims)
		} else if c.ai {
			parts = f.aiGenerateTextRow(r.Context(), project, rm, in, genCfg)
		} else if c.fn == "GENERATE_TEXT" {
			parts = f.generateRow(r.Context(), project, rm, in, flatten, genCfg)
		} else {
			parts = f.embedRow(r.Context(), project, rm, in, flatten, taskType, dims)
		}
		lits = append(lits, "STRUCT("+sqlString(in)+", "+strings.Join(parts, ", ")+")")
	}
	var sel, types, names []string
	for _, oc := range outCols {
		name := strings.Fields(oc)[0]
		names = append(names, name)
		sel = append(sel, "_cbml_p."+name+" AS "+name)
		types = append(types, oc)
	}
	var except []string
	for _, ic := range cols {
		for _, n := range names {
			if strings.EqualFold(ic.Name, n) {
				except = append(except, sqlIdent(ic.Name))
			}
		}
	}
	inSel := "_cbml_in.*"
	if len(except) > 0 {
		inSel += " EXCEPT (" + strings.Join(except, ", ") + ")"
	}
	return "(SELECT " + strings.Join(sel, ", ") + ", " + inSel + " FROM (" + c.input + ") AS _cbml_in LEFT JOIN UNNEST(ARRAY<STRUCT<_cbml_k STRING, " +
		strings.Join(types, ", ") + ">>[" + strings.Join(lits, ", ") + "]) AS _cbml_p ON " + key + " = _cbml_p._cbml_k)", 0, ""
}

// jsonLiteral is a JSON literal of raw, or NULL.
func jsonLiteral(raw []byte) string {
	if len(raw) == 0 {
		return "CAST(NULL AS JSON)"
	}
	return "PARSE_JSON(" + sqlString(string(raw)) + ")"
}

// vertexCall posts body to the local Vertex AI endpoint's method of the
// remote model: the response, or the row status of its failure.
func (f front) vertexCall(ctx context.Context, project string, rm *remoteModel, method string, body any) ([]byte, string) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err.Error()
	}
	// The local endpoint serves generation under /v1beta1, the version
	// the official genai SDK's Vertex backend calls, and :predict under
	// /v1 (internal/service/vertexai).
	version := "v1"
	if method == "generateContent" {
		version = "v1beta1"
	}
	url := strings.TrimSuffix(f.vertexai, "/") + "/" + version + "/projects/" + project + "/locations/" + vertexLocation(rm.Location) +
		"/publishers/google/models/" + rm.Model + ":" + method
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "A retryable error occurred: the local Vertex AI endpoint did not answer: " + err.Error()
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Sprintf("%s: %s", e.Error.Status, e.Error.Message)
		}
		return nil, fmt.Sprintf("the local Vertex AI endpoint answered %d", resp.StatusCode)
	}
	return out, ""
}

// generateBody is a generateContent request for prompt with generationConfig
// cfg (none when cfg is empty).
func generateBody(prompt string, cfg map[string]any) map[string]any {
	body := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": prompt}}}}}
	if len(cfg) > 0 {
		body["generationConfig"] = cfg
	}
	return body
}

// generateRow is one prompt's output columns, as SQL literals.
func (f front) generateRow(ctx context.Context, project string, rm *remoteModel, prompt string, flatten bool, cfg map[string]any) []string {
	body := generateBody(prompt, cfg)
	out, status := f.vertexCall(ctx, project, rm, "generateContent", body)
	if !flatten {
		return []string{jsonLiteral(out), sqlString(status)}
	}
	if status != "" {
		return []string{"CAST(NULL AS STRING)", "CAST(NULL AS JSON)", "CAST(NULL AS JSON)", sqlString(status)}
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			SafetyRatings     json.RawMessage `json:"safetyRatings"`
			GroundingMetadata json.RawMessage `json:"groundingMetadata"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || len(resp.Candidates) == 0 {
		return []string{"CAST(NULL AS STRING)", "CAST(NULL AS JSON)", "CAST(NULL AS JSON)", sqlString("The response had no candidates")}
	}
	cand := resp.Candidates[0]
	var text strings.Builder
	for _, p := range cand.Content.Parts {
		text.WriteString(p.Text)
	}
	return []string{sqlString(text.String()), jsonLiteral(cand.SafetyRatings), jsonLiteral(cand.GroundingMetadata), sqlString("")}
}

// embedRow is one content's output columns, as SQL literals.
func (f front) embedRow(ctx context.Context, project string, rm *remoteModel, content string, flatten bool, taskType string, dims int) []string {
	inst := map[string]any{"content": content}
	if taskType != "" {
		inst["task_type"] = taskType
	}
	body := map[string]any{"instances": []any{inst}}
	if dims > 0 {
		body["parameters"] = map[string]any{"outputDimensionality": dims}
	}
	out, status := f.vertexCall(ctx, project, rm, "predict", body)
	if status != "" {
		if flatten {
			return []string{"CAST(NULL AS ARRAY<FLOAT64>)", "CAST(NULL AS JSON)", sqlString(status)}
		}
		return []string{"CAST(NULL AS JSON)", sqlString(status)}
	}
	if !flatten {
		return []string{jsonLiteral(out), sqlString("")}
	}
	var resp struct {
		Predictions []struct {
			Embeddings struct {
				Values     []float64       `json:"values"`
				Statistics json.RawMessage `json:"statistics"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || len(resp.Predictions) != 1 {
		return []string{"CAST(NULL AS ARRAY<FLOAT64>)", "CAST(NULL AS JSON)", sqlString("The response had no prediction")}
	}
	e := resp.Predictions[0].Embeddings
	vals := make([]string, len(e.Values))
	for i, v := range e.Values {
		vals[i] = sqlF(v)
	}
	return []string{"ARRAY<FLOAT64>[" + strings.Join(vals, ", ") + "]", jsonLiteral(e.Statistics), sqlString("")}
}

// aiFunction is the first AI.<fn>( call in sql, upper-cased, or "".
// AI.GENERATE is rewritten before this is asked (aigenerate.go, #1765);
// BigQuery's other generative AI functions (AI.GENERATE_TEXT,
// AI.GENERATE_TABLE, AI.EMBED, AI.FORECAST, …) are not served yet.
func aiFunction(sql string) string {
	if !strings.Contains(strings.ToUpper(sql), "AI.") {
		return ""
	}
	toks, ok := lex(sql)
	if !ok {
		return ""
	}
	for i := 0; i+3 < len(toks); i++ {
		if toks[i].is("AI") && toks[i+1].punct(".") && toks[i+2].kind == tokWord && toks[i+3].punct("(") &&
			(i == 0 || !toks[i-1].punct(".")) {
			if (toks[i+2].is("GENERATE_TEXT") || toks[i+2].is("GENERATE_TABLE") || toks[i+2].is("GENERATE_EMBEDDING")) && i+4 < len(toks) && toks[i+4].is("MODEL") {
				continue // served with the ML functions of a model (#1765, #1771, #1772)
			}
			if _, ok := aiScalars[strings.ToUpper(toks[i+2].text)]; ok || toks[i+2].is("AGG") {
				continue // evaluated row by row (aigenerate.go) or group by group (aiagg.go, #1777)
			}
			return "AI." + strings.ToUpper(toks[i+2].text)
		}
	}
	return ""
}

// aiGenerateTextRow is one prompt's AI.GENERATE_TEXT columns, as SQL
// literals: result, rai_result, grounding_result, statistics (the
// response's usageMetadata), full_response and status (#1765).
func (f front) aiGenerateTextRow(ctx context.Context, project string, rm *remoteModel, prompt string, cfg map[string]any) []string {
	body := generateBody(prompt, cfg)
	out, status := f.vertexCall(ctx, project, rm, "generateContent", body)
	null := []string{"CAST(NULL AS STRING)", "CAST(NULL AS JSON)", "CAST(NULL AS JSON)", "CAST(NULL AS JSON)", jsonLiteral(out)}
	if status != "" {
		return append(null, sqlString(status))
	}
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			SafetyRatings     json.RawMessage `json:"safetyRatings"`
			GroundingMetadata json.RawMessage `json:"groundingMetadata"`
		} `json:"candidates"`
		UsageMetadata json.RawMessage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || len(resp.Candidates) == 0 {
		return append(null, sqlString("The response had no candidates"))
	}
	cand := resp.Candidates[0]
	var text strings.Builder
	for _, p := range cand.Content.Parts {
		text.WriteString(p.Text)
	}
	return []string{sqlString(text.String()), jsonLiteral(cand.SafetyRatings), jsonLiteral(cand.GroundingMetadata), jsonLiteral(resp.UsageMetadata),
		jsonLiteral(out), sqlString("")}
}

// generateTableRow is one prompt's AI.GENERATE_TABLE columns, as SQL
// literals: the schema's fields, full_response and status (#1771).
func (f front) generateTableRow(r *http.Request, project string, rm *remoteModel, schema []aiField, prompt string, cfg map[string]any) []string {
	text, raw, status := f.aiGenerateText(r, project, rm, prompt+aiSchemaInstruction(schema), schemaObject(schema), cfg)
	var vals []string
	if status == "" {
		vals, status = schemaLiterals(schema, text)
	}
	if status != "" {
		vals = nil
		for _, fd := range schema {
			vals = append(vals, "CAST(NULL AS "+fd.typ.sql()+")")
		}
	}
	return append(vals, jsonLiteral(raw), sqlString(status))
}
