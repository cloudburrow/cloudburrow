package bigqueryfront

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

func TestParseExportModel(t *testing.T) {
	parse := func(sql string) (exportDDL, bool, int, string) {
		t.Helper()
		toks, ok := lex(sql)
		if !ok {
			t.Fatalf("lex %q", sql)
		}
		return parseExportModel(toks)
	}
	d, is, code, msg := parse("EXPORT MODEL `p.d.m` OPTIONS(URI = 'gs://b/path/m/')")
	if !is || code != 0 || d.uri != "gs://b/path/m/" || strings.Join(d.path, ".") != "p.d.m" || d.trialOK {
		t.Fatalf("%+v %v %d %q", d, is, code, msg)
	}
	// As GCPC's BigqueryExportModelJobOp writes it.
	d, _, code, msg = parse(`EXPORT MODEL ` + "`p.d.m`" + ` OPTIONS(URI="gs://b/x",add_serving_default_signature=True)`)
	if code != 0 || d.uri != "gs://b/x" {
		t.Fatalf("GCPC's statement: %d %q", code, msg)
	}
	if d, _, code, _ = parse("EXPORT MODEL d.m OPTIONS(URI='gs://b/x', TRIAL_ID=12)"); code != 0 || !d.trialOK || d.trial != 12 {
		t.Fatalf("TRIAL_ID: %d %+v", code, d)
	}
	if _, is, _, _ := parse("EXPORT DATA OPTIONS(uri='gs://b/*.csv', format='CSV') AS SELECT 1"); is {
		t.Error("EXPORT DATA read as EXPORT MODEL")
	}
	for _, tc := range []struct{ sql, want string }{
		{"EXPORT MODEL d.m", "URI option is required"},
		{"EXPORT MODEL d.m OPTIONS(TRIAL_ID=1)", "URI option is required"},
		{"EXPORT MODEL d.m OPTIONS(URI=1)", "a string is expected"},
		{"EXPORT MODEL d.m OPTIONS(URI='gs://b/x', format='x')", "Unknown option"},
		{"EXPORT MODEL OPTIONS(URI='gs://b/x')", "expected a model name"},
	} {
		if _, _, code, msg := parse(tc.sql); code != 400 || !strings.Contains(msg, tc.want) {
			t.Errorf("%s: %d %q, want 400 %q", tc.sql, code, msg, tc.want)
		}
	}
}

// exportTrain trains a model of the CREATE MODEL options opts on cols and
// rows.
func exportTrain(t *testing.T, opts string, cols []mlColumn, rows [][]*string) bqModel {
	t.Helper()
	sql := "CREATE MODEL ds.m OPTIONS(" + opts + ") AS SELECT 1"
	toks, ok := lex(sql)
	if !ok {
		t.Fatal("lex")
	}
	d, _, code, msg := parseModelDDL(toks, sql)
	if code == 0 {
		code, msg = checkModelOptions(d.opts)
	}
	if code != 0 {
		t.Fatal(code, msg)
	}
	tr, fe := trainModel(d.opts, cols, rows, func(texts []string) ([]int64, *mlFail) {
		out := make([]int64, len(texts))
		for i, s := range texts {
			for _, c := range s {
				out[i] = out[i]*31 + int64(c)
			}
		}
		return out, nil
	})
	if fe != nil {
		t.Fatal(fe)
	}
	return bqModel{Project: "p", Dataset: "ds", ID: "m", Type: d.opts.ModelType, Options: d.opts, Trained: tr}
}

// exportRows are rows of a numeric x, a categorical c, a numeric y and a
// two-class label.
func exportRows() ([]mlColumn, [][]*string) {
	cols := []mlColumn{{"x", "FLOAT64"}, {"c", "STRING"}, {"y", "FLOAT64"}, {"label", "STRING"}}
	var rows [][]*string
	for i := 0; i < 60; i++ {
		x := float64(i%10) - 4.5
		c := []string{"red", "green", "blue"}[i%3]
		y := 2*x + map[string]float64{"red": 1, "green": -2, "blue": 0.5}[c] + float64(i%7)*0.1
		label := "no"
		if y > 0 {
			label = "yes"
		}
		rows = append(rows, []*string{sp(fmt.Sprint(x)), sp(c), sp(fmt.Sprint(y)), sp(label)})
	}
	return cols, rows
}

