package vertexai

// The import and export formats of Vertex AI managed datasets (#2210), as
// Google's ioformat schema files describe them
// (gs://google-cloud-aiplatform/schema/dataset/ioformat/*_io_format_1.0.0.yaml,
// read 2026-11-24) and the data preparation pages document them
// (https://docs.cloud.google.com/vertex-ai/docs/image-data/classification/prepare-data,
// .../image-data/object-detection/prepare-data):
//   - JSON Lines: imageGcsUri, videoGcsUri, or textContent / textGcsUri
//     (up to 10 MB); classificationAnnotation(s), boundingBoxAnnotations,
//     textSegmentAnnotations, sentimentAnnotation, timeSegmentAnnotations,
//     temporalBoundingBoxAnnotations; annotationResourceLabels and
//     dataItemResourceLabels, whose aiplatform.googleapis.com/ml_use is
//     training, test or validation;
//   - CSV: [ML_USE],GCS_FILE_PATH,[LABEL...] for image classification,
//     [ML_USE],GCS_FILE_PATH,[LABEL],[BOUNDING_BOX]* (X_MIN,Y_MIN,,,X_MAX,Y_MAX,,
//     or four vertices) for bounding boxes, ML_USE one of TRAINING, TEST or
//     VALIDATION; text classification [ML_USE],GCS_FILE_PATH|"TEXT",LABEL;
//     video classification [ML_USE],VIDEO_URI,LABEL,START,END.
//   - bounding box coordinates are relative, in [0, 1], 0,0 the top left.
// Export writes each line back in the import format of its annotation set.

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ioFormat is one import format.
type ioFormat struct {
	file        string // the ioformat schema file
	kind        string // image, text or video
	problemType string // its SavedQuery's problem_type
	annotation  string // the annotation payload schema file
	title       string // that schema's title
	multi       bool   // classification: several labels an item
}

var ioFormats = []*ioFormat{
	{"image_classification_single_label_io_format_1.0.0.yaml", "image", "IMAGE_CLASSIFICATION_SINGLE_LABEL", "image_classification_1.0.0.yaml", "ImageClassification", false},
	{"image_classification_multi_label_io_format_1.0.0.yaml", "image", "IMAGE_CLASSIFICATION_MULTI_LABEL", "image_classification_1.0.0.yaml", "ImageClassification", true},
	{"image_bounding_box_io_format_1.0.0.yaml", "image", "IMAGE_BOUNDING_BOX", "image_bounding_box_1.0.0.yaml", "ImageBoundingBox", false},
	{"text_classification_single_label_io_format_1.0.0.yaml", "text", "TEXT_CLASSIFICATION_SINGLE_LABEL", "text_classification_1.0.0.yaml", "TextClassification", false},
	{"text_classification_multi_label_io_format_1.0.0.yaml", "text", "TEXT_CLASSIFICATION_MULTI_LABEL", "text_classification_1.0.0.yaml", "TextClassification", true},
	{"text_extraction_io_format_1.0.0.yaml", "text", "TEXT_EXTRACTION", "text_extraction_1.0.0.yaml", "TextExtraction", false},
	{"text_sentiment_io_format_1.0.0.yaml", "text", "TEXT_SENTIMENT", "text_sentiment_1.0.0.yaml", "TextSentiment", false},
	{"video_classification_io_format_1.0.0.yaml", "video", "VIDEO_CLASSIFICATION", "video_classification_1.0.0.yaml", "VideoClassification", false},
	{"video_object_tracking_io_format_1.0.0.yaml", "video", "VIDEO_OBJECT_TRACKING", "video_object_tracking_1.0.0.yaml", "VideoObjectTracking", false},
	{"video_action_recognition_io_format_1.0.0.yaml", "video", "VIDEO_ACTION_RECOGNITION", "video_action_recognition_1.0.0.yaml", "VideoActionRecognition", false},
}

func ioFormatFor(uri string) (*ioFormat, error) {
	name := strings.TrimPrefix(strings.TrimSpace(uri), ioformatPrefix)
	for _, f := range ioFormats {
		if f.file == name && strings.HasPrefix(strings.TrimSpace(uri), ioformatPrefix) {
			return f, nil
		}
	}
	var names []string
	for _, f := range ioFormats {
		names = append(names, f.file)
	}
	return nil, apierror.InvalidArgument("%q is not an import schema served; the import schemas are %s{%s}", uri, ioformatPrefix, strings.Join(names, ","))
}

func ioFormatByProblem(p string) *ioFormat {
	for _, f := range ioFormats {
		if f.problemType == p {
			return f
		}
	}
	return nil
}

func ioFormatByAnnotation(schema string) *ioFormat {
	for _, f := range ioFormats {
		if annotationPrefix+f.annotation == schema {
			return f
		}
	}
	return nil
}

// importAnn is one annotation of an import line.
type importAnn struct {
	label   string // the AnnotationSpec display name ("" for sentiment)
	payload map[string]any
	labels  map[string]string
}

