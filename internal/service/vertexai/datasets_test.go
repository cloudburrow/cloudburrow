package vertexai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
)

const dsParent = "projects/p/locations/us-central1"

func opResult(t *testing.T, s *Server, op *longrunningpb.Operation) *longrunningpb.Operation {
	t.Helper()
	got, err := (&operations{s: s}).WaitOperation(context.Background(), &longrunningpb.WaitOperationRequest{Name: op.GetName()})
	if err != nil || !got.GetDone() {
		t.Fatalf("operation %s = %v, %v", op.GetName(), got, err)
	}
	return got
}

func mustDataset(t *testing.T, s *Server, d *datasetService, display, schema string, meta map[string]any) *aiplatformpb.Dataset {
	t.Helper()
	ds := &aiplatformpb.Dataset{DisplayName: display, MetadataSchemaUri: metadataPrefix + schema}
	if meta != nil {
		v, _ := structpb.NewValue(meta)
		ds.Metadata = v
	}
	op, err := d.CreateDataset(context.Background(), &aiplatformpb.CreateDatasetRequest{Parent: dsParent, Dataset: ds})
	if err != nil {
		t.Fatal(err)
	}
	got := opResult(t, s, op)
	out := &aiplatformpb.Dataset{}
	if err := got.GetResponse().UnmarshalTo(out); err != nil {
		t.Fatal(err)
	}
	return out
}

func importOp(t *testing.T, s *Server, d *datasetService, ds, schema string, uris ...string) *longrunningpb.Operation {
	t.Helper()
	op, err := d.ImportData(context.Background(), &aiplatformpb.ImportDataRequest{Name: ds, ImportConfigs: []*aiplatformpb.ImportDataConfig{{
		Source:          &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: uris}},
		ImportSchemaUri: ioformatPrefix + schema, DataItemLabels: map[string]string{"batch": "one"}}}})
	if err != nil {
		t.Fatal(err)
	}
	return opResult(t, s, op)
}

func TestDatasetsCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := newPipelineServer(t)
	d := &datasetService{s: s}
	for name, bad := range map[string]*aiplatformpb.Dataset{
		"no display name": {MetadataSchemaUri: metadataPrefix + "image_1.0.0.yaml"},
		"no schema":       {DisplayName: "x"},
		"unknown schema":  {DisplayName: "x", MetadataSchemaUri: metadataPrefix + "bogus_1.0.0.yaml"},
		"long name":       {DisplayName: strings.Repeat("x", 129), MetadataSchemaUri: metadataPrefix + "image_1.0.0.yaml"},
	} {
		if _, err := d.CreateDataset(ctx, &aiplatformpb.CreateDatasetRequest{Parent: dsParent, Dataset: bad}); codeOf(err) != codes.InvalidArgument {
			t.Errorf("create %s = %v, want INVALID_ARGUMENT", name, err)
		}
	}
	img := mustDataset(t, s, d, "flowers", "image_1.0.0.yaml", nil)
	if !strings.HasPrefix(img.GetName(), dsParent+"/datasets/") || img.GetEtag() == "" || img.GetCreateTime() == nil {
		t.Fatalf("created = %v", img)
	}
	mustDataset(t, s, d, "reviews", "text_1.0.0.yaml", nil)
	list, err := d.ListDatasets(ctx, &aiplatformpb.ListDatasetsRequest{Parent: dsParent, Filter: `display_name="flowers"`})
	if err != nil || len(list.GetDatasets()) != 1 || list.GetDatasets()[0].GetName() != img.GetName() {
		t.Fatalf("list = %v %v", list, err)
	}
	if _, err := d.UpdateDataset(ctx, &aiplatformpb.UpdateDatasetRequest{Dataset: &aiplatformpb.Dataset{Name: img.GetName(), DisplayName: "y"}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("update with no mask = %v", err)
	}
	if _, err := d.UpdateDataset(ctx, &aiplatformpb.UpdateDatasetRequest{Dataset: &aiplatformpb.Dataset{Name: img.GetName(), MetadataSchemaUri: "x"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"metadata_schema_uri"}}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("update of metadata_schema_uri = %v", err)
	}
	up, err := d.UpdateDataset(ctx, &aiplatformpb.UpdateDatasetRequest{Dataset: &aiplatformpb.Dataset{Name: img.GetName(), DisplayName: "flowers2", Labels: map[string]string{"team": "ml"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "labels"}}})
	if err != nil || up.GetDisplayName() != "flowers2" || up.GetLabels()["team"] != "ml" || up.GetEtag() == img.GetEtag() {
		t.Fatalf("update = %v %v", up, err)
	}
	// It persists.
	s2 := NewServer(nil)
	s2.SetNotebookRuntimes(newFakeHost(), s.notebooks().file)
	if got, err := (&datasetService{s: s2}).GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: img.GetName()}); err != nil || got.GetDisplayName() != "flowers2" {
		t.Fatalf("after a restart = %v %v", got, err)
	}
	op, err := d.DeleteDataset(ctx, &aiplatformpb.DeleteDatasetRequest{Name: img.GetName()})
	if err != nil || !op.GetDone() {
		t.Fatal(op, err)
	}
	if _, err := d.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: img.GetName()}); codeOf(err) != codes.NotFound {
		t.Errorf("get deleted = %v", err)
	}
}

