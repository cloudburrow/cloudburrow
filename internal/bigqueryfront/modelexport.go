package bigqueryfront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// EXPORT MODEL and extract jobs of a model (#2210), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-export-model
// and https://docs.cloud.google.com/bigquery/docs/exporting-models
// (read 2026-11-24) document them:
//
//	EXPORT MODEL MODEL_NAME [OPTIONS(URI = STRING_VALUE [, TRIAL_ID = INT_VALUE])]
//
// URI, "the Cloud Storage bucket URI where the model is exported", is
// required; a model that is not there is "Not found: Model
// myproject:mydataset.mymodel". An extract job with sourceModel and a
// destinationFormat of ML_TF_SAVED_MODEL (the default) or
// ML_XGBOOST_BOOSTER writes the same files (bq extract -m). The formats:
//
//   - TF SavedModel: AUTOENCODER, DNN_*, DNN_LINEAR_COMBINED_*, KMEANS,
//     LINEAR_REG, LOGISTIC_REG, MATRIX_FACTORIZATION, PCA (TRANSFORM_ONLY
//     too, below): saved_model.pb, variables/ and assets/, which the
//     converter image writes with TensorFlow (tfconvert.Exporter,
//     deploy/tfconvert/convert.py's export) from the model's
//     preprocessing and weights (exportSpec);
//   - XGBoost Booster: BOOSTED_TREE_* and RANDOM_FOREST_*
//     (modelexportxgb.go);
//   - an imported TENSORFLOW model: "the same files that were present
//     when the model was imported".
//
// "Export isn't supported if the input data used ARRAY, TIMESTAMP, or
// GEOGRAPHY feature types", and "the BigQuery dataset and the Cloud
// Storage bucket must be in the same location".
//
// UNVERIFIED (re-read 2026-11-24, neither page says): BigQuery's messages for a model type it
// does not export, a feature type it does not export, a destination format
// the model has no export in, a bucket of another location and TRIAL_ID of
// a model with no trials; the job's statistics beyond statementType
// EXPORT_MODEL; and the serving_default signature's output names beyond
// those Google's codelab shows for a logistic regression
// (predicted_<label>, <label>_probs, <label>_values): CloudBurrow names
// them as ML.PREDICT names its columns (centroid_id and
// centroid_distances, principal_component_<n>, latent_col_<n>,
// predicted_<rating>). GCPC's BigqueryExportModelJobOp adds
// add_serving_default_signature=True, which the export always has.
//
// A model with a TRANSFORM clause also has its preprocessing SavedModel in
// transform/, and a TRANSFORM_ONLY model is that SavedModel alone
// (modelexporttransform.go). Not exported (501, naming why): AutoML
// models, which BigQuery exports in AutoML's own TF 2.1.0 format.

// exportDDL is an EXPORT MODEL statement.
type exportDDL struct {
	path    []string
	uri     string
	trial   int64
	trialOK bool
}

// parseExportModel reads stmt as EXPORT MODEL; isExport is false for any
// other statement; code and msg are a refusal.
func parseExportModel(stmt []token) (d exportDDL, isExport bool, code int, msg string) {
	if len(stmt) < 2 || !stmt[0].is("EXPORT") || !stmt[1].is("MODEL") {
		return d, false, 0, ""
	}
	path, n := readModelPath(stmt[2:])
	if n == 0 {
		return d, true, 400, "Syntax error: expected a model name"
	}
	d.path = path
	i := 2 + n
	if i < len(stmt) {
		if !stmt[i].is("OPTIONS") || i+1 >= len(stmt) || !stmt[i+1].punct("(") {
			return d, true, 400, "Syntax error: Unexpected " + stmt[i].text
		}
		end := matchParen(stmt, i+1)
		if end < 0 || end != len(stmt)-1 {
			return d, true, 400, "Syntax error: Expected \")\""
		}
		toks := stmt[i+2 : end]
		for j := 0; j < len(toks); {
			if toks[j].kind != tokWord || j+2 >= len(toks) || !toks[j+1].punct("=") {
				if toks[j].punct(",") && j == len(toks)-1 {
					break // a trailing comma, as GCPC writes
				}
				return d, true, 400, "Syntax error in OPTIONS"
			}
			name, val := strings.ToLower(toks[j].text), toks[j+2]
			switch name {
			case "uri":
				s, ok := unquoteString(val.text)
				if val.kind != tokString || !ok {
					return d, true, 400, "Invalid value for option URI: a string is expected"
				}
				d.uri = s
			case "trial_id":
				v, err := strconv.ParseInt(val.text, 10, 64)
				if err != nil {
					return d, true, 400, "Invalid value for option TRIAL_ID: an integer is expected"
				}
				d.trial, d.trialOK = v, true
			case "add_serving_default_signature":
				// GCPC's BigqueryExportModelJobOp sends True; the export
				// always has a serving_default signature.
				if !val.is("TRUE") && !val.is("FALSE") {
					return d, true, 400, "Invalid value for option add_serving_default_signature: a BOOL is expected"
				}
			default:
				return d, true, 400, "Unknown option: " + toks[j].text
			}
			j += 3
			if j < len(toks) {
				if !toks[j].punct(",") {
					return d, true, 400, "Syntax error in OPTIONS"
				}
				j++
			}
		}
	}
	if d.uri == "" {
		return d, true, 400, "URI option is required for the EXPORT MODEL statement"
	}
	return d, true, 0, ""
}

