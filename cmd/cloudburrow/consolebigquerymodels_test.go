package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testModel = `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"m"},"modelType":"LINEAR_REGRESSION",
"creationTime":"1700000000000","lastModifiedTime":"1700000000000","location":"US","etag":"e",
"featureColumns":[{"name":"x","type":{"typeKind":"FLOAT64"}},{"name":"c","type":{"typeKind":"STRING"}}],
"labelColumns":[{"name":"predicted_y","type":{"typeKind":"FLOAT64"}}],
"trainingRuns":[{"trainingOptions":{"optimizationStrategy":"NORMAL_EQUATION","dataSplitMethod":"AUTO_SPLIT"},
"results":[{"index":0,"durationMs":"3","trainingLoss":0.25,"learnRate":0}],
"evaluationMetrics":{"regressionMetrics":{"meanAbsoluteError":0.5,"meanSquaredError":0.25,"medianAbsoluteError":0.5,"rSquared":0.9}}}]}`

// modelsAPI serves models.list, models.get, models.patch, models.delete and
// the ML.WEIGHTS query of the model page, as the front answers them.
func modelsAPI(t *testing.T, calls *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/datasets/d/models"):
			fmt.Fprint(w, `{"models":[`+testModel+`]}`)
		case strings.HasSuffix(r.URL.Path, "/datasets/d/models/m") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/datasets/d/models/m") && r.Method == http.MethodPatch:
			if !strings.Contains(string(b), `"description":"new"`) {
				t.Errorf("models.patch body %s", b)
			}
			fmt.Fprint(w, testModel)
		case strings.HasSuffix(r.URL.Path, "/datasets/d/models/m"):
			fmt.Fprint(w, testModel)
		case strings.HasSuffix(r.URL.Path, "/queries") && strings.Contains(string(b), "ML.GLOBAL_EXPLAIN"):
			// A model trained without enable_global_explain.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"ML.GLOBAL_EXPLAIN requires a model trained with enable_global_explain = TRUE"}}`)
		case strings.HasSuffix(r.URL.Path, "/queries"):
			if !strings.Contains(string(b), "ML.WEIGHTS(MODEL `p.d.m`)") {
				t.Errorf("query %s", b)
			}
			fmt.Fprint(w, `{"jobReference":{"projectId":"p","jobId":"j"},"jobComplete":true,"totalRows":"3",
"schema":{"fields":[{"name":"processed_input","type":"STRING"},{"name":"weight","type":"FLOAT"},{"name":"n","type":"INTEGER"}]},
"rows":[{"f":[{"v":"x"},{"v":"2"},{"v":"0"}]},{"f":[{"v":"c"},{"v":null},{"v":"2"}]},{"f":[{"v":"__INTERCEPT__"},{"v":"3"},{"v":"0"}]}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestBigQueryModelsSectionAndPage(t *testing.T) {
	var calls []string
	api := modelsAPI(t, &calls)
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	ctx := context.Background()
	c, err := p.client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sec := p.modelsSection(ctx, c.Dataset("d"))
	if sec.ID != "models" || len(sec.Listing.Items) != 1 {
		t.Fatalf("the Models section = %+v", sec)
	}
	row := sec.Listing.Items[0]
	if row.Name != "m" || row.Fields["Type"] != "LINEAR_REGRESSION" || strings.Join(row.Opens, "/") != "d/models/m" || row.Actions[0].ID != actDeleteModel {
		t.Errorf("the m row = %+v", row)
	}
	d, err := p.Detail(ctx, "p", []string{"d", "models", "m"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("the model's page: %v %+v", err, d)
	}
	s := fmt.Sprintf("%+v", d.Sections)
	for _, want := range []string{"Weight:2", "2 category weights", "__INTERCEPT__", "NORMAL_EQUATION", "R²", "predicted_y FLOAT64"} {
		if !strings.Contains(s, want) {
			t.Errorf("the model's page lacks %q: %s", want, s)
		}
	}
	if err := p.editModel(ctx, "d", "m", map[string]string{"description": "new"}); err != nil {
		t.Errorf("editModel: %v", err)
	}
	if err := p.deleteModel(ctx, "d", "m"); err != nil {
		t.Errorf("deleteModel: %v", err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "DELETE /bigquery/v2/projects/p/datasets/d/models/m") &&
		!strings.Contains(strings.Join(calls, "\n"), "DELETE /projects/p/datasets/d/models/m") {
		t.Errorf("no models.delete: %v", calls)
	}
}

// TestBigQueryModelPageResults: a KMEANS model's page shows its
// centroids, a LOGISTIC_REG model's its confusion matrix, a PCA model's
// its components, a MATRIX_FACTORIZATION model's its recommendations and
// factors, and a boosted tree model's its feature importance, from the ML
// functions the front answers (#1481, #1504).
func TestBigQueryModelPageResults(t *testing.T) {
	models := map[string]string{
		"imp": `"modelType":"MATRIX_FACTORIZATION","trainingRuns":[{"evaluationMetrics":{"rankingMetrics":{"meanAveragePrecision":0.9,"averageRank":0.2}}}]`,
		"km":  `"modelType":"KMEANS","trainingRuns":[{"evaluationMetrics":{"clusteringMetrics":{"daviesBouldinIndex":0.1,"meanSquaredDistance":1.5}}}]`,
		"lg":  `"modelType":"LOGISTIC_REGRESSION","trainingRuns":[{"evaluationMetrics":{"multiClassClassificationMetrics":{"aggregateClassificationMetrics":{"accuracy":0.9}}}}]`,
		"pc":  `"modelType":"PCA","trainingRuns":[{"evaluationMetrics":{"dimensionalityReductionMetrics":{"totalExplainedVarianceRatio":0.75}}}]`,
		"bt":  `"modelType":"BOOSTED_TREE_REGRESSOR","trainingRuns":[{"evaluationMetrics":{"regressionMetrics":{"meanSquaredError":0.5}}}]`,
		"rf":  `"modelType":"RANDOM_FOREST_CLASSIFIER","trainingRuns":[{"evaluationMetrics":{"regressionMetrics":{"meanSquaredError":0.5}}}]`,
		"ar":  `"modelType":"ARIMA_PLUS","trainingRuns":[{"results":[{"index":0,"arimaResult":{"arimaModelInfo":[]}}]}]`,
		"mf":  `"modelType":"MATRIX_FACTORIZATION","trainingRuns":[{"evaluationMetrics":{"regressionMetrics":{"meanSquaredError":0.5}}}]`,
		"xr":  `"modelType":"ARIMA_PLUS_XREG","trainingRuns":[{"results":[{"index":0,"arimaResult":{"arimaModelInfo":[]}}]}]`,
	}
	rows := func(fields, rows string) string {
		return `{"jobReference":{"projectId":"p","jobId":"j"},"jobComplete":true,"schema":{"fields":[` + fields + `]},"rows":[` + rows + `]}`
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		for id, m := range models {
			if strings.HasSuffix(r.URL.Path, "/datasets/d/models/"+id) {
				fmt.Fprint(w, `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"`+id+`"},`+m+`}`)
				return
			}
		}
		q := string(b)
		switch {
		case strings.Contains(q, "ML.CENTROIDS"):
			fmt.Fprint(w, rows(`{"name":"centroid_id","type":"INTEGER"},{"name":"feature","type":"STRING"},{"name":"numerical_value","type":"FLOAT"},`+
				`{"name":"categorical_value","type":"RECORD","mode":"REPEATED","fields":[{"name":"category","type":"STRING"},{"name":"value","type":"FLOAT"}]}`,
				`{"f":[{"v":"1"},{"v":"x"},{"v":"11.5"},{"v":[]}]},{"f":[{"v":"1"},{"v":"c"},{"v":null},{"v":[{"v":{"f":[{"v":"a"},{"v":"0.25"}]}}]}]}`))
		case strings.Contains(q, "ML.CONFUSION_MATRIX"):
			fmt.Fprint(w, rows(`{"name":"expected_label","type":"STRING"},{"name":"a","type":"INTEGER"},{"name":"b","type":"INTEGER"}`,
				`{"f":[{"v":"a"},{"v":"7"},{"v":"1"}]},{"f":[{"v":"b"},{"v":"0"},{"v":"9"}]}`))
		case strings.Contains(q, "ML.FEATURE_IMPORTANCE"):
			fmt.Fprint(w, rows(`{"name":"feature","type":"STRING"},{"name":"importance_weight","type":"INTEGER"},{"name":"importance_gain","type":"FLOAT"},`+
				`{"name":"importance_cover","type":"FLOAT"}`, `{"f":[{"v":"x"},{"v":"20"},{"v":"3.5"},{"v":"10"}]}`))
		case strings.Contains(q, "ML.GLOBAL_EXPLAIN"):
			fmt.Fprint(w, rows(`{"name":"feature","type":"STRING"},{"name":"attribution","type":"FLOAT"}`, `{"f":[{"v":"x"},{"v":"1.25"}]}`))
		case strings.Contains(q, "ML.FORECAST"):
			fmt.Fprint(w, rows(`{"name":"forecast_timestamp","type":"TIMESTAMP"},{"name":"forecast_value","type":"FLOAT"},{"name":"standard_error","type":"FLOAT"},`+
				`{"name":"confidence_level","type":"FLOAT"},{"name":"prediction_interval_lower_bound","type":"FLOAT"},{"name":"prediction_interval_upper_bound","type":"FLOAT"}`,
				`{"f":[{"v":"1704931200000000"},{"v":"19.5"},{"v":"0.75"},{"v":"0.95"},{"v":"18"},{"v":"21"}]}`))
		case strings.Contains(q, "ML.ARIMA_COEFFICIENTS"):
			fmt.Fprint(w, rows(`{"name":"processed_input","type":"STRING"},{"name":"weight","type":"FLOAT"},{"name":"f0_","type":"STRING"}`,
				`{"f":[{"v":"temp"},{"v":"2.5"},{"v":""}]}`))
		case strings.Contains(q, "ML.ARIMA_EVALUATE"):
			fmt.Fprint(w, rows(`{"name":"f0_","type":"STRING"},{"name":"log_likelihood","type":"FLOAT"},{"name":"AIC","type":"FLOAT"},{"name":"variance","type":"FLOAT"},`+
				`{"name":"f1_","type":"STRING"},{"name":"has_spikes_and_dips","type":"BOOLEAN"},{"name":"has_step_changes","type":"BOOLEAN"},`+
				`{"name":"has_holiday_effect","type":"BOOLEAN"}`,
				`{"f":[{"v":"ARIMA(0, 1, 0) with drift"},{"v":"-9.5"},{"v":"23"},{"v":"0.6"},{"v":"WEEKLY"},{"v":"true"},{"v":"false"},{"v":"true"}]}`))
		case strings.Contains(q, "ML.HOLIDAY_INFO"):
			fmt.Fprint(w, rows(`{"name":"holiday_name","type":"STRING"},{"name":"region","type":"STRING"},{"name":"f0_","type":"STRING"},`+
				`{"name":"preholiday_days","type":"INTEGER"},{"name":"postholiday_days","type":"INTEGER"}`,
				`{"f":[{"v":"Fest"},{"v":"XX"},{"v":"2024-02-10"},{"v":"1"},{"v":"2"}]}`))
		case strings.Contains(q, "ML.RECOMMEND"):
			fmt.Fprint(w, rows(`{"name":"predicted_rating","type":"FLOAT"},{"name":"user","type":"INTEGER"},{"name":"item","type":"STRING"}`,
				`{"f":[{"v":"4.5"},{"v":"7"},{"v":"film"}]}`))
		case strings.Contains(q, "ML.WEIGHTS") && strings.Contains(q, "factor_weights"):
			fmt.Fprint(w, rows(`{"name":"processed_input","type":"STRING"},{"name":"feature","type":"STRING"},{"name":"intercept","type":"FLOAT"},`+
				`{"name":"f0_","type":"FLOAT","mode":"REPEATED"}`,
				`{"f":[{"v":"user"},{"v":"7"},{"v":"0.25"},{"v":[{"v":"1.5"},{"v":"-2"}]}]}`))
		case strings.Contains(q, "ML.PRINCIPAL_COMPONENT_INFO"):
			fmt.Fprint(w, rows(`{"name":"principal_component_id","type":"INTEGER"},{"name":"eigenvalue","type":"FLOAT"},`+
				`{"name":"explained_variance_ratio","type":"FLOAT"},{"name":"cumulative_explained_variance_ratio","type":"FLOAT"}`,
				`{"f":[{"v":"1"},{"v":"1.5"},{"v":"0.75"},{"v":"0.75"}]}`))
		case strings.Contains(q, "ML.PRINCIPAL_COMPONENTS"):
			fmt.Fprint(w, rows(`{"name":"principal_component_id","type":"INTEGER"},{"name":"feature","type":"STRING"},{"name":"numerical_value","type":"FLOAT"}`,
				`{"f":[{"v":"1"},{"v":"x"},{"v":"0.6"}]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"not supported"}}`)
		}
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	for id, wants := range map[string][]string{
		"imp": {"Mean average precision", "Average rank"},
		"km":  {"Centroids", "Value:11.5", "a=0.25", "Davies-Bouldin index"},
		"lg":  {"Confusion matrix", "Predicted a:7", "Predicted b:9", "Accuracy"},
		"pc":  {"Components", "Eigenvalue:1.5", "x=0.6", "Total explained variance ratio"},
		"bt":  {"Feature importance", "Splits:20", "Mean gain:3.5", "Mean cover:10"},
		"rf":  {"Feature importance", "Splits:20", "Global explanation", "Attribution:1.25"},
		"ar": {"Forecast", "Name:2024-01-11T00:00:00Z", "Forecast:19.5", "Interval:18 – 21", "Candidate models", "AIC:23", "Seasonal periods:WEEKLY", "Spikes and dips:true", "Step changes:false",
			"Holiday effect:true", "Custom holidays", "Name:Fest", "Region:XX", "Primary date:2024-02-10", "Days before:1", "Days after:2"},
		"mf": {"Recommendations", "Name:4.5", "user:7", "item:film", "Factors", "Intercept:0.25", "Factor weights:1.5, -2"},
	} {
		d, err := p.Detail(context.Background(), "p", []string{"d", "models", id})
		if err != nil || d.Unavailable != "" {
			t.Fatalf("%s: %v %+v", id, err, d)
		}
		s := fmt.Sprintf("%+v", d.Sections)
		for _, want := range wants {
			if !strings.Contains(s, want) {
				t.Errorf("%s's page lacks %q: %s", id, want, s)
			}
		}
	}
}

// TestBigQueryONNXModelPage (#1711): an imported ONNX model's page shows
// its inputs and outputs, as models.get reports them, and where it was
// imported from; it asks for no weights.
func TestBigQueryONNXModelPage(t *testing.T) {
	const model = `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"lin"},"modelType":"ONNX",