func TestDatasetImportAndExport(t *testing.T) {
	ctx := context.Background()
	s, r := newPipelineServer(t)
	d := &datasetService{s: s}
	r.objects["b/img/1.jpg"] = []byte("one")
	r.objects["b/img/2.png"] = []byte("two")
	r.objects["b/img/copy.jpg"] = []byte("one") // the same bytes as 1.jpg
	r.objects["b/single.jsonl"] = []byte(`{"imageGcsUri": "gs://b/img/1.jpg", "classificationAnnotation": {"displayName": "daisy", "annotationResourceLabels": {"who": "me"}}, "dataItemResourceLabels": {"aiplatform.googleapis.com/ml_use": "test"}}
{"imageGcsUri": "gs://b/img/2.png", "classificationAnnotation": {"displayName": "rose"}}
`)
	r.objects["b/more.csv"] = []byte("TRAINING,gs://b/img/copy.jpg,daisy\ngs://b/img/2.png,tulip\n")
	ds := mustDataset(t, s, d, "flowers", "image_1.0.0.yaml", nil)

	if _, err := d.ImportData(ctx, &aiplatformpb.ImportDataRequest{Name: ds.GetName(), ImportConfigs: []*aiplatformpb.ImportDataConfig{{
		Source:          &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://b/x.jsonl"}}},
		ImportSchemaUri: ioformatPrefix + "text_extraction_io_format_1.0.0.yaml"}}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("a text format on an image dataset = %v", err)
	}
	if got := importOp(t, s, d, ds.GetName(), "image_classification_single_label_io_format_1.0.0.yaml", "gs://b/single.jsonl"); got.GetError() != nil {
		t.Fatal(got.GetError())
	}
	if got := importOp(t, s, d, ds.GetName(), "image_classification_single_label_io_format_1.0.0.yaml", "gs://b/more.csv"); got.GetError() != nil {
		t.Fatal(got.GetError())
	}
	view, _ := d.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: ds.GetName()})
	if view.GetDataItemCount() != 2 {
		t.Fatalf("data_item_count = %d, want 2 (identical bytes are one item)", view.GetDataItemCount())
	}
	if len(view.GetSavedQueries()) != 1 || view.GetSavedQueries()[0].GetProblemType() != "IMAGE_CLASSIFICATION_SINGLE_LABEL" || view.GetSavedQueries()[0].GetAnnotationSpecCount() != 3 {
		t.Fatalf("saved queries = %v", view.GetSavedQueries())
	}
	items, err := d.ListDataItems(ctx, &aiplatformpb.ListDataItemsRequest{Parent: ds.GetName(), Filter: `labels.aiplatform.googleapis.com/ml_use="training"`})
	if err != nil || len(items.GetDataItems()) != 1 {
		t.Fatalf("items with ml_use training = %v %v", items, err)
	}
	it := items.GetDataItems()[0]
	if it.GetLabels()["batch"] != "one" || it.GetPayload().GetStructValue().GetFields()["mimeType"].GetStringValue() != "image/jpeg" {
		t.Errorf("item = %v", it)
	}
	anns, err := d.ListAnnotations(ctx, &aiplatformpb.ListAnnotationsRequest{Parent: it.GetName()})
	// The CSV's daisy differs from the JSON Lines' in its labels, so it is
	// an annotation of its own.
	if err != nil || len(anns.GetAnnotations()) != 2 {
		t.Fatalf("annotations = %v %v", anns, err)
	}
	a := anns.GetAnnotations()[0]
	if a.GetLabels()["who"] != "me" {
		a = anns.GetAnnotations()[1]
	}
	if a.GetPayloadSchemaUri() != annotationPrefix+"image_classification_1.0.0.yaml" || a.GetLabels()[payloadSchemaLabel] != "ImageClassification" || a.GetLabels()[annotationSetLabel] == "" {
		t.Errorf("annotation = %v", a)
	}
	spec := a.GetPayload().GetStructValue().GetFields()["annotationSpecId"].GetStringValue()
	sp, err := d.GetAnnotationSpec(ctx, &aiplatformpb.GetAnnotationSpecRequest{Name: ds.GetName() + "/annotationSpecs/" + spec})
	if err != nil || sp.GetDisplayName() != "daisy" {
		t.Fatalf("spec = %v %v", sp, err)
	}
	all, err := d.ListAnnotations(ctx, &aiplatformpb.ListAnnotationsRequest{Parent: ds.GetName() + "/dataItems/-"})
	if err != nil || len(all.GetAnnotations()) != 4 {
		t.Fatalf("every annotation = %d %v", len(all.GetAnnotations()), err)
	}
	search, err := d.SearchDataItems(ctx, &aiplatformpb.SearchDataItemsRequest{Dataset: ds.GetName(), Order: &aiplatformpb.SearchDataItemsRequest_OrderByDataItem{OrderByDataItem: "create_time"}})
	if err != nil || len(search.GetDataItemViews()) != 2 || len(search.GetDataItemViews()[1].GetAnnotations()) != 2 {
		t.Fatalf("search = %v %v", search, err)
	}

	// ExportData: export-data-NAME-TIME/<annotation set>/..., each line in
	// the import format.
	op, err := d.ExportData(ctx, &aiplatformpb.ExportDataRequest{Name: ds.GetName(), ExportConfig: &aiplatformpb.ExportDataConfig{
		Destination: &aiplatformpb.ExportDataConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://b/out"}}}})
	if err != nil {
		t.Fatal(err)
	}
	done := opResult(t, s, op)
	resp := &aiplatformpb.ExportDataResponse{}
	if done.GetError() != nil || done.GetResponse().UnmarshalTo(resp) != nil || len(resp.GetExportedFiles()) != 1 {
		t.Fatalf("export = %v", done)
	}
	f := resp.GetExportedFiles()[0]
	if !strings.HasPrefix(f, "gs://b/out/export-data-flowers-") || !strings.HasSuffix(f, "/image_classification_single_label/data-00000-of-00001.jsonl") {
		t.Errorf("exported file = %s", f)
	}
	body := string(r.objects[strings.TrimPrefix(f, "gs://")])
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("export = %q", body)
	}
	var first map[string]any
	for _, l := range lines {
		if strings.Contains(l, "1.jpg") {
			_ = json.Unmarshal([]byte(l), &first)
		}
	}
	if first["imageGcsUri"] != "gs://b/img/1.jpg" || first["classificationAnnotation"].(map[string]any)["displayName"] != "daisy" ||
		first["dataItemResourceLabels"].(map[string]any)["aiplatform.googleapis.com/ml_use"] != "training" {
		t.Errorf("1.jpg's line = %v", first)
	}

	// CUSTOM_CODE_TRAINING: training-*, validation-*, test-* files.
	op, err = d.ExportData(ctx, &aiplatformpb.ExportDataRequest{Name: ds.GetName(), ExportConfig: &aiplatformpb.ExportDataConfig{
		Destination: &aiplatformpb.ExportDataConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://b/train"}},
		ExportUse:   aiplatformpb.ExportDataConfig_CUSTOM_CODE_TRAINING,
		Split: &aiplatformpb.ExportDataConfig_FilterSplit{FilterSplit: &aiplatformpb.ExportFilterSplit{
			TrainingFilter: `labels.aiplatform.googleapis.com/ml_use="training"`, ValidationFilter: "-", TestFilter: `labels.aiplatform.googleapis.com/ml_use="test"`}}}})
	if err != nil {
		t.Fatal(err)
	}
	done = opResult(t, s, op)
	resp = &aiplatformpb.ExportDataResponse{}
	if done.GetError() != nil || done.GetResponse().UnmarshalTo(resp) != nil || len(resp.GetExportedFiles()) != 3 {
		t.Fatalf("training export = %v", done)
	}
	if !strings.HasSuffix(resp.GetExportedFiles()[0], "/training-*.jsonl") || resp.GetDataStats().GetTrainingDataItemsCount() != 1 ||
		resp.GetDataStats().GetTestDataItemsCount() != 0 || resp.GetDataStats().GetValidationDataItemsCount() != 0 {
		t.Errorf("training export = %v (2.png has no ml_use; 1.jpg is training after the CSV import)", resp)
	}

	// Dataset versions: a snapshot to restore.
	vop, err := d.CreateDatasetVersion(ctx, &aiplatformpb.CreateDatasetVersionRequest{Parent: ds.GetName(), DatasetVersion: &aiplatformpb.DatasetVersion{DisplayName: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	ver := &aiplatformpb.DatasetVersion{}
	if err := opResult(t, s, vop).GetResponse().UnmarshalTo(ver); err != nil || ver.GetDisplayName() != "v1" {
		t.Fatalf("version = %v %v", ver, err)
	}
	r.objects["b/img/3.gif"] = []byte("three")
	r.objects["b/third.jsonl"] = []byte(`{"imageGcsUri": "gs://b/img/3.gif"}`)
	importOp(t, s, d, ds.GetName(), "image_classification_single_label_io_format_1.0.0.yaml", "gs://b/third.jsonl")
	if v, _ := d.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: ds.GetName()}); v.GetDataItemCount() != 3 {
		t.Fatalf("after a third import: %d items", v.GetDataItemCount())
	}
	rop, err := d.RestoreDatasetVersion(ctx, &aiplatformpb.RestoreDatasetVersionRequest{Name: ver.GetName()})
	if err != nil || opResult(t, s, rop).GetError() != nil {
		t.Fatal(err)
	}
	if v, _ := d.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: ds.GetName()}); v.GetDataItemCount() != 2 {
		t.Fatalf("after restoring v1: %d items, want 2", v.GetDataItemCount())
	}
	vl, err := d.ListDatasetVersions(ctx, &aiplatformpb.ListDatasetVersionsRequest{Parent: ds.GetName()})
	if err != nil || len(vl.GetDatasetVersions()) != 1 {
		t.Fatalf("versions = %v %v", vl, err)
	}
}