func rowVals(cols []mlColumn, row []*string) map[string]*string {
	m := map[string]*string{}
	for i, c := range cols {
		m[strings.ToLower(c.Name)] = row[i]
	}
	return m
}

// xgbRead reads back the model.bst xgbBooster writes, as XGBoost 0.82's
// LearnerImpl::Load does, and predicts the margin of each group.
type xgbRead struct {
	base      float32
	numClass  int32
	objective string
	groups    int32
	trees     [][]struct {
		parent, left, right int32
		sindex              uint32
		value               float32
	}
	info []int32
}

func readXGB(t *testing.T, b []byte) xgbRead {
	t.Helper()
	r := bytes.NewReader(b)
	rd := func(v any) {
		if err := binary.Read(r, binary.LittleEndian, v); err != nil {
			t.Fatal(err)
		}
	}
	str := func() string {
		var n uint64
		rd(&n)
		s := make([]byte, n)
		rd(s)
		return string(s)
	}
	var m xgbRead
	var numFeature uint32
	var extra, evals int32
	rd(&m.base)
	rd(&numFeature)
	rd(&m.numClass)
	rd(&extra)
	rd(&evals)
	rd(make([]int32, 29))
	m.objective = str()
	if g := str(); g != "gbtree" {
		t.Fatalf("booster %q", g)
	}
	var numTrees, roots, nf, pad int32
	var pbuf int64
	var leafVec int32
	rd(&numTrees)
	rd(&roots)
	rd(&nf)
	rd(&pad)
	rd(&pbuf)
	rd(&m.groups)
	rd(&leafVec)
	rd(make([]int32, 32))
	if uint32(nf) != numFeature || roots != 1 {
		t.Fatalf("num_feature %d / %d, num_roots %d", nf, numFeature, roots)
	}
	for k := int32(0); k < numTrees; k++ {
		param := make([]int32, 37)
		rd(param)
		n := param[1]
		nodes := make([]struct {
			parent, left, right int32
			sindex              uint32
			value               float32
		}, n)
		for j := range nodes {
			rd(&nodes[j].parent)
			rd(&nodes[j].left)
			rd(&nodes[j].right)
			rd(&nodes[j].sindex)
			rd(&nodes[j].value)
		}
		rd(make([]float32, 4*n)) // the stats
		m.trees = append(m.trees, nodes)
	}
	m.info = make([]int32, numTrees)
	rd(m.info)
	if r.Len() != 0 {
		t.Fatalf("%d bytes left over", r.Len())
	}
	return m
}

// margins is XGBoost's prediction margin of encoded row x (no intercept).
func (m xgbRead) margins(x []float64) []float64 {
	base := float64(m.base)
	out := make([]float64, m.groups)
	for k := range out {
		out[k] = base
	}
	for j, tr := range m.trees {
		n := 0
		for tr[n].left != -1 {
			f := tr[n].sindex &^ (1 << 31)
			if tr[n].right != tr[n].left+1 {
				panic("a split's right child does not follow its left, as XGBoost's predictors need")
			}
			// XGBoost's GetNextNode: LeftChild() + !(value < split_cond).
			if float32(x[f]) < tr[n].value {
				n = int(tr[n].left)
			} else {
				n = int(tr[n].left) + 1
			}
			if p := uint32(tr[n].parent); int(p&^(1<<31)) >= len(tr) || (p>>31 == 1) != (tr[int(p&^(1<<31))].left == int32(n)) {
				panic("a node's parent does not name it")
			}
		}
		out[m.info[j]] += float64(tr[n].value)
	}
	return out
}

