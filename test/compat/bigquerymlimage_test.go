//go:build compat

package compat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/bigquery/connection/apiv1/connectionpb"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/api/iterator"
)

// TestBigQueryMLImageFunctions (#1798): BigQuery ML's image functions
// over an object table, through the official cloud.google.com/go
// bigquery, storage and bigquery/connection/apiv1 clients, checked
// against TensorFlow 2.20's own output for the same images
// (internal/tflitemodel/testdata/gen_image.py, tensorflow_image.json):
// ML.DECODE_IMAGE(data) of PNG (colour and grey) and JPEG objects, a
// STRUCT<ARRAY<INT64>, ARRAY<FLOAT64>> of height, width, channels and the
// pixels; of a BYTES value; ML.RESIZE_IMAGE (bilinear, and keeping the
// aspect ratio), also of an image a table stored;
// ML.CONVERT_COLOR_SPACE to HSV, GRAYSCALE, YIQ and YUV;
// ML.CONVERT_IMAGE_TYPE to INT64; the refusals of an object that is not
// an image and of one over 10,000,000 bytes, which a WHERE filtering
// them out avoids; data outside an ML function still unknown; ML.PREDICT
// of an imported TENSORFLOW_LITE image model over the decoded images,
// to TensorFlow's output, and refused for an image of the wrong size;
// ML.GENERATE_TEXT over an object table 501, as the local runtime reads
// text only.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLImageFunctions(t *testing.T) {
	h := New(t)
	// Cloud Storage under the emulators shard's own name for it, as
	// TestBigQueryMLImportedXGBoostModel (#1761), so the Cloud Storage suite
	// stays in the storage shard and this test runs in emulators.
	shardEndpoint(h, "CLOUDBURROW_TEST_BQML_STORAGE", EnvStorage)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	dir := filepath.Join("..", "..", "internal", "tflitemodel", "testdata")
	read := func(name string) []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	type tfImage struct {
		Shape []int64   `json:"shape"`
		Data  []float64 `json:"data"`
	}
	var fx struct {
		Decode      map[string]tfImage `json:"decode"`
		JPEGExact   tfImage            `json:"decode_jpeg_accurate"`
		Resize      map[string]tfImage `json:"resize"`
		Color       map[string]tfImage `json:"color"`
		ConvertType tfImage            `json:"convert_type"`
		Predict     map[string]struct {
			TensorFlow []float64 `json:"tensorflow"`
		} `json:"predict"`
	}
	if err := json.Unmarshal(read("tensorflow_image.json"), &fx); err != nil {
		t.Fatal(err)
	}

	id := strings.ReplaceAll(h.Project(), "-", "_") + "_img"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(ctx, &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: []string{callerMember(project)}}}}}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
	conn := project + ".us." + id
	ds := "compat_img_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(ctx) })

	bh := bucket(t, h, sc)
	put := func(name, ct string, data []byte) {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType = ct
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	put("img/small.png", "image/png", read("image_small.png"))
	put("img/big.png", "image/png", read("image_big.png"))
	put("img/gray.png", "image/png", read("image_gray.png"))
	put("img/photo.jpg", "image/jpeg", read("image.jpg"))
	put("img/notes.txt", "text/plain", []byte("not an image"))
	huge := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 10_000_000)...)
	put("img/huge.png", "image/png", huge)
	put("models/image.tflite", "application/octet-stream", read("image.tflite"))
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("bucket IAM Policy: %v", err)
	}
	pol.Add("serviceAccount:"+cn.GetCloudResource().GetServiceAccountId(), iam.RoleName("roles/storage.objectViewer"))
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("bucket IAM SetPolicy: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE EXTERNAL TABLE `%s.images` WITH CONNECTION `%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/img/*'])",
		ds, conn, bh.BucketName())); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE: %v", err)
	}

	// one runs sql and returns its one row's values.
	one := func(sql string) ([]bigquery.Value, bigquery.Schema) {
		t.Helper()
		it, err := c.Query(sql).Read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		var row []bigquery.Value
		if err := it.Next(&row); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return row, it.Schema
	}
	// image checks an image STRUCT against TensorFlow's.
	image := func(what string, v bigquery.Value, want tfImage, tol float64) {
		t.Helper()
		s, ok := v.([]bigquery.Value)
		if !ok || len(s) != 2 {
			t.Fatalf("%s: %T %v is not a STRUCT of two arrays", what, v, v)
		}
		dims, _ := s[0].([]bigquery.Value)
		data, _ := s[1].([]bigquery.Value)
		if len(dims) != 3 || dims[0] != want.Shape[0] || dims[1] != want.Shape[1] || dims[2] != want.Shape[2] {
			t.Fatalf("%s: dimensions %v, TensorFlow's %v", what, dims, want.Shape)
		}
		if len(data) != len(want.Data) {
			t.Fatalf("%s: %d values, TensorFlow's %d", what, len(data), len(want.Data))
		}
		worst := 0.0
		for i, d := range data {
			var f float64
			switch x := d.(type) {
			case float64:
				f = x
			case int64:
				f = float64(x)
			default:
				t.Fatalf("%s: value %T", what, d)
			}
			worst = math.Max(worst, math.Abs(f-want.Data[i]))
		}
		if worst > tol {
			t.Errorf("%s: differs from TensorFlow's by up to %g (tolerance %g)", what, worst, tol)
		}
	}
	at := func(name string) string {
		return fmt.Sprintf("FROM `%s.images` WHERE uri = 'gs://%s/img/%s'", ds, bh.BucketName(), name)
	}

	// ML.DECODE_IMAGE of the data pseudocolumn: the documented STRUCT.
	row, schema := one("SELECT ML.DECODE_IMAGE(data) AS img " + at("small.png"))
	if len(schema) != 1 || schema[0].Type != bigquery.RecordFieldType || len(schema[0].Schema) != 2 ||
		schema[0].Schema[0].Type != bigquery.IntegerFieldType || !schema[0].Schema[0].Repeated ||
		schema[0].Schema[1].Type != bigquery.FloatFieldType || !schema[0].Schema[1].Repeated ||
		schema[0].Schema[0].Name != "_field_1" || schema[0].Schema[1].Name != "_field_2" {
		t.Fatalf("schema of ML.DECODE_IMAGE: %+v", schema)
	}
	image("decode small.png", row[0], fx.Decode["image_small.png"], 1e-6)
	row, _ = one("SELECT ML.DECODE_IMAGE(t.data) " + strings.Replace(at("gray.png"), "images`", "images` AS t", 1))
	image("decode gray.png", row[0], fx.Decode["image_gray.png"], 1e-6)
	row, _ = one(fmt.Sprintf("SELECT ML.DECODE_IMAGE(`%s.images`.data) ", ds) + at("photo.jpg"))
	image("decode photo.jpg (libjpeg's accurate IDCT)", row[0], fx.JPEGExact, 2.0/255+1e-6)
	image("decode photo.jpg (TensorFlow's default fast IDCT)", row[0], fx.Decode["image.jpg"], 6.0/255)
	// Of a BYTES value.
	row, _ = one("SELECT ML.DECODE_IMAGE(FROM_BASE64('" + base64.StdEncoding.EncodeToString(read("image_small.png")) + "'))")
	image("decode BYTES", row[0], fx.Decode["image_small.png"], 1e-6)

	// ML.RESIZE_IMAGE, ML.CONVERT_COLOR_SPACE, ML.CONVERT_IMAGE_TYPE.
	row, _ = one("SELECT ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 6, 5, FALSE), ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 4, 4, TRUE) " + at("big.png"))
	image("resize big 6x5", row[0], fx.Resize["big_6x5"], 1e-5)
	image("resize big within 4x4", row[1], fx.Resize["big_4x4_preserve"], 1e-5)
	row, _ = one("SELECT ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 9, 7, FALSE) " + at("small.png"))
	image("resize small 9x7", row[0], fx.Resize["small_9x7"], 1e-5)
	for _, space := range []string{"HSV", "GRAYSCALE", "YIQ", "YUV"} {
		row, _ = one("SELECT ML.CONVERT_COLOR_SPACE(ML.DECODE_IMAGE(data), '" + space + "') " + at("small.png"))
		image("color "+space, row[0], fx.Color[space], 1e-5)
	}
	row, schema = one("SELECT ML.CONVERT_IMAGE_TYPE(ML.DECODE_IMAGE(data)) " + at("small.png"))
	if schema[0].Schema[1].Type != bigquery.IntegerFieldType {
		t.Errorf("ML.CONVERT_IMAGE_TYPE's values are %s, want INTEGER", schema[0].Schema[1].Type)
	}
	image("convert type", row[0], fx.ConvertType, 0)

	// An image a table stored, resized later.
	if err := mlRun(h, c, fmt.Sprintf("CREATE TABLE `%s.decoded` AS SELECT uri, ML.DECODE_IMAGE(data) AS img %s", ds, at("big.png"))); err != nil {
		t.Fatalf("CREATE TABLE AS ML.DECODE_IMAGE: %v", err)
	}
	row, _ = one(fmt.Sprintf("SELECT ML.RESIZE_IMAGE(img, 6, 5, FALSE) FROM `%s.decoded`", ds))
	image("resize a stored image", row[0], fx.Resize["big_6x5"], 1e-5)

	// The refusals, which a WHERE that filters the objects out avoids.
	refused := func(sql, want string) {
		t.Helper()
		_, err := c.Query(sql).Read(ctx)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error naming %q", sql, err, want)
		}
	}
	refused("SELECT ML.DECODE_IMAGE(data) "+at("notes.txt"), "JPEG, PNG and BMP")
	refused("SELECT ML.DECODE_IMAGE(data) "+at("huge.png"), "10,000,000 bytes")
	refused(fmt.Sprintf("SELECT ML.DECODE_IMAGE(data) FROM `%s.images`", ds), "ML.DECODE_IMAGE")
	refused(fmt.Sprintf("SELECT uri, data FROM `%s.images`", ds), "data")
	refused("SELECT ML.CONVERT_COLOR_SPACE(ML.DECODE_IMAGE(data), 'LAB') "+at("small.png"), "HSV, GRAYSCALE, YIQ and YUV")
	refused("SELECT ML.CONVERT_COLOR_SPACE(ML.DECODE_IMAGE(data), 'HSV') "+at("gray.png"), "RGB")
	it, err := c.Query(fmt.Sprintf("SELECT uri, ML.DECODE_IMAGE(data) AS img FROM `%s.images` WHERE content_type = 'image/png' AND size < 1000000 ORDER BY uri", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("decode the PNGs: %v", err)
	}
	var uris []string
	for {
		var r []bigquery.Value
		if err := it.Next(&r); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		uris = append(uris, r[0].(string)[strings.LastIndex(r[0].(string), "/")+1:])
	}
	if strings.Join(uris, ",") != "big.png,gray.png,small.png" {
		t.Errorf("decoded PNGs: %v", uris)
	}

	// ML.PREDICT of an imported TensorFlow Lite image model.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.imgmodel` OPTIONS(model_type='TENSORFLOW_LITE', model_path='gs://%s/models/image.tflite')", ds, bh.BucketName())); err != nil {
		t.Fatalf("CREATE MODEL TENSORFLOW_LITE: %v", err)
	}
	it, err = c.Query(fmt.Sprintf("SELECT uri, probs FROM ML.PREDICT(MODEL `%s.imgmodel`, (SELECT uri, "+
		"IF(uri LIKE '%%big.png', ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 6, 5, FALSE), ML.DECODE_IMAGE(data)) AS image "+
		"FROM `%s.images` WHERE uri LIKE '%%small.png' OR uri LIKE '%%big.png' OR uri LIKE '%%photo.jpg')) ORDER BY uri", ds, ds)).Read(ctx)
	if err != nil {
		t.Fatalf("ML.PREDICT over the object table: %v", err)
	}
	wants := map[string]struct {
		key string
		tol float64
	}{"big.png": {"big_6x5", 1e-5}, "photo.jpg": {"jpg", 0.01}, "small.png": {"small", 1e-5}}
	n := 0
	for {
		var r []bigquery.Value
		if err := it.Next(&r); err == iterator.Done {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		u := r[0].(string)
		w := wants[u[strings.LastIndex(u, "/")+1:]]
		got, _ := r[1].([]bigquery.Value)
		want := fx.Predict[w.key].TensorFlow
		if len(got) != len(want) {
			t.Fatalf("%s: probs %v, TensorFlow's %v", u, got, want)
		}
		for i := range want {
			if !near(got[i], want[i], w.tol) {
				t.Errorf("%s: probs %v, TensorFlow's %v", u, got, want)
				break
			}
		}
		n++
	}
	if n != 3 {
		t.Errorf("ML.PREDICT rows: %d, want 3", n)
	}
	refused(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.imgmodel`, (SELECT ML.DECODE_IMAGE(data) AS image %s))", ds, at("big.png")), "ML.RESIZE_IMAGE")

	// The same model as a TensorFlow SavedModel, converted by the
	// instance's TensorFlow converter.
	src := filepath.Join(dir, "savedmodel", "image")
	if err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		put("models/tf/"+filepath.ToSlash(rel), "application/octet-stream", read(filepath.Join("savedmodel", "image", rel)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The instance's first TENSORFLOW import may build the converter
	// image: tensorflowImportDeadline, not the harness's 60 s.
	if err := mlRunWithin(h, c, fmt.Sprintf("CREATE MODEL `%s.tfmodel` OPTIONS(model_type='TENSORFLOW', model_path='gs://%s/models/tf/*')", ds, bh.BucketName()), tensorflowImportDeadline); err != nil {
		t.Fatalf("CREATE MODEL TENSORFLOW: %v", err)
	}
	row, _ = one(fmt.Sprintf("SELECT probs FROM ML.PREDICT(MODEL `%s.tfmodel`, (SELECT ML.DECODE_IMAGE(data) AS image %s))", ds, at("small.png")))
	got, _ := row[0].([]bigquery.Value)
	want := fx.Predict["small"].TensorFlow
	for i := range want {
		if len(got) != len(want) || !near(got[i], want[i], 1e-5) {
			t.Fatalf("TENSORFLOW model's probs %v, TensorFlow's %v", got, want)
		}
	}

	// A multimodal prompt over the object table: the local runtime reads
	// text only.
	err = mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.gen` REMOTE WITH CONNECTION `%s` OPTIONS(ENDPOINT = 'gemini-2.5-flash')", ds, conn))
	if err != nil && strings.Contains(err.Error(), "local Vertex AI endpoint") {
		t.Log("this instance serves no local Vertex AI endpoint (up --local-ai-model): the multimodal refusal is not reached")
		return
	}
	if err != nil {
		t.Fatalf("CREATE MODEL REMOTE: %v", err)
	}
	refused(fmt.Sprintf("SELECT * FROM ML.GENERATE_TEXT(MODEL `%s.gen`, TABLE `%s.images`, STRUCT('Describe the image' AS prompt))", ds, ds), "multimodal")
}