// importRec is one import line.
type importRec struct {
	uri, text string
	mlUse     string
	labels    map[string]string
	anns      []importAnn
	line      int
}

func strMap(v any) map[string]string {
	m, _ := v.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, x := range m {
		out[k] = fmt.Sprint(x)
	}
	return out
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// box reads xMin, yMin, xMax and yMax, each relative.
func box(m map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range []string{"xMin", "xMax", "yMin", "yMax"} {
		v, ok := num(m[k])
		if !ok || v < 0 || v > 1 || math.IsNaN(v) {
			return nil, fmt.Errorf("%s must be a number in [0, 1] (bounding box coordinates are relative to the image size)", k)
		}
		out[k] = v
	}
	if out["xMin"].(float64) >= out["xMax"].(float64) || out["yMin"].(float64) >= out["yMax"].(float64) {
		return nil, fmt.Errorf("xMin must be less than xMax and yMin less than yMax")
	}
	return out, nil
}

// duration reads a time like "1.5s" (or a bare number of seconds), or
// Infinity.
func duration(v any, def string) (string, error) {
	s := strings.TrimSpace(fmt.Sprint(v))
	if v == nil || s == "" {
		return def, nil
	}
	if s == "Infinity" || s == "inf" {
		return "Infinity", nil
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil || f < 0 {
		return "", fmt.Errorf("time %q is not a number of seconds such as 1.5s", s)
	}
	return strconv.FormatFloat(f, 'f', -1, 64) + "s", nil
}

// parseJSONLine reads one JSON Lines record of format f.
func parseJSONLine(f *ioFormat, line string) (importRec, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return importRec{}, fmt.Errorf("not JSON: %v", err)
	}
	r := importRec{labels: strMap(m["dataItemResourceLabels"])}
	switch f.kind {
	case "image":
		r.uri, _ = m["imageGcsUri"].(string)
		if r.uri == "" {
			return r, fmt.Errorf("imageGcsUri is required")
		}
	case "video":
		r.uri, _ = m["videoGcsUri"].(string)
		if r.uri == "" {
			return r, fmt.Errorf("videoGcsUri is required")
		}
	case "text":
		r.uri, _ = m["textGcsUri"].(string)
		r.text, _ = m["textContent"].(string)
		if (r.uri == "") == (r.text == "") {
			return r, fmt.Errorf("exactly one of textContent and textGcsUri is required")
		}
	}
	ann := func(o map[string]any, extra map[string]any) importAnn {
		name, _ := o["displayName"].(string)
		return importAnn{label: name, payload: extra, labels: strMap(o["annotationResourceLabels"])}
	}
	list := func(k string) []map[string]any {
		var out []map[string]any
		if l, ok := m[k].([]any); ok {
			for _, x := range l {
				if o, ok := x.(map[string]any); ok {
					out = append(out, o)
				}
			}
		}
		return out
	}
	switch f.problemType {
	case "IMAGE_CLASSIFICATION_SINGLE_LABEL", "TEXT_CLASSIFICATION_SINGLE_LABEL":
		if o, ok := m["classificationAnnotation"].(map[string]any); ok {
			r.anns = append(r.anns, ann(o, nil))
		}
	case "IMAGE_CLASSIFICATION_MULTI_LABEL", "TEXT_CLASSIFICATION_MULTI_LABEL":
		for _, o := range list("classificationAnnotations") {
			r.anns = append(r.anns, ann(o, nil))
		}
	case "IMAGE_BOUNDING_BOX":
		for _, o := range list("boundingBoxAnnotations") {
			b, err := box(o)
			if err != nil {
				return r, err
			}
			r.anns = append(r.anns, ann(o, b))
		}
	case "TEXT_EXTRACTION":
		for _, o := range list("textSegmentAnnotations") {
			s, ok1 := num(o["startOffset"])
			e, ok2 := num(o["endOffset"])
			if !ok1 || !ok2 || s < 0 || e <= s {
				return r, fmt.Errorf("textSegmentAnnotations need startOffset < endOffset, both non-negative")
			}
			r.anns = append(r.anns, ann(o, map[string]any{"textSegment": map[string]any{"startOffset": s, "endOffset": e}}))
		}
	case "TEXT_SENTIMENT":
		if o, ok := m["sentimentAnnotation"].(map[string]any); ok {
			s, ok1 := num(o["sentiment"])
			mx, ok2 := num(o["sentimentMax"])
			if !ok1 || !ok2 || mx < 0 || mx > 10 || s < 0 || s > mx || s != math.Trunc(s) || mx != math.Trunc(mx) {
				return r, fmt.Errorf("sentimentAnnotation needs integers 0 <= sentiment <= sentimentMax <= 10")
			}
			r.anns = append(r.anns, importAnn{payload: map[string]any{"sentiment": s, "sentimentMax": mx}, labels: strMap(o["annotationResourceLabels"])})
		}
	case "VIDEO_CLASSIFICATION", "VIDEO_ACTION_RECOGNITION":
		for _, o := range list("timeSegmentAnnotations") {
			st, err := duration(o["startTime"], "0s")
			if err != nil {
				return r, err
			}
			en, err := duration(o["endTime"], "Infinity")
			if err != nil {
				return r, err
			}
			r.anns = append(r.anns, ann(o, map[string]any{"timeSegment": map[string]any{"startTimeOffset": st, "endTimeOffset": en}}))
		}
	case "VIDEO_OBJECT_TRACKING":
		for _, o := range list("temporalBoundingBoxAnnotations") {
			b, err := box(o)
			if err != nil {
				return r, err
			}
			t, err := duration(o["timeOffset"], "0s")
			if err != nil {
				return r, err
			}
			b["timeOffset"] = t
			if id, ok := num(o["instanceId"]); ok {
				b["instanceId"] = id
			}
			r.anns = append(r.anns, ann(o, b))
		}
	}
	if f.problemType == "IMAGE_CLASSIFICATION_SINGLE_LABEL" || f.problemType == "TEXT_CLASSIFICATION_SINGLE_LABEL" {
		if len(r.anns) > 1 {
			return r, fmt.Errorf("a single-label line has one classificationAnnotation")
		}
	}
	for _, a := range r.anns {
		if f.problemType != "TEXT_SENTIMENT" && a.label == "" {
			return r, fmt.Errorf("an annotation's displayName is required")
		}
	}
	return r, nil
}

var csvMLUse = map[string]string{"TRAINING": "training", "TRAIN": "training", "TEST": "test", "VALIDATION": "validation", "VALIDATE": "validation", "UNASSIGNED": ""}

// parseCSVRecord reads one CSV record of format f.
func parseCSVRecord(f *ioFormat, rec []string) (importRec, error) {
	r := importRec{}
	if len(rec) > 0 {
		if u, ok := csvMLUse[strings.ToUpper(strings.TrimSpace(rec[0]))]; ok && !strings.HasPrefix(rec[0], "gs://") {
			r.mlUse = u
			rec = rec[1:]
		}
	}
	if len(rec) == 0 || strings.TrimSpace(rec[0]) == "" {
		return r, fmt.Errorf("the file path column is required")
	}
	first := strings.TrimSpace(rec[0])
	rest := rec[1:]
	if f.kind == "text" && !strings.HasPrefix(first, "gs://") {
		r.text = rec[0]
	} else {
		r.uri = first
	}
	label := func(s string) importAnn { return importAnn{label: strings.TrimSpace(s)} }
	switch f.problemType {
	case "IMAGE_CLASSIFICATION_SINGLE_LABEL", "TEXT_CLASSIFICATION_SINGLE_LABEL":
		if len(rest) > 1 {
			return r, fmt.Errorf("a single-label line has one LABEL")
		}
		if len(rest) == 1 && strings.TrimSpace(rest[0]) != "" {
			r.anns = append(r.anns, label(rest[0]))
		}
	case "IMAGE_CLASSIFICATION_MULTI_LABEL", "TEXT_CLASSIFICATION_MULTI_LABEL":
		for _, l := range rest {
			if strings.TrimSpace(l) != "" {
				r.anns = append(r.anns, label(l))
			}
		}
	case "IMAGE_BOUNDING_BOX":
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			break
		}
		if len(rest) != 9 {
			return r, fmt.Errorf("a bounding box line is [ML_USE],GCS_FILE_PATH,LABEL and 8 vertex columns (X_MIN,Y_MIN,,,X_MAX,Y_MAX,, or four x,y vertices)")
		}
		v := rest[1:]
		var xs, ys []float64
		for i := 0; i < 8; i += 2 {
			if strings.TrimSpace(v[i]) == "" && strings.TrimSpace(v[i+1]) == "" {
				continue
			}
			x, ok1 := num(v[i])
			y, ok2 := num(v[i+1])
			if !ok1 || !ok2 {
				return r, fmt.Errorf("vertex %d is not two numbers", i/2+1)
			}
			xs, ys = append(xs, x), append(ys, y)
		}
		if len(xs) < 2 {
			return r, fmt.Errorf("a bounding box needs at least two vertices")
		}
		b, err := box(map[string]any{"xMin": minOf(xs), "xMax": maxOf(xs), "yMin": minOf(ys), "yMax": maxOf(ys)})
		if err != nil {
			return r, err
		}
		a := label(rest[0])
		a.payload = b
		r.anns = append(r.anns, a)
	case "VIDEO_CLASSIFICATION":
		if len(rest) == 0 || strings.TrimSpace(rest[0]) == "" {
			break
		}
		var stv, env any
		if len(rest) > 1 {
			stv = rest[1]
		}
		if len(rest) > 2 {
			env = rest[2]
		}
		st, err := duration(stv, "0s")
		if err != nil {
			return r, err
		}
		en, err := duration(env, "Infinity")
		if err != nil {
			return r, err
		}
		a := label(rest[0])
		a.payload = map[string]any{"timeSegment": map[string]any{"startTimeOffset": st, "endTimeOffset": en}}
		r.anns = append(r.anns, a)
	default:
		return r, fmt.Errorf("the %s import format is read from JSON Lines (.jsonl) files", f.file)
	}
	for _, a := range r.anns {
		if !csvLabelRE(a.label) {
			return r, fmt.Errorf("label %q: labels must start with a letter and only contain letters, numbers, and underscores", a.label)
		}
	}
	return r, nil
}

