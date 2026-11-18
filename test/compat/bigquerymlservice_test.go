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
// crop hint (#1799). LOGO_DETECTION (no local model) is refused 501, as are the
// service types with no local model at CREATE MODEL (Translation,
// Natural Language, Speech-to-Text, Document AI), each naming its
// function; ML.GENERATE_TEXT of the Vision model and ML.ANNOTATE_IMAGE
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

	for typ, fn := range map[string]string{"CLOUD_AI_TRANSLATE_V3": "ML.TRANSLATE", "CLOUD_AI_NATURAL_LANGUAGE_V1": "ML.UNDERSTAND_TEXT",
		"CLOUD_AI_SPEECH_TO_TEXT_V2": "ML.TRANSCRIBE", "CLOUD_AI_DOCUMENT_V1": "ML.PROCESS_DOCUMENT"} {
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
