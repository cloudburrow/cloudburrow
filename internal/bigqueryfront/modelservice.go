package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Remote models over Cloud AI services (#1773): CREATE MODEL … REMOTE
// WITH CONNECTION … OPTIONS (REMOTE_SERVICE_TYPE = …), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-remote-model-service
// documents it (read 2026-11-18). Each service type has its own function:
//
//   - CLOUD_AI_VISION_V1, ML.ANNOTATE_IMAGE(MODEL m, TABLE object_table,
//     STRUCT([features] AS vision_features))
//     (bigqueryml-syntax-annotate-image): served. Each object of the
//     object table is read at its row's generation (objectData, as the
//     table's connection may) and sent, as Vision v1's images:annotate
//     request, to the instance's local Vertex AI endpoint, whose Vision
//     route reads text with Tesseract (internal/service/vertexai/vision.go);
//     never to Google. TEXT_DETECTION and DOCUMENT_TEXT_DETECTION are
//     answered (text, with word and block boxes since #1799), and IMAGE_PROPERTIES
//     (dominant colors and a crop hint, computed from the pixels, #1799);
//     FACE_DETECTION, LANDMARK_DETECTION, LOGO_DETECTION, LABEL_DETECTION
//     and OBJECT_LOCALIZATION have no local model and are refused 501
//     before anything is read. The output is the input
//     table's columns with ml_annotate_image_result (JSON, the image's
//     AnnotateImageResponse) and ml_annotate_image_status (STRING, empty
//     on success, else the image's error).
//   - CLOUD_AI_SPEECH_TO_TEXT_V2 (ML.TRANSCRIBE, modeltranscribe.go),
//     CLOUD_AI_TRANSLATE_V3 (ML.TRANSLATE, modeltranslate.go) and
//     CLOUD_AI_NATURAL_LANGUAGE_V1 (ML.UNDERSTAND_TEXT, modelunderstand.go)
//     are served since #1799.
//   - CLOUD_AI_DOCUMENT_V1 (ML.PROCESS_DOCUMENT): 501 at CREATE MODEL,
//     naming why (serviceTypeRefusals): no local model answers it as the
//     service does.
//
// The connection rules are a remote LLM's (modelremote.go): a Cloud
// resource connection in the dataset's location, bigquery.connections.use
// for the caller and roles/aiplatform.user for its service account.
//
// UNVERIFIED (the pages above quote none of them, re-read 2026-11-18):
// the texts of the refusals; that vision_features is required (the page
// gives no default); that features are matched case-insensitively (its
// examples write them in lower case).

// serviceTypeFunctions are the documented service types, and each one's
// function.
var serviceTypeFunctions = map[string]string{
	"CLOUD_AI_NATURAL_LANGUAGE_V1": "ML.UNDERSTAND_TEXT",
	"CLOUD_AI_TRANSLATE_V3":        "ML.TRANSLATE",
	"CLOUD_AI_VISION_V1":           "ML.ANNOTATE_IMAGE",
	"CLOUD_AI_DOCUMENT_V1":         "ML.PROCESS_DOCUMENT",
	"CLOUD_AI_SPEECH_TO_TEXT_V2":   "ML.TRANSCRIBE",
}

// serviceTypeRefusals are the service types CloudBurrow cannot serve, and
// why.
var serviceTypeRefusals = map[string]string{
	"CLOUD_AI_DOCUMENT_V1": "a remote model over the Document AI API (ML.PROCESS_DOCUMENT): CloudBurrow serves no Document AI API, " +
		"so no processor can exist for DOCUMENT_PROCESSOR to name, and never calls Google (#1773).",
}

// checkServiceType checks a REMOTE_SERVICE_TYPE: 0, or the refusal.
func checkServiceType(v string) (int, string) {
	t := strings.ToUpper(v)
	if _, ok := serviceTypeFunctions[t]; !ok {
		names := make([]string, 0, len(serviceTypeFunctions))
		for n := range serviceTypeFunctions {
			names = append(names, n)
		}
		sort.Strings(names)
		return 400, "Invalid REMOTE_SERVICE_TYPE " + strconv.Quote(v) + ": expected one of " + strings.Join(names, ", ")
	}
	if why := serviceTypeRefusals[t]; why != "" {
		return 501, "Not implemented here: " + why + " Nothing was run."
	}
	return 0, ""
}

// visionFeatures are ML.ANNOTATE_IMAGE's documented features, true for
// those the local endpoint answers.
var visionFeatures = map[string]bool{
	"TEXT_DETECTION": true, "DOCUMENT_TEXT_DETECTION": true, "IMAGE_PROPERTIES": true,
	"FACE_DETECTION": false, "LANDMARK_DETECTION": false, "LOGO_DETECTION": false, "LABEL_DETECTION": false,
	"OBJECT_LOCALIZATION": false,
}

// parseStringArray reads an ARRAY<STRING> literal as mlStruct keeps it
// (its tokens joined): ['a','b'] or ARRAY<STRING>['a'].
func parseStringArray(v string) ([]string, bool) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToUpper(v), "ARRAY<STRING>") {
		v = v[len("ARRAY<STRING>"):]
	}
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil, false
	}
	toks, ok := lex(v[1 : len(v)-1])
	if !ok {
		return nil, false
	}
	var out []string
	for i, t := range toks {
		if i%2 == 1 {
			if !t.punct(",") {
				return nil, false
			}
			continue
		}
		s, ok := unquoteString(t.text)
		if t.kind != tokString || !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, len(out) > 0 && len(toks)%2 == 1
}