func csvLabelRE(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if i == 0 && !letter {
			return false
		}
		if !letter && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

func minOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = math.Min(m, x)
	}
	return m
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = math.Max(m, x)
	}
	return m
}

// readImportFile reads an import file's records, by its extension.
func readImportFile(f *ioFormat, name string, data []byte) ([]importRec, error) {
	var out []importRec
	if strings.HasSuffix(strings.ToLower(name), ".csv") {
		rd := csv.NewReader(strings.NewReader(string(data)))
		rd.FieldsPerRecord = -1
		recs, err := rd.ReadAll()
		if err != nil {
			return nil, apierror.InvalidArgument("%s is not CSV: %v", name, err)
		}
		for i, rec := range recs {
			r, err := parseCSVRecord(f, rec)
			if err != nil {
				return nil, apierror.InvalidArgument("%s line %d: %v", name, i+1, err)
			}
			r.line = i + 1
			out = append(out, r)
		}
		return out, nil
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		r, err := parseJSONLine(f, line)
		if err != nil {
			return nil, apierror.InvalidArgument("%s line %d: %v", name, i+1, err)
		}
		r.line = i + 1
		out = append(out, r)
	}
	return out, nil
}

// expandURIs is the objects gs:// URIs name, wildcards expanded.
func expandURIs(ctx context.Context, runner PipelineRunner, field string, uris []string) ([][2]string, error) {
	var out [][2]string
	for _, u := range uris {
		bucket, object, err := splitNotebookGCS(field, u, true)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(object, "*") {
			out = append(out, [2]string{bucket, object})
			continue
		}
		names, _, err := expandGCSURI(ctx, runner, u)
		if err != nil {
			return nil, apierror.InvalidArgument("%s %q matches no object", field, u)
		}
		for _, n := range names {
			out = append(out, [2]string{bucket, n})
		}
	}
	return out, nil
}

