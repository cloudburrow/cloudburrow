//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
)

// TestBigQueryMLAnnotateImage (#1773): a remote model over a Cloud AI
// service, through the official cloud.google.com/go/bigquery, storage and
// bigquery/connection/apiv1 clients: CREATE MODEL … REMOTE WITH
// CONNECTION … OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_VISION_V1'),
// models.get answering remoteModelInfo.remoteServiceType (the generated
// bigquery/v2 client), and ML.ANNOTATE_IMAGE over an object table:
// DOCUMENT_TEXT_DETECTION reads a scanned page's text (local Tesseract,
// never Google) into ml_annotate_image_result's fullTextAnnotation with
// an empty ml_annotate_image_status, with each word's box in
// textAnnotations and the page's blocks, paragraphs, words and symbols
// (#1799), and an object that is not an image
// is "Bad image data." in its row's status; the object table's columns
// are kept. IMAGE_PROPERTIES answers the page's dominant colors and a
// crop hint (#1799). LOGO_DETECTION (no local model) is refused 501;
// ML.GENERATE_TEXT of the Vision model and ML.ANNOTATE_IMAGE
// over a query are 400.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLAnnotateImage(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, ds, id := visionObjectTable(t, h)
	run := func(sql string) error { return mlRun(h, c, sql) }
	conn := c.Project() + ".us." + id
	ctx := h.Context()

	if err := run(fmt.Sprintf("CREATE MODEL `%s.vision` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_VISION_V1')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL CLOUD_AI_VISION_V1: %v", err)
	}
	md, err := generatedBigQuery(t, h).Models.Get(c.Project(), ds, "vision").Context(ctx).Do()
	if err != nil {
		t.Fatalf("models.get: %v", err)
	}
	if md.RemoteModelInfo == nil || md.RemoteModelInfo.RemoteServiceType != "CLOUD_AI_VISION_V1" || md.RemoteModelInfo.Endpoint != "" ||
		!strings.HasSuffix(md.RemoteModelInfo.Connection, "/connections/"+id) {
		t.Errorf("remoteModelInfo: %+v", md.RemoteModelInfo)
	}

	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT uri, content_type, ml_annotate_image_status AS status, "+
		"JSON_VALUE(ml_annotate_image_result, '$.fullTextAnnotation.text') AS text, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.textAnnotations')) AS annotations, "+
		"JSON_VALUE(ml_annotate_image_result, '$.textAnnotations[1].description') AS word, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.textAnnotations[1].boundingPoly.vertices')) AS corners, "+
		"JSON_VALUE(ml_annotate_image_result, '$.fullTextAnnotation.pages[0].blocks[0].blockType') AS block_type, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.fullTextAnnotation.pages[0].blocks[0].paragraphs[0].words[0].symbols')) AS symbols "+
		"FROM ML.ANNOTATE_IMAGE(MODEL `%s.vision`, TABLE `%s.images`, STRUCT(['document_text_detection'] AS vision_features)) ORDER BY uri", ds, ds))
	if len(rows) != 2 {
		t.Fatalf("rows: %v", rows)
	}
	if !strings.HasSuffix(fmt.Sprint(rows[0]["uri"]), "/img/broken.png") || rows[0]["status"] != "Bad image data." || rows[0]["text"] != nil {
		t.Errorf("broken image row: %v", rows[0])
	}
	if text, _ := rows[1]["text"].(string); rows[1]["status"] != "" || !strings.Contains(text, "INVOICE No. 2026-0417") || rows[1]["content_type"] != "image/png" {
		t.Errorf("scanned page row: %v", rows[1])
	}
	// Word and block boxes (#1799): the whole text, then a word an entry
	// with its four corners; blocks of paragraphs of words of symbols.
	annotations, _ := rows[1]["annotations"].(int64)
	symbols, _ := rows[1]["symbols"].(int64)
	if r := rows[1]; r["corners"] != int64(4) || r["block_type"] != "TEXT" || r["word"] == nil || r["word"] == "" || annotations < 5 || symbols < 1 {
		t.Errorf("text boxes of the scanned page: %v", r)
	}

	// IMAGE_PROPERTIES (#1799), computed from the pixels: the scanned
	// page is mostly white paper, so its first dominant color is near
	// white and covers most of it; and a whole-image crop hint.
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT uri, ml_annotate_image_status AS status, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.imagePropertiesAnnotation.dominantColors.colors')) AS n, "+
		"LAX_INT64(ml_annotate_image_result.imagePropertiesAnnotation.dominantColors.colors[0].color.red) AS red, "+
		"LAX_FLOAT64(ml_annotate_image_result.imagePropertiesAnnotation.dominantColors.colors[0].pixelFraction) AS fraction, "+
		"LAX_FLOAT64(ml_annotate_image_result.cropHintsAnnotation.cropHints[0].importanceFraction) AS importance, "+
		"JSON_VALUE(ml_annotate_image_result, '$.fullTextAnnotation.text') AS text "+
		"FROM ML.ANNOTATE_IMAGE(MODEL `%s.vision`, TABLE `%s.images`, STRUCT(['IMAGE_PROPERTIES'] AS vision_features)) ORDER BY uri", ds, ds))
	if len(rows) != 2 || rows[0]["status"] != "Bad image data." {
		t.Fatalf("IMAGE_PROPERTIES rows: %v", rows)
	}
	r := rows[1]
	red, _ := r["red"].(int64)
	fraction, _ := r["fraction"].(float64)
	n, _ := r["n"].(int64)
	if r["status"] != "" || r["text"] != nil || r["importance"] != 1.0 || red < 200 || fraction < 0.5 || n < 1 || n > 10 {
		t.Errorf("IMAGE_PROPERTIES of the scanned page: %v", r)
	}

	if err := run(fmt.Sprintf("SELECT * FROM ML.ANNOTATE_IMAGE(MODEL `%s.vision`, TABLE `%s.images`, STRUCT(['logo_detection'] AS vision_features))", ds, ds)); err == nil ||
		!strings.Contains(err.Error(), "Not implemented here") || !strings.Contains(err.Error(), "LOGO_DETECTION") {
		t.Errorf("LOGO_DETECTION: %v", err)
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.vision`, (SELECT 'hi' AS prompt))", ds)); err == nil ||
		!strings.Contains(err.Error(), "ML.ANNOTATE_IMAGE") {
		t.Errorf("ML.GENERATE_TEXT of a Vision model: %v", err)
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.ANNOTATE_IMAGE(MODEL `%s.vision`, (SELECT * FROM `%s.images`), STRUCT(['text_detection'] AS vision_features))", ds, ds)); err == nil ||
		!strings.Contains(err.Error(), "object table") {
		t.Errorf("over a query: %v", err)
	}
}

// visionObjectTable makes a Cloud resource connection its service account
// may use, a dataset, and an object table `images` over a bucket holding
// the scanned page (img/scan.png) and an object that is not an image
// (img/broken.png), and any extra objects: the client, the dataset and the
// connection's ID.
func visionObjectTable(t *testing.T, h *Harness, extra ...map[string][]byte) (*bigquery.Client, string, string) {
	t.Helper()
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	page, err := os.ReadFile("../../internal/ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_vision"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	grantRemoteModel(t, h, cc, project, cn)
	conn := project + ".us." + id

	ds := "compat_vision_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	bh := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-vision")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	objects := map[string][]byte{"img/scan.png": page, "img/broken.png": []byte("not an image")}
	for _, e := range extra {
		for k, v := range e {
			objects[k] = v
		}
	}
	for name, data := range objects {
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType = "image/png"
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("bucket IAM Policy: %v", err)
	}
	pol.Add("serviceAccount:"+cn.GetCloudResource().GetServiceAccountId(), iam.RoleName("roles/storage.objectViewer"))
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("bucket IAM SetPolicy: %v", err)
	}
	run := func(sql string) error { return mlRun(h, c, sql) }
	if err := run(fmt.Sprintf("CREATE EXTERNAL TABLE `%s.images` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/img/*'])",
		ds, conn, bh.BucketName())); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE: %v", err)
	}
	return c, ds, id
}

// EnvVisionLabels, when set, runs TestBigQueryMLAnnotateImageLabels: its
// instance fetches MediaPipe's EfficientNet-Lite0 classifier (18.6 MB,
// from storage.googleapis.com) the first time it labels an image.
const EnvVisionLabels = "CLOUDBURROW_TEST_VISION_LABELS"

// TestBigQueryMLAnnotateImageLabels (#1799): ML.ANNOTATE_IMAGE's
// LABEL_DETECTION through the official cloud.google.com/go/bigquery,
// storage and bigquery/connection/apiv1 clients, answered by MediaPipe's
// EfficientNet-Lite0 run locally (never Vision): the scanned page's
// labelAnnotations, ten by default, most probable first, each with a
// description, a score in [0, 1] equal to its topicality and no mid (the
// labels are ImageNet's, with no Knowledge Graph ID); its top label is
// "web site", as the unit test measured; an object that is not an image is
// "Bad image data.".
//
// covers: bigquery.jobs.query
func TestBigQueryMLAnnotateImageLabels(t *testing.T) {
	if os.Getenv(EnvVisionLabels) == "" {
		t.Skip("set " + EnvVisionLabels + " to run LABEL_DETECTION: the instance fetches MediaPipe's EfficientNet-Lite0 (18.6 MB)")
	}
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, ds, id := visionObjectTable(t, h)
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.vision` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_VISION_V1')",
		ds, c.Project()+".us."+id)); err != nil {
		t.Fatalf("CREATE MODEL CLOUD_AI_VISION_V1: %v", err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT uri, ml_annotate_image_status AS status, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.labelAnnotations')) AS n, "+
		"JSON_VALUE(ml_annotate_image_result, '$.labelAnnotations[0].description') AS top, "+
		"LAX_FLOAT64(ml_annotate_image_result.labelAnnotations[0].score) AS score, "+
		"LAX_FLOAT64(ml_annotate_image_result.labelAnnotations[0].topicality) AS topicality, "+
		"LAX_FLOAT64(ml_annotate_image_result.labelAnnotations[1].score) AS second, "+
		"JSON_VALUE(ml_annotate_image_result, '$.labelAnnotations[0].mid') AS mid "+
		"FROM ML.ANNOTATE_IMAGE(MODEL `%s.vision`, TABLE `%s.images`, STRUCT(['LABEL_DETECTION'] AS vision_features)) ORDER BY uri", ds, ds))
	if len(rows) != 2 || rows[0]["status"] != "Bad image data." {
		t.Fatalf("LABEL_DETECTION rows: %v", rows)
	}
	r := rows[1]
	score, _ := r["score"].(float64)
	second, _ := r["second"].(float64)
	if r["status"] != "" || r["n"] != int64(10) || r["top"] != "web site" || score <= 0 || score > 1 || r["topicality"] != r["score"] ||
		second > score || r["mid"] != nil {
		t.Errorf("LABEL_DETECTION of the scanned page: %v", r)
	}
}

