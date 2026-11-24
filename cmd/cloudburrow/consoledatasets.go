package main

// Vertex AI managed datasets' console page (#2210): Model development >
// Datasets, in Google's layout
// (https://docs.cloud.google.com/vertex-ai/docs/image-data/classification/create-dataset,
// .../tabular-data/classification-regression/create-dataset, read
// 2026-11-24):
//
//   - "go to the Datasets page", "Click Create to open the create dataset
//     details page": Dataset name, the data type's tab (Image, Tabular,
//     Text, Video) and its objective, Region (the page's Region menu), then
//     Create, which makes the empty dataset and goes on to its data:
//   - "Select an import method": Select an import file from Cloud Storage
//     (its Cloud Storage path); a tabular dataset's data source: Select CSV
//     files from Cloud Storage, or Select a table or view from BigQuery
//     (project, dataset and table IDs); then Continue;
//   - a dataset's page: its details, its data items (Browse), and Import
//     and Export.
//
// UNVERIFIED (not documented on the pages above): the list's columns and
// the dataset page's fields and tab names. Not offered: the pages' Upload
// from your computer (CloudBurrow's console reads Cloud Storage), and the
// labeling tools (Vertex AI Data Labeling Service is shut down: Vertex
// AI's deprecations page, read 2026-11-24).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

func (v vertexAPI) datasets(ctx context.Context) (*aiplatform.DatasetClient, error) {
	if v.at() == "" {
		return nil, errors.New("the local AI endpoint is not running")
	}
	return aiplatform.NewDatasetClient(ctx, v.opts()...)
}

const (
	dsSchemaPrefix   = "gs://google-cloud-aiplatform/schema/dataset/"
	dsMetadataPrefix = dsSchemaPrefix + "metadata/"
	dsIOFormatPrefix = dsSchemaPrefix + "ioformat/"
)

// dsObjectives are each data type's objectives, as the create page's tabs
// offer them, with the import format each reads.
var (
	dsTypes      = []string{"image", "tabular", "text", "video"}
	dsTypeLabels = map[string]string{"image": "Image", "tabular": "Tabular", "text": "Text", "video": "Video"}
	dsObjectives = map[string][]string{
		"image":   {"image_classification_single_label", "image_classification_multi_label", "image_bounding_box"},
		"tabular": {"tabular", "time_series"},
		"text":    {"text_classification_single_label", "text_classification_multi_label", "text_extraction", "text_sentiment"},
		"video":   {"video_classification", "video_action_recognition", "video_object_tracking"},
	}
	dsObjectiveLabels = map[string]string{
		"image_classification_single_label": "Image classification (Single-label)", "image_classification_multi_label": "Image classification (Multi-label)",
		"image_bounding_box": "Image object detection", "tabular": "Regression/classification", "time_series": "Forecasting",
		"text_classification_single_label": "Text classification (Single-label)", "text_classification_multi_label": "Text classification (Multi-label)",
		"text_extraction": "Text entity extraction", "text_sentiment": "Text sentiment analysis", "video_classification": "Video classification",
		"video_action_recognition": "Video action recognition", "video_object_tracking": "Video object tracking",
	}
	dsKinds = map[string]string{"image_1.0.0.yaml": "Image", "text_1.0.0.yaml": "Text", "video_1.0.0.yaml": "Video",
		"tabular_1.0.0.yaml": "Tabular", "time_series_1.0.0.yaml": "Tabular (forecasting)"}
	dsGCSPattern = `^(gs://[a-z0-9][\-a-z0-9._]+/\S+)?$`
)

func dsKindOf(uri string) string {
	if k, ok := dsKinds[strings.TrimPrefix(uri, dsMetadataPrefix)]; ok {
		return k
	}
	return dashIfEmpty(uri)
}

type datasetsProvider struct{ api vertexAPI }

func (datasetsProvider) ID() string           { return "vertex-datasets" }
func (datasetsProvider) Title() string        { return "Datasets" }
func (datasetsProvider) ScopeKey() string     { return colabRegionKey }
func (datasetsProvider) CreateTitle() string  { return "Create dataset" }
func (datasetsProvider) CreateSubmit() string { return "Create" }
func (datasetsProvider) CreateLayout() string { return console.CreateLayoutPage }

