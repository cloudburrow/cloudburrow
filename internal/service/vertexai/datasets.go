package vertexai

// Vertex AI managed datasets (#2210): v1 DatasetService. Image, text,
// video, tabular and time series datasets (metadata_schema_uri
// gs://google-cloud-aiplatform/schema/dataset/metadata/{image,text,video,
// tabular,time_series}_1.0.0.yaml); ImportData from Cloud Storage import
// files in Google's import formats (datasetformats.go); ExportData to Cloud
// Storage; data items, annotations, annotation specs, saved queries
// (annotation sets) and dataset versions. TrainingPipelines read them
// through input_data_config (datasetsplit.go).
//
// Documented behaviour (the aiplatform v1 protos, aiplatform v1.127.0;
// https://docs.cloud.google.com/vertex-ai/docs/reference/rest/v1/projects.locations.datasets,
// .../datasets/create-dataset-api, .../datasets/export-metadata-annotations,
// the ioformat, annotation and dataitem schema files under
// gs://google-cloud-aiplatform/schema/dataset/; read 2026-11-24):
//   - display_name (up to 128 UTF-8 characters) and metadata_schema_uri are
//     required; create, delete, import, export, and creating or restoring a
//     version are long-running operations;
//   - update takes an update_mask of display_name, description and labels;
//   - ImportData's data_item_labels and annotation_labels apply to every
//     imported item and annotation, the import file's
//     dataItemResourceLabels and annotationResourceLabels overriding them;
//     identical items (identical content bytes) are imported once, their
//     labels merged; identical annotations once;
//   - an annotation carries the system labels
//     aiplatform.googleapis.com/annotation_set_name (its SavedQuery) and
//     aiplatform.googleapis.com/payload_schema (its payload schema's title);
//   - ExportData writes export-data-DISPLAY_NAME-TIMESTAMP under the
//     output_uri_prefix, a subdirectory a annotation set, its lines in the
//     import format; with export_use CUSTOM_CODE_TRAINING, the training,
//     validation and test files, exported_files their wildcard paths and
//     data_stats their counts.
//
// UNVERIFIED (re-read 2026-11-24; not documented; chosen and kept stable): the annotation set
// an import makes (one a problem type, its display name the problem type in
// lower case), the export's file names (data-00000-of-00001.jsonl), the
// <annotation-type> of a training export's directory (the problem type in
// lower case with hyphens, "tables" for tabular data), and a text item's
// payload keeping its gcsUri beside the schema's content.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

const (
	schemaPrefix     = "gs://google-cloud-aiplatform/schema/dataset/"
	metadataPrefix   = schemaPrefix + "metadata/"
	annotationPrefix = schemaPrefix + "annotation/"
	dataItemPrefix   = schemaPrefix + "dataitem/"
	ioformatPrefix   = schemaPrefix + "ioformat/"

	mlUseLabel         = "aiplatform.googleapis.com/ml_use"
	annotationSetLabel = "aiplatform.googleapis.com/annotation_set_name"
	payloadSchemaLabel = "aiplatform.googleapis.com/payload_schema"
)

// datasetKinds are the metadata schemas served, by file name, and their
// kind.
var datasetKinds = map[string]string{
	"image_1.0.0.yaml": "image", "text_1.0.0.yaml": "text", "video_1.0.0.yaml": "video",
	"tabular_1.0.0.yaml": "tabular", "time_series_1.0.0.yaml": "tabular",
}

// datasetKind is a metadata schema URI's kind, or "".
func datasetKind(uri string) string {
	if !strings.HasPrefix(uri, metadataPrefix) {
		return ""
	}
	return datasetKinds[strings.TrimPrefix(uri, metadataPrefix)]
}

// dsStore is the datasets and what they hold; the notebook store's mutex
// guards it.
type dsStore struct {
	datasets map[string]*aiplatformpb.Dataset
	items    map[string]*aiplatformpb.DataItem
	anns     map[string]*aiplatformpb.Annotation
	specs    map[string]*aiplatformpb.AnnotationSpec
	queries  map[string]*aiplatformpb.SavedQuery
	versions map[string]*aiplatformpb.DatasetVersion
	// hashes are each item's content hash, by item name, for import's
	// dedupe; snaps each version's saved contents.
	hashes map[string]string
	snaps  map[string]json.RawMessage
}

func newDSStore() *dsStore {
	return &dsStore{datasets: map[string]*aiplatformpb.Dataset{}, items: map[string]*aiplatformpb.DataItem{},
		anns: map[string]*aiplatformpb.Annotation{}, specs: map[string]*aiplatformpb.AnnotationSpec{},
		queries: map[string]*aiplatformpb.SavedQuery{}, versions: map[string]*aiplatformpb.DatasetVersion{},
		hashes: map[string]string{}, snaps: map[string]json.RawMessage{}}
}

// dsets is the store's datasets; the caller holds st.mu.
func (st *notebookStore) dsets() *dsStore {
	if st.ds == nil {
		st.ds = newDSStore()
	}
	return st.ds
}

