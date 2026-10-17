package bigqueryfront

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func sp(s string) *string { return &s }

// linearRows are rows of y = 3 + 2·x1 − x2 + (c == "b" ? 5 : 0).
func linearRows() ([]mlColumn, [][]*string) {
	cols := []mlColumn{{"x1", "INTEGER"}, {"x2", "FLOAT"}, {"c", "STRING"}, {"y", "FLOAT"}}
	var rows [][]*string
	for i := 0; i < 40; i++ {
		x1, x2 := float64(i%7), float64(i%5)*1.5
		c := "a"
		y := 3 + 2*x1 - x2
		if i%3 == 0 {
			c = "b"
			y += 5
		}
		rows = append(rows, []*string{sp(strconv.Itoa(int(x1))), sp(strconv.FormatFloat(x2, 'g', -1, 64)), sp(c), sp(strconv.FormatFloat(y, 'g', -1, 64))})
	}
	return cols, rows
}

func TestTrainLinearNormalEquationRecoversTheWeights(t *testing.T) {
	cols, rows := linearRows()
	o := defaultMLOptions()
	o.ModelType, o.LabelCols = "LINEAR_REG", []string{"y"}
	m, fail := trainModel(o, cols, rows)
	if fail != nil {
		t.Fatal(fail)
	}
	if m.Strategy != "NORMAL_EQUATION" || len(m.Iterations) != 1 {
		t.Fatalf("strategy %s, %d iterations", m.Strategy, len(m.Iterations))
	}
	if l := m.Iterations[0].Loss; l > 1e-12 {
		t.Errorf("loss %g, want 0 for an exact fit", l)
	}
	if v := *m.Eval["r2_score"]; math.Abs(v-1) > 1e-9 {
		t.Errorf("r2 %g", v)
	}
	// The unstandardized weights are the generating ones.
	x1, x2 := m.Features[0], m.Features[1]
	if w := x1.Weight / x1.Stddev; math.Abs(w-2) > 1e-9 {
		t.Errorf("x1 weight %g, want 2", w)
	}
	if w := x2.Weight / x2.Stddev; math.Abs(w+1) > 1e-9 {
		t.Errorf("x2 weight %g, want -1", w)
	}
	c := m.Features[2]
	if d := c.CategoryWeights[1] - c.CategoryWeights[0]; math.Abs(d-5) > 1e-9 {
		t.Errorf("b - a = %g, want 5", d)
	}
	got := m.predictRow(map[string]*string{"x1": sp("4"), "x2": sp("2"), "c": sp("b")})
	if math.Abs(got-(3+8-2+5)) > 1e-9 {
		t.Errorf("prediction %g, want 14", got)
	}
}

func TestTrainLinearGradientDescentConverges(t *testing.T) {
	cols, rows := linearRows()
	o := defaultMLOptions()
	o.ModelType, o.LabelCols, o.Strategy, o.EarlyStop, o.MaxIterations = "LINEAR_REG", []string{"y"}, "BATCH_GRADIENT_DESCENT", false, 200
	m, fail := trainModel(o, cols, rows)
	if fail != nil {
		t.Fatal(fail)
	}
	if len(m.Iterations) != 200 || m.Iterations[0].Index != 199 {
		t.Fatalf("%d iterations, newest %d", len(m.Iterations), m.Iterations[0].Index)
	}
	if l := m.Iterations[0].Loss; l > 1e-3 {
		t.Errorf("loss %g after 200 iterations", l)
	}
	for i := 1; i < len(m.Iterations); i++ {
		if m.Iterations[i-1].Loss > m.Iterations[i].Loss+1e-12 {
			t.Fatalf("loss rose at iteration %d", m.Iterations[i-1].Index)
		}
	}
	// Early stop ends sooner.
	o.EarlyStop = true
	m2, _ := trainModel(o, cols, rows)
	if len(m2.Iterations) >= 200 {
		t.Errorf("early stop ran %d iterations", len(m2.Iterations))
	}
}

func TestTrainLogistic(t *testing.T) {
	cols := []mlColumn{{"x", "FLOAT"}, {"label", "BOOLEAN"}}
	var rows [][]*string
	for i := 0; i < 60; i++ {
		x := float64(i)/10 - 3
		l := "false"
		if x > 0.25 || i%11 == 0 {
			l = "true"
		}
		rows = append(rows, []*string{sp(strconv.FormatFloat(x, 'g', -1, 64)), sp(l)})
	}
	o := defaultMLOptions()
	o.ModelType = "LOGISTIC_REG"
	m, fail := trainModel(o, cols, rows)
	if fail != nil {
		t.Fatal(fail)
	}
	if m.Classes[0] != "false" || m.Classes[1] != "true" || m.Strategy != "BATCH_GRADIENT_DESCENT" {
		t.Fatalf("classes %v, strategy %s", m.Classes, m.Strategy)
	}
	if m.Features[0].Weight <= 0 {
		t.Errorf("weight %g, want positive", m.Features[0].Weight)
	}
	if a := *m.Eval["accuracy"]; a < 0.85 {
		t.Errorf("accuracy %g", a)
	}
	if a := *m.Eval["roc_auc"]; a < 0.85 {
		t.Errorf("roc_auc %g", a)
	}
}