// exportFile is one file of a model's export, its name relative to the
// export's URI.
type exportFile struct {
	name, contentType string
	data              []byte
}

// tfExporter is the converter's export (tfconvert.Client).
type tfExporter interface {
	Export(ctx context.Context, spec []byte) ([]tfconvert.File, error)
}

// exportFormatOf is the format BigQuery exports a model of type typ in,
// or "" for a type it does not export.
func exportFormatOf(typ string) string {
	switch {
	case isTreeModel(typ):
		return "ML_XGBOOST_BOOSTER"
	case typ == "LINEAR_REG", typ == "LOGISTIC_REG", typ == "KMEANS", typ == "PCA", typ == "MATRIX_FACTORIZATION", typ == "AUTOENCODER",
		typ == "TRANSFORM_ONLY", typ == "TENSORFLOW", typ == "AUTOML_CLASSIFIER", typ == "AUTOML_REGRESSOR", isNetwork(typ):
		return "ML_TF_SAVED_MODEL"
	}
	return ""
}

// exportModel makes the files of md's export in format ("" the model's
// own), or a refusal.
func (f front) exportModel(ctx context.Context, md bqModel, format string, trial int64, trialOK bool) ([]exportFile, int, string) {
	name := fmt.Sprintf("%s:%s.%s", md.Project, md.Dataset, md.ID)
	typ := md.Type
	if md.Remote != nil {
		typ = "REMOTE"
	}
	own := exportFormatOf(typ)
	if own == "" {
		return nil, 400, fmt.Sprintf("Exporting model %s of type %s is not supported. EXPORT MODEL supports the model types the export models page lists.", name, typ)
	}
	if trialOK {
		return nil, 400, fmt.Sprintf("TRIAL_ID is only supported for a model trained with hyperparameter tuning; model %s has no trials", name)
	}
	if format == "" {
		format = own
	}
	if format != own {
		return nil, 400, fmt.Sprintf("Model %s of type %s cannot be exported in destination format %s; its format is %s", name, typ, format, own)
	}
	if strings.HasPrefix(typ, "AUTOML_") {
		return nil, 501, "Not implemented here: EXPORT MODEL of an AutoML model, which BigQuery exports in AutoML's own TF SavedModel (TF 2.1.0). Nothing was written."
	}
	if len(md.Transform) == 0 && typ == "TRANSFORM_ONLY" {
		return nil, 500, "cloudburrow: the TRANSFORM_ONLY model keeps no TRANSFORM"
	}
	// The preprocessing SavedModel of the TRANSFORM (modelexporttransform.go):
	// the model's export itself for TRANSFORM_ONLY, its transform/ folder for
	// any other model.
	var pre map[string]any
	if len(md.Transform) > 0 {
		spec, code, msg := transformSpec(md)
		if code != 0 {
			return nil, code, msg
		}
		if typ == "TRANSFORM_ONLY" {
			return f.tfExport(ctx, spec)
		}
		pre = spec
	}
	files, code, msg := f.exportTrained(ctx, md, typ, name)
	if code != 0 || pre == nil {
		return files, code, msg
	}
	tfiles, code, msg := f.tfExport(ctx, pre)
	if code != 0 {
		return nil, code, msg
	}
	for _, fl := range tfiles {
		fl.name = "transform/" + fl.name
		files = append(files, fl)
	}
	return files, 0, ""
}