// dsSaved is the datasets' saved form, and a version's snapshot.
type dsSaved struct {
	Datasets []json.RawMessage          `json:"datasets,omitempty"`
	Items    []json.RawMessage          `json:"dataItems,omitempty"`
	Anns     []json.RawMessage          `json:"annotations,omitempty"`
	Specs    []json.RawMessage          `json:"annotationSpecs,omitempty"`
	Queries  []json.RawMessage          `json:"savedQueries,omitempty"`
	Versions []json.RawMessage          `json:"datasetVersions,omitempty"`
	Hashes   map[string]string          `json:"hashes,omitempty"`
	Snaps    map[string]json.RawMessage `json:"snapshots,omitempty"`
}

func marshalUnder[V proto.Message](m map[string]V, prefix string) []json.RawMessage {
	var out []json.RawMessage
	for _, k := range sortedNames(m) {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		if b, err := protojson.Marshal(m[k]); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// savedDatasets is the datasets' saved form; the caller holds st.mu.
func (st *notebookStore) savedDatasets() json.RawMessage {
	d := st.ds
	if d == nil || len(d.datasets) == 0 {
		return nil
	}
	b, err := json.Marshal(dsSaved{Datasets: marshalUnder(d.datasets, ""), Items: marshalUnder(d.items, ""), Anns: marshalUnder(d.anns, ""),
		Specs: marshalUnder(d.specs, ""), Queries: marshalUnder(d.queries, ""), Versions: marshalUnder(d.versions, ""), Hashes: d.hashes, Snaps: d.snaps})
	if err != nil {
		return nil
	}
	return b
}

func (d *dsStore) load(in dsSaved) {
	loadAll(in.Datasets, func() *aiplatformpb.Dataset { return &aiplatformpb.Dataset{} }, d.datasets, (*aiplatformpb.Dataset).GetName)
	loadAll(in.Items, func() *aiplatformpb.DataItem { return &aiplatformpb.DataItem{} }, d.items, (*aiplatformpb.DataItem).GetName)
	loadAll(in.Anns, func() *aiplatformpb.Annotation { return &aiplatformpb.Annotation{} }, d.anns, (*aiplatformpb.Annotation).GetName)
	loadAll(in.Specs, func() *aiplatformpb.AnnotationSpec { return &aiplatformpb.AnnotationSpec{} }, d.specs, (*aiplatformpb.AnnotationSpec).GetName)
	loadAll(in.Queries, func() *aiplatformpb.SavedQuery { return &aiplatformpb.SavedQuery{} }, d.queries, (*aiplatformpb.SavedQuery).GetName)
	loadAll(in.Versions, func() *aiplatformpb.DatasetVersion { return &aiplatformpb.DatasetVersion{} }, d.versions, (*aiplatformpb.DatasetVersion).GetName)
	for k, v := range in.Hashes {
		d.hashes[k] = v
	}
	for k, v := range in.Snaps {
		d.snaps[k] = v
	}
}

// loadDatasets restores savedDatasets' output; the caller holds st.mu.
func (st *notebookStore) loadDatasets(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var in dsSaved
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	st.dsets().load(in)
}

// ---- names

// parseDatasetName parses projects/{p}/locations/{l}/datasets/{d}.
func parseDatasetName(field, name string) (string, error) {
	if _, _, _, err := parseChild(field, "datasets", name); err != nil {
		return "", err
	}
	return name, nil
}

// parseDatasetChild parses DATASET/{collection}/{id}, answering the dataset.
func parseDatasetChild(field, collection, name string) (string, error) {
	p := strings.Split(name, "/")
	if len(p) == 8 && p[0] == "projects" && p[2] == "locations" && p[4] == "datasets" && p[6] == collection && p[1] != "" && p[3] != "" && p[5] != "" && p[7] != "" {
		return strings.Join(p[:6], "/"), nil
	}
	return "", apierror.InvalidArgument("%s %q is not projects/{project}/locations/{location}/datasets/{dataset}/%s/{id}", field, name, collection)
}

// parseDataItemName parses DATASET/dataItems/{i}, answering the dataset.
func parseDataItemName(field, name string) (string, error) {
	return parseDatasetChild(field, "dataItems", name)
}

func (d *dsStore) dataset(name string) (*aiplatformpb.Dataset, error) {
	if _, err := parseDatasetName("name", name); err != nil {
		return nil, err
	}
	ds, ok := d.datasets[name]
	if !ok {
		return nil, apierror.NotFound("Dataset %s not found", name)
	}
	return ds, nil
}

func dsEtag() string {
	h := sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano())))
	return hex.EncodeToString(h[:8])
}

func uniqueName(prefix string, taken func(string) bool) string {
	n := prefix + regNumericID()
	for taken(n) {
		n = prefix + regNumericID()
	}
	return n
}

// view is a dataset as Get and List answer it: its data item count and
// saved queries current.
func (d *dsStore) view(ds *aiplatformpb.Dataset) *aiplatformpb.Dataset {
	out := proto.Clone(ds).(*aiplatformpb.Dataset)
	var n int64
	for k := range d.items {
		if strings.HasPrefix(k, ds.GetName()+"/dataItems/") {
			n++
		}
	}
	out.DataItemCount = n
	out.SavedQueries = nil
	for _, k := range sortedNames(d.queries) {
		if strings.HasPrefix(k, ds.GetName()+"/savedQueries/") {
			out.SavedQueries = append(out.SavedQueries, d.querySpecCount(d.queries[k]))
		}
	}
	return out
}

