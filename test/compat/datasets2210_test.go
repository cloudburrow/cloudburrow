//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

const dsSchema = "gs://google-cloud-aiplatform/schema/dataset/"

func dsPutObject(t *testing.T, ctx context.Context, gcs *storage.Client, bucket, name, body string) {
	t.Helper()
	w := gcs.Bucket(bucket).Object(name).NewWriter(ctx)
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write gs://%s/%s: %v", bucket, name, err)
	}
}

func dsReadObject(t *testing.T, ctx context.Context, gcs *storage.Client, bucket, name string) string {
	t.Helper()
	r, err := gcs.Bucket(bucket).Object(name).NewReader(ctx)
	if err != nil {
		t.Fatalf("read gs://%s/%s: %v", bucket, name, err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// No covers: annotation yet (#2210): this test has not run against an
// instance, so DatasetService stays Implemented until it passes; it is to
// cover every DatasetService method.
//
// TestVertexAIDatasets (#2210), with the official Go client: an image
// dataset imports a JSON Lines classification file from Cloud Storage
// (its items, annotations, annotation specs and annotation set), is
// searched, updated and versioned (a version restored after a second
// import), and exported to Cloud Storage in the import format; a text
// dataset takes sentiment; a tabular dataset reads its BigQuery or Cloud
// Storage source and refuses ImportData; a custom training pipeline on the
// image dataset gets AIP_TRAINING_DATA_URI and its siblings, whose files
// hold the predefined split; AutoML is UNIMPLEMENTED.
// covers: google.cloud.aiplatform.v1.DatasetService/CreateDataset, google.cloud.aiplatform.v1.DatasetService/CreateDatasetVersion, google.cloud.aiplatform.v1.DatasetService/DeleteDataset, google.cloud.aiplatform.v1.DatasetService/DeleteDatasetVersion, google.cloud.aiplatform.v1.DatasetService/DeleteSavedQuery, google.cloud.aiplatform.v1.DatasetService/ExportData, google.cloud.aiplatform.v1.DatasetService/GetAnnotationSpec, google.cloud.aiplatform.v1.DatasetService/GetDataset, google.cloud.aiplatform.v1.DatasetService/GetDatasetVersion, google.cloud.aiplatform.v1.DatasetService/ImportData, google.cloud.aiplatform.v1.DatasetService/ListAnnotations, google.cloud.aiplatform.v1.DatasetService/ListDataItems, google.cloud.aiplatform.v1.DatasetService/ListDatasets, google.cloud.aiplatform.v1.DatasetService/ListDatasetVersions, google.cloud.aiplatform.v1.DatasetService/ListSavedQueries, google.cloud.aiplatform.v1.DatasetService/RestoreDatasetVersion, google.cloud.aiplatform.v1.DatasetService/SearchDataItems, google.cloud.aiplatform.v1.DatasetService/UpdateDataset, google.cloud.aiplatform.v1.DatasetService/UpdateDatasetVersion
func TestVertexAIDatasets(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	scaleShardEndpoint(h, envScaleRun, EnvRun)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	gcs := dataformStorage(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	opts := vertexOpts(endpoint)
	dc, err := aiplatform.NewDatasetClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "vx-ds-" + suffix
	dataformBucket(ctx, t, gcs, h.Project(), bucket)
	for _, n := range []string{"a", "b", "c", "d"} {
		dsPutObject(t, ctx, gcs, bucket, "img/"+n+".jpg", "image "+n)
	}
	dsPutObject(t, ctx, gcs, bucket, "flowers.jsonl", ""+
		`{"imageGcsUri": "gs://`+bucket+`/img/a.jpg", "classificationAnnotation": {"displayName": "daisy"}, "dataItemResourceLabels": {"aiplatform.googleapis.com/ml_use": "training"}}`+"\n"+
		`{"imageGcsUri": "gs://`+bucket+`/img/b.jpg", "classificationAnnotation": {"displayName": "rose"}, "dataItemResourceLabels": {"aiplatform.googleapis.com/ml_use": "validation"}}`+"\n"+
		`{"imageGcsUri": "gs://`+bucket+`/img/c.jpg", "classificationAnnotation": {"displayName": "daisy"}, "dataItemResourceLabels": {"aiplatform.googleapis.com/ml_use": "test"}}`+"\n")
	dsPutObject(t, ctx, gcs, bucket, "more.csv", "gs://"+bucket+"/img/d.jpg,tulip\n")

	// Create.
	op, err := dc.CreateDataset(ctx, &aiplatformpb.CreateDatasetRequest{Parent: parent, Dataset: &aiplatformpb.Dataset{
		DisplayName: "flowers-" + suffix, MetadataSchemaUri: dsSchema + "metadata/image_1.0.0.yaml", Labels: map[string]string{"compat": "t2210ds"}}})
	if err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}
	ds, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDataset's operation: %v", err)
	}
	if !strings.HasPrefix(ds.GetName(), parent+"/datasets/") || ds.GetEtag() == "" {
		t.Errorf("created = %s etag %q", ds.GetName(), ds.GetEtag())
	}
	defer func() {
		if op, err := dc.DeleteDataset(context.Background(), &aiplatformpb.DeleteDatasetRequest{Name: ds.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	}()
	if _, err := dc.CreateDataset(ctx, &aiplatformpb.CreateDatasetRequest{Parent: parent, Dataset: &aiplatformpb.Dataset{
		DisplayName: "bad", MetadataSchemaUri: dsSchema + "metadata/nothing_1.0.0.yaml"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an unknown metadata schema = %v, want InvalidArgument", err)
	}

	// Import JSON Lines, then CSV.
	imp := func(uri, format string) {
		t.Helper()
		op, err := dc.ImportData(ctx, &aiplatformpb.ImportDataRequest{Name: ds.GetName(), ImportConfigs: []*aiplatformpb.ImportDataConfig{{
			Source:          &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{uri}}},
			ImportSchemaUri: dsSchema + "ioformat/" + format}}})
		if err != nil {
			t.Fatalf("ImportData %s: %v", uri, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("ImportData %s's operation: %v", uri, err)
		}
	}
	imp("gs://"+bucket+"/flowers.jsonl", "image_classification_single_label_io_format_1.0.0.yaml")
	got, err := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: ds.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetDataItemCount() != 3 || len(got.GetSavedQueries()) != 1 || got.GetSavedQueries()[0].GetAnnotationSpecCount() != 2 {
		t.Errorf("after the import: %d items, saved queries %v", got.GetDataItemCount(), got.GetSavedQueries())
	}
	var items []*aiplatformpb.DataItem
	for it := dc.ListDataItems(ctx, &aiplatformpb.ListDataItemsRequest{Parent: ds.GetName()}); ; {
		di, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListDataItems: %v", err)
		}
		items = append(items, di)
	}
	if len(items) != 3 {
		t.Fatalf("ListDataItems = %d items, want 3", len(items))
	}
	var spec string
	for it := dc.ListAnnotations(ctx, &aiplatformpb.ListAnnotationsRequest{Parent: ds.GetName() + "/dataItems/-"}); ; {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListAnnotations: %v", err)
		}
		if a.GetPayload().GetStructValue().GetFields()["displayName"].GetStringValue() == "daisy" {
			spec = a.GetPayload().GetStructValue().GetFields()["annotationSpecId"].GetStringValue()
		}
	}
	if spec == "" {
		t.Fatal("no daisy annotation with an annotationSpecId")
	}
	if s, err := dc.GetAnnotationSpec(ctx, &aiplatformpb.GetAnnotationSpecRequest{Name: ds.GetName() + "/annotationSpecs/" + spec}); err != nil || s.GetDisplayName() != "daisy" {
		t.Errorf("GetAnnotationSpec = %v %v", s, err)
	}
	sq := dc.ListSavedQueries(ctx, &aiplatformpb.ListSavedQueriesRequest{Parent: ds.GetName()})
	query, err := sq.Next()
	if err != nil || query.GetProblemType() != "IMAGE_CLASSIFICATION_SINGLE_LABEL" {
		t.Fatalf("ListSavedQueries = %v %v", query, err)
	}
	n := 0
	for it := dc.SearchDataItems(ctx, &aiplatformpb.SearchDataItemsRequest{Dataset: ds.GetName(),
		Order:          &aiplatformpb.SearchDataItemsRequest_OrderByDataItem{OrderByDataItem: "create_time"},
		DataItemFilter: `labels.aiplatform.googleapis.com/ml_use="training"`}); ; n++ {
		if _, err := it.Next(); err != nil {
			if err != iterator.Done {
				t.Fatalf("SearchDataItems: %v", err)
			}
			break
		}
	}
	if n != 1 {
		t.Errorf("SearchDataItems for training = %d views, want 1", n)
	}

	// Update, list.
	got.DisplayName = "flowers-renamed-" + suffix
	up, err := dc.UpdateDataset(ctx, &aiplatformpb.UpdateDatasetRequest{Dataset: got, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil || up.GetDisplayName() != got.DisplayName {
		t.Errorf("UpdateDataset = %v %v", up.GetDisplayName(), err)
	}
	n = 0
	for it := dc.ListDatasets(ctx, &aiplatformpb.ListDatasetsRequest{Parent: parent, Filter: `labels.compat="t2210ds"`}); ; n++ {
		if _, err := it.Next(); err != nil {
			break
		}
	}
	if n != 1 {
		t.Errorf("ListDatasets by label = %d, want 1", n)
	}

	// A version, a second import, and a restore.
	vop, err := dc.CreateDatasetVersion(ctx, &aiplatformpb.CreateDatasetVersionRequest{Parent: ds.GetName(), DatasetVersion: &aiplatformpb.DatasetVersion{DisplayName: "v1"}})
	if err != nil {
		t.Fatalf("CreateDatasetVersion: %v", err)
	}
	ver, err := vop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatasetVersion's operation: %v", err)
	}
	if v, err := dc.GetDatasetVersion(ctx, &aiplatformpb.GetDatasetVersionRequest{Name: ver.GetName()}); err != nil || v.GetDisplayName() != "v1" {
		t.Errorf("GetDatasetVersion = %v %v", v, err)
	}
	ver.DisplayName = "first"
	if v, err := dc.UpdateDatasetVersion(ctx, &aiplatformpb.UpdateDatasetVersionRequest{DatasetVersion: ver,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}}); err != nil || v.GetDisplayName() != "first" {
		t.Errorf("UpdateDatasetVersion = %v %v", v, err)
	}
	imp("gs://"+bucket+"/more.csv", "image_classification_single_label_io_format_1.0.0.yaml")
	if g, _ := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: ds.GetName()}); g.GetDataItemCount() != 4 {
		t.Errorf("after the CSV import: %d items, want 4", g.GetDataItemCount())
	}
	rop, err := dc.RestoreDatasetVersion(ctx, &aiplatformpb.RestoreDatasetVersionRequest{Name: ver.GetName()})
	if err != nil {
		t.Fatalf("RestoreDatasetVersion: %v", err)
	}
	if _, err := rop.Wait(ctx); err != nil {
		t.Fatalf("RestoreDatasetVersion's operation: %v", err)
	}
	if g, _ := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: ds.GetName()}); g.GetDataItemCount() != 3 {
		t.Errorf("after the restore: %d items, want 3", g.GetDataItemCount())
	}
	n = 0
	for it := dc.ListDatasetVersions(ctx, &aiplatformpb.ListDatasetVersionsRequest{Parent: ds.GetName()}); ; n++ {
		if _, err := it.Next(); err != nil {
			break
		}
	}
	if n != 1 {
		t.Errorf("ListDatasetVersions = %d, want 1", n)
	}

	// Export in the import format.
	eop, err := dc.ExportData(ctx, &aiplatformpb.ExportDataRequest{Name: ds.GetName(), ExportConfig: &aiplatformpb.ExportDataConfig{
		Destination: &aiplatformpb.ExportDataConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/export"}}}})
	if err != nil {
		t.Fatalf("ExportData: %v", err)
	}
	ex, err := eop.Wait(ctx)
	if err != nil {
		t.Fatalf("ExportData's operation: %v", err)
	}
	if len(ex.GetExportedFiles()) != 1 || !strings.HasPrefix(ex.GetExportedFiles()[0], "gs://"+bucket+"/export/export-data-") {
		t.Fatalf("exported files = %v", ex.GetExportedFiles())
	}
	body := dsReadObject(t, ctx, gcs, bucket, strings.TrimPrefix(ex.GetExportedFiles()[0], "gs://"+bucket+"/"))
	if strings.Count(body, "imageGcsUri") != 3 || !strings.Contains(body, `"daisy"`) || !strings.Contains(body, "aiplatform.googleapis.com/ml_use") {
		t.Errorf("the export =\n%s", body)
	}

	// A custom training pipeline on the dataset.
	trainer := pushTrainer(t, ctx, h)
	pc, err := aiplatform.NewPipelineClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	inputs, _ := structpb.NewValue(map[string]any{
		"baseOutputDirectory": map[string]any{"outputUriPrefix": "gs://" + bucket + "/tp"},
		"workerPoolSpecs": []any{map[string]any{"machineSpec": map[string]any{"machineType": "n1-standard-4"}, "replicaCount": "1",
			"containerSpec": map[string]any{"imageUri": trainer, "command": []any{"sh", "-c",
				`d=/gcs/${AIP_MODEL_DIR#gs://}; mkdir -p "$d" && printf '%s\n%s\n%s\n%s\n' "$AIP_DATA_FORMAT" "$AIP_TRAINING_DATA_URI" "$AIP_VALIDATION_DATA_URI" "$AIP_TEST_DATA_URI" > "$d/env.txt"`}}}}})
	tp, err := pc.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName: "compat-ds-tp-" + suffix, TrainingTaskDefinition: "gs://google-cloud-aiplatform/schema/trainingjob/definition/custom_task_1.0.0.yaml",
		TrainingTaskInputs: inputs,
		InputDataConfig: &aiplatformpb.InputDataConfig{DatasetId: strings.TrimPrefix(ds.GetName(), parent+"/datasets/"),
			AnnotationSchemaUri: dsSchema + "annotation/image_classification_1.0.0.yaml",
			Split: &aiplatformpb.InputDataConfig_FilterSplit{FilterSplit: &aiplatformpb.FilterSplit{
				TrainingFilter: `labels.aiplatform.googleapis.com/ml_use="training"`, ValidationFilter: `labels.aiplatform.googleapis.com/ml_use="validation"`,
				TestFilter: `labels.aiplatform.googleapis.com/ml_use="test"`}},
			Destination: &aiplatformpb.InputDataConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://" + bucket + "/data"}}}}})
	if err != nil {
		t.Fatalf("CreateTrainingPipeline on the dataset: %v", err)
	}
	defer func() {
		_, _ = pc.DeleteTrainingPipeline(context.Background(), &aiplatformpb.DeleteTrainingPipelineRequest{Name: tp.GetName()})
	}()
	var tpGot *aiplatformpb.TrainingPipeline
	for {
		tpGot, err = pc.GetTrainingPipeline(ctx, &aiplatformpb.GetTrainingPipelineRequest{Name: tp.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if s := tpGot.GetState(); s == aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED || s == aiplatformpb.PipelineState_PIPELINE_STATE_FAILED ||
			s == aiplatformpb.PipelineState_PIPELINE_STATE_CANCELLED {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the training pipeline did not end: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	if tpGot.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("the training pipeline = %v %v", tpGot.GetState(), tpGot.GetError())
	}
	env := strings.Split(strings.TrimSpace(dsReadObject(t, ctx, gcs, bucket, "tp/model/env.txt")), "\n")
	if len(env) != 4 || env[0] != "jsonl" {
		t.Fatalf("the job's data env = %q", env)
	}
	for i, role := range []string{"training", "validation", "test"} {
		uri := env[i+1]
		if !strings.HasPrefix(uri, "gs://"+bucket+"/data/dataset-") || !strings.Contains(uri, "/"+role+"-") {
			t.Errorf("AIP_%s_DATA_URI = %q", strings.ToUpper(role), uri)
			continue
		}
		obj := strings.Replace(strings.TrimPrefix(uri, "gs://"+bucket+"/"), "*", "00000-of-00001", 1)
		if b := dsReadObject(t, ctx, gcs, bucket, obj); strings.Count(b, "imageGcsUri") != 1 {
			t.Errorf("the %s file = %q, want one item", role, b)
		}
	}

	// A text dataset takes sentiment.
	dsPutObject(t, ctx, gcs, bucket, "reviews.jsonl", `{"textContent": "lovely", "sentimentAnnotation": {"sentiment": 4, "sentimentMax": 4}}`+"\n")
	top, err := dc.CreateDataset(ctx, &aiplatformpb.CreateDatasetRequest{Parent: parent, Dataset: &aiplatformpb.Dataset{
		DisplayName: "reviews-" + suffix, MetadataSchemaUri: dsSchema + "metadata/text_1.0.0.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	text, err := top.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	iop, err := dc.ImportData(ctx, &aiplatformpb.ImportDataRequest{Name: text.GetName(), ImportConfigs: []*aiplatformpb.ImportDataConfig{{
		Source:          &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://" + bucket + "/reviews.jsonl"}}},
		ImportSchemaUri: dsSchema + "ioformat/text_sentiment_io_format_1.0.0.yaml"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iop.Wait(ctx); err != nil {
		t.Errorf("the sentiment import: %v", err)
	}
	if g, _ := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: text.GetName()}); g.GetDataItemCount() != 1 {
		t.Errorf("the text dataset has %d items, want 1", g.GetDataItemCount())
	}
	if op, err := dc.DeleteDataset(ctx, &aiplatformpb.DeleteDatasetRequest{Name: text.GetName()}); err != nil {
		t.Errorf("DeleteDataset: %v", err)
	} else if err := op.Wait(ctx); err != nil {
		t.Errorf("DeleteDataset's operation: %v", err)
	}
	if _, err := dc.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: text.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetDataset after delete = %v, want NotFound", err)
	}

	// A tabular dataset reads its source and takes no import.
	dsPutObject(t, ctx, gcs, bucket, "tab.csv", "x,y\n1,3\n2,5\n")
	meta, _ := structpb.NewValue(map[string]any{"inputConfig": map[string]any{"gcsSource": map[string]any{"uri": []any{"gs://" + bucket + "/tab.csv"}}}})
	tabop, err := dc.CreateDataset(ctx, &aiplatformpb.CreateDatasetRequest{Parent: parent, Dataset: &aiplatformpb.Dataset{
		DisplayName: "tab-" + suffix, MetadataSchemaUri: dsSchema + "metadata/tabular_1.0.0.yaml", Metadata: meta}})
	if err != nil {
		t.Fatalf("a tabular dataset: %v", err)
	}
	tab, err := tabop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if op, err := dc.DeleteDataset(context.Background(), &aiplatformpb.DeleteDatasetRequest{Name: tab.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	}()
	if _, err := dc.ImportData(ctx, &aiplatformpb.ImportDataRequest{Name: tab.GetName(), ImportConfigs: []*aiplatformpb.ImportDataConfig{{
		Source:          &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://" + bucket + "/tab.csv"}}},
		ImportSchemaUri: dsSchema + "ioformat/image_classification_single_label_io_format_1.0.0.yaml"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("ImportData on a tabular dataset = %v, want InvalidArgument", err)
	}

	// AutoML is refused.
	automl, _ := structpb.NewValue(map[string]any{"targetColumn": "y", "predictionType": "regression"})
	if _, err := pc.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: parent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
		DisplayName: "compat-automl-" + suffix, TrainingTaskDefinition: "gs://google-cloud-aiplatform/schema/trainingjob/definition/automl_tabular_1.0.0.yaml",
		TrainingTaskInputs: automl, InputDataConfig: &aiplatformpb.InputDataConfig{DatasetId: strings.TrimPrefix(tab.GetName(), parent+"/datasets/")}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("an AutoML pipeline = %v, want Unimplemented", err)
	}

	// The annotation set can be deleted.
	if sop, err := dc.DeleteSavedQuery(ctx, &aiplatformpb.DeleteSavedQueryRequest{Name: query.GetName()}); err != nil {
		t.Errorf("DeleteSavedQuery: %v", err)
	} else if err := sop.Wait(ctx); err != nil {
		t.Errorf("DeleteSavedQuery's operation: %v", err)
	}
	if dop, err := dc.DeleteDatasetVersion(ctx, &aiplatformpb.DeleteDatasetVersionRequest{Name: ver.GetName()}); err != nil {
		t.Errorf("DeleteDatasetVersion: %v", err)
	} else if err := dop.Wait(ctx); err != nil {
		t.Errorf("DeleteDatasetVersion's operation: %v", err)
	}
}

// TestPythonVertexAIDatasets (#2210): the pinned Vertex AI SDK
// (testdata/gcpcpy) creates an ImageDataset and a TextDataset from import
// files and a TabularDataset from a CSV file, lists and exports the image
// dataset, and runs a CustomContainerTrainingJob on the image dataset (a
// fraction split) and on the tabular one, each job's container recording
// the AIP_*_DATA_URI variables it was given (testdata/gcpcpy/datasets.py).
func TestPythonVertexAIDatasets(t *testing.T) {
	h := New(t)
	endpoint := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	shardEndpoint(h, "CLOUDBURROW_TEST_RUN_STORAGE", EnvStorage)
	gcs := dataformStorage(t, h)
	creds := os.Getenv("CLOUDBURROW_TEST_CREDENTIALS")
	if creds == "" {
		t.Skip("CLOUDBURROW_TEST_CREDENTIALS is not set: the SDK needs the instance's ADC fixture")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "docker", "build", "-t", gcpcPyImage, "testdata/gcpcpy")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the gcpcpy fixture: %v\n%s", err, out)
	}
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	bucket := "vx-pyds-" + suffix
	dataformBucket(ctx, t, gcs, h.Project(), bucket)
	var lines []string
	for i, label := range []string{"daisy", "rose", "daisy", "tulip"} {
		obj := fmt.Sprintf("img/%d.jpg", i)
		dsPutObject(t, ctx, gcs, bucket, obj, fmt.Sprintf("image %d", i))
		lines = append(lines, `{"imageGcsUri": "gs://`+bucket+"/"+obj+`", "classificationAnnotation": {"displayName": "`+label+`"}}`)
	}
	dsPutObject(t, ctx, gcs, bucket, "flowers.jsonl", strings.Join(lines, "\n")+"\n")
	dsPutObject(t, ctx, gcs, bucket, "reviews.jsonl", `{"textContent": "lovely", "sentimentAnnotation": {"sentiment": 1, "sentimentMax": 1}}`+"\n")
	dsPutObject(t, ctx, gcs, bucket, "tab.csv", "x,y\n1,3\n2,5\n3,7\n4,9\n5,11\n6,13\n7,15\n8,17\n9,19\n10,21\n")

	_, port, err := net.SplitHostPort(stripScheme(endpoint))
	if err != nil {
		t.Fatal(err)
	}
	network := internalNetwork(t, ctx, gcpcPyImage, port)
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", network,
		"-v", filepath.Join(moduleRoot(t), "testdata", "gcpcpy", "datasets.py")+":/flow/datasets.py:ro",
		"-v", creds+":/creds/adc.json:ro",
		"-e", "CB_ENDPOINT=http://host.docker.internal:"+port,
		"-e", "CB_PROJECT="+h.Project(), "-e", "CB_LOCATION=us-central1",
		"-e", "CB_CREDENTIALS=/creds/adc.json", "-e", "CB_TASK_IMAGE="+gcpcPyImage, "-e", "CB_BUCKET="+bucket,
		"-e", "GOOGLE_CLOUD_UNIVERSE_DOMAIN=cloudburrow.localhost",
		gcpcPyImage, "/flow/datasets.py")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the datasets flow: %v\n%s", err, out)
	}
	var res struct {
		Image struct {
			Name, Schema string
			Items        int
		}
		Text struct {
			Name  string
			Items int
		}
		Tabular          struct{ Name, Schema string }
		Listed, Exported []string
		Training         struct{ State, Name string }
		TabularTraining  struct{ State string } `json:"tabular_training"`
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if s, ok := strings.CutPrefix(l, "RESULT "); ok {
			line = s
		}
	}
	if err := json.Unmarshal([]byte(line), &res); err != nil {
		t.Fatalf("the flow printed no result (%v):\n%s", err, out)
	}
	if !strings.Contains(res.Image.Name, "/datasets/") || res.Image.Schema != dsSchema+"metadata/image_1.0.0.yaml" || res.Image.Items != 4 {
		t.Errorf("ImageDataset = %+v", res.Image)
	}
	if res.Text.Items != 1 {
		t.Errorf("TextDataset = %+v", res.Text)
	}
	if res.Tabular.Schema != dsSchema+"metadata/tabular_1.0.0.yaml" {
		t.Errorf("TabularDataset = %+v", res.Tabular)
	}
	if len(res.Listed) != 1 || res.Listed[0] != res.Image.Name {
		t.Errorf("ImageDataset.list = %v", res.Listed)
	}
	if len(res.Exported) != 1 || !strings.HasPrefix(res.Exported[0], "gs://"+bucket+"/export/export-data-") {
		t.Errorf("export_data = %v", res.Exported)
	}
	if res.Training.State != "PIPELINE_STATE_SUCCEEDED" || res.TabularTraining.State != "PIPELINE_STATE_SUCCEEDED" {
		t.Fatalf("the training jobs = %+v %+v\n%s", res.Training, res.TabularTraining, out)
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(dsReadObject(t, ctx, gcs, bucket, "tp/model/env.json")), &env); err != nil {
		t.Fatal(err)
	}
	if env["AIP_DATA_FORMAT"] != "jsonl" || !strings.Contains(env["AIP_TRAINING_DATA_URI"], "/training-") ||
		!strings.Contains(env["AIP_VALIDATION_DATA_URI"], "/validation-") || !strings.Contains(env["AIP_TEST_DATA_URI"], "/test-") {
		t.Errorf("the image job's data env = %v", env)
	}
	env = nil
	if err := json.Unmarshal([]byte(dsReadObject(t, ctx, gcs, bucket, "tab-tp/model/env.json")), &env); err != nil {
		t.Fatal(err)
	}
	if env["AIP_DATA_FORMAT"] != "csv" || !strings.HasSuffix(env["AIP_TRAINING_DATA_URI"], ".csv") {
		t.Errorf("the tabular job's data env = %v", env)
	}
}