// The XGBoost Booster export's model.bst, read back as XGBoost 0.82
// reads it, predicts what the model does, for a regressor, a binary and a
// multiclass classifier and a random forest; and its other files are
// there.
func TestExportXGBoostBooster(t *testing.T) {
	cols, rows := exportRows()
	three := append([]mlColumn(nil), cols...)
	three[3] = mlColumn{"label", "STRING"}
	var rows3 [][]*string
	for _, r := range rows {
		r3 := append([]*string(nil), r...)
		r3[3] = r[1]
		r3[1] = sp([]string{"a", "b"}[len(*r[0])%2])
		rows3 = append(rows3, r3)
	}
	for _, tc := range []struct {
		name, opts string
		cols       []mlColumn
		rows       [][]*string
	}{
		{"regressor", "model_type='BOOSTED_TREE_REGRESSOR', input_label_cols=['y'], max_iterations=5, data_split_method='NO_SPLIT'", cols[:3], cut(rows, 3)},
		{"binary", "model_type='BOOSTED_TREE_CLASSIFIER', input_label_cols=['label'], max_iterations=5, data_split_method='NO_SPLIT'", cols, rows},
		{"multiclass", "model_type='BOOSTED_TREE_CLASSIFIER', input_label_cols=['label'], max_iterations=3, data_split_method='NO_SPLIT'", three, rows3},
		{"forest", "model_type='RANDOM_FOREST_REGRESSOR', input_label_cols=['y'], num_parallel_tree=5, data_split_method='NO_SPLIT'", cols[:3], cut(rows, 3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := exportTrain(t, tc.opts, tc.cols, tc.rows)
			files := xgbFiles(md)
			byName := map[string][]byte{}
			for _, f := range files {
				byName[f.name] = f.data
			}
			for _, want := range []string{"model.bst", "main.py", "xgboost_predictor-0.1.tar.gz", "assets/model_metadata.json"} {
				if byName[want] == nil {
					t.Fatalf("no %s in %v", want, keys(byName))
				}
			}
			if len(md.Trained.Trees) == 0 {
				t.Fatal("no trees")
			}
			got := readXGB(t, byName["model.bst"])
			K := len(md.Trained.Classes)
			for _, row := range tc.rows {
				vals := rowVals(tc.cols, row)
				x := md.Trained.encodeRow(vals)
				m := got.margins(x[1:])
				var want []float64
				if K > 2 {
					want = md.Trained.treeMargins(x, K)
				} else {
					want = []float64{md.Trained.treeMargin(x)}
				}
				for k := range want {
					if math.Abs(m[k]-want[k]) > 1e-4*math.Max(1, math.Abs(want[k])) {
						t.Fatalf("row %v class %d: XGBoost's margin %g, the model's %g", row, k, m[k], want[k])
					}
				}
			}
			var meta map[string]any
			if err := json.Unmarshal(byName["assets/model_metadata.json"], &meta); err != nil {
				t.Fatal(err)
			}
			if names, _ := meta["feature_names"].(map[string]any); names["f0"] != "x" {
				t.Errorf("feature_names %v", meta["feature_names"])
			}
		})
	}
}