// EnvVisionDetect, when set, runs TestBigQueryMLAnnotateImageDetections:
// its instance fetches MediaPipe's EfficientDet-Lite0 object detector
// (13.8 MB) and BlazeFace short-range face detector (0.2 MB) from
// storage.googleapis.com the first time it needs them.
const EnvVisionDetect = "CLOUDBURROW_TEST_VISION_DETECT"

// TestBigQueryMLAnnotateImageDetections (#1799): ML.ANNOTATE_IMAGE's
// FACE_DETECTION and OBJECT_LOCALIZATION through the official
// cloud.google.com/go/bigquery, storage and bigquery/connection/apiv1
// clients, answered by MediaPipe's BlazeFace and EfficientDet-Lite0 run
// locally (never Vision). The object table holds the scanned page and
// internal/tflitemodel's image_big.png, in which BlazeFace finds one face
// (score 0.566, as the unit test measured): its faceAnnotations entry has
// an fdBoundingPoly and a boundingPoly of four vertices, six landmarks
// starting with LEFT_EYE, its detectionConfidence, and joyLikelihood and
// headwearLikelihood UNKNOWN; the page has no face. Neither has a COCO
// object, so localizedObjectAnnotations is omitted, and the status is
// empty; an object that is not an image is "Bad image data.".
//
// covers: bigquery.jobs.query
func TestBigQueryMLAnnotateImageDetections(t *testing.T) {
	if os.Getenv(EnvVisionDetect) == "" {
		t.Skip("set " + EnvVisionDetect + " to run FACE_DETECTION and OBJECT_LOCALIZATION: the instance fetches MediaPipe's EfficientDet-Lite0 (13.8 MB) and BlazeFace (0.2 MB)")
	}
	h := New(t)
	h.Endpoint(EnvLocalAI)
	face, err := os.ReadFile("../../internal/tflitemodel/testdata/image_big.png")
	if err != nil {
		t.Fatal(err)
	}
	c, ds, id := visionObjectTable(t, h, map[string][]byte{"img/face.png": face})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.vision` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_VISION_V1')",
		ds, c.Project()+".us."+id)); err != nil {
		t.Fatalf("CREATE MODEL CLOUD_AI_VISION_V1: %v", err)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT uri, ml_annotate_image_status AS status, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.faceAnnotations')) AS faces, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.faceAnnotations[0].fdBoundingPoly.vertices')) AS fd, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.faceAnnotations[0].boundingPoly.vertices')) AS bp, "+
		"ARRAY_LENGTH(JSON_QUERY_ARRAY(ml_annotate_image_result, '$.faceAnnotations[0].landmarks')) AS marks, "+
		"JSON_VALUE(ml_annotate_image_result, '$.faceAnnotations[0].landmarks[0].type') AS first, "+
		"LAX_FLOAT64(ml_annotate_image_result.faceAnnotations[0].detectionConfidence) AS confidence, "+
		"JSON_VALUE(ml_annotate_image_result, '$.faceAnnotations[0].joyLikelihood') AS joy, "+
		"JSON_VALUE(ml_annotate_image_result, '$.faceAnnotations[0].headwearLikelihood') AS headwear, "+
		"JSON_QUERY(ml_annotate_image_result, '$.localizedObjectAnnotations') AS objects "+
		"FROM ML.ANNOTATE_IMAGE(MODEL `%s.vision`, TABLE `%s.images`, STRUCT(['FACE_DETECTION', 'OBJECT_LOCALIZATION'] AS vision_features)) ORDER BY uri", ds, ds))
	if len(rows) != 3 || rows[0]["status"] != "Bad image data." {
		t.Fatalf("rows: %v", rows)
	}
	r := rows[1]
	conf, _ := r["confidence"].(float64)
	if r["uri"] == nil || !strings.HasSuffix(r["uri"].(string), "/img/face.png") || r["status"] != "" || r["faces"] != int64(1) ||
		r["fd"] != int64(4) || r["bp"] != int64(4) || r["marks"] != int64(6) || r["first"] != "LEFT_EYE" || conf < 0.56 || conf > 0.57 ||
		r["joy"] != "UNKNOWN" || r["headwear"] != "UNKNOWN" || r["objects"] != nil {
		t.Errorf("the face image: %v", r)
	}
	if r := rows[2]; r["status"] != "" || r["faces"] != nil || r["objects"] != nil {
		t.Errorf("the scanned page: %v", r)
	}
}

// TestBigQueryMLTranscribe (#1799): ML.TRANSCRIBE over a
// CLOUD_AI_SPEECH_TO_TEXT_V2 remote model and an object table of audio,
// through the official cloud.google.com/go/bigquery, storage and
// bigquery/connection/apiv1 clients. Each file is transcribed by the local
// Vertex AI endpoint's Speech-to-Text v2 recognize route with whisper.cpp
// (never Google): whisper.cpp's own sample, eleven seconds of John F.
// Kennedy's inaugural address as a WAV file, answers transcripts,
// ml_transcribe_result (results with alternatives, language_code and
// result_end_offset, metadata.total_billed_duration, and each word's
// offsets with enable_word_time_offsets) and an empty ml_transcribe_status;
// the same speech as a FLAC file (decoded locally since #1799) answers the
// same words; an Ogg Opus file (no local decoder) and a text file are their
// rows' errors.
// SPEECH_RECOGNIZER is 501 at CREATE MODEL; a missing recognition_config, a
// model other than chirp and a config option with no local equivalent
// (profanity_filter, 501) are refused before anything is read.
//
// covers: bigquery.jobs.query
func TestBigQueryMLTranscribe(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	wav, err := os.ReadFile("../../internal/speech/testdata/jfk.wav")
	if err != nil {
		t.Fatal(err)
	}
	flac, err := os.ReadFile("../../internal/speech/testdata/jfk.flac")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_speech"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	grantRemoteModel(t, h, cc, project, cn)
	conn := project + ".us." + id

	ds := "compat_speech_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	bh := sc.Bucket(strings.ReplaceAll(h.Project(), "_", "-") + "-speech")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	for name, o := range map[string]struct {
		ct   string
		data []byte
	}{
		"audio/jfk.wav":   {"audio/wav", wav},
		"audio/jfk.flac":  {"audio/flac", flac},
		"audio/song.ogg":  {"audio/ogg", []byte("OggS\x00\x02 not really")},
		"audio/notes.txt": {"text/plain", []byte("not audio at all")},
	} {
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType = o.ct
		if _, err := w.Write(o.data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("bucket IAM Policy: %v", err)
	}
	pol.Add("serviceAccount:"+cn.GetCloudResource().GetServiceAccountId(), iam.RoleName("roles/storage.objectViewer"))
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("bucket IAM SetPolicy: %v", err)
	}
	run := func(sql string) error { return mlRun(h, c, sql) }
	if err := run(fmt.Sprintf("CREATE EXTERNAL TABLE `%s.audio` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/audio/*'])",
		ds, conn, bh.BucketName())); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.refused` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_SPEECH_TO_TEXT_V2', "+
		"SPEECH_RECOGNIZER = 'projects/p/locations/us/recognizers/r')", ds, conn)); err == nil || !strings.Contains(err.Error(), "Not implemented here") ||
		!strings.Contains(err.Error(), "SPEECH_RECOGNIZER") {
		t.Errorf("SPEECH_RECOGNIZER: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.speech` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = 'CLOUD_AI_SPEECH_TO_TEXT_V2')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL CLOUD_AI_SPEECH_TO_TEXT_V2: %v", err)
	}
	md, err := generatedBigQuery(t, h).Models.Get(c.Project(), ds, "speech").Context(ctx).Do()
	if err != nil {
		t.Fatalf("models.get: %v", err)
	}
	if md.RemoteModelInfo == nil || md.RemoteModelInfo.RemoteServiceType != "CLOUD_AI_SPEECH_TO_TEXT_V2" || md.RemoteModelInfo.Endpoint != "" {
		t.Errorf("remoteModelInfo: %+v", md.RemoteModelInfo)
	}

	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT uri, content_type, transcripts, ml_transcribe_status AS status, "+
		"JSON_VALUE(ml_transcribe_result, '$.results[0].language_code') AS lang, "+
		"LAX_INT64(ml_transcribe_result.metadata.total_billed_duration.seconds) AS billed, "+
		"LAX_INT64(ml_transcribe_result.results[0].result_end_offset.seconds) AS end_s, "+
		"LAX_FLOAT64(ml_transcribe_result.results[0].alternatives[0].confidence) AS confidence, "+
		"JSON_VALUE(ml_transcribe_result, '$.results[0].alternatives[0].words[0].word') AS word, "+
		"LAX_INT64(ml_transcribe_result.results[0].alternatives[0].words[1].start_offset.nanos) AS word_nanos "+
		`FROM ML.TRANSCRIBE(MODEL `+"`%s.speech`, TABLE `%s.audio`"+`, recognition_config => (JSON '{"language_codes": ["en-US"], "model": "chirp", `+
		`"auto_decoding_config": {}, "features": {"enable_word_time_offsets": true}}')) ORDER BY uri`, ds, ds))
	if len(rows) != 4 {
		t.Fatalf("rows: %v", rows)
	}
	jfkFLAC, jfk, notes, song := rows[0], rows[1], rows[2], rows[3]
	text, _ := jfk["transcripts"].(string)
	confidence, _ := jfk["confidence"].(float64)
	if !strings.HasSuffix(fmt.Sprint(jfk["uri"]), "/audio/jfk.wav") || jfk["status"] != "" || jfk["content_type"] != "audio/wav" ||
		!strings.Contains(strings.ToLower(text), "ask not what your country can do for you") || jfk["lang"] != "en-us" ||
		jfk["billed"] != int64(11) || jfk["end_s"] != int64(11) || confidence <= 0 || confidence > 1 || jfk["word"] != "And" || jfk["word_nanos"] == nil {
		t.Errorf("jfk.wav row: %v", jfk)
	}
	if flacText, _ := jfkFLAC["transcripts"].(string); jfkFLAC["status"] != "" || flacText != text || jfkFLAC["billed"] != int64(11) {
		t.Errorf("jfk.flac row: %v", jfkFLAC)
	}
	if status, _ := song["status"].(string); !strings.Contains(status, "OGG_OPUS") || song["transcripts"] != nil {
		t.Errorf("Ogg Opus row: %v", song)
	}
	if status, _ := notes["status"].(string); !strings.Contains(status, "INVALID_ARGUMENT") || notes["transcripts"] != nil {
		t.Errorf("text row: %v", notes)
	}

	for cfg, want := range map[string]string{
		"": "requires recognition_config",
		`, recognition_config => (JSON '{"language_codes": ["en-US"], "model": "long", "auto_decoding_config": {}}')`:                                          "chirp",
		`, recognition_config => (JSON '{"language_codes": ["en-US"], "model": "chirp", "auto_decoding_config": {}, "features": {"profanity_filter": true}}')`: "Not implemented here",
	} {
		if err := run(fmt.Sprintf("SELECT * FROM ML.TRANSCRIBE(MODEL `%s.speech`, TABLE `%s.audio`%s)", ds, ds, cfg)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ML.TRANSCRIBE%s: %v, want %q", cfg, err, want)
		}
	}
}

