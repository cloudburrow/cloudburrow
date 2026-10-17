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
	_ = tr.trainDNN(o, X, Y, nil, nil, false, 1)

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

// dnnInitByHand draws a network's initial weights as trainDNN does
// (Glorot uniform from the seed 1548, a layer's fan-in the width of the
// one before it).
func dnnInitByHand(fn string, sizes []int) *dnnTrained {
	rng := rand.New(rand.NewSource(1548))
	d := &dnnTrained{Activation: fn}
	for l := 1; l < len(sizes); l++ {
		fanIn := sizes[l-1]
		if l > 1 {
			fanIn = dnnWidth(fn, sizes[l-1])
		}
		limit := math.Sqrt(6 / float64(fanIn+sizes[l]))
		ly := dnnLayer{W: make([][]float64, sizes[l]), B: make([]float64, sizes[l])}
		for j := range ly.W {
			ly.W[j] = make([]float64, fanIn)
			for i := range ly.W[j] {
				ly.W[j][i] = (2*rng.Float64() - 1) * limit
			}
		}
		d.Layers = append(d.Layers, ly)
	}
	return d
}

// eachWeight calls f with a pointer to each weight and bias of d, and the
// same one of e.
func eachWeight(d, e *dnnTrained, f func(name string, p, q *float64)) {
	for l, ly := range d.Layers {
		for j := range ly.B {
			for i := range ly.W[j] {
				f(strconv.Itoa(l)+"/"+strconv.Itoa(j)+"/"+strconv.Itoa(i), &ly.W[j][i], &e.Layers[l].W[j][i])
			}
			f(strconv.Itoa(l)+"/"+strconv.Itoa(j)+"/b", &ly.B[j], &e.Layers[l].B[j])
		}
	}
}

