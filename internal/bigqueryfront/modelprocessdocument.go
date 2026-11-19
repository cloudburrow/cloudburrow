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
)

// ML.PROCESS_DOCUMENT over a CLOUD_AI_DOCUMENT_V1 remote model (#1799), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-process-document
// and bigqueryml-syntax-create-remote-model-service document them (read
// 2026-11-19):
//
//   - CREATE MODEL … REMOTE WITH CONNECTION … OPTIONS(REMOTE_SERVICE_TYPE
//     = 'CLOUD_AI_DOCUMENT_V1', DOCUMENT_PROCESSOR = '…') needs the
//     option, takes it with no other service type, and only in the US and
//     EU multi-regions. The processor is a bare ID (its default version)
//     or projects/…/locations/…/processors/…[/processorVersions/…], and is
//     looked up on the instance's local Document AI API
//     (internal/service/vertexai/documentai.go): CREATE MODEL fails, as
//     the page lists, when it does not exist, is not enabled, is in
//     another region than the model, or is of a type BigQuery does not
//     support (of the local types, the layout parser is supported; the OCR
//     processor is not: "Supported processors have a description that
//     starts with the word Extract", or are the Form or Layout Parser).
//   - ML.PROCESS_DOCUMENT(MODEL m, {TABLE object_table | (query)}
//     [, PROCESS_OPTIONS => (JSON '…')]): each object, read at its row's
//     generation, is sent with its content_type to the processor's
//     :process, never to Google; the output is ml_process_document_result
//     (JSON, the processed Document), ml_process_document_status (STRING,
//     empty on success, else the row's error) and the input's columns. The
//     query must select uri and content_type from the object table alone.
//     Each request has the documented 120-second timeout.
//
// UNVERIFIED or not served (re-read 2026-11-19): the "processor fields"
// columns the page says a processor adds (the local layout parser extracts
// no entities, so none are added); the Document AI Viewer grant on the
// connection's service account is not checked (CloudBurrow's role
// catalogue has no Document AI roles); a document of more than 15 pages
// gets the local processor's online page limit as its row's status, where
// BigQuery's limit is 130 pages; the texts of the refusals.

// processDocumentTimeout is the documented per-request timeout.
const processDocumentTimeout = 120 * time.Second