func TestTrainRefusals(t *testing.T) {
	cols := []mlColumn{{"x", "TIMESTAMP"}, {"label", "FLOAT"}}
	o := defaultMLOptions()
	o.ModelType = "LINEAR_REG"
	if _, f := trainModel(o, cols, [][]*string{{sp("1"), sp("2")}}); f == nil || f.code != 501 {
		t.Errorf("TIMESTAMP feature: %v", f)
	}
	cols = []mlColumn{{"x", "FLOAT"}, {"label", "STRING"}}
	o.ModelType = "LOGISTIC_REG"
	rows := [][]*string{{sp("1"), sp("a")}, {sp("2"), sp("b")}, {sp("3"), sp("c")}}
	if m, f := trainModel(o, cols, rows); f != nil || len(m.ClassWeights) != 3 {
		t.Errorf("multiclass: %v", f)
	}
	if _, f := trainModel(o, []mlColumn{{"x", "FLOAT"}}, rows); f == nil || f.code != 400 {
		t.Errorf("no label: %v", f)
	}
}

func TestParseModelDDL(t *testing.T) {
	sql := "CREATE OR REPLACE MODEL `p.ds.m` OPTIONS(model_type='linear_reg', input_label_cols=['y'], l2_reg=0.5, max_iterations=5) AS SELECT * FROM ds.t"
	toks, _ := lex(sql)
	d, is, code, msg := parseModelDDL(toks, sql)
	if !is || code != 0 {
		t.Fatalf("%v %d %s", is, code, msg)
	}
	if !d.replace || strings.Join(d.path, ".") != "p.ds.m" || d.opts.ModelType != "LINEAR_REG" || d.opts.L2 != 0.5 ||
		d.opts.MaxIterations != 5 || d.opts.LabelCols[0] != "y" || d.query != "SELECT * FROM ds.t" {
		t.Errorf("%+v", d)
	}
	for _, bad := range []string{
		"CREATE MODEL ds.m OPTIONS(model_type='dnn_linear_combined_regressor') AS SELECT 1",
		"CREATE MODEL ds.m OPTIONS(model_type='dnn_classifier', optimizer='ftrl') AS SELECT 1",
		"CREATE MODEL ds.m OPTIONS(model_type='dnn_regressor', activation_fn='crelu') AS SELECT 1",
		"CREATE MODEL ds.m OPTIONS(model_type='autoencoder', l1_reg_activation=0.1) AS SELECT 1",
		"CREATE MODEL ds.m OPTIONS(model_type='onnx', model_path='gs://b/m.onnx')",
		"CREATE MODEL ds.m OPTIONS(model_type='pca', num_principal_components=2, pca_solver='randomized') AS SELECT 1",
		"CREATE MODEL ds.m REMOTE WITH CONNECTION `us.c` OPTIONS(endpoint='gemini')",
		"CREATE MODEL ds.m OPTIONS(model_type='linear_reg', warm_start=true) AS SELECT 1",
	} {
		toks, _ := lex(bad)
		if _, is, code, _ := parseModelDDL(toks, bad); !is || code != 501 {
			t.Errorf("%s: %v %d", bad, is, code)
		}
	}
	toks, _ = lex("DROP MODEL IF EXISTS ds.m")
	if d, is, code, _ := parseModelDDL(toks, ""); !is || code != 0 || !d.drop || !d.ifExists {
		t.Errorf("drop: %+v", d)
	}
}

func TestFindMLCalls(t *testing.T) {
	sql := "SELECT * FROM ML.PREDICT(MODEL `ds.m`, (SELECT x FROM ds.t WHERE x > 1), STRUCT(0.7 AS threshold)) JOIN ML.WEIGHTS(MODEL ds.m) ON TRUE"
	calls, msg := findMLCalls(sql)
	if msg != "" || len(calls) != 2 {
		t.Fatalf("%v %s", calls, msg)
	}
	if calls[0].fn != "PREDICT" || calls[0].input != "SELECT x FROM ds.t WHERE x > 1" || calls[0].struc["threshold"] != "0.7" {
		t.Errorf("%+v", calls[0])
	}
	if calls[1].fn != "WEIGHTS" || sql[calls[1].pos:calls[1].end] != "ML.WEIGHTS(MODEL ds.m)" {
		t.Errorf("%+v", calls[1])
	}
	calls, _ = findMLCalls("SELECT * FROM ML.EVALUATE(MODEL ds.m, TABLE ds.t)")
	if len(calls) != 1 || calls[0].input != "SELECT * FROM `ds`.`t`" && !strings.HasPrefix(calls[0].input, "SELECT * FROM ") {
		t.Errorf("%+v", calls)
	}
}