// exportTrained makes the files of the export of md, a trained model of
// type typ, without its TRANSFORM.
func (f front) exportTrained(ctx context.Context, md bqModel, typ, name string) ([]exportFile, int, string) {
	for _, ft := range md.Trained.Features {
		switch ft.Type {
		case "INT64", "FLOAT64", "NUMERIC", "BIGNUMERIC", "STRING", "BOOL":
		default:
			return nil, 400, fmt.Sprintf("Model %s cannot be exported: its feature %s is of type %s; export is not supported for ARRAY, TIMESTAMP or GEOGRAPHY features", name, ft.Name, ft.Type)
		}
	}
	if isTreeModel(typ) {
		return xgbFiles(md), 0, ""
	}
	if typ == "TENSORFLOW" {
		return f.importedSavedModel(ctx, md)
	}
	spec, code, msg := exportSpec(md)
	if code != 0 {
		return nil, code, msg
	}
	return f.tfExport(ctx, spec)
}

// tfExport runs the converter's export of spec: the SavedModel's files.
func (f front) tfExport(ctx context.Context, spec map[string]any) ([]exportFile, int, string) {
	ex, ok := f.tfconvert.(tfExporter)
	if f.tfconvert == nil || !ok {
		return nil, 501, "Not implemented here: EXPORT MODEL to a TF SavedModel: this BigQuery front was given no TensorFlow converter (--tfconvert), which `cloudburrow up` serves with BigQuery enabled. Nothing was written."
	}
	b, _ := json.Marshal(spec)
	got, err := ex.Export(ctx, b)
	if err != nil {
		var rf *tfconvert.Refusal
		if errors.As(err, &rf) && rf.Kind == "unavailable" {
			return nil, 503, "The TensorFlow exporter is unavailable: " + rf.Message
		}
		return nil, 500, "cloudburrow: the TensorFlow exporter failed: " + err.Error()
	}
	var files []exportFile
	for _, g := range got {
		files = append(files, exportFile{name: g.Name, contentType: "application/octet-stream", data: g.Data})
	}
	return files, 0, ""
}

// importedSavedModel are an imported TENSORFLOW model's files, read again
// from its model_path: "the same files that were present when the model
// was imported".
func (f front) importedSavedModel(ctx context.Context, md bqModel) ([]exportFile, int, string) {
	if md.TFLite == nil || md.TFLite.ModelPath == "" {
		return nil, 500, "cloudburrow: the TENSORFLOW model keeps no model_path"
	}
	_, files, code, msg := f.readSavedModel(ctx, md.TFLite.ModelPath)
	if code != 0 {
		return nil, code, msg
	}
	var size int64
	for _, fl := range files {
		size += int64(len(fl.Data))
	}
	if md.TFLite.Size != 0 && size != md.TFLite.Size {
		return nil, 400, fmt.Sprintf("Model %s:%s.%s cannot be exported: its SavedModel at %s changed since it was imported", md.Project, md.Dataset, md.ID, md.TFLite.ModelPath)
	}
	var out []exportFile
	for _, fl := range files {
		out = append(out, exportFile{name: fl.Name, contentType: "application/octet-stream", data: fl.Data})
	}
	return out, 0, ""
}

// exportDtype is the TensorFlow dtype a feature of BigQuery type t is
// given to the SavedModel as.
func exportDtype(t string) string {
	switch t {
	case "INT64":
		return "int64"
	case "STRING":
		return "string"
	case "BOOL":
		return "bool"
	}
	return "float64"
}