// TestBigQueryMLTranslate (#1799): ML.TRANSLATE over a
// CLOUD_AI_TRANSLATE_V3 remote model, through the official
// cloud.google.com/go/bigquery and bigquery/connection/apiv1 clients and
// the generated bigquery/v2. Each distinct text_content is sent to the
// local Vertex AI endpoint's Translation v3 route (never Google):
// DETECT_LANGUAGE answers ml_translate_result.languages[0] from MediaPipe's
// Language Detector (fr, de, ja for the three sentences) with an empty
// ml_translate_status, over a TABLE and over a query that aliases
// text_content; a NULL text has no result. TRANSLATE_TEXT runs the
// instance's local Gemma model: with one, the page's sample shape
// (glossary_translations, translation_memory_translations, and
// translations[0] with detected_language_code and translated_text) for the
// three sentences into en and es, a NULL text with no result; each
// translation is one line with no preamble, carries its sentence's meaning
// (en: "hello", "good morning", "weather"...; es: the detector reads it as
// es), and a second run answers the same words; with none (an instance
// without --local-ai-model), 501. An invalid target language is the row's
// INVALID_ARGUMENT; a missing target, a bad mode, an unknown option, an
// input without text_content and another function of the model are
// refused before anything is sent.
//
// covers: bigquery.jobs.query
func TestBigQueryMLTranslate(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_translate"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	grantRemoteModel(t, h, cc, project, cn)
	conn := project + ".us." + id

	ds := "compat_translate_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	run := func(sql string) error { return mlRun(h, c, sql) }
	if err := run(fmt.Sprintf("CREATE TABLE `%s.texts` AS SELECT 1 AS id, 'Bonjour tout le monde, comment allez-vous ?' AS text_content UNION ALL "+
		"SELECT 2, 'Guten Morgen, wie geht es dir heute?' UNION ALL SELECT 3, 'こんにちは、今日はいい天気ですね。' UNION ALL SELECT 4, CAST(NULL AS STRING)", ds)); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE TABLE `%s.comments` AS SELECT 'Hola, ¿cómo estás?' AS comment", ds)); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE MODEL `%s.translate` REMOTE WITH CONNECTION `%s` OPTIONS(remote_service_type = 'cloud_ai_translate_v3')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL CLOUD_AI_TRANSLATE_V3: %v", err)
	}
	md, err := generatedBigQuery(t, h).Models.Get(c.Project(), ds, "translate").Context(ctx).Do()
	if err != nil {
		t.Fatalf("models.get: %v", err)
	}
	if md.RemoteModelInfo == nil || md.RemoteModelInfo.RemoteServiceType != "CLOUD_AI_TRANSLATE_V3" || md.RemoteModelInfo.Endpoint != "" {
		t.Errorf("remoteModelInfo: %+v", md.RemoteModelInfo)
	}

	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT id, JSON_VALUE(ml_translate_result, '$.languages[0].language_code') AS code, "+
		"FLOAT64(ml_translate_result.languages[0].confidence) AS confidence, ml_translate_status AS status FROM ML.TRANSLATE(MODEL `%s.translate`, "+
		"TABLE `%s.texts`, STRUCT('DETECT_LANGUAGE' AS translate_mode)) ORDER BY id", ds, ds))
	if len(rows) != 4 {
		t.Fatalf("DETECT_LANGUAGE rows: %v", rows)
	}
	for i, want := range []string{"fr", "de", "ja"} {
		r := rows[i]
		if r["code"] != want || r["status"] != "" {
			t.Errorf("DETECT_LANGUAGE row %d: %v", i+1, r)
		}
		if p, _ := r["confidence"].(float64); p < 0.5 || p > 1 {
			t.Errorf("DETECT_LANGUAGE confidence %d: %v", i+1, r)
		}
	}
	if rows[3]["code"] != nil || rows[3]["status"] != nil {
		t.Errorf("a NULL text: %v", rows[3])
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.TRANSLATE(MODEL `%s.translate`, (SELECT comment AS text_content FROM `%s.comments`), "+
		"STRUCT('detect_language' AS translate_mode))", ds, ds))
	if len(rows) != 1 || rows[0]["text_content"] != "Hola, ¿cómo estás?" || rows[0]["ml_translate_status"] != "" ||
		!strings.Contains(fmt.Sprint(rows[0]["ml_translate_result"]), `"language_code":"es"`) {
		t.Errorf("a query input: %v", rows)
	}

	q := fmt.Sprintf("SELECT * FROM ML.TRANSLATE(MODEL `%s.translate`, TABLE `%s.texts`, STRUCT('translate_text' AS translate_mode, 'en' AS target_language_code)) ORDER BY id", ds, ds)
	if err := run(q); err != nil {
		if !strings.Contains(err.Error(), "Not implemented here") || !strings.Contains(err.Error(), "no local generation model") {
			t.Errorf("TRANSLATE_TEXT without a local model: %v", err)
		}
		t.Logf("TRANSLATE_TEXT: this instance has no local generation model: %v", err)
	} else {
		translate := func(target string) []map[string]bigquery.Value {
			rows := mlQuery(t, h, c, fmt.Sprintf("SELECT id, TO_JSON_STRING(ml_translate_result) AS result, ml_translate_status AS status, "+
				"STRING(ml_translate_result.translations[0].detected_language_code) AS detected, STRING(ml_translate_result.translations[0].translated_text) AS text "+
				"FROM ML.TRANSLATE(MODEL `%s.translate`, TABLE `%s.texts`, STRUCT('translate_text' AS translate_mode, '%s' AS target_language_code)) ORDER BY id", ds, ds, target))
			if len(rows) != 4 {
				t.Fatalf("TRANSLATE_TEXT %s rows: %v", target, rows)
			}
			if r := rows[3]; (r["result"] != nil && r["result"] != "null") || r["status"] != nil || r["text"] != nil {
				t.Errorf("TRANSLATE_TEXT %s, a NULL text: %v", target, rows[3])
			}
			for i, want := range []string{"fr", "de", "ja"} {
				r := rows[i]
				res := fmt.Sprint(r["result"])
				if r["status"] != "" || r["detected"] != want || strings.TrimSpace(fmt.Sprint(r["text"])) == "" ||
					!strings.Contains(res, `"glossary_translations":[]`) || !strings.Contains(res, `"translation_memory_translations":[]`) {
					t.Errorf("TRANSLATE_TEXT %s row %d: %v", target, i+1, r)
				}
				// The translation alone, as Translation answers it: one
				// line, no preamble, quotes or commentary around it.
				if s := fmt.Sprint(r["text"]); strings.ContainsAny(s, "\n\"“”*") || strings.Contains(strings.ToLower(s), "translat") {
					t.Errorf("TRANSLATE_TEXT %s row %d has more than the translation: %q", target, i+1, s)
				}
			}
			return rows
		}
		en := translate("en")
		// The words are Gemma's, not NMT's: what is asserted is the meaning
		// each sentence must carry, in English.
		for i, words := range [][]string{{"hello", "how are you"}, {"good morning", "how are you", "today"}, {"hello", "weather", "today"}} {
			s := strings.ToLower(fmt.Sprint(en[i]["text"]))
			for _, w := range words {
				if !strings.Contains(s, w) {
					t.Errorf("TRANSLATE_TEXT en row %d %q lacks %q", i+1, s, w)
				}
			}
			t.Logf("translation to en (row %d): %v", i+1, en[i]["text"])
		}
		// Temperature 0 and top-k 1: a second run answers the same words,
		// as Translation does.
		for i, r := range translate("en")[:3] {
			if r["text"] != en[i]["text"] {
				t.Errorf("TRANSLATE_TEXT is not deterministic, row %d: %q then %q", i+1, en[i]["text"], r["text"])
			}
		}
		// Another target: the detector reads each Spanish translation as
		// Spanish.
		es := translate("es")
		var lits []string
		for i, r := range es[:3] {
			t.Logf("translation to es (row %d): %v", i+1, r["text"])
			lits = append(lits, fmt.Sprintf("SELECT %d AS id, '%s' AS text_content", i+1,
				strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(fmt.Sprint(r["text"]))))
		}
		rows = mlQuery(t, h, c, fmt.Sprintf("SELECT id, JSON_VALUE(ml_translate_result, '$.languages[0].language_code') AS code FROM ML.TRANSLATE(MODEL `%s.translate`, "+
			"(%s), STRUCT('DETECT_LANGUAGE' AS translate_mode)) ORDER BY id", ds, strings.Join(lits, " UNION ALL ")))
		if len(rows) != 3 {
			t.Fatalf("DETECT_LANGUAGE of the Spanish translations: %v", rows)
		}
		for i, r := range rows {
			if r["code"] != "es" {
				t.Errorf("translation to es, row %d, detected as %v: %q", i+1, r["code"], es[i]["text"])
			}
		}
	}
	rows = mlQuery(t, h, c, fmt.Sprintf("SELECT ml_translate_result AS result, ml_translate_status AS status FROM ML.TRANSLATE(MODEL `%s.translate`, "+
		"(SELECT 'hello' AS text_content), STRUCT('TRANSLATE_TEXT' AS translate_mode, 'xx-nope' AS target_language_code))", ds))
	if len(rows) != 1 || rows[0]["result"] != nil || !strings.Contains(fmt.Sprint(rows[0]["status"]), "INVALID_ARGUMENT") {
		t.Errorf("an invalid target language: %v", rows)
	}

	for name, sql := range map[string]string{
		"no target":       "TABLE `%[1]s.texts`, STRUCT('TRANSLATE_TEXT' AS translate_mode)",
		"bad mode":        "TABLE `%[1]s.texts`, STRUCT('ROMANIZE' AS translate_mode)",
		"no mode":         "TABLE `%[1]s.texts`, STRUCT('en' AS target_language_code)",
		"unknown option":  "TABLE `%[1]s.texts`, STRUCT('DETECT_LANGUAGE' AS translate_mode, 1 AS glossary)",
		"no text_content": "TABLE `%[1]s.comments`, STRUCT('DETECT_LANGUAGE' AS translate_mode)",
	} {
		err := run(fmt.Sprintf("SELECT * FROM ML.TRANSLATE(MODEL `%[1]s.translate`, "+sql+")", ds))
		if err == nil || !strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.ANNOTATE_IMAGE(MODEL `%s.translate`, TABLE `%s.texts`, STRUCT(['TEXT_DETECTION'] AS vision_features))", ds, ds)); err == nil ||
		!strings.Contains(err.Error(), "ML.TRANSLATE") {
		t.Errorf("ML.ANNOTATE_IMAGE of the translation model: %v", err)
	}
}