func mimeOf(name string) string {
	t := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if t == "" {
		switch strings.ToLower(path.Ext(name)) {
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".png":
			return "image/png"
		case ".gif":
			return "image/gif"
		case ".bmp":
			return "image/bmp"
		case ".ico":
			return "image/vnd.microsoft.icon"
		case ".webp":
			return "image/webp"
		case ".mp4":
			return "video/mp4"
		case ".avi":
			return "video/x-msvideo"
		case ".mov":
			return "video/quicktime"
		case ".txt":
			return "text/plain"
		}
		return "application/octet-stream"
	}
	if i := strings.Index(t, ";"); i >= 0 {
		t = t[:i]
	}
	return t
}

func toValue(m map[string]any) *structpb.Value {
	v, err := structpb.NewValue(m)
	if err != nil {
		return structpb.NewStructValue(&structpb.Struct{})
	}
	return v
}

func mergeLabels(dst map[string]string, srcs ...map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for _, s := range srcs {
		for k, v := range s {
			dst[k] = v
		}
	}
	return dst
}

// importData runs one import config on the dataset.
func (s *Server) importData(ctx context.Context, runner PipelineRunner, dataset string, c *aiplatformpb.ImportDataConfig, f *ioFormat) error {
	files, err := expandURIs(ctx, runner, "import_configs.gcs_source.uris", c.GetGcsSource().GetUris())
	if err != nil {
		return err
	}
	type item struct {
		rec     importRec
		payload map[string]any
		hash    string
	}
	var items []item
	for _, bo := range files {
		data, err := runner.ReadObject(ctx, bo[0], bo[1], 0)
		if err != nil {
			return apierror.InvalidArgument("the import file gs://%s/%s cannot be read: %v", bo[0], bo[1], err)
		}
		recs, err := readImportFile(f, "gs://"+bo[0]+"/"+bo[1], data)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.labels[mlUseLabel] != "" {
				switch r.labels[mlUseLabel] {
				case "training", "test", "validation":
				default:
					return apierror.InvalidArgument("gs://%s/%s line %d: %s must be training, test or validation", bo[0], bo[1], r.line, mlUseLabel)
				}
			}
			var payload map[string]any
			var content []byte
			if r.text != "" {
				if len(r.text) > 10<<20 {
					return apierror.InvalidArgument("gs://%s/%s line %d: textContent is up to 10 MB", bo[0], bo[1], r.line)
				}
				content = []byte(r.text)
				payload = map[string]any{"content": r.text}
			} else {
				b, o, err := splitNotebookGCS("the import file's URI", r.uri, false)
				if err != nil {
					return apierror.InvalidArgument("gs://%s/%s line %d: %v", bo[0], bo[1], r.line, apierror.From(err).Message)
				}
				data, err := runner.ReadObject(ctx, b, o, 0)
				if err != nil {
					return apierror.InvalidArgument("gs://%s/%s line %d: %s cannot be read: %v", bo[0], bo[1], r.line, r.uri, err)
				}
				switch {
				case f.kind == "image" && len(data) > 30<<20:
					return apierror.InvalidArgument("gs://%s/%s line %d: %s is larger than the 30 MB an image can be", bo[0], bo[1], r.line, r.uri)
				case f.kind == "text" && len(data) > 10<<20:
					return apierror.InvalidArgument("gs://%s/%s line %d: %s is larger than the 10 MB a text can be", bo[0], bo[1], r.line, r.uri)
				}
				content = data
				if f.kind == "text" {
					payload = map[string]any{"content": string(data), "gcsUri": r.uri, "mimeType": "text/plain"}
				} else {
					payload = map[string]any{"gcsUri": r.uri, "mimeType": mimeOf(o)}
				}
			}
			sum := sha256.Sum256(content)
			items = append(items, item{rec: r, payload: payload, hash: hex.EncodeToString(sum[:])})
		}
	}
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	ds := st.dsets()
	parent, ok := ds.datasets[dataset]
	if !ok {
		return apierror.NotFound("Dataset %s not found", dataset)
	}
	now := timestamppb.New(st.now())
	// The annotation set of the problem type.
	var set *aiplatformpb.SavedQuery
	for _, k := range sortedNames(ds.queries) {
		if strings.HasPrefix(k, dataset+"/savedQueries/") && ds.queries[k].GetProblemType() == f.problemType {
			set = ds.queries[k]
			break
		}
	}
	if set == nil {
		set = &aiplatformpb.SavedQuery{DisplayName: strings.ToLower(f.problemType), ProblemType: f.problemType, CreateTime: now, UpdateTime: now,
			Etag: dsEtag(), SupportAutomlTraining: true}
		set.Name = uniqueName(dataset+"/savedQueries/", func(n string) bool { return ds.queries[n] != nil })
		set.AnnotationFilter = annotationSetLabel + "=" + path.Base(set.Name)
		ds.queries[set.Name] = set
	}
	setID := path.Base(set.Name)
	specByName := map[string]*aiplatformpb.AnnotationSpec{}
	for _, k := range sortedNames(ds.specs) {
		if strings.HasPrefix(k, dataset+"/annotationSpecs/") {
			specByName[ds.specs[k].GetDisplayName()] = ds.specs[k]
		}
	}
	byHash := map[string]string{}
	for n, h := range ds.hashes {
		if strings.HasPrefix(n, dataset+"/") {
			byHash[h] = n
		}
	}
	for _, it := range items {
		labels := mergeLabels(nil, c.GetDataItemLabels(), it.rec.labels)
		if it.rec.mlUse != "" {
			labels[mlUseLabel] = it.rec.mlUse
		}
		var di *aiplatformpb.DataItem
		if n, ok := byHash[it.hash]; ok && ds.items[n] != nil {
			di = ds.items[n]
			di.Labels = mergeLabels(di.Labels, labels)
			di.UpdateTime = now
		} else {
			di = &aiplatformpb.DataItem{CreateTime: now, UpdateTime: now, Payload: toValue(it.payload), Etag: dsEtag()}
			if len(labels) > 0 {
				di.Labels = labels
			}
			di.Name = uniqueName(dataset+"/dataItems/", func(n string) bool { return ds.items[n] != nil })
			ds.items[di.Name] = di
			ds.hashes[di.Name] = it.hash
			byHash[it.hash] = di.Name
		}
		for _, a := range it.rec.anns {
			payload := map[string]any{}
			for k, v := range a.payload {
				payload[k] = v
			}
			if a.label != "" {
				if utf8Len(a.label) > 128 {
					return apierror.InvalidArgument("annotation label %q: an annotation spec's display name can be up to 128 characters long", a.label)
				}
				sp := specByName[a.label]
				if sp == nil {
					sp = &aiplatformpb.AnnotationSpec{DisplayName: a.label, CreateTime: now, UpdateTime: now, Etag: dsEtag()}
					sp.Name = uniqueName(dataset+"/annotationSpecs/", func(n string) bool { return ds.specs[n] != nil })
					ds.specs[sp.Name] = sp
					specByName[a.label] = sp
				}
				payload["annotationSpecId"] = path.Base(sp.Name)
				payload["displayName"] = a.label
			}
			if f.problemType == "TEXT_EXTRACTION" {
				seg := payload["textSegment"].(map[string]any)
				text := []rune(it.payload["content"].(string))
				st, en := int(seg["startOffset"].(float64)), int(seg["endOffset"].(float64))
				if en > len(text) {
					return apierror.InvalidArgument("line %d: endOffset %d is past the text's end (%d characters)", it.rec.line, en, len(text))
				}
				seg["content"] = string(text[st:en])
				seg["startOffset"], seg["endOffset"] = strconv.Itoa(st), strconv.Itoa(en)
			}
			alabels := mergeLabels(nil, c.GetAnnotationLabels(), a.labels)
			alabels[annotationSetLabel] = setID
			alabels[payloadSchemaLabel] = f.title
			an := &aiplatformpb.Annotation{PayloadSchemaUri: annotationPrefix + f.annotation, Payload: toValue(payload), Labels: alabels,
				CreateTime: now, UpdateTime: now, Etag: dsEtag(), AnnotationSource: &aiplatformpb.UserActionReference{}}
			dup := false
			for _, k := range sortedNames(ds.anns) {
				if strings.HasPrefix(k, di.Name+"/annotations/") {
					o := ds.anns[k]
					if o.GetPayloadSchemaUri() == an.PayloadSchemaUri && proto.Equal(o.GetPayload(), an.Payload) && mapsEqual(o.GetLabels(), alabels) {
						dup = true
						break
					}
				}
			}
			if dup {
				continue
			}
			an.Name = uniqueName(di.Name+"/annotations/", func(n string) bool { return ds.anns[n] != nil })
			ds.anns[an.Name] = an
		}
	}
	set.UpdateTime = now
	parent.UpdateTime = now
	st.save()
	return nil
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// userLabels are labels less the system ones.
func userLabels(l map[string]string, keepMLUse bool) map[string]any {
	out := map[string]any{}
	for k, v := range l {
		if strings.HasPrefix(k, "aiplatform.googleapis.com/") && !(keepMLUse && k == mlUseLabel) {
			continue
		}
		out[k] = v
	}
	return out
}