"creationTime":"1700000000000","lastModifiedTime":"1700000000000","location":"US","etag":"e",
"featureColumns":[{"name":"a","type":{"typeKind":"FLOAT64"}},{"name":"v","type":{"typeKind":"ARRAY","arrayElementType":{"typeKind":"FLOAT64"}}}],
"labelColumns":[{"name":"y","type":{"typeKind":"FLOAT64"}},{"name":"label","type":{"typeKind":"INT64"}}],
"trainingRuns":[{"trainingOptions":{"modelUri":"gs://b/models/linear/*"},"startTime":"2023-11-14T22:13:20Z"}]}`
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/datasets/d/models/lin") {
			fmt.Fprint(w, model)
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	d, err := p.Detail(context.Background(), "p", []string{"d", "models", "lin"})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("the model's page: %v %+v", err, d)
	}
	sections := map[string]string{}
	for _, s := range d.Sections {
		var rows []string
		for _, it := range s.Listing.Items {
			rows = append(rows, it.Name+" "+it.Fields["Type"])
		}
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				rows = append(rows, p.Label+"="+p.Value)
			}
		}
		sections[s.ID] = strings.Join(rows, "; ")
	}
	if sections["inputs"] != "a FLOAT64; v ARRAY<FLOAT64>" || sections["outputs"] != "y FLOAT64; label INT64" ||
		!strings.Contains(sections["details"], "Imported from=gs://b/models/linear/*") || len(d.Sections) != 3 {
		t.Errorf("sections %q", sections)
	}
	for _, c := range calls {
		if strings.HasSuffix(c, "/queries") {
			t.Errorf("the page of an ONNX model ran a query: %v", calls)
		}
	}
}

// TestBigQueryXGBoostModelPage (#1711): an imported XGBoost model's page
// is an imported model's, its type XGBOOST, its one output the label.
func TestBigQueryXGBoostModelPage(t *testing.T) {
	const model = `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"x"},"modelType":"XGBOOST",