func cut(rows [][]*string, n int) [][]*string {
	var out [][]*string
	for _, r := range rows {
		out = append(out, r[:n])
	}
	return out
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The formats and refusals: each type in its format, a format the model
// has none of, an unsupported type, TRIAL_ID, TRANSFORM and an ARRAY
// feature.
func TestExportModelRefusals(t *testing.T) {
	f := front{}
	for typ, want := range map[string]string{"LINEAR_REG": "ML_TF_SAVED_MODEL", "BOOSTED_TREE_CLASSIFIER": "ML_XGBOOST_BOOSTER",
		"RANDOM_FOREST_CLASSIFIER": "ML_XGBOOST_BOOSTER", "DNN_LINEAR_COMBINED_REGRESSOR": "ML_TF_SAVED_MODEL", "AUTOENCODER": "ML_TF_SAVED_MODEL",
		"MATRIX_FACTORIZATION": "ML_TF_SAVED_MODEL", "TENSORFLOW": "ML_TF_SAVED_MODEL", "ARIMA_PLUS": "", "ONNX": "", "XGBOOST": "", "CONTRIBUTION_ANALYSIS": ""} {
		if got := exportFormatOf(typ); got != want {
			t.Errorf("%s: format %q, want %q", typ, got, want)
		}
	}
	ctx := context.Background()
	md := bqModel{Project: "p", Dataset: "d", ID: "m", Type: "ARIMA_PLUS"}
	if _, code, msg := f.exportModel(ctx, md, "", 0, false); code != 400 || !strings.Contains(msg, "type ARIMA_PLUS is not supported") {
		t.Errorf("ARIMA_PLUS: %d %q", code, msg)
	}
	md.Type = "LINEAR_REG"
	if _, code, msg := f.exportModel(ctx, md, "ML_XGBOOST_BOOSTER", 0, false); code != 400 || !strings.Contains(msg, "its format is ML_TF_SAVED_MODEL") {
		t.Errorf("format: %d %q", code, msg)
	}
	if _, code, msg := f.exportModel(ctx, md, "", 3, true); code != 400 || !strings.Contains(msg, "TRIAL_ID") {
		t.Errorf("TRIAL_ID: %d %q", code, msg)
	}
	md.Transform = []string{"x AS x"}
	if _, code, _ := f.exportModel(ctx, md, "", 0, false); code != 501 {
		t.Errorf("TRANSFORM: %d", code)
	}
	md.Transform = nil
	md.Trained.Features = []mlFeature{{Name: "a", Type: "ARRAY<FLOAT64>"}}
	if _, code, msg := f.exportModel(ctx, md, "", 0, false); code != 400 || !strings.Contains(msg, "ARRAY") {
		t.Errorf("ARRAY: %d %q", code, msg)
	}
	md.Trained.Features = nil
	if _, code, msg := f.exportModel(ctx, md, "", 0, false); code != 501 || !strings.Contains(msg, "--tfconvert") {
		t.Errorf("no converter: %d %q", code, msg)
	}
}

// With the converter image (CLOUDBURROW_TFCONVERT_IMAGE), each TF
// SavedModel export loads in TensorFlow and its serving_default answers
// what ML.PREDICT's model does: a linear and a logistic regression (binary
// and multiclass), a DNN regressor and classifier, KMEANS and PCA.
func TestExportTFSavedModelWithTensorFlow(t *testing.T) {
	image := os.Getenv("CLOUDBURROW_TFCONVERT_IMAGE")
	if image == "" {
		t.Skip("CLOUDBURROW_TFCONVERT_IMAGE is not set")
	}
	cols, rows := exportRows()
	feats := cols[:2]
	for _, tc := range []struct {
		name, opts string
		cols       []mlColumn
		rows       [][]*string
		check      func(md bqModel, vals map[string]*string, out map[string]any, i int) string
	}{
		{"linear", "model_type='LINEAR_REG', input_label_cols=['y'], data_split_method='NO_SPLIT'", cols[:3], cut(rows, 3), checkScalar("predicted_y")},
		{"logistic", "model_type='LOGISTIC_REG', input_label_cols=['label'], data_split_method='NO_SPLIT'", append(append([]mlColumn(nil), feats...), cols[3]), dropCol(rows, 2), checkBinary},
		{"dnn", "model_type='DNN_REGRESSOR', input_label_cols=['y'], hidden_units=[4, 3], max_iterations=3, data_split_method='NO_SPLIT'", cols[:3], cut(rows, 3), checkScalar("predicted_y")},
		{"dnn-classifier", "model_type='DNN_CLASSIFIER', input_label_cols=['label'], hidden_units=[4], max_iterations=3, data_split_method='NO_SPLIT'", append(append([]mlColumn(nil), feats...), cols[3]), dropCol(rows, 2), checkBinary},
		{"multiclass", "model_type='LOGISTIC_REG', input_label_cols=['c'], data_split_method='NO_SPLIT'", []mlColumn{cols[0], cols[2], cols[1]}, reorder(rows, 0, 2, 1), checkMulticlass},
		{"autoencoder", "model_type='AUTOENCODER', hidden_units=[3, 2, 3], max_iterations=3", cols[:3], cut(rows, 3), checkLatent},
		{"kmeans", "model_type='KMEANS', num_clusters=3", cols[:3], cut(rows, 3), checkKMeans},
		{"pca", "model_type='PCA', num_principal_components=2", []mlColumn{cols[0], cols[2]}, dropCol(cut(rows, 3), 1), checkPCA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := exportTrain(t, tc.opts, tc.cols, tc.rows)
			spec, code, msg := exportSpec(md)
			if code != 0 {
				t.Fatal(code, msg)
			}
			b, _ := json.Marshal(spec)
			dir := t.TempDir()
			model := filepath.Join(dir, "model")
			if err := os.Mkdir(model, 0o755); err != nil {
				t.Fatal(err)
			}
			d := &tfconvert.Docker{Image: image}
			if err := d.Export(context.Background(), b, model); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"saved_model.pb", "variables/variables.index"} {
				if _, err := os.Stat(filepath.Join(model, want)); err != nil {
					t.Fatalf("no %s: %v", want, err)
				}
			}
			// BigQuery ML imports it back: it converts to TensorFlow
			// Lite's builtins, as CREATE MODEL ... TENSORFLOW needs.
			if _, err := d.Convert(context.Background(), model); err != nil {
				t.Errorf("the export does not convert to TensorFlow Lite: %v", err)
			}
			// Run serving_default on the rows (convert.py's predict).
			req := map[string][]any{}
			for _, in := range spec["inputs"].([]map[string]any) {
				name := in["name"].(string)
				for _, row := range tc.rows {
					v := rowVals(tc.cols, row)[strings.ToLower(name)]
					if in["dtype"] == "string" {
						req[name] = append(req[name], *v)
					} else {
						var x float64
						_, _ = fmt.Sscan(*v, &x)
						req[name] = append(req[name], x)
					}
				}
			}
			body, _ := json.Marshal(req)
			cmd := exec.Command("docker", "run", "--rm", "-i", "--network", "none", "-e", "TF_CPP_MIN_LOG_LEVEL=2", "-v", model+":/model:ro", image, "predict")
			cmd.Stdin = bytes.NewReader(body)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			raw, err := cmd.Output()
			if err != nil {
				t.Fatalf("predict: %v: %s", err, stderr.String())
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			for i, row := range tc.rows {
				if why := tc.check(md, rowVals(tc.cols, row), out, i); why != "" {
					t.Fatalf("row %d: %s (outputs %v)", i, why, keysAny(out))
				}
			}
		})
	}
}

