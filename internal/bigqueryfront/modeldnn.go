package bigqueryfront

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DNN_REGRESSOR and DNN_CLASSIFIER models (#1548,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-dnn-models,
// read 2026-10-17), trained by the front: a fully connected feed-forward
// network of hidden_units layers, each unit activation_fn(b + w·x), and an
// output layer of one linear unit (a regressor), one logit (a binary
// classifier, sigmoid) or a logit a class (softmax), on the rows
// preprocessed as for the other models (auto-preprocessing: numeric
// features standardized, a NULL the mean; STRING and BOOL one-hot).
//
// BigQuery's documented options and defaults: optimizer ADAM (ADAGRAD,
// RMSPROP and SGD too), learn_rate 0.001, activation_fn RELU (RELU6, ELU,
// SELU, SIGMOID and TANH too), batch_size 32 or the rows when fewer,
// dropout 0, l1_reg and l2_reg 0 (only with ADAGRAD, FTRL or SGD),
// max_iterations 20 passes over the training rows, early_stop TRUE with
// min_rel_progress 0.01, and hidden_units, when not given, one layer of
// min(128, rows / (10 · (inputs + outputs))) units (at least 1); the loss
// is reduced SUM_OVER_BATCH_SIZE (each batch's mean).
//
// What is not documented, and so chosen here (UNVERIFIED against Google;
// docs/compatibility.md), as Keras's defaults are: Glorot uniform initial
// weights and zero biases, drawn from a fixed seed (1548), so the network
// is the same each run but not Google's; each pass's rows shuffled by the
// same seed; ADAM's beta_1 0.9, beta_2 0.999 and epsilon 1e-7, ADAGRAD's
// initial accumulator 0.1, RMSPROP's rho 0.9; the loss the mean squared
// error (regressor) or the mean log loss (classifier), the regularization
// l1_reg·‖w‖₁ + l2_reg·‖w‖² of the weights, not the biases; inverted
// dropout of each hidden layer's output in training only; an iteration's
// loss the training rows' mean loss after the pass, without dropout.
// Refused 501: optimizer FTRL, activation_fn CRELU, class weights,
// hyperparameter tuning, and warm_start.
//
// ML.PREDICT is written into the query as SQL of the network, a layer's
// units the columns of a subquery of their own; ML.EVALUATE,
// ML.CONFUSION_MATRIX, ML.ROC_CURVE, ML.FEATURE_INFO and ML.TRAINING_INFO
// serve it as they do the other models. ML.WEIGHTS, ML.FEATURE_IMPORTANCE
// and the explain functions are not supported for it, as BigQuery's
// documentation of each lists the model types it takes.

// dnnTrained is a network's layers, the hidden ones then the output.
type dnnTrained struct {
	Activation string     `json:"activation"`
	Layers     []dnnLayer `json:"layers"`
}

// dnnLayer is one layer: W[j] are unit j's weights on the layer's inputs,
// B[j] its bias.
type dnnLayer struct {
	W [][]float64 `json:"w"`
	B []float64   `json:"b"`
}

func isDNN(typ string) bool { return typ == "DNN_REGRESSOR" || typ == "DNN_CLASSIFIER" }

// isNetwork reports whether typ is trained as a network here: a DNN or
// an AUTOENCODER.
func isNetwork(typ string) bool { return isDNN(typ) || typ == "AUTOENCODER" }

// dnnActivations are the activation_fn values carried out.
var dnnActivations = map[string]bool{"RELU": true, "RELU6": true, "ELU": true, "SELU": true, "SIGMOID": true, "TANH": true}

const (
	seluScale = 1.0507009873554805
	seluAlpha = 1.6732632423543772
)

// dnnAct is activation fn of a.
func dnnAct(fn string, a float64) float64 {
	switch fn {
	case "RELU":
		return math.Max(a, 0)
	case "RELU6":
		return math.Min(math.Max(a, 0), 6)
	case "ELU":
		if a > 0 {
			return a
		}
		return math.Expm1(a)
	case "SELU":
		if a > 0 {
			return seluScale * a
		}
		return seluScale * seluAlpha * math.Expm1(a)
	case "SIGMOID":
		return sigmoid(a)
	case "TANH":
		return math.Tanh(a)
	}
	return a
}

// dnnActGrad is the derivative of activation fn at a.
func dnnActGrad(fn string, a float64) float64 {
	switch fn {
	case "RELU":
		if a > 0 {
			return 1
		}
		return 0
	case "RELU6":
		if a > 0 && a < 6 {
			return 1
		}
		return 0
	case "ELU":
		if a > 0 {
			return 1
		}
		return math.Exp(a)
	case "SELU":
		if a > 0 {
			return seluScale
		}
		return seluScale * seluAlpha * math.Exp(a)
	case "SIGMOID":
		s := sigmoid(a)
		return s * (1 - s)
	case "TANH":
		t := math.Tanh(a)
		return 1 - t*t
	}
	return 1
}

// dnnActSQL is activation fn of the SQL expression a.
func dnnActSQL(fn, a string) string {
	switch fn {
	case "RELU":
		return "GREATEST(" + a + ", 0.0)"
	case "RELU6":
		return "LEAST(GREATEST(" + a + ", 0.0), 6.0)"
	case "ELU":
		return "IF(" + a + " > 0, " + a + ", EXP(" + a + ") - 1)"
	case "SELU":
		return fmt.Sprintf("IF(%s > 0, %s * %s, %s * (EXP(%s) - 1))", a, sqlF(seluScale), a, sqlF(seluScale*seluAlpha), a)
	case "SIGMOID":
		return "(1 / (1 + EXP(-(" + a + "))))"
	case "TANH":
		// TANH(a) = 1 - 2 / (EXP(2a) + 1), which does not overflow to NaN.
		return "(1 - 2 / (EXP(2 * (" + a + ")) + 1))"
	}
	return a
}

// forward is the network's output for x (the encoded columns, no
// intercept): the output layer's values, before sigmoid or softmax.
func (d *dnnTrained) forward(x []float64) []float64 {
	h := x
	for l, ly := range d.Layers {
		out := make([]float64, len(ly.B))
		for j := range ly.B {
			a := ly.B[j] + dot(ly.W[j], h)
			if l < len(d.Layers)-1 {
				a = dnnAct(d.Activation, a)
			}
			out[j] = a
		}
		h = out
	}
	return h
}

// dnnFrom is from with the network's columns added, a subquery a layer:
// each hidden layer's units _cbml_h<l>_<j>, and the outputs _cbml_o<k>;
// added are the names of every column it added.
func (d *dnnTrained) dnnFrom(from string, enc []string) (string, []string) {
	in := enc
	var added []string
	for l, ly := range d.Layers {
		last := l == len(d.Layers)-1
		var cols, names []string
		for j := range ly.B {
			terms := []string{sqlF(ly.B[j])}
			for i, e := range in {
				if ly.W[j][i] != 0 {
					terms = append(terms, sqlF(ly.W[j][i])+" * "+e)
				}
			}
			a := "(" + strings.Join(terms, " + ") + ")"
			name := fmt.Sprintf("_cbml_h%d_%d", l, j)
			if last {
				name = fmt.Sprintf("_cbml_o%d", j)
			} else {
				a = dnnActSQL(d.Activation, a)
			}
			cols = append(cols, a+" AS "+name)
			names = append(names, name)
		}
		from = "(SELECT *, " + strings.Join(cols, ", ") + " FROM " + from + ")"
		added = append(added, names...)
		in = names
	}
	return from, added
}

// applyDNNDefaults sets a DNN model's documented defaults that differ
// from a linear model's.
func applyDNNDefaults(o *mlOptions) {
	if !o.given("learn_rate") {
		o.LearnRate = 0.001
	}
	if o.Optimizer == "" {
		o.Optimizer = "ADAM"
	}
	if o.Activation == "" {
		o.Activation = "RELU"
	}
}

// checkDNNOptions refuses what BigQuery documents as invalid for a DNN.
func checkDNNOptions(o mlOptions) (int, string) {
	if o.GlobalExplain {
		return 501, "Not implemented here: enable_global_explain of a DNN model: its integrated gradients attributions are not carried out. Nothing was run."
	}
	if (o.L1 != 0 || o.L2 != 0) && o.Optimizer != "ADAGRAD" && o.Optimizer != "SGD" && o.Optimizer != "FTRL" {
		return 400, "l1_reg and l2_reg are only supported with optimizer ADAGRAD, FTRL or SGD"
	}
	return 0, ""
}

// trainDNN fits the network on X (each row's encoded columns, the
// intercept's 1 first) and Y, and the metrics on EX, EY (or X, Y). K is
// the classes of a multiclass classifier, else 1.
func (t *mlTrained) trainDNN(o mlOptions, X [][]float64, Y []float64, EX [][]float64, EY []float64, classifier bool, K int) {
	t.Strategy = "BATCH_GRADIENT_DESCENT"
	strip := func(R [][]float64) [][]float64 {
		out := make([][]float64, len(R))
		for i, r := range R {
			out[i] = r[1:]
		}
		return out
	}
	X, EX = strip(X), strip(EX)
	n, in := len(X), len(X[0])
	auto := o.ModelType == "AUTOENCODER"
	if auto {
		// It reconstructs its input: an output a column (below).
		t.Strategy, K = "AUTOENCODER", in
	}
	units := o.HiddenUnits
	if len(units) == 0 {
		u := n / (10 * (in + K))
		units = []int{max(1, min(128, u))}
	}
	batch := o.BatchSize
	if batch == 0 || batch > n {
		batch = min(32, n)
	}
	rng := rand.New(rand.NewSource(1548))
	d := &dnnTrained{Activation: o.Activation}
	sizes := append(append([]int{in}, units...), K)
	for l := 1; l < len(sizes); l++ {
		limit := math.Sqrt(6 / float64(sizes[l-1]+sizes[l]))
		ly := dnnLayer{W: make([][]float64, sizes[l]), B: make([]float64, sizes[l])}
		for j := range ly.W {
			ly.W[j] = make([]float64, sizes[l-1])
			for i := range ly.W[j] {
				ly.W[j][i] = (2*rng.Float64() - 1) * limit
			}
		}
		d.Layers = append(d.Layers, ly)
	}
	t.DNN = d
	// The optimizer's state, laid out as the weights: a W and B per layer.
	type moments struct{ m, v [][][]float64 }
	zeros := func(fill float64) [][][]float64 {
		out := make([][][]float64, len(d.Layers))
		for l, ly := range d.Layers {
			out[l] = make([][]float64, len(ly.B))
			for j := range ly.B {
				out[l][j] = make([]float64, len(ly.W[j])+1) // the bias last
				for i := range out[l][j] {
					out[l][j][i] = fill
				}
			}
		}
		return out
	}
	acc := 0.0
	if o.Optimizer == "ADAGRAD" {
		acc = 0.1
	}
	st := moments{m: zeros(0), v: zeros(acc)}
	step := 0
	lossOf := func(z []float64, y float64, x []float64) float64 {
		switch {
		case auto:
			var s float64
			for k := range z {
				s += (z[k] - x[k]) * (z[k] - x[k])
			}
			return s / float64(K)
		case K > 1:
			return -math.Log(math.Max(softmaxOf(z)[int(y)], 1e-15))
		case classifier:
			q := math.Min(math.Max(sigmoid(z[0]), 1e-15), 1-1e-15)
			return -(y*math.Log(q) + (1-y)*math.Log(1-q))
		}
		return (z[0] - y) * (z[0] - y)
	}
	meanLoss := func(X [][]float64, Y []float64) float64 {
		var s float64
		for i, x := range X {
			y := 0.0
			if Y != nil {
				y = Y[i]
			}
			s += lossOf(d.forward(x), y, x)
		}
		return s / float64(len(X))
	}
	L := len(d.Layers)
	prev := math.Inf(1)
	for epoch := 0; epoch < o.MaxIterations; epoch++ {
		start := time.Now()
		order := rng.Perm(n)
		for b0 := 0; b0 < n; b0 += batch {
			rowsIn := order[b0:min(b0+batch, n)]
			grad := zeros(0)
			for _, ri := range rowsIn {
				// Forward, keeping each layer's input and pre-activation.
				inputs := make([][]float64, L)
				pre := make([][]float64, L)
				masks := make([][]float64, L)
				h := X[ri]
				for l, ly := range d.Layers {
					inputs[l] = h
					a := make([]float64, len(ly.B))
					out := make([]float64, len(ly.B))
					for j := range ly.B {
						a[j] = ly.B[j] + dot(ly.W[j], h)
						out[j] = a[j]
						if l < L-1 {
							out[j] = dnnAct(d.Activation, a[j])
						}
					}
					if l < L-1 && o.Dropout > 0 {
						masks[l] = make([]float64, len(out))
						for j := range out {
							if rng.Float64() >= o.Dropout {
								masks[l][j] = 1 / (1 - o.Dropout)
							}
							out[j] *= masks[l][j]
						}
					}
					pre[l] = a
					h = out
				}
				// The loss's gradient on the outputs.
				delta := make([]float64, K)
				switch {
				case auto:
					for k := range delta {
						delta[k] = 2 * (h[k] - X[ri][k]) / float64(K)
					}
				case K > 1:
					p := softmaxOf(h)
					for k := range delta {
						delta[k] = p[k]
						if int(Y[ri]) == k {
							delta[k]--
						}
					}
				case classifier:
					delta[0] = sigmoid(h[0]) - Y[ri]
				default:
					delta[0] = 2 * (h[0] - Y[ri])
				}
				for l := L - 1; l >= 0; l-- {
					ly := d.Layers[l]
					for j := range ly.B {
						for i, v := range inputs[l] {
							grad[l][j][i] += delta[j] * v
						}
						grad[l][j][len(ly.W[j])] += delta[j]
					}
					if l == 0 {
						break
					}
					next := make([]float64, len(inputs[l]))
					for i := range next {
						var s float64
						for j := range ly.B {
							s += ly.W[j][i] * delta[j]
						}
						s *= dnnActGrad(d.Activation, pre[l-1][i])
						if masks[l-1] != nil {
							s *= masks[l-1][i]
						}
						next[i] = s
					}
					delta = next
				}
			}
			step++
			bn := float64(len(rowsIn))
			for l, ly := range d.Layers {
				for j := range ly.B {
					for i := 0; i <= len(ly.W[j]); i++ {
						g := grad[l][j][i] / bn
						var w *float64
						if i < len(ly.W[j]) {
							w = &ly.W[j][i]
							g += 2*o.L2*(*w) + o.L1*sign(*w)
						} else {
							w = &ly.B[j]
						}
						m, v := &st.m[l][j][i], &st.v[l][j][i]
						switch o.Optimizer {
						case "SGD":
							*w -= o.LearnRate * g
						case "ADAGRAD":
							*v += g * g
							*w -= o.LearnRate * g / (math.Sqrt(*v) + 1e-7)
						case "RMSPROP":
							*v = 0.9*(*v) + 0.1*g*g
							*w -= o.LearnRate * g / (math.Sqrt(*v) + 1e-7)
						default: // ADAM
							*m = 0.9*(*m) + 0.1*g
							*v = 0.999*(*v) + 0.001*g*g
							mh := *m / (1 - math.Pow(0.9, float64(step)))
							vh := *v / (1 - math.Pow(0.999, float64(step)))
							*w -= o.LearnRate * mh / (math.Sqrt(vh) + 1e-7)
						}
					}
				}
			}
		}
		it := mlIteration{Index: epoch, Loss: meanLoss(X, Y), LearnRate: o.LearnRate, DurationMs: time.Since(start).Milliseconds()}
		if len(EX) > 0 {
			e := meanLoss(EX, EY)
			it.EvalLoss = &e
		}
		t.Iterations = append(t.Iterations, it)
		cur := it.Loss
		if it.EvalLoss != nil {
			cur = *it.EvalLoss
		}
		if o.EarlyStop && prev < math.Inf(1) && (prev == 0 || (prev-cur)/prev < o.MinRelProgress) {
			break
		}
		prev = cur
	}
	sort.Slice(t.Iterations, func(a, b int) bool { return t.Iterations[a].Index > t.Iterations[b].Index })
	evX, evY := EX, EY
	if len(evX) == 0 {
		evX, evY = X, Y
	}
	if auto {
		t.Eval = d.reconstructionMetrics(evX)
		return
	}
	if K > 1 {
		probs := make([][]float64, len(evX))
		for i, x := range evX {
			probs[i] = softmaxOf(d.forward(x))
		}
		t.Eval = multiclassMetrics(evY, probs)
		t.EvalY = evY
		for _, p := range probs {
			t.EvalScore = append(t.EvalScore, float64(argmax(p)))
		}
		return
	}
	pred := make([]float64, len(evX))
	for i, x := range evX {
		pred[i] = d.forward(x)[0]
		if classifier {
			pred[i] = sigmoid(pred[i])
		}
	}
	if classifier {
		t.Eval = classificationMetrics(evY, pred, 0.5)
		t.EvalY, t.EvalScore = evY, pred
	} else {
		t.Eval = regressionMetrics(evY, pred)
	}
}

func sign(v float64) float64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// parseDNNOption reads one of a DNN's own options into o; ok is false for
// an option that is not one of them.
func parseDNNOption(name string, val []token, o *mlOptions) (code int, msg string, ok bool) {
	bad := func() (int, string, bool) { return 400, "Invalid value for option " + name, true }
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
	switch name {
	case "hidden_units":
		if len(val) < 3 || !val[0].punct("[") || !val[len(val)-1].punct("]") {
			return bad()
		}
		o.HiddenUnits = nil
		for _, t := range val[1 : len(val)-1] {
			if t.punct(",") {
				continue
			}
			v, err := strconv.Atoi(t.text)
			if err != nil || v < 1 {
				return bad()
			}
			o.HiddenUnits = append(o.HiddenUnits, v)
		}
	case "activation_fn":
		s, okv := str()
		if !okv {
			return bad()
		}
		s = strings.ToUpper(s)
		if s == "CRELU" {
			return 501, "Not implemented here: activation_fn CRELU. CloudBurrow carries out RELU, RELU6, ELU, SELU, SIGMOID and TANH. Nothing was run.", true
		}
		if !dnnActivations[s] {
			return bad()
		}
		o.Activation = s
	case "optimizer":
		s, okv := str()
		if !okv {
			return bad()
		}
		s = strings.ToUpper(s)
		switch s {
		case "ADAM", "ADAGRAD", "RMSPROP", "SGD":
			o.Optimizer = s
		case "FTRL":
			return 501, "Not implemented here: optimizer FTRL. CloudBurrow trains DNNs with ADAM, ADAGRAD, RMSPROP and SGD. Nothing was run.", true
		default:
			return bad()
		}
	case "batch_size":
		v, okv := num()
		if !okv || v != float64(int(v)) || v < 1 || v > 8192 {
			return bad()
		}
		o.BatchSize = int(v)
	case "dropout":
		v, okv := num()
		if !okv || v < 0 || v >= 1 {
			return 400, "dropout must be in [0, 1)", true
		}
		o.Dropout = v
	case "l1_reg_activation":
		v, okv := num()
		if !okv || v < 0 {
			return bad()
		}
		if v > 0 {
			return 501, "Not implemented here: l1_reg_activation other than 0 (its activity regularization is not carried out). Nothing was run.", true
		}
	case "class_weights":
		return 501, "Not implemented here: CREATE MODEL option class_weights. Nothing was run.", true
	default:
		return 0, "", false
	}
	return 0, "", true
}

// reconstructionMetrics are an autoencoder's ML.EVALUATE metrics on the
// encoded rows X: mean_absolute_error, mean_squared_error and
// mean_squared_log_error of every column of every row against its
// reconstruction (regressionMetrics: NULL when a value is -1 or less).
func (d *dnnTrained) reconstructionMetrics(X [][]float64) map[string]*float64 {
	var y, p []float64
	for _, x := range X {
		y = append(y, x...)
		p = append(p, d.forward(x)...)
	}
	m := regressionMetrics(y, p)
	return map[string]*float64{"mean_absolute_error": m["mean_absolute_error"], "mean_squared_error": m["mean_squared_error"],
		"mean_squared_log_error": m["mean_squared_log_error"]}
}

// autoencoderMetricNames are an autoencoder's ML.EVALUATE and
// ML.RECONSTRUCTION_LOSS columns, in BigQuery's order.
var autoencoderMetricNames = []string{"mean_absolute_error", "mean_squared_error", "mean_squared_log_error"}

// latentLayer is the index of an autoencoder's latent layer: the middle
// of hidden_units ("the middle value ... defines the latent space").
func (d *dnnTrained) latentLayer() int { return (len(d.Layers) - 1) / 2 }

// trainAutoencoder trains an AUTOENCODER (#1548,
// https://cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-autoencoder):
// every input column a feature, preprocessed as for the other models, and
// a network of hidden_units whose output layer reconstructs the encoded
// columns, trained as a DNN (above) on their mean squared error; there is
// no data split. ML.PREDICT gives the middle hidden layer's units as
// latent_col_1, ..., then the input columns; ML.EVALUATE and
// ML.RECONSTRUCTION_LOSS the errors of the encoded columns' reconstruction.
// UNVERIFIED: BigQuery documents latent_col_<index> as INT64, which cannot
// hold a unit's value, so CloudBurrow gives FLOAT64; the errors are of the
// preprocessed columns (BigQuery does not say which).
func trainAutoencoder(opts mlOptions, cols []mlColumn, rows [][]*string) (mlTrained, *mlFail) {
	var t mlTrained
	if len(opts.LabelCols) > 0 {
		return t, mlInvalid("input_label_cols is not supported for model type AUTOENCODER")
	}
	fts, fidx, fe := mlUnsupervisedFeatures(cols, -1)
	if fe != nil {
		return t, fe
	}
	if len(rows) == 0 {
		return t, mlInvalid("Input data doesn't contain any rows")
	}
	all := make([]int, len(rows))
	for i := range all {
		all[i] = i
	}
	if fe := learnFeatures(fts, fidx, rows, all); fe != nil {
		return t, fe
	}
	t.Features, t.Rows = fts, len(rows)
	var X [][]float64
	for _, row := range rows {
		x := []float64{1}
		for k, ft := range fts {
			x = append(x, encodeFeature(ft, row[fidx[k]])...)
		}
		X = append(X, x)
	}
	t.trainDNN(opts, X, nil, nil, nil, false, 0)
	return t, nil
}

// autoencoderResource is an AUTOENCODER's trainingOptions and
// evaluationMetrics (models.get).
func (md bqModel) autoencoderResource(res map[string]any) (map[string]any, map[string]any) {
	o, t := md.Options, md.Trained
	ms := func(v int64) string { return strconv.FormatInt(v, 10) }
	var hidden []string
	var latent []map[string]any
	if t.DNN != nil {
		for _, ly := range t.DNN.Layers[:len(t.DNN.Layers)-1] {
			hidden = append(hidden, ms(int64(len(ly.B))))
		}
		for j := range t.DNN.Layers[t.DNN.latentLayer()].B {
			latent = append(latent, map[string]any{"name": fmt.Sprintf("latent_col_%d", j+1), "type": map[string]string{"typeKind": "FLOAT64"}})
		}
	}
	res["labelColumns"] = latent
	batch := o.BatchSize
	if batch == 0 {
		batch = min(32, t.Rows)
	}
	opts := map[string]any{"maxIterations": ms(int64(o.MaxIterations)), "earlyStop": o.EarlyStop, "minRelativeProgress": o.MinRelProgress,
		"learnRate": o.LearnRate, "optimizer": o.Optimizer, "activationFn": o.Activation, "hiddenUnits": hidden,
		"batchSize": ms(int64(batch)), "dropout": o.Dropout}
	num := func(k string) any {
		if v := t.Eval[k]; v != nil {
			return *v
		}
		return nil
	}
	return opts, map[string]any{"regressionMetrics": map[string]any{"meanAbsoluteError": num("mean_absolute_error"),
		"meanSquaredError": num("mean_squared_error"), "meanSquaredLogError": num("mean_squared_log_error")}}
}

// autoencoderSubquery is the subquery of call c of an AUTOENCODER md:
// ML.PREDICT, ML.EVALUATE and ML.RECONSTRUCTION_LOSS (trainAutoencoder).
func (f front) autoencoderSubquery(r *http.Request, q queryOptions, md bqModel, c mlCall) (string, int, string) {
	t := md.Trained
	if t.DNN == nil {
		return "", 500, "cloudburrow: the autoencoder has no network"
	}
	switch c.fn {
	case "EVALUATE":
		metrics := t.Eval
		if c.input != "" {
			X, code, msg := f.mlFeatureRows(r, q, md, c.input)
			if code != 0 {
				return "", code, msg
			}
			metrics = t.DNN.reconstructionMetrics(X)
		}
		var cols, vals []string
		for _, n := range autoencoderMetricNames {
			cols = append(cols, n+" FLOAT64")
			vals = append(vals, sqlFloat(metrics[n]))
		}
		return mlRowsSubquery("STRUCT<"+strings.Join(cols, ", ")+">", []string{"STRUCT(" + strings.Join(vals, ", ") + ")"}), 0, ""
	case "PREDICT", "RECONSTRUCTION_LOSS":
		if c.input == "" {
			return "", 400, "ML." + c.fn + " requires an input: a query in parentheses or TABLE name"
		}
		from, ref, helpers := md.mlInput(c.input)
		enc := t.encodedSQL(ref)
		from, added := t.DNN.dnnFrom(from, enc)
		hidden := except(append(helpers, added...))
		if c.fn == "PREDICT" {
			var latent []string
			for j := range t.DNN.Layers[t.DNN.latentLayer()].B {
				latent = append(latent, fmt.Sprintf("_cbml_h%d_%d AS latent_col_%d", t.DNN.latentLayer(), j, j+1))
			}
			return "(SELECT " + strings.Join(latent, ", ") + ", " + hidden + " FROM " + from + ")", 0, ""
		}
		var abs, sq, lg, all []string
		for k, e := range enc {
			o := fmt.Sprintf("_cbml_o%d", k)
			abs = append(abs, fmt.Sprintf("ABS(%s - %s)", o, e))
			sq = append(sq, fmt.Sprintf("POW(%s - %s, 2)", o, e))
			lg = append(lg, fmt.Sprintf("POW(LN(1 + %s) - LN(1 + %s), 2)", o, e))
			all = append(all, o, e)
		}
		d := sqlF(float64(len(enc)))
		mean := func(terms []string) string { return "((" + strings.Join(terms, " + ") + ") / " + d + ")" }
		return "(SELECT " + mean(abs) + " AS mean_absolute_error, " + mean(sq) + " AS mean_squared_error, " +
			"IF(LEAST(" + strings.Join(all, ", ") + ") <= -1, CAST(NULL AS FLOAT64), " + mean(lg) + ") AS mean_squared_log_error, " +
			hidden + " FROM " + from + ")", 0, ""
	}
	return "", 400, "ML." + c.fn + " is not supported for model type AUTOENCODER"
}
