package bigqueryfront

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestTrainMulticlassLogistic(t *testing.T) {
	// Three well separated classes on x.
	cols := []mlColumn{{"x", "FLOAT"}, {"label", "STRING"}}
	var rows [][]*string
	for i := 0; i < 60; i++ {
		x := float64(i%3)*10 + float64(i%5)*0.1
		rows = append(rows, []*string{sp(strconv.FormatFloat(x, 'f', -1, 64)), sp([]string{"a", "b", "c"}[i%3])})
	}
	o := defaultMLOptions()
	o.ModelType = "LOGISTIC_REG"
	o.MaxIterations = 50
	m, f := trainModel(o, cols, rows)
	if f != nil {
		t.Fatal(f)
	}
	if a := *m.Eval["accuracy"]; a < 0.9 {
		t.Errorf("accuracy %v", a)
	}
	p := m.classProbs(map[string]*string{"x": sp("20")})
	if argmax(p) != 2 || math.Abs(p[0]+p[1]+p[2]-1) > 1e-9 {
		t.Errorf("probs %v", p)
	}
	if len(m.EvalY) != 60 {
		t.Errorf("eval rows %d", len(m.EvalY))
	}
}

func TestTrainKMeans(t *testing.T) {
	cols := []mlColumn{{"x", "FLOAT"}, {"y", "FLOAT"}, {"init", "BOOLEAN"}}
	var rows [][]*string
	for i := 0; i < 40; i++ {
		cx, cy := 0.0, 0.0
		if i%2 == 1 {
			cx, cy = 10, 10
		}
		init := "false"
		if i < 2 {
			init = "true"
		}
		rows = append(rows, []*string{sp(strconv.FormatFloat(cx+float64(i%4)*0.1, 'f', -1, 64)),
			sp(strconv.FormatFloat(cy-float64(i%3)*0.1, 'f', -1, 64)), sp(init)})
	}
	for _, init := range []string{"RANDOM", "KMEANS++", "CUSTOM"} {
		o := defaultMLOptions()
		o.ModelType, o.NumClusters, o.KMeansInit = "KMEANS", 2, init
		if init == "CUSTOM" {
			o.KMeansInitCol = "init"
		}
		c := cols
		r := rows
		if init != "CUSTOM" {
			c = cols[:2]
			r = nil
			for _, row := range rows {
				r = append(r, row[:2])
			}
		}
		m, f := trainModel(o, c, r)
		if f != nil {
			t.Fatalf("%s: %v", init, f)
		}
		if len(m.Centroids) != 2 || m.ClusterSizes[0] != 20 || m.ClusterSizes[1] != 20 || len(m.Features) != 2 {
			t.Errorf("%s: %+v %v", init, m.Centroids, m.ClusterSizes)
		}
		if *m.Eval["davies_bouldin_index"] > 0.1 {
			t.Errorf("%s: davies-bouldin %v", init, *m.Eval["davies_bouldin_index"])
		}
	}
	o := defaultMLOptions()
	o.ModelType, o.NumClusters, o.KMeansInit, o.KMeansInitCol = "KMEANS", 3, "CUSTOM", "init"
	if _, f := trainModel(o, cols, rows); f == nil || f.code != 400 {
		t.Errorf("custom init with 2 TRUE rows for 3 clusters: %v", f)
	}
}

func TestTrainPCA(t *testing.T) {
	// y = 2x, z constant noise: one component explains nearly everything.
	cols := []mlColumn{{"x", "FLOAT"}, {"y", "FLOAT"}}
	var rows [][]*string
	for i := 0; i < 20; i++ {
		x := float64(i)
		rows = append(rows, []*string{sp(strconv.FormatFloat(x, 'f', -1, 64)), sp(strconv.FormatFloat(2*x+float64(i%2)*0.01, 'f', -1, 64))})
	}
	o := defaultMLOptions()
	o.ModelType, o.NumPC = "PCA", 1
	m, f := trainModel(o, cols, rows)
	if f != nil {
		t.Fatal(f)
	}
	v := m.Components[0]
	if math.Abs(v[0]-math.Sqrt(0.5)) > 1e-3 || math.Abs(v[1]-math.Sqrt(0.5)) > 1e-3 {
		t.Errorf("component %v", v)
	}
	if r := *m.Eval["total_explained_variance_ratio"]; r < 0.999 {
		t.Errorf("ratio %v", r)
	}
	// Two standardized columns: the eigenvalues sum to 2.
	if math.Abs(m.TotalVariance-2) > 1e-9 {
		t.Errorf("total variance %v", m.TotalVariance)
	}
	o.NumPC, o.PCAVarRatio = 0, 0.5
	if m, f = trainModel(o, cols, rows); f != nil || len(m.Components) != 1 {
		t.Errorf("by ratio: %v %d", f, len(m.Components))
	}
	o.NumPC, o.PCAVarRatio = 1, 0
	if _, f := trainModel(o, []mlColumn{{"x", "FLOAT"}, {"c", "STRING"}}, rows); f == nil || f.code != 501 {
		t.Errorf("categorical PCA: %v", f)
	}
}

func TestColumnNamesAndTransform(t *testing.T) {
	if got := strings.Join(columnNames([]string{"0", "1", "a b", "a_b", "Cash"}), ","); got != "_0,_1,a_b,a_b_1,Cash" {
		t.Errorf("columnNames: %s", got)
	}
	sql := "CREATE MODEL ds.m TRANSFORM(x * 2 AS x2, * EXCEPT (x), label) OPTIONS(model_type='linear_reg') AS SELECT 1"
	toks, _ := lex(sql)
	d, _, code, msg := parseModelDDL(toks, sql)
	if code != 0 || len(d.transform) != 3 || d.transform[0] != "x * 2 AS x2" {
		t.Fatalf("%d %s %q", code, msg, d.transform)
	}
	items, why := expandTransform(d.transform, []mlColumn{{"x", "FLOAT"}, {"z", "FLOAT"}, {"label", "FLOAT"}})
	if why != "" || strings.Join(items, "|") != "x * 2 AS x2|`z`|`label`|label" {
		t.Errorf("%q %s", items, why)
	}
	md := bqModel{Transform: []string{"x * 2 AS x2", "`label`"}, Trained: mlTrained{Label: "label"}}
	if p := md.predictTransform(); len(p) != 1 {
		t.Errorf("predictTransform %q", p)
	}
	gtoks, _ := lex("GENERATE_ARRAY(0.1, 0.3, 0.1)")
	th, msg := mlGenerateArray(gtoks)
	if msg != "" || len(th) != 3 || th[2] != 0.3 {
		t.Errorf("%v %s", th, msg)
	}
}