// TestBigQueryMLUnderstandText (#1799): ML.UNDERSTAND_TEXT over a
// CLOUD_AI_NATURAL_LANGUAGE_V1 remote model, through the official
// cloud.google.com/go/bigquery and bigquery/connection/apiv1 clients and
// the generated bigquery/v2. Each distinct text_content is sent to the
// local Vertex AI endpoint's Natural Language v1 route (never Google),
// which asks the instance's local Gemma model under a JSON schema, greedily.
// With a local model: ANALYZE_SENTIMENT answers document_sentiment,
// language and sentences (begin_offset -1, as the tutorial shows it) with
// the review's polarity; ANALYZE_ENTITIES names entities whose mentions are
// in the text, with salience summing to 1; ANALYZE_ENTITY_SENTIMENT adds
// each entity's sentiment; CLASSIFY_TEXT answers the BigQuery page's
// example sentence with a Version 1 content category under
// /Arts & Entertainment, and a text under twenty tokens is the row's
// INVALID_ARGUMENT; flatten_json_output TRUE answers the page's columns;
// encoding_type UTF8 answers byte offsets; a second run answers the same
// JSON; a NULL text has no result. Without one (an instance without
// --local-ai-model), 501. ANALYZE_SYNTAX is 501 either way, and an
// undocumented option (MODERATE_TEXT), encoding_type with CLASSIFY_TEXT, an
// input without text_content and another function of the model are
// refused before anything is sent.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLUnderstandText(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	ctx := h.Context()
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_nl"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	grantRemoteModel(t, h, cc, project, cn)
	conn := project + ".us." + id

	ds := "compat_nl_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })
	run := func(sql string) error { return mlRun(h, c, sql) }
	const tv = "That actor on TV makes movies in Hollywood and also stars in a variety of popular new TV shows."
	if err := run(fmt.Sprintf("CREATE TABLE `%s.reviews` AS SELECT 1 AS id, 'This movie is amazing. The acting was wonderful and I loved every minute.' AS text_content UNION ALL "+
		"SELECT 2, 'The film was a boring, terrible waste of time. I hated it.' UNION ALL SELECT 3, '%s' UNION ALL SELECT 4, CAST(NULL AS STRING)", ds, tv)); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE TABLE `%s.comments` AS SELECT 'Sundar Pichai is the CEO of Google, which is based in Mountain View, California.' AS comment", ds)); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := run(fmt.Sprintf("CREATE OR REPLACE MODEL `%s.nl` REMOTE WITH CONNECTION `%s` OPTIONS(remote_service_type = 'cloud_ai_natural_language_v1')", ds, conn)); err != nil {
		t.Fatalf("CREATE MODEL CLOUD_AI_NATURAL_LANGUAGE_V1: %v", err)
	}
	md, err := generatedBigQuery(t, h).Models.Get(c.Project(), ds, "nl").Context(ctx).Do()
	if err != nil {
		t.Fatalf("models.get: %v", err)
	}
	if md.RemoteModelInfo == nil || md.RemoteModelInfo.RemoteServiceType != "CLOUD_AI_NATURAL_LANGUAGE_V1" || md.RemoteModelInfo.Endpoint != "" {
		t.Errorf("remoteModelInfo: %+v", md.RemoteModelInfo)
	}
	understand := func(option, input, extra string) []map[string]bigquery.Value {
		return mlQuery(t, h, c, fmt.Sprintf("SELECT * EXCEPT (ml_understand_text_result), TO_JSON_STRING(ml_understand_text_result) AS result "+
			"FROM ML.UNDERSTAND_TEXT(MODEL `%s.nl`, %s, STRUCT('%s' AS nlu_option%s))", ds, input, option, extra))
	}

	q := fmt.Sprintf("SELECT * FROM ML.UNDERSTAND_TEXT(MODEL `%s.nl`, TABLE `%s.reviews`, STRUCT('ANALYZE_SENTIMENT' AS nlu_option))", ds, ds)
	if err := run(q); err != nil {
		if !strings.Contains(err.Error(), "Not implemented here") || !strings.Contains(err.Error(), "no local generation model") {
			t.Errorf("ANALYZE_SENTIMENT without a local model: %v", err)
		}
		t.Logf("ANALYZE_SENTIMENT: this instance has no local generation model: %v", err)
	} else {
		byID := func(rows []map[string]bigquery.Value) map[int64]map[string]any {
			out := map[int64]map[string]any{}
			for _, r := range rows {
				var m map[string]any
				if s, ok := r["result"].(string); ok {
					_ = json.Unmarshal([]byte(s), &m)
				}
				if r["ml_understand_text_status"] != nil && r["ml_understand_text_status"] != "" {
					m = map[string]any{"status": r["ml_understand_text_status"]}
				}
				out[r["id"].(int64)] = m
			}
			return out
		}
		sentiment := understand("analyze_sentiment", "TABLE `"+ds+".reviews`", "")
		s := byID(sentiment)
		if len(sentiment) != 4 || s[4] != nil {
			t.Fatalf("ANALYZE_SENTIMENT rows: %v", sentiment)
		}
		for id, sign := range map[int64]float64{1: 1, 2: -1} {
			r := s[id]
			b, _ := json.Marshal(r)
			t.Logf("ANALYZE_SENTIMENT row %d: %s", id, b)
			doc, _ := r["document_sentiment"].(map[string]any)
			score, _ := doc["score"].(float64)
			sents, _ := r["sentences"].([]any)
			if r["language"] != "en" || score*sign < 0.3 || len(sents) != 2 {
				t.Errorf("ANALYZE_SENTIMENT row %d: %s", id, b)
				continue
			}
			if txt := sents[0].(map[string]any)["text"].(map[string]any); txt["begin_offset"] != -1.0 || txt["content"] == "" {
				t.Errorf("ANALYZE_SENTIMENT row %d sentence: %v", id, txt)
			}
		}
		// Greedy decoding: a second run answers the same JSON.
		again := byID(understand("ANALYZE_SENTIMENT", "TABLE `"+ds+".reviews`", ""))
		for id := range s {
			a, _ := json.Marshal(s[id])
			b, _ := json.Marshal(again[id])
			if string(a) != string(b) {
				t.Errorf("ANALYZE_SENTIMENT is not deterministic, row %d: %s then %s", id, a, b)
			}
		}

		input := "(SELECT 1 AS id, comment AS text_content FROM `" + ds + ".comments`)"
		const text = "Sundar Pichai is the CEO of Google, which is based in Mountain View, California."
		for _, option := range []string{"ANALYZE_ENTITIES", "ANALYZE_ENTITY_SENTIMENT"} {
			rows := understand(option, input, ", 'UTF8' AS encoding_type")
			if len(rows) != 1 || rows[0]["ml_understand_text_status"] != "" {
				t.Fatalf("%s: %v", option, rows)
			}
			t.Logf("%s: %v", option, rows[0]["result"])
			var res struct {
				Entities []struct {
					Name      string          `json:"name"`
					Type      string          `json:"type"`
					Salience  float64         `json:"salience"`
					Sentiment json.RawMessage `json:"sentiment"`
					Mentions  []struct {
						Text struct {
							Content     string `json:"content"`
							BeginOffset int    `json:"begin_offset"`
						} `json:"text"`
						Type string `json:"type"`
					} `json:"mentions"`
				} `json:"entities"`
				Language string `json:"language"`
			}
			if err := json.Unmarshal([]byte(rows[0]["result"].(string)), &res); err != nil || res.Language != "en" || len(res.Entities) < 2 {
				t.Fatalf("%s: %v %v", option, err, rows[0]["result"])
			}
			var sum float64
			names := map[string]string{}
			for _, e := range res.Entities {
				sum += e.Salience
				names[e.Name] = e.Type
				if option == "ANALYZE_ENTITY_SENTIMENT" && len(e.Sentiment) == 0 {
					t.Errorf("%s: %s has no sentiment", option, e.Name)
				}
				for _, m := range e.Mentions {
					if o := m.Text.BeginOffset; o < 0 || o+len(m.Text.Content) > len(text) || text[o:o+len(m.Text.Content)] != m.Text.Content {
						t.Errorf("%s: mention %q at %d is not in the text", option, m.Text.Content, o)
					}
				}
			}
			if sum < 0.99 || sum > 1.01 || names["Google"] != "ORGANIZATION" || names["Sundar Pichai"] != "PERSON" {
				t.Errorf("%s: %v (salience %g)", option, names, sum)
			}
		}

		cls := understand("CLASSIFY_TEXT", "TABLE `"+ds+".reviews`", "")
		k := byID(cls)
		b, _ := json.Marshal(k[3])
		t.Logf("CLASSIFY_TEXT of the page's example: %s", b)
		cats, _ := k[3]["categories"].([]any)
		if len(cats) == 0 || !strings.HasPrefix(fmt.Sprint(cats[0].(map[string]any)["name"]), "/Arts & Entertainment") {
			t.Errorf("CLASSIFY_TEXT row 3: %s", b)
		}
		if st := fmt.Sprint(k[1]["status"]); !strings.Contains(st, "too few tokens") {
			t.Errorf("CLASSIFY_TEXT of 14 tokens: %v", k[1])
		}
		flat := mlQuery(t, h, c, fmt.Sprintf("SELECT id, TO_JSON_STRING(categories) AS categories, ml_understand_text_status AS status FROM ML.UNDERSTAND_TEXT(MODEL `%s.nl`, "+
			"TABLE `%s.reviews`, STRUCT('classify_text' AS nlu_option, TRUE AS flatten_json_output)) WHERE id = 3", ds, ds))
		if len(flat) != 1 || flat[0]["status"] != "" || !strings.Contains(fmt.Sprint(flat[0]["categories"]), "/Arts & Entertainment") {
			t.Errorf("CLASSIFY_TEXT flattened: %v", flat)
		}
		flat = mlQuery(t, h, c, fmt.Sprintf("SELECT language, TO_JSON_STRING(sentiment) AS sentiment, TO_JSON_STRING(sentences) AS sentences FROM ML.UNDERSTAND_TEXT(MODEL `%s.nl`, "+
			"TABLE `%s.reviews`, STRUCT('ANALYZE_SENTIMENT' AS nlu_option, TRUE AS flatten_json_output)) WHERE id = 1", ds, ds))
		if len(flat) != 1 || flat[0]["language"] != "en" || !strings.Contains(fmt.Sprint(flat[0]["sentiment"]), `"score"`) ||
			!strings.Contains(fmt.Sprint(flat[0]["sentences"]), `"begin_offset"`) {
			t.Errorf("ANALYZE_SENTIMENT flattened: %v", flat)
		}
	}

	if err := run(fmt.Sprintf("SELECT * FROM ML.UNDERSTAND_TEXT(MODEL `%[1]s.nl`, (SELECT comment AS text_content FROM `%[1]s.comments`), STRUCT('ANALYZE_SYNTAX' AS nlu_option))", ds)); err == nil ||
		!strings.Contains(err.Error(), "Not implemented here") {
		t.Errorf("ANALYZE_SYNTAX: %v", err)
	}
	for name, sql := range map[string]string{
		"no option":       "TABLE `%[1]s.reviews`, STRUCT(TRUE AS flatten_json_output)",
		"moderate text":   "TABLE `%[1]s.reviews`, STRUCT('MODERATE_TEXT' AS nlu_option)",
		"classify utf8":   "TABLE `%[1]s.reviews`, STRUCT('CLASSIFY_TEXT' AS nlu_option, 'UTF8' AS encoding_type)",
		"bad encoding":    "TABLE `%[1]s.reviews`, STRUCT('ANALYZE_SENTIMENT' AS nlu_option, 'UTF7' AS encoding_type)",
		"unknown option":  "TABLE `%[1]s.reviews`, STRUCT('ANALYZE_SENTIMENT' AS nlu_option, 1 AS model)",
		"no text_content": "TABLE `%[1]s.comments`, STRUCT('ANALYZE_SENTIMENT' AS nlu_option)",
	} {
		err := run(fmt.Sprintf("SELECT * FROM ML.UNDERSTAND_TEXT(MODEL `%[1]s.nl`, "+sql+")", ds))
		if err == nil || !strings.Contains(err.Error(), "400") && !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := run(fmt.Sprintf("SELECT * FROM ML.TRANSLATE(MODEL `%s.nl`, TABLE `%s.reviews`, STRUCT('DETECT_LANGUAGE' AS translate_mode))", ds, ds)); err == nil ||
		!strings.Contains(err.Error(), "ML.UNDERSTAND_TEXT") {
		t.Errorf("ML.TRANSLATE of the Natural Language model: %v", err)
	}
}