func TestDatasetImportFormats(t *testing.T) {
	ctx := context.Background()
	s, r := newPipelineServer(t)
	d := &datasetService{s: s}
	r.objects["b/a.jpg"] = []byte("a")
	r.objects["b/v.mp4"] = []byte("video")
	r.objects["b/boxes.csv"] = []byte("gs://b/a.jpg,cat,0.1,0.2,,,0.5,0.6,,\n")
	r.objects["b/badbox.jsonl"] = []byte(`{"imageGcsUri": "gs://b/a.jpg", "boundingBoxAnnotations": [{"displayName": "cat", "xMin": 0.6, "yMin": 0.1, "xMax": 0.5, "yMax": 0.9}]}`)
	r.objects["b/ents.jsonl"] = []byte(`{"textContent": "Paris is in France", "textSegmentAnnotations": [{"startOffset": 0, "endOffset": 5, "displayName": "city"}]}`)
	r.objects["b/sent.jsonl"] = []byte(`{"textContent": "great", "sentimentAnnotation": {"sentiment": 4, "sentimentMax": 4}}`)
	r.objects["b/track.jsonl"] = []byte(`{"videoGcsUri": "gs://b/v.mp4", "temporalBoundingBoxAnnotations": [{"displayName": "car", "xMin": 0, "yMin": 0, "xMax": 0.5, "yMax": 0.5, "timeOffset": "1.5s", "instanceId": 1}]}`)
	r.objects["b/clip.jsonl"] = []byte(`{"videoGcsUri": "gs://b/v.mp4", "timeSegmentAnnotations": [{"displayName": "run", "startTime": "1s", "endTime": "2.5s"}]}`)

	img := mustDataset(t, s, d, "boxes", "image_1.0.0.yaml", nil)
	if got := importOp(t, s, d, img.GetName(), "image_bounding_box_io_format_1.0.0.yaml", "gs://b/badbox.jsonl"); got.GetError() == nil || codes.Code(got.GetError().GetCode()) != codes.InvalidArgument {
		t.Errorf("xMin > xMax = %v", got)
	}
	if got := importOp(t, s, d, img.GetName(), "image_bounding_box_io_format_1.0.0.yaml", "gs://b/boxes.csv"); got.GetError() != nil {
		t.Fatal(got.GetError())
	}
	all, _ := d.ListAnnotations(ctx, &aiplatformpb.ListAnnotationsRequest{Parent: img.GetName() + "/dataItems/-"})
	if len(all.GetAnnotations()) != 1 {
		t.Fatalf("boxes = %v", all)
	}
	p := all.GetAnnotations()[0].GetPayload().GetStructValue().AsMap()
	if p["xMin"] != 0.1 || p["yMax"] != 0.6 || p["displayName"] != "cat" {
		t.Errorf("box = %v", p)
	}

	text := mustDataset(t, s, d, "ents", "text_1.0.0.yaml", nil)
	if got := importOp(t, s, d, text.GetName(), "text_extraction_io_format_1.0.0.yaml", "gs://b/ents.jsonl"); got.GetError() != nil {
		t.Fatal(got.GetError())
	}
	if got := importOp(t, s, d, text.GetName(), "text_sentiment_io_format_1.0.0.yaml", "gs://b/sent.jsonl"); got.GetError() != nil {
		t.Fatal(got.GetError())
	}
	all, _ = d.ListAnnotations(ctx, &aiplatformpb.ListAnnotationsRequest{Parent: text.GetName() + "/dataItems/-", Filter: `payload_schema_uri="` + annotationPrefix + `text_extraction_1.0.0.yaml"`})
	if len(all.GetAnnotations()) != 1 {
		t.Fatalf("entities = %v", all)
	}
	seg := all.GetAnnotations()[0].GetPayload().GetStructValue().AsMap()["textSegment"].(map[string]any)
	if seg["content"] != "Paris" || seg["startOffset"] != "0" || seg["endOffset"] != "5" {
		t.Errorf("segment = %v", seg)
	}
	sq, _ := d.ListSavedQueries(ctx, &aiplatformpb.ListSavedQueriesRequest{Parent: text.GetName()})
	if len(sq.GetSavedQueries()) != 2 {
		t.Errorf("saved queries = %v", sq)
	}

	video := mustDataset(t, s, d, "clips", "video_1.0.0.yaml", nil)
	for _, c := range [][2]string{{"video_object_tracking_io_format_1.0.0.yaml", "gs://b/track.jsonl"}, {"video_classification_io_format_1.0.0.yaml", "gs://b/clip.jsonl"}} {
		if got := importOp(t, s, d, video.GetName(), c[0], c[1]); got.GetError() != nil {
			t.Fatal(c, got.GetError())
		}
	}
	all, _ = d.ListAnnotations(ctx, &aiplatformpb.ListAnnotationsRequest{Parent: video.GetName() + "/dataItems/-", Filter: `payload_schema_uri="` + annotationPrefix + `video_classification_1.0.0.yaml"`})
	if len(all.GetAnnotations()) != 1 {
		t.Fatalf("segments = %v", all)
	}
	ts := all.GetAnnotations()[0].GetPayload().GetStructValue().AsMap()["timeSegment"].(map[string]any)
	if ts["startTimeOffset"] != "1s" || ts["endTimeOffset"] != "2.5s" {
		t.Errorf("time segment = %v", ts)
	}

	tab := mustDataset(t, s, d, "rows", "tabular_1.0.0.yaml", map[string]any{"inputConfig": map[string]any{"gcsSource": map[string]any{"uri": []any{"gs://b/t.csv"}}}})
	if _, err := d.ImportData(ctx, &aiplatformpb.ImportDataRequest{Name: tab.GetName(), ImportConfigs: []*aiplatformpb.ImportDataConfig{{
		Source:          &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{"gs://b/t.csv"}}},
		ImportSchemaUri: ioformatPrefix + "image_bounding_box_io_format_1.0.0.yaml"}}}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("import into a tabular dataset = %v", err)
	}
}

