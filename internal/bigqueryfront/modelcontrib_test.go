package bigqueryfront

import (
	"math"
	"strings"
	"testing"
)

// contribDDL parses a CREATE MODEL of a contribution analysis model.
func contribDDL(t *testing.T, opts string) (mlOptions, int, string) {
	t.Helper()
	sql := "CREATE MODEL ds.m OPTIONS(model_type='CONTRIBUTION_ANALYSIS'" + opts + ") AS SELECT 1"
	toks, ok := lex(sql)
	if !ok {
		t.Fatal("lex")
	}
	d, is, code, msg := parseModelDDL(toks, sql)
	if !is {
		t.Fatal("not a model statement")
	}
	if code == 0 {
		code, msg = checkModelOptions(d.opts)
	}
	return d.opts, code, msg
}

// salesRows are the summable example of the CREATE MODEL page.
func salesRows() ([]mlColumn, [][]*string) {
	cols := []mlColumn{{"product", "STRING"}, {"store", "STRING"}, {"city", "STRING"}, {"revenue", "INTEGER"}, {"is_test", "BOOLEAN"}}
	data := [][]string{
		{"shoe_1", "store_2", "Mountain View", "100", "true"},
		{"shoe_1", "store_3", "Sunnyvale", "200", "true"},
		{"shoe_2", "store_1", "San Francisco", "85", "true"},
		{"shoe_1", "store_2", "Mountain View", "100", "false"},
		{"shoe_1", "store_3", "Sunnyvale", "150", "false"},
		{"shoe_2", "store_1", "San Francisco", "175", "false"},
	}
	var rows [][]*string
	for _, d := range data {
		var r []*string
		for _, v := range d {
			r = append(r, sp(v))
		}
		rows = append(rows, r)
	}
	return cols, rows
}

func insightOf(t *testing.T, ins []contribInsight, contributors ...string) contribInsight {
	t.Helper()
	for _, in := range ins {
		if strings.Join(in.Contributors, ",") == strings.Join(contributors, ",") {
			return in
		}
	}
	t.Fatalf("no insight %v", contributors)
	return contribInsight{}
}

func near(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Errorf("%s = %v, want %g", what, got, want)
	}
}

func TestContribSummableInsights(t *testing.T) {
	o, code, msg := contribDDL(t, ", contribution_metric='sum(revenue)', dimension_id_cols=['product','store','city'], is_test_col='is_test', min_apriori_support=0")
	if code != 0 {
		t.Fatal(code, msg)
	}
	cols, rows := salesRows()
	tr, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	ins := tr.Contrib.Insights
	all := insightOf(t, ins, "all")
	near(t, "all metric_test", all.Metrics["metric_test"], 385)
	near(t, "all metric_control", all.Metrics["metric_control"], 425)
	near(t, "all difference", all.Metrics["difference"], -40)
	// The "all" row's unexpected difference is its difference (tutorial).
	near(t, "all unexpected_difference", all.Metrics["unexpected_difference"], -40)
	near(t, "all apriori_support", all.Metrics["apriori_support"], 1)
	s := insightOf(t, ins, "product=shoe_1")
	near(t, "difference", s.Metrics["difference"], 50)
	near(t, "relative_difference", s.Metrics["relative_difference"], 50.0/250)
	expected := 250 * (85.0 / 175)
	near(t, "unexpected_difference", s.Metrics["unexpected_difference"], 300-expected)
	near(t, "relative_unexpected_difference", s.Metrics["relative_unexpected_difference"], (300-expected)/expected)
	near(t, "apriori_support", s.Metrics["apriori_support"], math.Max(300.0/385, 250.0/425))
	near(t, "contribution", s.Metrics["contribution"], 50)
	// Segments of several dimensions, and ordered by contribution.
	insightOf(t, ins, "product=shoe_1", "store=store_3", "city=Sunnyvale")
	for i := 1; i < len(ins); i++ {
		if *ins[i].Metrics["contribution"] > *ins[i-1].Metrics["contribution"] {
			t.Fatalf("row %d not ordered by contribution", i)
		}
	}
	// Every combination present: the 3 distinct rows' 7 dimension subsets
	// each, product=shoe_1 shared by two, and "all".
	if len(ins) != 1+3*7-1 {
		t.Errorf("%d insights, want 21", len(ins))
	}
	// PRUNE_REDUNDANT_INSIGHTS keeps the most descriptive of equal rows:
	// store=store_3 equals city=Sunnyvale equals the three-way segment.
	o.Contrib.Pruning = "PRUNE_REDUNDANT_INSIGHTS"
	pr, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	for _, in := range pr.Contrib.Insights {
		if j := strings.Join(in.Contributors, ","); j == "store=store_3" || j == "city=Sunnyvale" || j == "store=store_3,city=Sunnyvale" {
			t.Errorf("redundant %s kept", j)
		}
	}
	insightOf(t, pr.Contrib.Insights, "product=shoe_1", "store=store_3", "city=Sunnyvale")
	insightOf(t, pr.Contrib.Insights, "all")
	// top_k: the k segments of the greatest support, and "all".
	o.Contrib.Pruning, o.Contrib.TopK = "NO_PRUNING", 2
	tk, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	if len(tk.Contrib.Insights) != 3 {
		t.Fatalf("top 2: %d insights", len(tk.Contrib.Insights))
	}
	insightOf(t, tk.Contrib.Insights, "product=shoe_1")
	// The minimum support leaves out the small segments.
	o.Contrib.TopK, o.Contrib.MinSupport = 0, 0.5
	ms, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	for _, in := range ms.Contrib.Insights {
		if in.Support < 0.5 {
			t.Errorf("%v support %g below 0.5", in.Contributors, in.Support)
		}
	}
}