// exportSpec is the converter's export spec of md (convert.py's export).
func exportSpec(md bqModel) (map[string]any, int, string) {
	t := md.Trained
	spec := map[string]any{"label": t.Label, "labelDtype": exportDtype(t.LabelType)}
	var inputs []map[string]any
	for _, ft := range t.Features {
		inputs = append(inputs, map[string]any{"name": ft.Name, "dtype": exportDtype(ft.Type)})
	}
	spec["features"] = exportEncoders(t)
	head := map[string]any{}
	task := func() {
		switch {
		case len(t.Classes) == 2:
			head["task"], head["classes"] = "binary", []string{t.Classes[1], t.Classes[0]}
		case len(t.Classes) > 2:
			head["task"], head["classes"] = "multiclass", t.Classes
		default:
			head["task"] = "regression"
		}
	}
	dnn := func() {
		var layers []map[string]any
		for _, ly := range t.DNN.Layers {
			layers = append(layers, map[string]any{"w": ly.W, "b": ly.B})
		}
		head["layers"], head["activation"] = layers, t.DNN.Activation
		if t.DNN.Wide != nil {
			head["wide"] = map[string]any{"w": t.DNN.Wide.W, "b": t.DNN.Wide.B}
		}
	}
	switch typ := md.Type; {
	case typ == "LINEAR_REG", typ == "LOGISTIC_REG":
		head["kind"] = "linear"
		if t.multiclass() {
			head["w"] = t.ClassWeights
		} else {
			head["w"] = [][]float64{t.weightVector()}
		}
		task()
	case isDNN(typ):
		if t.DNN == nil {
			return nil, 500, "cloudburrow: the model has no network"
		}
		head["kind"] = "dnn"
		dnn()
		task()
	case typ == "AUTOENCODER":
		if t.DNN == nil {
			return nil, 500, "cloudburrow: the autoencoder has no network"
		}
		head["kind"], head["latent"] = "autoencoder", t.DNN.latentLayer()
		dnn()
	case typ == "KMEANS":
		head["kind"], head["centroids"], head["distance"] = "kmeans", t.Centroids, t.Distance
	case typ == "PCA":
		head["kind"], head["components"] = "pca", t.Components
	case typ == "MATRIX_FACTORIZATION":
		m := t.MF
		if m == nil {
			return nil, 500, "cloudburrow: the model has no factors"
		}
		out := "predicted_" + t.Label
		if m.Implicit {
			out += "_confidence"
		}
		head["kind"], head["users"], head["items"], head["output"] = "mf", nonNil(m.Users), nonNil(m.Items), out
		head["userF"], head["itemF"], head["userB"], head["itemB"] = m.UserF, m.ItemF, m.UserB, m.ItemB
		inputs = []map[string]any{{"name": m.UserCol, "dtype": exportDtype(m.UserType)}, {"name": m.ItemCol, "dtype": exportDtype(m.ItemType)}}
		spec["features"] = []map[string]any{}
	default:
		return nil, 400, "Exporting a model of type " + typ + " is not supported"
	}
	spec["inputs"], spec["head"] = inputs, head
	return spec, 0, ""
}

// exportTo writes files under uri, a folder: gs://bucket/path/to/dir/.
func (f front) exportTo(ctx context.Context, uri string, files []exportFile) error {
	base := strings.TrimRight(uri, "/") + "/"
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	for _, fl := range files {
		if err := f.upload(ctx, base+fl.name, fl.contentType, fl.data); err != nil {
			return fmt.Errorf("could not write %s: %v", base+fl.name, err)
		}
	}
	return nil
}

// exportDestination checks an export's URI: a gs:// URI of a bucket that
// is there, in the dataset's location loc.
func (f front) exportDestination(ctx context.Context, uri, loc string) (int, string) {
	if !strings.HasPrefix(uri, "gs://") || strings.Contains(uri, "*") {
		return 400, "Invalid URI " + uri + ": the model is exported to a Cloud Storage folder, gs://bucket/path"
	}
	bucket, _, _ := strings.Cut(strings.TrimPrefix(uri, "gs://"), "/")
	if bucket == "" {
		return 400, "Invalid URI " + uri + ": it names no bucket"
	}
	if f.storage == nil || f.storageHost == "" {
		return 501, "Not implemented here: EXPORT MODEL: this front has no Cloud Storage to write it to. Nothing was written."
	}
	var b struct {
		Location string `json:"location"`
	}
	st, err := f.storage.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket), &b)
	if err != nil {
		return 404, f.storage.unreachable(uri, err).Error()
	}
	if st == http.StatusNotFound {
		return 404, "Not found: Bucket " + bucket
	}
	if st != http.StatusOK {
		return 400, fmt.Sprintf("Cloud Storage answered HTTP %d for bucket %s", st, bucket)
	}
	if loc != "" && b.Location != "" && !strings.EqualFold(loc, b.Location) {
		return 400, fmt.Sprintf("Cannot export model to bucket %s in location %s: the dataset is in location %s, and the BigQuery dataset and the Cloud Storage bucket must be in the same location",
			bucket, strings.ToUpper(b.Location), loc)
	}
	return 0, ""
}