"creationTime":"1700000000000","lastModifiedTime":"1700000000000","location":"US","etag":"e",
"featureColumns":[{"name":"f_a","type":{"typeKind":"FLOAT64"}},{"name":"f_cat","type":{"typeKind":"INT64"}}],
"labelColumns":[{"name":"predicted_label","type":{"typeKind":"FLOAT64"}}],
"trainingRuns":[{"trainingOptions":{"modelUri":"gs://b/xgb/*"},"startTime":"2023-11-14T22:13:20Z"}]}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/datasets/d/models/x") {
			fmt.Fprint(w, model)
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	d, err := p.Detail(context.Background(), "p", []string{"d", "models", "x"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 3 {
		t.Fatalf("the model's page: %v %+v", err, d)
	}
	if d.Summary[1].Value != "XGBOOST" || d.Sections[0].Listing.Items[1].Name != "f_cat" ||
		d.Sections[1].Listing.Items[0].Name != "predicted_label" {
		t.Errorf("page %+v", d)
	}
}

// TestBigQueryTFLiteModelPage (#1755): an imported TensorFlow Lite model's
// page is an imported model's, its type TENSORFLOW_LITE, its signature's
// inputs and outputs listed, ARRAY ones as ARRAY.
func TestBigQueryTFLiteModelPage(t *testing.T) {
	const model = `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"x"},"modelType":"TENSORFLOW_LITE",
"creationTime":"1700000000000","lastModifiedTime":"1700000000000","location":"US","etag":"e",
"featureColumns":[{"name":"a","type":{"typeKind":"FLOAT64"}},{"name":"b","type":{"typeKind":"ARRAY","arrayElementType":{"typeKind":"FLOAT64"}}}],
"labelColumns":[{"name":"logits","type":{"typeKind":"ARRAY","arrayElementType":{"typeKind":"FLOAT64"}}},{"name":"score","type":{"typeKind":"FLOAT64"}}],
"trainingRuns":[{"trainingOptions":{"modelUri":"gs://b/tfl/multi.tflite"},"startTime":"2023-11-14T22:13:20Z"}]}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/datasets/d/models/x") {
			fmt.Fprint(w, model)
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	d, err := p.Detail(context.Background(), "p", []string{"d", "models", "x"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 3 {
		t.Fatalf("the model's page: %v %+v", err, d)
	}
	if d.Summary[1].Value != "TENSORFLOW_LITE" || d.Sections[0].Listing.Items[1].Name != "b" ||
		d.Sections[1].Listing.Items[1].Name != "score" || d.Summary[3].Value != "2" {
		t.Errorf("page %+v", d)
	}
}

// TestBigQueryRemoteModelPage (#1760): a remote model's page names its
// endpoint and connection (models.get's remoteModelInfo), says it runs on
// the local Vertex AI, and offers the function that runs it; it runs no
// query.
func TestBigQueryRemoteModelPage(t *testing.T) {
	models := map[string]string{
		"gen": `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"gen"},"creationTime":"1700000000000","lastModifiedTime":"1700000000000",
"location":"US","etag":"e","remoteModelInfo":{"endpoint":"gemma-4-e2b-it-community","connection":"projects/1/locations/us/connections/ai"}}`,
		"emb": `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"emb"},"creationTime":"1700000000000","lastModifiedTime":"1700000000000",