// processorName matches a processor or processor version resource name.
var processorName = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/processors/([^/]+)(/processorVersions/[^/]+)?$`)

// documentProcessor resolves a DOCUMENT_PROCESSOR on the local Document AI
// API: the name the model keeps, or the refusal.
func (f front) documentProcessor(ctx context.Context, v, project string, number int64, location string) (string, int, string) {
	region := strings.ToLower(location)
	if region != "us" && region != "eu" {
		return "", 400, "Remote models over the Document AI API can only be created in the US and EU multi-regions, not " + location
	}
	var names []string
	version := ""
	if strings.Contains(v, "/") {
		m := processorName.FindStringSubmatch(v)
		if m == nil {
			return "", 400, "Invalid DOCUMENT_PROCESSOR " + strconv.Quote(v) +
				": expected a processor ID or projects/PROJECT_NUMBER/locations/LOCATION/processors/PROCESSOR_ID/processorVersions/PROCESSOR_VERSION"
		}
		if m[2] != region {
			return "", 400, fmt.Sprintf("The processor %s exists in a different region (%s) than the model (%s)", v, m[2], location)
		}
		names = []string{strings.TrimSuffix(v, m[4])}
		if m[4] != "" {
			version = v
		}
	} else {
		names = []string{fmt.Sprintf("projects/%d/locations/%s/processors/%s", number, region, v),
			"projects/" + project + "/locations/" + region + "/processors/" + v}
	}
	for _, name := range names {
		var p struct {
			Name                    string `json:"name"`
			Type                    string `json:"type"`
			State                   string `json:"state"`
			DefaultProcessorVersion string `json:"defaultProcessorVersion"`
		}
		code, msg := f.documentAI(ctx, http.MethodGet, name, nil, &p)
		if code == 404 {
			continue
		}
		if code != 0 {
			return "", code, "Cannot read the document processor " + name + ": " + msg
		}
		if version != "" && version != p.DefaultProcessorVersion {
			return "", 400, "The processor version " + version + " doesn't exist"
		}
		if p.State != "ENABLED" {
			return "", 400, "The processor " + p.Name + " exists but is not enabled: its state is " + p.State
		}
		if p.Type != "LAYOUT_PARSER_PROCESSOR" {
			return "", 400, "The processor " + p.Name + " is not supported: its type " + p.Type + " is not a prebuilt extractor, " +
				"the Form Parser, the Layout Parser or a Custom Extractor"
		}
		if version != "" {
			return version, 0, ""
		}
		return p.Name, 0, ""
	}
	return "", 400, "The processor " + v + " doesn't exist in location " + region
}

// documentAI calls the local Document AI API: 0, or the status and message
// of its error. out receives the answer.
func (f front) documentAI(ctx context.Context, method, path string, body []byte, out any) (int, string) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(f.vertexai, "/")+"/v1/"+path, rd)
	if err != nil {
		return 500, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 503, "A retryable error occurred: the local Document AI endpoint did not answer: " + err.Error()
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return 503, err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
			if e.Error.Status != "" {
				return resp.StatusCode, e.Error.Status + ": " + e.Error.Message
			}
			return resp.StatusCode, e.Error.Message
		}
		return resp.StatusCode, fmt.Sprintf("the local Document AI endpoint answered %d", resp.StatusCode)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return 500, "the local Document AI endpoint answered an unreadable response"
	}
	return 0, ""
}

// objectTableOfQuery is the one table a PROCESS_DOCUMENT query reads:
// SELECT … FROM table [WHERE …], with no join.
func objectTableOfQuery(query string) ([]string, bool) {
	toks, ok := lex(query)
	if !ok || len(toks) == 0 || !toks[0].is("SELECT") {
		return nil, false
	}
	var table []string
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.is("JOIN") || t.is("UNION") || t.punct("(") && i > 0 && toks[i-1].is("FROM") {
			return nil, false
		}
		if t.is("FROM") {
			if table != nil {
				return nil, false
			}
			p, n := readModelPath(toks[i+1:])
			if n == 0 {
				return nil, false
			}
			table = p
			i += n
			if i+1 < len(toks) && (toks[i+1].punct(",") || toks[i+1].kind == tokWord && !toks[i+1].is("WHERE") &&
				!toks[i+1].is("ORDER") && !toks[i+1].is("LIMIT")) {
				return nil, false // an alias or a second table
			}
		}
	}
	return table, table != nil
}

// processDocumentSubquery writes ML.PROCESS_DOCUMENT.
func (f front) processDocumentSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall, fname string) (string, int, string) {
	for k := range c.struc {
		return "", 400, "Unknown option " + k + " for " + fname
	}
	var opts json.RawMessage
	for k, v := range c.named {
		if k != "process_options" {
			return "", 400, "Unknown argument " + k + " of " + fname
		}
		s, ok := recognitionConfigText(v) // (JSON '…') or a string literal
		if !ok || !json.Valid([]byte(s)) {
			return "", 400, "process_options of " + fname + " must be a JSON literal: process_options => (JSON '{...}')"
		}
		opts = json.RawMessage(s)
	}
	if c.table == nil {
		if c.input == "" {
			return "", 400, fname + " requires an object table: TABLE name, or a query over one"
		}
		t, ok := objectTableOfQuery(c.input)
		if !ok {
			return "", 400, fname + "'s query must read only the object table, with no JOIN and no alias"
		}
		c.table, c.objectQuery = t, c.input
	}
	if md.Remote.Processor == "" {
		return "", 400, "The remote model names no document processor"
	}
	src, objects, code, msg := f.serviceObjects(r, q, md, c, fname)
	if code != 0 {
		return "", code, msg
	}
	var lits []string
	for _, o := range objects {
		var res []byte
		status := o.status
		if o.data != nil {
			res, status = f.processDocument(r.Context(), md.Remote.Processor, o.contentType, o.data, opts)
		}
		lits = append(lits, "STRUCT("+sqlString(o.uri)+", "+jsonLiteral(res)+", "+sqlString(status)+")")
	}
	return "(SELECT _cbml_p.ml_process_document_result AS ml_process_document_result, " +
		"_cbml_p.ml_process_document_status AS ml_process_document_status, _cbml_in.* FROM " + src + " AS _cbml_in LEFT JOIN " +
		"UNNEST(ARRAY<STRUCT<_cbml_k STRING, ml_process_document_result JSON, ml_process_document_status STRING>>[" +
		strings.Join(lits, ", ") + "]) AS _cbml_p ON _cbml_in.uri = _cbml_p._cbml_k)", 0, ""
}

// processDocument sends one document to the processor: its Document's
// JSON, and the row's status.
func (f front) processDocument(ctx context.Context, processor, contentType string, data []byte, opts json.RawMessage) ([]byte, string) {
	if contentType == "" {
		return nil, "The object has no content_type: Document AI needs the document's MIME type"
	}
	req := map[string]any{"rawDocument": map[string]string{"content": base64.StdEncoding.EncodeToString(data), "mimeType": contentType}}
	if opts != nil {
		req["processOptions"] = opts
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(ctx, processDocumentTimeout)
	defer cancel()
	var out struct {
		Document json.RawMessage `json:"document"`
	}
	if code, msg := f.documentAI(ctx, http.MethodPost, processor+":process", body, &out); code != 0 {
		if ctx.Err() != nil {
			return nil, "DEADLINE_EXCEEDED: the document was not processed within 120 seconds"
		}
		return nil, msg
	}
	if out.Document == nil {
		return []byte("{}"), ""
	}
	return out.Document, ""
}