func (d *dsStore) querySpecCount(q *aiplatformpb.SavedQuery) *aiplatformpb.SavedQuery {
	out := proto.Clone(q).(*aiplatformpb.SavedQuery)
	id := q.GetName()[strings.LastIndex(q.GetName(), "/")+1:]
	specs := map[string]bool{}
	for _, a := range d.anns {
		if a.GetLabels()[annotationSetLabel] == id && strings.HasPrefix(a.GetName(), strings.TrimSuffix(q.GetName(), "/savedQueries/"+id)+"/") {
			if s := a.GetPayload().GetStructValue().GetFields()["annotationSpecId"].GetStringValue(); s != "" {
				specs[s] = true
			}
		}
	}
	out.AnnotationSpecCount = int32(len(specs))
	return out
}

// ---- the service

type datasetService struct {
	aiplatformpb.UnimplementedDatasetServiceServer
	s *Server
}

func checkTabularMetadata(ds *aiplatformpb.Dataset) error {
	in := ds.GetMetadata().GetStructValue().GetFields()["inputConfig"].GetStructValue().GetFields()
	if in == nil {
		return nil
	}
	if g := in["gcsSource"].GetStructValue().GetFields()["uri"]; g != nil {
		var uris []string
		if l := g.GetListValue(); l != nil {
			for _, v := range l.GetValues() {
				uris = append(uris, v.GetStringValue())
			}
		} else {
			uris = append(uris, g.GetStringValue())
		}
		for _, u := range uris {
			if _, _, err := splitNotebookGCS("dataset.metadata.inputConfig.gcsSource.uri", u, true); err != nil {
				return err
			}
		}
	}
	if b := in["bigquerySource"].GetStructValue().GetFields()["uri"]; b != nil {
		if !dsBQTableRE.MatchString(b.GetStringValue()) {
			return apierror.InvalidArgument("dataset.metadata.inputConfig.bigquerySource.uri %q is not bq://PROJECT.DATASET.TABLE", b.GetStringValue())
		}
	}
	return nil
}

func (d *datasetService) CreateDataset(_ context.Context, req *aiplatformpb.CreateDatasetRequest) (*longrunningpb.Operation, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	in := req.GetDataset()
	switch {
	case in == nil:
		return nil, grpcError(apierror.InvalidArgument("dataset is required"))
	case in.GetDisplayName() == "":
		return nil, grpcError(apierror.InvalidArgument("dataset.display_name is required"))
	case utf8Len(in.GetDisplayName()) > 128:
		return nil, grpcError(apierror.InvalidArgument("dataset.display_name can be up to 128 characters long"))
	case in.GetMetadataSchemaUri() == "":
		return nil, grpcError(apierror.InvalidArgument("dataset.metadata_schema_uri is required"))
	case datasetKind(in.GetMetadataSchemaUri()) == "":
		return nil, grpcError(apierror.InvalidArgument("dataset.metadata_schema_uri %q is not one of the dataset metadata schemas served: %simage_1.0.0.yaml, "+
			"text_1.0.0.yaml, video_1.0.0.yaml, tabular_1.0.0.yaml or time_series_1.0.0.yaml", in.GetMetadataSchemaUri(), metadataPrefix))
	}
	if err := checkLabels(in.GetLabels()); err != nil {
		return nil, grpcError(err)
	}
	if datasetKind(in.GetMetadataSchemaUri()) == "tabular" {
		if err := checkTabularMetadata(in); err != nil {
			return nil, grpcError(err)
		}
	}
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	out := proto.Clone(in).(*aiplatformpb.Dataset)
	out.Name = uniqueName(req.GetParent()+"/datasets/", func(n string) bool { return ds.datasets[n] != nil })
	now := timestamppb.New(st.now())
	out.CreateTime, out.UpdateTime, out.Etag, out.DataItemCount, out.SavedQueries = now, now, dsEtag(), 0, nil
	if out.Metadata == nil {
		out.Metadata = structpb.NewStructValue(&structpb.Struct{Fields: map[string]*structpb.Value{}})
	}
	ds.datasets[out.Name] = out
	st.save()
	view := ds.view(out)
	st.mu.Unlock()
	return d.s.finished(out.Name, &aiplatformpb.CreateDatasetOperationMetadata{GenericMetadata: generic()}, view, nil)
}