// serviceSubquery writes the function of a remote model over a Cloud AI
// service.
func (f front) serviceSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall, fname string) (string, int, string) {
	rm := md.Remote
	if want := serviceTypeFunctions[rm.ServiceType]; "ML."+c.fn != want || c.ai {
		return "", 400, fmt.Sprintf("Function %s is not supported for the remote model with REMOTE_SERVICE_TYPE %s: it takes %s", fname, rm.ServiceType, want)
	}
	if f.vertexai == "" {
		return "", 501, "Not implemented here: " + fname + ": this instance does not serve its local Vertex AI endpoint to BigQuery (#1760). Nothing was run."
	}
	if c.fn == "TRANSLATE" {
		return f.translateSubquery(r, q, md, c, fname) // modeltranslate.go, #1799
	}
	if c.fn == "UNDERSTAND_TEXT" {
		return f.understandSubquery(r, q, md, c, fname) // modelunderstand.go, #1799
	}
	if c.table == nil {
		return "", 400, fname + " requires an object table: TABLE name"
	}
	if c.fn == "TRANSCRIBE" {
		return f.transcribeSubquery(r, q, md, c, fname) // modeltranscribe.go, #1799
	}
	for k := range c.named {
		return "", 400, "Unknown argument " + k + " of " + fname
	}
	var features []string
	for k, v := range c.struc {
		if k != "vision_features" {
			return "", 400, "Unknown option " + k + " for " + fname
		}
		arr, ok := parseStringArray(v)
		if !ok {
			return "", 400, "vision_features must be an ARRAY<STRING> literal of Vision feature names"
		}
		for _, ft := range arr {
			u := strings.ToUpper(ft)
			served, known := visionFeatures[u]
			if !known {
				return "", 400, "Invalid vision feature " + strconv.Quote(ft)
			}
			if !served {
				return "", 501, "Not implemented here: the " + u + " feature of " + fname + ": CloudBurrow has no local model for it " +
					"(TEXT_DETECTION and DOCUMENT_TEXT_DETECTION are read with local Tesseract OCR, IMAGE_PROPERTIES from the pixels) " +
					"and never calls Google. Nothing was run."
			}
			features = append(features, u)
		}
	}
	if len(features) == 0 {
		return "", 400, fname + " requires STRUCT([...] AS vision_features)"
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
			res, status = f.annotateImage(r.Context(), o.data, features)
		}
		lits = append(lits, "STRUCT("+sqlString(o.uri)+", "+jsonLiteral(res)+", "+sqlString(status)+")")
	}
	return "(SELECT _cbml_p.ml_annotate_image_result AS ml_annotate_image_result, _cbml_p.ml_annotate_image_status AS ml_annotate_image_status, " +
		"_cbml_in.* FROM " + src + " AS _cbml_in LEFT JOIN UNNEST(ARRAY<STRUCT<_cbml_k STRING, ml_annotate_image_result JSON, ml_annotate_image_status STRING>>[" +
		strings.Join(lits, ", ") + "]) AS _cbml_p ON _cbml_in.uri = _cbml_p._cbml_k)", 0, ""
}