"location":"US","etag":"e","remoteModelInfo":{"endpoint":"embeddinggemma-300m-onnx-community","connection":"projects/1/locations/us/connections/ai"}}`,
	}
	var calls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if m, ok := models[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]; ok && strings.Contains(r.URL.Path, "/datasets/d/models/") {
			fmt.Fprint(w, m)
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	for id, fn := range map[string]string{"gen": "ML.GENERATE_TEXT(MODEL `d.gen`", "emb": "ML.GENERATE_EMBEDDING(MODEL `d.emb`"} {
		d, err := p.Detail(context.Background(), "p", []string{"d", "models", id})
		if err != nil || d.Unavailable != "" || len(d.Sections) != 1 {
			t.Fatalf("%s: %v %+v", id, err, d)
		}
		var props []string
		for _, g := range d.Sections[0].Groups {
			for _, pr := range g.Properties {
				props = append(props, pr.Label+"="+pr.Value)
			}
		}
		all := strings.Join(props, "; ")
		if !strings.Contains(all, "Type=Remote model") || !strings.Contains(all, "Connection=projects/1/locations/us/connections/ai") ||
			!strings.Contains(all, "Endpoint="+map[string]string{"gen": "gemma-4-e2b-it-community", "emb": "embeddinggemma-300m-onnx-community"}[id]) ||
			!strings.Contains(all, fn) || !strings.Contains(all, "never Google's") {
			t.Errorf("%s: %s", id, all)
		}
	}
	for _, c := range calls {
		if strings.HasSuffix(c, "/queries") {
			t.Errorf("the page of a remote model ran a query: %v", calls)
		}
	}
}

// TestBigQueryTensorFlowModelPage (#1775): an imported TensorFlow
// SavedModel's page is an imported model's, its type TENSORFLOW, its
// serving_default inputs and outputs listed and its SavedModel folder
// named.
func TestBigQueryTensorFlowModelPage(t *testing.T) {
	const model = `{"modelReference":{"projectId":"p","datasetId":"d","modelId":"tf"},"modelType":"TENSORFLOW",
"creationTime":"1700000000000","lastModifiedTime":"1700000000000","location":"US","etag":"e",
"featureColumns":[{"name":"k","type":{"typeKind":"INT64"}},{"name":"x","type":{"typeKind":"ARRAY","arrayElementType":{"typeKind":"FLOAT64"}}}],
"labelColumns":[{"name":"probs","type":{"typeKind":"ARRAY","arrayElementType":{"typeKind":"FLOAT64"}}},{"name":"score","type":{"typeKind":"FLOAT64"}}],
"trainingRuns":[{"trainingOptions":{"modelUri":"gs://b/tf/mlp/*"},"startTime":"2023-11-14T22:13:20Z"}]}`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/datasets/d/models/tf") {
			fmt.Fprint(w, model)
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	p := bigqueryProvider{endpoint: strings.TrimPrefix(api.URL, "http://"), project: "p"}
	d, err := p.Detail(context.Background(), "p", []string{"d", "models", "tf"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 3 {
		t.Fatalf("the model's page: %v %+v", err, d)
	}
	var details []string
	for _, g := range d.Sections[2].Groups {
		for _, p := range g.Properties {
			details = append(details, p.Label+"="+p.Value)
		}
	}
	if d.Summary[1].Value != "TENSORFLOW" || d.Sections[0].Listing.Items[1].Fields["Type"] != "ARRAY<FLOAT64>" ||
		d.Sections[1].Listing.Items[0].Name != "probs" || !strings.Contains(strings.Join(details, ";"), "Imported from=gs://b/tf/mlp/*") {
		t.Errorf("page %+v", d)
	}
}