func dropCol(rows [][]*string, j int) [][]*string {
	var out [][]*string
	for _, r := range rows {
		out = append(out, append(append([]*string(nil), r[:j]...), r[j+1:]...))
	}
	return out
}

func keysAny(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// closeTo is a float32 computation's closeness: the export computes in
// float32.
func closeTo(a, b float64) bool { return math.Abs(a-b) <= 2e-5*math.Max(1, math.Abs(b)) }

func checkScalar(name string) func(bqModel, map[string]*string, map[string]any, int) string {
	return func(md bqModel, vals map[string]*string, out map[string]any, i int) string {
		got, _ := out[name].([]any)
		if len(got) <= i {
			return "no " + name
		}
		if want := md.Trained.predictRow(vals); !closeTo(got[i].(float64), want) {
			return fmt.Sprintf("%s %v, want %g", name, got[i], want)
		}
		return ""
	}
}

func checkBinary(md bqModel, vals map[string]*string, out map[string]any, i int) string {
	probs, _ := out["label_probs"].([]any)
	values, _ := out["label_values"].([]any)
	pred, _ := out["predicted_label"].([]any)
	if len(probs) <= i || len(pred) <= i || len(values) == 0 {
		return "no predicted_label, label_probs or label_values"
	}
	p := md.Trained.predictRow(vals)
	pr := probs[i].([]any)
	if !closeTo(pr[0].(float64), p) || !closeTo(pr[1].(float64), 1-p) {
		return fmt.Sprintf("label_probs %v, want [%g %g]", pr, p, 1-p)
	}
	want := md.Trained.Classes[0]
	if p >= 0.5 {
		want = md.Trained.Classes[1]
	}
	if pred[i] != want {
		return fmt.Sprintf("predicted_label %v, want %s", pred[i], want)
	}
	return ""
}

func checkKMeans(md bqModel, vals map[string]*string, out map[string]any, i int) string {
	ids, _ := out["centroid_id"].([]any)
	if len(ids) <= i {
		return "no centroid_id"
	}
	x := md.Trained.encodeRow(vals)[1:]
	best, bd := 0, math.Inf(1)
	for k, c := range md.Trained.Centroids {
		d := 0.0
		for j := range c {
			d += (x[j] - c[j]) * (x[j] - c[j])
		}
		if d < bd {
			best, bd = k, d
		}
	}
	if int(ids[i].(float64)) != best+1 {
		return fmt.Sprintf("centroid_id %v, want %d", ids[i], best+1)
	}
	return ""
}

func checkPCA(md bqModel, vals map[string]*string, out map[string]any, i int) string {
	x := md.Trained.encodeRow(vals)[1:]
	for k, c := range md.Trained.Components {
		got, _ := out[fmt.Sprintf("principal_component_%d", k+1)].([]any)
		if len(got) <= i {
			return fmt.Sprintf("no principal_component_%d", k+1)
		}
		if want := dot(c, x); !closeTo(got[i].(float64), want) {
			return fmt.Sprintf("principal_component_%d %v, want %g", k+1, got[i], want)
		}
	}
	return ""
}

func reorder(rows [][]*string, idx ...int) [][]*string {
	var out [][]*string
	for _, r := range rows {
		var o []*string
		for _, j := range idx {
			o = append(o, r[j])
		}
		out = append(out, o)
	}
	return out
}

func checkMulticlass(md bqModel, vals map[string]*string, out map[string]any, i int) string {
	probs, _ := out["c_probs"].([]any)
	pred, _ := out["predicted_c"].([]any)
	if len(probs) <= i || len(pred) <= i {
		return "no predicted_c or c_probs"
	}
	want := md.Trained.classProbs(vals)
	got := probs[i].([]any)
	for k := range want {
		if !closeTo(got[k].(float64), want[k]) {
			return fmt.Sprintf("c_probs %v, want %v", got, want)
		}
	}
	if pred[i] != md.Trained.Classes[argmax(want)] {
		return fmt.Sprintf("predicted_c %v, want %s", pred[i], md.Trained.Classes[argmax(want)])
	}
	return ""
}

func checkLatent(md bqModel, vals map[string]*string, out map[string]any, i int) string {
	_, acts := md.Trained.DNN.forwardActs(md.Trained.encodeRow(vals)[1:])
	lat := acts[md.Trained.DNN.latentLayer()]
	for j, want := range lat {
		got, _ := out[fmt.Sprintf("latent_col_%d", j+1)].([]any)
		if len(got) <= i || !closeTo(got[i].(float64), want) {
			return fmt.Sprintf("latent_col_%d %v, want %g", j+1, got, want)
		}
	}
	return ""
}

// With an image that has XGBoost (CLOUDBURROW_XGBOOST_IMAGE: python with
// xgboost 1.7, which loads 0.82's binary Booster), the export's main.py
// runs model.bst through predictor.py on JSON instances and answers what
// ML.PREDICT's model does.
func TestExportXGBoostWithXGBoost(t *testing.T) {
	image := os.Getenv("CLOUDBURROW_XGBOOST_IMAGE")
	if image == "" {
		t.Skip("CLOUDBURROW_XGBOOST_IMAGE is not set")
	}
	cols, rows := exportRows()
	for _, tc := range []struct {
		name, opts, out string
		cols            []mlColumn
		rows            [][]*string
	}{
		{"regressor", "model_type='BOOSTED_TREE_REGRESSOR', input_label_cols=['y'], max_iterations=5, data_split_method='NO_SPLIT'", "predicted_y", cols[:3], cut(rows, 3)},
		{"binary", "model_type='BOOSTED_TREE_CLASSIFIER', input_label_cols=['label'], max_iterations=5, data_split_method='NO_SPLIT'", "predicted_label", cols, rows},
		{"multiclass", "model_type='RANDOM_FOREST_CLASSIFIER', input_label_cols=['c'], num_parallel_tree=4, data_split_method='NO_SPLIT'", "predicted_c", []mlColumn{cols[0], cols[2], cols[1]}, reorder(rows, 0, 2, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := exportTrain(t, tc.opts, tc.cols, tc.rows)
			dir := t.TempDir()
			for _, f := range xgbFiles(md) {
				p := filepath.Join(dir, filepath.FromSlash(f.name))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, f.data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var instances []map[string]any
			for _, row := range tc.rows {
				in := map[string]any{}
				for i, c := range tc.cols {
					if c.Type == "STRING" {
						in[c.Name] = *row[i]
					} else {
						var x float64
						_, _ = fmt.Sscan(*row[i], &x)
						in[c.Name] = x
					}
				}
				instances = append(instances, in)
			}
			arg, _ := json.Marshal(instances)
			// The tutorial's steps: extract the predictor's package, then
			// python main.py '<instances>'.
			script := "cd /model && tar xzf xgboost_predictor-0.1.tar.gz -C /tmp && cp -r /model /tmp/m && cp /tmp/xgboost_predictor-0.1/predictor.py /tmp/m/ && python /tmp/m/main.py \"$1\""
			cmd := exec.Command("docker", "run", "--rm", "--network", "none", "-v", dir+":/model:ro", "--entrypoint", "sh", image, "-c", script, "sh", string(arg))
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			raw, err := cmd.Output()
			if err != nil {
				t.Fatalf("main.py: %v: %s", err, stderr.String())
			}
			// XGBoost prints its "Loading model from XGBoost < 1.0.0"
			// warning first: the answer is the last line.
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			raw = []byte(lines[len(lines)-1])
			var got []map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			for i, row := range tc.rows {
				vals := rowVals(tc.cols, row)
				switch len(md.Trained.Classes) {
				case 0:
					if want := md.Trained.predictRow(vals); math.Abs(got[i][tc.out].(float64)-want) > 1e-4*math.Max(1, math.Abs(want)) {
						t.Fatalf("row %d: %s %v, want %g", i, tc.out, got[i][tc.out], want)
					}
				case 2:
					p := md.Trained.predictRow(vals)
					want := md.Trained.Classes[0]
					if p >= 0.5 {
						want = md.Trained.Classes[1]
					}
					if pr := got[i][strings.TrimPrefix(tc.out, "predicted_")+"_probs"].([]any); math.Abs(pr[0].(float64)-p) > 1e-4 || got[i][tc.out] != want {
						t.Fatalf("row %d: %v, want %s at %g", i, got[i], want, p)
					}
				default:
					probs := md.Trained.classProbs(vals)
					if got[i][tc.out] != md.Trained.Classes[argmax(probs)] {
						t.Fatalf("row %d: %v, want %s (%v)", i, got[i], md.Trained.Classes[argmax(probs)], probs)
					}
				}
			}
		})
	}
}
