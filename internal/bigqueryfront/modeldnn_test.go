package bigqueryfront

import (
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// TestDNNForwardByHand checks the network's forward pass against a small
// network computed by hand: inputs (3, 1) and (1, 2), hidden layer
// W1 = [[1, -1], [0.5, 2]], b1 = [0, -1], RELU, output W2 = [[2, -3]],
// b2 = [0.5]. (3, 1): hidden (2, 2.5), output 0.5 + 4 - 7.5 = -3.
// (1, 2): hidden (relu(-1) = 0, 3.5), output 0.5 - 10.5 = -10. With TANH
// the first is 0.5 + 2·tanh(2) - 3·tanh(2.5).
func TestDNNForwardByHand(t *testing.T) {
	d := &dnnTrained{Activation: "RELU", Layers: []dnnLayer{
		{W: [][]float64{{1, -1}, {0.5, 2}}, B: []float64{0, -1}},
		{W: [][]float64{{2, -3}}, B: []float64{0.5}},
	}}
	for _, c := range []struct {
		x    []float64
		want float64
	}{{[]float64{3, 1}, -3}, {[]float64{1, 2}, -10}} {
		if got := d.forward(c.x)[0]; math.Abs(got-c.want) > 1e-12 {
			t.Errorf("forward(%v) = %v, want %v", c.x, got, c.want)
		}
	}
	d.Activation = "TANH"
	want := 0.5 + 2*math.Tanh(2) - 3*math.Tanh(2.5)
	if got := d.forward([]float64{3, 1})[0]; math.Abs(got-want) > 1e-12 {
		t.Errorf("TANH forward = %v, want %v", got, want)
	}
	from, added := d.dnnFrom("src", []string{"a", "b"})
	if len(added) != 3 || added[2] != "_cbml_o0" || !strings.Contains(from, "FROM src") {
		t.Errorf("dnnFrom: %v, %s", added, from)
	}
}

// TestDNNOneStepByHand checks one SGD step of trainDNN against the
// gradient computed independently, by central differences of the batch's
// mean squared error at the initial weights (Glorot uniform from the
// seed 1548, drawn as trainDNN draws them): one pass, one batch of both
// rows, so the weights after are w0 - learn_rate · ∇loss(w0).
func TestDNNOneStepByHand(t *testing.T) {
	X := [][]float64{{1, -0.5}, {1, 1.5}}
	Y := []float64{1, -2}
	o := defaultMLOptions()
	o.ModelType, o.Optimizer, o.Activation, o.LearnRate, o.MaxIterations = "DNN_REGRESSOR", "SGD", "TANH", 0.1, 1
	o.HiddenUnits = []int{2}
	var tr mlTrained
	tr.trainDNN(o, X, Y, nil, nil, false, 1)

	// The initial weights, drawn as trainDNN draws them.
	rng := rand.New(rand.NewSource(1548))
	init := &dnnTrained{Activation: "TANH"}
	sizes := []int{1, 2, 1}
	for l := 1; l < len(sizes); l++ {
		limit := math.Sqrt(6 / float64(sizes[l-1]+sizes[l]))
		ly := dnnLayer{W: make([][]float64, sizes[l]), B: make([]float64, sizes[l])}
		for j := range ly.W {
			ly.W[j] = make([]float64, sizes[l-1])
			for i := range ly.W[j] {
				ly.W[j][i] = (2*rng.Float64() - 1) * limit
			}
		}
		init.Layers = append(init.Layers, ly)
	}
	loss := func(d *dnnTrained) float64 {
		var s float64
		for i, x := range X {
			z := d.forward(x[1:])[0]
			s += (z - Y[i]) * (z - Y[i])
		}
		return s / float64(len(X))
	}
	const h = 1e-6
	for l, ly := range init.Layers {
		for j := range ly.B {
			for i := 0; i <= len(ly.W[j]); i++ {
				p := func(d *dnnTrained) *float64 {
					if i < len(d.Layers[l].W[j]) {
						return &d.Layers[l].W[j][i]
					}
					return &d.Layers[l].B[j]
				}
				w0 := *p(init)
				*p(init) = w0 + h
				up := loss(init)
				*p(init) = w0 - h
				down := loss(init)
				*p(init) = w0
				want := w0 - o.LearnRate*(up-down)/(2*h)
				if got := *p(tr.DNN); math.Abs(got-want) > 1e-7 {
					t.Errorf("layer %d unit %d weight %d after one step: %v, want %v", l, j, i, got, want)
				}
			}
		}
	}
}

// TestDNNTrains: a DNN regressor learns y = 2x from 40 rows, the same each
// run, and a classifier separates two classes.
func TestDNNTrains(t *testing.T) {
	var rows [][]*string
	for i := 0; i < 40; i++ {
		x := float64(i)/10 - 2
		rows = append(rows, []*string{sp(strconv.FormatFloat(x, 'g', -1, 64)), sp(strconv.FormatFloat(2*x, 'g', -1, 64))})
	}
	cols := []mlColumn{{"x", "FLOAT"}, {"label", "FLOAT"}}
	o := defaultMLOptions()
	o.ModelType = "DNN_REGRESSOR"
	o.Set = []string{"model_type"}
	applyDNNDefaults(&o)
	o.LearnRate, o.MaxIterations, o.EarlyStop, o.HiddenUnits = 0.05, 50, false, []int{8}
	a, fe := trainModel(o, cols, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	b, _ := trainModel(o, cols, rows)
	if a.Iterations[0].Loss != b.Iterations[0].Loss {
		t.Errorf("two runs differ: %v, %v", a.Iterations[0].Loss, b.Iterations[0].Loss)
	}
	if mse := *a.Eval["mean_squared_error"]; mse > 0.1 {
		t.Errorf("mean squared error %v, want under 0.1", mse)
	}
	if got := a.predictRow(map[string]*string{"x": sp("1")}); math.Abs(got-2) > 0.4 {
		t.Errorf("predict(1) = %v, want about 2", got)
	}
	if len(a.Iterations) != 50 || a.Iterations[0].Index != 49 {
		t.Errorf("iterations %d, newest %d", len(a.Iterations), a.Iterations[0].Index)
	}
	var crows [][]*string
	for i := 0; i < 40; i++ {
		l := "a"
		if i >= 20 {
			l = "b"
		}
		crows = append(crows, []*string{sp(strconv.Itoa(i)), sp(l)})
	}
	o.ModelType = "DNN_CLASSIFIER"
	c, fe := trainModel(o, []mlColumn{{"x", "FLOAT"}, {"label", "STRING"}}, crows)
	if fe != nil {
		t.Fatal(fe)
	}
	if acc := *c.Eval["accuracy"]; acc < 0.9 {
		t.Errorf("classifier accuracy %v", acc)
	}
	// The documented default hidden layer: min(128, rows / (10 (inputs + outputs))), at least 1.
	o.HiddenUnits = nil
	c, _ = trainModel(o, []mlColumn{{"x", "FLOAT"}, {"label", "STRING"}}, crows)
	if n := len(c.DNN.Layers[0].B); n != 2 {
		t.Errorf("default hidden units %d, want 40 / (10 · 2) = 2", n)
	}
}

// TestAutoencoderTrains: an autoencoder of hidden_units [4, 1, 4] learns to
// reconstruct two perfectly correlated columns through its one latent
// unit, the middle layer.
func TestAutoencoderTrains(t *testing.T) {
	var rows [][]*string
	for i := 0; i < 60; i++ {
		x := strconv.Itoa(i)
		rows = append(rows, []*string{sp(x), sp(strconv.Itoa(2 * i))})
	}
	o := defaultMLOptions()
	o.ModelType = "AUTOENCODER"
	applyDNNDefaults(&o)
	o.HiddenUnits, o.Activation, o.LearnRate, o.MaxIterations, o.EarlyStop = []int{4, 1, 4}, "TANH", 0.01, 100, false
	a, fe := trainModel(o, []mlColumn{{"a", "INT64"}, {"b", "INT64"}}, rows)
	if fe != nil {
		t.Fatal(fe)
	}
	if a.DNN.latentLayer() != 1 || len(a.DNN.Layers[1].B) != 1 || len(a.DNN.Layers[3].B) != 2 {
		t.Fatalf("layers: latent %d, sizes %d %d", a.DNN.latentLayer(), len(a.DNN.Layers[1].B), len(a.DNN.Layers[3].B))
	}
	if mse := *a.Eval["mean_squared_error"]; mse > 0.05 {
		t.Errorf("reconstruction mean squared error %v, want under 0.05", mse)
	}
	if a.Eval["median_absolute_error"] != nil {
		t.Errorf("an autoencoder's metrics are its three errors: %v", a.Eval)
	}
	if _, fe := trainModel(mlOptions{ModelType: "AUTOENCODER", LabelCols: []string{"a"}}, []mlColumn{{"a", "INT64"}}, rows); fe == nil || fe.code != 400 {
		t.Errorf("input_label_cols of an autoencoder: %v, want 400", fe)
	}
}

// TestParseDNNOptions: a DNN's options, its defaults, and the
// combinations BigQuery documents as invalid.
func TestParseDNNOptions(t *testing.T) {
	sql := "CREATE MODEL ds.m OPTIONS(model_type='dnn_classifier', hidden_units=[8, 4], activation_fn='tanh', batch_size=16, dropout=0.2, optimizer='sgd', l2_reg=0.1) AS SELECT 1"
	toks, _ := lex(sql)
	d, _, code, msg := parseModelDDL(toks, sql)
	if code != 0 {
		t.Fatalf("%d %s", code, msg)
	}
	o := d.opts
	if len(o.HiddenUnits) != 2 || o.HiddenUnits[1] != 4 || o.Activation != "TANH" || o.BatchSize != 16 || o.Dropout != 0.2 ||
		o.Optimizer != "SGD" || o.L2 != 0.1 || o.LearnRate != 0.001 {
		t.Errorf("%+v", o)
	}
	sql = "CREATE MODEL ds.m OPTIONS(model_type='dnn_regressor') AS SELECT 1"
	toks, _ = lex(sql)
	if d, _, _, _ = parseModelDDL(toks, sql); d.opts.Optimizer != "ADAM" || d.opts.Activation != "RELU" || d.opts.LearnRate != 0.001 {
		t.Errorf("defaults: %+v", d.opts)
	}
	for _, bad := range []string{
		"CREATE MODEL ds.m OPTIONS(model_type='dnn_regressor', l2_reg=0.1) AS SELECT 1", // ADAM
		"CREATE MODEL ds.m OPTIONS(model_type='dnn_regressor', batch_size=0) AS SELECT 1",
		"CREATE MODEL ds.m OPTIONS(model_type='autoencoder', input_label_cols=['x']) AS SELECT 1",
	} {
		toks, _ := lex(bad)
		if _, _, code, _ := parseModelDDL(toks, bad); code != 400 {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
}