func (d *datasetService) GetDataset(_ context.Context, req *aiplatformpb.GetDatasetRequest) (*aiplatformpb.Dataset, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	ds, err := st.dsets().dataset(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return st.dsets().view(ds), nil
}

func (d *datasetService) UpdateDataset(_ context.Context, req *aiplatformpb.UpdateDatasetRequest) (*aiplatformpb.Dataset, error) {
	in := req.GetDataset()
	if in == nil {
		return nil, grpcError(apierror.InvalidArgument("dataset is required"))
	}
	if len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, grpcError(apierror.InvalidArgument("update_mask is required: the updatable fields are display_name, description and labels"))
	}
	st := d.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	ds, err := st.dsets().dataset(in.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	if in.GetEtag() != "" && in.GetEtag() != ds.GetEtag() {
		return nil, grpcError(apierror.Aborted("the dataset's etag %q is not its current one", in.GetEtag()))
	}
	next := proto.Clone(ds).(*aiplatformpb.Dataset)
	for _, p := range req.GetUpdateMask().GetPaths() {
		switch p {
		case "display_name", "displayName":
			if in.GetDisplayName() == "" || utf8Len(in.GetDisplayName()) > 128 {
				return nil, grpcError(apierror.InvalidArgument("dataset.display_name must be 1 to 128 characters long"))
			}
			next.DisplayName = in.GetDisplayName()
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			if err := checkLabels(in.GetLabels()); err != nil {
				return nil, grpcError(err)
			}
			next.Labels = in.GetLabels()
		default:
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated: the updatable fields are display_name, description and labels", p))
		}
	}
	next.UpdateTime, next.Etag = timestamppb.New(st.now()), dsEtag()
	st.dsets().datasets[next.Name] = next
	st.save()
	return st.dsets().view(next), nil
}

var (
	datasetFilter = append([]aipfilter.Option{
		aipfilter.Fields("display_name", "metadata_schema_uri", "create_time", "update_time", "labels"),
	}, bothCases(nil, nil)...)
	datasetOrder = aipfilter.Fields("display_name", "create_time", "update_time")
)

func pageSize(n int32) int {
	if n <= 0 {
		return 100
	}
	if n > 1000 {
		return 1000
	}
	return int(n)
}

func (d *datasetService) ListDatasets(_ context.Context, req *aiplatformpb.ListDatasetsRequest) (*aiplatformpb.ListDatasetsResponse, error) {
	if _, _, err := parseParent("parent", req.GetParent()); err != nil {
		return nil, grpcError(err)
	}
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	var all []*aiplatformpb.Dataset
	for _, k := range sortedNames(ds.datasets) {
		if strings.HasPrefix(k, req.GetParent()+"/datasets/") {
			all = append(all, ds.view(ds.datasets[k]))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, datasetFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	order := req.GetOrderBy()
	if order == "" {
		order = "create_time desc"
	}
	if err := aipfilter.SortBy(order, all, datasetOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("datasets:"+req.GetParent()+"|"+req.GetFilter()+"|"+order, all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListDatasetsResponse{Datasets: items, NextPageToken: next}, nil
}

// dropUnder removes every entry of m under prefix.
func dropUnder[V any](m map[string]V, prefix string) {
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			delete(m, k)
		}
	}
}

func (d *datasetService) DeleteDataset(_ context.Context, req *aiplatformpb.DeleteDatasetRequest) (*longrunningpb.Operation, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, err := ds.dataset(req.GetName()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	p := req.GetName() + "/"
	delete(ds.datasets, req.GetName())
	dropUnder(ds.items, p)
	dropUnder(ds.anns, p)
	dropUnder(ds.specs, p)
	dropUnder(ds.queries, p)
	dropUnder(ds.versions, p)
	dropUnder(ds.hashes, p)
	dropUnder(ds.snaps, p)
	st.save()
	st.mu.Unlock()
	return d.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

// ---- data items, annotations, annotation specs, saved queries

var (
	dataItemFilter = append([]aipfilter.Option{aipfilter.Fields("create_time", "update_time", "labels", "name")}, bothCases(nil, nil)...)
	dataItemOrder  = aipfilter.Fields("create_time", "update_time", "name")
	annFilter      = append([]aipfilter.Option{aipfilter.Fields("create_time", "update_time", "labels", "payload_schema_uri", "name")}, bothCases(nil, nil)...)
)

func (d *datasetService) ListDataItems(_ context.Context, req *aiplatformpb.ListDataItemsRequest) (*aiplatformpb.ListDataItemsResponse, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, err := ds.dataset(req.GetParent()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	var all []*aiplatformpb.DataItem
	for _, k := range sortedNames(ds.items) {
		if strings.HasPrefix(k, req.GetParent()+"/dataItems/") {
			all = append(all, proto.Clone(ds.items[k]).(*aiplatformpb.DataItem))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, dataItemFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, dataItemOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("dataItems:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListDataItemsResponse{DataItems: items, NextPageToken: next}, nil
}

func (d *datasetService) ListAnnotations(_ context.Context, req *aiplatformpb.ListAnnotationsRequest) (*aiplatformpb.ListAnnotationsResponse, error) {
	dsName, err := parseDataItemName("parent", req.GetParent())
	if err != nil {
		return nil, grpcError(err)
	}
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, err := ds.dataset(dsName); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	wild := strings.HasSuffix(req.GetParent(), "/dataItems/-")
	if _, ok := ds.items[req.GetParent()]; !ok && !wild {
		st.mu.Unlock()
		return nil, grpcError(apierror.NotFound("DataItem %s not found", req.GetParent()))
	}
	prefix := req.GetParent() + "/annotations/"
	if wild {
		prefix = dsName + "/dataItems/"
	}
	var all []*aiplatformpb.Annotation
	for _, k := range sortedNames(ds.anns) {
		if strings.HasPrefix(k, prefix) {
			all = append(all, proto.Clone(ds.anns[k]).(*aiplatformpb.Annotation))
		}
	}
	st.mu.Unlock()
	all, err = aipfilter.Apply(req.GetFilter(), all, annFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, dataItemOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("annotations:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListAnnotationsResponse{Annotations: items, NextPageToken: next}, nil
}

func (d *datasetService) GetAnnotationSpec(_ context.Context, req *aiplatformpb.GetAnnotationSpecRequest) (*aiplatformpb.AnnotationSpec, error) {
	if _, err := parseDatasetChild("name", "annotationSpecs", req.GetName()); err != nil {
		return nil, grpcError(err)
	}
	st := d.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	sp, ok := st.dsets().specs[req.GetName()]
	if !ok {
		return nil, grpcError(apierror.NotFound("AnnotationSpec %s not found", req.GetName()))
	}
	return proto.Clone(sp).(*aiplatformpb.AnnotationSpec), nil
}

func (d *datasetService) ListSavedQueries(_ context.Context, req *aiplatformpb.ListSavedQueriesRequest) (*aiplatformpb.ListSavedQueriesResponse, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, err := ds.dataset(req.GetParent()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	var all []*aiplatformpb.SavedQuery
	for _, k := range sortedNames(ds.queries) {
		if strings.HasPrefix(k, req.GetParent()+"/savedQueries/") {
			all = append(all, ds.querySpecCount(ds.queries[k]))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, append([]aipfilter.Option{aipfilter.Fields("display_name", "problem_type", "create_time", "update_time")}, bothCases(nil, nil)...)...)
	if err != nil {
		return nil, grpcError(err)
	}
	if err := aipfilter.SortBy(req.GetOrderBy(), all, datasetOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("savedQueries:"+req.GetParent()+"|"+req.GetFilter()+"|"+req.GetOrderBy(), all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListSavedQueriesResponse{SavedQueries: items, NextPageToken: next}, nil
}

func (d *datasetService) DeleteSavedQuery(_ context.Context, req *aiplatformpb.DeleteSavedQueryRequest) (*longrunningpb.Operation, error) {
	if _, err := parseDatasetChild("name", "savedQueries", req.GetName()); err != nil {
		return nil, grpcError(err)
	}
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, ok := ds.queries[req.GetName()]; !ok {
		st.mu.Unlock()
		return nil, grpcError(apierror.NotFound("SavedQuery %s not found", req.GetName()))
	}
	delete(ds.queries, req.GetName())
	st.save()
	st.mu.Unlock()
	return d.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

// SearchDataItems: the data items of a dataset with their annotations,
// filtered by data_item_filter and annotation_filters (or a saved query's
// annotation set), as the method documents.
func (d *datasetService) SearchDataItems(_ context.Context, req *aiplatformpb.SearchDataItemsRequest) (*aiplatformpb.SearchDataItemsResponse, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, err := ds.dataset(req.GetDataset()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	if req.GetDataLabelingJob() != "" {
		st.mu.Unlock()
		return nil, grpcError(apierror.Unimplemented("data labeling jobs are not served on this instance (docs/gaps.md)"))
	}
	setID := ""
	if q := req.GetSavedQuery(); q != "" {
		if _, ok := ds.queries[q]; !ok {
			st.mu.Unlock()
			return nil, grpcError(apierror.NotFound("SavedQuery %s not found", q))
		}
		setID = q[strings.LastIndex(q, "/")+1:]
	}
	var items []*aiplatformpb.DataItem
	byItem := map[string][]*aiplatformpb.Annotation{}
	for _, k := range sortedNames(ds.items) {
		if strings.HasPrefix(k, req.GetDataset()+"/dataItems/") {
			items = append(items, proto.Clone(ds.items[k]).(*aiplatformpb.DataItem))
		}
	}
	for _, k := range sortedNames(ds.anns) {
		a := ds.anns[k]
		if strings.HasPrefix(k, req.GetDataset()+"/dataItems/") && (setID == "" || a.GetLabels()[annotationSetLabel] == setID) {
			item := k[:strings.Index(k, "/annotations/")]
			byItem[item] = append(byItem[item], proto.Clone(a).(*aiplatformpb.Annotation))
		}
	}
	st.mu.Unlock()
	items, err := aipfilter.Apply(req.GetDataItemFilter(), items, dataItemFilter...)
	if err != nil {
		return nil, grpcError(err)
	}
	order := req.GetOrderByDataItem()
	if order == "" {
		order = req.GetOrderBy()
	}
	if err := aipfilter.SortBy(order, items, dataItemOrder); err != nil {
		return nil, grpcError(err)
	}
	filters := req.GetAnnotationFilters()
	if f := req.GetAnnotationsFilter(); f != "" {
		filters = append([]string{f}, filters...)
	}
	var views []*aiplatformpb.DataItemView
	for _, it := range items {
		anns := byItem[it.GetName()]
		for _, f := range filters {
			if anns, err = aipfilter.Apply(f, anns, annFilter...); err != nil {
				return nil, grpcError(err)
			}
		}
		if req.GetAnnotationsLimit() > 0 && int(req.GetAnnotationsLimit()) < len(anns) {
			anns = anns[:req.GetAnnotationsLimit()]
		}
		views = append(views, &aiplatformpb.DataItemView{DataItem: it, Annotations: anns})
	}
	page, next, err := aipfilter.Page("searchDataItems:"+req.GetDataset()+"|"+req.GetDataItemFilter()+"|"+order, views, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.SearchDataItemsResponse{DataItemViews: page, NextPageToken: next}, nil
}

// ---- dataset versions

// snapshot is the dataset's items, annotations, specs and saved queries,
// for a version; the caller holds st.mu.
func (d *dsStore) snapshot(dataset string) json.RawMessage {
	p := dataset + "/"
	hashes := map[string]string{}
	for k, v := range d.hashes {
		if strings.HasPrefix(k, p) {
			hashes[k] = v
		}
	}
	b, _ := json.Marshal(dsSaved{Items: marshalUnder(d.items, p), Anns: marshalUnder(d.anns, p), Specs: marshalUnder(d.specs, p),
		Queries: marshalUnder(d.queries, p), Hashes: hashes})
	return b
}

func (d *datasetService) CreateDatasetVersion(_ context.Context, req *aiplatformpb.CreateDatasetVersionRequest) (*longrunningpb.Operation, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	parent, err := ds.dataset(req.GetParent())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	in := req.GetDatasetVersion()
	if in == nil {
		in = &aiplatformpb.DatasetVersion{}
	}
	if utf8Len(in.GetDisplayName()) > 128 {
		st.mu.Unlock()
		return nil, grpcError(apierror.InvalidArgument("dataset_version.display_name can be up to 128 characters long"))
	}
	v := &aiplatformpb.DatasetVersion{DisplayName: in.GetDisplayName(), Metadata: proto.Clone(parent.GetMetadata()).(*structpb.Value)}
	v.Name = uniqueName(parent.GetName()+"/datasetVersions/", func(n string) bool { return ds.versions[n] != nil })
	now := timestamppb.New(st.now())
	v.CreateTime, v.UpdateTime, v.Etag = now, now, dsEtag()
	ds.versions[v.Name] = v
	ds.snaps[v.Name] = ds.snapshot(parent.GetName())
	st.save()
	out := proto.Clone(v).(*aiplatformpb.DatasetVersion)
	st.mu.Unlock()
	return d.s.finished(parent.GetName(), &aiplatformpb.CreateDatasetVersionOperationMetadata{GenericMetadata: generic()}, out, nil)
}

func (d *dsStore) version(name string) (*aiplatformpb.DatasetVersion, error) {
	if _, err := parseDatasetChild("name", "datasetVersions", name); err != nil {
		return nil, err
	}
	v, ok := d.versions[name]
	if !ok {
		return nil, apierror.NotFound("DatasetVersion %s not found", name)
	}
	return v, nil
}

func (d *datasetService) GetDatasetVersion(_ context.Context, req *aiplatformpb.GetDatasetVersionRequest) (*aiplatformpb.DatasetVersion, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	v, err := st.dsets().version(req.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	return proto.Clone(v).(*aiplatformpb.DatasetVersion), nil
}

func (d *datasetService) UpdateDatasetVersion(_ context.Context, req *aiplatformpb.UpdateDatasetVersionRequest) (*aiplatformpb.DatasetVersion, error) {
	in := req.GetDatasetVersion()
	if in == nil || len(req.GetUpdateMask().GetPaths()) == 0 {
		return nil, grpcError(apierror.InvalidArgument("dataset_version and update_mask are required: the updatable field is display_name"))
	}
	st := d.s.notebooks()
	st.mu.Lock()
	defer st.mu.Unlock()
	v, err := st.dsets().version(in.GetName())
	if err != nil {
		return nil, grpcError(err)
	}
	for _, p := range req.GetUpdateMask().GetPaths() {
		if p != "display_name" && p != "displayName" {
			return nil, grpcError(apierror.InvalidArgument("update_mask path %q cannot be updated: the updatable field is display_name", p))
		}
	}
	if utf8Len(in.GetDisplayName()) > 128 {
		return nil, grpcError(apierror.InvalidArgument("dataset_version.display_name can be up to 128 characters long"))
	}
	v.DisplayName, v.UpdateTime, v.Etag = in.GetDisplayName(), timestamppb.New(st.now()), dsEtag()
	st.save()
	return proto.Clone(v).(*aiplatformpb.DatasetVersion), nil
}

func (d *datasetService) ListDatasetVersions(_ context.Context, req *aiplatformpb.ListDatasetVersionsRequest) (*aiplatformpb.ListDatasetVersionsResponse, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, err := ds.dataset(req.GetParent()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	var all []*aiplatformpb.DatasetVersion
	for _, k := range sortedNames(ds.versions) {
		if strings.HasPrefix(k, req.GetParent()+"/datasetVersions/") {
			all = append(all, proto.Clone(ds.versions[k]).(*aiplatformpb.DatasetVersion))
		}
	}
	st.mu.Unlock()
	all, err := aipfilter.Apply(req.GetFilter(), all, append([]aipfilter.Option{aipfilter.Fields("display_name", "create_time", "update_time")}, bothCases(nil, nil)...)...)
	if err != nil {
		return nil, grpcError(err)
	}
	order := req.GetOrderBy()
	if order == "" {
		order = "create_time desc"
	}
	if err := aipfilter.SortBy(order, all, datasetOrder); err != nil {
		return nil, grpcError(err)
	}
	items, next, err := aipfilter.Page("datasetVersions:"+req.GetParent()+"|"+req.GetFilter()+"|"+order, all, req.GetPageToken(), pageSize(req.GetPageSize()))
	if err != nil {
		return nil, grpcError(err)
	}
	return &aiplatformpb.ListDatasetVersionsResponse{DatasetVersions: items, NextPageToken: next}, nil
}

func (d *datasetService) DeleteDatasetVersion(_ context.Context, req *aiplatformpb.DeleteDatasetVersionRequest) (*longrunningpb.Operation, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	if _, err := ds.version(req.GetName()); err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	delete(ds.versions, req.GetName())
	delete(ds.snaps, req.GetName())
	st.save()
	st.mu.Unlock()
	return d.s.finished(req.GetName(), &aiplatformpb.DeleteOperationMetadata{GenericMetadata: generic()}, &emptypb.Empty{}, nil)
}

// RestoreDatasetVersion puts the dataset's items, annotations, specs and
// saved queries back as the version saved them.
func (d *datasetService) RestoreDatasetVersion(_ context.Context, req *aiplatformpb.RestoreDatasetVersionRequest) (*longrunningpb.Operation, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds := st.dsets()
	v, err := ds.version(req.GetName())
	if err != nil {
		st.mu.Unlock()
		return nil, grpcError(err)
	}
	dataset := req.GetName()[:strings.Index(req.GetName(), "/datasetVersions/")]
	var snap dsSaved
	_ = json.Unmarshal(ds.snaps[req.GetName()], &snap)
	p := dataset + "/"
	dropUnder(ds.items, p)
	dropUnder(ds.anns, p)
	dropUnder(ds.specs, p)
	dropUnder(ds.queries, p)
	dropUnder(ds.hashes, p)
	ds.load(dsSaved{Items: snap.Items, Anns: snap.Anns, Specs: snap.Specs, Queries: snap.Queries, Hashes: snap.Hashes})
	if parent := ds.datasets[dataset]; parent != nil {
		parent.UpdateTime = timestamppb.New(st.now())
	}
	st.save()
	out := proto.Clone(v).(*aiplatformpb.DatasetVersion)
	st.mu.Unlock()
	return d.s.finished(req.GetName(), &aiplatformpb.RestoreDatasetVersionOperationMetadata{GenericMetadata: generic()}, out, nil)
}

// ---- import and export

func (d *datasetService) ImportData(_ context.Context, req *aiplatformpb.ImportDataRequest) (*longrunningpb.Operation, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds, err := st.dsets().dataset(req.GetName())
	var kind string
	if err == nil {
		kind = datasetKind(ds.GetMetadataSchemaUri())
	}
	runner := st.pipelineRunner
	st.mu.Unlock()
	if err != nil {
		return nil, grpcError(err)
	}
	if kind == "tabular" {
		return nil, grpcError(apierror.InvalidArgument("ImportData is not supported for tabular datasets: a tabular dataset reads its data from metadata.inputConfig " +
			"(gcsSource or bigquerySource), set when it is created"))
	}
	if len(req.GetImportConfigs()) == 0 {
		return nil, grpcError(apierror.InvalidArgument("import_configs is required"))
	}
	var formats []*ioFormat
	for i, c := range req.GetImportConfigs() {
		f, err := ioFormatFor(c.GetImportSchemaUri())
		if err != nil {
			return nil, grpcError(apierror.InvalidArgument("import_configs[%d].import_schema_uri: %v", i, apierror.From(err).Message))
		}
		if f.kind != kind {
			return nil, grpcError(apierror.InvalidArgument("import_configs[%d].import_schema_uri %s imports %s data, and the dataset %s is a %s dataset",
				i, c.GetImportSchemaUri(), f.kind, req.GetName(), kind))
		}
		if len(c.GetGcsSource().GetUris()) == 0 {
			return nil, grpcError(apierror.InvalidArgument("import_configs[%d].gcs_source.uris is required", i))
		}
		for _, u := range c.GetGcsSource().GetUris() {
			if _, _, err := splitNotebookGCS(fmt.Sprintf("import_configs[%d].gcs_source.uris", i), u, true); err != nil {
				return nil, grpcError(err)
			}
		}
		if err := checkLabels(c.GetDataItemLabels()); err != nil {
			return nil, grpcError(err)
		}
		formats = append(formats, f)
	}
	if runner == nil {
		return nil, grpcError(apierror.FailedPrecondition("ImportData reads Cloud Storage, which this instance does not serve (start with --services including storage)"))
	}
	op := d.s.newOp(req.GetName())
	_ = d.s.ops.SetMetadata(op, &aiplatformpb.ImportDataOperationMetadata{GenericMetadata: generic()})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		for i, c := range req.GetImportConfigs() {
			if err := d.s.importData(ctx, runner, req.GetName(), c, formats[i]); err != nil {
				_ = d.s.ops.Fail(op, err)
				return
			}
		}
		_ = d.s.ops.Succeed(op, &aiplatformpb.ImportDataResponse{})
	}()
	return d.s.opView(op)
}

func (d *datasetService) ExportData(_ context.Context, req *aiplatformpb.ExportDataRequest) (*longrunningpb.Operation, error) {
	st := d.s.notebooks()
	st.mu.Lock()
	ds, err := st.dsets().dataset(req.GetName())
	var display, kind string
	if err == nil {
		display, kind = ds.GetDisplayName(), datasetKind(ds.GetMetadataSchemaUri())
	}
	runner := st.pipelineRunner
	st.mu.Unlock()
	if err != nil {
		return nil, grpcError(err)
	}
	cfg := req.GetExportConfig()
	prefix := cfg.GetGcsDestination().GetOutputUriPrefix()
	if prefix == "" {
		return nil, grpcError(apierror.InvalidArgument("export_config.gcs_destination.output_uri_prefix is required"))
	}
	if _, _, err := splitNotebookGCS("export_config.gcs_destination.output_uri_prefix", prefix, true); err != nil {
		return nil, grpcError(err)
	}
	if kind == "tabular" {
		return nil, grpcError(apierror.FailedPrecondition("ExportData exports a dataset's data items and annotations, and the tabular dataset %s has none: "+
			"its data is the source its metadata.inputConfig names", req.GetName()))
	}
	if cfg.GetSavedQueryId() != "" && cfg.GetAnnotationSchemaUri() != "" {
		return nil, grpcError(apierror.InvalidArgument("export_config: only one of saved_query_id and annotation_schema_uri may be set"))
	}
	sel := dsSelect{annotationsFilter: cfg.GetAnnotationsFilter(), savedQuery: cfg.GetSavedQueryId(), schema: cfg.GetAnnotationSchemaUri()}
	switch sp := cfg.GetSplit().(type) {
	case *aiplatformpb.ExportDataConfig_FractionSplit:
		f := sp.FractionSplit
		sel.fraction = &aiplatformpb.FractionSplit{TrainingFraction: f.GetTrainingFraction(), ValidationFraction: f.GetValidationFraction(), TestFraction: f.GetTestFraction()}
	case *aiplatformpb.ExportDataConfig_FilterSplit:
		f := sp.FilterSplit
		sel.filter = &aiplatformpb.FilterSplit{TrainingFilter: f.GetTrainingFilter(), ValidationFilter: f.GetValidationFilter(), TestFilter: f.GetTestFilter()}
	}
	if sel.fraction != nil {
		if err := checkFractions(sel.fraction.GetTrainingFraction(), sel.fraction.GetValidationFraction(), sel.fraction.GetTestFraction(), "export_config.fraction_split"); err != nil {
			return nil, grpcError(err)
		}
	}
	if runner == nil {
		return nil, grpcError(apierror.FailedPrecondition("ExportData writes Cloud Storage, which this instance does not serve (start with --services including storage)"))
	}
	now := time.Now().UTC()
	dir := strings.TrimSuffix(prefix, "/") + "/export-data-" + display + "-" + now.Format("2006-01-02T15:04:05.000000Z")
	op := d.s.newOp(req.GetName())
	meta := &aiplatformpb.ExportDataOperationMetadata{GenericMetadata: generic(), GcsOutputDirectory: dir}
	_ = d.s.ops.SetMetadata(op, meta)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		var resp *aiplatformpb.ExportDataResponse
		var err error
		if cfg.GetExportUse() == aiplatformpb.ExportDataConfig_CUSTOM_CODE_TRAINING {
			var files [3]string
			var stats *aiplatformpb.Model_DataStats
			files, stats, _, err = d.s.exportUnstructured(ctx, runner, req.GetName(), sel, strings.TrimSuffix(prefix, "/"), now)
			resp = &aiplatformpb.ExportDataResponse{ExportedFiles: files[:], DataStats: stats}
		} else {
			var files []string
			files, err = d.s.exportSets(ctx, runner, req.GetName(), sel, dir)
			resp = &aiplatformpb.ExportDataResponse{ExportedFiles: files}
		}
		if err != nil {
			_ = d.s.ops.Fail(op, err)
			return
		}
		_ = d.s.ops.Succeed(op, resp)
	}()
	return d.s.opView(op)
}

// sortStrings is sort.Strings, returning s.
func sortStrings(s []string) []string { sort.Strings(s); return s }

// automlRefusal is why an AutoML training task definition is not run: text
// and video AutoML are shut down at Google (Vertex AI's deprecations page,
// https://docs.cloud.google.com/vertex-ai/docs/deprecations, read
// 2026-11-24: AutoML Text shut down June 15, 2025, AutoML Video July 31,
// 2025); image, tabular and forecasting AutoML train Google's own model
// search and architectures, which are not available to run locally
// (docs/gaps.md).
func automlRefusal(def string) string {
	switch {
	case strings.Contains(def, "/automl_text_"):
		return fmt.Sprintf("training_pipeline.training_task_definition %s: AutoML Text training is no longer available on Vertex AI (shut down June 15, 2025; "+
			"https://docs.cloud.google.com/vertex-ai/docs/deprecations)", def)
	case strings.Contains(def, "/automl_video_"):
		return fmt.Sprintf("training_pipeline.training_task_definition %s: AutoML Video is no longer available on Vertex AI (shut down July 31, 2025; "+
			"https://docs.cloud.google.com/vertex-ai/docs/deprecations)", def)
	}
	return fmt.Sprintf("training_pipeline.training_task_definition %s is AutoML training, which is not run on this instance: AutoML trains Google's own "+
		"model search and architectures, which are not available locally (docs/gaps.md). Train with a custom training task (%s) on the dataset instead", def, customTaskDefinition)
}