// serviceObject is one object of a service function's object table: its
// data, or the row's status when it could not be read.
type serviceObject struct {
	uri    string
	data   []byte
	status string
}

// serviceObjects checks a service function's object table, the model's
// location and connection, and reads every object at its row's generation
// (objectData, as the table's connection may). src is the table's quoted
// path.
func (f front) serviceObjects(r *http.Request, q queryOptions, md bqModel, c mlCall, fname string) (src string, objects []serviceObject, code int, msg string) {
	rm := md.Remote
	project := projectOf(f.base)
	ds, table, ok := tableOf(q, c.table)
	if !ok {
		return "", nil, 400, "Table name " + strings.Join(c.table, ".") + " missing dataset while no default dataset is set in the request."
	}
	if len(c.table) == 3 && c.table[0] != project {
		return "", nil, 400, fmt.Sprintf("%s takes an object table of project %s", fname, project)
	}
	ot, live := f.objectTable(r, ds, table)
	if !live {
		if st, got := f.get(r, tablePath(ds, table)); st != http.StatusOK {
			return "", nil, st, errorMessage(got, st)
		}
		return "", nil, 400, fmt.Sprintf("%s takes an object table; %s:%s.%s is not one", fname, project, ds, table)
	}
	if !sameLocation(rm.Location, f.datasetLocation(r, ds)) {
		return "", nil, 400, fmt.Sprintf("%s must run in the remote model's location %s", fname, rm.Location)
	}
	cn, ok := f.remoteConnection(rm)
	if !ok {
		return "", nil, 404, "Not found: Connection " + rm.Connection
	}
	if code, msg := f.checkConnectionUse(r.Context(), md.Project, md.Project, cn); code != 0 {
		return "", nil, code, msg
	}
	if ot.stale(f.now()) {
		if code, msg := f.refreshObjectTable(r, streamKey(project, ds, table)); code != 0 {
			return "", nil, code, msg
		}
	}
	src = quotePath([]string{project, ds, table})
	_, rows, code, msg := f.queryAllRows(r, q, "SELECT uri, generation FROM "+src)
	if code != 0 {
		return "", nil, code, msg
	}
	for _, row := range rows {
		if len(row) != 2 || row[0] == nil || row[1] == nil {
			continue
		}
		o := serviceObject{uri: *row[0]}
		gen, _ := strconv.ParseInt(*row[1], 10, 64)
		data, code, msg := f.objectData(r, ot, o.uri, gen)
		switch {
		case code == 403:
			return "", nil, code, msg
		case code != 0:
			o.status = msg
		case data == nil:
			o.data = []byte{}
		default:
			o.data = data
		}
		objects = append(objects, o)
	}
	return src, objects, 0, ""
}

// annotateImage sends one image to the local endpoint's Vision route:
// its AnnotateImageResponse, and the row's status.
func (f front) annotateImage(ctx context.Context, data []byte, features []string) ([]byte, string) {
	var fs []any
	for _, ft := range features {
		fs = append(fs, map[string]string{"type": ft})
	}
	body, _ := json.Marshal(map[string]any{"requests": []any{map[string]any{
		"image": map[string]string{"content": base64.StdEncoding.EncodeToString(data)}, "features": fs}}})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(f.vertexai, "/")+"/v1/images:annotate", bytes.NewReader(body))
	if err != nil {
		return nil, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "A retryable error occurred: the local Vision endpoint did not answer: " + err.Error()
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err.Error()
	}
	var batch struct {
		Responses []json.RawMessage `json:"responses"`
		Error     struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &batch) != nil || resp.StatusCode != http.StatusOK || len(batch.Responses) != 1 {
		if batch.Error.Message != "" {
			return nil, batch.Error.Message
		}
		return nil, fmt.Sprintf("the local Vision endpoint answered %d", resp.StatusCode)
	}
	one := batch.Responses[0]
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(one, &e) == nil && e.Error != nil {
		return one, e.Error.Message
	}
	return one, ""
}
