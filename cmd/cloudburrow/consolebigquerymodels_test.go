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