func (p datasetsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{Columns: []string{"Name", "Data type", "Items", "Last updated", "Labels"},
		NameColumn: "ID", Noun: "datasets", Scope: colabScope(ctx),
		Note: "A dataset's items are its Cloud Storage files, read when they are imported; a tabular dataset reads its CSV files or BigQuery table when a model trains on it."}
	if project == "" {
		l.Prompt = "Datasets are listed per project. Choose one in the toolbar."
		return l, nil
	}
	c, err := p.api.datasets(ctx)
	if err != nil {
		return l, err
	}
	defer func() { _ = c.Close() }()
	for it := c.ListDatasets(ctx, &aiplatformpb.ListDatasetsRequest{Parent: colabParent(ctx, project)}); ; {
		d, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return l, err
		}
		items := strconv.FormatInt(d.GetDataItemCount(), 10)
		if strings.HasPrefix(dsKindOf(d.GetMetadataSchemaUri()), "Tabular") {
			items = "—"
		}
		l.Items = append(l.Items, console.Resource{Name: vertexLast(d.GetName()), Fields: map[string]string{
			"Name": dashIfEmpty(d.GetDisplayName()), "Data type": dsKindOf(d.GetMetadataSchemaUri()), "Items": items,
			"Last updated": optVertexTime(d.GetUpdateTime()), "Labels": pipelineLabels(d.GetLabels())}})
	}
	l.Total = len(l.Items)
	l.RowsOpenable = true
	return l, nil
}

func (datasetsProvider) CreateForm() (string, []console.Field) {
	const s1, s2 = "Dataset", "Data"
	f := []console.Field{
		{Name: "displayName", Label: "Dataset name", Type: "text", Required: true, Section: s1, Pattern: `^.{1,128}$`},
		{Name: "dataType", Label: "Select a data type and objective", Type: "radio", Section: s1, Options: dsTypes, OptionLabels: dsTypeLabels, Default: "image"},
	}
	for _, t := range dsTypes {
		f = append(f, console.Field{Name: "objective-" + t, Label: "Objective", Type: "radio", Section: s1, ShowWhen: "dataType=" + t,
			Options: dsObjectives[t], OptionLabels: dsObjectiveLabels, Default: dsObjectives[t][0]})
	}
	f = append(f,
		console.Field{Name: "importPath", Label: "Select an import file from Cloud Storage", Type: "text", Section: s2, ShowWhen: "dataType=image|text|video",
			Pattern: dsGCSPattern, Help: "gs://BUCKET/import.jsonl or .csv in the objective's import format. Leave empty to create an empty dataset and import later."},
		console.Field{Name: "dataSource", Label: "Select a data source", Type: "radio", Section: s2, ShowWhen: "dataType=tabular",
			Options: []string{"gcs", "bigquery"}, Default: "gcs",
			OptionLabels: map[string]string{"gcs": "Select CSV files from Cloud Storage", "bigquery": "Select a table or view from BigQuery"}},
		console.Field{Name: "csvPath", Label: "Import file path", Type: "text", Section: s2, ShowWhen: "dataSource=gcs", Pattern: dsGCSPattern,
			Help: "gs://BUCKET/data.csv; a * matches several files with the same header."},
		console.Field{Name: "bqPath", Label: "BigQuery path", Type: "text", Section: s2, ShowWhen: "dataSource=bigquery", Pattern: `^(bq://[^.\s]+\.[^.\s]+\.[^.\s]+)?$`,
			Help: "bq://PROJECT.DATASET.TABLE"},
	)
	return "Create", f
}

// datasetOf is the create page's dataset, and the import its data takes
// (nil for none).
func datasetOf(values map[string]string) (*aiplatformpb.Dataset, *aiplatformpb.ImportDataConfig, error) {
	name := strings.TrimSpace(values["displayName"])
	if name == "" {
		return nil, nil, errors.New("Dataset name is required")
	}
	t := values["dataType"]
	if t == "" {
		t = "image"
	}
	objs, ok := dsObjectives[t]
	if !ok {
		return nil, nil, fmt.Errorf("unknown data type %q", t)
	}
	obj := values["objective-"+t]
	if obj == "" {
		obj = objs[0]
	}
	known := false
	for _, o := range objs {
		known = known || o == obj
	}
	if !known {
		return nil, nil, fmt.Errorf("%q is not a %s objective", obj, dsTypeLabels[t])
	}
	ds := &aiplatformpb.Dataset{DisplayName: name}
	if t == "tabular" {
		ds.MetadataSchemaUri = dsMetadataPrefix + obj + "_1.0.0.yaml"
		var in map[string]any
		switch values["dataSource"] {
		case "bigquery":
			if p := strings.TrimSpace(values["bqPath"]); p != "" {
				in = map[string]any{"bigquerySource": map[string]any{"uri": p}}
			}
		default:
			if p := strings.TrimSpace(values["csvPath"]); p != "" {
				in = map[string]any{"gcsSource": map[string]any{"uri": []any{p}}}
			}
		}
		if in != nil {
			v, err := structpb.NewValue(map[string]any{"inputConfig": in})
			if err != nil {
				return nil, nil, err
			}
			ds.Metadata = v
		}
		return ds, nil, nil
	}
	ds.MetadataSchemaUri = dsMetadataPrefix + t + "_1.0.0.yaml"
	p := strings.TrimSpace(values["importPath"])
	if p == "" {
		return ds, nil, nil
	}
	return ds, &aiplatformpb.ImportDataConfig{Source: &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{p}}},
		ImportSchemaUri: dsIOFormatPrefix + obj + "_io_format_1.0.0.yaml"}, nil
}

