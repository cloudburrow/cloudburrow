package bigqueryfront

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BigQuery ML models (#1465). The engine refuses CREATE MODEL ("Statement
// not supported: CreateModelStatement", measured), so the front carries
// out CREATE [OR REPLACE] MODEL [IF NOT EXISTS] and DROP MODEL [IF EXISTS]
// itself, keeps the models (models.json in the state directory), serves
// the models.* REST API from them (get, list, patch, delete), and answers
// ML.PREDICT, ML.EVALUATE, ML.WEIGHTS, ML.FEATURE_INFO and
// ML.TRAINING_INFO (modelfuncs.go). Training is modeltrain.go's.
//
// Served model types: LINEAR_REG, LOGISTIC_REG, KMEANS and PCA (#1481),
// explicit MATRIX_FACTORIZATION (modelmf.go), BOOSTED_TREE_* (modeltree.go)
// and ARIMA_PLUS (modelarima.go) (#1504). Refused 501 with the reason: the
// other types (their training is Google's, and either undocumented in the
// detail a faithful result needs, as for DNNs, or Google's
// infrastructure itself: AUTOML_*, imported TENSORFLOW/ONNX/XGBOOST models
// read from Cloud Storage, and remote models over a Cloud resource
// connection, which CloudBurrow has none of); a TRANSFORM clause; and the
// options CloudBurrow does not carry out.

// bqModel is a model the front keeps.
type bqModel struct {
	Project      string            `json:"project"`
	Dataset      string            `json:"dataset"`
	ID           string            `json:"id"`
	Type         string            `json:"modelType"`
	Created      int64             `json:"creationTime"`
	Modified     int64             `json:"lastModifiedTime"`
	Expiration   int64             `json:"expirationTime,omitempty"`
	Description  string            `json:"description,omitempty"`
	FriendlyName string            `json:"friendlyName,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Location     string            `json:"location,omitempty"`
	Options      mlOptions         `json:"options"`
	Trained      mlTrained         `json:"trained"`
	Query        string            `json:"query,omitempty"`
	// Transform is the TRANSFORM clause's items, * expanded
	// (modeltransform.go).
	Transform []string `json:"transform,omitempty"`
}

func modelKey(project, dataset, id string) string { return project + "\x00" + dataset + "\x00" + id }

// bqModels are the models of every dataset.
type bqModels struct {
	mu     sync.Mutex
	models map[string]*bqModel
	path   string
	logf   func(string, ...any)
	// now is the instance's clock (#1487); nil is the system's.
	now func() time.Time
}

// clock is the time now by m.now.
func (m *bqModels) clock() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

func modelsStateFile(dir string) string { return filepath.Join(dir, "models.json") }

// keep restores the models from path, and from then on keeps them there.
func (m *bqModels) keep(path string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	b, err := os.ReadFile(path)
	var saved []bqModel
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logf("bigquery front: read %s: %v; starting without the models it kept", path, err)
	case json.Unmarshal(b, &saved) != nil:
		logf("bigquery front: %s is unreadable; starting without the models it kept", path)
		saved = nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range saved {
		if m.models == nil {
			m.models = map[string]*bqModel{}
		}
		md := saved[i]
		m.models[modelKey(md.Project, md.Dataset, md.ID)] = &md
	}
	m.path, m.logf = path, logf
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		logf("bigquery front: %v", err)
	}
	m.saveLocked()
}

func (m *bqModels) saveLocked() {
	if m.path == "" {
		return
	}
	saved := make([]bqModel, 0, len(m.models))
	for _, md := range m.models {
		saved = append(saved, *md)
	}
	sort.Slice(saved, func(a, b int) bool {
		return modelKey(saved[a].Project, saved[a].Dataset, saved[a].ID) < modelKey(saved[b].Project, saved[b].Dataset, saved[b].ID)
	})
	b, err := json.Marshal(saved)
	if err == nil {
		err = os.WriteFile(m.path, b, 0o600)
	}
	if err != nil {
		m.logf("bigquery front: keep the models: %v", err)
	}
}

func (m *bqModels) reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = nil
	m.saveLocked()
}

// get returns a copy of a model, dropping it first if it has expired.
func (m *bqModels) get(project, dataset, id string) (bqModel, bool) {
	if m == nil {
		return bqModel{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	md, ok := m.models[modelKey(project, dataset, id)]
	if !ok {
		return bqModel{}, false
	}
	if md.Expiration > 0 && md.Expiration <= m.clock().UnixMilli() {
		delete(m.models, modelKey(project, dataset, id))
		m.saveLocked()
		return bqModel{}, false
	}
	return *md, true
}

func (m *bqModels) put(md bqModel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.models == nil {
		m.models = map[string]*bqModel{}
	}
	m.models[modelKey(md.Project, md.Dataset, md.ID)] = &md
	m.saveLocked()
}

func (m *bqModels) remove(project, dataset, id string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := modelKey(project, dataset, id)
	if _, ok := m.models[k]; !ok {
		return false
	}
	delete(m.models, k)
	m.saveLocked()
	return true
}

// list returns a dataset's live models, sorted by ID.
func (m *bqModels) list(project, dataset string) []bqModel {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock().UnixMilli()
	var out []bqModel
	for k, md := range m.models {
		if md.Project != project || md.Dataset != dataset {
			continue
		}
		if md.Expiration > 0 && md.Expiration <= now {
			delete(m.models, k)
			continue
		}
		out = append(out, *md)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// dropDataset removes a dataset's models.
func (m *bqModels) dropDataset(project, dataset string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, md := range m.models {
		if md.Project == project && md.Dataset == dataset {
			delete(m.models, k)
		}
	}
	m.saveLocked()
}

// etag is the model's etag, a hash of what a patch changes.
func (md bqModel) etag() string {
	b, _ := json.Marshal([]any{md.Modified, md.Description, md.FriendlyName, md.Labels, md.Expiration})
	h := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(h[:12])
}

// bqTypeName is the REST typeKind of a model column type.
func bqTypeName(t string) string {
	switch t {
	case "INT64":
		return "INT64"
	case "BOOL":
		return "BOOL"
	}
	return t
}

// resource is the model's REST resource
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/models#Model).
func (md bqModel) resource(full bool) map[string]any {
	ms := func(v int64) string { return strconv.FormatInt(v, 10) }
	res := map[string]any{
		"etag":             md.etag(),
		"modelReference":   map[string]string{"projectId": md.Project, "datasetId": md.Dataset, "modelId": md.ID},
		"creationTime":     ms(md.Created),
		"lastModifiedTime": ms(md.Modified),
		"modelType": map[string]string{"LINEAR_REG": "LINEAR_REGRESSION", "LOGISTIC_REG": "LOGISTIC_REGRESSION", "KMEANS": "KMEANS", "PCA": "PCA",
			"MATRIX_FACTORIZATION": "MATRIX_FACTORIZATION", "BOOSTED_TREE_REGRESSOR": "BOOSTED_TREE_REGRESSOR",
			"BOOSTED_TREE_CLASSIFIER": "BOOSTED_TREE_CLASSIFIER", "ARIMA_PLUS": "ARIMA_PLUS",
			"RANDOM_FOREST_REGRESSOR": "RANDOM_FOREST_REGRESSOR", "RANDOM_FOREST_CLASSIFIER": "RANDOM_FOREST_CLASSIFIER",
			"DNN_REGRESSOR": "DNN_REGRESSOR", "DNN_CLASSIFIER": "DNN_CLASSIFIER", "AUTOENCODER": "AUTOENCODER"}[md.Options.ModelType],
	}
	if md.Location != "" {
		res["location"] = md.Location
	}
	if len(md.Labels) > 0 {
		res["labels"] = md.Labels
	}
	if md.Description != "" {
		res["description"] = md.Description
	}
	if md.FriendlyName != "" {
		res["friendlyName"] = md.FriendlyName
	}
	if md.Expiration > 0 {
		res["expirationTime"] = ms(md.Expiration)
	}
	if !full {
		return res
	}
	var feats []map[string]any
	for _, ft := range md.Trained.Features {
		feats = append(feats, map[string]any{"name": ft.Name, "type": map[string]string{"typeKind": bqTypeName(ft.Type)}})
	}
	res["featureColumns"] = feats
	o := md.Options
	t := md.Trained
	var opts, metrics map[string]any
	var results []map[string]any
	for _, it := range t.Iterations {
		r := map[string]any{"index": it.Index, "durationMs": ms(it.DurationMs), "trainingLoss": it.Loss}
		if o.ModelType != "KMEANS" && o.ModelType != "MATRIX_FACTORIZATION" {
			r["learnRate"] = it.LearnRate
		}
		if it.EvalLoss != nil {
			r["evalLoss"] = *it.EvalLoss
		}
		results = append(results, r)
	}
	num := func(k string) any {
		if v := t.Eval[k]; v != nil {
			return *v
		}
		return nil
	}
	switch o.ModelType {
	case "ARIMA_PLUS":
		var info []map[string]any
		opts, info = md.arimaResource()
		// The model's seasonal periods: every series' (#1518).
		var periods []string
		seen := map[string]bool{}
		for _, m := range info {
			for _, p := range m["seasonalPeriods"].([]string) {
				if !seen[p] && p != "NO_SEASONALITY" {
					seen[p] = true
					periods = append(periods, p)
				}
			}
		}
		if len(periods) == 0 {
			periods = []string{"NO_SEASONALITY"}
		}
		results = []map[string]any{{"index": 0, "arimaResult": map[string]any{"arimaModelInfo": info, "seasonalPeriods": periods}}}
		var single []map[string]any
		for _, m := range info {
			sm := map[string]any{"nonSeasonalOrder": m["nonSeasonalOrder"], "arimaFittingMetrics": m["arimaFittingMetrics"], "hasDrift": m["hasDrift"],
				"seasonalPeriods": m["seasonalPeriods"], "hasHolidayEffect": false}
			if id, ok := m["timeSeriesId"]; ok {
				sm["timeSeriesId"] = id
			}
			single = append(single, sm)
		}
		metrics = map[string]any{"arimaForecastingMetrics": map[string]any{"arimaSingleModelForecastingMetrics": single}}
	case "MATRIX_FACTORIZATION":
		opts, metrics = md.mfResource(res)
	case "AUTOENCODER":
		opts, metrics = md.autoencoderResource(res)
	case "KMEANS":
		res["labelColumns"] = []map[string]any{{"name": "centroid_id", "type": map[string]string{"typeKind": "INT64"}}}
		opts = map[string]any{"numClusters": ms(int64(len(t.Centroids))), "kmeansInitializationMethod": o.KMeansInit,
			"distanceType": o.DistanceType, "standardizeFeatures": o.Standardize, "maxIterations": ms(int64(o.MaxIterations)),
			"earlyStop": o.EarlyStop, "minRelativeProgress": o.MinRelProgress}
		if o.KMeansInitCol != "" {
			opts["kmeansInitializationColumn"] = o.KMeansInitCol
		}
		var clusters []map[string]any
		for k, cen := range t.Centroids {
			var fvs []map[string]any
			j := 0
			for _, ft := range t.Features {
				if ft.numeric() {
					v := cen[j]
					j++
					if t.Scaling == "" {
						v = v*ft.Stddev + ft.Mean
					}
					fvs = append(fvs, map[string]any{"featureColumn": ft.Name, "numericalValue": v})
					continue
				}
				var cc []map[string]any
				size := 0
				if k < len(t.ClusterSizes) {
					size = t.ClusterSizes[k]
				}
				for _, cat := range ft.Categories {
					cc = append(cc, map[string]any{"category": cat, "count": ms(int64(math.Round(cen[j] * float64(size))))})
					j++
				}
				if ft.NullSeen {
					j++
				}
				fvs = append(fvs, map[string]any{"featureColumn": ft.Name, "categoricalValue": map[string]any{"categoryCounts": cc}})
			}
			cl := map[string]any{"centroidId": ms(int64(k + 1)), "featureValues": fvs}
			if k < len(t.ClusterSizes) {
				cl["count"] = ms(int64(t.ClusterSizes[k]))
			}
			clusters = append(clusters, cl)
		}
		metrics = map[string]any{"clusteringMetrics": map[string]any{"daviesBouldinIndex": num("davies_bouldin_index"),
			"meanSquaredDistance": num("mean_squared_distance"), "clusters": clusters}}
	case "PCA":
		var lc []map[string]any
		for k := range t.Components {
			lc = append(lc, map[string]any{"name": fmt.Sprintf("principal_component_%d", k+1), "type": map[string]string{"typeKind": "FLOAT64"}})
		}
		res["labelColumns"] = lc
		opts = map[string]any{"scaleFeatures": o.ScaleFeatures, "pcaSolver": o.PCASolver}
		if o.NumPC > 0 {
			opts["numPrincipalComponents"] = ms(int64(o.NumPC))
		} else {
			opts["pcaExplainedVarianceRatio"] = o.PCAVarRatio
		}
		metrics = map[string]any{"dimensionalityReductionMetrics": map[string]any{"totalExplainedVarianceRatio": num("total_explained_variance_ratio")}}
	default:
		res["labelColumns"] = []map[string]any{{"name": "predicted_" + t.Label, "type": map[string]string{"typeKind": bqTypeName(t.LabelType)}}}
		opts = map[string]any{
			"maxIterations":         ms(int64(o.MaxIterations)),
			"l1Regularization":      o.L1,
			"l2Regularization":      o.L2,
			"minRelativeProgress":   o.MinRelProgress,
			"earlyStop":             o.EarlyStop,
			"inputLabelColumns":     []string{t.Label},
			"dataSplitMethod":       o.SplitMethod,
			"dataSplitEvalFraction": o.SplitEvalFrac,
			"learnRateStrategy":     o.LearnRateStrat,
			"optimizationStrategy":  t.Strategy,
		}
		switch {
		case isTreeModel(o.ModelType):
			for _, k := range []string{"learnRateStrategy", "optimizationStrategy", "l1Regularization"} {
				delete(opts, k)
			}
			opts["learnRate"], opts["l1Regularization"], opts["maxTreeDepth"] = o.LearnRate, o.L1, ms(int64(o.MaxTreeDepth))
			opts["minSplitLoss"], opts["minTreeChildWeight"], opts["boosterType"], opts["treeMethod"] = o.MinSplitLoss, ms(int64(o.MinChildWeight)), o.BoosterType, o.TreeMethod
			opts["subsample"], opts["numParallelTree"] = o.Subsample, ms(int64(o.NumParallelTree))
			opts["colsampleBytree"], opts["colsampleBylevel"], opts["colsampleBynode"] = o.ColsampleTree, o.ColsampleLevel, o.ColsampleNode
			if isRandomForest(o.ModelType) {
				delete(opts, "learnRate")
				delete(opts, "boosterType")
				delete(opts, "maxIterations")
			}
		case isDNN(o.ModelType):
			for _, k := range []string{"learnRateStrategy", "optimizationStrategy"} {
				delete(opts, k)
			}
			var hidden []string
			if t.DNN != nil {
				for _, ly := range t.DNN.Layers[:len(t.DNN.Layers)-1] {
					hidden = append(hidden, ms(int64(len(ly.B))))
				}
			}
			batch := o.BatchSize
			if batch == 0 {
				batch = min(32, t.Rows)
			}
			opts["learnRate"], opts["optimizer"], opts["activationFn"], opts["hiddenUnits"] = o.LearnRate, o.Optimizer, o.Activation, hidden
			opts["batchSize"], opts["dropout"] = ms(int64(batch)), o.Dropout
		case o.LearnRateStrat == "CONSTANT":
			opts["learnRate"] = o.LearnRate
		default:
			opts["initialLearnRate"] = o.InitLearnRate
		}
		if o.SplitCol != "" {
			opts["dataSplitColumn"] = o.SplitCol
		}
		if isClassifierType(o.ModelType) {
			opts["lossType"] = "MEAN_LOG_LOSS"
		} else {
			opts["lossType"] = "MEAN_SQUARED_LOSS"
		}
		switch {
		case t.multiclass():
			metrics = map[string]any{"multiClassClassificationMetrics": map[string]any{
				"aggregateClassificationMetrics": map[string]any{"precision": num("precision"), "recall": num("recall"),
					"accuracy": num("accuracy"), "f1Score": num("f1_score"), "logLoss": num("log_loss"), "rocAuc": num("roc_auc")}}}
		case isClassifierType(o.ModelType):
			metrics = map[string]any{"binaryClassificationMetrics": map[string]any{
				"aggregateClassificationMetrics": map[string]any{"precision": num("precision"), "recall": num("recall"),
					"accuracy": num("accuracy"), "threshold": 0.5, "f1Score": num("f1_score"), "logLoss": num("log_loss"), "rocAuc": num("roc_auc")},
				"negativeLabel": t.Classes[0], "positiveLabel": t.Classes[1]}}
		default:
			metrics = map[string]any{"regressionMetrics": map[string]any{"meanAbsoluteError": num("mean_absolute_error"),
				"meanSquaredError": num("mean_squared_error"), "meanSquaredLogError": num("mean_squared_log_error"),
				"medianAbsoluteError": num("median_absolute_error"), "rSquared": num("r2_score")}}
		}
	}
	if len(md.Transform) > 0 {
		// The TRANSFORM's output features (UNVERIFIED: BigQuery's
		// featureColumns are then the input's, and it gives transformSql).
		var tc []map[string]any
		for _, ft := range t.Features {
			tc = append(tc, map[string]any{"name": ft.Name, "type": map[string]string{"typeKind": bqTypeName(ft.Type)}})
		}
		res["transformColumns"] = tc
	}
	res["trainingRuns"] = []map[string]any{{
		"trainingOptions":   opts,
		"startTime":         time.UnixMilli(md.Created).UTC().Format(time.RFC3339Nano),
		"results":           results,
		"evaluationMetrics": metrics,
	}}
	return res
}

var (
	modelsRoute = regexp.MustCompile(`^(/bigquery/v2)?/projects/([^/]+)/datasets/([^/]+)/models(?:/([^/]+))?$`)
)

// serveModels answers the models.* REST methods
// (https://cloud.google.com/bigquery/docs/reference/rest/v2/models), and
// reports whether r was one.
func (f front) serveModels(w http.ResponseWriter, r *http.Request) bool {
	mm := modelsRoute.FindStringSubmatch(r.URL.EscapedPath())
	if mm == nil || f.models == nil {
		return false
	}
	project, err0 := url.PathUnescape(mm[2])
	dataset, err1 := url.PathUnescape(mm[3])
	id, err2 := url.PathUnescape(mm[4])
	if err0 != nil || err1 != nil || err2 != nil {
		return false
	}
	f.base = mm[1] + "/projects/" + mm[2]
	notFound := func() {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Model %s:%s.%s", project, dataset, id))
	}
	if st, _ := f.get(r, "/datasets/"+url.PathEscape(dataset)); st == http.StatusNotFound {
		writeError(w, http.StatusNotFound, "notFound", fmt.Sprintf("Not found: Dataset %s:%s", project, dataset))
		return true
	}
	if id == "" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid", "models.insert does not exist: models are made by CREATE MODEL")
			return true
		}
		all := f.models.list(project, dataset)
		start := 0
		if t := r.URL.Query().Get("pageToken"); t != "" {
			n, err := strconv.Atoi(t)
			if err != nil || n < 0 || n > len(all) {
				writeError(w, http.StatusBadRequest, "invalid", "Invalid page token")
				return true
			}
			start = n
		}
		size := len(all) - start
		if v, err := strconv.Atoi(r.URL.Query().Get("maxResults")); err == nil && v > 0 && v < size {
			size = v
		}
		out := map[string]any{}
		var items []map[string]any
		for _, md := range all[start : start+size] {
			items = append(items, md.resource(false))
		}
		if len(items) > 0 {
			out["models"] = items
		}
		if start+size < len(all) {
			out["nextPageToken"] = strconv.Itoa(start + size)
		}
		writeJSON(w, http.StatusOK, out)
		return true
	}
	md, ok := f.models.get(project, dataset, id)
	if !ok {
		notFound()
		return true
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, md.resource(true))
	case http.MethodDelete:
		f.models.remove(project, dataset, id)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		if m := r.Header.Get("If-Match"); m != "" && m != "*" && m != md.etag() {
			writeError(w, http.StatusPreconditionFailed, "conditionNotMet", "Precondition check failed.")
			return true
		}
		b, err := readBody(r)
		var body map[string]json.RawMessage
		if err != nil || json.Unmarshal(b, &body) != nil {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid JSON payload received.")
			return true
		}
		for k, v := range body {
			null := string(v) == "null"
			switch k {
			case "description":
				md.Description = ""
				if !null && json.Unmarshal(v, &md.Description) != nil {
					writeError(w, http.StatusBadRequest, "invalid", "Invalid value for description")
					return true
				}
			case "friendlyName":
				md.FriendlyName = ""
				if !null && json.Unmarshal(v, &md.FriendlyName) != nil {
					writeError(w, http.StatusBadRequest, "invalid", "Invalid value for friendlyName")
					return true
				}
			case "expirationTime":
				md.Expiration = 0
				if !null {
					var s json.Number
					if json.Unmarshal(v, &s) != nil {
						var str string
						if json.Unmarshal(v, &str) != nil {
							writeError(w, http.StatusBadRequest, "invalid", "Invalid value for expirationTime")
							return true
						}
						s = json.Number(str)
					}
					n, err := s.Int64()
					if err != nil {
						writeError(w, http.StatusBadRequest, "invalid", "Invalid value for expirationTime")
						return true
					}
					md.Expiration = n
				}
			case "labels":
				var labels map[string]*string
				if !null && json.Unmarshal(v, &labels) != nil {
					writeError(w, http.StatusBadRequest, "invalid", "Invalid value for labels")
					return true
				}
				if null {
					md.Labels = nil
				}
				for lk, lv := range labels {
					if lv == nil {
						delete(md.Labels, lk)
						continue
					}
					if md.Labels == nil {
						md.Labels = map[string]string{}
					}
					md.Labels[lk] = *lv
				}
			case "modelReference", "etag", "creationTime", "lastModifiedTime", "modelType", "location", "featureColumns",
				"labelColumns", "trainingRuns":
				// Output only, or not changed by a patch.
			default:
				writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: models.patch of "+k+
					" (CloudBurrow patches description, friendlyName, labels and expirationTime)")
				return true
			}
		}
		md.Modified = f.now().UnixMilli()
		f.models.put(md)
		writeJSON(w, http.StatusOK, md.resource(true))
	case http.MethodPut:
		writeError(w, http.StatusNotImplemented, "notImplemented", "Not implemented here: models.update; use models.patch")
	default:
		writeError(w, http.StatusMethodNotAllowed, "invalid", "Method not allowed")
	}
	return true
}

// modelDDL is a CREATE MODEL or DROP MODEL statement.
type modelDDL struct {
	drop, replace, ifNotExists, ifExists bool
	path                                 []string
	opts                                 mlOptions
	query                                string
	transform                            []string
}

// unsupportedModelTypes are the model types CloudBurrow refuses, and
// why.
var unsupportedModelTypes = map[string]string{
	"DNN_LINEAR_COMBINED_REGRESSOR":  "its wide-and-deep network's split of the features between the linear and the deep part is Google's, undocumented in the detail a faithful result needs",
	"DNN_LINEAR_COMBINED_CLASSIFIER": "its wide-and-deep network's split of the features between the linear and the deep part is Google's, undocumented in the detail a faithful result needs",
	"AUTOML_REGRESSOR":               "AutoML is Google's service (Vertex AI)",
	"AUTOML_CLASSIFIER":              "AutoML is Google's service (Vertex AI)",
	"ARIMA_PLUS_XREG":                "its automatic model selection, holiday effects and decomposition are Google's and undocumented in the detail a faithful result needs",
	"TENSORFLOW":                     "an imported TensorFlow model runs on Google's infrastructure",
	"TENSORFLOW_LITE":                "an imported TensorFlow Lite model runs on Google's infrastructure",
	"ONNX":                           "an imported ONNX model runs on Google's infrastructure",
	"XGBOOST":                        "an imported XGBoost model runs on Google's infrastructure",
	"CONTRIBUTION_ANALYSIS":          "its analysis is Google's",
	"TRANSFORM_ONLY":                 "a model of a TRANSFORM alone is not carried out; CloudBurrow applies a TRANSFORM as part of a model it trains",
}

// parseModelDDL reads stmt as CREATE MODEL or DROP MODEL. isModel is
// false for any other statement; code and msg are a refusal.
func parseModelDDL(stmt []token, sql string) (d modelDDL, isModel bool, code int, msg string) {
	i := 0
	switch {
	case len(stmt) > 1 && stmt[0].is("DROP") && stmt[1].is("MODEL"):
		d.drop = true
		i = 2
		if i+1 < len(stmt) && stmt[i].is("IF") && stmt[i+1].is("EXISTS") {
			d.ifExists = true
			i += 2
		}
	case len(stmt) > 1 && stmt[0].is("CREATE"):
		i = 1
		if i+1 < len(stmt) && stmt[i].is("OR") && stmt[i+1].is("REPLACE") {
			d.replace = true
			i += 2
		}
		if i >= len(stmt) || !stmt[i].is("MODEL") {
			return d, false, 0, ""
		}
		i++
		if i+2 < len(stmt) && stmt[i].is("IF") && stmt[i+1].is("NOT") && stmt[i+2].is("EXISTS") {
			d.ifNotExists = true
			i += 3
		}
	default:
		return d, false, 0, ""
	}
	path, n := readModelPath(stmt[i:])
	if n == 0 {
		return d, true, 400, "Syntax error: expected a model name"
	}
	d.path = path
	i += n
	if d.drop {
		if i != len(stmt) {
			return d, true, 400, "Syntax error: unexpected " + stmt[i].text
		}
		return d, true, 0, ""
	}
	if i < len(stmt) {
		switch {
		case stmt[i].is("TRANSFORM"):
			if i+1 >= len(stmt) || !stmt[i+1].punct("(") {
				return d, true, 400, "Syntax error: expected ( after TRANSFORM"
			}
			end := matchParen(stmt, i+1)
			if end < 0 {
				return d, true, 400, "Syntax error: unclosed TRANSFORM"
			}
			items, c, m := transformItems(stmt[i+2:end], sql)
			if c != 0 {
				return d, true, c, m
			}
			d.transform = items
			i = end + 1
		case stmt[i].is("INPUT"), stmt[i].is("OUTPUT"), stmt[i].is("REMOTE"):
			return d, true, 501, "Not implemented here: a remote model (REMOTE WITH CONNECTION): it calls a Vertex AI or Cloud AI endpoint through a " +
				"Cloud resource connection, which CloudBurrow has none of. Nothing was run."
		}
	}
	d.opts = defaultMLOptions()
	if i < len(stmt) && stmt[i].is("OPTIONS") {
		if i+1 >= len(stmt) || !stmt[i+1].punct("(") {
			return d, true, 400, "Syntax error: expected ( after OPTIONS"
		}
		end := matchParen(stmt, i+1)
		if end < 0 {
			return d, true, 400, "Syntax error: unclosed OPTIONS"
		}
		if c, m := parseModelOptions(stmt[i+2:end], &d.opts); c != 0 {
			return d, true, c, m
		}
		i = end + 1
	}
	if d.opts.ModelType == "" {
		return d, true, 400, "CREATE MODEL requires the model_type option"
	}
	if why, ok := unsupportedModelTypes[d.opts.ModelType]; ok {
		return d, true, 501, "Not implemented here: model_type " + d.opts.ModelType + ": " + why +
			". CloudBurrow trains LINEAR_REG, LOGISTIC_REG, KMEANS, PCA, MATRIX_FACTORIZATION, BOOSTED_TREE_REGRESSOR / BOOSTED_TREE_CLASSIFIER, RANDOM_FOREST_REGRESSOR / RANDOM_FOREST_CLASSIFIER, DNN_REGRESSOR / DNN_CLASSIFIER, AUTOENCODER and ARIMA_PLUS models. Nothing was run."
	}
	if c, m := checkModelOptions(d.opts); c != 0 {
		return d, true, c, m
	}
	if d.opts.ModelType == "MATRIX_FACTORIZATION" {
		applyMFDefaults(&d.opts)
	}
	if isTreeModel(d.opts.ModelType) {
		applyTreeDefaults(&d.opts)
	}
	if isNetwork(d.opts.ModelType) {
		applyDNNDefaults(&d.opts)
		if c, m := checkDNNOptions(d.opts); c != 0 {
			return d, true, c, m
		}
	}
	if d.opts.ModelType == "ARIMA_PLUS" {
		applyArimaDefaults(&d.opts)
		if c, m := checkArimaOptions(d.opts); c != 0 {
			return d, true, c, m
		}
	}
	if i >= len(stmt) || !stmt[i].is("AS") {
		return d, true, 400, "CREATE MODEL of model_type " + d.opts.ModelType + " requires a query (AS SELECT ...)"
	}
	i++
	if i >= len(stmt) {
		return d, true, 400, "Syntax error: expected a query after AS"
	}
	d.query = strings.TrimSpace(sql[stmt[i].pos:stmt[len(stmt)-1].end])
	return d, true, 0, ""
}

// readModelPath reads a [project.]dataset.model name, plain or quoted,
// and returns its parts and how many tokens it took.
func readModelPath(toks []token) ([]string, int) {
	var parts []string
	i := 0
	for i < len(toks) {
		t := toks[i]
		if t.kind != tokWord && t.kind != tokQuoted {
			break
		}
		if t.kind == tokWord && len(parts) == 0 && (t.is("OPTIONS") || t.is("AS")) {
			break
		}
		if t.kind == tokQuoted {
			parts = append(parts, strings.Split(t.text, ".")...)
		} else {
			parts = append(parts, t.text)
		}
		i++
		// a-b project IDs lex as words and punctuation.
		for i+1 < len(toks) && toks[i].punct("-") && toks[i+1].kind == tokWord && toks[i].pos == toks[i-1].end && toks[i+1].pos == toks[i].end {
			parts[len(parts)-1] += "-" + toks[i+1].text
			i += 2
		}
		if i < len(toks) && toks[i].punct(".") {
			i++
			continue
		}
		break
	}
	if len(parts) == 0 || len(parts) > 3 {
		return nil, 0
	}
	return parts, i
}

// matchParen is the index of the ) that closes the ( at open, or -1.
func matchParen(toks []token, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch {
		case toks[i].punct("("), toks[i].punct("["):
			depth++
		case toks[i].punct(")"), toks[i].punct("]"):
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// parseModelOptions reads OPTIONS(...)'s list into o.
func parseModelOptions(toks []token, o *mlOptions) (int, string) {
	for i := 0; i < len(toks); {
		if toks[i].kind != tokWord || i+2 >= len(toks) || !toks[i+1].punct("=") {
			return 400, "Syntax error in OPTIONS"
		}
		name := strings.ToLower(toks[i].text)
		i += 2
		// The value: up to the next top-level comma.
		j := i
		depth := 0
		for ; j < len(toks); j++ {
			if toks[j].punct("(") || toks[j].punct("[") {
				depth++
			}
			if toks[j].punct(")") || toks[j].punct("]") {
				depth--
			}
			if depth == 0 && toks[j].punct(",") {
				break
			}
		}
		val := toks[i:j]
		i = j + 1
		str := func() (string, bool) {
			if len(val) != 1 || val[0].kind != tokString {
				return "", false
			}
			return unquoteString(val[0].text)
		}
		num := func() (float64, bool) {
			s := ""
			for _, t := range val {
				s += t.text
			}
			v, err := strconv.ParseFloat(s, 64)
			return v, err == nil
		}
		boolv := func() (bool, bool) {
			if len(val) == 1 && (val[0].is("TRUE") || val[0].is("FALSE")) {
				return val[0].is("TRUE"), true
			}
			return false, false
		}
		bad := func() (int, string) { return 400, "Invalid value for option " + name }
		o.Set = append(o.Set, name)
		if c, m, ok := parseDNNOption(name, val, o); ok { // modeldnn.go, #1548
			if c != 0 {
				return c, m
			}
			continue
		}
		switch name {
		case "model_type":
			s, ok := str()
			if !ok {
				return bad()
			}
			o.ModelType = strings.ToUpper(s)
		case "input_label_cols":
			if len(val) < 2 || !val[0].punct("[") || !val[len(val)-1].punct("]") {
				return bad()
			}
			o.LabelCols = nil
			for _, t := range val[1 : len(val)-1] {
				if t.punct(",") {
					continue
				}
				s, ok := unquoteString(t.text)
				if t.kind != tokString || !ok {
					return bad()
				}
				o.LabelCols = append(o.LabelCols, s)
			}
		case "feedback_type":
			s, ok := str()
			if !ok || !strings.EqualFold(s, "EXPLICIT") && !strings.EqualFold(s, "IMPLICIT") {
				return bad()
			}
			o.FeedbackType = strings.ToUpper(s)
			if o.FeedbackType == "IMPLICIT" {
				return 501, "Not implemented here: feedback_type IMPLICIT: implicit feedback's confidence weighting (wals_alpha) and ranking metrics " +
					"are not carried out; CloudBurrow trains EXPLICIT matrix factorization. Nothing was run."
			}
		case "user_col", "item_col", "rating_col":
			s, ok := str()
			if !ok || s == "" {
				return bad()
			}
			switch name {
			case "user_col":
				o.UserCol = s
			case "item_col":
				o.ItemCol = s
			default:
				o.RatingCol = s
			}
		case "max_tree_depth", "num_parallel_tree":
			v, ok := num()
			if !ok || v != float64(int(v)) || v < 1 {
				return bad()
			}
			if name == "num_parallel_tree" {
				o.NumParallelTree = int(v)
				continue
			}
			o.MaxTreeDepth = int(v)
		case "min_split_loss", "min_tree_child_weight":
			v, ok := num()
			if !ok || v < 0 {
				return bad()
			}
			if name == "min_split_loss" {
				o.MinSplitLoss = v
			} else {
				o.MinChildWeight = v
			}
		case "subsample", "colsample_bytree", "colsample_bylevel", "colsample_bynode":
			v, ok := num()
			if !ok || v <= 0 || v > 1 {
				return bad()
			}
			switch name {
			case "subsample":
				o.Subsample = v
			case "colsample_bytree":
				o.ColsampleTree = v
			case "colsample_bylevel":
				o.ColsampleLevel = v
			default:
				o.ColsampleNode = v
			}
		case "booster_type", "tree_method":
			s, ok := str()
			if !ok {
				return bad()
			}
			s = strings.ToUpper(s)
			switch {
			case name == "booster_type" && s == "GBTREE":
				o.BoosterType = s
			case name == "tree_method" && (s == "AUTO" || s == "EXACT"):
				o.TreeMethod = s
			case name == "booster_type" && s == "DART", name == "tree_method" && (s == "APPROX" || s == "HIST"):
				return 501, "Not implemented here: " + name + " " + s + "; CloudBurrow builds GBTREE boosters by the exact greedy method. Nothing was run."
			default:
				return bad()
			}
		case "time_series_timestamp_col", "time_series_data_col":
			s, ok := str()
			if !ok || s == "" {
				return bad()
			}
			if name == "time_series_timestamp_col" {
				o.TS.TimestampCol = s
			} else {
				o.TS.DataCol = s
			}
		case "time_series_id_col":
			if s, ok := str(); ok && s != "" {
				o.TS.IDCol = s
				continue
			}
			if len(val) < 2 || !val[0].punct("[") || !val[len(val)-1].punct("]") {
				return bad()
			}
			var ids []string
			for _, t := range val[1 : len(val)-1] {
				if t.punct(",") {
					continue
				}
				s, ok := unquoteString(t.text)
				if t.kind != tokString || !ok {
					return bad()
				}
				ids = append(ids, s)
			}
			if len(ids) != 1 {
				return 501, "Not implemented here: time_series_id_col of more than one column; CloudBurrow forecasts series identified by one STRING or INT64 column. Nothing was run."
			}
			o.TS.IDCol = ids[0]
		case "horizon", "auto_arima_max_order", "auto_arima_min_order", "trend_smoothing_window_size", "min_time_series_length", "max_time_series_length":
			v, ok := num()
			if !ok || v != float64(int(v)) {
				return bad()
			}
			n := int(v)
			switch name {
			case "horizon":
				if n < 1 || n > 10000 {
					return 400, "horizon must be between 1 and 10000"
				}
				o.TS.Horizon = n
			case "auto_arima_max_order":
				if n < 1 || n > 5 {
					return 400, "auto_arima_max_order must be between 1 and 5"
				}
				o.TS.MaxOrder = n
			case "auto_arima_min_order":
				if n < 0 || n > 4 {
					return 400, "auto_arima_min_order must be between 0 and 4"
				}
				o.TS.MinOrder = n
			case "trend_smoothing_window_size":
				if n < 1 {
					return bad()
				}
				o.TS.SmoothWindow = n
			default:
				if n < 4 {
					return 400, name + " must be at least 4"
				}
				if name == "min_time_series_length" {
					o.TS.MinLength = n
				} else {
					o.TS.MaxLength = n
				}
			}
		case "time_series_length_fraction":
			v, ok := num()
			if !ok || v <= 0 || v >= 1 {
				return 400, "time_series_length_fraction must be in (0, 1)"
			}
			o.TS.LengthFraction = v
		case "non_seasonal_order":
			var nums []int
			if len(val) != 7 || !val[0].punct("(") || !val[6].punct(")") || !val[2].punct(",") || !val[4].punct(",") {
				return 400, "non_seasonal_order must be a tuple (p, d, q) of integers"
			}
			for _, k := range []int{1, 3, 5} {
				n, err := strconv.Atoi(val[k].text)
				if err != nil {
					return 400, "non_seasonal_order must be a tuple (p, d, q) of integers"
				}
				nums = append(nums, n)
			}
			if nums[0] < 0 || nums[0] > 5 || nums[2] < 0 || nums[2] > 5 || nums[1] < 0 || nums[1] > 2 {
				return 400, "non_seasonal_order's p and q must be between 0 and 5, and d between 0 and 2"
			}
			o.TS.Order = nums
		case "data_frequency":
			s, ok := str()
			if !ok {
				return bad()
			}
			s = strings.ToUpper(s)
			found := s == "AUTO_FREQUENCY"
			for _, f := range arimaFreqs {
				found = found || f.name == s
			}
			if !found {
				return bad()
			}
			o.TS.Frequency = s
		case "auto_arima", "include_drift", "clean_spikes_and_dips", "adjust_step_changes", "decompose_time_series":
			b, ok := boolv()
			if !ok {
				return bad()
			}
			switch name {
			case "auto_arima":
				o.TS.AutoArima = b
			case "include_drift":
				o.TS.IncludeDrift = b
			case "clean_spikes_and_dips":
				o.TS.CleanSpikes = b
			case "adjust_step_changes":
				o.TS.AdjustSteps = b
			default:
				o.TS.Decompose = b
			}
		case "seasonalities":
			var ss []string
			if s, ok := str(); ok {
				ss = []string{s}
			} else if len(val) >= 2 && val[0].punct("[") && val[len(val)-1].punct("]") {
				for _, t := range val[1 : len(val)-1] {
					if t.punct(",") {
						continue
					}
					s, ok := unquoteString(t.text)
					if t.kind != tokString || !ok {
						return bad()
					}
					ss = append(ss, strings.ToUpper(s))
				}
			}
			if len(ss) == 1 && (strings.EqualFold(ss[0], "AUTO") || strings.EqualFold(ss[0], "NO_SEASONALITY")) {
				o.TS.Seasonalities = strings.ToUpper(ss[0])
				continue
			}
			for _, s := range ss {
				switch strings.ToUpper(s) {
				case "AUTO", "NO_SEASONALITY":
					return 400, "AUTO and NO_SEASONALITY cannot be combined with other seasonalities"
				case "DAILY", "WEEKLY", "MONTHLY", "QUARTERLY", "YEARLY":
				default:
					return bad()
				}
			}
			if len(ss) == 0 {
				return bad()
			}
			o.TS.Seasonalities = strings.Join(ss, ",")
		case "holiday_region":
			return 501, "Not implemented here: holiday_region: its holiday effects come from Google's holiday calendar " +
				"(bigquery-public-data.ml_datasets.holidays_and_events_for_forecasting), which CloudBurrow does not have. Nothing was run."
		case "forecast_limit_lower_bound", "forecast_limit_upper_bound":
			v, ok := num()
			if !ok {
				return bad()
			}
			if name == "forecast_limit_lower_bound" {
				o.TS.LimitLower = &v
			} else {
				o.TS.LimitUpper = &v
			}
		case "hierarchical_time_series_cols":
			return 501, "Not implemented here: hierarchical_time_series_cols: the reconciliation of hierarchical forecasts is Google's. Nothing was run."
		case "num_factors":
			v, ok := num()
			if !ok || v != float64(int(v)) || v < 1 || v > 200 {
				return 400, "num_factors must be an integer between 1 and 200"
			}
			o.NumFactors = int(v)
		case "num_clusters", "num_principal_components":
			v, ok := num()
			if !ok || v != float64(int(v)) || v < 1 {
				return bad()
			}
			if name == "num_clusters" {
				if v < 2 || v > 100 {
					return 400, "num_clusters must be between 2 and 100"
				}
				o.NumClusters = int(v)
			} else {
				o.NumPC = int(v)
			}
		case "pca_explained_variance_ratio":
			v, ok := num()
			if !ok || v <= 0 || v >= 1 {
				return 400, "pca_explained_variance_ratio must be in (0, 1)"
			}
			o.PCAVarRatio = v
		case "kmeans_init_method", "distance_type", "pca_solver":
			s, ok := str()
			if !ok {
				return bad()
			}
			s = strings.ToUpper(s)
			allowed := map[string][]string{
				"kmeans_init_method": {"RANDOM", "KMEANS++", "CUSTOM"},
				"distance_type":      {"EUCLIDEAN", "COSINE"},
				"pca_solver":         {"FULL", "RANDOMIZED", "AUTO"},
			}[name]
			found := false
			for _, a := range allowed {
				found = found || a == s
			}
			if !found {
				return bad()
			}
			switch name {
			case "kmeans_init_method":
				o.KMeansInit = s
			case "distance_type":
				o.DistanceType = s
			default:
				if s == "RANDOMIZED" {
					return 501, "Not implemented here: pca_solver RANDOMIZED: its randomized solver is Google's; CloudBurrow solves PCA in full (FULL, AUTO). Nothing was run."
				}
				o.PCASolver = s
			}
		case "kmeans_init_col":
			s, ok := str()
			if !ok {
				return bad()
			}
			o.KMeansInitCol = s
		case "standardize_features", "scale_features":
			b, ok := boolv()
			if !ok {
				return bad()
			}
			if name == "standardize_features" {
				o.Standardize = b
			} else {
				o.ScaleFeatures = b
			}
		case "optimize_strategy", "learn_rate_strategy", "data_split_method":
			s, ok := str()
			if !ok {
				return bad()
			}
			s = strings.ToUpper(s)
			allowed := map[string][]string{
				"optimize_strategy":   {"AUTO_STRATEGY", "BATCH_GRADIENT_DESCENT", "NORMAL_EQUATION"},
				"learn_rate_strategy": {"LINE_SEARCH", "CONSTANT"},
				"data_split_method":   {"AUTO_SPLIT", "RANDOM", "CUSTOM", "NO_SPLIT"},
			}[name]
			found := false
			for _, a := range allowed {
				found = found || a == s
			}
			if !found {
				if name == "data_split_method" && s == "SEQ" {
					return 501, "Not implemented here: data_split_method SEQ. CloudBurrow splits AUTO_SPLIT, RANDOM, CUSTOM and NO_SPLIT. Nothing was run."
				}
				return bad()
			}
			switch name {
			case "optimize_strategy":
				o.Strategy = s
			case "learn_rate_strategy":
				o.LearnRateStrat = s
			default:
				o.SplitMethod = s
			}
		case "l1_reg", "l2_reg", "learn_rate", "ls_init_learn_rate", "min_rel_progress", "data_split_eval_fraction", "max_iterations":
			v, ok := num()
			if !ok || v < 0 {
				return bad()
			}
			switch name {
			case "l1_reg":
				o.L1 = v
			case "l2_reg":
				o.L2 = v
			case "learn_rate":
				o.LearnRate = v
			case "ls_init_learn_rate":
				o.InitLearnRate = v
			case "min_rel_progress":
				o.MinRelProgress = v
			case "data_split_eval_fraction":
				if v > 1 {
					return bad()
				}
				o.SplitEvalFrac = v
			case "max_iterations":
				if v < 1 || v != float64(int(v)) {
					return bad()
				}
				o.MaxIterations = int(v)
			}
		case "early_stop":
			b, ok := boolv()
			if !ok {
				return bad()
			}
			o.EarlyStop = b
		case "data_split_col":
			s, ok := str()
			if !ok {
				return bad()
			}
			o.SplitCol = s
		case "category_encoding_method":
			if s, ok := str(); !ok || !strings.EqualFold(s, "ONE_HOT_ENCODING") {
				return 501, "Not implemented here: category_encoding_method other than ONE_HOT_ENCODING. Nothing was run."
			}
		case "enable_global_explain":
			b, ok := boolv()
			if !ok {
				return bad()
			}
			o.GlobalExplain = b
		case "auto_class_weights", "calculate_p_values", "warm_start":
			b, ok := boolv()
			if !ok {
				return bad()
			}
			if !b {
				continue
			}
			return 501, "Not implemented here: CREATE MODEL option " + name + " = " + val[0].text + ". Nothing was run."
		default:
			return 501, "Not implemented here: CREATE MODEL option " + name + "; CloudBurrow carries out model_type, input_label_cols, " +
				"optimize_strategy, l1_reg, l2_reg, max_iterations, learn_rate_strategy, learn_rate, ls_init_learn_rate, early_stop, " +
				"min_rel_progress, data_split_method, data_split_eval_fraction, data_split_col, category_encoding_method, num_clusters, " +
				"kmeans_init_method, kmeans_init_col, distance_type, standardize_features, num_principal_components, " +
				"pca_explained_variance_ratio, scale_features, pca_solver, feedback_type, user_col, item_col, rating_col, num_factors, " +
				"max_tree_depth, min_split_loss, min_tree_child_weight, booster_type, tree_method, num_parallel_tree, subsample, colsample_* and " +
				"the ARIMA_PLUS options. Nothing was run."
		}
	}
	return 0, ""
}

// modelTypeOptions are the options each served model type takes, besides
// model_type.
var modelTypeOptions = map[string][]string{
	"LINEAR_REG": {"input_label_cols", "optimize_strategy", "l1_reg", "l2_reg", "max_iterations", "learn_rate_strategy", "learn_rate",
		"ls_init_learn_rate", "early_stop", "min_rel_progress", "data_split_method", "data_split_eval_fraction", "data_split_col",
		"category_encoding_method", "enable_global_explain", "calculate_p_values", "warm_start"},
	"KMEANS": {"num_clusters", "kmeans_init_method", "kmeans_init_col", "distance_type", "standardize_features", "max_iterations",
		"early_stop", "min_rel_progress", "warm_start"},
	"PCA": {"num_principal_components", "pca_explained_variance_ratio", "scale_features", "pca_solver"},
	"BOOSTED_TREE_REGRESSOR": {"input_label_cols", "max_iterations", "learn_rate", "l1_reg", "l2_reg", "early_stop", "min_rel_progress",
		"data_split_method", "data_split_eval_fraction", "data_split_col", "category_encoding_method", "enable_global_explain", "max_tree_depth",
		"min_split_loss", "min_tree_child_weight", "booster_type", "tree_method", "num_parallel_tree", "subsample", "colsample_bytree",
		"colsample_bylevel", "colsample_bynode"},
	"ARIMA_PLUS": {"time_series_timestamp_col", "time_series_data_col", "time_series_id_col", "horizon", "auto_arima", "auto_arima_max_order",
		"auto_arima_min_order", "non_seasonal_order", "data_frequency", "include_drift", "clean_spikes_and_dips", "adjust_step_changes",
		"decompose_time_series", "seasonalities", "trend_smoothing_window_size", "time_series_length_fraction", "min_time_series_length",
		"max_time_series_length", "forecast_limit_lower_bound", "forecast_limit_upper_bound"},
	"RANDOM_FOREST_REGRESSOR": {"input_label_cols", "l1_reg", "l2_reg", "early_stop", "min_rel_progress", "data_split_method",
		"data_split_eval_fraction", "data_split_col", "enable_global_explain", "max_tree_depth", "min_split_loss", "min_tree_child_weight",
		"tree_method", "num_parallel_tree", "subsample", "colsample_bytree", "colsample_bylevel", "colsample_bynode"},
	"DNN_REGRESSOR": {"input_label_cols", "optimizer", "l1_reg", "l2_reg", "max_iterations", "learn_rate", "early_stop", "min_rel_progress",
		"data_split_method", "data_split_eval_fraction", "data_split_col", "category_encoding_method", "hidden_units", "activation_fn",
		"batch_size", "dropout", "warm_start", "enable_global_explain"},
	"AUTOENCODER": {"optimizer", "learn_rate", "activation_fn", "batch_size", "dropout", "hidden_units", "early_stop", "min_rel_progress",
		"max_iterations", "warm_start", "l1_reg_activation"},
	"MATRIX_FACTORIZATION": {"feedback_type", "user_col", "item_col", "rating_col", "num_factors", "l2_reg", "max_iterations", "early_stop",
		"min_rel_progress", "data_split_method", "data_split_eval_fraction", "data_split_col"},
}

// checkModelOptions refuses an option the model type does not take, and
// a combination BigQuery documents as invalid.
func checkModelOptions(o mlOptions) (int, string) {
	allowed, ok := modelTypeOptions[o.ModelType]
	if o.ModelType == "LOGISTIC_REG" {
		allowed, ok = append(append([]string(nil), modelTypeOptions["LINEAR_REG"]...), "auto_class_weights"), true
	}
	if o.ModelType == "BOOSTED_TREE_CLASSIFIER" {
		allowed, ok = append(append([]string(nil), modelTypeOptions["BOOSTED_TREE_REGRESSOR"]...), "auto_class_weights"), true
	}
	if o.ModelType == "RANDOM_FOREST_CLASSIFIER" {
		allowed, ok = append(append([]string(nil), modelTypeOptions["RANDOM_FOREST_REGRESSOR"]...), "auto_class_weights"), true
	}
	if o.ModelType == "DNN_CLASSIFIER" {
		allowed, ok = append(append([]string(nil), modelTypeOptions["DNN_REGRESSOR"]...), "auto_class_weights", "class_weights"), true
	}
	if c, m := checkTreeSampling(o); c != 0 {
		return c, m
	}
	if !ok {
		return 400, "Invalid model_type: " + o.ModelType
	}
	for _, s := range o.Set {
		found := s == "model_type"
		for _, a := range allowed {
			found = found || a == s
		}
		if !found {
			return 400, "Option " + s + " is not supported for model type " + o.ModelType
		}
	}
	switch o.ModelType {
	case "KMEANS":
		if (o.KMeansInit == "CUSTOM") != (o.KMeansInitCol != "") {
			return 400, "kmeans_init_col must be set exactly when kmeans_init_method is CUSTOM"
		}
	case "PCA":
		if (o.NumPC > 0) == (o.PCAVarRatio > 0) {
			return 400, "Exactly one of num_principal_components and pca_explained_variance_ratio must be set for a PCA model"
		}
	}
	return 0, ""
}

// resolveModel is a model path's project, dataset and ID, with q's
// default dataset for a bare ID.
func (f front) resolveModel(path []string, defaultDataset string) (project, dataset, id string, ok bool) {
	project = projectOf(f.base)
	switch len(path) {
	case 1:
		if defaultDataset == "" {
			return "", "", "", false
		}
		ds := defaultDataset
		if k := strings.Index(ds, "."); k >= 0 {
			project, ds = ds[:k], ds[k+1:]
		}
		return project, ds, path[0], true
	case 2:
		return project, path[0], path[1], true
	case 3:
		return path[0], path[1], path[2], true
	}
	return "", "", "", false
}

// modelDatasetLocation reads a dataset's location, or reports it missing.
func (f front) modelDatasetLocation(r *http.Request, dataset string) (string, bool) {
	st, b := f.get(r, "/datasets/"+url.PathEscape(dataset))
	if st == http.StatusNotFound {
		return "", false
	}
	var ds struct {
		Location string `json:"location"`
	}
	_ = json.Unmarshal(b, &ds)
	return ds.Location, true
}

// modelStatements carries out q when it is a lone CREATE MODEL or DROP
// MODEL statement, and reports whether it was one.
func (f front) modelStatements(w http.ResponseWriter, r *http.Request, q queryOptions, insert bool) bool {
	if f.models == nil || !strings.Contains(strings.ToUpper(q.Query), "MODEL") {
		return false
	}
	toks, ok := lex(q.Query)
	if !ok {
		return false
	}
	stmts := splitStatements(toks)
	var found *modelDDL
	others := 0
	var code int
	var msg string
	for _, stmt := range stmts {
		if len(stmt) == 0 {
			continue
		}
		d, is, c, m := parseModelDDL(stmt, q.Query)
		if !is {
			others++
			continue
		}
		if found == nil {
			found, code, msg = &d, c, m
		} else {
			others++
		}
	}
	if found == nil {
		return false
	}
	fail := func(code int, msg string) {
		reason := map[int]string{400: "invalidQuery", 404: "notFound", 409: "duplicate", 501: "notImplemented"}[code]
		if code == 501 {
			writeError(w, code, reason, msg)
			return
		}
		f.failBeforeRun(w, r, q, insert, "_cloudburrow", code, rowError{Reason: reason, Message: msg})
	}
	if others > 0 {
		fail(501, "Not implemented here: CREATE MODEL or DROP MODEL in a script; CloudBurrow carries them out as a query of their own. Nothing was run.")
		return true
	}
	if code != 0 {
		fail(code, msg)
		return true
	}
	d := *found
	project, dataset, id, ok := f.resolveModel(d.path, defaultDatasetOf(q))
	if !ok {
		fail(400, "Model name "+strings.Join(d.path, ".")+" missing dataset while no default dataset is set in the request.")
		return true
	}
	if project != projectOf(f.base) {
		fail(501, "Not implemented here: a model of project "+project+", which is not this instance's project. Nothing was run.")
		return true
	}
	loc, exists := f.modelDatasetLocation(r, dataset)
	if !exists {
		fail(404, fmt.Sprintf("Not found: Dataset %s:%s", project, dataset))
		return true
	}
	_, had := f.models.get(project, dataset, id)
	dry := dryRun(r, insert)
	switch {
	case d.drop && !had && !d.ifExists:
		fail(404, fmt.Sprintf("Not found: Model %s:%s.%s", project, dataset, id))
		return true
	case d.drop:
		if !dry {
			f.models.remove(project, dataset, id)
		}
	case had && d.ifNotExists:
		// Nothing to do.
	case had && !d.replace:
		fail(409, fmt.Sprintf("Already Exists: Model %s:%s.%s", project, dataset, id))
		return true
	default:
		trainSQL := d.query
		if len(d.transform) > 0 {
			for _, it := range d.transform {
				if strings.HasPrefix(it, "*") {
					qc, _, c, m := f.queryAllRows(r, q, "SELECT * FROM ("+d.query+") LIMIT 0")
					if c != 0 {
						fail(c, m)
						return true
					}
					items, why := expandTransform(d.transform, qc)
					if why != "" {
						fail(501, why)
						return true
					}
					d.transform = items
					break
				}
			}
			items, c, m := f.learnTransform(r, q, d.transform, d.query)
			if c != 0 {
				fail(c, m)
				return true
			}
			d.transform = items
			trainSQL = "SELECT " + strings.Join(d.transform, ", ") + " FROM (" + d.query + ")"
		}
		cols, rows, c, m := f.queryAllRows(r, q, trainSQL)
		if c != 0 {
			fail(c, m)
			return true
		}
		trained, fe := trainModel(d.opts, cols, rows)
		if fe != nil {
			msg := fe.msg
			if fe.code == 501 {
				msg += ". Nothing was run."
			}
			fail(fe.code, msg)
			return true
		}
		if !dry {
			now := f.now().UnixMilli()
			f.models.put(bqModel{Project: project, Dataset: dataset, ID: id, Type: d.opts.ModelType, Created: now, Modified: now,
				Location: loc, Options: d.opts, Trained: trained, Query: d.query, Transform: d.transform})
		}
	}
	// The emulator runs a statement that does nothing in its place, which
	// records the job (as policyStatements does).
	noop := "DROP TABLE IF EXISTS " + quotePath([]string{"_cloudburrow", scratchTable()})
	if !setQueryText(r, insert, noop) {
		writeError(w, http.StatusInternalServerError, "internalError", "cloudburrow: could not rewrite the query")
		return true
	}
	client := jobText{query: q.Query}
	q.Query = noop
	rec := newRecorder()
	f.serveQuery(rec, r, q, insert)
	f.clientText(w, rec, client)
	return true
}

// queryAllRows runs sql, a query, with q's default dataset and
// parameters, and returns its columns and every row's values.
func (f front) queryAllRows(r *http.Request, q queryOptions, sql string) ([]mlColumn, [][]*string, int, string) {
	text, _, msg := qualifyTables(sql, defaultDatasetOf(q))
	if msg != "" {
		return nil, nil, 400, msg
	}
	legacy := false
	req, err := json.Marshal(map[string]any{"query": text, "useLegacySql": legacy, "defaultDataset": q.DefaultDataset,
		"parameterMode": q.ParameterMode, "queryParameters": q.QueryParameters, "maxResults": 100000, "timeoutMs": 600000})
	if err != nil {
		return nil, nil, 500, err.Error()
	}
	type page struct {
		Schema struct {
			Fields []struct {
				Name string `json:"name"`
				Type string `json:"type"`
				Mode string `json:"mode"`
			} `json:"fields"`
		} `json:"schema"`
		Rows []struct {
			F []struct {
				V any `json:"v"`
			} `json:"f"`
		} `json:"rows"`
		PageToken    string `json:"pageToken"`
		JobReference struct {
			JobID    string `json:"jobId"`
			Location string `json:"location"`
		} `json:"jobReference"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	st, got := f.send(r, http.MethodPost, "/queries", req)
	var p page
	if json.Unmarshal(got, &p) != nil || st != http.StatusOK {
		m := p.Error.Message
		if m == "" {
			m = fmt.Sprintf("HTTP %d", st)
		}
		if st >= 500 || st == 0 {
			return nil, nil, 501, "Not implemented here: the model's training query could not be run (" + m + "). Nothing was run."
		}
		return nil, nil, st, m
	}
	var cols []mlColumn
	for _, fl := range p.Schema.Fields {
		ty := fl.Type
		if fl.Mode == "REPEATED" {
			ty = "ARRAY<" + ty + ">"
		}
		cols = append(cols, mlColumn{Name: fl.Name, Type: ty})
	}
	var rows [][]*string
	for {
		for _, row := range p.Rows {
			vals := make([]*string, len(row.F))
			for i, c := range row.F {
				if s, ok := c.V.(string); ok {
					vals[i] = &s
				} else if c.V != nil {
					s := fmt.Sprint(c.V)
					vals[i] = &s
				}
			}
			rows = append(rows, vals)
		}
		if p.PageToken == "" || p.JobReference.JobID == "" {
			break
		}
		path := "/queries/" + url.PathEscape(p.JobReference.JobID) + "?maxResults=100000&pageToken=" + url.QueryEscape(p.PageToken)
		if p.JobReference.Location != "" {
			path += "&location=" + url.QueryEscape(p.JobReference.Location)
		}
		st, got = f.get(r, path)
		token := p.PageToken
		p = page{}
		if st != http.StatusOK || json.Unmarshal(got, &p) != nil {
			return nil, nil, 500, "cloudburrow: could not read the training query's rows: " + errorMessage(got, st)
		}
		if p.PageToken == token {
			break
		}
	}
	return cols, rows, 0, ""
}