func waitTraining(t *testing.T, p *pipelineService, name string) *aiplatformpb.TrainingPipeline {
	t.Helper()
	var got *aiplatformpb.TrainingPipeline
	for i := 0; i < 500; i++ {
		got, _ = p.GetTrainingPipeline(context.Background(), &aiplatformpb.GetTrainingPipelineRequest{Name: name})
		if !trainingRunning(got.GetState()) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pipeline %s did not end", name)
	return nil
}

func TestTrainingPipelineOnADataset(t *testing.T) {
	ctx := context.Background()
	s, r := newRichServer(t)
	d := &datasetService{s: s}
	p := &pipelineService{s: s}
	var csvRows strings.Builder
	csvRows.WriteString("x,y,split\n")
	for i := 0; i < 10; i++ {
		split := "training"
		if i == 8 {
			split = "validation"
		} else if i == 9 {
			split = "test"
		}
		csvRows.WriteString(strings.Repeat("1", i+1) + ",0," + split + "\n")
	}
	r.objects["b/t.csv"] = []byte(csvRows.String())
	tab := mustDataset(t, s, d, "rows", "tabular_1.0.0.yaml", map[string]any{"inputConfig": map[string]any{"gcsSource": map[string]any{"uri": []any{"gs://b/t.csv"}}}})
	id := filepath.Base(tab.GetName())
	inputs, _ := structpb.NewValue(map[string]any{"workerPoolSpecs": []any{map[string]any{"machineSpec": map[string]any{"machineType": "n1-standard-4"},
		"replicaCount": "1", "containerSpec": map[string]any{"imageUri": "local/train:1"}}}})
	mk := func(c *aiplatformpb.InputDataConfig) (*aiplatformpb.TrainingPipeline, error) {
		return p.CreateTrainingPipeline(ctx, &aiplatformpb.CreateTrainingPipelineRequest{Parent: dsParent, TrainingPipeline: &aiplatformpb.TrainingPipeline{
			DisplayName: "on-data", TrainingTaskDefinition: customTaskDefinition, TrainingTaskInputs: inputs, InputDataConfig: c}})
	}
	gcs := &aiplatformpb.InputDataConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: "gs://b/data"}}
	for name, bad := range map[string]*aiplatformpb.InputDataConfig{
		"no dataset":     {Destination: gcs},
		"missing":        {DatasetId: "999", Destination: gcs},
		"no destination": {DatasetId: id},
		"filter split":   {DatasetId: id, Destination: gcs, Split: &aiplatformpb.InputDataConfig_FilterSplit{FilterSplit: &aiplatformpb.FilterSplit{TrainingFilter: "-"}}},
		"bigquery from a CSV source": {DatasetId: id, Destination: &aiplatformpb.InputDataConfig_BigqueryDestination{
			BigqueryDestination: &aiplatformpb.BigQueryDestination{OutputUri: "bq://p"}}},
		"fractions over 1": {DatasetId: id, Destination: gcs, Split: &aiplatformpb.InputDataConfig_FractionSplit{FractionSplit: &aiplatformpb.FractionSplit{TrainingFraction: 0.9, TestFraction: 0.2}}},
	} {
		if _, err := mk(bad); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	tp, err := mk(&aiplatformpb.InputDataConfig{DatasetId: id, Destination: gcs, Split: &aiplatformpb.InputDataConfig_PredefinedSplit{PredefinedSplit: &aiplatformpb.PredefinedSplit{Key: "split"}}})
	if err != nil {
		t.Fatal(err)
	}
	got := waitTraining(t, p, tp.GetName())
	if got.GetState() != aiplatformpb.PipelineState_PIPELINE_STATE_SUCCEEDED {
		t.Fatalf("pipeline = %v %v", got.GetState(), got.GetError())
	}
	r.mu.Lock()
	env := r.runs[len(r.runs)-1].Env
	r.mu.Unlock()
	if env["AIP_DATA_FORMAT"] != "csv" || !strings.HasPrefix(env["AIP_TRAINING_DATA_URI"], "gs://b/data/dataset-"+id+"-tables-") || !strings.HasSuffix(env["AIP_TEST_DATA_URI"], "/test-*.csv") {
		t.Fatalf("env = %v", env)
	}
	train := strings.TrimPrefix(strings.Replace(env["AIP_TRAINING_DATA_URI"], "*", "00000-of-00001", 1), "gs://")
	if lines := strings.Split(strings.TrimSpace(string(r.objects[train])), "\n"); len(lines) != 9 || lines[0] != "x,y,split" {
		t.Errorf("training file %s = %q", train, r.objects[train])
	}
	test := strings.TrimPrefix(strings.Replace(env["AIP_TEST_DATA_URI"], "*", "00000-of-00001", 1), "gs://")
	if !strings.Contains(string(r.objects[test]), "1111111111,0,test") {
		t.Errorf("test file = %q", r.objects[test])
	}
}