// TestDNNOptionsOneStepByHand (#1562): one SGD step with CRELU, with
// class weights (a binary classifier, its rows' log loss weighted), and
// of an autoencoder with l1_reg_activation, each against the gradient by
// central differences of the loss written out here; and one FTRL step
// against TensorFlow's update in closed form.
func TestDNNOptionsOneStepByHand(t *testing.T) {
	X := [][]float64{{1, -0.5, 0.25}, {1, 1.5, -1}, {1, 0.75, 0.5}}
	const h = 1e-6
	check := func(what string, o mlOptions, Y []float64, classifier bool, sizes []int, loss func(d *dnnTrained) float64, classes []string) {
		t.Helper()
		tr := mlTrained{Classes: classes}
		if fe := tr.trainDNN(o, X, Y, nil, nil, classifier, 1); fe != nil {
			t.Fatalf("%s: %v", what, fe.msg)
		}
		init := dnnInitByHand(o.Activation, sizes)
		eachWeight(init, tr.DNN, func(name string, p, q *float64) {
			w0 := *p
			*p = w0 + h
			up := loss(init)
			*p = w0 - h
			down := loss(init)
			*p = w0
			if want := w0 - o.LearnRate*(up-down)/(2*h); math.Abs(*q-want) > 1e-7 {
				t.Errorf("%s: weight %s after one step: %v, want %v", what, name, *q, want)
			}
		})
	}
	base := func(typ string) mlOptions {
		o := defaultMLOptions()
		o.ModelType, o.Optimizer, o.LearnRate, o.MaxIterations = typ, "SGD", 0.1, 1
		return o
	}

	// CRELU: a hidden layer of 2 units is 4 wide.
	o := base("DNN_REGRESSOR")
	o.Activation, o.HiddenUnits = "CRELU", []int{2}
	Y := []float64{1, -2, 0.5}
	check("CRELU", o, Y, false, []int{2, 2, 1}, func(d *dnnTrained) float64 {
		var s float64
		for i, x := range X {
			l0 := d.Layers[0]
			var hid []float64
			for j := range l0.B {
				hid = append(hid, math.Max(l0.B[j]+dot(l0.W[j], x[1:]), 0))
			}
			for j := range l0.B {
				hid = append(hid, math.Max(-(l0.B[j]+dot(l0.W[j], x[1:])), 0))
			}
			z := d.Layers[1].B[0] + dot(d.Layers[1].W[0], hid)
			s += (z - Y[i]) * (z - Y[i])
		}
		return s / float64(len(X))
	}, nil)
	d := dnnInitByHand("CRELU", []int{2, 2, 1})
	from, added := d.dnnFrom("src", []string{"a", "b"})
	if len(added) != 5 || !strings.Contains(from, "_cbml_h0_3") {
		t.Errorf("CRELU dnnFrom: %v %s", added, from)
	}

	// Class weights: label "no" 0.25, "yes" 3.
	o = base("DNN_CLASSIFIER")
	o.Activation, o.HiddenUnits = "TANH", []int{2}
	o.ClassWeights = map[string]float64{"no": 0.25, "yes": 3}
	Yc := []float64{1, 0, 1}
	cw := []float64{0.25, 3}
	check("class weights", o, Yc, true, []int{2, 2, 1}, func(d *dnnTrained) float64 {
		var s float64
		for i, x := range X {
			q := sigmoid(d.forward(x[1:])[0])
			s += -cw[int(Yc[i])] * (Yc[i]*math.Log(q) + (1-Yc[i])*math.Log(1-q))
		}
		return s / float64(len(X))
	}, []string{"no", "yes"})
	// auto_class_weights: 3 / (rows of the class · 2).
	o.ClassWeights, o.AutoClassWeights = nil, true
	aw := []float64{3.0 / 2, 3.0 / 4}
	check("auto class weights", o, Yc, true, []int{2, 2, 1}, func(d *dnnTrained) float64 {
		var s float64
		for i, x := range X {
			q := sigmoid(d.forward(x[1:])[0])
			s += -aw[int(Yc[i])] * (Yc[i]*math.Log(q) + (1-Yc[i])*math.Log(1-q))
		}
		return s / float64(len(X))
	}, []string{"no", "yes"})
	o.AutoClassWeights, o.ClassWeights = false, map[string]float64{"no": 1}
	var tr mlTrained
	tr.Classes = []string{"no", "yes"}
	if fe := tr.trainDNN(o, X, Yc, nil, nil, true, 1); fe == nil || fe.code != 400 {
		t.Errorf("a class with no weight: %v", fe)
	}

	// An autoencoder's l1_reg_activation on its latent layer, the middle
	// of [3, 1, 3].
	o = base("AUTOENCODER")
	o.Activation, o.HiddenUnits, o.L1Activation = "TANH", []int{3, 1, 3}, 0.5
	check("l1_reg_activation", o, nil, false, []int{2, 3, 1, 3, 2}, func(d *dnnTrained) float64 {
		var s float64
		for _, x := range X {
			z, acts := d.forwardActs(x[1:])
			for k := range z {
				s += (z[k] - x[1+k]) * (z[k] - x[1+k]) / 2
			}
			s += 0.5 * math.Abs(acts[1][0])
		}
		return s / float64(len(X))
	}, nil)

	// FTRL: n = 0.1 + g², z = g - (√n - √0.1)/lr · w0, w1 = -z · lr / √n.
	o = base("DNN_REGRESSOR")
	o.Optimizer, o.Activation, o.HiddenUnits = "FTRL", "TANH", []int{2}
	tr = mlTrained{}
	if fe := tr.trainDNN(o, X, Y, nil, nil, false, 1); fe != nil {
		t.Fatal(fe.msg)
	}
	init := dnnInitByHand("TANH", []int{2, 2, 1})
	mse := func(d *dnnTrained) float64 {
		var s float64
		for i, x := range X {
			z := d.forward(x[1:])[0]
			s += (z - Y[i]) * (z - Y[i])
		}
		return s / float64(len(X))
	}
	eachWeight(init, tr.DNN, func(name string, p, q *float64) {
		w0 := *p
		*p = w0 + h
		up := mse(init)
		*p = w0 - h
		down := mse(init)
		*p = w0
		g := (up - down) / (2 * h)
		n := 0.1 + g*g
		z := g - (math.Sqrt(n)-math.Sqrt(0.1))/o.LearnRate*w0
		if want := -z * o.LearnRate / math.Sqrt(n); math.Abs(*q-want) > 1e-7 {
			t.Errorf("FTRL: weight %s after one step: %v, want %v", name, *q, want)
		}
	})
}
