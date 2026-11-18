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
// an empty ml_annotate_image_status, and an object that is not an image
// is "Bad image data." in its row's status; the object table's columns
// are kept. LABEL_DETECTION (no local model) is refused 501, as are the
// service types with no local model at CREATE MODEL (Translation,
// Natural Language, Speech-to-Text, Document AI), each naming its
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
		"JSON_VALUE(ml_annotate_image_result, '$.fullTextAnnotation.text') AS text "+
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