// TestContribRatioInsights checks metric_test_over_complement against the
// GET_INSIGHTS page's worked example, and that the Aumann-Shapley
// attributions of a partition add up to the population's change.
func TestContribRatioInsights(t *testing.T) {
	o, code, msg := contribDDL(t, ", contribution_metric='SUM(metric_a) / SUM(metric_b)', is_test_col='t', min_apriori_support=0")
	if code != 0 {
		t.Fatal(code, msg)
	}
	cols := []mlColumn{{"dim1", "INTEGER"}, {"dim2", "INTEGER"}, {"dim3", "INTEGER"}, {"metric_a", "INTEGER"}, {"metric_b", "INTEGER"}, {"t", "BOOLEAN"}}
	data := [][]string{
		{"1", "10", "20", "50", "100", "true"}, {"1", "15", "30", "75", "200", "true"}, {"5", "20", "40", "1", "10", "true"},
		{"1", "10", "20", "40", "100", "false"}, {"1", "15", "30", "80", "200", "false"}, {"5", "20", "40", "3", "10", "false"},
	}
	var rows [][]*string
	for _, d := range data {
		var r []*string
		for _, v := range d {
			r = append(r, sp(v))
		}
		rows = append(rows, r)
	}
	tr, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	if got := tr.Contrib.Dims; strings.Join(got, ",") != "dim1,dim2,dim3" {
		t.Fatalf("dimensions %v", got)
	}
	ins := tr.Contrib.Insights
	first := insightOf(t, ins, "dim1=1", "dim2=10", "dim3=20")
	near(t, "first metric_test_over_complement", first.Metrics["metric_test_over_complement"], 0.5/(76.0/210))
	third := insightOf(t, ins, "dim1=5", "dim2=20", "dim3=40")
	near(t, "third metric_test_over_complement", third.Metrics["metric_test_over_complement"], 0.1/(125.0/300))
	all := insightOf(t, ins, "all")
	if all.Metrics["metric_test_over_complement"] != nil {
		t.Error("all metric_test_over_complement is not NULL")
	}
	change := 126.0/310 - 123.0/310
	near(t, "all aumann_shapley_attribution", all.Metrics["aumann_shapley_attribution"], change)
	var sum float64
	for _, c := range []string{"dim2=10", "dim2=15", "dim2=20"} {
		sum += *insightOf(t, ins, c).Metrics["aumann_shapley_attribution"]
	}
	if math.Abs(sum-change) > 1e-9 {
		t.Errorf("attributions add up to %g, want %g", sum, change)
	}
	near(t, "metric_test_over_metric_control", first.Metrics["metric_test_over_metric_control"], 0.5/0.4)
}