func (p datasetsProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	if project == "" {
		return "", errors.New("choose a project before creating a dataset")
	}
	ds, imp, err := datasetOf(values)
	if err != nil {
		return "", err
	}
	c, err := p.api.datasets(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	op, err := c.CreateDataset(ctx, &aiplatformpb.CreateDatasetRequest{Parent: colabParent(ctx, project), Dataset: ds})
	if err != nil {
		return "", err
	}
	out, err := op.Wait(ctx)
	if err != nil {
		return "", err
	}
	if imp != nil {
		iop, err := c.ImportData(ctx, &aiplatformpb.ImportDataRequest{Name: out.GetName(), ImportConfigs: []*aiplatformpb.ImportDataConfig{imp}})
		if err != nil {
			return vertexLast(out.GetName()), err
		}
		if _, err := iop.Wait(ctx); err != nil {
			return vertexLast(out.GetName()), fmt.Errorf("the dataset was created, and its import failed: %w", err)
		}
	}
	return vertexLast(out.GetName()), nil
}

func (p datasetsProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	c, err := p.api.datasets(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	defer func() { _ = c.Close() }()
	name := colabParent(ctx, project) + "/datasets/" + path[0]
	d, err := c.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	kind := dsKindOf(d.GetMetadataSchemaUri())
	props := []console.Property{
		{Label: "Name", Value: dashIfEmpty(d.GetDisplayName())},
		{Label: "ID", Value: vertexLast(d.GetName()), Copy: d.GetName()},
		{Label: "Region", Value: vertexLocation(d.GetName())},
		{Label: "Data type", Value: kind},
		{Label: "Created", Value: optVertexTime(d.GetCreateTime())},
		{Label: "Last updated", Value: optVertexTime(d.GetUpdateTime())},
		{Label: "Labels", Value: pipelineLabels(d.GetLabels())},
	}
	groups := []console.PropertyGroup{{Heading: "Dataset", Properties: props}}
	sections := []console.Section{}
	if strings.HasPrefix(kind, "Tabular") {
		in := d.GetMetadata().GetStructValue().GetFields()["inputConfig"].GetStructValue().GetFields()
		src := "—"
		if u := in["bigquerySource"].GetStructValue().GetFields()["uri"].GetStringValue(); u != "" {
			src = u
		} else if l := in["gcsSource"].GetStructValue().GetFields()["uri"].GetListValue(); l != nil {
			var uris []string
			for _, v := range l.GetValues() {
				uris = append(uris, v.GetStringValue())
			}
			src = strings.Join(uris, ", ")
		}
		groups = append(groups, console.PropertyGroup{Heading: "Source", Properties: []console.Property{{Label: "Data source", Value: src}}})
		sections = append(sections, console.Section{ID: "source", Label: "Source", Kind: console.KindProperties, Groups: groups})
	} else {
		var sets []string
		for _, q := range d.GetSavedQueries() {
			sets = append(sets, fmt.Sprintf("%s (%s, %d labels)", q.GetDisplayName(), q.GetProblemType(), q.GetAnnotationSpecCount()))
		}
		props = append(props, console.Property{Label: "Items", Value: strconv.FormatInt(d.GetDataItemCount(), 10)},
			console.Property{Label: "Annotation sets", Value: dashIfEmpty(strings.Join(sets, "; "))})
		groups[0].Properties = props
		browse := console.Listing{Columns: []string{"Item", "Labels", "ML use"}, NameColumn: "ID", Noun: "data items"}
		for it := c.ListDataItems(ctx, &aiplatformpb.ListDataItemsRequest{Parent: name}); ; {
			di, err := it.Next()
			if err != nil {
				break
			}
			payload := di.GetPayload().GetStructValue().GetFields()
			item := payload["gcsUri"].GetStringValue()
			if item == "" {
				item = payload["content"].GetStringValue()
				if r := []rune(item); len(r) > 80 {
					item = string(r[:80]) + "…"
				}
			}
			var labels []string
			for ai := c.ListAnnotations(ctx, &aiplatformpb.ListAnnotationsRequest{Parent: di.GetName()}); ; {
				a, err := ai.Next()
				if err != nil {
					break
				}
				f := a.GetPayload().GetStructValue().GetFields()
				if n := f["displayName"].GetStringValue(); n != "" {
					labels = append(labels, n)
				} else if s := f["sentiment"]; s != nil {
					labels = append(labels, fmt.Sprintf("sentiment %v/%v", s.GetNumberValue(), f["sentimentMax"].GetNumberValue()))
				}
			}
			browse.Items = append(browse.Items, console.Resource{Name: vertexLast(di.GetName()), Fields: map[string]string{
				"Item": dashIfEmpty(item), "Labels": dashIfEmpty(strings.Join(labels, ", ")), "ML use": dashIfEmpty(di.GetLabels()["aiplatform.googleapis.com/ml_use"])}})
		}
		browse.Total = len(browse.Items)
		sections = append(sections, console.Section{ID: "browse", Label: "Browse", Listing: browse},
			console.Section{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups})
	}
	return console.Detail{Title: dashIfEmpty(d.GetDisplayName()), Sections: sections, Actions: p.DetailActions(ctx, project, path)}, nil
}

// DetailActions are Import (an image, text or video dataset's) and Export.
func (p datasetsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 || project == "" {
		return nil
	}
	c, err := p.api.datasets(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	d, err := c.GetDataset(ctx, &aiplatformpb.GetDatasetRequest{Name: colabParent(ctx, project) + "/datasets/" + path[0]})
	if err != nil || strings.HasPrefix(dsKindOf(d.GetMetadataSchemaUri()), "Tabular") {
		return nil
	}
	t := strings.ToLower(dsKindOf(d.GetMetadataSchemaUri()))
	return []console.Action{
		{ID: "import", Label: "Import", Title: "Import data", Submit: "Continue", Fields: []console.Field{
			{Name: "objective", Label: "Objective", Type: "radio", Options: dsObjectives[t], OptionLabels: dsObjectiveLabels, Default: dsObjectives[t][0]},
			{Name: "importPath", Label: "Select an import file from Cloud Storage", Type: "text", Required: true, Pattern: dsGCSPattern,
				Help: "gs://BUCKET/import.jsonl or .csv in the objective's import format."}}},
		{ID: "export", Label: "Export", Title: "Export dataset", Submit: "Export", Fields: []console.Field{
			{Name: "exportPath", Label: "Cloud Storage path", Type: "text", Required: true, Pattern: `^gs://[a-z0-9][\-a-z0-9._]+(/\S*)?$`,
				Help: "gs://BUCKET/folder: an export-data-NAME-TIME folder is written under it, one JSON Lines folder an annotation set."}}},
	}
}

func (p datasetsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("unknown action %q", action)
	}
	c, err := p.api.datasets(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	name := colabParent(ctx, project) + "/datasets/" + path[0]
	switch action {
	case "import":
		op, err := c.ImportData(ctx, &aiplatformpb.ImportDataRequest{Name: name, ImportConfigs: []*aiplatformpb.ImportDataConfig{{
			Source:          &aiplatformpb.ImportDataConfig_GcsSource{GcsSource: &aiplatformpb.GcsSource{Uris: []string{strings.TrimSpace(values["importPath"])}}},
			ImportSchemaUri: dsIOFormatPrefix + values["objective"] + "_io_format_1.0.0.yaml"}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	case "export":
		op, err := c.ExportData(ctx, &aiplatformpb.ExportDataRequest{Name: name, ExportConfig: &aiplatformpb.ExportDataConfig{
			Destination: &aiplatformpb.ExportDataConfig_GcsDestination{GcsDestination: &aiplatformpb.GcsDestination{OutputUriPrefix: strings.TrimSpace(values["exportPath"])}}}})
		if err != nil {
			return err
		}
		_, err = op.Wait(ctx)
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

func (p datasetsProvider) Delete(ctx context.Context, project, name string) error {
	c, err := p.api.datasets(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	op, err := c.DeleteDataset(ctx, &aiplatformpb.DeleteDatasetRequest{Name: colabParent(ctx, project) + "/datasets/" + name})
	if err != nil {
		return err
	}
	return op.Wait(ctx)
}