// exportLine is a data item and its annotations of format f, in f's
// import format; mlUse, when set, is its ml_use.
func exportLine(f *ioFormat, di *aiplatformpb.DataItem, anns []*aiplatformpb.Annotation, mlUse string) map[string]any {
	p := di.GetPayload().GetStructValue().AsMap()
	line := map[string]any{}
	switch f.kind {
	case "image":
		line["imageGcsUri"] = p["gcsUri"]
	case "video":
		line["videoGcsUri"] = p["gcsUri"]
	case "text":
		if u, ok := p["gcsUri"].(string); ok && u != "" {
			line["textGcsUri"] = u
		} else {
			line["textContent"] = p["content"]
		}
	}
	labels := userLabels(di.GetLabels(), true)
	if mlUse != "" {
		labels[mlUseLabel] = mlUse
	}
	if len(labels) > 0 {
		line["dataItemResourceLabels"] = labels
	}
	var out []map[string]any
	for _, a := range anns {
		ap := a.GetPayload().GetStructValue().AsMap()
		o := map[string]any{}
		if n, ok := ap["displayName"]; ok {
			o["displayName"] = n
		}
		if l := userLabels(a.GetLabels(), false); len(l) > 0 {
			o["annotationResourceLabels"] = l
		}
		switch f.problemType {
		case "IMAGE_BOUNDING_BOX":
			for _, k := range []string{"xMin", "yMin", "xMax", "yMax"} {
				o[k] = ap[k]
			}
		case "VIDEO_OBJECT_TRACKING":
			for _, k := range []string{"xMin", "yMin", "xMax", "yMax", "timeOffset", "instanceId"} {
				if v, ok := ap[k]; ok {
					o[k] = v
				}
			}
		case "TEXT_EXTRACTION":
			seg, _ := ap["textSegment"].(map[string]any)
			s, _ := num(seg["startOffset"])
			e, _ := num(seg["endOffset"])
			o["startOffset"], o["endOffset"] = s, e
		case "TEXT_SENTIMENT":
			o["sentiment"], o["sentimentMax"] = ap["sentiment"], ap["sentimentMax"]
		case "VIDEO_CLASSIFICATION", "VIDEO_ACTION_RECOGNITION":
			seg, _ := ap["timeSegment"].(map[string]any)
			o["startTime"], o["endTime"] = seg["startTimeOffset"], seg["endTimeOffset"]
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		return line
	}
	switch f.problemType {
	case "IMAGE_CLASSIFICATION_SINGLE_LABEL", "TEXT_CLASSIFICATION_SINGLE_LABEL":
		line["classificationAnnotation"] = out[0]
	case "IMAGE_CLASSIFICATION_MULTI_LABEL", "TEXT_CLASSIFICATION_MULTI_LABEL":
		line["classificationAnnotations"] = out
	case "IMAGE_BOUNDING_BOX":
		line["boundingBoxAnnotations"] = out
	case "TEXT_EXTRACTION":
		line["textSegmentAnnotations"] = out
	case "TEXT_SENTIMENT":
		line["sentimentAnnotation"] = out[0]
	case "VIDEO_CLASSIFICATION", "VIDEO_ACTION_RECOGNITION":
		line["timeSegmentAnnotations"] = out
	case "VIDEO_OBJECT_TRACKING":
		line["temporalBoundingBoxAnnotations"] = out
	}
	return line
}

// dsSelect is what an export or a training pipeline reads of a dataset.
type dsSelect struct {
	annotationsFilter string
	savedQuery        string // a SavedQuery ID
	schema            string // an annotation schema URI
	fraction          *aiplatformpb.FractionSplit
	filter            *aiplatformpb.FilterSplit
}

// dsView is a dataset's items and annotations, copied under st.mu.
type dsView struct {
	dataset *aiplatformpb.Dataset
	items   []*aiplatformpb.DataItem
	anns    map[string][]*aiplatformpb.Annotation // by item
	sets    []*aiplatformpb.SavedQuery
}

func (s *Server) datasetView(dataset string) (dsView, error) {
	st := s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	ds := st.dsets()
	d, err := ds.dataset(dataset)
	if err != nil {
		return dsView{}, err
	}
	v := dsView{dataset: proto.Clone(d).(*aiplatformpb.Dataset), anns: map[string][]*aiplatformpb.Annotation{}}
	for _, k := range sortedNames(ds.items) {
		if strings.HasPrefix(k, dataset+"/dataItems/") {
			v.items = append(v.items, proto.Clone(ds.items[k]).(*aiplatformpb.DataItem))
		}
	}
	for _, k := range sortedNames(ds.anns) {
		if strings.HasPrefix(k, dataset+"/dataItems/") {
			item := k[:strings.Index(k, "/annotations/")]
			v.anns[item] = append(v.anns[item], proto.Clone(ds.anns[k]).(*aiplatformpb.Annotation))
		}
	}
	for _, k := range sortedNames(ds.queries) {
		if strings.HasPrefix(k, dataset+"/savedQueries/") {
			v.sets = append(v.sets, proto.Clone(ds.queries[k]).(*aiplatformpb.SavedQuery))
		}
	}
	return v, nil
}

// annotationsOf are an item's annotations the selection keeps of set
// (a SavedQuery ID, "" any).
func (v dsView) annotationsOf(item, set string, sel dsSelect) ([]*aiplatformpb.Annotation, error) {
	var out []*aiplatformpb.Annotation
	for _, a := range v.anns[item] {
		if set != "" && a.GetLabels()[annotationSetLabel] != set {
			continue
		}
		if sel.schema != "" && a.GetPayloadSchemaUri() != sel.schema {
			continue
		}
		out = append(out, a)
	}
	return aipfilter.Apply(sel.annotationsFilter, out, annFilter...)
}

// assignItems gives each item its role: training, validation or test, or ""
// (left out), by the selection's split.
func assignItems(items []*aiplatformpb.DataItem, sel dsSelect) (map[string]string, error) {
	out := map[string]string{}
	if f := sel.filter; f != nil {
		match := func(filter string) (map[string]bool, error) {
			m := map[string]bool{}
			if strings.TrimSpace(filter) == "-" {
				return m, nil
			}
			got, err := aipfilter.Apply(filter, items, dataItemFilter...)
			if err != nil {
				return nil, err
			}
			for _, it := range got {
				m[it.GetName()] = true
			}
			return m, nil
		}
		var sets [3]map[string]bool
		for i, filter := range []string{f.GetTrainingFilter(), f.GetValidationFilter(), f.GetTestFilter()} {
			if filter == "" {
				return nil, apierror.InvalidArgument("filter_split: training_filter, validation_filter and test_filter are required ('-' matches nothing)")
			}
			m, err := match(filter)
			if err != nil {
				return nil, err
			}
			sets[i] = m
		}
		for _, it := range items {
			for i, role := range []string{"training", "validation", "test"} {
				if sets[i][it.GetName()] {
					out[it.GetName()] = role
					break
				}
			}
		}
		return out, nil
	}
	tr, va, te := 0.8, 0.1, 0.1
	if f := sel.fraction; f != nil && (f.GetTrainingFraction() != 0 || f.GetValidationFraction() != 0 || f.GetTestFraction() != 0) {
		tr, va, te = f.GetTrainingFraction(), f.GetValidationFraction(), f.GetTestFraction()
	}
	for _, it := range items {
		out[it.GetName()] = fractionRole(it.GetName(), tr, va, te)
	}
	return out, nil
}

// fractionRole is a deterministic role for key: a hash in [0, 1) against
// the fractions, the remainder of fractions summing to less than 1 to
// training.
func fractionRole(key string, tr, va, te float64) string {
	sum := sha256.Sum256([]byte(key))
	u := float64(uint64(sum[0])<<24|uint64(sum[1])<<16|uint64(sum[2])<<8|uint64(sum[3])) / float64(1<<32)
	switch {
	case u < va:
		return "validation"
	case u < va+te:
		return "test"
	}
	_ = tr
	return "training"
}

func checkFractions(tr, va, te float64, field string) error {
	for _, v := range []float64{tr, va, te} {
		if v < 0 || v > 1 || math.IsNaN(v) {
			return apierror.InvalidArgument("%s: each fraction must be in [0, 1]", field)
		}
	}
	if tr+va+te > 1+1e-9 {
		return apierror.InvalidArgument("%s: the fractions must sum to up to 1, and they sum to %v", field, tr+va+te)
	}
	return nil
}

func jsonl(lines []map[string]any) []byte {
	var b strings.Builder
	for _, l := range lines {
		j, _ := json.Marshal(l)
		b.Write(j)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// exportSets writes an ExportData: a subdirectory a annotation set under
// dir, its lines in the set's import format.
func (s *Server) exportSets(ctx context.Context, runner PipelineRunner, dataset string, sel dsSelect, dir string) ([]string, error) {
	v, err := s.datasetView(dataset)
	if err != nil {
		return nil, err
	}
	roles := map[string]string{}
	if sel.fraction != nil || sel.filter != nil {
		if roles, err = assignItems(v.items, sel); err != nil {
			return nil, err
		}
	}
	var files []string
	for _, set := range v.sets {
		id := path.Base(set.GetName())
		if sel.savedQuery != "" && sel.savedQuery != id {
			continue
		}
		f := ioFormatByProblem(set.GetProblemType())
		if f == nil || (sel.schema != "" && annotationPrefix+f.annotation != sel.schema) {
			continue
		}
		var lines []map[string]any
		for _, it := range v.items {
			if (sel.fraction != nil || sel.filter != nil) && roles[it.GetName()] == "" {
				continue
			}
			anns, err := v.annotationsOf(it.GetName(), id, sel)
			if err != nil {
				return nil, err
			}
			lines = append(lines, exportLine(f, it, anns, roles[it.GetName()]))
		}
		uri := dir + "/" + set.GetDisplayName() + "/data-00000-of-00001.jsonl"
		bucket, object, err := splitNotebookGCS("export", uri, false)
		if err != nil {
			return nil, err
		}
		if err := runner.WriteObject(ctx, bucket, object, "application/jsonl", jsonl(lines)); err != nil {
			return nil, apierror.FailedPrecondition("writing %s: %v", uri, err)
		}
		files = append(files, uri)
	}
	return files, nil
}

// exportUnstructured writes an image, text or video dataset's items for
// custom training: dataset-<id>-<annotation-type>-<time>/{training,
// validation,test}-00000-of-00001.jsonl under prefix, as
// InputDataConfig.gcs_destination documents it. It answers the three
// wildcard URIs and the counts.
func (s *Server) exportUnstructured(ctx context.Context, runner PipelineRunner, dataset string, sel dsSelect, prefix string, at time.Time) ([3]string, *aiplatformpb.Model_DataStats, string, error) {
	var none [3]string
	v, err := s.datasetView(dataset)
	if err != nil {
		return none, nil, "", err
	}
	var f *ioFormat
	setID := sel.savedQuery
	for _, set := range v.sets {
		if setID != "" && path.Base(set.GetName()) != setID {
			continue
		}
		g := ioFormatByProblem(set.GetProblemType())
		if g == nil || (sel.schema != "" && annotationPrefix+g.annotation != sel.schema) {
			continue
		}
		f = g
		if setID == "" && sel.schema == "" {
			setID = "" // every annotation, in the first set's format
		}
		break
	}
	if f == nil && sel.schema != "" {
		f = ioFormatByAnnotation(sel.schema)
	}
	if f == nil {
		if setID != "" {
			return none, nil, "", apierror.NotFound("SavedQuery %s/savedQueries/%s not found", dataset, setID)
		}
		return none, nil, "", apierror.FailedPrecondition("the dataset %s has no data imported", dataset)
	}
	roles, err := assignItems(v.items, sel)
	if err != nil {
		return none, nil, "", err
	}
	dir := prefix + "/dataset-" + path.Base(dataset) + "-" + strings.ReplaceAll(strings.ToLower(f.problemType), "_", "-") + "-" + at.UTC().Format("2006-01-02T15:04:05.000Z")
	lines := map[string][]map[string]any{}
	stats := &aiplatformpb.Model_DataStats{}
	for _, it := range v.items {
		role := roles[it.GetName()]
		if role == "" {
			continue
		}
		anns, err := v.annotationsOf(it.GetName(), setID, sel)
		if err != nil {
			return none, nil, "", err
		}
		lines[role] = append(lines[role], exportLine(f, it, anns, ""))
		n := int64(len(anns))
		switch role {
		case "training":
			stats.TrainingDataItemsCount++
			stats.TrainingAnnotationsCount += n
		case "validation":
			stats.ValidationDataItemsCount++
			stats.ValidationAnnotationsCount += n
		case "test":
			stats.TestDataItemsCount++
			stats.TestAnnotationsCount += n
		}
	}
	var out [3]string
	for i, role := range []string{"training", "validation", "test"} {
		bucket, object, err := splitNotebookGCS("gcs_destination", dir+"/"+role+"-00000-of-00001.jsonl", false)
		if err != nil {
			return none, nil, "", err
		}
		if err := runner.WriteObject(ctx, bucket, object, "application/jsonl", jsonl(lines[role])); err != nil {
			return none, nil, "", apierror.FailedPrecondition("writing %s: %v", dir, err)
		}
		out[i] = dir + "/" + role + "-*.jsonl"
	}
	return out, stats, "jsonl", nil
}