func TestContribByCategoryInsights(t *testing.T) {
	o, code, msg := contribDDL(t, ", contribution_metric='SUM(sales)/COUNT(DISTINCT month)', dimension_id_cols=['item_id','category'], is_test_col='is_test'")
	if code != 0 {
		t.Fatal(code, msg)
	}
	cols := []mlColumn{{"item_id", "INTEGER"}, {"category", "STRING"}, {"sales", "INTEGER"}, {"month", "STRING"}, {"is_test", "BOOLEAN"}}
	data := [][]string{
		{"1", "shirts", "150", "December", "true"}, {"2", "jackets", "100", "January", "true"}, {"3", "hats", "50", "April", "true"},
		{"1", "shirts", "125", "February", "false"}, {"4", "hats", "10", "February", "false"}, {"2", "jackets", "75", "November", "false"},
	}
	var rows [][]*string
	for _, d := range data {
		var r []*string
		for _, v := range d {
			r = append(r, sp(v))
		}
		rows = append(rows, r)
	}
	tr, fe := trainModel(o, cols, rows, nil)
	if fe != nil {
		t.Fatal(fe)
	}
	all := insightOf(t, tr.Contrib.Insights, "all")
	near(t, "all metric_test", all.Metrics["metric_test"], 300.0/3)
	near(t, "all metric_control", all.Metrics["metric_control"], 210.0/2)
	h := insightOf(t, tr.Contrib.Insights, "category=hats")
	near(t, "hats metric_test", h.Metrics["metric_test"], 50)
	near(t, "hats metric_control", h.Metrics["metric_control"], 10)
	near(t, "hats metric_test_over_population", h.Metrics["metric_test_over_population"], 50/100.0)
	near(t, "hats apriori_support", h.Metrics["apriori_support"], 50.0/300)
	sub, c, why := front{}.contribSubquery(nil, queryOptions{}, bqModel{Options: o, Trained: tr}, mlCall{fn: "GET_INSIGHTS"})
	if c != 0 || !strings.Contains(sub, "contributors ARRAY<STRING>, `item_id` INT64, `category` STRING, metric_test FLOAT64") ||
		!strings.Contains(sub, "metric_control_over_population FLOAT64, apriori_support FLOAT64, contribution FLOAT64") {
		t.Fatalf("%d %s %s", c, why, sub)
	}
}

func TestContribOptionValidation(t *testing.T) {
	for _, tc := range []struct{ opts, want string }{
		{", is_test_col='t'", "contribution_metric is required"},
		{", contribution_metric='SUM(x)'", "is_test_col is required"},
		{", contribution_metric='AVG(x)', is_test_col='t'", "Invalid contribution_metric"},
		{", contribution_metric='SUM(AVG(x))', is_test_col='t'", "Invalid contribution_metric"},
		{", contribution_metric='SUM(x)', is_test_col='t', min_apriori_support=1.5", "[0, 1]"},
		{", contribution_metric='SUM(x)', is_test_col='t', top_k_insights_by_apriori_support=0", "1,000,000"},
		{", contribution_metric='SUM(x)', is_test_col='t', min_apriori_support=0.1, top_k_insights_by_apriori_support=5", "cannot be used together"},
		{", contribution_metric='SUM(x)', is_test_col='t', pruning_method='SOME'", "pruning_method"},
		{", contribution_metric='SUM(x)', is_test_col='t', l2_reg=1", "not supported for model type CONTRIBUTION_ANALYSIS"},
		{", contribution_metric='SUM(x)', is_test_col='t', min_apriori_support=0, dimension_id_cols=['a','b','c','d','e','f','g','h','i','j','k','l','m','n']", "at most 13"},
	} {
		_, code, msg := contribDDL(t, tc.opts)
		if code != 400 || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: %d %s, want 400 %q", tc.opts, code, msg, tc.want)
		}
	}
	// Training checks the columns.
	cols, rows := salesRows()
	o, _, _ := contribDDL(t, ", contribution_metric='SUM(revenue)', dimension_id_cols=['product'], is_test_col='is_test'")
	if _, fe := trainModel(o, cols, rows, nil); fe == nil || !strings.Contains(fe.msg, "not referenced") {
		t.Errorf("extra columns: %v", fe)
	}
	o, _, _ = contribDDL(t, ", contribution_metric='SUM(revenue)', is_test_col='product'")
	if _, fe := trainModel(o, cols, rows, nil); fe == nil || !strings.Contains(fe.msg, "BOOL") {
		t.Errorf("is_test_col type: %v", fe)
	}
	rows[0][3] = sp("-1")
	o, _, _ = contribDDL(t, ", contribution_metric='SUM(revenue)', is_test_col='is_test'")
	if _, fe := trainModel(o, cols, rows, nil); fe == nil || !strings.Contains(fe.msg, "non-negative") {
		t.Errorf("negative metric: %v", fe)
	}
	o, _, _ = contribDDL(t, ", contribution_metric='SUM(revenue)', is_test_col='is_test', min_apriori_support=0")
	if _, fe := trainModel(o, cols, rows, nil); fe != nil {
		t.Errorf("negative metric with min_apriori_support 0: %v", fe)
	}
	// ML.GET_INSIGHTS of another model type, and another function of this.
	if _, c, _ := (front{}).contribSubquery(nil, queryOptions{}, bqModel{Options: mlOptions{ModelType: "LINEAR_REG"}}, mlCall{fn: "GET_INSIGHTS"}); c != 400 {
		t.Errorf("GET_INSIGHTS of LINEAR_REG: %d", c)
	}
	if _, c, _ := (front{}).contribSubquery(nil, queryOptions{}, bqModel{Options: o}, mlCall{fn: "PREDICT"}); c != 400 {
		t.Errorf("PREDICT of CONTRIBUTION_ANALYSIS: %d", c)
	}
}
