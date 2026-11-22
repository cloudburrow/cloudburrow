//go:build compat

package compat

import (
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

	"github.com/cloudburrow/cloudburrow/internal/onnxmodel/onnxgen"
)

// TestBigQueryMLImportedONNXModelOverImages (#2104): ML.PREDICT of an
// imported ONNX model over an image object table, as
// https://docs.cloud.google.com/bigquery/docs/object-table-inference
// documents it ("The model must be a TensorFlow or TensorFlow Lite model,
// or a PyTorch model in ONNX format", its input "a shape of [batch_size,
// width, height, 3]"), through the official cloud.google.com/go bigquery,
// storage and bigquery/connection/apiv1 clients: the input column is
// ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 2, 2, FALSE) AS input, the model
// onnxgen's ImageMean, whose outputs are each channel's mean (rgb) and
// the mean of every value (brightness), so the image reaches ONNX Runtime
// as [1, 2, 2, 3], channels last, exactly as the resized STRUCT holds it;
// an image of the wrong size is refused, naming ML.RESIZE_IMAGE.
//
// It runs on an instance of the onnx build, as TestBigQueryMLImportedONNXModel.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryMLImportedONNXModelOverImages(t *testing.T) {
	if os.Getenv(EnvONNX) == "" {
		t.Skipf("%s is not set: the instance must be a -tags onnx build run with ONNXRUNTIME_LIB", EnvONNX)
	}
	h := New(t)
	c, project := bigqueryClient(t, h)
	cc, _ := remoteConnectionClient(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	png, err := os.ReadFile(filepath.Join("..", "..", "internal", "tflitemodel", "testdata", "image_small.png"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.ReplaceAll(h.Project(), "-", "_") + "_onnximg"
	cn, err := cc.CreateConnection(ctx, &connectionpb.CreateConnectionRequest{Parent: "projects/" + project + "/locations/us", ConnectionId: id,
		Connection: &connectionpb.Connection{Properties: &connectionpb.Connection_CloudResource{CloudResource: &connectionpb.CloudResourceProperties{}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	t.Cleanup(func() { _ = cc.DeleteConnection(h.Context(), &connectionpb.DeleteConnectionRequest{Name: cn.Name}) })
	if _, err := cc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: cn.Name, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/bigquery.connectionUser", Members: []string{callerMember(project)}}}}}); err != nil {
		t.Fatalf("SetIamPolicy on the connection: %v", err)
	}
	ds := "compat_onnximg_" + strings.ReplaceAll(h.Project(), "-", "_")
	if err := c.Dataset(ds).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(h.Context()) })
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
	put("onnximg/small.png", "image/png", png)
	put("onnxmodels/image_mean.onnx", "application/octet-stream", onnxgen.ImageMean())
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("bucket IAM Policy: %v", err)
	}
	pol.Add("serviceAccount:"+cn.GetCloudResource().GetServiceAccountId(), iam.RoleName("roles/storage.objectViewer"))
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("bucket IAM SetPolicy: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE EXTERNAL TABLE `%s.images` WITH CONNECTION `%s.us.%s` OPTIONS(object_metadata = 'SIMPLE', uris = ['gs://%s/onnximg/*'])",
		ds, project, id, bh.BucketName())); err != nil {
		t.Fatalf("CREATE EXTERNAL TABLE: %v", err)
	}
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.imean` OPTIONS(model_type='ONNX', model_path='gs://%s/onnxmodels/image_mean.onnx')", ds, bh.BucketName())); err != nil {
		t.Fatalf("CREATE MODEL: %v", err)
	}

	// The resized image, as BigQuery holds it: height, width, channels and
	// its values, row-major, channels last.
	it, err := c.Query(fmt.Sprintf("SELECT ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 2, 2, FALSE) AS input FROM `%s.images`", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("ML.RESIZE_IMAGE: %v", err)
	}
	var img []bigquery.Value
	if err := it.Next(&img); err != nil {
		t.Fatalf("ML.RESIZE_IMAGE: %v", err)
	}
	st, _ := img[0].([]bigquery.Value)
	if len(st) != 2 {
		t.Fatalf("the resized image is %v, want a STRUCT of two arrays", img[0])
	}
	dims, _ := st[0].([]bigquery.Value)
	vals, _ := st[1].([]bigquery.Value)
	if len(dims) != 3 || dims[0] != int64(2) || dims[1] != int64(2) || dims[2] != int64(3) || len(vals) != 12 {
		t.Fatalf("the resized image is %v × %d values, want 2 × 2 × 3", dims, len(vals))
	}
	var rgb [3]float64
	total := 0.0
	for i, v := range vals {
		x, _ := v.(float64)
		rgb[i%3] += x / 4
		total += x / 12
	}

	it, err = c.Query(fmt.Sprintf("SELECT uri, rgb, brightness FROM ML.PREDICT(MODEL `%s.imean`, "+
		"(SELECT uri, ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 2, 2, FALSE) AS input FROM `%[1]s.images`))", ds)).Read(ctx)
	if err != nil {
		t.Fatalf("ML.PREDICT over the images: %v", err)
	}
	n := 0
	for {
		var row map[string]bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ML.PREDICT over the images: %v", err)
		}
		n++
		got, _ := row["rgb"].([]bigquery.Value)
		if len(got) != 3 {
			t.Fatalf("rgb %v, want the three channels' means", row["rgb"])
		}
		for k := range rgb {
			if g, _ := got[k].(float64); math.Abs(g-rgb[k]) > 1e-5 {
				t.Errorf("channel %d's mean %v, want %v (the image as [1, 2, 2, 3], channels last)", k, g, rgb[k])
			}
		}
		if b, _ := row["brightness"].(float64); math.Abs(b-total) > 1e-5 {
			t.Errorf("brightness %v, want %v", b, total)
		}
		if u, _ := row["uri"].(string); !strings.HasSuffix(u, "/onnximg/small.png") {
			t.Errorf("uri %q passed through", u)
		}
	}
	if n != 1 {
		t.Errorf("ML.PREDICT answered %d rows, want 1", n)
	}

	// An image of another size than the model's input is refused.
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.imean`, "+
		"(SELECT ML.RESIZE_IMAGE(ML.DECODE_IMAGE(data), 3, 3, FALSE) AS input FROM `%[1]s.images`))", ds)).Read(ctx)
	if err == nil || !strings.Contains(err.Error(), "ML.RESIZE_IMAGE") {
		t.Errorf("a 3 × 3 image: %v, want a 400 naming ML.RESIZE_IMAGE", err)
	}
}