func TestAutoMLRefusals(t *testing.T) {
	for def, want := range map[string]string{
		"gs://google-cloud-aiplatform/schema/trainingjob/definition/automl_text_classification_1.0.0.yaml":  "AutoML Text training is no longer available",
		"gs://google-cloud-aiplatform/schema/trainingjob/definition/automl_video_classification_1.0.0.yaml": "AutoML Video is no longer available",
		"gs://google-cloud-aiplatform/schema/trainingjob/definition/automl_image_classification_1.0.0.yaml": "Google's own model search",
	} {
		if got := automlRefusal(def); !strings.Contains(got, want) {
			t.Errorf("%s: %s", def, got)
		}
	}
}

// The REST surface the Vertex AI SDK sends (api_transport="rest"):
// datasets, :import, dataItems and the dataset's operations.
func TestDatasetsREST(t *testing.T) {
	s, r := newPipelineServer(t)
	r.objects["b/a.jpg"] = []byte("a")
	r.objects["b/i.jsonl"] = []byte(`{"imageGcsUri": "gs://b/a.jpg", "classificationAnnotation": {"displayName": "cat"}}`)
	h := httptest.NewServer(s.Handler())
	defer h.Close()
	call := func(method, path, body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(method, h.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s = %d %v", method, path, resp.StatusCode, out)
		}
		return out
	}
	op := call("POST", "/v1/"+dsParent+"/datasets", `{"displayName": "pets", "metadataSchemaUri": "`+metadataPrefix+`image_1.0.0.yaml"}`)
	name := op["response"].(map[string]any)["name"].(string)
	if !strings.HasPrefix(op["name"].(string), name+"/operations/") {
		t.Fatalf("operation = %v", op)
	}
	op = call("POST", "/v1/"+name+":import", `{"importConfigs": [{"gcsSource": {"uris": ["gs://b/i.jsonl"]}, "importSchemaUri": "`+ioformatPrefix+`image_classification_single_label_io_format_1.0.0.yaml"}]}`)
	for i := 0; i < 200 && op["done"] != true; i++ {
		time.Sleep(10 * time.Millisecond)
		op = call("GET", "/v1/"+op["name"].(string), "")
	}
	if op["done"] != true || op["error"] != nil {
		t.Fatalf("import = %v", op)
	}
	items := call("GET", "/v1/"+name+"/dataItems", "")
	if l, _ := items["dataItems"].([]any); len(l) != 1 {
		t.Fatalf("dataItems = %v", items)
	}
	if got := call("GET", "/v1/"+name, ""); got["dataItemCount"] != "1" {
		t.Errorf("dataset = %v", got)
	}
}
