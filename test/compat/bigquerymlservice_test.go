//go:build compat

package compat

import (
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
// crop hint (#1799). LABEL_DETECTION (no local model) is refused 501, as are the
// service types with no local model at CREATE MODEL (Translation,
// Natural Language, Document AI), each naming its
// function; ML.GENERATE_TEXT of the Vision model and ML.ANNOTATE_IMAGE
// over a query are 400.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLAnnotateImage(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvLocalAI)
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
	for name, data := range map[string][]byte{"img/scan.png": page, "img/broken.png": []byte("not an image")} {
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

	for typ, fn := range map[string]string{"CLOUD_AI_TRANSLATE_V3": "ML.TRANSLATE", "CLOUD_AI_NATURAL_LANGUAGE_V1": "ML.UNDERSTAND_TEXT",
		"CLOUD_AI_DOCUMENT_V1": "ML.PROCESS_DOCUMENT"} {
		err := run(fmt.Sprintf("CREATE MODEL `%s.refused` REMOTE WITH CONNECTION `%s` OPTIONS(REMOTE_SERVICE_TYPE = '%s')", ds, conn, typ))
		if err == nil || !strings.Contains(err.Error(), "Not implemented here") || !strings.Contains(err.Error(), fn) {
			t.Errorf("%s: %v", typ, err)
		}
	}
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

	if err := run(fmt.Sprintf("SELECT * FROM ML.ANNOTATE_IMAGE(MODEL `%s.vision`, TABLE `%s.images`, STRUCT(['label_detection'] AS vision_features))", ds, ds)); err == nil ||
		!strings.Contains(err.Error(), "Not implemented here") || !strings.Contains(err.Error(), "LABEL_DETECTION") {
		t.Errorf("LABEL_DETECTION: %v", err)
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
// a FLAC file (no local decoder) and a text file are their rows' errors.
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
		"audio/song.flac": {"audio/flac", []byte("fLaC\x00\x00\x00\x22 not really")},
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
	if len(rows) != 3 {
		t.Fatalf("rows: %v", rows)
	}
	jfk, notes, song := rows[0], rows[1], rows[2]
	text, _ := jfk["transcripts"].(string)
	confidence, _ := jfk["confidence"].(float64)
	if !strings.HasSuffix(fmt.Sprint(jfk["uri"]), "/audio/jfk.wav") || jfk["status"] != "" || jfk["content_type"] != "audio/wav" ||
		!strings.Contains(strings.ToLower(text), "ask not what your country can do for you") || jfk["lang"] != "en-us" ||
		jfk["billed"] != int64(11) || jfk["end_s"] != int64(11) || confidence <= 0 || confidence > 1 || jfk["word"] != "And" || jfk["word_nanos"] == nil {
		t.Errorf("jfk.wav row: %v", jfk)
	}
	if status, _ := song["status"].(string); !strings.Contains(status, "FLAC") || song["transcripts"] != nil {
		t.Errorf("FLAC row: %v", song)
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