// exportStatement carries out EXPORT MODEL d, failing with fail.
func (f front) exportStatement(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool, d exportDDL, fail func(int, string)) {
	project, dataset, id, ok := f.resolveModel(d.path, q)
	if !ok {
		fail(400, "Model name "+strings.Join(d.path, ".")+" missing dataset while no default dataset is set in the request.")
		return
	}
	if project != projectOf(f.base) {
		fail(501, "Not implemented here: a model of project "+project+", which is not this instance's project. Nothing was run.")
		return
	}
	loc, exists := f.modelDatasetLocation(r, dataset)
	if !exists {
		fail(404, fmt.Sprintf("Not found: Dataset %s:%s", project, dataset))
		return
	}
	md, had := f.models.get(project, dataset, id)
	if !had {
		fail(404, fmt.Sprintf("Not found: Model %s:%s.%s", project, dataset, id))
		return
	}
	if code, msg := f.exportDestination(r.Context(), d.uri, loc); code != 0 {
		fail(code, msg)
		return
	}
	if !dryRun(r, insert) {
		files, code, msg := f.exportModel(r.Context(), md, "", d.trial, d.trialOK)
		if code != 0 {
			fail(code, msg)
			return
		}
		if err := f.exportTo(r.Context(), d.uri, files); err != nil {
			fail(500, err.Error())
			return
		}
	}
	noop := "DROP TABLE IF EXISTS " + quotePath([]string{"_cloudburrow", scratchTable()})
	if !setQueryText(r, insert, noop) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return
	}
	client := jobText{query: q.Query, ddl: &ownDDL{statementType: "EXPORT_MODEL"}}
	q.Query = noop
	rec := newRecorder()
	f.serveQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
}

// modelExtractJob is an extract job of a model (bq extract -m): its
// sourceModel's files to its one destination URI, in destinationFormat.
func (f front) modelExtractJob(w http.ResponseWriter, r *http.Request, e *extractConfig, uris []string) {
	var src struct {
		ProjectID string `json:"projectId"`
		DatasetID string `json:"datasetId"`
		ModelID   string `json:"modelId"`
	}
	if json.Unmarshal(e.SourceModel, &src) != nil || src.DatasetID == "" || src.ModelID == "" {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid sourceModel: it needs a datasetId and a modelId")
		return
	}
	project := projectOf(f.base)
	if src.ProjectID != "" && src.ProjectID != project {
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: an extract job of a model of project "+src.ProjectID+", which is not this instance's project. Nothing was written.")
		return
	}
	format := strings.ToUpper(e.DestinationFormat)
	switch format {
	case "", "ML_TF_SAVED_MODEL", "ML_XGBOOST_BOOSTER":
	default:
		writeError(w, http.StatusBadRequest, "invalid", "Invalid destinationFormat "+e.DestinationFormat+" for a model: ML_TF_SAVED_MODEL or ML_XGBOOST_BOOSTER")
		return
	}
	if format == "" {
		// The extract job's default format for a model.
		format = "ML_TF_SAVED_MODEL"
	}
	if len(uris) != 1 {
		writeError(w, http.StatusBadRequest, "invalid", "An extract job of a model has exactly one destination URI")
		return
	}
	loc, exists := f.modelDatasetLocation(r, src.DatasetID)
	if !exists {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Dataset %s:%s", project, src.DatasetID))
		return
	}
	md, had := f.models.get(project, src.DatasetID, src.ModelID)
	if !had {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Model %s:%s.%s", project, src.DatasetID, src.ModelID))
		return
	}
	if code, msg := f.exportDestination(r.Context(), uris[0], loc); code != 0 {
		writeError(w, code, map[int]string{400: "invalid", 404: "notFound", 501: "notImplemented"}[code], msg)
		return
	}
	j, ok := f.startOwnJob(w, r, "an extract job of a model")
	if !ok {
		return
	}
	var failure *rowError
	files, code, msg := f.exportModel(r.Context(), md, format, 0, false)
	if code == 501 || code == 503 {
		writeError(w, code, map[int]string{501: "notImplemented", 503: "backendError"}[code], msg)
		return
	}
	if code != 0 {
		failure = &rowError{Reason: map[int]string{400: "invalid", 404: "notFound"}[code], Message: msg}
		if failure.Reason == "" {
			failure.Reason = "backendError"
		}
	} else if err := f.exportTo(r.Context(), uris[0], files); err != nil {
		failure = &rowError{Reason: "backendError", Message: err.Error()}
	}
	f.finishOwnJob(w, r, j, "EXTRACT", "extract", map[string]any{"destinationUriFileCounts": []string{strconv.Itoa(len(files))}}, failure)
}
