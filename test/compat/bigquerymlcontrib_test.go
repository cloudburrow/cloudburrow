//go:build compat

package compat

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
)

// TestBigQueryMLContributionAnalysis: a CONTRIBUTION_ANALYSIS model and
// ML.GET_INSIGHTS through the official cloud.google.com/go/bigquery
// client (#1929), on the CREATE MODEL page's summable example: the "all"
// row's sums, a segment's documented difference, unexpected_difference
// and apriori_support, rows ordered by contribution, models.get's
// CONTRIBUTION_ANALYSIS type and options; a summable ratio model's
// metric_test_over_complement against the ML.GET_INSIGHTS page's worked
// example; a summable by category model's columns; and the documented
// option errors.
//
// covers: bigquery.jobs.query, bigquery.models.get
func TestBigQueryMLContributionAnalysis(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ctx := h.Context()
	name := mlDataset(t, h, c, map[string]string{
		"sales": "SELECT * FROM UNNEST([" +
			"STRUCT('shoe_1' AS product, 'store_2' AS store, 'Mountain View' AS city, 100 AS revenue, TRUE AS is_test), " +
			"('shoe_1', 'store_3', 'Sunnyvale', 200, TRUE), ('shoe_2', 'store_1', 'San Francisco', 85, TRUE), " +
			"('shoe_1', 'store_2', 'Mountain View', 100, FALSE), ('shoe_1', 'store_3', 'Sunnyvale', 150, FALSE), " +
			"('shoe_2', 'store_1', 'San Francisco', 175, FALSE)])",
		"ratio": "SELECT * FROM UNNEST([" +
			"STRUCT(1 AS dim1, 10 AS dim2, 20 AS dim3, 50 AS metric_a, 100 AS metric_b, TRUE AS t), " +
			"(1, 15, 30, 75, 200, TRUE), (5, 20, 40, 1, 10, TRUE), " +
			"(1, 10, 20, 40, 100, FALSE), (1, 15, 30, 80, 200, FALSE), (5, 20, 40, 3, 10, FALSE)])",
		"monthly": "SELECT * FROM UNNEST([" +
			"STRUCT(1 AS item_id, 'shirts' AS category, 150 AS sales, 'December' AS month, TRUE AS is_test), " +
			"(2, 'jackets', 100, 'January', TRUE), (3, 'hats', 50, 'April', TRUE), " +
			"(1, 'shirts', 125, 'February', FALSE), (4, 'hats', 10, 'February', FALSE), (2, 'jackets', 75, 'November', FALSE)])",
	})
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.ca` OPTIONS(MODEL_TYPE='CONTRIBUTION_ANALYSIS', CONTRIBUTION_METRIC='sum(revenue)', "+
		"DIMENSION_ID_COLS=['product', 'store', 'city'], IS_TEST_COL='is_test', MIN_APRIORI_SUPPORT=0.05) AS SELECT * FROM `%s.sales`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL summable: %v", err)
	}
	meta, err := c.Dataset(name).Model("ca").Metadata(ctx)
	if err != nil {
		t.Fatalf("Model.Metadata: %v", err)
	}
	if meta.Type != "CONTRIBUTION_ANALYSIS" {
		t.Errorf("model type %q; want CONTRIBUTION_ANALYSIS", meta.Type)
	}
	rows := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.GET_INSIGHTS(MODEL `%s.ca`)", name))
	if len(rows) != 21 {
		t.Fatalf("ML.GET_INSIGHTS: %d rows; want 21 (every segment and all)", len(rows))
	}
	key := func(r map[string]bigquery.Value) string {
		var cs []string
		for _, v := range r["contributors"].([]bigquery.Value) {
			cs = append(cs, v.(string))
		}
		return strings.Join(cs, ",")
	}
	byKey := map[string]map[string]bigquery.Value{}
	for i, r := range rows {
		byKey[key(r)] = r
		if i > 0 && r["contribution"].(float64) > rows[i-1]["contribution"].(float64) {
			t.Errorf("row %d is not ordered by contribution: %v", i, r)
		}
	}
	near := func(what string, got bigquery.Value, want float64) {
		t.Helper()
		g, ok := got.(float64)
		if !ok || math.Abs(g-want) > 1e-9*math.Max(1, math.Abs(want)) {
			t.Errorf("%s = %v; want %g", what, got, want)
		}
	}
	all := byKey["all"]
	if all == nil {
		t.Fatalf("no all row: %v", rows)
	}
	near("all metric_test", all["metric_test"], 385)
	near("all metric_control", all["metric_control"], 425)
	near("all difference", all["difference"], -40)
	near("all apriori_support", all["apriori_support"], 1)
	if all["product"] != nil {
		t.Errorf("all row's product %v; want NULL", all["product"])
	}
	s := byKey["product=shoe_1"]
	if s == nil || s["product"] != "shoe_1" || s["store"] != nil {
		t.Fatalf("product=shoe_1 row: %v", s)
	}
	expected := 250 * (85.0 / 175)
	near("difference", s["difference"], 50)
	near("relative_difference", s["relative_difference"], 0.2)
	near("unexpected_difference", s["unexpected_difference"], 300-expected)
	near("relative_unexpected_difference", s["relative_unexpected_difference"], (300-expected)/expected)
	near("apriori_support", s["apriori_support"], 300.0/385)
	near("contribution", s["contribution"], 50)
	if byKey["product=shoe_1,store=store_3,city=Sunnyvale"] == nil {
		t.Error("no three-dimension segment")
	}

	// PRUNE_REDUNDANT_INSIGHTS keeps the most descriptive of equal rows.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.pruned` OPTIONS(model_type='contribution_analysis', contribution_metric='SUM(revenue)', "+
		"is_test_col='is_test', top_k_insights_by_apriori_support=25, pruning_method='PRUNE_REDUNDANT_INSIGHTS') AS SELECT * FROM `%s.sales`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL pruned: %v", err)
	}
	for _, r := range mlQuery(t, h, c, fmt.Sprintf("SELECT contributors FROM ML.GET_INSIGHTS(MODEL `%s.pruned`)", name)) {
		if k := key(r); k == "store=store_3" || k == "city=Sunnyvale" {
			t.Errorf("redundant insight %s kept", k)
		}
	}

	// Summable ratio: the page's metric_test_over_complement example.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.ratio` OPTIONS(model_type='CONTRIBUTION_ANALYSIS', "+
		"contribution_metric='SUM(metric_a)/SUM(metric_b)', dimension_id_cols=['dim1','dim2','dim3'], is_test_col='t', min_apriori_support=0) "+
		"AS SELECT * FROM `%s.ratio`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL ratio: %v", err)
	}
	rr := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.GET_INSIGHTS(MODEL `%s.ratio`)", name))
	found := 0
	for _, r := range rr {
		for _, col := range []string{"metric_test_over_metric_control", "metric_test_over_complement", "metric_control_over_complement",
			"aumann_shapley_attribution", "apriori_support", "contribution"} {
			if _, ok := r[col]; !ok {
				t.Fatalf("ratio row lacks %s: %v", col, r)
			}
		}
		switch key(r) {
		case "dim1=1,dim2=10,dim3=20":
			near("first row's metric_test_over_complement", r["metric_test_over_complement"], 0.5/(76.0/210))
			found++
		case "dim1=5,dim2=20,dim3=40":
			near("third row's metric_test_over_complement", r["metric_test_over_complement"], 0.1/(125.0/300))
			found++
		case "all":
			if r["metric_test_over_complement"] != nil {
				t.Errorf("all row's metric_test_over_complement %v; want NULL", r["metric_test_over_complement"])
			}
			near("all aumann_shapley_attribution", r["aumann_shapley_attribution"], 126.0/310-123.0/310)
			found++
		}
	}
	if found != 3 {
		t.Errorf("ratio insights: found %d of the 3 checked rows in %v", found, rr)
	}

	// Summable by category.
	if err := mlRun(h, c, fmt.Sprintf("CREATE MODEL `%s.bycat` OPTIONS(model_type='CONTRIBUTION_ANALYSIS', "+
		"contribution_metric='SUM(sales)/COUNT(DISTINCT month)', dimension_id_cols=['item_id','category'], is_test_col='is_test') "+
		"AS SELECT * FROM `%s.monthly`", name, name)); err != nil {
		t.Fatalf("CREATE MODEL by category: %v", err)
	}
	cr := mlQuery(t, h, c, fmt.Sprintf("SELECT * FROM ML.GET_INSIGHTS(MODEL `%s.bycat`) WHERE ARRAY_TO_STRING(contributors, ',') = 'category=hats'", name))
	if len(cr) != 1 {
		t.Fatalf("category=hats rows: %v", cr)
	}
	near("hats metric_test", cr[0]["metric_test"], 50)
	near("hats metric_control", cr[0]["metric_control"], 10)
	near("hats metric_test_over_population", cr[0]["metric_test_over_population"], 0.5)
	near("hats metric_control_over_population", cr[0]["metric_control_over_population"], 10/105.0)

	// The documented option rules.
	for _, opts := range []string{
		"contribution_metric='AVG(revenue)', is_test_col='is_test'",
		"contribution_metric='SUM(revenue)'",
		"contribution_metric='SUM(revenue)', is_test_col='is_test', min_apriori_support=0.1, top_k_insights_by_apriori_support=5",
		"contribution_metric='SUM(revenue)', is_test_col='is_test', min_apriori_support=2",
		"contribution_metric='SUM(revenue)', is_test_col='is_test', dimension_id_cols=['product']",
		"contribution_metric='SUM(revenue)', is_test_col='product'",
	} {
		_, err := c.Query(fmt.Sprintf("CREATE MODEL `%s.bad` OPTIONS(model_type='CONTRIBUTION_ANALYSIS', %s) AS SELECT * FROM `%s.sales`", name, opts, name)).Read(ctx)
		wantHTTPStatus(t, opts, err, http.StatusBadRequest)
	}
	_, err = c.Query(fmt.Sprintf("SELECT * FROM ML.PREDICT(MODEL `%s.ca`, (SELECT * FROM `%s.sales`))", name, name)).Read(ctx)
	wantHTTPStatus(t, "ML.PREDICT of a contribution analysis model", err, http.StatusBadRequest)
}
